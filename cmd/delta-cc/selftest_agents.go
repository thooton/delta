package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Backup sessions are tested with stand-ins for the model and the container
// runtime, so the self-test needs neither the network nor containers.

// fakeModel answers as the model would: a user's message gets a reply that
// writes a file, and a tool's result gets a closing reply. It checks each
// request is one the free tier would take, and keeps what it was sent.
type fakeModel struct {
	*httptest.Server
	mu       sync.Mutex
	problems []string
	outputs  map[string]string // tool results, by call id
}

var testModel *fakeModel

func newFakeModel() *fakeModel {
	m := &fakeModel{outputs: map[string]string{}}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	return m
}

func (m *fakeModel) problem(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.problems = append(m.problems, fmt.Sprintf(format, args...))
}

func (m *fakeModel) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model   string            `json:"model"`
		Store   *bool             `json:"store"`
		Include []string          `json:"include"`
		Input   []json.RawMessage `json:"input"`
		Tools   []struct {
			Name       string `json:"name"`
			Parameters struct {
				Properties map[string]any `json:"properties"`
			} `json:"parameters"`
		} `json:"tools"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil || r.URL.Path != "/responses" {
		m.problem("model request to %s: %v", r.URL.Path, err)
		http.Error(w, "bad request", 400)
		return
	}
	ses := r.Header.Get("x-opencode-session")
	switch {
	case r.Header.Get("Authorization") != "Bearer public":
		m.problem("model request has Authorization %q", r.Header.Get("Authorization"))
	case !strings.Contains(r.Header.Get("User-Agent"), "opencode/1."):
		m.problem("model request has User-Agent %q", r.Header.Get("User-Agent"))
	case !regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(ses) || r.Header.Get("x-opencode-session-id") != ses:
		m.problem("model request has OpenCode session %q / %q", ses, r.Header.Get("x-opencode-session-id"))
	case req.Model != agentModelID || !strings.HasSuffix(req.Model, "-free"):
		m.problem("model request asks for %q", req.Model)
	case req.Store == nil || *req.Store || len(req.Include) != 1:
		m.problem("model request should be stateless and ask for encrypted reasoning: %s", body[:min(len(body), 300)])
	}
	readOK := false
	for _, t := range req.Tools {
		readOK = readOK || (t.Name == "read" && len(t.Parameters.Properties) > 0)
	}
	if !readOK {
		m.problem("model request has no read tool with parameters")
	}
	if len(req.Input) < 2 || head(req.Input[0]).Role != "developer" {
		m.problem("model request doesn't start with the system prompt")
		http.Error(w, "bad request", 400)
		return
	}
	last := head(req.Input[len(req.Input)-1])
	var items []string
	switch {
	case last.Type == "message" && last.Role == "user":
		items = []string{
			`{"id":"rs_1","type":"reasoning","encrypted_content":"enc-thinking","summary":[{"type":"summary_text","text":"thinking it over"}]}`,
			`{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"pong","annotations":[]}]}`,
			`{"id":"fc_1","type":"function_call","status":"completed","name":"write","call_id":"call_w","arguments":"{\"path\":\"/root/a.txt\",\"content\":\"secret file contents\"}"}`,
		}
	case last.Type == "function_call_output":
		var out struct {
			Output string `json:"output"`
		}
		json.Unmarshal(req.Input[len(req.Input)-1], &out)
		m.mu.Lock()
		m.outputs[last.CallID] = out.Output
		m.mu.Unlock()
		if last.CallID == "call_w" && !bytes.Contains(body, []byte(`"encrypted_content":"enc-thinking"`)) {
			m.problem("the reasoning didn't come back with the tool's result")
		}
		items = []string{`{"id":"msg_2","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done","annotations":[]}]}`}
	default:
		m.problem("model request ends with %s", req.Input[len(req.Input)-1])
		http.Error(w, "bad request", 400)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
	// Out of order, as output_index allows.
	for i := len(items) - 1; i >= 0; i-- {
		fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", i, items[i])
	}
	fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
}

// fakeRuntime keeps each container's state in a file next to it, and logs
// every command.
const fakeRuntime = `#!/bin/sh
dir=$(dirname "$0")
echo "$*" >> "$dir/runtime.log"
case "$1" in
exec)
  # Keep what a command was given on stdin, so the test can see it.
  cat >> "$dir/exec-stdin" ;;
