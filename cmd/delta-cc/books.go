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

// The book source login. `delta books` fetches it whenever it has to log in
// (it keeps the session it gets locally), and `delta books login set` replaces
// it. It is stored as JSON, readable only by the command center's user.
//
// Routes (all behind the delta bearer token):
//
//	GET    /books-login   {"email": ..., "password": ...}; 404 while none is set.
//	PUT    /books-login   Replace it with a body of the same shape.
//	DELETE /books-login   Remove it.
const (
	booksLoginFile = "books-login.json"
	maxBooksLogin  = 4 << 10
)

type booksLogin struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type booksLoginStore struct{ path string }

func (b *booksLoginStore) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /books-login", b.get)
	mux.HandleFunc("PUT /books-login", b.put)
	mux.HandleFunc("DELETE /books-login", b.reset)
}

func (b *booksLoginStore) get(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(b.path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no book login is set; run `delta books login set <email> -` with the user's account")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var l booksLogin
	if err := json.Unmarshal(data, &l); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("malformed %s: %v", booksLoginFile, err))
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (b *booksLoginStore) put(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBooksLogin))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("login exceeds %d bytes", maxBooksLogin))
		return
	}
	var l booksLogin
	if err := json.Unmarshal(data, &l); err != nil {
		writeError(w, http.StatusBadRequest, "expected {\"email\": ..., \"password\": ...}")
		return
	}
	l.Email = strings.TrimSpace(l.Email)
	if !strings.Contains(l.Email, "@") || l.Password == "" {
		writeError(w, http.StatusBadRequest, "the login needs an email address and a password")
		return
	}
	out, _ := json.Marshal(l)
	tmp, err := os.CreateTemp(filepath.Dir(b.path), ".books-login-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(out, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), b.path)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": l.Email})
}

func (b *booksLoginStore) reset(w http.ResponseWriter, r *http.Request) {
	if err := os.Remove(b.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
}
