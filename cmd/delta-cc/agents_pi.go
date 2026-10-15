package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// pi, the agent behind backup sessions. See agents.go for the overview.

// piVersion is the pi release the command center installs (with npm, into
// data/agents/pi-<version>/) unless -agent-pi names one.
const piVersion = "1.2.0"

// agentSystemPrompt replaces pi's own system prompt, which names pi and
// points at its documentation on this machine.
const agentSystemPrompt = `You are Delta, a coding assistant the user reaches from the Delta app on their flip phone. You help them by reading files, running commands, editing code and writing new files.

Tools:
- read: read a file's contents
- bash: run a bash command
- edit: change a file by replacing exact text
- write: create or overwrite a file

Guidelines:
- Use bash to list, search and find files (ls, grep, find).
- Read a file before editing it.
- Show file paths clearly when working with files.
- The user reads your replies on a small flip-phone screen: keep them short and plain.

You run in your own Linux container (Debian, as root, home /root), made for this session on the user's command center. It keeps its files until the user deletes the session, but it is stopped while the session is idle, so don't leave anything running in the background that the user relies on. Install whatever you need with apt-get.`

// deliver hands a message to pi, starting the container and pi first if
// they aren't running. Called on its own goroutine for every message.
func (s *agentSession) deliver(text string, images []string) {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	deleted := s.deleted
	s.mu.Unlock()
	if deleted {
		return
	}
	if err := s.ensureRunning(); err != nil {
		log.Printf("backup session %s: %v", s.meta.ID, err)
		s.fail(err.Error())
		return
	}
	imgs := []map[string]string{}
	for _, img := range images {
		imgs = append(imgs, map[string]string{"type": "image", "data": img, "mimeType": "image/jpeg"})
	}
	// Steering delivers a message sent mid-turn as soon as the current
	// tool calls finish, as a cloud session does; an idle pi just starts.
	cmd := map[string]any{"type": "prompt", "message": text, "images": imgs, "streamingBehavior": "steer"}
	if err := s.command(cmd); err != nil {
		s.fail("Delta couldn't be reached: " + err.Error())
	}
}

func (s *agentSession) abort() {
	if err := s.command(map[string]string{"type": "abort"}); err != nil {
		// Nothing is running to stop, so the turn is over anyway.
		s.mu.Lock()
		working := s.meta.Working
		s.mu.Unlock()
		if working {
			s.setWorking(false)
			s.record("worker", map[string]string{"type": "result", "subtype": "error_during_execution"})
		}
	}
}

// ensureRunning gets the container and pi going. Callers hold s.op.
func (s *agentSession) ensureRunning() error {
	s.mu.Lock()
	s.parked = false
	s.mu.Unlock()
	if err := s.svc.ensureContainer(s); err != nil {
		return err
	}
	s.mu.Lock()
	pid := s.meta.PID
	s.mu.Unlock()
	if s.piAlive(pid) {
		return nil
	}
	pi, err := s.svc.ensurePi(s)
	if err != nil {
		return err
	}
	pid, err = s.launch(pi)
	if err != nil {
		return fmt.Errorf("Delta couldn't start: %w", err)
	}
	s.follow(pid)
	return nil
}

// ensurePi returns the pi executable, installing the pinned version on first use.
func (a *agentService) ensurePi(s *agentSession) (string, error) {
	if a.cfg.Pi != "" {
		return a.cfg.Pi, nil
	}
	a.installMu.Lock()
	defer a.installMu.Unlock()
	dir := filepath.Join(a.dir, "pi-"+piVersion)
	pi := filepath.Join(dir, "node_modules", ".bin", "pi")
	if _, err := os.Stat(pi); err == nil {
		return pi, nil
	}
	s.status("Setting up Delta on the command center (first time only)…")
	tmp := dir + ".partial"
	os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "install", "--prefix", tmp, "--no-audit", "--no-fund", "--omit=dev",
		"@earendil-works/pi-coding-agent@"+piVersion)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("installing the agent failed (is Node.js 22.19+ with npm installed on the command center?): %v: %s",
			err, lastLine(string(out)))
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	return pi, nil
}

