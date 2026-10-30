package main

import (
	"fmt"
	"net/http"
	"os"
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

// read returns the secret, or an error satisfying os.ErrNotExist while none
// is set.
func (s *secretStore) read() (string, error) {
	data, err := os.ReadFile(s.path)
	return strings.TrimSpace(string(data)), err
}

func (s *secretStore) get(w http.ResponseWriter, r *http.Request) {
	secret, err := s.read()
	if err != nil {
		writeReadError(w, err, s.missing)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{s.field: secret})
}

func (s *secretStore) put(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r, maxSecret, s.what+" exceeds %d bytes")
	if !ok {
		return
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" || strings.ContainsFunc(secret, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the %s must be non-empty and contain no spaces or control characters", s.what))
		return
	}
	writeResult(w, writePrivateFile(s.path, []byte(secret+"\n")), http.StatusOK, map[string]bool{"saved": true})
}
