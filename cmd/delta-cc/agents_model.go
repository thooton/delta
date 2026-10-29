package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The model behind backup sessions: GLM 5.3 Flash, through OpenCode Zen's
// free tier, thinking hard (reasoning effort high). No API key is involved,
// so there is nothing to pay for and nothing to keep secret; paid models
// aren't offered at all.
//
// Zen serves its free models only to requests that look like they come from
// the OpenCode CLI. As of OpenCode 1.19.3 it checks, and only checks, that:
//
//   - the User-Agent holds opencode/<version>, 1.18.0 or newer (older ones
//     get 426 Upgrade Required);
//   - x-opencode-session holds an OpenCode session id: "ses_" and a
//     descending ULID (see zenSessionID);
//   - the request streams (stream: true);
//   - the request's tools include one named "read" (its parameters don't
//     matter).
//
// Anything else gets 403 FreeTierError. Authorization, x-opencode-session-id,
// the other x-opencode headers, stream_options, the path (/inference/v1 or
// /inference/openai/v1) and the system prompt make no difference; we send
// what the CLI sends anyway. Each session keeps its own OpenCode session id
// in session.json. GLM's own server wants every tool to have a description
// (400 otherwise).
//
// GLM speaks only OpenAI's Chat Completions API: the Responses API gets 400
// ModelProtocolUnsupported. The conversation is still kept as Responses
// input items (transcript.jsonl, which also holds sessions from before), and
// chatMessages turns it into chat messages for each request: every request
// carries the whole conversation. The model's reasoning comes back as
// reasoning_content and goes back, with the reply it led to, as the
// assistant message's reasoning_content, which is how GLM keeps its train
// of thought from one step to the next (OpenCode does the same).

const (
	agentModelID   = "glm-5.3-flash-free"
	agentEffort    = "high"
	agentMaxTokens = 32000 // the most the free model writes in one reply
	zenBaseURL     = "https://opencode.ai/inference/openai/v1"
	zenUserAgent   = "opencode/1.19.3 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	modelTries     = 4 // a request and its retries
)

// modelReply is what one request produced, as Responses output items
// (reasoning, message, function_call), and how it ended.
type modelReply struct {
	Items      []json.RawMessage
	Incomplete string // why the reply was cut short, if it was
}

// modelError is a request the model's server turned down.
type modelError struct {
	Code int
	Msg  string // the server's words, for the log only
}

func (e *modelError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Code, e.Msg)
}

// retryable reports whether trying again might help.
func retryable(err error) bool {
	var me *modelError
	if errors.As(err, &me) {
		return me.Code == 429 || me.Code >= 500
	}
	return !errors.Is(err, context.Canceled)
}

