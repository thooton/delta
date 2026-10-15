package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// The cloud accounts: the Claude subscriptions the phone's agent sessions run
// on. The command center is the only thing that holds their logins. Each is a
// subscription OAuth login, whose refresh token is replaced every time it is
// used, so exactly one party may renew it or the copies fall out of step and
// the login dies. That party is this store: the phone (and the CLI) ask it
// for an access token whenever they need one, and it renews the login when
// the token is close to expiring, one renewal per account at a time.
//
// The phone lists the accounts, starts each new session on the first one
// that hasn't reached its usage limit, and moves a conversation to another
// account when its own runs out. `delta accounts` adds and removes them.
//
// Routes (all behind the delta bearer token):
//
//	GET    /cloud-accounts              [{"id", "email", "plan", "added"}], in the order the phone tries them.
//	POST   /cloud-accounts              Add (or re-add) one from {"refresh_token", "access_token"?, "expires_at"?}.
//	DELETE /cloud-accounts/{id}         Remove one.
//	POST   /cloud-accounts/{id}/token   {"access_token", "expires_at"}. A body of {"refused": <token>} says
//	                                    the provider turned that token down, so a new one is needed.
//	GET    /cloud-accounts/{id}/usage   The provider's usage report for the account, as it sends it.
//
// Times are milliseconds since the epoch. The file is JSON, readable only by
// the command center's user.
const (
	cloudAccountsFile = "cloud-accounts.json"
	maxCloudAccount   = 16 << 10

	cloudClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// Asked for on every renewal, as the provider's own CLI does: the
	// sessions scope is what lets a login drive cloud sessions at all.
	cloudScopes = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	cloudBeta   = "oauth-2025-04-20"

	// A token handed out is good for at least this long. The phone renews
	// five minutes before expiry, so it never gets one it would renew at once.
	cloudRenewBefore = 15 * time.Minute
)

// cloudUpstream is where logins are renewed and accounts looked up. The
// self-test points it at a fake.
var cloudUpstream = struct{ tokenURL, apiURL string }{
	tokenURL: "https://platform.claude.com/v1/oauth/token",
	apiURL:   "https://api.anthropic.com",
}

