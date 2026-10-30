package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// shorthandResult is what the command center's shorthand reader returns
// (see cmd/delta-cc/shorthand.go and the worker in the topaz project).
type shorthandResult struct {
	Lines []struct {
		Grascii    string `json:"grascii"`
		English    string `json:"english"` // the finished line: DeepSeek's, or the draft
		Draft      string `json:"draft"`   // the Grascii-to-English model's
		Postedited *bool  `json:"postedited"`
		Postedit   string `json:"postedit"` // "done", "salvaged" (answered from cut-off reasoning) or "draft"
		Box        []int  `json:"box"`
		Outlines   []struct {
			Outline string   `json:"outline"`
			Words   []string `json:"words"`
			Exact   *bool    `json:"exact"`
		} `json:"outlines"`
	} `json:"lines"`
	Image struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"image"`
	Seconds  float64 `json:"seconds"`
	Postedit *struct {
		Model  string   `json:"model"`
		Errors []string `json:"errors"`
	} `json:"postedit"`
}

const shorthandUsage = "usage: transform shorthand <image.png|.jpg> [--line] [--region x,y,w,h] [--json]"

// A reading takes minutes (the worker's post-edit), longer than the proxy in
// front of the command center lets one request wait, so it is submitted, and
// then polled for with short requests until it is done.
var (
	shorthandSubmit             = &http.Client{Timeout: 90 * time.Second} // the upload; it is answered at once
	shorthandPoll               = &http.Client{Timeout: 30 * time.Second} // each poll; it is answered at once
	shorthandEvery              = 4 * time.Second                         // between polls
	shorthandProgress           = 30 * time.Second                        // between progress lines while waiting
	shorthandGiveUp             = 12 * time.Minute                        // the command center itself gives up at 10
	shorthandStatus   io.Writer = os.Stderr                               // where progress lines go
)

func shorthand(args []string) error {
	var pos []string
	q := url.Values{}
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--line" || a == "--lines":
			q.Set("mode", "line")
		case a == "--json":
			asJSON = true
		case a == "--region" && i+1 < len(args):
			i++
			q.Set("region", args[i])
		case strings.HasPrefix(a, "--region="):
			q.Set("region", strings.TrimPrefix(a, "--region="))
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown option %s (%s)", a, shorthandUsage)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) != 1 {
		return errors.New(shorthandUsage)
	}
	image, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	if strings.HasPrefix(string(image), "%PDF-") {
		return errors.New("that's a PDF: render the page to a PNG first (SHORTHAND in `transform help` shows how)")
	}
	k, err := key()
	if err != nil {
		return err
	}
	path := "/transform/shorthand"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	data, err := readShorthand(k, path, image)
	if err != nil {
		if strings.Contains(err.Error(), "shorthand service offline") {
			return errors.New("shorthand service offline: the shorthand reader isn't connected right now. Read the shorthand from the page images yourself, as best you can")
		}
		return err
	}
	if asJSON {
		os.Stdout.Write(data)
		return nil
	}
	var r shorthandResult
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("the shorthand reader's answer isn't readable: %w", err)
	}
	fmt.Print(formatShorthand(pos[0], r))
	return nil
}

// readShorthand submits image to the shorthand reader at path (with its
// query) and polls until the reading is done, returning the result. Only a
// poll is retried, and only when it failed in passing (no answer, or a 5xx
// such as the proxy's 524); the reading is never submitted twice.
func readShorthand(k, path string, image []byte) ([]byte, error) {
	data, err := callWith(shorthandSubmit, k, http.MethodPost, path, "application/octet-stream", image)
	if err != nil {
		return nil, err
	}
	var sub struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &sub) != nil || sub.ID == "" {
		return nil, fmt.Errorf("the shorthand reader's answer to the submission isn't readable: %.200s", data)
	}
	poll, _, _ := strings.Cut(path, "?")
	poll += "/" + url.PathEscape(sub.ID)
	start := time.Now()
	fmt.Fprintf(shorthandStatus, "transform shorthand: submitted (reading %s); this takes a few minutes, and a progress line comes every %d s.\n", sub.ID, int(shorthandProgress/time.Second))
	var said time.Time
	lastState, failed := "", ""
	for {
		if time.Since(start) > shorthandGiveUp {
			msg := fmt.Sprintf("the shorthand reader didn't finish within %d minutes", int(shorthandGiveUp/time.Minute))
			if failed != "" {
				msg += " (the last poll failed: " + failed + ")"
			}
			return nil, errors.New(msg + "; try a smaller region, or read the shorthand from the page images yourself")
		}
		time.Sleep(shorthandEvery)
		status, data, err := shorthandGet(k, poll)
		switch {
		case err != nil:
			failed = err.Error()
			continue
		case status >= 500:
			failed = fmt.Sprintf("HTTP %d", status)
			continue
		case status != http.StatusOK:
			return nil, serverError(status, data)
		}
		var st struct {
			Status string          `json:"status"`
			State  string          `json:"state"`
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
			Code   int             `json:"code"`
		}
		if err := json.Unmarshal(data, &st); err != nil {
			failed = "an unreadable answer"
			continue
		}
		failed = ""
		switch st.Status {
		case "done":
			return st.Result, nil
		case "error":
			if st.Error == "" {
				st.Error = fmt.Sprintf("the shorthand reader failed (%d)", st.Code)
			}
			return nil, errors.New(st.Error)
		}
		if st.State != lastState || time.Since(said) >= shorthandProgress {
			fmt.Fprintf(shorthandStatus, "transform shorthand: still %s after %d s...\n", st.State, int(time.Since(start).Seconds()))
			said, lastState = time.Now(), st.State
		}
	}
}

// shorthandGet makes one short poll, returning its status and body.
func shorthandGet(k, path string) (int, []byte, error) {
	base, err := server()
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodGet, base+path, bytes.NewReader(nil))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k)
	resp, err := shorthandPoll.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, data, err
}

// formatShorthand lays out a result for the agent, line by line: the
// finished English, and under it the strokes it was read from, compactly.
//
// The worker's DeepSeek post-edit has already weighed the strokes, the
// dictionary's words and the draft together (about one word in eight wrong
// on book pages, against one in four for the draft), so the agent gets its
// line as the transcription, to check lightly rather than redo. What it
// needs for that check is where the reading is doubtful: the outlines the
// dictionary doesn't know (~), with the words of similar outlines. Every
// other outline's dictionary words are left out, since the post-edit has
// already chosen among them. A line the post-edit didn't finish (it failed,
// or is off) is marked "draft", and gets every outline's words, as before,
// because the agent has to finish that one itself.
func formatShorthand(name string, r shorthandResult) string {
	var b strings.Builder
	n := len(r.Lines)
	if n == 0 {
		fmt.Fprintf(&b, "No lines of shorthand were found in %s (%dx%d px). If there is shorthand on it, try a region around it, or --line for a single line.\n", name, r.Image.Width, r.Image.Height)
		return b.String()
	}
	plural := "s"
	if n == 1 {
		plural = ""
	}
	drafts := 0
	for _, ln := range r.Lines {
		if ln.Postedited == nil || !*ln.Postedited {
			drafts++
		}
	}
	fmt.Fprintf(&b, "Shorthand in %s (%dx%d px): %d line%s, read in %.0f s.\n", name, r.Image.Width, r.Image.Height, n, plural, r.Seconds)
	switch {
	case drafts == 0:
		b.WriteString("TRANSCRIBED: each line's English is finished (a language model read the strokes, the\n")
		b.WriteString("dictionary's words and a draft together). Use it as the text; check it lightly (see SHORTHAND).\n")
	case drafts == n:
		b.WriteString("DRAFT ONLY: the finishing step didn't run, so each line's English is the Grascii-to-English\n")
		b.WriteString("model's rough guess (a fifth to a third of the words wrong). Work out each line from it, the\n")
		b.WriteString("outlines' dictionary words and the page image (see SHORTHAND).\n")
	default:
		fmt.Fprintf(&b, "TRANSCRIBED, but %d line%s marked \"draft\" only have the rough Grascii-to-English guess (a fifth\n", drafts, map[bool]string{true: "", false: "s"}[drafts == 1])
		b.WriteString("to a third of the words wrong): work those out from the outlines' words and the image (see SHORTHAND).\n")
	}
	for _, ln := range r.Lines {
		if ln.Postedit == "salvaged" && ln.Postedited != nil && *ln.Postedited {
			b.WriteString("Lines marked \"salvaged\" were finished in a hurry (the model ran long): check them a little closer.\n")
			break
		}
	}
	b.WriteString("Under each line: its strokes (Grascii); ~ marks an outline not in the dictionary, the likeliest\n")
	b.WriteString("misreading, with the words of similar outlines.\n\n")
	for i, ln := range r.Lines {
		draft := ln.Postedited == nil || !*ln.Postedited
		mark := ""
		if draft && drafts < n {
			mark = "draft: "
		} else if !draft && ln.Postedit == "salvaged" {
			mark = "salvaged: "
		}
		text := ln.English
		if strings.TrimSpace(text) == "" {
			text = "(no English)"
		}
		fmt.Fprintf(&b, "%3d  %s%s\n", i+1, mark, text)
		var parts []string
		for _, o := range ln.Outlines {
			p := o.Outline
			fuzzy := o.Exact != nil && !*o.Exact
			if fuzzy {
				p = "~" + p
			}
			if len(o.Words) > 0 && (fuzzy || draft) {
				p += " (" + strings.Join(o.Words, "/") + ")"
			}
			parts = append(parts, p)
		}
		if len(parts) == 0 {
			parts = []string{"(nothing read)"}
		}
		box := ""
		if len(ln.Box) == 4 {
			box = fmt.Sprintf("  [x %d-%d, y %d-%d]", ln.Box[0], ln.Box[2], ln.Box[1], ln.Box[3])
		}
		fmt.Fprintf(&b, "     %s%s\n", strings.Join(parts, " "), box)
	}
	return b.String()
}
