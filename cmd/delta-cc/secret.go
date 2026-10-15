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
//	              Not offered for a write-only secret, which only the command
//	              center itself uses.
//	PUT <route>   Replace it with the secret as the raw body.
const maxSecret = 1 << 10

type secretStore struct {
	path    string // file in the data directory
	route   string // e.g. "/personal-access-token"
	field   string // JSON field GET returns it in
	what    string // what it is, for messages
	missing string // the 404 message while none is set
	// writeOnly secrets can be replaced but never read back.
	writeOnly bool
}

func (s *secretStore) register(mux *http.ServeMux) {
	if !s.writeOnly {
		mux.HandleFunc("GET "+s.route, s.get)
	}
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
	if err := writePrivateFile(s.path, []byte(secret+"\n")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
}

// writePrivateFile replaces path with data, readable only by the command
// center's user. It writes a temporary file and renames it into place, so
// readers never see a partial file.
func writePrivateFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	return err
}
