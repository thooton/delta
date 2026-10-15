package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
	agents, err := newTestAgents(dir)
	if err != nil {
		return err
	}
	public, agentHandler := newHandler(hash[:], filesDir, 1<<20, u, nil, agents)
	ts := httptest.NewServer(public)
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
		{"existing file is not replaced", 409, "PUT", "/files/test.apk", token, []byte("other")},
		{"oversized upload is rejected", 413, "PUT", "/files/big.bin", token, make([]byte, 1<<20+1)},
		{"hidden name is rejected", 400, "PUT", "/files/.hidden", token, content},
		{"path traversal is rejected", 400, "PUT", "/files/..%2Fescape", token, content},
		{"github relay requires the token", 401, "GET", "/github/api.github.com/user", "", nil},
		{"github relay without a GitHub token says so", 503, "GET", "/github/api.github.com/user", token, nil},
		{"github relay refuses other hosts", 404, "GET", "/github/example.com/x", token, nil},
		{"the GitHub token can't be read back", 405, "GET", "/personal-access-token", token, nil},
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
	if err := musicSelfTest(expect, token, dir); err != nil {
		return err
	}
	if err := booksSelfTest(expect, token); err != nil {
		return err
	}
	if err := gmailSelfTest(expect, token, dir); err != nil {
		return err
	}
	if err := secretSelfTest(expect, token, accessTokenStore(dir), "ghp_first", "github_pat_second"); err != nil {
		return err
	}
	if err := secretSelfTest(expect, token, mapsKeyStore(dir), "AIzaFirst", "AIzaSecond"); err != nil {
		return err
	}
	if err := agentsSelfTest(expect, token, dir, agents, agentHandler, ts.URL); err != nil {
		return err
	}
	if err := agentSocketSelfTest(dir, agentHandler); err != nil {
		return err
	}
	return canvasSelfTest(dir)
}

// musicSelfTest checks that the music config starts as the default, can be
// replaced and reset, and never stores a login; that bad downloads and
// unknown jobs are refused; that re-login replaces the account; and that the
// bundled engine (if this build has one) runs.
func musicSelfTest(expect func(desc string, want int, method, path, tok string, body []byte) ([]byte, error), token, dir string) error {
	steps := []struct {
		desc, method, path string
		want               int
		body               string
	}{
		{"music download requires the token", "POST", "/music", 401, `{"mode":"song","query":"A - B"}`},
		{"music download with a bad mode is rejected", "POST", "/music", 400, `{"mode":"playlist","query":"A - B"}`},
		{"music download without a query is rejected", "POST", "/music", 400, `{"mode":"song","query":" "}`},
		{"music download of an option is rejected", "POST", "/music", 400, `{"mode":"album","query":"--help"}`},
		{"music download that isn't JSON is rejected", "POST", "/music", 400, `song`},
		{"unknown music job", "GET", "/music-jobs/0123456789abcdef0123456789abcdef", 404, ""},
		{"malformed music job", "GET", "/music-jobs/not-a-job", 404, ""},
		{"deleting an unknown music job", "DELETE", "/music-jobs/0123456789abcdef0123456789abcdef", 404, ""},
	}
	for _, s := range steps {
		tok := token
		if s.want == 401 {
			tok = ""
		}
		if _, err := expect(s.desc, s.want, s.method, s.path, tok, []byte(s.body)); err != nil {
			return err
		}
	}
	loginPath := filepath.Join(dir, musicLoginFile)
	if _, err := os.Stat(loginPath); err == nil {
		return fmt.Errorf("music login made up before any download")
	}
	m := newMusicService(dir)
	first, err := m.account(false)
	if err != nil {
		return fmt.Errorf("music login: %w", err)
	}
	if again, err := m.account(false); err != nil || again != first {
		return fmt.Errorf("music login not kept: %+v then %+v (%v)", first, again, err)
	}
	if _, err := expect("music re-login", 200, "POST", "/music-login", token, nil); err != nil {
		return err
	}
	if renewed, err := m.account(false); err != nil || renewed == first || len(renewed.Username) != 16 || len(renewed.Password) != 24 {
		return fmt.Errorf("music re-login: %+v then %+v (%v)", first, renewed, err)
	}
	if info, err := os.Stat(loginPath); err != nil || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("music login file: %v %v", info.Mode(), err)
	}
	if err := musicEngineSelfTest(m); err != nil {
		return err
	}

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

