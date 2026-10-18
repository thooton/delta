package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// canvasSelfTest runs the Canvas relay against a fake cookie service, a fake
// Canvas, and a fake storage host (standing in for Canvas's file store on
// another domain). It checks that the session reaches Canvas and nothing
// else, that writes carry the CSRF token, that an expired session is
// refreshed, and that the upload protocol works end to end.
func canvasSelfTest(dir string) error {
	const svcToken = "svc-selftest-token"
	tokenFile := filepath.Join(dir, "cookie-token")
	if err := os.WriteFile(tokenFile, []byte(svcToken+"\n"), 0o600); err != nil {
		return err
	}

	// The cookie service hands out a stale session first, then a good one.
	var fetches atomic.Int32
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+svcToken {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		switch r.URL.Path {
		case "/health":
			writeJSON(w, 200, map[string]any{"ready": true, "error": nil})
		case "/cookie":
			session := "stale"
			if fetches.Add(1) > 1 {
				session = "good"
			}
			writeJSON(w, 200, map[string]any{"cookie": "_csrf_token=abc%2Bdef%3D%3D; canvas_session=" + session})
		default:
			writeJSON(w, 404, map[string]string{"error": "not_found"})
		}
	}))
	defer svc.Close()

	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	// Storage lives on a different host; it must never see the session.
	var canvasURL string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" {
			fail("session leaked to storage host on %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/blob":
			io.WriteString(w, "attachment bytes")
		case "/upload":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				fail("upload: bad multipart: %v", err)
				return
			}
			f, h, err := r.FormFile("file")
			if err != nil {
				fail("upload: no file part: %v", err)
				return
			}
			content, _ := io.ReadAll(f)
			if string(content) != "my essay" || h.Filename != "essay.pdf" || r.FormValue("key") != "k1" {
				fail("upload: got file %q named %q key %q", content, h.Filename, r.FormValue("key"))
			}
			if r.FormValue("mode") == "redirect" {
				http.Redirect(w, r, canvasURL+"/api/v1/files/99/create_success?uuid=u", http.StatusFound)
				return
			}
			writeJSON(w, 201, map[string]any{"id": 98, "display_name": "essay.pdf"})
		}
	}))
	defer storage.Close()

	canvas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		if !strings.Contains(cookie, "canvas_session=good") {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, 401, map[string]any{"status": "unauthenticated", "errors": []any{map[string]string{"message": "user authorization required"}}})
			} else {
				http.Redirect(w, r, "/login/saml", http.StatusFound)
			}
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-CSRF-Token") != "abc+def==" {
			writeJSON(w, 422, map[string]any{"errors": []any{map[string]string{"message": "Invalid CSRF token"}}})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "canvas_session", Value: "rotated"})
		switch {
		case r.URL.Path == "/api/v1/users/self":
			w.Header().Set("Link", `<`+canvasURL+`/api/v1/users/self?page=2>; rel="next"`)
			writeJSON(w, 200, map[string]any{"id": 1, "name": "Student"})
		case r.URL.Path == "/api/v1/courses/1/files/hidden":
			writeJSON(w, 401, map[string]any{"status": "unauthorized", "errors": []any{map[string]string{"message": "user not authorized to perform that action"}}})
		case r.URL.Path == "/api/v1/echo":
			body, _ := io.ReadAll(r.Body)
			writeJSON(w, 200, map[string]string{"method": r.Method, "body": string(body), "query": r.URL.RawQuery})
		case r.URL.Path == "/files/5/download":
			http.Redirect(w, r, "/files/5/download2", http.StatusFound)
		case r.URL.Path == "/files/5/download2":
			http.Redirect(w, r, storage.URL+"/blob", http.StatusFound)
		case r.URL.Path == "/api/v1/courses/1/assignments/2/submissions/self/files":
			r.ParseForm()
			if r.Form.Get("name") != "essay.pdf" || r.Form.Get("size") != "8" || r.Form.Get("content_type") != "application/pdf" {
				fail("preflight: got %v", r.Form)
			}
			writeJSON(w, 200, map[string]any{
				"upload_url":    storage.URL + "/upload",
				"upload_params": map[string]any{"key": "k1", "mode": r.Form.Get("mode")},
			})
		case r.URL.Path == "/api/v1/files/99/create_success":
			writeJSON(w, 200, map[string]any{"id": 99, "display_name": "essay.pdf"})
		default:
			writeJSON(w, 404, map[string]any{"errors": []any{map[string]string{"message": "The specified resource does not exist."}}})
		}
	}))
	defer canvas.Close()
	canvasURL = canvas.URL

	proxy, err := newCanvasProxy(canvas.URL, svc.URL, tokenFile, 1<<20)
	if err != nil {
		return err
	}
	const token = "delta_selftest"
	hash := sha256.Sum256([]byte(token))
	agents, err := newTestAgents(dir)
	if err != nil {
		return err
	}
	public, _ := newHandler(hash[:], filepath.Join(dir, "files"), 1<<20, &updater{restart: make(chan struct{}, 1)}, proxy, nil, agents)
	ts := httptest.NewServer(public)
	defer ts.Close()

	call := func(desc string, want int, method, path string, body []byte) (*http.Response, []byte, error) {
		req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if method == http.MethodPost && !strings.HasPrefix(path, "/canvas-upload") {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", desc, err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			return nil, nil, fmt.Errorf("%s: got HTTP %d, want %d (%s)", desc, resp.StatusCode, want, strings.TrimSpace(string(data)))
		}
		return resp, data, nil
	}

	// The first cookie is stale: the relay must refetch and retry.
	resp, data, err := call("canvas GET with session refresh", 200, "GET", "/canvas/api/v1/users/self", nil)
	if err != nil {
		return err
	}
	if fetches.Load() != 2 {
		return fmt.Errorf("canvas: expected 2 cookie fetches after a 401, got %d", fetches.Load())
	}
	if !bytes.Contains(data, []byte(`"Student"`)) || resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Delta-Canvas") != "1" {
		return fmt.Errorf("canvas GET: unexpected relay response %s (headers %v)", data, resp.Header)
	}
	if !strings.Contains(resp.Header.Get("Link"), "page=2") {
		return fmt.Errorf("canvas GET: Link header not relayed")
	}
	if _, _, err := call("canvas 401 unauthorized is passed through", 401, "GET", "/canvas/api/v1/courses/1/files/hidden", nil); err != nil {
		return err
	}
	if fetches.Load() != 2 {
		return fmt.Errorf("canvas: 'unauthorized' (not unauthenticated) should not refetch the cookie")
	}
	_, data, err = call("canvas POST carries CSRF token", 200, "POST", "/canvas/api/v1/echo?a=1", []byte("x=y"))
	if err != nil {
		return err
	}
	var echo map[string]string
	json.Unmarshal(data, &echo)
	if echo["method"] != "POST" || echo["body"] != "x=y" || echo["query"] != "a=1" {
		return fmt.Errorf("canvas POST: unexpected echo %s", data)
	}
	if _, _, err := call("canvas 404 passes through", 404, "GET", "/canvas/api/v1/nope", nil); err != nil {
		return err
	}
	_, data, err = call("canvas file download through redirects", 200, "GET", "/canvas/files/5/download", nil)
	if err != nil {
		return err
	}
	if string(data) != "attachment bytes" {
		return fmt.Errorf("canvas download: got %q", data)
	}
	_, data, err = call("canvas upload (storage returns file)", 200, "POST",
		"/canvas-upload/api/v1/courses/1/assignments/2/submissions/self/files?name=essay.pdf&content_type=application/pdf", []byte("my essay"))
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte(`"id":98`)) {
		return fmt.Errorf("canvas upload: unexpected result %s", data)
	}
	_, data, err = call("canvas upload (storage redirects to confirm)", 200, "POST",
		"/canvas-upload/api/v1/courses/1/assignments/2/submissions/self/files?name=essay.pdf&content_type=application/pdf&mode=redirect", []byte("my essay"))
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte(`"id":99`)) {
		return fmt.Errorf("canvas upload confirm: unexpected result %s", data)
	}
	if _, _, err := call("canvas upload without a name", 400, "POST", "/canvas-upload/api/v1/x", []byte("x")); err != nil {
		return err
	}
	_, data, err = call("canvas status", 200, "GET", "/canvas-status", nil)
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte(`"ready":true`)) {
		return fmt.Errorf("canvas status: %s", data)
	}

	// A session that keeps failing must be reported, not looped on or
	// relayed as the login page.
	fetches.Store(-100)
	proxy.mu.Lock()
	proxy.cookie = ""
	proxy.mu.Unlock()
	if _, _, err := call("canvas login redirect is reported", 502, "GET", "/canvas/files/5/download", nil); err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("canvas: %s", strings.Join(problems, "; "))
	}
	return nil
}