container)
  name=$5
  [ -f "$dir/c-$name" ] || { echo "Error: no such container $name" >&2; exit 1; }
  cat "$dir/c-$name" ;;
run)
  shift
  while [ "$1" != "--name" ]; do shift; done
  echo true > "$dir/c-$2"; echo "id-$2" ;;
start) echo true > "$dir/c-$2" ;;
stop) [ -f "$dir/c-$4" ] && echo false > "$dir/c-$4"; echo "$4" ;;
rm) rm -f "$dir/c-$3"; echo "$3" ;;
esac
`

func newTestAgents(dir string) (*agentService, error) {
	bin := filepath.Join(dir, "agent-bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(bin, "runtime"), []byte(fakeRuntime), 0o700); err != nil {
		return nil, err
	}
	testModel = newFakeModel()
	return newAgentService(dir, agentConfig{
		Runtime:  filepath.Join(bin, "runtime"),
		Image:    "example/image",
		ModelURL: testModel.URL,
		Socket:   filepath.Join(dir, "agent-socket", "delta.sock"),
		Idle:     time.Hour,
	})
}

type expectFunc func(desc string, want int, method, path, tok string, body []byte) ([]byte, error)

// agentsSelfTest runs a backup session through its life (create, a reply,
// a second message, delete) and checks what its token can reach.
func agentsSelfTest(expect expectFunc, token, dir string, agents *agentService, agentHandler http.Handler, publicURL string) error {
	defer testModel.Close()
	msg := func(text string) []byte {
		b, _ := json.Marshal(agentMessage{Title: "Fix the list app", UUID: "u-" + text, Text: text})
		return b
	}
	if _, err := expect("backup sessions require the token", 401, "GET", "/agent-sessions", "", nil); err != nil {
		return err
	}
	if _, err := expect("empty backup session message is refused", 400, "POST", "/agent-sessions", token, []byte(`{"text":" "}`)); err != nil {
		return err
	}
	if err := checkZenSessionID(); err != nil {
		return err
	}
	data, err := expect("create a backup session", 201, "POST", "/agent-sessions", token, msg("ping"))
	if err != nil {
		return err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &created); err != nil || !strings.HasPrefix(created.ID, agentIDPrefix) {
		return fmt.Errorf("create a backup session: got %s", data)
	}
	id := created.ID

	frames, err := readAgentStream(publicURL, token, id, 0, 1)
	if err != nil {
		return fmt.Errorf("backup session's first turn: %w", err)
	}
	if err := checkTurn(frames, "u-ping"); err != nil {
		return fmt.Errorf("backup session's first turn: %w", err)
	}
	if err := testModel.check(); err != nil {
		return fmt.Errorf("backup session's first turn: %w", err)
	}
	if out := testModel.outputs["call_w"]; !strings.Contains(out, "Successfully wrote 20 bytes to /root/a.txt") {
		return fmt.Errorf("backup session's write tool returned %q", out)
	}
	if got := readFileString(filepath.Join(dir, "agent-bin", "exec-stdin")); !strings.Contains(got, "secret file contents") {
		return fmt.Errorf("backup session's write tool didn't pass the file to the container: %q", got)
	}
	if bytes.Contains(frames, []byte("secret file contents")) || bytes.Contains(frames, []byte("delta-setup")) || bytes.Contains(frames, []byte("\"thinking\"")) {
		return fmt.Errorf("backup session's stream shows file contents, thinking or the setup block: %s", frames)
	}

	data, err = expect("list backup sessions", 200, "GET", "/agent-sessions", token, nil)
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte(id)) || !bytes.Contains(data, []byte(`"status":"idle"`)) || !bytes.Contains(data, []byte("Fix the list app")) {
		return fmt.Errorf("list backup sessions: got %s", data)
	}

	// A second message carries on the same conversation, and the stream
	// resumes after the first turn.
	last := lastSeq(frames)
	if _, err := expect("second message", 200, "POST", "/agent-sessions/"+id+"/messages", token, msg("again")); err != nil {
		return err
	}
	frames, err = readAgentStream(publicURL, token, id, last, 1)
	if err != nil {
		return fmt.Errorf("backup session's second turn: %w", err)
	}
	if err := checkTurn(frames, "u-again"); err != nil {
		return fmt.Errorf("backup session's second turn: %w", err)
	}
	if err := checkTranscript(filepath.Join(dir, "agents", id, "transcript.jsonl")); err != nil {
		return err
	}

	// A turn an update cut short in the middle of a tool call carries on
	// when the command center starts again, as start does.
	agents.mu.Lock()
	s := agents.sessions[id]
	agents.mu.Unlock()
	lost := `{"type":"function_call","call_id":"call_lost","name":"bash","arguments":"{\"command\":\"delta update-cc\"}"}` + "\n"
	f, err := os.OpenFile(filepath.Join(s.dir, "transcript.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	f.WriteString(lost)
	f.Close()
	last = lastSeq(frames)
	s.mu.Lock()
	s.meta.Working = true
	s.startTurn()
	s.mu.Unlock()
	frames, err = readAgentStream(publicURL, token, id, last, 1)
	if err != nil {
		return fmt.Errorf("backup session resumed after a restart: %w", err)
	}
	if !bytes.Contains(frames, []byte(`"text":"done"`)) || !bytes.Contains(frames, []byte(`"subtype":"success"`)) {
		return fmt.Errorf("backup session resumed after a restart: %s", frames)
	}
	if out := testModel.outputs["call_lost"]; out != lostOutput {
		return fmt.Errorf("backup session resumed after a restart: the lost call's result was %q", out)
	}
	if err := testModel.check(); err != nil {
		return err
	}
	if n := strings.Count(readFileString(filepath.Join(dir, "agent-bin", "runtime.log")), "run -d"); n != 1 {
		return fmt.Errorf("backup session made %d containers, want 1", n)
	}
	if err := checkContainerSetup(dir, agents); err != nil {
		return err
	}

	// What a session's token may reach, on the agent socket only.
	const agentToken = "delta_agent_selftest"
	agents.mu.Lock()
	agents.tokens[sha256.Sum256([]byte(agentToken))] = agents.sessions[id]
	agents.mu.Unlock()
	as := httptest.NewServer(agentHandler)
	defer as.Close()
	agentCall := func(desc string, want int, method, path, tok string, body []byte) error {
		req, _ := http.NewRequest(method, as.URL+path, bytes.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := as.Client().Do(req)
		if err != nil {
			return fmt.Errorf("%s: %w", desc, err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			return fmt.Errorf("%s: got HTTP %d, want %d (%s)", desc, resp.StatusCode, want, strings.TrimSpace(string(data)))
		}
		if desc == "session token's scope" && !bytes.Contains(data, []byte(`"scope":"agent"`)) {
			return fmt.Errorf("%s: got %s", desc, data)
		}
		return nil
	}
	checks := []struct {
		desc         string
		want         int
		method, path string
		tok          string
		body         string
	}{
		{"session token's scope", 200, "GET", "/auth", agentToken, ""},
		{"session token lists files", 200, "GET", "/files", agentToken, ""},
		{"session token can't read Gmail", 403, "GET", "/gmail-login", agentToken, ""},
		{"session token can't use Canvas", 403, "GET", "/canvas/api/v1/users/self", agentToken, ""},
		{"session token can't replace the GitHub token", 403, "PUT", "/personal-access-token", agentToken, "ghp_x"},
		{"session token can't change the maps key", 403, "PUT", "/maps-key", agentToken, "x"},
		{"session token can't run backup sessions", 403, "GET", "/agent-sessions", agentToken, ""},
		{"session token can't list the Claude accounts", 403, "GET", "/cloud-accounts", agentToken, ""},
		{"session token can't take a Claude account's token", 403, "POST", "/cloud-accounts/11111111-aaaa/token", agentToken, ""},
		{"session token can't store a conversation", 403, "POST", "/conversations", agentToken, "x"},
		{"session token reads carried conversations", 200, "GET", "/conversations", agentToken, ""},
		{"GitHub relay reaches only GitHub", 404, "GET", "/github/example.com/x", agentToken, ""},
		{"the delta token isn't taken on the agent socket", 401, "GET", "/auth", token, ""},
		{"a made-up session token is refused", 401, "GET", "/auth", "delta_agent_nope", ""},
	}
	for _, c := range checks {
		if err := agentCall(c.desc, c.want, c.method, c.path, c.tok, []byte(c.body)); err != nil {
			return err
		}
	}
	if _, err := expect("a session token isn't taken on the public listener", 401, "GET", "/auth", agentToken, nil); err != nil {
		return err
	}

	if _, err := expect("delete the backup session", 200, "DELETE", "/agent-sessions/"+id, token, nil); err != nil {
		return err
	}
	if err := agentCall("a deleted session's token is refused", 401, "GET", "/auth", agentToken, nil); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", id)); err == nil {
		return fmt.Errorf("deleted backup session's files are still there")
	}
	if _, err := os.Stat(filepath.Join(dir, "agent-bin", "c-"+containerName(id))); err == nil {
		return fmt.Errorf("deleted backup session's container is still there")
	}
	_, err = expect("deleted backup session", 404, "GET", "/agent-sessions/"+id+"/events", token, nil)
	return err
}

// readAgentStream reads a session's stream after seq until it has seen
// `results` results.
func readAgentStream(base, token, id string, after int64, results int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/agent-sessions/%s/events?from_sequence_num=%d", base, id, after), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		out.WriteString(line + "\n")
		if strings.Contains(line, `"type":"result"`) {
			if results--; results == 0 {
				return out.Bytes(), nil
			}
		}
	}
	return out.Bytes(), fmt.Errorf("stream ended before the turn did (%v): %s", sc.Err(), out.String())
}

// checkTurn checks that a turn's frames are the user's message, the reply
// with its tool call cut down to the file's path, the closing reply, and a
// success.
func checkTurn(frames []byte, uuid string) error {
	var types []string
	replies := 0
	for _, line := range strings.Split(strings.TrimSpace(string(frames)), "\n") {
		var f struct {
			Seq     int64           `json:"sequence_num"`
			Source  string          `json:"source"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			return err
		}
		var p struct {
			Type    string `json:"type"`
			UUID    string `json:"uuid"`
			Subtype string `json:"subtype"`
		}
		json.Unmarshal(f.Payload, &p)
		switch p.Type {
		case "user":
			if f.Source != "client" || p.UUID != uuid {
				return fmt.Errorf("user frame %s", line)
			}
		case "assistant":
			want := `[{"text":"pong","type":"text"},{"input":{"file_path":"/root/a.txt"},"name":"write","type":"tool_use"}]`
			if replies > 0 {
				want = `[{"text":"done","type":"text"}]`
			}
			replies++
			var m struct {
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			json.Unmarshal(f.Payload, &m)
			if string(m.Message.Content) != want {
				return fmt.Errorf("assistant frame: got %s, want %s", m.Message.Content, want)
			}
		case "result":
			if p.Subtype != "success" {
				return fmt.Errorf("result frame %s", line)
			}
		}
		if p.Type != "env_manager_log" {
			types = append(types, p.Type)
		}
	}
	if got := strings.Join(types, ","); got != "user,assistant,assistant,result" {
		return fmt.Errorf("frames %s: %s", got, frames)
	}
	return nil
}

