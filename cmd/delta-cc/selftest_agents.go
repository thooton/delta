package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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
	"time"
)

// Backup sessions are tested with stand-ins for pi and the container
// runtime, so the self-test needs neither Node nor containers.

// fakePi answers every prompt with one reply and one tool call, as pi's RPC
// mode would.
const fakePi = `#!/bin/sh
case "$DELTA_ZEN_SESSION" in ses_??????????????????????????) ;; *) echo "no OpenCode session id" >&2; exit 1 ;; esac
[ -z "$OPENCODE_API_KEY" ] || { echo "an API key reached pi" >&2; exit 1; }
while IFS= read -r line; do
  case "$line" in
  *'"type":"prompt"'*)
    echo '{"type":"response","command":"prompt","success":true}'
    echo '{"type":"agent_start"}'
    echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"pong"},{"type":"toolCall","id":"t1","name":"write","arguments":{"path":"/root/a.txt","content":"secret file contents"}}],"stopReason":"toolUse"}}'
    echo '{"type":"message_end","message":{"role":"toolResult","content":[{"type":"text","text":"ok"}]}}'
    echo '{"type":"agent_settled","aborted":false}'
    ;;
  esac
done
`

// fakeRuntime keeps each container's state in a file next to it, and logs
// every command.
const fakeRuntime = `#!/bin/sh
dir=$(dirname "$0")
echo "$*" >> "$dir/runtime.log"
case "$1" in
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
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte(fakePi), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(bin, "runtime"), []byte(fakeRuntime), 0o700); err != nil {
		return nil, err
	}
	return newAgentService(dir, agentConfig{
		Runtime: filepath.Join(bin, "runtime"),
		Image:   "example/image",
		Pi:      filepath.Join(bin, "pi"),
		Socket:  filepath.Join(dir, "agent-socket", "delta.sock"),
		Idle:    time.Hour,
	})
}

type expectFunc func(desc string, want int, method, path, tok string, body []byte) ([]byte, error)

// agentsSelfTest runs a backup session through its life (create, a reply,
// a second message, delete) and checks what its token can reach.
func agentsSelfTest(expect expectFunc, token, dir string, agents *agentService, agentHandler http.Handler, publicURL string) error {
	defer func() {
		// Never leave a stand-in pi running.
		agents.mu.Lock()
		defer agents.mu.Unlock()
		for _, s := range agents.sessions {
			if s.meta.PID != 0 {
				s.stopPi(s.meta.PID)
			}
		}
	}()
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
	if err := checkModelsJSON(agents); err != nil {
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

	// A second message goes to the same pi, and the stream resumes after
	// the first turn.
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
	if n := strings.Count(readFileString(filepath.Join(dir, "agent-bin", "runtime.log")), "run -d"); n != 1 {
		return fmt.Errorf("backup session made %d containers, want 1", n)
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
// with its tool call cut down to the file's path, and a success.
func checkTurn(frames []byte, uuid string) error {
	var types []string
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
	if got := strings.Join(types, ","); got != "user,assistant,result" {
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

// checkModelsJSON checks that pi is given the free model and nothing else,
// with no API key but "public", and that session ids come out as the
// OpenCode CLI makes them (agents_model.go).
func checkModelsJSON(agents *agentService) error {
	data, err := os.ReadFile(filepath.Join(agents.piAgentDir(), "models.json"))
	if err != nil {
		return fmt.Errorf("pi's models.json: %w", err)
	}
	var cfg struct {
		Providers map[string]struct {
			BaseURL string            `json:"baseUrl"`
			APIKey  string            `json:"apiKey"`
			Headers map[string]string `json:"headers"`
			Models  []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("pi's models.json: %w", err)
	}
	p, ok := cfg.Providers[agentProvider]
	if len(cfg.Providers) != 1 || !ok || len(p.Models) != 1 || p.Models[0].ID != agentModelID ||
		!strings.HasSuffix(agentModelID, "-free") {
		return fmt.Errorf("pi's models.json should offer only %s: %s", agentModel, data)
	}
	if p.APIKey != "public" || p.BaseURL != zenBaseURL {
		return fmt.Errorf("pi's models.json: want apiKey public at %s, got %q at %q", zenBaseURL, p.APIKey, p.BaseURL)
	}
	for _, h := range []string{"x-opencode-session", "x-opencode-session-id"} {
		if p.Headers[h] != "${DELTA_ZEN_SESSION}" {
			return fmt.Errorf("pi's models.json: header %s is %q", h, p.Headers[h])
		}
	}
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
