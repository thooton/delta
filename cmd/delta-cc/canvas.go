package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Canvas relay. The command center runs on the same machine as the
// canvas-cookie service (see the canvas-cookie README), which keeps a
// logged-in Canvas browser session alive and hands out its Cookie header to
// local clients holding its API token. The relay attaches that cookie (and the
// matching CSRF token) to the requests Canvas on the phone makes, so it gets
// full student access to Canvas without ever seeing the session.
//
// Routes (all behind the delta bearer token):
//
//	GET  /canvas-status          Canvas base URL and cookie-service health.
//	ANY  /canvas/{path...}       Relayed to <canvas-url>/{path} with the session.
//	POST /canvas-upload/{path...} Canvas's three-step file upload; the request
//	                             body is the file, {path} is the preflight
//	                             endpoint, and the query holds preflight params.
const (
	canvasCookieTTL = 5 * time.Minute
	canvasUserAgent = "delta-cc"
	// Request bodies are buffered so a request can be retried after the
	// session cookie is refreshed. Large files go through /canvas-upload.
	canvasMaxRelayBody = 64 << 20
)

type canvasProxy struct {
	base      *url.URL
	cookieURL string // cookie service root, e.g. http://127.0.0.1:4782
	tokenFile string
	maxUpload int64

	client   *http.Client // follows redirects, cookie only on the Canvas host
	noFollow *http.Client // for the upload step, where redirects mean "confirm"
	svc      *http.Client

	mu      sync.Mutex
	cookie  string
	fetched time.Time
}

// newCanvasProxy returns nil when baseURL is empty, which disables the relay.
func newCanvasProxy(baseURL, cookieURL, tokenFile string, maxUpload int64) (*canvasProxy, error) {
	if baseURL == "" {
		return nil, nil
	}
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.Path != "" {
		return nil, fmt.Errorf("invalid Canvas URL %q (expected https://host)", baseURL)
	}
	p := &canvasProxy{
		base:      base,
		cookieURL: strings.TrimRight(cookieURL, "/"),
		tokenFile: tokenFile,
		maxUpload: maxUpload,
		svc:       &http.Client{Timeout: 15 * time.Second},
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 2 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   8,
	}
	p.client = &http.Client{Transport: transport, CheckRedirect: p.checkRedirect}
	p.noFollow = &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return p, nil
}

// checkRedirect keeps the session on the Canvas host only (file downloads
// redirect to signed storage URLs elsewhere) and stops at the login page,
// which means the session is no longer valid.
func (p *canvasProxy) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if req.URL.Host != p.base.Host {
		req.Header.Del("Cookie")
		req.Header.Del("X-CSRF-Token")
		return nil
	}
	if req.URL.Path == "/login" || strings.HasPrefix(req.URL.Path, "/login/") {
		return http.ErrUseLastResponse
	}
	return nil
}

func (p *canvasProxy) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /canvas-status", p.status)
	mux.HandleFunc("/canvas/{path...}", p.relay)
	mux.HandleFunc("POST /canvas-upload/{path...}", p.upload)
}

// registerDisabled answers every Canvas route with a clear error.
func registerCanvasDisabled(mux *http.ServeMux) {
	off := func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "Canvas is not configured on this command center (start delta-cc with -canvas-url)")
	}
	mux.HandleFunc("GET /canvas-status", off)
	mux.HandleFunc("/canvas/{path...}", off)
	mux.HandleFunc("POST /canvas-upload/{path...}", off)
}

// getCookie returns the cached session cookie, fetching a fresh one from the
// cookie service when it is stale or when force is set (after a Canvas 401).
func (p *canvasProxy) getCookie(ctx context.Context, force bool) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !force && p.cookie != "" && time.Since(p.fetched) < canvasCookieTTL {
		return p.cookie, nil
	}
	p.cookie = ""
	var res struct {
		Cookie string `json:"cookie"`
		Error  string `json:"error"`
	}
	status, err := p.svcGet(ctx, "/cookie", &res)
	if err != nil {
		return "", err
	}
	switch {
	case status == http.StatusOK && res.Cookie != "":
		p.cookie, p.fetched = res.Cookie, time.Now()
		return p.cookie, nil
	case status == http.StatusServiceUnavailable:
		return "", fmt.Errorf("the Canvas session is not available (cookie service says %q); the user may need to log in to Canvas again and reseed the canvas-cookie service", res.Error)
	case status == http.StatusUnauthorized:
		return "", fmt.Errorf("the canvas-cookie service rejected the API token in %s", p.tokenFile)
	default:
		return "", fmt.Errorf("canvas-cookie service returned HTTP %d", status)
	}
}