type cloudAccount struct {
	ID           string    `json:"id"` // the provider's account UUID
	Email        string    `json:"email"`
	Plan         string    `json:"plan,omitempty"` // e.g. "claude_pro"
	Added        time.Time `json:"added"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    int64     `json:"expires_at"`
}

// cloudAccountInfo is an account as it is listed: everything but the login.
type cloudAccountInfo struct {
	ID    string    `json:"id"`
	Email string    `json:"email"`
	Plan  string    `json:"plan,omitempty"`
	Added time.Time `json:"added"`
}

func (a *cloudAccount) info() cloudAccountInfo {
	return cloudAccountInfo{ID: a.ID, Email: a.Email, Plan: a.Plan, Added: a.Added}
}

// errCloudRejected means the provider turned a login down for good.
var errCloudRejected = errors.New("rejected")

type cloudAccountStore struct {
	path             string
	tokenURL, apiURL string
	client           *http.Client

	mu       sync.Mutex             // the file
	renewing map[string]*sync.Mutex // one per account, guarded by mu
}

func newCloudAccountStore(path string) *cloudAccountStore {
	return &cloudAccountStore{
		path:     path,
		tokenURL: cloudUpstream.tokenURL,
		apiURL:   cloudUpstream.apiURL,
		client:   &http.Client{Timeout: 30 * time.Second},
		renewing: map[string]*sync.Mutex{},
	}
}

func (s *cloudAccountStore) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /cloud-accounts", s.list)
	mux.HandleFunc("POST /cloud-accounts", s.add)
	mux.HandleFunc("DELETE /cloud-accounts/{id}", s.remove)
	mux.HandleFunc("POST /cloud-accounts/{id}/token", s.token)
	mux.HandleFunc("GET /cloud-accounts/{id}/usage", s.usage)
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

func (s *cloudAccountStore) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	accounts, err := s.load()
	s.mu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	infos := []cloudAccountInfo{}
	for i := range accounts {
		infos = append(infos, accounts[i].info())
	}
	writeJSON(w, http.StatusOK, infos)
}

func (s *cloudAccountStore) add(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCloudAccount))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("login exceeds %d bytes", maxCloudAccount))
		return
	}
	var in struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		writeError(w, http.StatusBadRequest, `expected {"refresh_token", "access_token", "expires_at"}`)
		return
	}
	a := cloudAccount{
		AccessToken:  strings.TrimSpace(in.AccessToken),
		RefreshToken: strings.TrimSpace(in.RefreshToken),
		ExpiresAt:    in.ExpiresAt,
	}
	if !cloudCredential(a.RefreshToken) || (a.AccessToken != "" && !cloudCredential(a.AccessToken)) {
		writeError(w, http.StatusBadRequest, "the login needs a refresh token (and may have an access token), with no spaces")
		return
	}
	if err := s.identify(r.Context(), &a); errors.Is(err, errCloudRejected) {
		writeError(w, http.StatusForbidden, "the login was rejected; sign in again with `delta accounts login`")
		return
	} else if err != nil {
		writeError(w, http.StatusBadGateway, "couldn't check the login: "+err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	accounts, err := s.load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusCreated
	a.Added = time.Now().UTC()
	replaced := false
	for i := range accounts {
		if accounts[i].ID == a.ID {
			// The same account again: the new login replaces the old one in
			// its place, so the order the phone tries them in stays put.
			a.Added = accounts[i].Added
			accounts[i] = a
			status, replaced = http.StatusOK, true
		}
	}
	if !replaced {
		accounts = append(accounts, a)
	}
	if err := s.save(accounts); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, status, a.info())
}

func (s *cloudAccountStore) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts, err := s.load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	kept := accounts[:0]
	var removed *cloudAccount
	for i := range accounts {
		if accounts[i].ID == id {
			a := accounts[i]
			removed = &a
			continue
		}
		kept = append(kept, accounts[i])
	}
	if removed == nil {
		writeError(w, http.StatusNotFound, "no such account")
		return
	}
	if err := s.save(kept); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, removed.info())
}

func (s *cloudAccountStore) token(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Refused string `json:"refused"`
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCloudAccount))
	if err == nil && len(bytes.TrimSpace(data)) > 0 {
		err = json.Unmarshal(data, &in)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, `expected nothing, or {"refused": <access token>}`)
		return
	}
	a, err := s.fresh(r.Context(), r.PathValue("id"), in.Refused)
	if err != nil {
		writeCloudError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": a.AccessToken, "expires_at": a.ExpiresAt})
}

func (s *cloudAccountStore) usage(w http.ResponseWriter, r *http.Request) {
	a, err := s.fresh(r.Context(), r.PathValue("id"), "")
	if err != nil {
		writeCloudError(w, err)
		return
	}
	status, body, err := s.api(r.Context(), "/api/oauth/usage", a.AccessToken)
	if err == nil && status == http.StatusUnauthorized {
		// Turned down before its time: renew once and ask again.
		if a, err = s.fresh(r.Context(), a.ID, a.AccessToken); err == nil {
			status, body, err = s.api(r.Context(), "/api/oauth/usage", a.AccessToken)
		}
	}
	if err != nil {
		writeCloudError(w, err)
		return
	}
	if status != http.StatusOK {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("the usage report answered HTTP %d", status))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

var errCloudUnknown = errors.New("no such account")

func writeCloudError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errCloudUnknown):
		writeError(w, http.StatusNotFound, "no such account")
	case errors.Is(err, errCloudRejected):
		writeError(w, http.StatusForbidden, "the account's login was rejected; sign it in again with `delta accounts login`")
	default:
		writeError(w, http.StatusBadGateway, err.Error())
	}
}

// ---------------------------------------------------------------------------
// Logins
// ---------------------------------------------------------------------------

// fresh returns the account with an access token good for a while yet,
// renewing its login first if it is close to expiring or if `refused` (a
// token the provider turned down) is still the current one. Renewals of one
// account are taken one at a time, so two callers never both spend the same
// refresh token; the second gets what the first renewed.
func (s *cloudAccountStore) fresh(ctx context.Context, id, refused string) (*cloudAccount, error) {
	s.mu.Lock()
	lock := s.renewing[id]
	if lock == nil {
		lock = &sync.Mutex{}
		s.renewing[id] = lock
	}
	s.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()

	a, err := s.find(id)
	if err != nil {
		return nil, err
	}
	good := a.AccessToken != "" && a.AccessToken != refused &&
		time.Until(time.UnixMilli(a.ExpiresAt)) > cloudRenewBefore
	if good {
		return a, nil
	}
	t, err := s.renew(ctx, a.RefreshToken)
	if err != nil {
		return nil, err
	}
	a.AccessToken, a.ExpiresAt = t.access, t.expiresAt
	if t.refresh != "" {
		a.RefreshToken = t.refresh
	}
	err = s.update(id, func(stored *cloudAccount) {
		stored.AccessToken, stored.RefreshToken, stored.ExpiresAt = a.AccessToken, a.RefreshToken, a.ExpiresAt
	})
	if err != nil {
		// The old refresh token is spent, so this is the only copy left of
		// the new one. Keep using it while this process runs.
		log.Printf("cloud account %s: renewed login not saved: %v", id, err)
	}
	return a, nil
}

// identify fills in an account's ID, email and plan, renewing the login
// first unless its access token is still good. Any login is renewed on first
// use anyway, so this also proves that the refresh token works.
func (s *cloudAccountStore) identify(ctx context.Context, a *cloudAccount) error {
	if a.AccessToken != "" && time.Until(time.UnixMilli(a.ExpiresAt)) > cloudRenewBefore {
		err := s.profile(ctx, a)
		if !errors.Is(err, errCloudRejected) {
			return err
		}
	}
	t, err := s.renew(ctx, a.RefreshToken)
	if err != nil {
		return err
	}
	a.AccessToken, a.ExpiresAt = t.access, t.expiresAt
	if t.refresh != "" {
		a.RefreshToken = t.refresh
	}
	return s.profile(ctx, a)
}

// profile looks up whose login a.AccessToken is.
func (s *cloudAccountStore) profile(ctx context.Context, a *cloudAccount) error {
	status, body, err := s.api(ctx, "/api/oauth/profile", a.AccessToken)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return errCloudRejected
	}
	if status != http.StatusOK {
		return fmt.Errorf("the profile lookup answered HTTP %d", status)
	}
	var p struct {
		Account struct {
			UUID  string `json:"uuid"`
			Email string `json:"email_address"`
		} `json:"account"`
		Organization struct {
			Type string `json:"organization_type"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(body, &p); err != nil || !cloudID(p.Account.UUID) {
		return errors.New("the profile lookup sent something unreadable")
	}
	a.ID, a.Email, a.Plan = p.Account.UUID, p.Account.Email, p.Organization.Type
	return nil
}

