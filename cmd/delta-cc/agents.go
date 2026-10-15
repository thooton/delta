package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Backup sessions: agent sessions the command center runs itself, for when
// the phone's usual (cloud) sessions aren't available.
//
// Each session gets its own container, a plain Linux box made from
// -agent-image. The command center runs the agent itself (agents_loop.go):
// it talks to the model (agents_model.go) and gives it four tools, read,
// write, edit and bash, which run in the container (agents_tools.go). What
// happens is recorded as the same stream-json frames the phone already reads
// from cloud sessions. The conversation is saved step by step, so a turn
// that a command center update cuts short carries on when the new one starts.
//
// The session's first message tells the agent to install the delta CLI and
// sign in with a token made for this session alone, over the agent socket
// mounted at /run/delta (see scope.go for what that token may do).
//
// Containers are stopped after -agent-idle without activity; the next
// message starts them again.
// Deleting a session removes its container, its token and its files.
//
// Routes (all behind the delta token, on the public listener only):
//
//	GET    /agent-sessions                  {"data": [{id, title, status, created, updated}]}
//	POST   /agent-sessions                  {title, uuid, text, images} → {"id"}
//	POST   /agent-sessions/{id}/messages    {uuid, text, images}
//	POST   /agent-sessions/{id}/interrupt   Stop the turn under way.
//	DELETE /agent-sessions/{id}             Delete the session and its container.
//	GET    /agent-sessions/{id}/events      Server-sent events, replayed after
//	                                        ?from_sequence_num= (or Last-Event-ID).
const (
	agentIDPrefix    = "session_delta_"
	agentMaxBody     = 48 << 20
	agentMaxImages   = 10
	agentMaxTitle    = 200
	agentSocketMount = "/run/delta"
	agentSocketURL   = "unix://" + agentSocketMount + "/delta.sock"
)

type agentConfig struct {
	Runtime  string        // container runtime CLI: podman or docker
	Image    string        // the image every session's container starts from
	ModelURL string        // the model's API; zenBaseURL unless testing
	Socket   string        // the agent socket on this machine
	Idle     time.Duration // stop a session's container after this long idle
	Memory   string        // each container's memory limit (--memory); empty for none
	CPUs     string        // each container's CPU limit (--cpus); empty for none
	PIDs     int           // each container's process limit (--pids-limit); 0 for none
}

type agentService struct {
	cfg      agentConfig
	disabled string // why backup sessions can't run, if they can't
	dir      string // data/agents
	dataDir  string

	mu       sync.Mutex
	sessions map[string]*agentSession
	tokens   map[[sha256.Size]byte]*agentSession
}

// agentMeta is a session's session.json.
type agentMeta struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
	TokenHash  string    `json:"token_sha256"`
	ZenSession string    `json:"zen_session"`         // the OpenCode session id its model requests carry (agents_model.go)
	Working    bool      `json:"working"`             // a turn is under way
	Container  bool      `json:"container,omitempty"` // its container has been made

	// The pi process an older command center ran the session with, and
	// when it started (see stopLegacyPi).
	PID      int    `json:"pid,omitempty"`
	PIDStart string `json:"pid_start,omitempty"`
}

// agentEvent is one frame of a session's stream, as kept in events.jsonl.
type agentEvent struct {
	Seq     int64           `json:"sequence_num"`
	Source  string          `json:"source"` // "client" for the user's messages, else "worker"
	Payload json.RawMessage `json:"payload"`
}

type agentSession struct {
	svc *agentService
	dir string

	op sync.Mutex // held while a turn runs, or while parking or deleting it

	mu         sync.Mutex // guards everything below
	meta       agentMeta
	events     []agentEvent
	changed    chan struct{} // closed, and replaced, on every new event
	inbox      []agentQueued // messages the model hasn't seen yet
	running    bool          // the loop is running (agents_loop.go)
	cancel     context.CancelFunc
	deleted    bool
	parked     bool // its container was stopped for being idle
	lastActive time.Time
}