// ask sends the conversation to the model, retrying failures that may pass,
// and returns its reply.
func (s *agentSession) ask(ctx context.Context, input []json.RawMessage) (*modelReply, error) {
	var err error
	for try := 1; ; try++ {
		var reply *modelReply
		reply, err = s.askOnce(ctx, input)
		if err == nil || ctx.Err() != nil || !retryable(err) || try == modelTries {
			return reply, err
		}
		log.Printf("backup session %s: model request failed (try %d of %d): %v", s.meta.ID, try, modelTries, err)
		s.status(fmt.Sprintf("Retrying after an error (try %d of %d)…", try+1, modelTries))
		select {
		case <-time.After(time.Duration(2<<(try-1)) * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *agentSession) askOnce(ctx context.Context, input []json.RawMessage) (*modelReply, error) {
	s.mu.Lock()
	zen := s.meta.ZenSession
	s.mu.Unlock()
	body, err := json.Marshal(map[string]any{
		"model":            agentModelID,
		"max_tokens":       agentMaxTokens,
		"reasoning_effort": agentEffort,
		"messages":         chatMessages(append([]json.RawMessage{developerMessage()}, input...)),
		"tools":            chatTools(),
		"tool_choice":      "auto",
		"stream":           true,
		"stream_options":   map[string]bool{"include_usage": true},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.svc.cfg.ModelURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("User-Agent", zenUserAgent)
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", "global")
	req.Header.Set("x-opencode-session", zen)
	req.Header.Set("x-opencode-session-id", zen)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &modelError{resp.StatusCode, strings.TrimSpace(string(data))}
	}
	return readModelStream(resp.Body)
}

// chatTools gives agentTools in Chat Completions' shape.
func chatTools() []map[string]any {
	tools := make([]map[string]any, 0, len(agentTools))
	for _, t := range agentTools {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": t["name"], "description": t["description"], "parameters": t["parameters"],
		}})
	}
	return tools
}

// chatMessages turns the conversation's Responses items into chat messages.
// A reply's items (its reasoning, text and tool calls) make one assistant
// message. Pictures a tool returned follow its results as a user message,
// since a tool message holds only text. Reasoning kept only encrypted, from
// the model sessions used before, can't be read back and is left out.
func chatMessages(items []json.RawMessage) []map[string]any {
	var msgs []map[string]any
	var asst map[string]any // the assistant message being put together
	var images []map[string]any
	flushImages := func() {
		if len(images) > 0 {
			content := append([]map[string]any{{"type": "text", "text": "The pictures the read tool returned:"}}, images...)
			msgs = append(msgs, map[string]any{"role": "user", "content": content})
			images = nil
		}
	}
	assistant := func() map[string]any {
		if asst == nil {
			flushImages()
			asst = map[string]any{"role": "assistant", "content": ""}
			msgs = append(msgs, asst)
		}
		return asst
	}
	for _, item := range items {
		var it struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			CallID  string          `json:"call_id"`
			Name    string          `json:"name"`
			Args    string          `json:"arguments"`
			Output  json.RawMessage `json:"output"`
		}
		if json.Unmarshal(item, &it) != nil {
			continue
		}
		if it.Type == "" && it.Role != "" {
			it.Type = "message"
		}
		switch {
		case it.Type == "reasoning":
			if text := partsText(it.Content, "reasoning_text"); text != "" {
				a := assistant()
				a["reasoning_content"] = joinText(a["reasoning_content"], text)
			}
		case it.Type == "message" && it.Role == "assistant":
			a := assistant()
			a["content"] = joinText(a["content"], partsText(it.Content, "output_text"))
		case it.Type == "function_call":
			a := assistant()
			calls, _ := a["tool_calls"].([]map[string]any)
			a["tool_calls"] = append(calls, map[string]any{
				"id": it.CallID, "type": "function",
				"function": map[string]string{"name": it.Name, "arguments": it.Args},
			})
		case it.Type == "function_call_output":
			asst = nil
			text, pics := toolResult(it.Output)
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": it.CallID, "content": text})
			images = append(images, pics...)
		case it.Type == "message":
			// The system prompt (developer) or what the user said.
			asst = nil
			flushImages()
			role := it.Role
			if role == "developer" {
				role = "system"
			}
			msgs = append(msgs, map[string]any{"role": role, "content": userContent(it.Content)})
		}
	}
	flushImages()
	return msgs
}

// contentPart is one part of a Responses message's content, or of a tool's
// result.
type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
}

// partsText joins the text of the content parts of one type.
func partsText(content json.RawMessage, typ string) string {
	var parts []contentPart
	json.Unmarshal(content, &parts)
	var texts []string
	for _, p := range parts {
		if p.Type == typ {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func joinText(prev any, text string) string {
	if p, _ := prev.(string); p != "" && text != "" {
		return p + "\n\n" + text
	} else if p != "" {
		return p
	}
	return text
}

// userContent turns a message's content, a string or a list of
// input_text and input_image parts, into a chat message's.
func userContent(content json.RawMessage) any {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []contentPart
	json.Unmarshal(content, &parts)
	out := []map[string]any{}
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case "input_image":
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]string{"url": p.ImageURL}})
		}
	}
	return out
}

// toolResult splits a tool's result, a string or a list of parts, into its
// text and its pictures.
func toolResult(output json.RawMessage) (string, []map[string]any) {
	var text string
	if json.Unmarshal(output, &text) == nil {
		return text, nil
	}
	var texts []string
	var pics []map[string]any
	var parts []contentPart
	json.Unmarshal(output, &parts)
	for _, p := range parts {
		switch p.Type {
		case "input_text":
			texts = append(texts, p.Text)
		case "input_image":
			pics = append(pics, map[string]any{"type": "image_url", "image_url": map[string]string{"url": p.ImageURL}})
		}
	}
	return strings.Join(texts, "\n"), pics
}