func lastSeq(frames []byte) int64 {
	var seq int64
	for _, line := range strings.Split(strings.TrimSpace(string(frames)), "\n") {
		var f struct {
			Seq int64 `json:"sequence_num"`
		}
		if json.Unmarshal([]byte(line), &f) == nil && f.Seq > seq {
			seq = f.Seq
		}
	}
	return seq
}

func readFileString(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

// agentSocketSelfTest checks that the agent socket is reachable over a unix
// socket at all, the way containers reach it.
func agentSocketSelfTest(dir string, handler http.Handler) error {
	path := filepath.Join(dir, "agent-socket-test", "delta.sock")
	l, err := listenAgentSocket(path)
	if err != nil {
		return fmt.Errorf("agent socket: %w", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(l)
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	resp, err := client.Get("http://delta/auth")
	if err != nil {
		return fmt.Errorf("agent socket: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("agent socket without a token: got HTTP %d, want 401", resp.StatusCode)
	}
	return nil
}

func (m *fakeModel) check() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.problems) > 0 {
		return errors.New(strings.Join(m.problems, "; "))
	}
	return nil
}

// checkTranscript checks that the conversation was kept as the model's
// input items: two turns of the user's message, the reply with its
// reasoning, the tool's result and the closing reply.
func checkTranscript(path string) error {
	var types []string
	for _, line := range strings.Split(strings.TrimSpace(readFileString(path)), "\n") {
		h := head(json.RawMessage(line))
		types = append(types, h.Type+":"+h.Role)
	}
	turn := "message:user,reasoning:,message:assistant,function_call:,function_call_output:,message:assistant"
	if got := strings.Join(types, ","); got != turn+","+turn {
		return fmt.Errorf("backup session's transcript: %s", got)
	}
	if !strings.Contains(readFileString(path), "delta-setup") {
		return fmt.Errorf("backup session's first message lacks the setup block")
	}
	return nil
}

// checkContainerSetup checks that the container was made signed in, with a
// token of its own that the command center takes, and ran the setup script
// as root before the agent's first command.
func checkContainerSetup(dir string, agents *agentService) error {
	log := readFileString(filepath.Join(dir, "agent-bin", "runtime.log"))
	run := ""
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "run -d") {
			run = line
		}
	}
	if !strings.Contains(run, "DELTA_SERVER="+agentSocketURL) {
		return fmt.Errorf("backup session's container lacks DELTA_SERVER: %s", run)
	}
	i := strings.Index(run, "DELTA_TOKEN=")
	if i < 0 {
		return fmt.Errorf("backup session's container lacks DELTA_TOKEN: %s", run)
	}
	token := strings.Fields(run[i+len("DELTA_TOKEN="):])[0]
	if !agents.validToken(token) {
		return fmt.Errorf("backup session's container token isn't taken")
	}
	execs := []string{}
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "exec ") {
			execs = append(execs, line)
		}
	}
	if len(execs) == 0 || !strings.HasSuffix(execs[0], " bash -s") {
		return fmt.Errorf("backup session's first command in its container wasn't the setup script: %v", execs)
	}
	if !strings.HasPrefix(readFileString(filepath.Join(dir, "agent-bin", "exec-stdin")), defaultAgentSetup) {
		return fmt.Errorf("backup session's container wasn't given the setup script")
	}
	return nil
}

// checkZenSessionID checks that session ids come out as the OpenCode CLI
// makes them (agents_model.go).
func checkZenSessionID() error {
	id, err := zenSessionID()
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(id) {
		return fmt.Errorf("zenSessionID: malformed %q", id)
	}
	// Descending: the hex is the bit-inverted (ms * 0x1000 + 1), low 48 bits.
	v, _ := strconv.ParseUint(id[4:16], 16, 64)
	ms := int64(^v&(1<<48-1)) >> 12
	if want := time.Now().UnixMilli() & (1<<36 - 1); ms < want-60_000 || ms > want+1000 {
		return fmt.Errorf("zenSessionID: %q holds time %d, want about %d", id, ms, want)
	}
	return nil
}
