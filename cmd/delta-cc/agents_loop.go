package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The agent loop behind backup sessions. See agents.go for the overview.
//
// A turn asks the model for a reply, runs the tool calls in it, and asks
// again with their results, until a reply calls no tools. Messages the user
// sends meanwhile wait in the session's inbox and join the conversation
// before the next request, so they reach the model between its steps.
//
// The conversation is kept in transcript.jsonl as the Responses API's input
// items, appended as they come, so a turn cut short by an update or a crash
// carries on from its last step when the command center starts again.

// agentQueued is a message waiting for the model: its text, and JPEGs as base64.
type agentQueued struct {
	Text   string   `json:"text"`
	Images []string `json:"images,omitempty"`
}

// lostOutput is the result of a tool call that was under way when the
// command center stopped.
const lostOutput = "The command center restarted while this was running, so its result was lost. Check whether it finished before relying on it."

// startTurn begins running the session's loop, if it isn't already.
// Callers hold s.mu.
func (s *agentSession) startTurn() {
	if s.running || s.deleted {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.running, s.cancel = true, cancel
	go s.run(ctx)
}

// run takes turns until the model has answered everything there is to
// answer. It holds s.op throughout, so the session is neither parked nor
// deleted under it.
func (s *agentSession) run(ctx context.Context) {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	s.parked = false
	s.mu.Unlock()
	if err := s.svc.ensureContainer(s); err != nil {
		log.Printf("backup session %s: %v", s.meta.ID, err)
		s.end(failure(err.Error()), true)
		return
	}
	items := s.loadTranscript()
	for {
		if ctx.Err() != nil {
			// Interrupted, or deleted. What the user sent meanwhile is kept
			// for the next turn.
			if ctx = s.interrupted(); ctx == nil {
				return
			}
			continue
		}
		s.mu.Lock()
		queued := s.inbox
		s.inbox = nil
		s.saveInbox()
		s.mu.Unlock()
		for _, q := range queued {
			items = s.appendItems(items, userItem(q))
		}
		if !needsReply(items) {
			if s.end(map[string]string{"type": "result", "subtype": "success"}, false) {
				return
			}
			continue
		}
		reply, err := s.ask(ctx, items)
		if ctx.Err() != nil {
			continue
		}
		if err != nil {
			log.Printf("backup session %s: model request failed: %v", s.meta.ID, err)
			s.end(failure(describeModelError(err)), true)
			return
		}
		items = s.appendItems(items, reply.Items...)
		calls := s.show(reply.Items)
		for _, c := range calls {
			var out any = "Interrupted by the user."
			if ctx.Err() == nil {
				out = s.runTool(ctx, c.Name, c.Arguments)
			}
			items = s.appendItems(items, toolOutput(c.CallID, out))
		}
		if len(calls) == 0 && reply.Incomplete != "" {
			log.Printf("backup session %s: reply cut short: %s", s.meta.ID, reply.Incomplete)
		}
	}
}

// end ends the turn with a result frame and reports true, unless messages
// have arrived that the loop should answer first (then it reports false).
// With force it ends the turn regardless, leaving any messages waiting for
// the next one.
func (s *agentSession) end(result any, force bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inbox) > 0 && !force && !s.deleted {
		return false
	}
	s.running, s.cancel = false, nil
	s.meta.Working = false
	s.saveMeta()
	s.recordLocked("worker", result)
	return true
}

// interrupted ends a turn the user stopped. If a message is already
// waiting, it returns a new context for the turn that answers it.
func (s *agentSession) interrupted() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted {
		s.running, s.cancel = false, nil
		return nil
	}
	s.recordLocked("worker", map[string]string{"type": "result", "subtype": "error_during_execution"})
	if len(s.inbox) == 0 {
		s.running, s.cancel = false, nil
		s.meta.Working = false
		s.saveMeta()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	return ctx
}

