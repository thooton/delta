package main

import (
	"bufio"
	"encoding/json"
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

type fakeGmail struct {
	mu sync.Mutex

	exchanged url.Values // the login's code exchange
	saved     gmailLogin // the login it saved
}

// gmailFixture starts a command center serving a Gmail login, Google's
// token endpoint and a Gmail API that knows only the account's profile.
func gmailFixture(t *testing.T) *fakeGmail {
	f := &fakeGmail{}
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gmail-login" || r.Header.Get("Authorization") != "Bearer delta_test" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			f.mu.Lock()
			json.NewDecoder(r.Body).Decode(&f.saved)
			f.mu.Unlock()
			w.Write([]byte(`{"email":"me@gmail.com"}`))
			return
		}
		json.NewEncoder(w).Encode(gmailLogin{ClientID: "cid", ClientSecret: "csecret", RefreshToken: "rtok", Email: "me@gmail.com"})
	}))
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") == "authorization_code" {
			f.mu.Lock()
			f.exchanged = r.Form
			f.mu.Unlock()
			if r.Form.Get("code") != "the-code" || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			w.Write([]byte(`{"access_token":"atok","refresh_token":"new-rtok","scope":"` + gmailScope + `"}`))
			return
		}
		if r.Form.Get("refresh_token") != "rtok" || r.Form.Get("client_secret") != "csecret" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Write([]byte(`{"access_token":"atok","expires_in":3599}`))
	}))
	api := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() { cc.Close(); token.Close(); api.Close() })
	t.Setenv("DELTA_SERVER", cc.URL)
	t.Setenv("DELTA_TOKEN", "delta_test")
	oldAPI, oldToken := gmailAPI, googleTokenURL
	gmailAPI, googleTokenURL = api.URL, token.URL
	t.Cleanup(func() { gmailAPI, googleTokenURL = oldAPI, oldToken })
	return f
}

func (f *fakeGmail) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer atok" {
		w.WriteHeader(401)
		return
	}
	if r.URL.Path != "/gmail/v1/users/me/profile" {
		http.NotFound(w, r)
		return
	}
	w.Write([]byte(`{"emailAddress":"me@gmail.com"}`))
}

func TestGmailLogin(t *testing.T) {
	f := gmailFixture(t)
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	secret := filepath.Join(t.TempDir(), "client_secret.json")
	os.WriteFile(secret, []byte(`{"installed":{"client_id":"cid","client_secret":"csecret","redirect_uris":["http://localhost"]}}`), 0o600)

	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	done := make(chan error, 1)
	go func() { done <- run([]string{"gmail", "login", secret}) }()
	// Play the browser: open the printed address's redirect with a code.
	var auth *url.URL
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, googleAuthURL) {
			auth, _ = url.Parse(line)
			break
		}
	}
	go io.Copy(io.Discard, r)
	if auth == nil {
		os.Stdout = old
		t.Fatal("no sign-in address printed")
	}
	q := auth.Query()
	if q.Get("scope") != gmailScope+" "+strings.Join(peopleScopes, " ") || q.Get("access_type") != "offline" || q.Get("code_challenge_method") != "S256" {
		t.Errorf("auth URL = %s", auth)
	}
	resp, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{"code": {"the-code"}, "state": {q.Get("state")}, "scope": {gmailScope}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	err = <-done
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	if f.exchanged.Get("redirect_uri") != q.Get("redirect_uri") {
		t.Errorf("exchange = %v", f.exchanged)
	}
	if want := (gmailLogin{ClientID: "cid", ClientSecret: "csecret", RefreshToken: "new-rtok", Email: "me@gmail.com"}); f.saved != want {
		t.Errorf("saved %+v, want %+v", f.saved, want)
	}
}

func TestOAuthClientFile(t *testing.T) {
	dir := t.TempDir()
	web := filepath.Join(dir, "web.json")
	os.WriteFile(web, []byte(`{"web":{"client_id":"a","client_secret":"b"}}`), 0o600)
	if _, _, err := oauthClient(web); err == nil || !strings.Contains(err.Error(), "Desktop app") {
		t.Errorf("web client: %v", err)
	}
	empty := filepath.Join(dir, "empty.json")
	os.WriteFile(empty, []byte(`{}`), 0o600)
	if _, _, err := oauthClient(empty); err == nil {
		t.Error("empty client file accepted")
	}
}
