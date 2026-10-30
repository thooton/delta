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

// expectFunc makes a request to the handler under test and checks its
// status, returning the body.
type expectFunc func(desc string, want int, method, path, tok string, body []byte) ([]byte, error)

// step is one request of the self-test and the status it must get.
type step struct {
	desc         string
	want         int
	method, path string
	tok          string
	body         []byte
}

// runSteps makes each step's request in turn, stopping at the first that
// doesn't get its status.
func runSteps(expect expectFunc, steps ...step) error {
	for _, s := range steps {
		if _, err := expect(s.desc, s.want, s.method, s.path, s.tok, s.body); err != nil {
			return err
		}
	}
	return nil
}

// jsonBody is v as JSON, for a request's body.
func jsonBody(v any) []byte { b, _ := json.Marshal(v); return b }

// expectJSON is expect, decoding the body into v.
func expectJSON(expect expectFunc, v any, desc string, want int, method, path, tok string, body []byte) ([]byte, error) {
	data, err := expect(desc, want, method, path, tok, body)
	if err == nil {
		if err = json.Unmarshal(data, v); err != nil {
			err = fmt.Errorf("%s: %w (%s)", desc, err, data)
		}
	}
	return data, err
}

// testRequest makes a request to url with tok as its bearer token (if any)
// and header's name, value pairs, and reads the whole response.
func testRequest(ctx context.Context, method, url, tok string, body []byte, header ...string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp, data, err
}

// expectAt is an expectFunc for the server at base. A form (selfTestForm)
// says so in its Content-Type.
func expectAt(base string) expectFunc {
	return func(desc string, want int, method, path, tok string, body []byte) ([]byte, error) {
		var header []string
		if bytes.HasPrefix(body, []byte("--"+selfTestBoundary+"\r\n")) {
			header = []string{"Content-Type", "multipart/form-data; boundary=" + selfTestBoundary}
		}
		resp, data, err := testRequest(context.Background(), method, base+path, tok, body, header...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", desc, err)
		}
		if resp.StatusCode != want {
			return nil, fmt.Errorf("%s: got HTTP %d, want %d (%s)", desc, resp.StatusCode, want, strings.TrimSpace(string(data)))
		}
		return data, nil
	}
}

