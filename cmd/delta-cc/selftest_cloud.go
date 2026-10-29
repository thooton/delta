package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// fakeCloud stands in for the provider's login server and API. It knows one
// account whose refresh tokens are "rt-1", "rt-2", ... and only the latest
// one works, as with the real thing; using an old one fails. Access token
// "at-N" goes with "rt-N". "at-other" is a second, current login with a
// refresh token ("rt-other") that is never spent.
type fakeCloud struct {
	*httptest.Server
	mu       sync.Mutex
	current  int // the live refresh token's number
	renewals int
	refuse   bool             // the sessions list turns the next token down, as if it expired early
	envs     []map[string]any // environments made, as sent
	sessions []map[string]any // sessions started, as sent
}

func newFakeCloud() *fakeCloud {
	f := &fakeCloud{current: 1}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		if in["grant_type"] != "refresh_token" || in["client_id"] != cloudClientID || !strings.Contains(in["scope"], "user:sessions:claude_code") {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if in["refresh_token"] == "rt-other" {
			writeJSON(w, http.StatusOK, map[string]any{"access_token": "at-other", "refresh_token": "rt-other", "expires_in": 28800})
			return
		}
		if in["refresh_token"] != fmt.Sprintf("rt-%d", f.current) {
			writeError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		f.current++
		f.renewals++
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  fmt.Sprintf("at-%d", f.current),
			"refresh_token": fmt.Sprintf("rt-%d", f.current),
			"expires_in":    28800,
		})
	})
	who := func(r *http.Request) string {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Header.Get("Authorization") {
		case fmt.Sprintf("Bearer at-%d", f.current):
			return "first"
		case "Bearer at-other":
			return "other"
		}
		return ""
	}
	mux.HandleFunc("GET /api/oauth/profile", func(w http.ResponseWriter, r *http.Request) {
		switch who(r) {
		case "first":
			writeJSON(w, http.StatusOK, map[string]any{
				"account":      map[string]string{"uuid": "11111111-aaaa", "email_address": "first@example.com"},
				"organization": map[string]string{"uuid": "org-1", "organization_type": "claude_pro"},
			})
		case "other":
			writeJSON(w, http.StatusOK, map[string]any{
				"account":      map[string]string{"uuid": "22222222-bbbb", "email_address": "other@example.com"},
				"organization": map[string]string{"uuid": "org-2", "organization_type": "claude_max"},
			})
		default:
			writeError(w, http.StatusUnauthorized, "invalid token")
		}
	})
	mux.HandleFunc("GET /api/oauth/usage", func(w http.ResponseWriter, r *http.Request) {
		if who(r) == "" || r.Header.Get("anthropic-beta") != cloudBeta {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"five_hour": map[string]any{"utilization": 42.0, "resets_at": "2030-01-01T00:00:00Z"}})
	})
	mux.HandleFunc("GET /v1/code/sessions", func(w http.ResponseWriter, r *http.Request) {
		who := who(r)
		f.mu.Lock()
		refuse := f.refuse
		f.refuse = false
		f.mu.Unlock()
		if refuse || who == "" || r.Header.Get("anthropic-version") == "" {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "provider", Value: "secret"})
		writeJSON(w, http.StatusOK, map[string]any{
			"who": who, "org": r.Header.Get("x-organization-uuid"), "query": r.URL.RawQuery,
			"leaked": r.Header.Get("Cookie") + r.Header.Get("Origin"),
		})
	})
	// Environments and sessions, for transform jobs: only for the first
	// account's organization.
	scoped := func(w http.ResponseWriter, r *http.Request, beta bool) bool {
		if who(r) != "first" || r.Header.Get("x-organization-uuid") != "org-1" || beta && r.Header.Get("anthropic-beta") != cloudSessionsBeta {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/environment_providers", func(w http.ResponseWriter, r *http.Request) {
		if !scoped(w, r, false) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		envs := []map[string]any{{"kind": "anthropic_cloud", "name": "Delta 12345678", "environment_id": "env_other"}}
		for i, e := range f.envs {
			envs = append(envs, map[string]any{"kind": e["kind"], "name": e["name"], "environment_id": fmt.Sprintf("env_%d", i+1)})
		}
		writeJSON(w, http.StatusOK, map[string]any{"environments": envs})
	})
	mux.HandleFunc("POST /v1/environment_providers/cloud/create", func(w http.ResponseWriter, r *http.Request) {
		if !scoped(w, r, true) {
			return
		}
		var env map[string]any
		json.NewDecoder(r.Body).Decode(&env)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.envs = append(f.envs, env)
		writeJSON(w, http.StatusOK, map[string]any{"environment_id": fmt.Sprintf("env_%d", len(f.envs))})
	})
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if !scoped(w, r, true) {
			return
		}
		var session map[string]any
		json.NewDecoder(r.Body).Decode(&session)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sessions = append(f.sessions, session)
		writeJSON(w, http.StatusOK, map[string]any{"id": fmt.Sprintf("session_%d", len(f.sessions))})
	})
	f.Server = httptest.NewServer(mux)
	return f
}