func (p *canvasProxy) svcGet(ctx context.Context, path string, v any) (int, error) {
	raw, err := os.ReadFile(p.tokenFile)
	if err != nil {
		return 0, fmt.Errorf("cannot read the canvas-cookie API token: %w (the delta user needs read access, e.g. SupplementaryGroups=canvas-cookie)", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cookieURL+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(raw)))
	resp, err := p.svc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("canvas-cookie service unreachable at %s: %w", p.cookieURL, err)
	}
	defer resp.Body.Close()
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
	return resp.StatusCode, nil
}

// csrfToken extracts Canvas's _csrf_token cookie, which must be echoed in the
// X-CSRF-Token header on any non-GET request made with session auth.
func csrfToken(cookie string) string {
	for _, pair := range strings.Split(cookie, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && name == "_csrf_token" {
			if v, err := url.QueryUnescape(value); err == nil {
				return v
			}
			return value
		}
	}
	return ""
}

// do sends one request to Canvas with the session attached. If Canvas says the
// session is unauthenticated, it fetches a new cookie and tries once more.
// The caller must close the returned body.
func (p *canvasProxy) do(ctx context.Context, client *http.Client, method, target string, header http.Header, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		cookie, err := p.getCookie(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, vs := range header {
			req.Header[k] = vs
		}
		req.Header.Set("User-Agent", canvasUserAgent)
		if req.URL.Host == p.base.Host {
			req.Header.Set("Cookie", cookie)
			if method != http.MethodGet && method != http.MethodHead {
				req.Header.Set("X-CSRF-Token", csrfToken(cookie))
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if attempt > 0 || !p.sessionExpired(resp) {
			return resp, nil
		}
		resp.Body.Close()
		log.Printf("canvas: session rejected; refetching cookie")
	}
}

// sessionExpired reports whether resp means "log in again" rather than "you
// may not see this" (Canvas uses 401 for both). It may consume a small 401
// body, which it restores.
func (p *canvasProxy) sessionExpired(resp *http.Response) bool {
	if resp.StatusCode/100 == 3 {
		loc, err := resp.Location()
		return err == nil && loc.Host == p.base.Host && (loc.Path == "/login" || strings.HasPrefix(loc.Path, "/login/"))
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return false
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return !bytes.Contains(data, []byte(`"unauthorized"`))
}

// origin is the Canvas origin, as scheme://host.
func (p *canvasProxy) origin() string {
	return p.base.Scheme + "://" + p.base.Host
}

// target maps a relay request path onto the Canvas origin, keeping the
// original escaping of the path.
func (p *canvasProxy) target(r *http.Request, prefix string) string {
	t := p.origin() + strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	if r.URL.RawQuery != "" {
		t += "?" + r.URL.RawQuery
	}
	return t
}

func (p *canvasProxy) relay(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, canvasMaxRelayBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("relay bodies are limited to %d bytes; upload files with /canvas-upload", canvasMaxRelayBody))
		return
	}
	header := http.Header{}
	copyListed(header, r.Header, "Accept", "Content-Type", "Range", "If-None-Match", "If-Modified-Since")
	if header.Get("Accept") == "" {
		header.Set("Accept", "application/json")
	}
	resp, err := p.do(r.Context(), p.client, r.Method, p.target(r, "/canvas"), header, body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "canvas: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if p.sessionExpired(resp) {
		writeError(w, http.StatusBadGateway, "canvas: the Canvas session was rejected even after refreshing the cookie; the user may need to log in to Canvas again and reseed the canvas-cookie service")
		return
	}
	copyListed(w.Header(), resp.Header, "Content-Type", "Content-Length", "Content-Disposition", "Content-Range", "Link", "Location", "ETag", "Last-Modified", "X-Rate-Limit-Remaining")
	w.Header().Set("X-Delta-Canvas", "1")
	w.Header().Set("X-Canvas-Url", resp.Request.URL.String())
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// copyListed copies the headers named from src to dst.
func copyListed(dst, src http.Header, names ...string) {
	for _, k := range names {
		if v := src.Values(k); len(v) > 0 {
			dst[k] = v
		}
	}
}

func (p *canvasProxy) status(w http.ResponseWriter, r *http.Request) {
	var health map[string]any
	out := map[string]any{"base_url": p.base.String()}
	status, err := p.svcGet(r.Context(), "/health", &health)
	if err != nil {
		out["ready"] = false
		out["error"] = err.Error()
	} else {
		out["ready"] = status == http.StatusOK
		out["cookie_service"] = health
	}
	writeJSON(w, http.StatusOK, out)
}

// upload performs Canvas's file upload protocol: a preflight POST to {path}
// (for example /api/v1/courses/1/assignments/2/submissions/self/files), a
// multipart POST of the file to the returned upload_url, and a confirmation
// GET when the storage server asks for one. It responds with the Canvas file
// object, whose "id" can then be attached to a submission, comment or message.
func (p *canvasProxy) upload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	size := r.ContentLength
	switch {
	case name == "":
		writeError(w, http.StatusBadRequest, "missing ?name= (the file name)")
		return
	case size < 0:
		writeError(w, http.StatusLengthRequired, "upload needs a Content-Length")
		return
	case size > p.maxUpload:
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds %d bytes", p.maxUpload))
		return
	}
	contentType := q.Get("content_type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	pre, ok := p.preflight(w, r, size, contentType)
	if !ok {
		return
	}
	resp, data, ok := p.store(w, r, pre, name, contentType, size)
	if !ok {
		return
	}
	if file, ok := p.confirm(r.Context(), w, resp, data); ok {
		writeJSON(w, http.StatusOK, file)
	}
}

// canvasPreflight is where Canvas says to send a file.
type canvasPreflight struct {
	UploadURL    string         `json:"upload_url"`
	UploadParams map[string]any `json:"upload_params"`
	FileParam    string         `json:"file_param"`
}

// preflight is the upload's first step. Every query parameter is passed on
// (name, content_type, parent_folder_path, on_duplicate, ...), plus the size.
func (p *canvasProxy) preflight(w http.ResponseWriter, r *http.Request, size int64, contentType string) (pre canvasPreflight, ok bool) {
	form := url.Values{}
	for k, vs := range r.URL.Query() {
		form[k] = vs
	}
	form.Set("size", strconv.FormatInt(size, 10))
	form.Set("content_type", contentType)
	resp, err := p.do(r.Context(), p.client, http.MethodPost, p.origin()+strings.TrimPrefix(r.URL.EscapedPath(), "/canvas-upload"),
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json"}},
		[]byte(form.Encode()))
	if err != nil {
		writeError(w, http.StatusBadGateway, "canvas upload preflight: "+err.Error())
		return pre, false
	}
	data := readSmall(resp)
	if resp.StatusCode/100 != 2 {
		relayCanvasError(w, resp.StatusCode, "upload preflight", data)
		return pre, false
	}
	if err := json.Unmarshal(stripWhile(data), &pre); err != nil || pre.UploadURL == "" {
		writeError(w, http.StatusBadGateway, "canvas upload preflight: unexpected response: "+snippet(data))
		return pre, false
	}
	if pre.FileParam == "" {
		pre.FileParam = "file"
	}
	return pre, true
}

// store is the upload's second step: a multipart POST of the request's body
// to the storage server. The length is computed up front because storage
// backends reject chunked uploads. It returns the storage server's answer.
func (p *canvasProxy) store(w http.ResponseWriter, r *http.Request, pre canvasPreflight, name, contentType string, size int64) (*http.Response, []byte, bool) {
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	for _, k := range slices.Sorted(maps.Keys(pre.UploadParams)) {
		v := pre.UploadParams[k]
		s, ok := v.(string)
		if !ok {
			b, _ := json.Marshal(v)
			s = string(b)
		}
		mw.WriteField(k, s)
	}
	part := textproto.MIMEHeader{}
	part.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, quoteField(pre.FileParam), quoteField(name)))
	part.Set("Content-Type", contentType)
	mw.CreatePart(part)
	tail := "\r\n--" + mw.Boundary() + "--\r\n"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, pre.UploadURL,
		io.MultiReader(&head, io.LimitReader(r.Body, size), strings.NewReader(tail)))
	if err != nil {
		writeError(w, http.StatusBadGateway, "canvas upload: bad upload_url: "+err.Error())
		return nil, nil, false
	}
	req.ContentLength = int64(head.Len()) + size + int64(len(tail))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", canvasUserAgent)
	if req.URL.Host == p.base.Host { // local Canvas storage needs the session
		cookie, err := p.getCookie(r.Context(), false)
		if err != nil {
			writeError(w, http.StatusBadGateway, "canvas upload: "+err.Error())
			return nil, nil, false
		}
		req.Header.Set("Cookie", cookie)
		req.Header.Set("X-CSRF-Token", csrfToken(cookie))
	}
	resp, err := p.noFollow.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "canvas upload: "+err.Error())
		return nil, nil, false
	}
	return resp, readSmall(resp), true
}

