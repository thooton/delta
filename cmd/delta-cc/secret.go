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

// A secretStore keeps one secret string (a token or an API key) in a file in
// the data directory, readable only by the command center's user, and serves
// it on one route:
//
//	GET <route>   {<field>: ...}; 404 with the missing message while none is set.
//	PUT <route>   Replace it with the secret as the raw body.
const maxSecret = 1 << 10

type secretStore struct {
	path    string // file in the data directory
	route   string // e.g. "/personal-access-token"
	field   string // JSON field GET returns it in
	what    string // what it is, for messages
	missing string // the 404 message while none is set
}

func (s *secretStore) register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+s.route, s.get)
	mux.HandleFunc("PUT "+s.route, s.put)
}

func (s *secretStore) get(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, s.missing)
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{s.field: strings.TrimSpace(string(data))})
}

func (s *secretStore) put(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSecret))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("%s exceeds %d bytes", s.what, maxSecret))
		return
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" || strings.ContainsFunc(secret, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the %s must be non-empty and contain no spaces or control characters", s.what))
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "."+filepath.Base(s.path)+"-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(secret + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.path)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
}
