package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The GitHub personal access token the agent uses for git and gh on the
// user's repositories. `delta get-personal-access-token` fetches it and
// `delta set-personal-access-token` replaces it. It is stored as plain text,
// readable only by the command center's user.
//
// Routes (all behind the delta bearer token):
//
//	GET /personal-access-token   {"token": ...}; 404 while none is set.
//	PUT /personal-access-token   Replace it with the token as the raw body.
const (
	accessTokenFile = "personal-access-token"
	maxAccessToken  = 1 << 10
)

type accessTokenStore struct{ path string }

func (a *accessTokenStore) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /personal-access-token", a.get)
	mux.HandleFunc("PUT /personal-access-token", a.put)
}

func (a *accessTokenStore) get(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no personal access token is set; ask the user for one and run `delta set-personal-access-token -`")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": strings.TrimSpace(string(data))})
}

func (a *accessTokenStore) put(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAccessToken))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("token exceeds %d bytes", maxAccessToken))
		return
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		writeError(w, http.StatusBadRequest, "the token must be non-empty and contain no spaces or control characters")
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(a.path), ".personal-access-token-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(token + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), a.path)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
}
