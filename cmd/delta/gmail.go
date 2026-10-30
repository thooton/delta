package main

import (
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
	gmailAPI       = "https://gmail.googleapis.com"
	googleTokenURL = "https://oauth2.googleapis.com/token"
)

const (
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
	if len(args) == 0 || isHelp(args[0]) {
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

// ---- the login and Google ----

type gmailLogin struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email"`
}

// googleToken is a response from Google's OAuth token endpoint.
type googleToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

// exchangeCode trades a sign-in code for a login at the token endpoint.
func exchangeCode(form url.Values) (*googleToken, error) {
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
		return nil, errors.New("Google refused the sign-in code (expired or already used); run the login again")
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

// gmailAddress asks Gmail, with an access token, for the account's address.
func gmailAddress(token string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, gmailAPI+"/gmail/v1/users/me/profile", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("Gmail can't be reached: %w", stripURL(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(data, &e)
		return "", gmailError(resp.StatusCode, strings.TrimSpace(e.Error.Message))
	}
	var prof struct {
		Email string `json:"emailAddress"`
	}
	if err := json.Unmarshal(data, &prof); err != nil {
		return "", fmt.Errorf("unexpected response from Gmail: %w", err)
	}
	return prof.Email, nil
}

// gmailError is the error for a response from Gmail that isn't a success.
func gmailError(code int, msg string) error {
	if msg == "" {
		msg = http.StatusText(code)
	}
	switch {
	case code == http.StatusUnauthorized:
		return errors.New("Gmail rejected the login: " + msg + "; ask the user to run `delta gmail login` again on a computer")
	case code == http.StatusForbidden && (strings.Contains(strings.ToLower(msg), "scope") || strings.Contains(strings.ToLower(msg), "permission")):
		return errors.New("the Gmail login lacks a permission (" + msg + "); ask the user to run `delta gmail login` again and allow everything it asks for")
	case code == http.StatusTooManyRequests:
		return errors.New("Gmail: too many requests; wait a minute and try again")
	}
	return fmt.Errorf("Gmail: %s (HTTP %d)", msg, code)
}
