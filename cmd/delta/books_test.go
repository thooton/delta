package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeBooks plays both the command center and the book source.
type fakeBooks struct {
	mu      sync.Mutex
	login   *booksAccount
	key     string // the session key the source currently accepts
	logins  int
	pushed  map[string][]byte
	content []byte
}

func (f *fakeBooks) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /books-login", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.login == nil {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"no book login is set"}`)
			return
		}
		json.NewEncoder(w).Encode(f.login)
	})
	mux.HandleFunc("PUT /books-login", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var a booksAccount
		json.NewDecoder(r.Body).Decode(&a)
		f.login = &a
		io.WriteString(w, `{}`)
	})
	mux.HandleFunc("PUT /files/{name}", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.pushed[r.PathValue("name")] = data
		f.mu.Unlock()
		fmt.Fprintf(w, `{"name":%q,"size":%d}`, r.PathValue("name"), len(data))
	})
	mux.HandleFunc("GET /eapi/info/domains", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"domains":[]}`)
	})
	mux.HandleFunc("POST /eapi/user/login", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.login == nil || r.Form.Get("email") != f.login.Email || r.Form.Get("password") != f.login.Password {
			io.WriteString(w, `{"success":0,"error":"Incorrect email or password (z-library.sk)"}`)
			return
		}
		f.logins++
		f.key = fmt.Sprintf("key%d", f.logins)
		fmt.Fprintf(w, `{"success":1,"user":{"id":42,"remix_userkey":%q}}`, f.key)
	})
	authed := func(r *http.Request) bool {
		c, err := r.Cookie("remix_userkey")
		f.mu.Lock()
		defer f.mu.Unlock()
		return err == nil && c.Value == f.key
	}
	mux.HandleFunc("POST /eapi/book/search", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			io.WriteString(w, `{"success":0,"error":"Please login"}`)
			return
		}
		r.ParseForm()
		if got := r.Form["extensions[]"]; len(got) != 1 || got[0] != "EPUB" {
			t.Errorf("extensions sent as %v", got)
		}
		io.WriteString(w, `{"success":1,"totalPages":3,"books":[
			{"id":7,"hash":"abc","title":"The Hobbit","author":"J.R.R. Tolkien","year":1937,"language":"english","extension":"epub","filesize":2048},
			{"id":8,"hash":"def","title":"Élan","author":"","year":0,"language":"","extension":"pdf","filesizeString":"1 MB"}]}`)
	})
	mux.HandleFunc("GET /eapi/book/7/abc/file", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			io.WriteString(w, `{"success":0,"error":"Please login"}`)
			return
		}
		io.WriteString(w, `{"success":1,"file":{"downloadLink":"/dl/7"}}`)
	})
	mux.HandleFunc("GET /dl/7", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write(f.content)
		gz.Close()
	})
	return mux
}

func TestBooksEndToEnd(t *testing.T) {
	f := &fakeBooks{pushed: map[string][]byte{}, content: []byte("not really an epub")}
	ts := httptest.NewServer(f.handler(t))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("DELTA_SERVER", ts.URL)
	t.Setenv("DELTA_TOKEN", "t")
	t.Setenv("DELTA_BOOKS_DOMAIN", u.Host)
	booksScheme = "http"
	defer func() { booksScheme = "https" }()

	err := runBooks([]string{"search", "hobbit"})
	if err == nil || !strings.Contains(err.Error(), "no book login is set") {
		t.Fatalf("search without a login: %v", err)
	}
	err = runBooks([]string{"login", "set", "me@example.com", "wrong"})
	if err == nil || strings.Contains(err.Error(), "z-library") || !strings.Contains(err.Error(), "the book source") {
		t.Fatalf("bad login set: %v", err)
	}
	f.login = &booksAccount{Email: "me@example.com", Password: "right"}
	if err := runBooks([]string{"login", "set", "me@example.com", "right"}); err != nil {
		t.Fatal(err)
	}
	if err := runBooks([]string{"search", "--ext", "epub", "-n", "5", "the", "hobbit"}); err != nil {
		t.Fatal(err)
	}
	st, _ := loadBooksState()
	if st.TotalPages != 3 || len(st.Results) != 2 || st.Search.Query != "the hobbit" || st.Search.Limit != 5 {
		t.Fatalf("state after search: %+v", st)
	}
	if f.logins != 1 {
		t.Errorf("search logged in again (%d logins)", f.logins)
	}

	// The session expires: the next call logs in once and retries.
	f.key = "expired"
	dir := t.TempDir()
	if err := runBooks([]string{"download", "-o", dir, "--push", "1"}); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 {
		t.Errorf("want one new login after expiry, have %d logins", f.logins)
	}
	name := "TolkienJRR_TheHobbit_1937_engli_7.epub"
	if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != string(f.content) {
		t.Errorf("downloaded file: %q, %v", data, err)
	}
	if string(f.pushed[name]) != string(f.content) {
		t.Errorf("pushed: %v", f.pushed)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
	if err := runBooks([]string{"download", "-o", dir, "3"}); err == nil {
		t.Error("out-of-range result number accepted")
	}
	if err := runBooks([]string{"page", "4"}); err == nil {
		t.Error("out-of-range page accepted")
	}
}

func TestBookFilename(t *testing.T) {
	cases := []struct {
		b    bookInfo
		ext  string
		want string
	}{
		{bookInfo{ID: "7", Title: "The Hobbit", Author: "J.R.R. Tolkien", Year: "1937", Language: "english"}, "EPUB", "TolkienJRR_TheHobbit_1937_engli_7.epub"},
		{bookInfo{ID: "8", Title: "Les Misérables", Author: "Hugo, Victor", Year: "0"}, ".pdf", "HugoVictor_LesMiserables_8.pdf"},
		{bookInfo{Title: "", Author: ""}, "exe", "UnknownAuthor_UntitledBook_NoID"},
		{bookInfo{ID: "../9", Title: "x/y", Language: "../.."}, "txt", "UnknownAuthor_XY_NoID.txt"},
	}
	for _, c := range cases {
		if got := bookFilename(c.b, c.ext); got != c.want {
			t.Errorf("bookFilename(%+v) = %q, want %q", c.b, got, c.want)
		}
	}
	long := bookInfo{ID: "1", Title: flexString(strings.Repeat("word ", 60)), Author: flexString(strings.Repeat("name ", 30))}
	if got := bookFilename(long, "epub"); len(got) > 205 {
		t.Errorf("long name not shortened: %d", len(got))
	}
}

func TestBooksErrorHidesSource(t *testing.T) {
	for _, in := range []string{
		"Incorrect password for z-library.sk",
		"see https://singlelogin.re/x for details",
		"DiamWall page from 1lib.sk",
		"ZLibrary daily limit",
	} {
		got := booksError(errors.New(in)).Error()
		for _, bad := range []string{"lib", "singlelogin", "diamwall", "DiamWall"} {
			if strings.Contains(strings.ToLower(got), strings.ToLower(bad)) {
				t.Errorf("booksError(%q) = %q", in, got)
			}
		}
	}
	ue := &url.Error{Op: "Get", URL: "https://z-library.ec/eapi/x", Err: errors.New("dial tcp: lookup z-library.ec")}
	if got := sourceError(ue); got != errBooksUnreachable {
		t.Errorf("network error kept: %v", got)
	}
}
