package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// `delta gmail login` and `logout` keep the Gmail login (the user's OAuth
// client and the refresh token they granted it) on the command center
// (GET/PUT/DELETE /gmail-login), for the phone's Mail app and the SMTP
// relay. The agent's mail commands are gone: Mail on the phone took over.

var (
	gmailAPI        = "https://gmail.googleapis.com"
	googleTokenURL  = "https://oauth2.googleapis.com/token"
	googleAuthURL   = "https://accounts.google.com/o/oauth2/v2/auth"
	googleRevokeURL = "https://oauth2.googleapis.com/revoke"
)

// gmail.modify covers reading, sending, drafts, labels and trash, but not
// deleting mail for good.
const gmailScope = "https://www.googleapis.com/auth/gmail.modify"

// The login also asks to search the people the user might write to, for
// the Mail app's suggestions as an address is typed: their organization's
// directory, and the "other contacts" Gmail saves from mail. Both are
// optional; Gmail works without them.
var peopleScopes = []string{
	"https://www.googleapis.com/auth/directory.readonly",
	"https://www.googleapis.com/auth/contacts.other.readonly",
}

const gmailUsage = `usage: delta gmail login <client-secret.json>   save a Gmail login on the command center
       delta gmail logout                       remove and revoke it

The login is for the phone's Mail app and the SMTP relay; it is set up by
the user, on a computer with a browser (see AGENTS.md).
`

func runGmail(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(gmailUsage)
		return nil
	}
	switch args[0] {
	case "login":
		return gmailLoginCmd(args[1:])
	case "logout":
		return gmailLogoutCmd(args[1:])
	default:
		return fmt.Errorf("unknown gmail command %q: only login and logout remain", args[0])
	}
}

// ---- the login and the API client ----

type gmailLogin struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email"`
}

func fetchGmailLogin() (gmailLogin, error) {
	var l gmailLogin
	cfg, err := loadConfig()
	if err != nil {
		return l, err
	}
	data, err := do(cfg, http.MethodGet, "/gmail-login", nil)
	if err != nil {
		return l, err
	}
	err = json.Unmarshal(data, &l)
	return l, err
}

var errGmailLoginExpired = errors.New("the Gmail login has expired or was revoked (a password change does this); ask the user to run `delta gmail login` again on a computer")

// googleToken is a response from Google's OAuth token endpoint.
type googleToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

// requestToken posts form to the token endpoint.
func requestToken(form url.Values) (*googleToken, error) {
	resp, err := (&http.Client{Timeout: 30 * time.Second}).PostForm(googleTokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("Google can't be reached: %w", stripURL(err))
	}
	defer resp.Body.Close()
	var t googleToken
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return nil, fmt.Errorf("unexpected response from Google (HTTP %d)", resp.StatusCode)
	}
	switch {
	case t.Error == "invalid_grant":
		return nil, errGmailLoginExpired
	case t.Error == "invalid_client" || t.Error == "unauthorized_client":
		return nil, errors.New("Google no longer accepts the Gmail login's OAuth client (deleted, or its secret was reset); ask the user to run `delta gmail login` again on a computer")
	case t.Error != "":
		return nil, fmt.Errorf("Google refused the Gmail login: %s", firstNonEmpty(t.Description, t.Error))
	case t.AccessToken == "":
		return nil, fmt.Errorf("unexpected response from Google (HTTP %d)", resp.StatusCode)
	}
	return &t, nil
}

func stripURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

type gmailClient struct {
	http  *http.Client
	token string
}

// gmailAPIError is an error response from Gmail.
type gmailAPIError struct {
	Code    int
	Message string
}

func (e *gmailAPIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Code)
	}
	switch e.Code {
	case http.StatusUnauthorized:
		return "Gmail rejected the login: " + msg + "; ask the user to run `delta gmail login` again on a computer"
	case http.StatusForbidden:
		if strings.Contains(strings.ToLower(msg), "scope") || strings.Contains(strings.ToLower(msg), "permission") {
			return "the Gmail login lacks a permission (" + msg + "); ask the user to run `delta gmail login` again and allow everything it asks for"
		}
	case http.StatusTooManyRequests:
		return "Gmail: too many requests; wait a minute and try again"
	}
	return fmt.Sprintf("Gmail: %s (HTTP %d)", msg, e.Code)
}

// do sends a request to the Gmail API (path is relative to
// /gmail/v1/users/me, or a full /upload/... path) and decodes the JSON
// response into out.
func (c *gmailClient) do(method, path string, query url.Values, contentType string, body io.Reader, out any) error {
	u := gmailAPI + path
	if !strings.HasPrefix(path, "/upload/") {
		u = gmailAPI + "/gmail/v1/users/me" + path
	}
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Gmail can't be reached: %w", stripURL(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(data, &e)
		return &gmailAPIError{Code: resp.StatusCode, Message: strings.TrimSpace(e.Error.Message)}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("unexpected response from Gmail: %w", err)
		}
	}
	return nil
}

func (c *gmailClient) get(path string, query url.Values, out any) error {
	return c.do(http.MethodGet, path, query, "", nil, out)
}