// launch starts pi in RPC mode, detached: its stdin is the session's FIFO
// (opened read-write, so it never sees end of file) and its stdout is
// appended to pi.out. A shell starts it in the background and exits, so pi
// belongs to no command center process and survives an update.
func (s *agentSession) launch(pi string) (int, error) {
	in := filepath.Join(s.dir, "pi.in")
	if _, err := os.Stat(in); errors.Is(err, os.ErrNotExist) {
		if err := syscall.Mkfifo(in, 0o600); err != nil {
			return 0, err
		}
	}
	cwd := filepath.Join(s.dir, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		return 0, err
	}
	key, err := s.svc.modelKey()
	if err != nil {
		return 0, err
	}
	// A fresh pi starts a fresh pi.out.
	if err := os.WriteFile(filepath.Join(s.dir, "pi.out"), nil, 0o600); err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.meta.Offset = 0
	piSession := s.meta.PiSession
	s.mu.Unlock()

	cfg := s.svc.cfg
	args := []string{
		"--mode", "rpc",
		"--session-dir", filepath.Join(s.dir, "pi"),
		"--session-id", piSession,
		"--model", cfg.Model,
		"--thinking", "medium",
		"--no-extensions", "-e", s.svc.extensionPath(),
		"--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--no-mcp", "--no-approve",
		"--tools", "read,bash,edit,write",
		"--system-prompt", agentSystemPrompt,
	}
	script := `"$0" "$@" <>"$DELTA_PI_IN" >>"$DELTA_PI_OUT" 2>>"$DELTA_PI_ERR" & echo $!`
	cmd := exec.Command("sh", append([]string{"-c", script, pi}, args...)...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LANG=C.UTF-8",
		"PI_CODING_AGENT_DIR=" + filepath.Join(s.svc.dir, "pi-agent"),
		"PI_OFFLINE=1",
		"OPENCODE_API_KEY=" + key,
		"DELTA_AGENT_RUNTIME=" + cfg.Runtime,
		"DELTA_AGENT_CONTAINER=" + containerName(s.meta.ID),
		"DELTA_PI_IN=" + in,
		"DELTA_PI_OUT=" + filepath.Join(s.dir, "pi.out"),
		"DELTA_PI_ERR=" + filepath.Join(s.dir, "pi.err"),
	}
	if xdg := runtimeDir(); xdg != "" {
		cmd.Env = append(cmd.Env, "XDG_RUNTIME_DIR="+xdg)
	}
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("unexpected pid %q", out)
	}
	s.mu.Lock()
	s.meta.PID, s.meta.PIDStart = pid, procStart(pid)
	s.saveMeta()
	s.mu.Unlock()
	return pid, nil
}

// piAlive reports whether pid is still the session's pi: the same process,
// by its start time, and not yet exited.
func (s *agentSession) piAlive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	s.mu.Lock()
	want := s.meta.PIDStart
	s.mu.Unlock()
	return want != "" && procStart(pid) == want
}

// procStart returns when process pid started (field 22 of /proc/<pid>/stat,
// which an exec doesn't change), or "" if it is gone or a zombie.
func procStart(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// The command name, in parentheses, may itself hold spaces.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return ""
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 20 || fields[0] == "Z" {
		return ""
	}
	return fields[19]
}

