package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the agent's machine may not have zoneinfo
)

// canvasClient talks to Canvas through the command center's relay, which
// attaches the user's Canvas session. Paths are Canvas paths such as
// "/api/v1/courses"; "courses" alone is shorthand for "/api/v1/courses".
type canvasClient struct {
	cfg     config
	http    *http.Client
	base    string         // Canvas origin, e.g. https://canvas.asu.edu
	loc     *time.Location // the user's Canvas time zone
	courses []any          // cached course list
}

func newCanvasClient() (*canvasClient, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return &canvasClient{cfg: cfg, http: &http.Client{Timeout: 10 * time.Minute}, loc: time.Local}, nil
}

// canvasCache holds facts about the user's Canvas that rarely change.
type canvasCache struct {
	Server   string `json:"server"`
	BaseURL  string `json:"base_url"`
	TimeZone string `json:"time_zone"`
}

func canvasCachePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "delta", "canvas.json")
}

// init loads the Canvas origin and the user's time zone, fetching them once
// and caching them next to the CLI config.
func (c *canvasClient) init() error {
	if c.base != "" {
		return nil
	}
	var cache canvasCache
	path := canvasCachePath()
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &cache)
	}
	if cache.Server != c.cfg.Server || cache.BaseURL == "" || cache.TimeZone == "" {
		var st map[string]any
		if err := c.ccJSON("/canvas-status", &st); err != nil {
			return err
		}
		cache = canvasCache{Server: c.cfg.Server, BaseURL: gs(st, "base_url")}
		c.base = cache.BaseURL
		if prof, err := c.getObj("users/self/profile", nil); err == nil {
			cache.TimeZone = gs(prof, "time_zone")
		}
		if path != "" && cache.TimeZone != "" {
			os.MkdirAll(filepath.Dir(path), 0o700)
			data, _ := json.MarshalIndent(cache, "", "  ")
			os.WriteFile(path, data, 0o600)
		}
	}
	c.base = cache.BaseURL
	tz := cache.TimeZone
	if v := os.Getenv("DELTA_TZ"); v != "" {
		tz = v
	}
	if loc, err := time.LoadLocation(tz); err == nil && tz != "" {
		c.loc = loc
	}
	return nil
}

