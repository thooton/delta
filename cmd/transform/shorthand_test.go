package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const shorthandSample = `{"lines": [
 {"grascii": "TH G '-EU-M-R", "english": "The good humour", "draft": "The good humor", "postedited": %s, "box": [10, 20, 300, 60],
  "outlines": [{"outline": "TH", "words": ["the", "there"], "exact": true}, {"outline": "G", "words": ["good", "go"], "exact": true},
               {"outline": "'-EU-M-R", "words": ["humor", "humour"], "exact": false}]},
 {"grascii": "¶", "english": "", "draft": "", "postedited": %s, "box": [10, 70, 40, 90], "outlines": [{"outline": "¶"}]}],
 "image": {"width": 960, "height": 1295}, "seconds": 84.2}`

func sampleShorthand(t *testing.T, first, second string) shorthandResult {
	var r shorthandResult
	if err := json.Unmarshal([]byte(strings.Replace(strings.Replace(shorthandSample, "%s", first, 1), "%s", second, 1)), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFormatShorthand(t *testing.T) {
	out := formatShorthand("p1.png", sampleShorthand(t, "true", "true"))
	for _, want := range []string{"2 lines, read in 84 s", "TRANSCRIBED:", "  1  The good humour\n",
		"     TH G ~'-EU-M-R (humor/humour)  [x 10-300, y 20-60]\n", "  2  (no English)\n", "     ¶  [x"} {
		if !strings.Contains(out, want) {
			t.Errorf("post-edited: missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "the/there") || strings.Contains(out, "draft:") {
		t.Errorf("post-edited: exact outlines' words or a draft mark shown:\n%s", out)
	}

	out = formatShorthand("p1.png", sampleShorthand(t, "false", "true"))
	for _, want := range []string{"1 line marked \"draft\"", "  1  draft: The good humour\n", "TH (the/there) G (good/go) ~'-EU-M-R (humor/humour)"} {
		if !strings.Contains(out, want) {
			t.Errorf("mixed: missing %q in\n%s", want, out)
		}
	}

	r := sampleShorthand(t, "true", "true")
	r.Lines[0].Postedit = "salvaged"
	if out := formatShorthand("p1.png", r); !strings.Contains(out, "  1  salvaged: The good humour\n") || !strings.Contains(out, "finished in a hurry") {
		t.Errorf("salvaged: %s", out)
	}

	out = formatShorthand("p1.png", sampleShorthand(t, "false", "false"))
	if !strings.Contains(out, "DRAFT ONLY") || strings.Contains(out, "draft: ") {
		t.Errorf("all drafts: want the DRAFT ONLY header and no per-line marks:\n%s", out)
	}

	// An older worker, without postedited: its English is the draft.
	var old shorthandResult
	json.Unmarshal([]byte(`{"lines": [{"grascii": "T", "english": "it", "outlines": [{"outline": "T", "words": ["it", "at"], "exact": true}]}]}`), &old)
	if out := formatShorthand("x.png", old); !strings.Contains(out, "DRAFT ONLY") || !strings.Contains(out, "T (it/at)") {
		t.Errorf("old worker: %s", out)
	}

	if out := formatShorthand("x.png", shorthandResult{}); !strings.Contains(out, "No lines of shorthand") {
		t.Errorf("empty: %s", out)
	}
}

// fakeShorthand serves the command center's shorthand routes: a reading is
// accepted, then each poll answers with the next of polls (a status, and a
// body or "" for {"status":"pending"}).
func fakeShorthand(t *testing.T, polls []fakePoll) (*int, *int) {
	submits, n := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k1" {
			http.Error(w, `{"error":"bad key"}`, 401)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/transform/shorthand":
			submits++
			if r.URL.Query().Get("region") != "1,2,3,4" {
				t.Errorf("submitted with %s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"id":"abc","status":"pending"}`)
		case r.Method == "GET" && r.URL.Path == "/transform/shorthand/abc":
			if n >= len(polls) {
				t.Errorf("poll %d: more polls than expected", n)
				w.WriteHeader(500)
				return
			}
			p := polls[n]
			n++
			if p.body == "" {
				p.body = `{"status":"pending","state":"reading","seconds":5}`
			}
			w.WriteHeader(p.status)
			io.WriteString(w, p.body)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TRANSFORM_SERVER", srv.URL)
	return &submits, &n
}

type fakePoll struct {
	status int
	body   string
}

func fastShorthand(t *testing.T) *bytes.Buffer {
	every, progress, giveUp, out := shorthandEvery, shorthandProgress, shorthandGiveUp, shorthandStatus
	t.Cleanup(func() {
		shorthandEvery, shorthandProgress, shorthandGiveUp, shorthandStatus = every, progress, giveUp, out
	})
	var buf bytes.Buffer
	shorthandEvery, shorthandProgress, shorthandGiveUp, shorthandStatus = time.Millisecond, time.Hour, time.Minute, &buf
	return &buf
}

func TestReadShorthandPolls(t *testing.T) {
	progress := fastShorthand(t)
	submits, polls := fakeShorthand(t, []fakePoll{
		{200, `{"status":"pending","state":"queued","seconds":0}`},
		{200, ""},
		{524, "error code: 524"}, // the proxy, in passing
		{502, "<html>Bad gateway</html>"},
		{200, ""},
		{200, `{"status":"done","result":{"lines":[{"english":"I'll go."}]}}`},
	})
	data, err := readShorthand("k1", "/transform/shorthand?region=1,2,3,4", []byte("\x89PNG"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"lines":[{"english":"I'll go."}]}` {
		t.Errorf("result %s", data)
	}
	if *submits != 1 || *polls != 6 {
		t.Errorf("%d submits and %d polls, want 1 and 6", *submits, *polls)
	}
	p := progress.String()
	for _, want := range []string{"submitted (reading abc)", "still queued after", "still reading after"} {
		if !strings.Contains(p, want) {
			t.Errorf("progress lacks %q:\n%s", want, p)
		}
	}
	if strings.Count(p, "still reading") != 1 {
		t.Errorf("a progress line per poll rather than per change or interval:\n%s", p)
	}
}

func TestReadShorthandFails(t *testing.T) {
	fastShorthand(t)
	for _, c := range []struct {
		name  string
		polls []fakePoll
		want  string
	}{
		{"the worker's error", []fakePoll{{200, ""}, {200, `{"status":"error","code":502,"error":"the shorthand reader lost this request; try again"}`}}, "lost this request"},
		{"offline", []fakePoll{{200, `{"status":"error","code":503,"error":"shorthand service offline: gone"}`}}, "Read the shorthand from the page images yourself"},
		{"forgotten", []fakePoll{{524, ""}, {404, `{"error":"no such shorthand reading"}`}}, "no such shorthand reading"},
	} {
		t.Run(c.name, func(t *testing.T) {
			submits, _ := fakeShorthand(t, c.polls)
			k := t.TempDir()
			t.Setenv("APPDATA", k)
			t.Setenv("XDG_CONFIG_HOME", k)
			t.Setenv("HOME", k)
			dir, _ := os.UserConfigDir()
			os.MkdirAll(filepath.Join(dir, "transform"), 0o700)
			os.WriteFile(filepath.Join(dir, "transform", "key"), []byte("k1\n"), 0o600)
			img := filepath.Join(k, "p.png")
			os.WriteFile(img, []byte("\x89PNG\r\n\x1a\n"), 0o600)
			err := shorthand([]string{img, "--region", "1,2,3,4"})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want %q", err, c.want)
			}
			if *submits != 1 {
				t.Errorf("%d submits", *submits)
			}
		})
	}
}

func TestReadShorthandGivesUp(t *testing.T) {
	fastShorthand(t)
	shorthandGiveUp = 30 * time.Millisecond
	shorthandEvery = 5 * time.Millisecond
	polls := make([]fakePoll, 200)
	for i := range polls {
		polls[i] = fakePoll{524, "error code: 524"}
	}
	submits, _ := fakeShorthand(t, polls)
	_, err := readShorthand("k1", "/transform/shorthand?region=1,2,3,4", []byte("\x89PNG"))
	if err == nil || !strings.Contains(err.Error(), "didn't finish within") || !strings.Contains(err.Error(), "HTTP 524") {
		t.Errorf("error %v", err)
	}
	if *submits != 1 {
		t.Errorf("%d submits: a reading is never resubmitted", *submits)
	}
}
