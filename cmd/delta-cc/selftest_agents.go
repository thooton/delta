package main

import (
	"bufio"
	"bytes"
	"context"
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

// zenSessionPattern is an OpenCode session id, as the CLI makes them.
var zenSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

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
	type chatMsg struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ReasoningContent string          `json:"reasoning_content"`
		ToolCallID       string          `json:"tool_call_id"`
		ToolCalls        []struct {
			ID       string `json:"id"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	var req struct {
		Model    string    `json:"model"`
		Effort   string    `json:"reasoning_effort"`
		Stream   bool      `json:"stream"`
		Messages []chatMsg `json:"messages"`
		Tools    []struct {
			Function struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"function"`
		} `json:"tools"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil || r.URL.Path != "/chat/completions" {
		m.problem("model request to %s: %v", r.URL.Path, err)
		http.Error(w, "bad request", 400)
		return
	}
	ses := r.Header.Get("x-opencode-session")
	switch {
	case !strings.Contains(r.Header.Get("User-Agent"), "opencode/1."):
		m.problem("model request has User-Agent %q", r.Header.Get("User-Agent"))
	case !zenSessionPattern.MatchString(ses):
		m.problem("model request has OpenCode session %q", ses)
	case req.Model != agentModelID || !strings.HasSuffix(req.Model, "-free"):
		m.problem("model request asks for %q", req.Model)
	case !req.Stream:
		m.problem("model request doesn't stream")
	case req.Effort != "high":
		m.problem("model request has reasoning effort %q", req.Effort)
	}
	readOK := false
	for _, t := range req.Tools {
		readOK = readOK || t.Function.Name == "read"
		if t.Function.Description == "" {
			m.problem("model request's tool %s has no description", t.Function.Name)
		}
	}
	if !readOK {
		m.problem("model request has no read tool")
	}
	if len(req.Messages) < 2 || req.Messages[0].Role != "system" {
		m.problem("model request doesn't start with the system prompt")
		http.Error(w, "bad request", 400)
		return
	}
	last := req.Messages[len(req.Messages)-1]
	var chunks []string
	switch last.Role {
	case "user":
		chunks = []string{
			`{"role":"assistant","content":""}`,
			`{"reasoning_content":"thinking "}`,
			`{"reasoning_content":"it over"}`,
			`{"content":"pong"}`,
			`{"tool_calls":[{"index":0,"id":"call_w","type":"function","function":{"name":"write","arguments":""}}]}`,
			`{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"/root/a.txt\","}}]}`,
			`{"tool_calls":[{"index":0,"function":{"arguments":"\"content\":\"secret file contents\"}"}}]}`,
		}
	case "tool":
		var out string
		json.Unmarshal(last.Content, &out)
		m.mu.Lock()
		m.outputs[last.ToolCallID] = out
		m.mu.Unlock()
		if last.ToolCallID == "call_w" {
			prev := req.Messages[len(req.Messages)-2]
			if prev.Role != "assistant" || prev.ReasoningContent != "thinking it over" || len(prev.ToolCalls) != 1 || prev.ToolCalls[0].ID != "call_w" {
				m.problem("the reasoning and the tool call didn't come back with the tool's result: %+v", prev)
			}
		}
		chunks = []string{`{"content":"done"}`}
	default:
		m.problem("model request ends with a %s message", last.Role)
		http.Error(w, "bad request", 400)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for i, c := range chunks {
		finish := "null"
		if i == len(chunks)-1 {
			finish = `"stop"`
			if last.Role == "user" {
				finish = `"tool_calls"`
			}
		}
		fmt.Fprintf(w, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"finish_reason\":%s,\"delta\":%s}]}\n\n", finish, c)
	}
	fmt.Fprint(w, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":10}}\n\ndata: [DONE]\n\n")
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

// agentsSelfTest runs a backup session through its life (create, a reply,
// a second message, delete) and checks what its token can reach.
func agentsSelfTest(expect expectFunc, token, dir string, agents *agentService, agentHandler http.Handler, publicURL string) error {
	defer testModel.Close()
	msg := func(text string) []byte {
		b, _ := json.Marshal(agentMessage{Title: "Fix the list app", UUID: "u-" + text, Text: text})
		return b
	}
	if err := runSteps(expect,
		step{"backup sessions require the token", 401, "GET", "/agent-sessions", "", nil},
		step{"empty backup session message is refused", 400, "POST", "/agent-sessions", token, []byte(`{"text":" "}`)},
	); err != nil {
		return err
	}
	if err := checkZenSessionID(); err != nil {
		return err
	}
	var created struct {
		ID string `json:"id"`
	}
	data, err := expectJSON(expect, &created, "create a backup session", 201, "POST", "/agent-sessions", token, msg("ping"))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(created.ID, agentIDPrefix) {
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
	agents.tokens[keyHash(agentToken)] = agents.sessions[id]
	agents.mu.Unlock()
	as := httptest.NewServer(agentHandler)
	defer as.Close()
	agentExpect := expectAt(as.URL)
	if data, err := agentExpect("session token's scope", 200, "GET", "/auth", agentToken, nil); err != nil {
		return err
	} else if !bytes.Contains(data, []byte(`"scope":"agent"`)) {
		return fmt.Errorf("session token's scope: got %s", data)
	}
	if err := runSteps(agentExpect,
		step{"session token lists files", 200, "GET", "/files", agentToken, nil},
		step{"session token can't read Gmail", 403, "GET", "/gmail-login", agentToken, nil},
		step{"session token can't read the two-factor accounts", 403, "GET", "/otp", agentToken, nil},
		step{"session token can't use Canvas", 403, "GET", "/canvas/api/v1/users/self", agentToken, nil},
		step{"session token can't replace the GitHub token", 403, "PUT", "/personal-access-token", agentToken, []byte("ghp_x")},
		step{"session token can't change the maps key", 403, "PUT", "/maps-key", agentToken, []byte("x")},
		step{"session token can't run backup sessions", 403, "GET", "/agent-sessions", agentToken, nil},
		step{"session token lists the Claude accounts", 200, "GET", "/cloud-accounts", agentToken, nil},
		step{"session token takes a Claude account's token", 200, "POST", "/cloud-accounts/11111111-aaaa/token", agentToken, nil},
		step{"session token can't store a conversation", 403, "POST", "/conversations", agentToken, []byte("x")},
		step{"session token reads carried conversations", 200, "GET", "/conversations", agentToken, nil},
		step{"GitHub relay reaches only GitHub", 404, "GET", "/github/example.com/x", agentToken, nil},
		step{"the delta token isn't taken on the agent socket", 401, "GET", "/auth", token, nil},
		step{"a made-up session token is refused", 401, "GET", "/auth", "delta_agent_nope", nil},
	); err != nil {
		return err
	}
	if err := runSteps(expect,
		step{"a session token isn't taken on the public listener", 401, "GET", "/auth", agentToken, nil},
		step{"delete the backup session", 200, "DELETE", "/agent-sessions/" + id, token, nil},
	); err != nil {
		return err
	}
	if _, err := agentExpect("a deleted session's token is refused", 401, "GET", "/auth", agentToken, nil); err != nil {
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
	id := zenSessionID()
	if !zenSessionPattern.MatchString(id) {
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
