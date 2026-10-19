package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// `delta accounts` manages the cloud accounts on the command center: the
// Claude subscriptions the website's sessions run on. The command center
// keeps their logins and renews them; the website reaches the sessions
// through it and rotates between the accounts as their usage limits run
// out. Nothing here keeps a login: `login` and `add` hand theirs to the
// command center, which checks it before saving it.

const accountsHelpText = `delta accounts: the cloud accounts the sessions run on

The sessions the website starts (this one included) run on the user's
Claude subscriptions. Their logins are kept on the command center, which
renews them; no session or browser ever holds one. The website starts each
new session on the first account in the list that hasn't reached its usage
limit. When an account runs out in the middle of a conversation, the
website says until when.

COMMANDS
  delta accounts                List the accounts, in the order the
                                website tries them, with how much of each
                                one's 5-hour and weekly limits is used.
  delta accounts login          Add an account (or renew one whose login
                                stopped working). Prints an address to open
                                in a browser; sign in there with the account
                                to add, then paste the code the page shows.
                                Without a terminal to paste into, finish with
                                "delta accounts finish <code>".
  delta accounts finish <code>  Finish a login started by "login".
  delta accounts add <file|->   Add an account from a credentials file: the
                                .credentials.json Claude Code keeps in
                                ~/.claude (on macOS, in the keychain), a Pi
                                auth.json, or {"refresh_token": ...}. Once
                                the command center renews it, the copy it
                                came from stops working, so prefer "login".
  delta accounts remove <email|number>
                                Remove an account.

Adding an account that is already there replaces its login and keeps its
place. A login needs a browser, so when the user asks you to add an
account, run "delta accounts login", give them the address, and finish
with the code they send back. Never show a token.
`

const (
	accountsAuthorizeURL = "https://claude.com/cai/oauth/authorize"
	accountsTokenURL     = "https://platform.claude.com/v1/oauth/token"
	accountsRedirectURL  = "https://platform.claude.com/oauth/code/callback"
	accountsClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	accountsScopes       = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	accountsPendingFile  = "accounts-login.json"
)

type cloudAccount struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Plan  string `json:"plan"`
}

func runAccounts(args []string) error {
	if len(args) == 0 {
		return listAccounts()
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(accountsHelpText)
		return nil
	case "list", "ls":
		if len(args) != 0 {
			return errors.New("usage: delta accounts list")
		}
		return listAccounts()
	case "login":
		if len(args) != 0 {
			return errors.New("usage: delta accounts login")
		}
		return accountsLogin()
	case "finish":
		if len(args) != 1 {
			return errors.New("usage: delta accounts finish <code>")
		}
		return accountsFinish(args[0])
	case "add":
		if len(args) != 1 {
			return errors.New("usage: delta accounts add <credentials-file|->")
		}
		return accountsAdd(args[0])
	case "remove", "rm", "delete":
		if len(args) != 1 {
			return errors.New("usage: delta accounts remove <email|number>")
		}
		return accountsRemove(args[0])
	default:
		return fmt.Errorf("unknown accounts command %q (run `delta accounts help`)", cmd)
	}
}

func fetchAccounts(c config) ([]cloudAccount, error) {
	body, err := do(c, http.MethodGet, "/cloud-accounts", nil)
	if err != nil {
		return nil, err
	}
	var accounts []cloudAccount
	if err := json.Unmarshal(body, &accounts); err != nil {
		return nil, err
	}
	return accounts, nil
}

type usageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

func (w *usageWindow) String() string {
	if w == nil || w.Utilization == nil {
		return "-"
	}
	s := fmt.Sprintf("%.0f%%", *w.Utilization)
	if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
		s += ", resets " + t.Local().Format("Mon 15:04")
	}
	return s
}

func listAccounts() error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	accounts, err := fetchAccounts(c)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Println("No accounts. Add one with `delta accounts login`.")
		return nil
	}
	type usage struct {
		FiveHour *usageWindow `json:"five_hour"`
		SevenDay *usageWindow `json:"seven_day"`
		err      error
	}
	usages := make([]usage, len(accounts))
	var wg sync.WaitGroup
	for i, a := range accounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, err := do(c, http.MethodGet, "/cloud-accounts/"+url.PathEscape(a.ID)+"/usage", nil)
			if err == nil {
				err = json.Unmarshal(body, &usages[i])
			}
			usages[i].err = err
		}()
	}
	wg.Wait()
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tACCOUNT\tPLAN\t5-HOUR\tWEEKLY")
	for i, a := range accounts {
		plan := strings.TrimPrefix(a.Plan, "claude_")
		if plan == "" {
			plan = "-"
		}
		u := usages[i]
		if u.err != nil {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t\n", i+1, a.Email, plan, u.err)
			continue
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", i+1, a.Email, plan, u.FiveHour, u.SevenDay)
	}
	return tw.Flush()
}

// ---------------------------------------------------------------------------
// Adding
// ---------------------------------------------------------------------------

type cloudLogin struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
}

func saveLogin(c config, l cloudLogin) error {
	data, _ := json.Marshal(l)
	body, err := do(c, http.MethodPost, "/cloud-accounts", bytes.NewReader(data))
	if err != nil {
		return err
	}
	var a cloudAccount
	if err := json.Unmarshal(body, &a); err != nil {
		return err
	}
	plan := ""
	if p := strings.TrimPrefix(a.Plan, "claude_"); p != "" {
		plan = " (" + p + ")"
	}
	fmt.Printf("Saved %s%s on the command center.\n", a.Email, plan)
	return nil
}

