package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	if err := runSteps(expect,
		step{"the shorthand reader requires the token", 401, "POST", "/shorthand", "", png},
		step{"the shorthand reader is offline with no worker", 503, "POST", "/shorthand", token, png},
		step{"the worker routes need a secret set", 503, "POST", "/shorthand/worker/next", secret, nil},
		step{"the delta token reaches no transform job's shorthand", 403, "POST", "/transform/shorthand", token, png},
	); err != nil {
		return err
	}
	if _, err := writeKeyHash(dir, shorthandSecretFile, secret); err != nil {
		return err
	}
	if err := runSteps(expect,
		step{"the worker routes refuse the delta token", 401, "POST", "/shorthand/worker/next", token, []byte("{}")},
		step{"the worker routes refuse a wrong secret", 401, "POST", "/shorthand/worker/next", secret + "x", []byte("{}")},
		step{"the worker's secret isn't the delta token", 401, "GET", "/files", secret, []byte("{}")},
		step{"the worker's secret starts no reading", 401, "POST", "/shorthand", secret, []byte("{}")},
		step{"no result for a request never made", 404, "POST", "/shorthand/worker/result/0123", secret, []byte("{}")},
	); err != nil {
		return err
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
		resp, data, err := testRequest(ctx, "POST", base+path, secret, body)
		if err != nil {
			return 0, nil, err
		}
		return resp.StatusCode, data, nil
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
	if err := runSteps(expect,
		step{"a reading takes images only", 400, "POST", "/shorthand", token, []byte("%PDF-1.4")},
		step{"a reading's mode is page or line", 400, "POST", "/shorthand?mode=word", token, png},
		step{"a region is four numbers", 400, "POST", "/shorthand?region=1,2,3", token, png},
		step{"a region has a size", 400, "POST", "/shorthand?region=1,2,0,4", token, png},
	); err != nil {
		return err
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
	if err := runSteps(expect,
		step{"a reading never submitted isn't there", 404, "GET", "/shorthand/0123", token, nil},
		step{"a reading is fetched with the token", 401, "GET", "/shorthand/" + id, "", nil},
	); err != nil {
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
	_, err = expect("a reload's checkpoint stays in the repository", 400, "POST", "/shorthand/reload", token, []byte(`{"g2e":"../x"}`))
	return err
}
