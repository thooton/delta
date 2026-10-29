package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// shorthandSelfTest checks the shorthand reader's relay with a stand-in
// worker: that it says it is offline until a worker polls, that only the
// worker's secret reaches the worker routes (and reaches nothing else), and
// that a reading reaches the worker, with its image and region, and its
// result or error comes back to the caller, who submits it and then polls
// for it.
func shorthandSelfTest(expect expectFunc, base, token, dir string) error {
	png := []byte("\x89PNG\r\n\x1a\n a page of shorthand")
	const secret = shorthandSecretStart + "selftest"
	steps := []struct {
		desc, method, path, tok string
		want                    int
		body                    []byte
	}{
		{"the shorthand reader requires the token", "POST", "/shorthand", "", 401, png},
		{"the shorthand reader is offline with no worker", "POST", "/shorthand", token, 503, png},
		{"the worker routes need a secret set", "POST", "/shorthand/worker/next", secret, 503, nil},
		{"the delta token reaches no transform job's shorthand", "POST", "/transform/shorthand", token, 403, png},
	}
	for _, s := range steps {
		if _, err := expect(s.desc, s.want, s.method, s.path, s.tok, s.body); err != nil {
			return err
		}
	}
	sum := sha256.Sum256([]byte(secret))
	if err := os.WriteFile(filepath.Join(dir, shorthandSecretFile), []byte(hex.EncodeToString(sum[:])+"\n"), 0o600); err != nil {
		return err
	}
	for _, s := range []struct {
		desc, method, path, tok string
		want                    int
	}{
		{"the worker routes refuse the delta token", "POST", "/shorthand/worker/next", token, 401},
		{"the worker routes refuse a wrong secret", "POST", "/shorthand/worker/next", secret + "x", 401},
		{"the worker's secret isn't the delta token", "GET", "/files", secret, 401},
		{"the worker's secret starts no reading", "POST", "/shorthand", secret, 401},
		{"no result for a request never made", "POST", "/shorthand/worker/result/0123", secret, 404},
	} {
		body := []byte("{}")
		if _, err := expect(s.desc, s.want, s.method, s.path, s.tok, body); err != nil {
			return err
		}
	}

	// The stand-in worker: it takes requests and answers each as told.
	type taken struct {
		job shorthandJob
		err error
	}
	got := make(chan taken, 4)
	answer := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := ctx.Done()
	post := func(path string, body []byte) (int, []byte, error) {
		req, _ := http.NewRequestWithContext(ctx, "POST", base+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		return resp.StatusCode, data, err
	}
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			status, data, err := post("/shorthand/worker/next", []byte(`{"worker":"w1","info":{"ocr":"stand-in"}}`))
			if err != nil || status == http.StatusNoContent {
				continue
			}
			var j shorthandJob
			if status != 200 || json.Unmarshal(data, &j) != nil {
				err = fmt.Errorf("poll: HTTP %d %s", status, data)
			}
			got <- taken{j, err}
			if err != nil {
				return
			}
			select {
			case a := <-answer:
				post("/shorthand/worker/result/"+j.ID, []byte(a))
			case <-stop:
				return
			}
		}
	}()
	online := false
	for i := 0; i < 100 && !online; i++ {
		data, err := expect("the reader's status", 200, "GET", "/shorthand", token, nil)
		if err != nil {
			return err
		}
		online = strings.Contains(string(data), `"online":true`) && strings.Contains(string(data), `"stand-in"`)
		time.Sleep(20 * time.Millisecond)
	}
	if !online {
		return fmt.Errorf("the reader isn't online while a worker polls")
	}
	for _, s := range []struct {
		desc, path string
		want       int
		body       []byte
	}{
		{"a reading takes images only", "/shorthand", 400, []byte("%PDF-1.4")},
		{"a reading's mode is page or line", "/shorthand?mode=word", 400, png},
		{"a region is four numbers", "/shorthand?region=1,2,3", 400, png},
		{"a region has a size", "/shorthand?region=1,2,0,4", 400, png},
	} {
		if _, err := expect(s.desc, s.want, "POST", s.path, token, s.body); err != nil {
			return err
		}
	}

	// read submits a reading and polls for it until it is no longer pending.
	read := func(desc, path string) (string, map[string]json.RawMessage, error) {
		data, err := expect(desc+": submitted", 202, "POST", path, token, png)
		if err != nil {
			return "", nil, err
		}
		var sub struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if json.Unmarshal(data, &sub) != nil || sub.ID == "" || sub.Status != "pending" {
			return "", nil, fmt.Errorf("%s: the submission came back as %s", desc, data)
		}
		for i := 0; i < 250; i++ {
			data, err := expect(desc+": polled", 200, "GET", "/shorthand/"+sub.ID, token, nil)
			if err != nil {
				return "", nil, err
			}
			var st map[string]json.RawMessage
			if err := json.Unmarshal(data, &st); err != nil {
				return "", nil, fmt.Errorf("%s: a poll came back as %s", desc, data)
			}
			if string(st["status"]) != `"pending"` {
				return sub.ID, st, nil
			}
			time.Sleep(20 * time.Millisecond)
		}
		return "", nil, fmt.Errorf("%s: still pending after 5 s", desc)
	}
	same := func(a, b []byte) bool {
		var x, y bytes.Buffer
		return json.Compact(&x, a) == nil && json.Compact(&y, b) == nil && bytes.Equal(x.Bytes(), y.Bytes())
	}

	result := `{"lines":[{"grascii":"I-L G T-U TH M-A-T","english":"I'll go to the mat.","outlines":[{"outline":"I-L","words":["while","I will"],"exact":true}]}]}`
	answer <- result
	id, st, err := read("a reading reaches the worker and comes back", "/shorthand?mode=line&region=1,2,30,40")
	if err != nil {
		return err
	}
	if string(st["status"]) != `"done"` || !same(st["result"], []byte(result)) {
		return fmt.Errorf("the reading came back as %v", st)
	}
	if data, err := expect("a result is kept for its caller", 200, "GET", "/shorthand/"+id, token, nil); err != nil {
		return err
	} else if !strings.Contains(string(data), `"done"`) {
		return fmt.Errorf("the kept result came back as %s", data)
	}
	if _, err := expect("a reading never submitted isn't there", 404, "GET", "/shorthand/0123", token, nil); err != nil {
		return err
	}
	if _, err := expect("a reading is fetched with the token", 401, "GET", "/shorthand/"+id, "", nil); err != nil {
		return err
	}
	t := <-got
	if t.err != nil {
		return t.err
	}
	if t.job.Kind != "read" || !bytes.Equal(t.job.Image, png) || t.job.Mode != "line" || fmt.Sprint(t.job.Region) != "[1 2 30 40]" {
		return fmt.Errorf("the worker got %+v", t.job)
	}
	answer <- `{"error":"the image can't be read","status":400}`
	if _, st, err := read("the worker's refusal reaches the caller", "/shorthand"); err != nil {
		return err
	} else if string(st["status"]) != `"error"` || string(st["code"]) != "400" || !strings.Contains(string(st["error"]), "can't be read") {
		return fmt.Errorf("the refusal came back as %v", st)
	}
	<-got
	answer <- `{"reloaded":true,"g2e":"state/best"}`
	if _, err := expect("a reload reaches the worker", 200, "POST", "/shorthand/reload", token, []byte(`{"g2e":"state/best"}`)); err != nil {
		return err
	}
	if t := <-got; t.job.Kind != "reload" || t.job.G2E != "state/best" {
		return fmt.Errorf("the worker got %+v for a reload", t.job)
	}
	if _, err := expect("a reload's checkpoint stays in the repository", 400, "POST", "/shorthand/reload", token, []byte(`{"g2e":"../x"}`)); err != nil {
		return err
	}
	return nil
}
