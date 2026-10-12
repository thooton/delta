package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
)

// selfTest runs the real handler against a temporary data directory and
// checks the basics. An update only installs a binary whose selfTest passes,
// so keep it covering anything that must never break.
func selfTest() error {
	dir, err := os.MkdirTemp("", "delta-cc-test-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	filesDir := filepath.Join(dir, "files")
	if err := os.Mkdir(filesDir, 0o700); err != nil {
		return err
	}

	const token = "delta_selftest"
	hash := sha256.Sum256([]byte(token))
	u := &updater{restart: make(chan struct{}, 1)}
	ts := httptest.NewServer(newHandler(hash[:], filesDir, 1<<20, u, nil))
	defer ts.Close()

	call := func(method, path, tok string, body []byte) (int, []byte, error) {
		req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		return resp.StatusCode, data, err
	}
	expect := func(desc string, want int, method, path, tok string, body []byte) ([]byte, error) {
		got, data, err := call(method, path, tok, body)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", desc, err)
		}
		if got != want {
			return nil, fmt.Errorf("%s: got HTTP %d, want %d (%s)", desc, got, want, strings.TrimSpace(string(data)))
		}
		return data, nil
	}

	content := []byte("hello from the delta-cc self-test\n")
	steps := []struct {
		desc         string
		want         int
		method, path string
		tok          string
		body         []byte
	}{
		{"no token is rejected", 401, "GET", "/files", "", nil},
		{"wrong token is rejected", 401, "GET", "/files", "nope", nil},
		{"auth check", 200, "GET", "/auth", token, nil},
		{"version", 200, "GET", "/version", token, nil},
		{"upload new file", 201, "PUT", "/files/test.apk", token, content},
		{"replace file", 200, "PUT", "/files/test.apk", token, content},
		{"oversized upload is rejected", 413, "PUT", "/files/big.bin", token, make([]byte, 1<<20+1)},
		{"hidden name is rejected", 400, "PUT", "/files/.hidden", token, content},
		{"path traversal is rejected", 400, "PUT", "/files/..%2Fescape", token, content},
		{"update without a URL is rejected", 400, "POST", "/update", token, []byte("{}")},
		{"missing file download", 404, "GET", "/files/missing", token, nil},
		{"canvas relay requires the token", 401, "GET", "/canvas/api/v1/users/self", "", nil},
		{"unconfigured canvas relay says so", 503, "GET", "/canvas/api/v1/users/self", token, nil},
	}
	for _, s := range steps {
		if _, err := expect(s.desc, s.want, s.method, s.path, s.tok, s.body); err != nil {
			return err
		}
	}

	data, err := expect("download", 200, "GET", "/files/test.apk", token, nil)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, content) {
		return fmt.Errorf("download: content does not match upload")
	}

	data, err = expect("list", 200, "GET", "/files", token, nil)
	if err != nil {
		return err
	}
	var files []fileInfo
	if err := json.Unmarshal(data, &files); err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if len(files) != 1 || files[0].Name != "test.apk" || files[0].Size != int64(len(content)) {
		return fmt.Errorf("list: unexpected result %s", data)
	}

	if _, err := expect("delete", 200, "DELETE", "/files/test.apk", token, nil); err != nil {
		return err
	}
	if _, err := expect("delete again", 404, "DELETE", "/files/test.apk", token, nil); err != nil {
		return err
	}
	if entries, _ := os.ReadDir(filesDir); len(entries) != 0 {
		return fmt.Errorf("files directory not empty after delete (leftover temp files?)")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); err == nil {
		return fmt.Errorf("path traversal wrote outside the files directory")
	}
	if err := musicSelfTest(expect, token); err != nil {
		return err
	}
	if err := booksSelfTest(expect, token); err != nil {
		return err
	}
	if err := accessTokenSelfTest(expect, token); err != nil {
		return err
	}
	return canvasSelfTest(dir)
}