// selfTestToken is the delta token the self-test's handlers take.
const selfTestToken = "delta_selftest"

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

	const token = selfTestToken
	hash := sha256.Sum256([]byte(token))
	// The cloud accounts renew their logins against a fake of the provider.
	fake := newFakeCloud()
	defer fake.Close()
	upstream := cloudUpstream
	cloudUpstream.tokenURL, cloudUpstream.apiURL = fake.URL+"/v1/oauth/token", fake.URL
	defer func() { cloudUpstream = upstream }()
	u := &updater{restart: make(chan struct{}, 1)}
	agents, err := newTestAgents(dir)
	if err != nil {
		return err
	}
	rmDir := filepath.Join(dir, "rmfakecloud")
	remarkable := newRemarkableStore(rmDir, "user", "", 1<<20)
	if err := os.MkdirAll(remarkable.dir, 0o700); err != nil {
		return err
	}
	public, agentHandler, qa := newHandler(hash[:], filesDir, 1<<20, u, nil, remarkable, agents)
	ts := httptest.NewServer(public)
	defer ts.Close()
	expect := expectAt(ts.URL)

	content := []byte("hello from the delta-cc self-test\n")
	if err := runSteps(expect,
		step{"no token is rejected", 401, "GET", "/files", "", nil},
		step{"the website needs no token", 200, "GET", "/", "", nil},
		step{"nor do its scripts", 200, "GET", "/web/app.js", "", nil},
		step{"the website's directory isn't listed", 404, "GET", "/web/missing.js", "", nil},
		step{"wrong token is rejected", 401, "GET", "/files", "nope", nil},
		step{"auth check", 200, "GET", "/auth", token, nil},
		step{"version", 200, "GET", "/version", token, nil},
		step{"upload new file", 201, "PUT", "/files/test.apk", token, content},
		step{"existing file is not replaced", 409, "PUT", "/files/test.apk", token, []byte("other")},
		step{"oversized upload is rejected", 413, "PUT", "/files/big.bin", token, make([]byte, 1<<20+1)},
		step{"hidden name is rejected", 400, "PUT", "/files/.hidden", token, content},
		step{"path traversal is rejected", 400, "PUT", "/files/..%2Fescape", token, content},
		step{"github relay requires the token", 401, "GET", "/github/api.github.com/user", "", nil},
		step{"github relay without a GitHub token says so", 503, "GET", "/github/api.github.com/user", token, nil},
		step{"github relay refuses other hosts", 404, "GET", "/github/example.com/x", token, nil},
		step{"the GitHub token can't be read back", 405, "GET", "/personal-access-token", token, nil},
		step{"missing file download", 404, "GET", "/files/missing", token, nil},
		step{"canvas relay requires the token", 401, "GET", "/canvas/api/v1/users/self", "", nil},
		step{"unconfigured canvas relay says so", 503, "GET", "/canvas/api/v1/users/self", token, nil},
	); err != nil {
		return err
	}

	page, err := expect("the website", 200, "GET", "/", "", nil)
	if err != nil {
		return err
	}
	if !bytes.Contains(page, []byte(`src="web/app.js?v=`)) || !bytes.Contains(page, []byte(`href="web/app.css?v=`)) {
		return fmt.Errorf("the website's script and styles don't carry the build's version, so a cache could serve the last build's")
	}

	if err := selfTestRemarkable(remarkable, expect, token); err != nil {
		return fmt.Errorf("remarkable: %w", err)
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

	if err := runSteps(expect,
		step{"delete", 200, "DELETE", "/files/test.apk", token, nil},
		step{"delete again", 404, "DELETE", "/files/test.apk", token, nil},
	); err != nil {
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
	if err := loginSelfTest(expect, token, booksLoginStore(dir),
		[]string{`{"email":"a@b.c"}`, `{"email":"ab","password":"x"}`},
		booksLogin{Email: "reader@example.com", Password: "p@ss word"},
	); err != nil {
		return err
	}
	if err := qaSelfTest(expect, token, qa); err != nil {
		return fmt.Errorf("qa: %w", err)
	}
	if err := otpSelfTest(expect, token); err != nil {
		return err
	}
	if err := flashcardsSelfTest(expect, token); err != nil {
		return fmt.Errorf("flashcards: %w", err)
	}
	if err := loginSelfTest(expect, token, gmailLoginStore(dir),
		[]string{`{"client_id":"a","client_secret":"b","refresh_token":"c"}`, `{"client_id":"a","client_secret":"b","email":"me@gmail.com"}`},
		gmailLogin{ClientID: "id-1.apps.googleusercontent.com", ClientSecret: "secret-1", RefreshToken: "1//first", Email: "me@gmail.com"},
		gmailLogin{ClientID: "id-2.apps.googleusercontent.com", ClientSecret: "secret-2", RefreshToken: "1//second", Email: "other@gmail.com"},
	); err != nil {
		return err
	}
	if err := cloudSelfTest(expect, ts.URL, token, dir, fake); err != nil {
		return err
	}
	if err := conversationsSelfTest(expect, token); err != nil {
		return err
	}
	if err := transformSelfTest(expect, token, filesDir, remarkable, fake); err != nil {
		return fmt.Errorf("transform: %w", err)
	}
	if err := transformCLISelfTest(dir); err != nil {
		return err
	}
	if err := shorthandSelfTest(expect, ts.URL, token, dir); err != nil {
		return fmt.Errorf("shorthand: %w", err)
	}
	if err := smtpSelfTest(dir); err != nil {
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
func musicSelfTest(expect expectFunc, token, dir string) error {
	if err := runSteps(expect,
		step{"music download requires the token", 401, "POST", "/music", "", []byte(`{"mode":"song","query":"A - B"}`)},
		step{"music download with a bad mode is rejected", 400, "POST", "/music", token, []byte(`{"mode":"playlist","query":"A - B"}`)},
		step{"music download without a query is rejected", 400, "POST", "/music", token, []byte(`{"mode":"song","query":" "}`)},
		step{"music download of an option is rejected", 400, "POST", "/music", token, []byte(`{"mode":"album","query":"--help"}`)},
		step{"music download that isn't JSON is rejected", 400, "POST", "/music", token, []byte(`song`)},
		step{"music download of a bad link is rejected", 400, "POST", "/music", token, []byte(`{"link":"http://example.com/a.mp3"}`)},
		step{"music search requires the token", 401, "POST", "/music-search", "", []byte(`{"mode":"song","query":"A - B"}`)},
		step{"music search with a bad mode is rejected", 400, "POST", "/music-search", token, []byte(`{"mode":"playlist","query":"A - B"}`)},
		step{"unknown music job", 404, "GET", "/music-jobs/0123456789abcdef0123456789abcdef", token, nil},
		step{"malformed music job", 404, "GET", "/music-jobs/not-a-job", token, nil},
		step{"deleting an unknown music job", 404, "DELETE", "/music-jobs/0123456789abcdef0123456789abcdef", token, nil},
	); err != nil {
		return err
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
	var steps []step
	for _, bad := range []string{"username = someone\n", "[p]\nPassword=x\n", "l = a;b\n", "format\n", "format =\n"} {
		steps = append(steps, step{fmt.Sprintf("music config %q is rejected", bad), 400, "PUT", "/music-config", token, []byte(bad)})
	}
	custom := "# lossless\npref-format = flac\n\n[albums]\nprofile-cond = album\n"
	steps = append(steps, step{"set music config", 200, "PUT", "/music-config", token, []byte(custom)})
	if err := runSteps(expect, steps...); err != nil {
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

// loginSelfTest checks that a loginStore's login can be set, read back,
// replaced and removed, that it is private, and that incomplete and
// oversized logins are rejected.
func loginSelfTest[T comparable](expect expectFunc, token string, s *loginStore[T], bad []string, logins ...T) error {
	steps := []step{
		{s.route + " requires the token", 401, "GET", s.route, "", nil},
		{s.route + " is unset", 404, "GET", s.route, token, nil},
	}
	for _, b := range append(bad, "", "nope") {
		steps = append(steps, step{fmt.Sprintf("%s %q is rejected", s.route, b), 400, "PUT", s.route, token, []byte(b)})
	}
	steps = append(steps, step{"oversized " + s.route + " is rejected", 413, "PUT", s.route, token, make([]byte, s.max+1)})
	if err := runSteps(expect, steps...); err != nil {
		return err
	}
	for _, login := range logins {
		if _, err := expect("set "+s.route, 200, "PUT", s.route, token, jsonBody(login)); err != nil {
			return err
		}
		var got T
		if data, err := expectJSON(expect, &got, "read "+s.route, 200, "GET", s.route, token, nil); err != nil {
			return err
		} else if got != login {
			return fmt.Errorf("read %s: got %s", s.route, data)
		}
	}
	if info, err := os.Stat(s.path); err != nil || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%s: %v %v", s.path, info, err)
	}
	return runSteps(expect,
		step{"remove " + s.route, 200, "DELETE", s.route, token, nil},
		step{s.route + " after removal", 404, "GET", s.route, token, nil},
	)
}

// secretSelfTest checks that a secretStore's secret can be set and replaced,
// that malformed values are rejected, and that it reads back, unless it is
// write-only, in which case no request can read it.
func secretSelfTest(expect expectFunc, token string, s *secretStore, values ...string) error {
	steps := []step{{s.route + " requires the token", 401, "PUT", s.route, "", []byte(values[0])}}
	if !s.writeOnly {
		steps = append(steps, step{s.route + " is unset", 404, "GET", s.route, token, nil})
	}
	for _, bad := range []string{"", " \n", "two words", "a\x00b"} {
		steps = append(steps, step{fmt.Sprintf("%s %q is rejected", s.route, bad), 400, "PUT", s.route, token, []byte(bad)})
	}
	steps = append(steps, step{"oversized " + s.route + " is rejected", 413, "PUT", s.route, token, make([]byte, maxSecret+1)})
	if err := runSteps(expect, steps...); err != nil {
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
		var got map[string]string
		if data, err := expectJSON(expect, &got, "read "+s.route, 200, "GET", s.route, token, nil); err != nil {
			return err
		} else if got[s.field] != v {
			return fmt.Errorf("read %s: got %s, want %q", s.route, data, v)
		}
	}
	return nil
}

// otpSelfTest checks that two-factor accounts can be added (one, then a list
// with a duplicate in it), are tidied and given defaults, are refused when
// malformed, can be changed, and can be removed.
func otpSelfTest(expect expectFunc, token string) error {
	if _, err := expect("two-factor accounts require the token", 401, "GET", "/otp", "", nil); err != nil {
		return err
	}
	data, err := expect("no two-factor accounts yet", 200, "GET", "/otp", token, nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "[]" {
		return fmt.Errorf("two-factor accounts: want [], got %s", data)
	}
	var steps []step
	for _, bad := range []string{
		`{}`,
		`{"issuer": "GitHub", "secret": "not base32!"}`,
		`{"issuer": "GitHub", "secret": "JBSWY3DPEHPK3PXP", "algorithm": "MD5"}`,
		`{"issuer": "GitHub", "secret": "JBSWY3DPEHPK3PXP", "digits": 4}`,
		`{"secret": "JBSWY3DPEHPK3PXP"}`,
		`[]`,
	} {
		steps = append(steps, step{fmt.Sprintf("two-factor account %s is refused", bad), 400, "POST", "/otp", token, []byte(bad)})
	}
	steps = append(steps, step{"add a two-factor account", 201, "POST", "/otp", token,
		[]byte(`{"issuer": " GitHub ", "account": "sam", "secret": "jbsw y3dp ehpk 3pxp=="}`)})
	if err := runSteps(expect, steps...); err != nil {
		return err
	}
	var got []otpAccount
	data, err = expectJSON(expect, &got, "add two-factor accounts", 201, "POST", "/otp", token, []byte(`[
		{"issuer": "GitHub", "account": "again", "secret": "JBSWY3DPEHPK3PXP"},
		{"issuer": "AWS", "account": "root", "secret": "GEZDGNBVGY3TQOJQ", "algorithm": "sha256", "digits": 8, "period": 60}
	]`))
	if err != nil {
		return err
	}
	if len(got) != 2 {
		return fmt.Errorf("two-factor accounts: want 2 (the duplicate secret skipped), got %s", data)
	}
	first, second := got[0], got[1]
	if first.Issuer != "GitHub" || first.Account != "sam" || first.Secret != "JBSWY3DPEHPK3PXP" ||
		first.Algorithm != "SHA1" || first.Digits != 6 || first.Period != 30 || first.ID == "" {
		return fmt.Errorf("two-factor account not tidied with defaults: %+v", first)
	}
	if second.Algorithm != "SHA256" || second.Digits != 8 || second.Period != 60 {
		return fmt.Errorf("two-factor account lost its settings: %+v", second)
	}
	got = nil
	data, err = expectJSON(expect, &got, "change a two-factor account", 200, "PUT", "/otp/"+second.ID, token,
		[]byte(`{"issuer": "Amazon Web Services", "account": "admin", "algorithm": "SHA256", "digits": 8, "period": 60}`))
	if err != nil {
		return err
	}
	if len(got) != 2 || got[1].Issuer != "Amazon Web Services" ||
		got[1].Account != "admin" || got[1].Secret != "GEZDGNBVGY3TQOJQ" || got[1].ID != second.ID || got[1].Digits != 8 {
		return fmt.Errorf("two-factor account not changed (its secret kept): %s", data)
	}
	if err := runSteps(expect,
		step{"change a secret to another account's", 400, "PUT", "/otp/" + second.ID, token, []byte(`{"issuer": "AWS", "secret": "JBSWY3DPEHPK3PXP"}`)},
		step{"change a missing two-factor account", 404, "PUT", "/otp/nope", token, []byte(`{"issuer": "AWS"}`)},
		step{"remove a two-factor account", 200, "DELETE", "/otp/" + first.ID, token, nil},
		step{"remove it again", 404, "DELETE", "/otp/" + first.ID, token, nil},
	); err != nil {
		return err
	}
	got = nil
	if data, err = expectJSON(expect, &got, "two-factor accounts after removal", 200, "GET", "/otp", token, nil); err != nil {
		return err
	}
	if len(got) != 1 || got[0].ID != second.ID {
		return fmt.Errorf("two-factor accounts after removal: %s", data)
	}
	return nil
}