// abort stops the turn under way.
func (s *agentSession) abort() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		return
	}
	if s.meta.Working {
		// Nothing is running to stop, so the turn is over anyway.
		s.meta.Working = false
		s.saveMeta()
		s.recordLocked("worker", map[string]string{"type": "result", "subtype": "error_during_execution"})
	}
}

func failure(msg string) map[string]any {
	return map[string]any{"type": "result", "subtype": "error", "is_error": true, "result": msg}
}

// ---------------------------------------------------------------------------
// The conversation

// itemHead holds the fields of an input item that the loop looks at.
type itemHead struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func head(item json.RawMessage) itemHead {
	var h itemHead
	json.Unmarshal(item, &h)
	if h.Type == "" && h.Role != "" {
		h.Type = "message"
	}
	return h
}

func userItem(q agentQueued) json.RawMessage {
	content := []map[string]string{}
	for _, img := range q.Images {
		content = append(content, map[string]string{"type": "input_image", "detail": "auto", "image_url": "data:image/jpeg;base64," + img})
	}
	if q.Text != "" || len(content) == 0 {
		content = append(content, map[string]string{"type": "input_text", "text": q.Text})
	}
	data, _ := json.Marshal(map[string]any{"role": "user", "content": content})
	return data
}

func toolOutput(callID string, out any) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": callID, "output": out})
	return data
}

// needsReply reports whether the conversation waits on the model: it ends
// with something the user said or a tool's result.
func needsReply(items []json.RawMessage) bool {
	if len(items) == 0 {
		return false
	}
	h := head(items[len(items)-1])
	return h.Type == "function_call_output" || (h.Type == "message" && h.Role == "user")
}