// cloudSelfTest checks that accounts can be added (by renewing the login, or
// by looking up a still-good access token), listed without their logins,
// handed out as access tokens that are renewed exactly when they must be,
// asked for usage and removed, and that bad and rejected logins are refused.
func cloudSelfTest(expect expectFunc, base, token, dir string, fake *fakeCloud) error {
	if _, err := expect("cloud accounts require the token", 401, "GET", "/cloud-accounts", "", nil); err != nil {
		return err
	}
	if data, err := expect("no cloud accounts yet", 200, "GET", "/cloud-accounts", token, nil); err != nil {
		return err
	} else if strings.TrimSpace(string(data)) != "[]" {
		return fmt.Errorf("no cloud accounts yet: got %s", data)
	}
	for _, bad := range []string{"", "nope", `{"access_token":"at-1"}`, `{"refresh_token":"two words"}`} {
		if _, err := expect(fmt.Sprintf("cloud account %q is rejected", bad), 400, "POST", "/cloud-accounts", token, []byte(bad)); err != nil {
			return err
		}
	}
	if _, err := expect("a dead login is refused", 403, "POST", "/cloud-accounts", token, []byte(`{"refresh_token":"rt-0"}`)); err != nil {
		return err
	}
	if _, err := expect("add an account by renewing its login", 201, "POST", "/cloud-accounts", token, []byte(`{"refresh_token":"rt-1"}`)); err != nil {
		return err
	}
	if _, err := expect("add an account by its access token", 201, "POST", "/cloud-accounts", token,
		[]byte(`{"access_token":"at-other","refresh_token":"rt-other","expires_at":99999999999999}`)); err != nil {
		return err
	}
	if _, err := expect("re-adding an account replaces it", 200, "POST", "/cloud-accounts", token,
		[]byte(`{"access_token":"at-other","refresh_token":"rt-other","expires_at":99999999999999}`)); err != nil {
		return err
	}
	data, err := expect("list cloud accounts", 200, "GET", "/cloud-accounts", token, nil)
	if err != nil {
		return err
	}
	var listed []cloudAccountInfo
	if err := json.Unmarshal(data, &listed); err != nil || len(listed) != 2 ||
		listed[0].ID != "11111111-aaaa" || listed[0].Email != "first@example.com" || listed[0].Plan != "claude_pro" ||
		listed[1].ID != "22222222-bbbb" || strings.Contains(string(data), "rt-") || strings.Contains(string(data), "at-") {
		return fmt.Errorf("list cloud accounts: got %s", data)
	}

	tokenOf := func(desc, body string) (string, error) {
		data, err := expect(desc, 200, "POST", "/cloud-accounts/11111111-aaaa/token", token, []byte(body))
		if err != nil {
			return "", err
		}
		var t struct {
			AccessToken string `json:"access_token"`
			ExpiresAt   int64  `json:"expires_at"`
		}
		if err := json.Unmarshal(data, &t); err != nil || t.ExpiresAt == 0 {
			return "", fmt.Errorf("%s: got %s", desc, data)
		}
		return t.AccessToken, nil
	}
	if got, err := tokenOf("access token", ""); err != nil {
		return err
	} else if got != "at-2" || fake.renewals != 1 {
		return fmt.Errorf("access token: got %s after %d renewals, want at-2 after 1", got, fake.renewals)
	}
	if got, err := tokenOf("a refused token is renewed", `{"refused":"at-2"}`); err != nil {
		return err
	} else if got != "at-3" || fake.renewals != 2 {
		return fmt.Errorf("refused token: got %s after %d renewals, want at-3 after 2", got, fake.renewals)
	}
	if got, err := tokenOf("an outdated refusal changes nothing", `{"refused":"at-2"}`); err != nil {
		return err
	} else if got != "at-3" || fake.renewals != 2 {
		return fmt.Errorf("outdated refusal: got %s after %d renewals, want at-3 after 2", got, fake.renewals)
	}
	if data, err := expect("usage", 200, "GET", "/cloud-accounts/11111111-aaaa/usage", token, nil); err != nil {
		return err
	} else if !strings.Contains(string(data), "five_hour") {
		return fmt.Errorf("usage: got %s", data)
	}
	if _, err := expect("token of an unknown account", 404, "POST", "/cloud-accounts/nobody/token", token, nil); err != nil {
		return err
	}
	if info, err := os.Stat(filepath.Join(dir, cloudAccountsFile)); err != nil || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("cloud accounts file: %v %v", info, err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, cloudAccountsFile)); err != nil || !strings.Contains(string(raw), `"rt-3"`) {
		return fmt.Errorf("cloud accounts file doesn't hold the renewed login (%v)", err)
	}
	if err := cloudRelaySelfTest(base, token, fake); err != nil {
		return err
	}
	if _, err := expect("relay to a route it doesn't reach", 404, "GET", "/cloud-accounts/11111111-aaaa/api/api/oauth/usage", token, nil); err != nil {
		return err
	}
	if _, err := expect("relay for an unknown account", 404, "GET", "/cloud-accounts/nobody/api/v1/code/sessions", token, nil); err != nil {
		return err
	}
	if _, err := expect("the relay requires the token", 401, "GET", "/cloud-accounts/11111111-aaaa/api/v1/code/sessions", "", nil); err != nil {
		return err
	}
	if _, err := expect("remove a cloud account", 200, "DELETE", "/cloud-accounts/22222222-bbbb", token, nil); err != nil {
		return err
	}
	if _, err := expect("remove it again", 404, "DELETE", "/cloud-accounts/22222222-bbbb", token, nil); err != nil {
		return err
	}
	data, err = expect("list after removal", 200, "GET", "/cloud-accounts", token, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &listed); err != nil || len(listed) != 1 || listed[0].ID != "11111111-aaaa" {
		return fmt.Errorf("list after removal: got %s", data)
	}
	return nil
}