func (s *agentSession) stopPi(pid int) {
	if !s.piAlive(pid) {
		return
	}
	syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 50 && s.piAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if s.piAlive(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// command writes one RPC command to pi's FIFO.
func (s *agentSession) command(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	pid := s.meta.PID
	s.mu.Unlock()
	// Non-blocking, so a FIFO nobody reads fails instead of hanging. A pi
	// that has only just started may not have opened it yet.
	var f *os.File
	for try := 0; ; try++ {
		if !s.piAlive(pid) {
			return errors.New("it isn't running")
		}
		f, err = os.OpenFile(filepath.Join(s.dir, "pi.in"), os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.ENXIO) || try >= 50 {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer f.Close()
	f.SetWriteDeadline(time.Now().Add(time.Minute))
	_, err = f.Write(append(line, '\n'))
	return err
}

// follow tails pi.out for the session's pi until it exits, or until a
// newer pi has taken over the session.
func (s *agentSession) follow(pid int) {
	s.mu.Lock()
	if s.tailPID == pid {
		s.mu.Unlock()
		return
	}
	s.tailPID = pid
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			if s.tailPID == pid {
				s.tailPID = 0
			}
			s.mu.Unlock()
		}()
		path := filepath.Join(s.dir, "pi.out")
		var pending []byte
		for {
			s.mu.Lock()
			offset, current, deleted := s.meta.Offset, s.meta.PID, s.deleted
			s.mu.Unlock()
			if deleted || current != pid {
				return
			}
			alive := s.piAlive(pid)
			chunk, err := readFrom(path, offset+int64(len(pending)))
			if err == nil && len(chunk) > 0 {
				pending = append(pending, chunk...)
				for {
					i := bytes.IndexByte(pending, '\n')
					if i < 0 {
						break
					}
					s.handle(pending[:i])
					pending = pending[i+1:]
					s.mu.Lock()
					s.meta.Offset += int64(i + 1)
					s.mu.Unlock()
				}
				s.mu.Lock()
				s.saveMeta()
				s.mu.Unlock()
				continue
			}
			if !alive {
				s.exited(pid)
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()
}

// exited cleans up after pi has gone, ending any turn it left unfinished.
func (s *agentSession) exited(pid int) {
	// A message starting a new pi takes the turn over.
	if !s.op.TryLock() {
		s.mu.Lock()
		if s.meta.PID == pid {
			s.meta.PID = 0
		}
		s.mu.Unlock()
		return
	}
	defer s.op.Unlock()
	s.mu.Lock()
	if s.meta.PID != pid {
		s.mu.Unlock()
		return
	}
	working, deleted := s.meta.Working, s.deleted
	s.meta.PID = 0
	s.saveMeta()
	s.mu.Unlock()
	if !working || deleted {
		return
	}
	// pi's own words may name it or the provider, so they go to the log only.
	log.Printf("backup session %s: pi exited mid-turn: %s", s.meta.ID, lastLine(readTail(filepath.Join(s.dir, "pi.err"), 4096)))
	s.fail("Delta stopped unexpectedly. Send a message to carry on.")
}

func readFrom(path string, offset int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, 4<<20))
}

func readTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > n {
		f.Seek(-n, io.SeekEnd)
	}
	data, _ := io.ReadAll(f)
	return string(data)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	if len(line) > 300 {
		line = line[:300] + "…"
	}
	return line
}

// ---------------------------------------------------------------------------
// From pi's events to the phone's frames

// piEvent holds the fields of pi's RPC events that the phone needs.
type piEvent struct {
	Type        string    `json:"type"`
	Message     piMessage `json:"message"`
	Command     string    `json:"command"`
	Success     *bool     `json:"success"`
	Error       string    `json:"error"`
	Aborted     bool      `json:"aborted"`
	Attempt     int       `json:"attempt"`
	MaxAttempts int       `json:"maxAttempts"`
}

type piMessage struct {
	Role         string          `json:"role"`
	RawContent   json.RawMessage `json:"content"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
}

type piBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// handle turns one line of pi's output into frames for the phone.
func (s *agentSession) handle(line []byte) {
	var e piEvent
	if json.Unmarshal(line, &e) != nil {
		return
	}
	switch e.Type {
	case "message_end":
		if e.Message.Role != "assistant" {
			return
		}
		var blocks []piBlock
		json.Unmarshal(e.Message.RawContent, &blocks)
		content := []any{}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					content = append(content, map[string]string{"type": "text", "text": b.Text})
				}
			case "toolCall":
				content = append(content, map[string]any{"type": "tool_use", "name": b.Name, "input": toolSummary(b.Arguments)})
			}
		}
		if len(content) > 0 {
			s.record("worker", map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": content}})
		}
		if e.Message.StopReason == "error" && e.Message.ErrorMessage != "" {
			s.mu.Lock()
			s.lastError = modelError(e.Message.ErrorMessage)
			s.mu.Unlock()
		}
	case "auto_retry_start":
		s.status(fmt.Sprintf("Retrying after an error (try %d of %d)…", e.Attempt, e.MaxAttempts))
	case "compaction_start":
		s.status("Summarizing the conversation so far…")
	case "agent_settled":
		s.mu.Lock()
		failed := s.lastError
		s.mu.Unlock()
		s.setWorking(false)
		switch {
		case e.Aborted:
			s.record("worker", map[string]string{"type": "result", "subtype": "error_during_execution"})
		case failed != "":
			s.record("worker", map[string]any{"type": "result", "subtype": "error", "is_error": true, "result": failed})
		default:
			s.record("worker", map[string]string{"type": "result", "subtype": "success"})
		}
	case "response":
		if e.Command == "prompt" && e.Success != nil && !*e.Success {
			log.Printf("backup session %s: pi refused a prompt: %s", s.meta.ID, e.Error)
			s.fail("Delta couldn't take the message. Try sending it again.")
		}
	}
}

var httpStatus = regexp.MustCompile(`\((\d{3})\)`)

// modelError says what went wrong with a model request without naming the
// provider: the phone never shows it.
func modelError(msg string) string {
	code := 0
	if m := httpStatus.FindStringSubmatch(msg); m != nil {
		code, _ = strconv.Atoi(m[1])
	}
	lower := strings.ToLower(msg)
	switch {
	case code == 401 || code == 403:
		return "The API key for backup sessions was rejected. Set a new one from a regular (●) session with `delta backup-key set -`."
	case code == 402 || strings.Contains(lower, "insufficient") || strings.Contains(lower, "credit") || strings.Contains(lower, "balance"):
		return "The account for backup sessions is out of credit."
	case code == 429:
		return "Too many requests right now. Try again in a minute."
	case code >= 500:
		return fmt.Sprintf("The model isn't answering right now (HTTP %d). Try again in a minute.", code)
	case code != 0:
		return fmt.Sprintf("The model couldn't answer (HTTP %d).", code)
	case strings.Contains(lower, "context") && (strings.Contains(lower, "length") || strings.Contains(lower, "too long") || strings.Contains(lower, "overflow")):
		return "The conversation has grown too long. Start a new session."
	}
	return "The model couldn't answer. Try again in a minute."
}

// toolSummary keeps the one argument of a tool call that says what it does,
// under the name the phone looks for, rather than whole file contents.
func toolSummary(args map[string]any) map[string]string {
	out := map[string]string{}
	clip := func(v string) string {
		if len(v) > 300 {
			return v[:300] + "…"
		}
		return v
	}
	if v, ok := args["command"].(string); ok {
		out["command"] = clip(v)
	} else if v, ok := args["path"].(string); ok {
		out["file_path"] = clip(v)
	}
	return out
}

// ---------------------------------------------------------------------------
// Idle sessions

// sweep stops the pi and the container of every session that has been idle
// for cfg.Idle.
func (a *agentService) sweep() {
	for range time.Tick(time.Minute) {
		a.mu.Lock()
		sessions := make([]*agentSession, 0, len(a.sessions))
		for _, s := range a.sessions {
			sessions = append(sessions, s)
		}
		a.mu.Unlock()
		for _, s := range sessions {
			s.mu.Lock()
			idle := !s.meta.Working && !s.deleted && !s.parked && time.Since(s.lastActive) > a.cfg.Idle
			s.mu.Unlock()
			if idle {
				a.park(s)
			}
		}
	}
}

// park stops an idle session's pi and container, unless a message arrived meanwhile.
func (a *agentService) park(s *agentSession) {
	if !s.op.TryLock() {
		return
	}
	defer s.op.Unlock()
	s.mu.Lock()
	pid := s.meta.PID
	s.mu.Unlock()
	if pid != 0 {
		s.stopPi(pid)
	}
	if err := a.stopContainer(s.meta.ID); err != nil {
		log.Printf("backup session %s: stopping its container: %v", s.meta.ID, err)
	}
	s.mu.Lock()
	s.parked = true
	s.mu.Unlock()
}