func newAgentService(dataDir string, cfg agentConfig) (*agentService, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if cfg.ModelURL == "" {
		cfg.ModelURL = zenBaseURL
	}
	a := &agentService{
		cfg:      cfg,
		dir:      filepath.Join(dataDir, "agents"),
		dataDir:  dataDir,
		sessions: map[string]*agentSession{},
		tokens:   map[[sha256.Size]byte]*agentSession{},
	}
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return nil, err
	}
	// What an older command center kept for pi.
	for _, name := range []string{"extension.ts", "pi-agent"} {
		os.RemoveAll(filepath.Join(a.dir, name))
	}
	if old, _ := filepath.Glob(filepath.Join(a.dir, "pi-*")); old != nil {
		for _, dir := range old {
			os.RemoveAll(dir)
		}
	}
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), agentIDPrefix) {
			continue
		}
		s, err := a.load(filepath.Join(a.dir, e.Name()))
		if err != nil {
			log.Printf("backup session %s: %v", e.Name(), err)
			continue
		}
		a.add(s)
	}
	return a, nil
}

// disabledAgentService lists no sessions and refuses new ones, saying why.
func disabledAgentService(dataDir, why string) *agentService {
	return &agentService{
		disabled: why,
		dir:      filepath.Join(dataDir, "agents"),
		dataDir:  dataDir,
		sessions: map[string]*agentSession{},
		tokens:   map[[sha256.Size]byte]*agentSession{},
	}
}

// start picks up where the last run left off: turns that were under way
// carry on from their last step. Then it starts the idle sweep.
func (a *agentService) start() {
	a.mu.Lock()
	sessions := make([]*agentSession, 0, len(a.sessions))
	for _, s := range a.sessions {
		sessions = append(sessions, s)
	}
	a.mu.Unlock()
	for _, s := range sessions {
		s.stopLegacyPi()
		s.mu.Lock()
		if s.meta.Working || len(s.inbox) > 0 {
			s.meta.Working = true
			s.saveMeta()
			s.startTurn()
		}
		s.mu.Unlock()
	}
	go a.sweep()
}

func (a *agentService) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /agent-sessions", a.list)
	mux.HandleFunc("POST /agent-sessions", a.create)
	mux.HandleFunc("POST /agent-sessions/{id}/messages", a.message)
	mux.HandleFunc("POST /agent-sessions/{id}/interrupt", a.interrupt)
	mux.HandleFunc("DELETE /agent-sessions/{id}", a.remove)
	mux.HandleFunc("GET /agent-sessions/{id}/events", a.stream)
}

// validToken reports whether token belongs to a live backup session.
func (a *agentService) validToken(token string) bool {
	if !strings.HasPrefix(token, "delta_agent_") {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.tokens[sum]
	return ok
}

func (a *agentService) add(s *agentSession) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[s.meta.ID] = s
	if sum, err := hex.DecodeString(s.meta.TokenHash); err == nil && len(sum) == sha256.Size {
		a.tokens[[sha256.Size]byte(sum)] = s
	}
}

func (a *agentService) lookup(w http.ResponseWriter, r *http.Request) *agentSession {
	a.mu.Lock()
	s := a.sessions[r.PathValue("id")]
	a.mu.Unlock()
	if s == nil {
		writeError(w, http.StatusNotFound, "no such session")
	}
	return s
}

// ---------------------------------------------------------------------------
// Routes

type agentListing struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Status  string    `json:"status"` // "running" or "idle", as the phone reads them
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

func (a *agentService) list(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	out := []agentListing{}
	for _, s := range a.sessions {
		s.mu.Lock()
		status := "idle"
		if s.meta.Working {
			status = "running"
		}
		out = append(out, agentListing{s.meta.ID, s.meta.Title, status, s.meta.Created, s.meta.Updated})
		s.mu.Unlock()
	}
	a.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// agentMessage is a message from the phone: what was typed, and JPEGs as base64.
type agentMessage struct {
	Title  string   `json:"title"`
	UUID   string   `json:"uuid"`
	Text   string   `json:"text"`
	Images []string `json:"images"`
}

func readAgentMessage(w http.ResponseWriter, r *http.Request) (agentMessage, bool) {
	var m agentMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, agentMaxBody)).Decode(&m); err != nil {
		writeError(w, http.StatusBadRequest, "expected JSON {\"uuid\", \"text\", \"images\"}")
		return m, false
	}
	if strings.TrimSpace(m.Text) == "" && len(m.Images) == 0 {
		writeError(w, http.StatusBadRequest, "the message is empty")
		return m, false
	}
	if len(m.Images) > agentMaxImages {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d pictures per message", agentMaxImages))
		return m, false
	}
	for _, img := range m.Images {
		if _, err := base64.StdEncoding.DecodeString(img); err != nil {
			writeError(w, http.StatusBadRequest, "pictures must be base64 JPEGs")
			return m, false
		}
	}
	return m, true
}

