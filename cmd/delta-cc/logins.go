package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// A loginStore keeps one login (an account's details, as JSON) in a file in
// the data directory, readable only by the command center's user, and
// serves it on one route (all behind the delta bearer token):
//
//	GET    <route>   The login; 404 with the missing message while none is set.
//	PUT    <route>   Replace it with a body of the same shape.
//	DELETE <route>   Remove it.
type loginStore[T any] struct {
	path    string // file in the data directory
	route   string // e.g. "/books-login"
	max     int64  // the largest body PUT takes
	missing string // the 404 message while none is set
	shape   string // the PUT message for a body that isn't the login's JSON
	invalid string // the PUT message for a login that tidy refuses
	// tidy trims what was given and returns the login's email address, or
	// false when something is missing.
	tidy func(*T) (email string, ok bool)
}

func (s *loginStore[T]) register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+s.route, s.get)
	mux.HandleFunc("PUT "+s.route, s.put)
	mux.HandleFunc("DELETE "+s.route, s.reset)
}

func (s *loginStore[T]) get(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, s.missing)
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var l T
	if err := json.Unmarshal(data, &l); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("malformed %s: %v", filepath.Base(s.path), err))
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *loginStore[T]) put(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.max))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("login exceeds %d bytes", s.max))
		return
	}
	var l T
	if err := json.Unmarshal(data, &l); err != nil {
		writeError(w, http.StatusBadRequest, s.shape)
		return
	}
	email, ok := s.tidy(&l)
	if !ok {
		writeError(w, http.StatusBadRequest, s.invalid)
		return
	}
	out, _ := json.Marshal(l)
	if err := writePrivateFile(s.path, append(out, '\n')); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": email})
}

func (s *loginStore[T]) reset(w http.ResponseWriter, r *http.Request) {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
}

// The book source login. Source on the phone fetches it whenever it has to
// log in (it keeps the session it gets locally).
const (
	booksLoginFile = "books-login.json"
	maxBooksLogin  = 4 << 10
)

type booksLogin struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func booksLoginStore(dataDir string) *loginStore[booksLogin] {
	return &loginStore[booksLogin]{
		path:    filepath.Join(dataDir, booksLoginFile),
		route:   "/books-login",
		max:     maxBooksLogin,
		missing: "no book login is set; PUT the user's account to /books-login",
		shape:   `expected {"email": ..., "password": ...}`,
		invalid: "the login needs an email address and a password",
		tidy: func(l *booksLogin) (string, bool) {
			l.Email = strings.TrimSpace(l.Email)
			return l.Email, strings.Contains(l.Email, "@") && l.Password != ""
		},
	}
}

// The Gmail login: an OAuth client (from the user's Google Cloud project)
// and the refresh token the user granted it. Mail on the phone fetches it
// and trades the refresh token for an access token with Google itself, and
// the SMTP relay sends with it. `delta gmail login` replaces it and `delta
// gmail logout` removes it.
const (
	gmailLoginFile = "gmail-login.json"
	maxGmailLogin  = 8 << 10
)

type gmailLogin struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email"`
}

func gmailLoginStore(dataDir string) *loginStore[gmailLogin] {
	return &loginStore[gmailLogin]{
		path:    filepath.Join(dataDir, gmailLoginFile),
		route:   "/gmail-login",
		max:     maxGmailLogin,
		missing: "Gmail isn't set up; ask the user to run `delta gmail login` on a computer (see `delta gmail help`)",
		shape:   `expected {"client_id", "client_secret", "refresh_token", "email"}`,
		invalid: "the login needs a client ID, a client secret, a refresh token and an email address",
		tidy: func(l *gmailLogin) (string, bool) {
			for _, f := range []*string{&l.ClientID, &l.ClientSecret, &l.RefreshToken, &l.Email} {
				*f = strings.TrimSpace(*f)
			}
			return l.Email, l.ClientID != "" && l.ClientSecret != "" && l.RefreshToken != "" && strings.Contains(l.Email, "@")
		},
	}
}
