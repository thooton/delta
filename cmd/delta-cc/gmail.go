package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// The Gmail login `delta gmail` uses: an OAuth client (from the user's Google
// Cloud project) and the refresh token the user granted it. The CLI fetches
// it on every run and trades the refresh token for an access token with
// Google itself; the command center only keeps it. `delta gmail login`
// replaces it and `delta gmail logout` removes it. It is stored as JSON,
// readable only by the command center's user.
//
// Routes (all behind the delta bearer token):
//
//	GET    /gmail-login   {"client_id", "client_secret", "refresh_token", "email"}; 404 while none is set.
//	PUT    /gmail-login   Replace it with a body of the same shape.
//	DELETE /gmail-login   Remove it.
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

type gmailLoginStore struct{ path string }

func (g *gmailLoginStore) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /gmail-login", g.get)
	mux.HandleFunc("PUT /gmail-login", g.put)
	mux.HandleFunc("DELETE /gmail-login", g.reset)
}

func (g *gmailLoginStore) get(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(g.path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "Gmail isn't set up; ask the user to run `delta gmail login` on a computer (see `delta gmail help`)")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var l gmailLogin
	if err := json.Unmarshal(data, &l); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("malformed %s: %v", gmailLoginFile, err))
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (g *gmailLoginStore) put(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxGmailLogin))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("login exceeds %d bytes", maxGmailLogin))
		return
	}
	var l gmailLogin
	if err := json.Unmarshal(data, &l); err != nil {
		writeError(w, http.StatusBadRequest, `expected {"client_id", "client_secret", "refresh_token", "email"}`)
		return
	}
	l.ClientID = strings.TrimSpace(l.ClientID)
	l.ClientSecret = strings.TrimSpace(l.ClientSecret)
	l.RefreshToken = strings.TrimSpace(l.RefreshToken)
	l.Email = strings.TrimSpace(l.Email)
	if l.ClientID == "" || l.ClientSecret == "" || l.RefreshToken == "" || !strings.Contains(l.Email, "@") {
		writeError(w, http.StatusBadRequest, "the login needs a client ID, a client secret, a refresh token and an email address")
		return
	}
	out, _ := json.Marshal(l)
	if err := writePrivateFile(g.path, append(out, '\n')); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": l.Email})
}

func (g *gmailLoginStore) reset(w http.ResponseWriter, r *http.Request) {
	if err := os.Remove(g.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
}