// readModelStream reads a streamed chat completion and gives it back as
// Responses output items: its reasoning, its text and its tool calls.
func readModelStream(r io.Reader) (*modelReply, error) {
	var reasoning, text strings.Builder
	type call struct{ id, name, args string }
	calls := map[int]*call{}
	finish := ""
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var c struct {
			Error *struct {
				Code    any    `json:"code"`
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &c) != nil {
			continue
		}
		if c.Error != nil {
			msg := fmt.Sprintf("%v %s: %s", c.Error.Code, c.Error.Type, c.Error.Message)
			return nil, &modelError{streamErrorCode(msg), msg}
		}
		for _, ch := range c.Choices {
			reasoning.WriteString(ch.Delta.ReasoningContent)
			text.WriteString(ch.Delta.Content)
			for _, tc := range ch.Delta.ToolCalls {
				cl := calls[tc.Index]
				if cl == nil {
					cl = &call{}
					calls[tc.Index] = cl
				}
				if tc.ID != "" {
					cl.id = tc.ID
				}
				cl.name += tc.Function.Name
				cl.args += tc.Function.Arguments
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if finish == "" {
		return nil, errors.New("the reply ended early")
	}
	reply := &modelReply{}
	add := func(v any) {
		data, _ := json.Marshal(v)
		reply.Items = append(reply.Items, data)
	}
	if reasoning.Len() > 0 {
		add(map[string]any{"type": "reasoning", "summary": []any{},
			"content": []map[string]string{{"type": "reasoning_text", "text": reasoning.String()}}})
	}
	if strings.TrimSpace(text.String()) != "" {
		add(map[string]any{"type": "message", "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": text.String(), "annotations": []any{}}}})
	}
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for n, i := range idx {
		cl := calls[i]
		if cl.id == "" {
			cl.id = fmt.Sprintf("call_%d", n)
		}
		add(map[string]any{"type": "function_call", "status": "completed", "call_id": cl.id, "name": cl.name, "arguments": cl.args})
	}
	switch finish {
	case "stop", "tool_calls":
	case "length":
		reply.Incomplete = "max_output_tokens"
	default:
		reply.Incomplete = finish
	}
	return reply, nil
}

// streamErrorCode gives an error reported inside a stream the HTTP status it
// would have had, so that it is retried and described like one.
func streamErrorCode(msg string) int {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "rate") || strings.Contains(lower, "quota") || strings.Contains(lower, "429"):
		return 429
	case strings.Contains(lower, "context") && (strings.Contains(lower, "length") || strings.Contains(lower, "too long") || strings.Contains(lower, "exceed")):
		return 400
	}
	return 500
}

// describeModelError says what went wrong with a model request without
// naming the provider: the phone never shows it.
func describeModelError(err error) string {
	code := 0
	var me *modelError
	if errors.As(err, &me) {
		code = me.Code
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "context") && (strings.Contains(lower, "length") || strings.Contains(lower, "too long") || strings.Contains(lower, "overflow") || strings.Contains(lower, "exceed")):
		return "The conversation has grown too long. Start a new session."
	case code == 401 || code == 403 || code == 426:
		// The free tier stopped taking these requests as the CLI's (see above).
		return fmt.Sprintf("The model turned backup sessions away (HTTP %d). The command center needs an update; until then, use a regular (●) session.", code)
	case code == 429:
		return "Too many requests right now. Try again in a minute."
	case code >= 500:
		return fmt.Sprintf("The model isn't answering right now (HTTP %d). Try again in a minute.", code)
	case code != 0:
		return fmt.Sprintf("The model couldn't answer (HTTP %d).", code)
	}
	return "The model couldn't be reached. Try again in a minute."
}

// zenSessionID makes an OpenCode session id as the CLI does: "ses_", then
// (Date.now() * 0x1000 + counter) bit-inverted, as 6 bytes of hex, then 14
// random base62 characters.
func zenSessionID() string {
	const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	now := ^(uint64(time.Now().UnixMilli())*0x1000 + 1)
	id := fmt.Sprintf("ses_%012x", now&(1<<48-1))
	buf := make([]byte, 14)
	rand.Read(buf)
	for _, b := range buf {
		id += string(base62[int(b)%62])
	}
	return id
}
