package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Conversations carried from one session to another. When a session's
// account runs out of usage, the website (as the retired phone app did) can move the conversation to a new
// session on another account: it reads the old session's whole history from
// the cloud, writes it out as text and stores it here under a random id. The
// new session is told the id and saves the conversation to a file with
// `delta resume <id> <file>`. They are kept, so a conversation can be resumed again.
//
// Routes (all behind the delta bearer token):
//
//	POST /conversations        Store the raw body; {"id"} with 201.
//	GET  /conversations        [{"id", "size", "modified", "title"}], newest first.
//	GET  /conversations/{id}   The conversation, as text.
const conversationsDir = "conversations"

type conversationStore struct {
	dir string
	max int64
}

func (c *conversationStore) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /conversations", c.put)
	mux.HandleFunc("GET /conversations", c.list)
	mux.HandleFunc("GET /conversations/{id}", c.get)
}

// conversationID is 16 lowercase letters and digits, so it is easy to read
// out and type, and can't name anything outside the directory.
func conversationID(id string) bool {
	return len(id) == 16 && !strings.ContainsFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '2' && r <= '7')
	})
}

func newConversationID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.EncodeToString(b))
}

func (c *conversationStore) put(w http.ResponseWriter, r *http.Request) {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmp, err := os.CreateTemp(c.dir, ".upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, c.max))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("conversation exceeds %d bytes", c.max))
		} else {
			writeError(w, http.StatusBadRequest, "upload failed: "+err.Error())
		}
		return
	}
	if n == 0 {
		writeError(w, http.StatusBadRequest, "the conversation is empty")
		return
	}
	for {
		id := newConversationID()
		// A link fails rather than replace, should two ids ever collide.
		err := os.Link(tmp.Name(), filepath.Join(c.dir, id+".txt"))
		if errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
		return
	}
}

func (c *conversationStore) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !conversationID(id) {
		writeError(w, http.StatusNotFound, "no such conversation")
		return
	}
	f, err := os.Open(filepath.Join(c.dir, id+".txt"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no such conversation")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.ServeContent(w, r, id+".txt", info.ModTime(), f)
}

type conversationInfo struct {
	ID       string    `json:"id"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Title    string    `json:"title"`
}

func (c *conversationStore) list(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(c.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	infos := []conversationInfo{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".txt")
		if !ok || !conversationID(id) || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		infos = append(infos, conversationInfo{ID: id, Size: info.Size(), Modified: info.ModTime().UTC(),
			Title: conversationTitle(filepath.Join(c.dir, e.Name()))})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Modified.After(infos[j].Modified) })
	writeJSON(w, http.StatusOK, infos)
}

// conversationTitle is the file's first line without its "# ", which the
// phone writes as the old session's title.
func conversationTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, _ := bufio.NewReader(io.LimitReader(f, 512)).ReadString('\n')
	return strings.TrimSpace(strings.TrimPrefix(line, "# "))
}