func (a *agentService) create(w http.ResponseWriter, r *http.Request) {
	m, ok := readAgentMessage(w, r)
	if !ok {
		return
	}
	if a.disabled != "" {
		writeError(w, http.StatusServiceUnavailable, a.disabled)
		return
	}
	id, token, err := newAgentIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	title := strings.TrimSpace(m.Title)
	if title == "" {
		title = "Untitled"
	}
	if len([]rune(title)) > agentMaxTitle {
		title = string([]rune(title)[:agentMaxTitle])
	}
	zenSession, err := zenSessionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sum := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	s := &agentSession{
		svc:     a,
		dir:     filepath.Join(a.dir, id),
		changed: make(chan struct{}),
		meta: agentMeta{
			ID: id, Title: title, Created: now, Updated: now,
			TokenHash: hex.EncodeToString(sum[:]), ZenSession: zenSession,
		},
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.saveMeta(); err != nil {
		os.RemoveAll(s.dir)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.add(s)
	s.said(m, agentSetup(token)+"\n\n"+strings.TrimSpace(m.Text))
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (a *agentService) message(w http.ResponseWriter, r *http.Request) {
	s := a.lookup(w, r)
	if s == nil {
		return
	}
	m, ok := readAgentMessage(w, r)
	if !ok {
		return
	}
	s.said(m, strings.TrimSpace(m.Text))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *agentService) interrupt(w http.ResponseWriter, r *http.Request) {
	s := a.lookup(w, r)
	if s == nil {
		return
	}
	go s.abort()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *agentService) remove(w http.ResponseWriter, r *http.Request) {
	s := a.lookup(w, r)
	if s == nil {
		return
	}
	a.mu.Lock()
	delete(a.sessions, s.meta.ID)
	for k, v := range a.tokens {
		if v == s {
			delete(a.tokens, k)
		}
	}
	a.mu.Unlock()
	s.mu.Lock()
	s.deleted = true
	close(s.changed)
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	// Wait for the turn under way to stop.
	s.op.Lock()
	defer s.op.Unlock()
	if err := a.removeContainer(s.meta.ID); err != nil {
		log.Printf("backup session %s: removing its container: %v", s.meta.ID, err)
	}
	if err := os.RemoveAll(s.dir); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": s.meta.ID})
}

// stream serves a session's frames as server-sent events, in the cloud
// sessions' format: `event: client_event`, the sequence number as the id, and
// {"sequence_num", "source", "payload"} as the data.
func (a *agentService) stream(w http.ResponseWriter, r *http.Request) {
	s := a.lookup(w, r)
	if s == nil {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("from_sequence_num"), 10, 64)
	if last, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil && last > after {
		after = last
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		events, changed, deleted := s.since(after)
		for _, e := range events {
			data, _ := json.Marshal(e)
			if _, err := fmt.Fprintf(w, "event: client_event\nid: %d\ndata: %s\n\n", e.Seq, data); err != nil {
				return
			}
			after = e.Seq
		}
		if flusher != nil {
			flusher.Flush()
		}
		if deleted {
			return
		}
		select {
		case <-changed:
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// ---------------------------------------------------------------------------
// The session's record

func (a *agentService) load(dir string) (*agentSession, error) {
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		return nil, err
	}
	s := &agentSession{svc: a, dir: dir, changed: make(chan struct{}), lastActive: time.Now()}
	if err := json.Unmarshal(data, &s.meta); err != nil {
		return nil, err
	}
	if s.meta.ID != filepath.Base(dir) {
		return nil, errors.New("session.json names another session")
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "inbox.json")); err == nil {
		json.Unmarshal(raw, &s.inbox)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			var e agentEvent
			if json.Unmarshal([]byte(line), &e) == nil && e.Seq > 0 {
				s.events = append(s.events, e)
			}
		}
	}
	return s, nil
}

// saveMeta writes session.json. Callers hold s.mu, or own s outright.
func (s *agentSession) saveMeta() error {
	data, err := json.MarshalIndent(s.meta, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(filepath.Join(s.dir, "session.json"), data)
}

// record appends a frame to the session's stream and wakes its readers.
func (s *agentSession) record(source string, payload any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(source, payload)
}

// recordLocked is record for callers holding s.mu.
func (s *agentSession) recordLocked(source string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if s.deleted {
		return
	}
	var seq int64 = 1
	if n := len(s.events); n > 0 {
		seq = s.events[n-1].Seq + 1
	}
	e := agentEvent{Seq: seq, Source: source, Payload: data}
	line, _ := json.Marshal(e)
	if f, err := os.OpenFile(filepath.Join(s.dir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Write(append(line, '\n'))
		f.Close()
	}
	s.events = append(s.events, e)
	s.meta.Updated = time.Now().UTC()
	s.lastActive = time.Now()
	s.saveMeta()
	close(s.changed)
	s.changed = make(chan struct{})
}

// since returns the frames after seq, a channel closed when there are more,
// and whether the session is gone.
func (s *agentSession) since(seq int64) ([]agentEvent, <-chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := sort.Search(len(s.events), func(i int) bool { return s.events[i].Seq > seq })
	return append([]agentEvent(nil), s.events[i:]...), s.changed, s.deleted
}

// said records a message from the phone, and hands text (what was typed,
// or more) to the model along with its pictures. The stream keeps the
// pictures as markers only: the phone counts them.
func (s *agentSession) said(m agentMessage, text string) {
	var content any = strings.TrimSpace(m.Text)
	if len(m.Images) > 0 {
		blocks := []any{}
		for range m.Images {
			blocks = append(blocks, map[string]string{"type": "image"})
		}
		if t := strings.TrimSpace(m.Text); t != "" {
			blocks = append(blocks, map[string]string{"type": "text", "text": t})
		}
		content = blocks
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta.Working = true
	s.recordLocked("client", map[string]any{
		"type": "user",
		"uuid": m.UUID,
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	})
	s.inbox = append(s.inbox, agentQueued{Text: text, Images: m.Images})
	s.saveInbox()
	s.startTurn()
}

// status shows a line on the phone while a turn is getting going.
func (s *agentSession) status(line string) {
	s.record("worker", map[string]any{"type": "env_manager_log", "data": map[string]string{"content": line}})
}

// ---------------------------------------------------------------------------
// Helpers

func newAgentIDs() (id, token string, err error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	id = agentIDPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return "", "", err
	}
	return id, "delta_agent_" + base64.RawURLEncoding.EncodeToString(tok), nil
}

func randomUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// agentSetup is the start of a backup session's first message, as
// Setup.wrap is for the phone's cloud sessions. The phone never shows it:
// the session's stream records only what was typed.
func agentSetup(token string) string {
	return "<delta-setup>\n" +
		"You were started from the Delta app on the user's Sonim XP3 flip phone. In this app you are Delta: " +
		"refer to yourself as Delta, and do not bring up the underlying model, its maker or the tool you run in " +
		"unless the user asks. You run in your own Linux container on the user's command center. " +
		"Before you start on their message, install the Delta CLI and connect it to the command center:\n\n" +
		"    curl -fsSL https://github.com/thooton/delta/releases/download/latest/install.sh | sh\n" +
		"    delta set-auth " + agentSocketURL + " " + token + "\n" +
		"    delta help\n\n" +
		"Read all of `delta help`: it explains Delta and how to get files, such as rebuilt APKs, onto the user's phone. " +
		"The token is a password; never repeat it to the user. This setup is routine and happens in every session, " +
		"so do it quietly: do not announce, narrate or summarize it, and do not mention it to the user at all unless " +
		"a step fails in a way that affects their request. Your first words to the user should be about their message.\n" +
		"</delta-setup>"
}