func pendingPath() (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), accountsPendingFile), nil
}

func accountsLogin() error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	verifier, state := randomString(32), randomString(32)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", accountsClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", accountsRedirectURL)
	q.Set("scope", accountsScopes)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)

	path, err := pendingPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	pending, _ := json.Marshal(map[string]string{"verifier": verifier, "state": state})
	if err := os.WriteFile(path, pending, 0o600); err != nil {
		return err
	}
	fmt.Printf("Open this address in a browser and sign in with the account to add:\n\n  %s?%s\n\n", accountsAuthorizeURL, q.Encode())
	const later = "Then run: delta accounts finish <the code the page shows>"
	if info, err := os.Stdin.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		fmt.Println(later)
		return nil
	}
	fmt.Print("Paste the code the page shows: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) == "" {
		// No terminal after all (stdin is /dev/null, say).
		fmt.Println()
		fmt.Println(later)
		return nil
	}
	return finishLogin(c, line)
}

func accountsFinish(code string) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	return finishLogin(c, code)
}

// finishLogin trades the code the sign-in page showed for a login and hands
// it to the command center. The page shows "<code>#<state>".
func finishLogin(c config, pasted string) error {
	path, err := pendingPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("no login in progress; start one with `delta accounts login`")
	}
	var pending struct{ Verifier, State string }
	if err := json.Unmarshal(data, &pending); err != nil || pending.Verifier == "" {
		return errors.New("the login in progress is unreadable; start again with `delta accounts login`")
	}
	code, state, _ := strings.Cut(strings.TrimSpace(pasted), "#")
	if code == "" {
		return errors.New("that is not a code")
	}
	if state != "" && state != pending.State {
		return errors.New("that code is from a different login; use the address from the latest `delta accounts login`")
	}
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  accountsRedirectURL,
		"client_id":     accountsClientID,
		"code_verifier": pending.Verifier,
		"state":         pending.State,
	})
	req, _ := http.NewRequest(http.MethodPost, accountsTokenURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach the login server: %w", err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		return errors.New("the code was refused; codes work once and expire quickly, so start again with `delta accounts login`")
	} else if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the login server answered HTTP %d", resp.StatusCode)
	}
	var t struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(answer, &t); err != nil || t.RefreshToken == "" {
		return errors.New("the login server sent no refresh token")
	}
	os.Remove(path)
	return saveLogin(c, cloudLogin{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(t.ExpiresIn * float64(time.Second))).UnixMilli(),
	})
}

func accountsAdd(source string) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	var data []byte
	if source == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	} else {
		data, err = os.ReadFile(expandHome(source))
	}
	if err != nil {
		return err
	}
	l, err := parseLogin(data)
	if err != nil {
		return err
	}
	return saveLogin(c, l)
}

// parseLogin reads a login from any of the shapes it is found in: Claude
// Code's .credentials.json ({"claudeAiOauth": {"accessToken", "refreshToken",
// "expiresAt"}}), Pi's auth.json ({"anthropic": {"access", "refresh",
// "expires"}}), the same fields at the top level, snake_case fields, or a
// bare refresh token.
func parseLogin(data []byte) (cloudLogin, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed != "" && !strings.HasPrefix(trimmed, "{") && !strings.ContainsAny(trimmed, " \t\n\"") {
		return cloudLogin{RefreshToken: trimmed}, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return cloudLogin{}, errors.New("that isn't a credentials file (expected JSON)")
	}
	type fields struct {
		AccessToken  string      `json:"accessToken"`
		RefreshToken string      `json:"refreshToken"`
		ExpiresAt    json.Number `json:"expiresAt"`
		Access       string      `json:"access"`
		Refresh      string      `json:"refresh"`
		Expires      json.Number `json:"expires"`
		SnakeAccess  string      `json:"access_token"`
		SnakeRefresh string      `json:"refresh_token"`
		SnakeExpires json.Number `json:"expires_at"`
	}
	candidates := []json.RawMessage{root["claudeAiOauth"], root["anthropic"], root["claude"], data}
	for _, raw := range candidates {
		if len(raw) == 0 {
			continue
		}
		var f fields
		if json.Unmarshal(raw, &f) != nil {
			continue
		}
		l := cloudLogin{
			AccessToken:  strings.TrimSpace(firstNonEmpty(f.AccessToken, f.Access, f.SnakeAccess)),
			RefreshToken: strings.TrimSpace(firstNonEmpty(f.RefreshToken, f.Refresh, f.SnakeRefresh)),
		}
		for _, n := range []json.Number{f.ExpiresAt, f.Expires, f.SnakeExpires} {
			if v, err := strconv.ParseInt(n.String(), 10, 64); err == nil && v > 0 {
				l.ExpiresAt = v
				break
			}
		}
		if l.RefreshToken != "" {
			return l, nil
		}
	}
	return cloudLogin{}, errors.New("no refresh token in that file")
}

func accountsRemove(which string) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	accounts, err := fetchAccounts(c)
	if err != nil {
		return err
	}
	var match *cloudAccount
	n, numErr := strconv.Atoi(which)
	for i := range accounts {
		a := &accounts[i]
		if strings.EqualFold(a.Email, which) || a.ID == which || (numErr == nil && n == i+1) {
			match = a
			break
		}
	}
	if match == nil {
		return fmt.Errorf("no account %q (see `delta accounts`)", which)
	}
	if _, err := do(c, http.MethodDelete, "/cloud-accounts/"+url.PathEscape(match.ID), nil); err != nil {
		return err
	}
	fmt.Printf("Removed %s.\n", match.Email)
	return nil
}
