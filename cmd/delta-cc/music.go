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

// The music downloading configuration, which every music run hands
// to the download engine (see music_run.go). It is an INI-style file: "key =
// value" lines, "#" comments, and "[profile]" sections. It never holds a
// login: the command center keeps its own account in music-login.json, so
// configs that name one are rejected.
//
// Routes (all behind the delta bearer token):
//
//	GET    /music-config   The configuration as text/plain. X-Delta-Default is
//	                       "true" while the built-in default is in use.
//	PUT    /music-config   Replace it with the request body.
//	DELETE /music-config   Go back to the default.
const (
	musicConfigFile = "music.conf"
	maxMusicConfig  = 64 << 10
)

const defaultMusicConfig = `output-dir = ~/Music/sockseek

format      = mp3
min-bitrate = 256

pref-format      = mp3
pref-min-bitrate = 256
pref-max-bitrate = 320

name-format = {artist( - )title|filename}

[albums]
profile-cond = album
name-format  = {albumartist( - )album|artist( - )album|slsk-foldername}/{artist( - )title|filename}
`

// musicLoginKeys are the config keys that would set the Soulseek login.
var musicLoginKeys = map[string]bool{
	"user": true, "username": true, "pass": true, "password": true, "login": true, "l": true,
}

// checkMusicConfig rejects text the download engine can't parse, and any
// login. The engine's own parser accepts the same syntax.
func checkMusicConfig(text string) error {
	if strings.ContainsRune(text, 0) {
		return errors.New("config contains a NUL byte")
	}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.TrimSpace(val) == "" {
			return fmt.Errorf("line %d: expected \"key = value\", \"[profile]\" or a # comment", i+1)
		}
		if musicLoginKeys[strings.ToLower(key)] {
			return fmt.Errorf("line %d: %q is not allowed; the command center keeps its own login", i+1, key)
		}
	}
	return nil
}

type musicConfig struct{ path string }

func (m *musicConfig) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /music-config", m.get)
	mux.HandleFunc("PUT /music-config", m.put)
	mux.HandleFunc("DELETE /music-config", m.reset)
}

// text returns the configuration in use.
func (m *musicConfig) text() (string, error) {
	text, _, err := m.read()
	return text, err
}

func (m *musicConfig) read() (text string, isDefault bool, err error) {
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultMusicConfig, true, nil
	}
	return string(data), false, err
}

func (m *musicConfig) get(w http.ResponseWriter, r *http.Request) {
	text, isDefault, err := m.read()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Delta-Default", fmt.Sprint(isDefault))
	io.WriteString(w, text)
}

func (m *musicConfig) put(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMusicConfig))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("config exceeds %d bytes", maxMusicConfig))
		return
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if err := checkMusicConfig(text); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".music-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	_, err = io.WriteString(tmp, text)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), m.path)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"size": len(text)})
}

func (m *musicConfig) reset(w http.ResponseWriter, r *http.Request) {
	if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"default": true})
}