// musicEngineSelfTest unpacks the bundled engine and checks that it runs on
// this machine and is the version it claims. Builds without an engine pass.
func musicEngineSelfTest(m *musicService) error {
	version, err := musicEngineFS.ReadFile("music-engine/engine.version")
	if err != nil {
		return nil
	}
	engine, err := m.engine()
	if err != nil {
		return fmt.Errorf("music engine: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, engine, "--version").CombinedOutput()
	want := strings.Fields(string(version))[0]
	if err != nil || strings.TrimSpace(string(out)) != want {
		return fmt.Errorf("music engine --version: got %q (%v), want %s", strings.TrimSpace(string(out)), err, want)
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

// gmailSelfTest checks that the Gmail login can be set, read back, replaced
// and removed, that it is private, and that incomplete logins are rejected.
func gmailSelfTest(expect func(desc string, want int, method, path, tok string, body []byte) ([]byte, error), token, dir string) error {
	if _, err := expect("gmail login requires the token", 401, "GET", "/gmail-login", "", nil); err != nil {
		return err
	}
	if _, err := expect("no gmail login yet", 404, "GET", "/gmail-login", token, nil); err != nil {
		return err
	}
	for _, bad := range []string{"", "nope", `{"client_id":"a","client_secret":"b","refresh_token":"c"}`, `{"client_id":"a","client_secret":"b","email":"me@gmail.com"}`} {
		if _, err := expect(fmt.Sprintf("gmail login %q is rejected", bad), 400, "PUT", "/gmail-login", token, []byte(bad)); err != nil {
			return err
		}
	}
	if _, err := expect("oversized gmail login is rejected", 413, "PUT", "/gmail-login", token, make([]byte, maxGmailLogin+1)); err != nil {
		return err
	}
	for _, login := range []gmailLogin{
		{ClientID: "id-1.apps.googleusercontent.com", ClientSecret: "secret-1", RefreshToken: "1//first", Email: "me@gmail.com"},
		{ClientID: "id-2.apps.googleusercontent.com", ClientSecret: "secret-2", RefreshToken: "1//second", Email: "other@gmail.com"},
	} {
		body, _ := json.Marshal(login)
		if _, err := expect("set gmail login", 200, "PUT", "/gmail-login", token, body); err != nil {
			return err
		}
		data, err := expect("read gmail login", 200, "GET", "/gmail-login", token, nil)
		if err != nil {
			return err
		}
		var got gmailLogin
		if err := json.Unmarshal(data, &got); err != nil || got != login {
			return fmt.Errorf("read gmail login: got %s", data)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, gmailLoginFile)); err != nil || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("gmail login file: %v %v", info, err)
	}
	if _, err := expect("remove gmail login", 200, "DELETE", "/gmail-login", token, nil); err != nil {
		return err
	}
	_, err := expect("gmail login after removal", 404, "GET", "/gmail-login", token, nil)
	return err
}

// secretSelfTest checks that a secretStore's secret can be set and replaced,
// that malformed values are rejected, and that it reads back, unless it is
// write-only, in which case no request can read it.
func secretSelfTest(expect func(desc string, want int, method, path, tok string, body []byte) ([]byte, error), token string, s *secretStore, values ...string) error {
	if _, err := expect(s.route+" requires the token", 401, "PUT", s.route, "", []byte(values[0])); err != nil {
		return err
	}
	if !s.writeOnly {
		if _, err := expect(s.route+" is unset", 404, "GET", s.route, token, nil); err != nil {
			return err
		}
	}
	for _, bad := range []string{"", " \n", "two words", "a\x00b"} {
		if _, err := expect(fmt.Sprintf("%s %q is rejected", s.route, bad), 400, "PUT", s.route, token, []byte(bad)); err != nil {
			return err
		}
	}
	if _, err := expect("oversized "+s.route+" is rejected", 413, "PUT", s.route, token, make([]byte, maxSecret+1)); err != nil {
		return err
	}
	for _, v := range values {
		if _, err := expect("set "+s.route, 200, "PUT", s.route, token, []byte(v+"\n")); err != nil {
			return err
		}
		if s.writeOnly {
			// 405 or 404, depending on what else is on the route.
			data, err := expect(s.route+" can't be read back", 405, "GET", s.route, token, nil)
			if err != nil {
				data, err = expect(s.route+" can't be read back", 404, "GET", s.route, token, nil)
			}
			if err != nil {
				return err
			} else if strings.Contains(string(data), v) {
				return fmt.Errorf("%s: GET showed the secret", s.route)
			}
			if saved, err := os.ReadFile(s.path); err != nil || strings.TrimSpace(string(saved)) != v {
				return fmt.Errorf("%s: saved %q (%v), want %q", s.route, saved, err, v)
			}
			continue
		}
		data, err := expect("read "+s.route, 200, "GET", s.route, token, nil)
		if err != nil {
			return err
		}
		var got map[string]string
		if err := json.Unmarshal(data, &got); err != nil || got[s.field] != v {
			return fmt.Errorf("read %s: got %s, want %q", s.route, data, v)
		}
	}
	return nil
}