type cloudTokens struct {
	access, refresh string
	expiresAt       int64
}

// renew trades a refresh token for a new access token and, usually, a new
// refresh token; the old one stops working once it has been used.
func (s *cloudAccountStore) renew(ctx context.Context, refresh string) (cloudTokens, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     cloudClientID,
		"refresh_token": refresh,
		"scope":         cloudScopes,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, bytes.NewReader(body))
	if err != nil {
		return cloudTokens{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return cloudTokens{}, fmt.Errorf("couldn't reach the login server: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return cloudTokens{}, errCloudRejected
	case resp.StatusCode != http.StatusOK:
		return cloudTokens{}, fmt.Errorf("the login server answered HTTP %d", resp.StatusCode)
	}
	var t struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &t); err != nil || !cloudCredential(t.AccessToken) || t.ExpiresIn <= 0 {
		return cloudTokens{}, errors.New("the login server sent something unreadable")
	}
	return cloudTokens{
		access:    t.AccessToken,
		refresh:   strings.TrimSpace(t.RefreshToken),
		expiresAt: time.Now().Add(time.Duration(t.ExpiresIn * float64(time.Second))).UnixMilli(),
	}, nil
}

// api is a GET on the provider's API with an account's access token.
func (s *cloudAccountStore) api(ctx context.Context, path, access string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", cloudBeta)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("couldn't reach the provider: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}

// cloudCredential is a token that can go in a header as it is.
func cloudCredential(v string) bool {
	return v != "" && len(v) <= 4096 && !strings.ContainsFunc(v, func(r rune) bool { return r <= ' ' || r >= 0x7f })
}

// cloudID is an account UUID, which goes in a route.
func cloudID(v string) bool {
	return v != "" && len(v) <= 64 && !strings.ContainsFunc(v, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	})
}

// ---------------------------------------------------------------------------
// The file
// ---------------------------------------------------------------------------

// find returns a copy of one account.
func (s *cloudAccountStore) find(id string) (*cloudAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts, err := s.load()
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		if accounts[i].ID == id {
			a := accounts[i]
			return &a, nil
		}
	}
	return nil, errCloudUnknown
}

// update changes one account in place. One removed meanwhile stays removed.
func (s *cloudAccountStore) update(id string, change func(*cloudAccount)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts, err := s.load()
	if err != nil {
		return err
	}
	for i := range accounts {
		if accounts[i].ID == id {
			change(&accounts[i])
			return s.save(accounts)
		}
	}
	return nil
}

// load reads every account; none while the file doesn't exist. Callers hold mu.
func (s *cloudAccountStore) load() ([]cloudAccount, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var accounts []cloudAccount
	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, fmt.Errorf("malformed %s: %v", cloudAccountsFile, err)
	}
	return accounts, nil
}

// save replaces the file. Callers hold mu.
func (s *cloudAccountStore) save(accounts []cloudAccount) error {
	if accounts == nil {
		accounts = []cloudAccount{}
	}
	data, _ := json.MarshalIndent(accounts, "", "  ")
	return writePrivateFile(s.path, append(data, '\n'))
}
