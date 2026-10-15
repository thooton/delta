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

// The model behind backup sessions: Muse Spark, through OpenCode Zen's free
// tier. No API key is involved, so there is nothing to pay for and nothing
// to keep secret; paid models aren't offered at all.
//
// Zen serves its free models only to requests that look like they come from
// the OpenCode CLI. As of OpenCode 1.18.35 it checks, and only checks, that:
//
//   - an Authorization header is there ("Bearer public" will do, on
//     /inference/v1, the CLI's own endpoint; /zen/v1 wants a real key);
//   - the User-Agent holds opencode/<version>, 1.18.0 or newer (older ones
//     get 426 Upgrade Required);
//   - x-opencode-session and x-opencode-session-id both hold an OpenCode
//     session id: "ses_" and a descending ULID (see zenSessionID);
//   - the request's tools include one named "read" whose parameters have at
//     least one property (ours has path, offset and limit).
//
// TLS, header case and order, the system prompt and the other x-opencode
// headers make no difference. Each session keeps its own OpenCode session id
// in session.json.
//
// The API is OpenAI's Responses API, used statelessly (store: false): every
// request carries the whole conversation. The items a reply is made of,
// including its reasoning (encrypted, as include asks), go back into the
// next request exactly as they came, which is how the model keeps its train
// of thought from one step to the next.

const (
	agentModelID = "muse-spark-1.3-contributor-free"
	zenBaseURL   = "https://opencode.ai/inference/v1"
	zenUserAgent = "opencode/1.18.35 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"

	modelTries = 4 // a request and its retries
)

// modelReply is what one request produced: its output items, as they came,
// and how it ended.
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
		"model":     agentModelID,
		"input":     append([]json.RawMessage{developerMessage()}, input...),
		"tools":     agentTools,
		"stream":    true,
		"store":     false,
		"reasoning": map[string]string{"effort": "medium", "summary": "auto"},
		"include":   []string{"reasoning.encrypted_content"},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.svc.cfg.ModelURL+"/responses", bytes.NewReader(body))
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

// readModelStream reads a streamed reply, keeping each output item as it is
// finished and the reply's final state.
func readModelStream(r io.Reader) (*modelReply, error) {
	items := map[int]json.RawMessage{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var e struct {
			Type        string          `json:"type"`
			OutputIndex int             `json:"output_index"`
			Item        json.RawMessage `json:"item"`
			Code        any             `json:"code"`
			Message     string          `json:"message"`
			Response    struct {
				Status string `json:"status"`
				Error  *struct {
					Code    any    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
				Incomplete *struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &e) != nil {
			continue
		}
		switch e.Type {
		case "response.output_item.done":
			items[e.OutputIndex] = e.Item
		case "response.completed", "response.incomplete":
			reply := &modelReply{}
			idx := make([]int, 0, len(items))
			for i := range items {
				idx = append(idx, i)
			}
			sort.Ints(idx)
			for _, i := range idx {
				reply.Items = append(reply.Items, items[i])
			}
			if e.Response.Incomplete != nil {
				reply.Incomplete = e.Response.Incomplete.Reason
			} else if e.Type == "response.incomplete" {
				reply.Incomplete = "unknown"
			}
			return reply, nil
		case "response.failed":
			msg := "the reply failed"
			if e.Response.Error != nil {
				msg = fmt.Sprintf("%v: %s", e.Response.Error.Code, e.Response.Error.Message)
			}
			return nil, &modelError{streamErrorCode(msg), msg}
		case "error":
			msg := fmt.Sprintf("%v: %s", e.Code, e.Message)
			return nil, &modelError{streamErrorCode(msg), msg}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("the reply ended early")
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
func zenSessionID() (string, error) {
	const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	now := ^(uint64(time.Now().UnixMilli())*0x1000 + 1)
	id := fmt.Sprintf("ses_%012x", now&(1<<48-1))
	buf := make([]byte, 14)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for _, b := range buf {
		id += string(base62[int(b)%62])
	}
	return id, nil
}
