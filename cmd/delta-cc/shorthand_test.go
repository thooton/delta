package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// shorthandServer serves s's reading routes for any owner (the path names
// it), and the worker's routes without their secret.
func shorthandServer(t *testing.T, s *shorthandService) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /read/{owner}", func(w http.ResponseWriter, r *http.Request) { s.read(w, r, r.PathValue("owner")) })
	mux.HandleFunc("GET /read/{owner}/{id}", func(w http.ResponseWriter, r *http.Request) { s.poll(w, r, r.PathValue("owner")) })
	mux.HandleFunc("POST /next", s.next)
	mux.HandleFunc("POST /result/{id}", s.result)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func shorthandDo(t *testing.T, method, url string, body []byte) (int, map[string]json.RawMessage) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]json.RawMessage
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestShorthandAsync(t *testing.T) {
	s := newShorthandService(t.TempDir())
	srv := shorthandServer(t, s)
	png := []byte("\x89PNG\r\n\x1a\n a page")

	if code, _ := shorthandDo(t, "POST", srv.URL+"/read/a", png); code != http.StatusServiceUnavailable {
		t.Fatalf("offline: HTTP %d, want 503", code)
	}

	s.mu.Lock()
	s.seen = time.Now() // a worker polled just now
	s.mu.Unlock()
	code, sub := shorthandDo(t, "POST", srv.URL+"/read/a", png)
	var id string
	json.Unmarshal(sub["id"], &id)
	if code != http.StatusAccepted || id == "" || string(sub["status"]) != `"pending"` {
		t.Fatalf("submit: HTTP %d %v", code, sub)
	}
	if code, _ := shorthandDo(t, "GET", srv.URL+"/read/b/"+id, nil); code != http.StatusNotFound {
		t.Fatalf("another owner's poll: HTTP %d, want 404", code)
	}
	if code, st := shorthandDo(t, "GET", srv.URL+"/read/a/"+id, nil); code != 200 || string(st["status"]) != `"pending"` || string(st["state"]) != `"queued"` {
		t.Fatalf("queued poll: HTTP %d %v", code, st)
	}

	code, job := shorthandDo(t, "POST", srv.URL+"/next", []byte(`{"worker":"w1"}`))
	if code != 200 || string(job["id"]) != `"`+id+`"` {
		t.Fatalf("the worker got HTTP %d %v", code, job)
	}
	if _, st := shorthandDo(t, "GET", srv.URL+"/read/a/"+id, nil); string(st["state"]) != `"reading"` {
		t.Fatalf("reading poll: %v", st)
	}
	result := `{"lines":[{"english":"I'll go."}]}`
	if code, _ := shorthandDo(t, "POST", srv.URL+"/result/"+id, []byte(result)); code != 200 {
		t.Fatalf("result: HTTP %d", code)
	}
	for i := 0; i < 2; i++ { // and it is kept
		code, st := shorthandDo(t, "GET", srv.URL+"/read/a/"+id, nil)
		if code != 200 || string(st["status"]) != `"done"` || string(st["result"]) != result {
			t.Fatalf("done poll %d: HTTP %d %v", i, code, st)
		}
	}

	s.mu.Lock()
	s.reads[id].finished = time.Now().Add(-shorthandKeep - time.Second)
	s.mu.Unlock()
	if code, _ := shorthandDo(t, "GET", srv.URL+"/read/a/"+id, nil); code != http.StatusNotFound {
		t.Fatalf("an old result: HTTP %d, want 404", code)
	}
}

func TestShorthandAsyncFailures(t *testing.T) {
	s := newShorthandService(t.TempDir())
	srv := shorthandServer(t, s)
	png := []byte("\x89PNG\r\n\x1a\n a page")
	submit := func() string {
		s.mu.Lock()
		s.seen = time.Now()
		s.mu.Unlock()
		_, sub := shorthandDo(t, "POST", srv.URL+"/read/a", png)
		var id string
		json.Unmarshal(sub["id"], &id)
		return id
	}
	errorOf := func(id string) (int, string) {
		_, st := shorthandDo(t, "GET", srv.URL+"/read/a/"+id, nil)
		var e struct {
			Status string `json:"status"`
			Code   int    `json:"code"`
			Error  string `json:"error"`
		}
		raw, _ := json.Marshal(st)
		json.Unmarshal(raw, &e)
		if e.Status != "error" {
			t.Fatalf("%s: %v, want an error", id, st)
		}
		return e.Code, e.Error
	}

	// Queued, and the worker went away.
	id := submit()
	s.mu.Lock()
	s.seen = time.Now().Add(-2 * shorthandSeen)
	s.mu.Unlock()
	if code, msg := errorOf(id); code != 503 || !strings.Contains(msg, "offline") {
		t.Fatalf("stranded: %d %q", code, msg)
	}

	// Taken, and never answered in time.
	id = submit()
	shorthandDo(t, "POST", srv.URL+"/next", []byte(`{"worker":"w1"}`))
	s.mu.Lock()
	s.reads[id].created = time.Now().Add(-shorthandWait - time.Second)
	s.mu.Unlock()
	if code, _ := errorOf(id); code != 504 {
		t.Fatalf("timed out: %d", code)
	}
	if code, _ := shorthandDo(t, "POST", srv.URL+"/result/"+id, []byte(`{"lines":[]}`)); code != http.StatusNotFound {
		t.Fatalf("a late result: HTTP %d, want 404", code)
	}

	// The worker's own error.
	id = submit()
	shorthandDo(t, "POST", srv.URL+"/next", []byte(`{"worker":"w2"}`))
	shorthandDo(t, "POST", srv.URL+"/result/"+id, []byte(`{"error":"the image can't be read","status":400}`))
	if code, msg := errorOf(id); code != 400 || msg != "the image can't be read" {
		t.Fatalf("refused: %d %q", code, msg)
	}
}