func (c *canvasClient) ccJSON(path string, v any) error {
	body, err := do(c.cfg, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// apiPath expands shorthand paths and accepts full Canvas URLs.
func apiPath(p string) string {
	if u, err := url.Parse(p); err == nil && u.IsAbs() {
		return u.RequestURI()
	}
	if !strings.HasPrefix(p, "/") {
		p = "/api/v1/" + p
	}
	return p
}

// request sends a request through the relay. Non-2xx responses become
// errors that explain what Canvas said.
func (c *canvasClient) request(method, path string, query url.Values, contentType string, body io.Reader) (*http.Response, error) {
	p := apiPath(path)
	if len(query) > 0 {
		sep := "?"
		if strings.Contains(p, "?") {
			sep = "&"
		}
		p += sep + query.Encode()
	}
	req, err := http.NewRequest(method, c.cfg.Server+"/canvas"+p, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, canvasError(resp, method, p, data)
	}
	return resp, nil
}

// canvasError turns a failed relay response into an error the agent can act on.
func canvasError(resp *http.Response, method, path string, data []byte) error {
	fromCanvas := resp.Header.Get("X-Delta-Canvas") != ""
	var v any
	json.Unmarshal(stripWhile(data), &v)
	msg := canvasMessage(v)
	if msg == "" {
		msg = strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
	}
	if !fromCanvas {
		return fmt.Errorf("command center: %s (HTTP %d)", msg, resp.StatusCode)
	}
	hint := ""
	switch resp.StatusCode {
	case 401, 403:
		hint = "\n  The user's account is not allowed to do this. The course may hide this feature (for example the Files or Pages tab), the item may be locked, or a discussion may require posting before viewing replies."
	case 404:
		hint = "\n  Check the IDs; the item may be unpublished or in a different course."
	}
	return fmt.Errorf("Canvas: %s (HTTP %d on %s %s)%s", msg, resp.StatusCode, method, path, hint)
}

func canvasMessage(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	var parts []string
	switch e := m["errors"].(type) {
	case []any:
		for _, x := range e {
			if s := gs(x, "message"); s != "" {
				parts = append(parts, s)
			} else if s, ok := x.(string); ok {
				parts = append(parts, s)
			}
		}
	case map[string]any:
		keys := sortedKeys(e)
		for _, k := range keys {
			b, _ := json.Marshal(e[k])
			parts = append(parts, k+": "+string(b))
		}
	case string:
		parts = append(parts, e)
	}
	for _, k := range []string{"message", "error", "status"} {
		if s := gs(m, k); s != "" && len(parts) == 0 {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "; ")
}

func stripWhile(b []byte) []byte {
	return bytes.TrimPrefix(bytes.TrimSpace(b), []byte("while(1);"))
}

func decodeJSON(r io.Reader) (any, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	data = stripWhile(data)
	if len(data) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // Canvas IDs can exceed float64 precision
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("unexpected response from Canvas: %w", err)
	}
	return v, nil
}

func (c *canvasClient) call(method, path string, query url.Values, contentType string, body io.Reader) (any, error) {
	resp, err := c.request(method, path, query, contentType, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return decodeJSON(resp.Body)
}

func (c *canvasClient) get(path string, query url.Values) (any, error) {
	return c.call(http.MethodGet, path, query, "", nil)
}

func (c *canvasClient) getObj(path string, query url.Values) (map[string]any, error) {
	v, err := c.get(path, query)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected response from Canvas for %s", path)
	}
	return m, nil
}

// send makes a form-encoded write request (POST, PUT or DELETE).
func (c *canvasClient) send(method, path string, form url.Values) (any, error) {
	return c.call(method, path, nil, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
}

var linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

// getAll fetches every page of a list endpoint.
func (c *canvasClient) getAll(path string, query url.Values) ([]any, error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	if q.Get("per_page") == "" {
		q.Set("per_page", "100")
	}
	var all []any
	next, nextQuery := path, q
	for page := 0; next != ""; page++ {
		if page == 100 {
			return all, errors.New("stopped after 100 pages; narrow the request")
		}
		resp, err := c.request(http.MethodGet, next, nextQuery, "", nil)
		if err != nil {
			return all, err
		}
		v, err := decodeJSON(resp.Body)
		resp.Body.Close()
		if err != nil {
			return all, err
		}
		switch x := v.(type) {
		case []any:
			all = append(all, x...)
		case map[string]any: // a few endpoints wrap their list
			for _, k := range sortedKeys(x) {
				if l, ok := x[k].([]any); ok {
					all = append(all, l...)
				}
			}
		}
		next, nextQuery = "", nil
		if m := linkNext.FindStringSubmatch(strings.Join(resp.Header.Values("Link"), ",")); m != nil {
			next = apiPath(m[1])
		}
	}
	return all, nil
}

// upload sends a local file through the command center's Canvas upload
// endpoint. endpoint is the Canvas preflight path; it returns the file object.
func (c *canvasClient) upload(endpoint, localPath string, extra url.Values) (map[string]any, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", localPath)
	}
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	if q.Get("name") == "" {
		q.Set("name", filepath.Base(localPath))
	}
	q.Set("content_type", contentTypeFor(localPath))
	req, err := http.NewRequest(http.MethodPost, c.cfg.Server+"/canvas-upload"+apiPath(endpoint)+"?"+q.Encode(), f)
	if err != nil {
		return nil, err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("uploading %s: %w", filepath.Base(localPath), canvasError(resp, "POST", endpoint, data))
	}
	v, err := decodeJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	if gs(m, "id") == "" {
		return nil, fmt.Errorf("uploading %s: no file id returned", localPath)
	}
	return m, nil
}

var extraTypes = map[string]string{
	".doc": "application/msword", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls": "application/vnd.ms-excel", ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt": "application/vnd.ms-powerpoint", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt": "application/vnd.oasis.opendocument.text", ".ods": "application/vnd.oasis.opendocument.spreadsheet",
	".odp": "application/vnd.oasis.opendocument.presentation", ".rtf": "application/rtf",
	".txt": "text/plain", ".md": "text/markdown", ".csv": "text/csv", ".pdf": "application/pdf",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif", ".heic": "image/heic", ".webp": "image/webp",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".wav": "audio/wav", ".mp4": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm",
	".zip": "application/zip", ".py": "text/x-python", ".java": "text/x-java", ".c": "text/x-c", ".cpp": "text/x-c++", ".html": "text/html",
}

func contentTypeFor(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if t, ok := extraTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// ---- course and context resolution ----

func (c *canvasClient) courseList() ([]any, error) {
	if c.courses != nil {
		return c.courses, nil
	}
	list, err := c.getAll("courses", url.Values{"include[]": {"term", "concluded", "total_scores", "teachers"}})
	if err != nil {
		return nil, err
	}
	c.courses = list
	return list, nil
}

// courseCurrent reports whether a course is still running. Many schools
// (ASU included) leave course and term dates empty, so besides Canvas's own
// "concluded" flag this compares the semester named in the term or course
// code ("2026 Fall C", "ORG-2024Spring") with today's semester.
func courseCurrent(course any) bool {
	if gb(course, "concluded") || gb(course, "access_restricted_by_date") {
		return false
	}
	if t, err := time.Parse(time.RFC3339, gs(course, "term", "end_at")); err == nil && t.Before(time.Now()) {
		return false
	}
	for _, s := range []string{gs(course, "term", "name"), gs(course, "course_code"), gs(course, "name")} {
		if key, ok := semesterKey(s); ok {
			return key >= semesterOf(time.Now())
		}
	}
	return true
}

var semesterPattern = regexp.MustCompile(`(?i)(20\d\d)\s*-?\s*(spring|summer|fall|winter)|(spring|summer|fall|winter)\s*-?\s*(20\d\d)`)

var seasonRank = map[string]int{"spring": 1, "summer": 2, "fall": 3, "winter": 4}

// semesterKey turns "2026 Fall C" into a sortable number like 20263.
func semesterKey(s string) (int, bool) {
	m := semesterPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	year, season := firstNonEmpty(m[1], m[4]), strings.ToLower(firstNonEmpty(m[2], m[3]))
	y, _ := strconv.Atoi(year)
	return y*10 + seasonRank[season], true
}

// semesterOf approximates which semester a date falls in.
func semesterOf(t time.Time) int {
	switch day := t.YearDay(); {
	case day < 130: // through early May
		return t.Year()*10 + 1
	case day < 222: // through early August
		return t.Year()*10 + 2
	case day < 349: // through mid December
		return t.Year()*10 + 3
	default:
		return t.Year()*10 + 4
	}
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func normalize(s string) string { return nonAlnum.ReplaceAllString(strings.ToLower(s), "") }

// resolveCourse accepts a course ID, a Canvas course URL, or part of a
// course's code or name ("eng105", "THF 101") and returns the course ID.
func (c *canvasClient) resolveCourse(arg string) (string, error) {
	if m := regexp.MustCompile(`/courses/(\d+)`).FindStringSubmatch(arg); m != nil {
		return m[1], nil
	}
	if isID(arg) {
		return arg, nil
	}
	courses, err := c.courseList()
	if err != nil {
		return "", err
	}
	want := normalize(arg)
	if want == "" {
		return "", fmt.Errorf("invalid course %q", arg)
	}
	var current, other []any
	for _, co := range courses {
		if strings.Contains(normalize(gs(co, "course_code")), want) || strings.Contains(normalize(gs(co, "name")), want) {
			if courseCurrent(co) {
				current = append(current, co)
			} else {
				other = append(other, co)
			}
		}
	}
	matches := current
	if len(matches) == 0 {
		matches = other
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no course matches %q (run `delta canvas courses --all`)", arg)
	case 1:
		return gs(matches[0], "id"), nil
	}
	var names []string
	for _, m := range matches {
		names = append(names, fmt.Sprintf("%s (%s)", gs(m, "id"), gs(m, "name")))
	}
	return "", fmt.Errorf("%q matches several courses; use an ID: %s", arg, strings.Join(names, ", "))
}

// resolveContext accepts a course (as for resolveCourse) or "group:<id>" and
// returns the API path prefix, e.g. "courses/123" or "groups/45".
func (c *canvasClient) resolveContext(arg string) (string, error) {
	if m := regexp.MustCompile(`^(?:group:|g)(\d+)$|/groups/(\d+)`).FindStringSubmatch(arg); m != nil {
		return "groups/" + m[1] + m[2], nil
	}
	id, err := c.resolveCourse(arg)
	if err != nil {
		return "", err
	}
	return "courses/" + id, nil
}

func (c *canvasClient) courseName(id string) string {
	if courses, err := c.courseList(); err == nil {
		for _, co := range courses {
			if gs(co, "id") == id {
				return gs(co, "name")
			}
		}
	}
	return "course " + id
}

var idPattern = regexp.MustCompile(`^\d+$`)

func isID(s string) bool { return idPattern.MatchString(s) }

func needID(what, s string) (string, error) {
	// Accept URLs ending in the ID, like .../assignments/123.
	if m := regexp.MustCompile(`/(\d+)/?(?:\?.*)?$`).FindStringSubmatch(s); m != nil {
		return m[1], nil
	}
	if !isID(s) {
		return "", fmt.Errorf("%s must be a numeric ID, got %q", what, s)
	}
	return s, nil
}

// ---- generic JSON access ----

// walk follows keys through nested objects.
func walk(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// gs returns a value as a string ("" for null or missing).
func gs(v any, keys ...string) string {
	switch x := walk(v, keys...).(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func gb(v any, keys ...string) bool {
	b, _ := walk(v, keys...).(bool)
	return b
}

func gl(v any, keys ...string) []any {
	l, _ := walk(v, keys...).([]any)
	return l
}

func gf(v any, keys ...string) (float64, bool) {
	switch x := walk(v, keys...).(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// ---- argument parsing ----

// opts holds parsed command-line arguments. Flags may appear anywhere and
// be written --name value, --name=value, or -name value.
type opts struct {
	pos   []string
	vals  map[string][]string
	bools map[string]bool
}

func parseOpts(args []string, valueFlags, boolFlags string) (*opts, error) {
	vf := map[string]bool{}
	for _, f := range strings.Fields(valueFlags) {
		vf[f] = true
	}
	bf := map[string]bool{"json": true}
	for _, f := range strings.Fields(boolFlags) {
		bf[f] = true
	}
	o := &opts{vals: map[string][]string{}, bools: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			o.pos = append(o.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" || isNumber(a) {
			o.pos = append(o.pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch {
		case bf[name] && !hasVal:
			o.bools[name] = true
		case vf[name]:
			if !hasVal {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--%s needs a value", name)
				}
				i++
				val = args[i]
			}
			o.vals[name] = append(o.vals[name], val)
		default:
			return nil, fmt.Errorf("unknown option %s", a)
		}
	}
	return o, nil
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func (o *opts) str(name string) string {
	if v := o.vals[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func (o *opts) int(name string, def int) (int, error) {
	s := o.str(name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("--%s must be a whole number", name)
	}
	return n, nil
}

// need checks the positional argument count.
func (o *opts) need(min, max int, usage string) error {
	if len(o.pos) < min || (max >= 0 && len(o.pos) > max) {
		return errors.New("usage: delta canvas " + usage)
	}
	return nil
}

// ---- time formatting ----

func (c *canvasClient) parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.In(c.loc), true
}

// fmtTime renders a Canvas timestamp in the user's time zone with a relative
// hint, e.g. "Tue Oct 14 11:59pm (in 3d 2h)".
func (c *canvasClient) fmtTime(s string) string {
	t, ok := c.parseTime(s)
	if !ok {
		return "-"
	}
	now := time.Now()
	layout := "Mon Jan 2 3:04pm"
	if t.Year() != now.Year() {
		layout = "Mon Jan 2 2006 3:04pm"
	}
	return t.Format(layout) + " (" + relTime(t.Sub(now)) + ")"
}

func (c *canvasClient) fmtDate(s string) string {
	t, ok := c.parseTime(s)
	if !ok {
		return "-"
	}
	if t.Year() != time.Now().Year() {
		return t.Format("Jan 2 2006 3:04pm")
	}
	return t.Format("Jan 2 3:04pm")
}

func relTime(d time.Duration) string {
	future := d >= 0
	if !future {
		d = -d
	}
	var s string
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		s = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		h := int(d.Hours())
		s = fmt.Sprintf("%dh", h)
		if m := int(d.Minutes()) % 60; h < 10 && m > 0 {
			s += fmt.Sprintf(" %dm", m)
		}
	default:
		days := int(d.Hours()) / 24
		s = fmt.Sprintf("%dd", days)
		if h := int(d.Hours()) % 24; days < 7 && h > 0 {
			s += fmt.Sprintf(" %dh", h)
		}
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}