// confirm is the upload's last step. A redirect must be followed with the
// session to mark the file available; a 2xx usually carries the file object
// already. It returns the file object.
func (p *canvasProxy) confirm(ctx context.Context, w http.ResponseWriter, resp *http.Response, data []byte) (map[string]any, bool) {
	confirm := ""
	var file map[string]any
	switch {
	case resp.StatusCode/100 == 3:
		confirm = resp.Header.Get("Location")
	case resp.StatusCode/100 == 2:
		json.Unmarshal(stripWhile(data), &file)
		if file["id"] == nil {
			if confirm = resp.Header.Get("Location"); confirm == "" {
				confirm, _ = file["location"].(string)
			}
		}
	default:
		writeError(w, http.StatusBadGateway, fmt.Sprintf("canvas upload: storage server returned HTTP %d: %s", resp.StatusCode, snippet(data)))
		return nil, false
	}
	if confirm != "" {
		loc, err := resp.Request.URL.Parse(confirm)
		if err != nil || loc.Host != p.base.Host {
			writeError(w, http.StatusBadGateway, "canvas upload: unexpected confirmation URL "+confirm)
			return nil, false
		}
		cresp, err := p.do(ctx, p.client, http.MethodGet, loc.String(), http.Header{"Accept": {"application/json"}}, nil)
		if err != nil {
			writeError(w, http.StatusBadGateway, "canvas upload confirmation: "+err.Error())
			return nil, false
		}
		data = readSmall(cresp)
		if cresp.StatusCode/100 != 2 {
			relayCanvasError(w, cresp.StatusCode, "upload confirmation", data)
			return nil, false
		}
		file = nil
		json.Unmarshal(stripWhile(data), &file)
	}
	if file["id"] == nil {
		writeError(w, http.StatusBadGateway, "canvas upload: no file id in response: "+snippet(data))
		return nil, false
	}
	return file, true
}

// relayCanvasError passes a Canvas error through, marked as coming from
// Canvas so the client can explain it.
func relayCanvasError(w http.ResponseWriter, status int, step string, data []byte) {
	w.Header().Set("X-Delta-Canvas", "1")
	if json.Valid(stripWhile(data)) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(stripWhile(data))
		return
	}
	writeError(w, status, fmt.Sprintf("canvas %s failed: %s", step, snippet(data)))
}

// stripWhile removes the "while(1);" guard Canvas may prefix JSON with.
func stripWhile(b []byte) []byte {
	return bytes.TrimPrefix(bytes.TrimSpace(b), []byte("while(1);"))
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	return s
}

func quoteField(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "", "\n", "").Replace(s)
}