// cloudRelaySelfTest checks that the relay reaches the provider with the
// account's token and the headers it passes on, renews a token the provider
// turns down, and keeps the browser's cookies and the provider's to
// themselves. It needs the request's own headers, which expect doesn't set.
func cloudRelaySelfTest(base, token string, fake *fakeCloud) error {
	get := func(desc string) (string, error) {
		req, _ := http.NewRequest("GET", base+"/cloud-accounts/11111111-aaaa/api/v1/code/sessions?limit=5", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("x-organization-uuid", "org-1")
		req.Header.Set("Cookie", "site=mine")
		req.Header.Set("Origin", "https://example.com")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("%s: %v", desc, err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || resp.Header.Get("Set-Cookie") != "" {
			return "", fmt.Errorf("%s: HTTP %d %s (Set-Cookie %q)", desc, resp.StatusCode, data, resp.Header.Get("Set-Cookie"))
		}
		return string(data), nil
	}
	data, err := get("relay to the sessions list")
	if err != nil {
		return err
	}
	if !strings.Contains(data, `"who":"first"`) || !strings.Contains(data, `"org":"org-1"`) ||
		!strings.Contains(data, `"query":"limit=5"`) || !strings.Contains(data, `"leaked":""`) {
		return fmt.Errorf("relay to the sessions list: got %s", data)
	}
	// A token turned down before its time: the relay must renew and try again.
	fake.mu.Lock()
	fake.refuse = true
	fake.mu.Unlock()
	before := fake.renewals
	if data, err = get("relay after the token was turned down"); err != nil {
		return err
	}
	if !strings.Contains(data, `"who":"first"`) || fake.renewals != before+1 {
		return fmt.Errorf("relay after the token was turned down: got %s after %d renewals", data, fake.renewals-before)
	}
	return nil
}

// conversationsSelfTest checks that a carried conversation can be stored,
// listed by its title and read back whole, and that bad ids find nothing.
func conversationsSelfTest(expect expectFunc, token string) error {
	if _, err := expect("conversations require the token", 401, "GET", "/conversations", "", nil); err != nil {
		return err
	}
	if data, err := expect("no conversations yet", 200, "GET", "/conversations", token, nil); err != nil {
		return err
	} else if strings.TrimSpace(string(data)) != "[]" {
		return fmt.Errorf("no conversations yet: got %s", data)
	}
	if _, err := expect("an empty conversation is refused", 400, "POST", "/conversations", token, nil); err != nil {
		return err
	}
	text := "# Fix the list app\n\n[User]\nhello\n\n" + strings.Repeat("tool output\n", 20000)
	data, err := expect("store a conversation", 201, "POST", "/conversations", token, []byte(text))
	if err != nil {
		return err
	}
	var stored struct{ ID string }
	if err := json.Unmarshal(data, &stored); err != nil || !conversationID(stored.ID) {
		return fmt.Errorf("store a conversation: got %s", data)
	}
	if back, err := expect("read it back", 200, "GET", "/conversations/"+stored.ID, token, nil); err != nil {
		return err
	} else if string(back) != text {
		return fmt.Errorf("read it back: got %d bytes, want %d", len(back), len(text))
	}
	data, err = expect("list conversations", 200, "GET", "/conversations", token, nil)
	if err != nil {
		return err
	}
	var listed []conversationInfo
	if err := json.Unmarshal(data, &listed); err != nil || len(listed) != 1 || listed[0].ID != stored.ID ||
		listed[0].Title != "Fix the list app" || listed[0].Size != int64(len(text)) {
		return fmt.Errorf("list conversations: got %s", data)
	}
	for _, bad := range []string{"aaaaaaaaaaaaaaaa", "..%2Ffiles%2Fx", "short"} {
		if _, err := expect("conversation "+bad+" is not found", 404, "GET", "/conversations/"+bad, token, nil); err != nil {
			return err
		}
	}
	return nil
}