// loadTranscript reads the conversation so far, and gives every tool call
// left without a result (because the command center stopped while it ran)
// one saying so.
func (s *agentSession) loadTranscript() []json.RawMessage {
	var items []json.RawMessage
	f, err := os.Open(filepath.Join(s.dir, "transcript.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return s.appendItems(nil, s.legacyTranscript()...)
	}
	if err == nil {
		r := bufio.NewReaderSize(f, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			// A line cut short by a crash isn't JSON, and is skipped.
			if line = bytes.TrimSpace(line); json.Valid(line) {
				items = append(items, json.RawMessage(line))
			}
			if err != nil {
				break
			}
		}
		f.Close()
	}
	answered := map[string]bool{}
	for _, item := range items {
		if h := head(item); h.Type == "function_call_output" {
			answered[h.CallID] = true
		}
	}
	for _, item := range items {
		if h := head(item); h.Type == "function_call" && !answered[h.CallID] {
			items = s.appendItems(items, toolOutput(h.CallID, lostOutput))
		}
	}
	return items
}

// appendItems adds items to the conversation and to transcript.jsonl.
func (s *agentSession) appendItems(items []json.RawMessage, add ...json.RawMessage) []json.RawMessage {
	var buf bytes.Buffer
	for _, item := range add {
		var compact bytes.Buffer
		if json.Compact(&compact, item) != nil {
			continue
		}
		items = append(items, json.RawMessage(compact.Bytes()))
		buf.Write(compact.Bytes())
		buf.WriteByte('\n')
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "transcript.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.Write(buf.Bytes())
		f.Close()
	}
	if err != nil {
		log.Printf("backup session %s: saving the conversation: %v", s.meta.ID, err)
	}
	return items
}

// show records a reply for the website: its text, and its tool calls cut
// down to what they act on. It returns the tool calls.
func (s *agentSession) show(reply []json.RawMessage) []itemHead {
	content := []any{}
	var calls []itemHead
	for _, item := range reply {
		h := head(item)
		switch h.Type {
		case "message":
			for _, c := range h.Content {
				if c.Type == "output_text" && strings.TrimSpace(c.Text) != "" {
					content = append(content, map[string]string{"type": "text", "text": c.Text})
				}
			}
		case "function_call":
			calls = append(calls, h)
			var args map[string]any
			json.Unmarshal([]byte(h.Arguments), &args)
			content = append(content, map[string]any{"type": "tool_use", "name": h.Name, "input": toolSummary(args)})
		}
	}
	if len(content) > 0 {
		s.record("worker", map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": content}})
	}
	return calls
}

// toolSummary keeps the one argument of a tool call that says what it does,
// under the name the website looks for, rather than whole file contents.
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

// saveInbox keeps the waiting messages in inbox.json, so a restart doesn't
// lose them. Callers hold s.mu.
func (s *agentSession) saveInbox() {
	path := filepath.Join(s.dir, "inbox.json")
	if len(s.inbox) == 0 {
		os.Remove(path)
		return
	}
	data, err := json.Marshal(s.inbox)
	if err == nil {
		err = writePrivateFile(path, data)
	}
	if err != nil {
		log.Printf("backup session %s: saving its waiting messages: %v", s.meta.ID, err)
	}
}

// ---------------------------------------------------------------------------
// Sessions from before

// stopLegacyPi stops the pi process an older command center left running
// for the session, and clears away what it kept.
func (s *agentSession) stopLegacyPi() {
	s.mu.Lock()
	pid, start := s.meta.PID, s.meta.PIDStart
	s.meta.PID, s.meta.PIDStart = 0, ""
	s.mu.Unlock()
	if pid > 0 && start != "" && procStart(pid) == start {
		syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 50 && procStart(pid) == start; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if procStart(pid) == start {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	for _, name := range []string{"pi.in", "pi.out", "pi.err", "pi", "cwd"} {
		os.RemoveAll(filepath.Join(s.dir, name))
	}
}

// legacyTranscript rebuilds the conversation of a session an older command
// center ran with pi, which kept it in pi's own files, from the session's
// stream: what the user typed and what the agent replied.
func (s *agentSession) legacyTranscript() []json.RawMessage {
	s.mu.Lock()
	events := append([]agentEvent(nil), s.events...)
	queued := len(s.inbox)
	s.mu.Unlock()
	var items []json.RawMessage
	for _, e := range events {
		var p struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		json.Unmarshal(e.Payload, &p)
		var text string
		if json.Unmarshal(p.Message.Content, &text) != nil {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			json.Unmarshal(p.Message.Content, &blocks)
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					text = strings.TrimSpace(text + "\n\n" + b.Text)
				}
			}
		}
		if text == "" {
			continue
		}
		switch p.Type {
		case "user":
			items = append(items, userItem(agentQueued{Text: text}))
		case "assistant":
			data, _ := json.Marshal(map[string]any{"role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text}}})
			items = append(items, data)
		}
	}
	// The messages still waiting are in the stream too; they join afterwards.
	for n := 0; n < queued && len(items) > 0 && head(items[len(items)-1]).Role == "user"; n++ {
		items = items[:len(items)-1]
	}
	return items
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

// ---------------------------------------------------------------------------
// Idle sessions

// sweep stops the container of every session that has been idle for cfg.Idle.
func (a *agentService) sweep() {
	for range time.Tick(time.Minute) {
		for _, s := range a.all() {
			s.mu.Lock()
			idle := !s.meta.Working && !s.running && !s.deleted && !s.parked && time.Since(s.lastActive) > a.cfg.Idle
			s.mu.Unlock()
			if idle {
				a.park(s)
			}
		}
	}
}

// park stops an idle session's container, unless a turn started meanwhile.
func (a *agentService) park(s *agentSession) {
	if !s.op.TryLock() {
		return
	}
	defer s.op.Unlock()
	if err := a.stopContainer(s.meta.ID); err != nil {
		log.Printf("backup session %s: stopping its container: %v", s.meta.ID, err)
	}
	s.mu.Lock()
	s.parked = true
	s.mu.Unlock()
}