// musicSelfTest checks that the music config starts as the default, can be
// replaced and reset, and never stores a login.
func musicSelfTest(expect func(desc string, want int, method, path, tok string, body []byte) ([]byte, error), token string) error {
	if _, err := expect("music config requires the token", 401, "GET", "/music-config", "", nil); err != nil {
		return err
	}
	data, err := expect("default music config", 200, "GET", "/music-config", token, nil)
	if err != nil {
		return err
	}
	if string(data) != defaultMusicConfig || checkMusicConfig(defaultMusicConfig) != nil {
		return fmt.Errorf("default music config: got %q", data)
	}
	for _, bad := range []string{"username = someone\n", "[p]\nPassword=x\n", "l = a;b\n", "format\n", "format =\n"} {
		if _, err := expect(fmt.Sprintf("music config %q is rejected", bad), 400, "PUT", "/music-config", token, []byte(bad)); err != nil {
			return err
		}
	}
	custom := "# lossless\npref-format = flac\n\n[albums]\nprofile-cond = album\n"
	if _, err := expect("set music config", 200, "PUT", "/music-config", token, []byte(custom)); err != nil {
		return err
	}
	if data, err := expect("read music config", 200, "GET", "/music-config", token, nil); err != nil {
		return err
	} else if string(data) != custom {
		return fmt.Errorf("read music config: got %q, want %q", data, custom)
	}
	if _, err := expect("reset music config", 200, "DELETE", "/music-config", token, nil); err != nil {
		return err
	}
	if data, err := expect("music config after reset", 200, "GET", "/music-config", token, nil); err != nil {
		return err
	} else if string(data) != defaultMusicConfig {
		return fmt.Errorf("music config after reset: got %q", data)
	}
	return nil
}

// booksSelfTest checks that the book login can be set, read back and removed,
// and that incomplete logins are rejected.
func booksSelfTest(expect func(desc string, want int, method, path, tok string, body []byte) ([]byte, error), token string) error {
	if _, err := expect("book login requires the token", 401, "GET", "/books-login", "", nil); err != nil {
		return err
	}
	if _, err := expect("no book login yet", 404, "GET", "/books-login", token, nil); err != nil {
		return err
	}
	for _, bad := range []string{"", "nope", `{"email":"a@b.c"}`, `{"email":"ab","password":"x"}`} {
		if _, err := expect(fmt.Sprintf("book login %q is rejected", bad), 400, "PUT", "/books-login", token, []byte(bad)); err != nil {
			return err
		}
	}
	login := booksLogin{Email: "reader@example.com", Password: "p@ss word"}
	body, _ := json.Marshal(login)
	if _, err := expect("set book login", 200, "PUT", "/books-login", token, body); err != nil {
		return err
	}
	data, err := expect("read book login", 200, "GET", "/books-login", token, nil)
	if err != nil {
		return err
	}
	var got booksLogin
	if err := json.Unmarshal(data, &got); err != nil || got != login {
		return fmt.Errorf("read book login: got %s", data)
	}
	if _, err := expect("remove book login", 200, "DELETE", "/books-login", token, nil); err != nil {
		return err
	}
	_, err = expect("book login after removal", 404, "GET", "/books-login", token, nil)
	return err
}

// accessTokenSelfTest checks that the personal access token can be set,
// read back and replaced, and that malformed tokens are rejected.
func accessTokenSelfTest(expect func(desc string, want int, method, path, tok string, body []byte) ([]byte, error), token string) error {
	if _, err := expect("personal access token requires the token", 401, "GET", "/personal-access-token", "", nil); err != nil {
		return err
	}
	if _, err := expect("no personal access token yet", 404, "GET", "/personal-access-token", token, nil); err != nil {
		return err
	}
	for _, bad := range []string{"", " \n", "two words", "a\x00b"} {
		if _, err := expect(fmt.Sprintf("personal access token %q is rejected", bad), 400, "PUT", "/personal-access-token", token, []byte(bad)); err != nil {
			return err
		}
	}
	if _, err := expect("oversized personal access token is rejected", 413, "PUT", "/personal-access-token", token, make([]byte, maxAccessToken+1)); err != nil {
		return err
	}
	for _, pat := range []string{"ghp_first", "github_pat_second"} {
		if _, err := expect("set personal access token", 200, "PUT", "/personal-access-token", token, []byte(pat+"\n")); err != nil {
			return err
		}
		data, err := expect("read personal access token", 200, "GET", "/personal-access-token", token, nil)
		if err != nil {
			return err
		}
		var got struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(data, &got); err != nil || got.Token != pat {
			return fmt.Errorf("read personal access token: got %s, want %q", data, pat)
		}
	}
	return nil
}
