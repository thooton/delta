package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// POST /music downloads music from the Soulseek network, and the command
// center does the downloading: neither the phone nor an agent's sandbox can
// reach the network. The work is done by an engine bundled into this binary
// (an unmodified sockseek release, see scripts/fetch-music-engine.sh), which
// the client never runs or sees directly: we run it and, for a download by
// name, pass on its log with its own name taken out.
//
// A run by name searches, downloads the best match into a job directory
// under data/music-jobs/, and if nothing was downloaded tries once more with
// a broader search. Its response is a stream of JSON lines (musicEvent): log
// lines for stdout and stderr, an empty object now and then to keep proxies
// from timing out, and finally the result, naming the job and its files.
// The client then fetches the files as a tar and deletes the job.
//
// The command center has one Soulseek account, kept in data/music-login.json.
// It is made up on first use and kept, because registering a new account on
// every run gets the machine throttled by the Soulseek server; only
// POST /music-login makes up a new one. One account can only be logged in
// once, so runs take turns.
//
// The phone's Source app lets the user choose instead, and sees everything:
// POST /music-search streams the same kind of run, ending with every result
// that meets the config's conditions, ranked by its preferences, and POST
// /music with a result's link downloads that one. Neither is white-labeled,
// and neither is open to backup sessions.
//
// Routes (all behind the delta bearer token):
//
//	POST   /music             {"mode": "song"|"album", "query": "..."}, or
//	                          {"link": "slsk://..."}; streams the run.
//	POST   /music-search      {"mode": "song"|"album", "query": "..."}; streams
//	                          the search, ending with {"done": {"error", "results"}}.
//	GET    /music-jobs/{id}   The job's files, as a tar.
//	DELETE /music-jobs/{id}   Remove the job.
//	POST   /music-login       Replace the account with a new one.

//go:embed music-engine
var musicEngineFS embed.FS

const (
	musicLoginFile = "music-login.json"
	musicJobsDir   = "music-jobs"
	// Jobs whose files the client never fetched are removed after this.
	musicJobTTL = 24 * time.Hour
)

// musicBroader is what the second try adds: search for the artist and the
// title separately, and leave "feat. ..." and [bracketed] text out.
var musicBroader = []string{"--desperate", "--remove-ft", "--remove-brackets"}

const musicTryAgain = ` Try the name written differently: ` +
	`a shorter title, without "feat. ..." or remix and edition names, or without the artist if it is "Various Artists"`

var errMusicFailed = errors.New(`No match found on Amazon Music (see above).` + musicTryAgain)

// The plain log's errors, for Source.
var (
	errEngineFailed = errors.New("the engine stopped with an error (see above)")
	errMusicNoHits  = errors.New(`Nothing on Soulseek met the config's conditions (see above).` + musicTryAgain)
)

// musicMaxHits is how many search results reach the phone.
const musicMaxHits = 100

type musicService struct {
	dataDir string
	config  *musicConfig
	turn    chan struct{} // holds the running download
	mu      sync.Mutex    // guards the login file and unpacking the engine
}

func newMusicService(dataDir string) *musicService {
	// The engine runs in a temporary directory, so the paths we give it (its
	// own, and the job's) must not be relative to ours.
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	return &musicService{
		dataDir: dataDir,
		config:  &musicConfig{path: filepath.Join(dataDir, musicConfigFile)},
		turn:    make(chan struct{}, 1),
	}
}

func (m *musicService) register(mux *http.ServeMux) {
	m.config.register(mux)
	mux.HandleFunc("POST /music", m.run)
	mux.HandleFunc("POST /music-search", m.search)
	mux.HandleFunc("GET /music-jobs/{id}", m.getJob)
	mux.HandleFunc("DELETE /music-jobs/{id}", m.deleteJob)
	mux.HandleFunc("POST /music-login", m.relogin)
}

// musicEvent is one line of a run's response. An event with no fields only
// keeps the connection alive. Done is a musicResult, or a musicSearchResult
// for a search.
type musicEvent struct {
	Stdout *string `json:"stdout,omitempty"`
	Stderr *string `json:"stderr,omitempty"`
	Done   any     `json:"done,omitempty"`
}

// musicResult ends a run. Job is empty when nothing was saved.
type musicResult struct {
	Error string      `json:"error,omitempty"`
	Job   string      `json:"job,omitempty"`
	Files []musicFile `json:"files,omitempty"`
}

type musicFile struct {
	Path string `json:"path"` // relative to the job, with forward slashes
	Size int64  `json:"size"`
}

// musicRequest is the body of POST /music and POST /music-search.
type musicRequest struct {
	Mode  string `json:"mode"`
	Query string `json:"query"`
	Link  string `json:"link"`
}

// musicLink is a search result's link: a file, or a folder ending in "/".
var musicLink = regexp.MustCompile(`^slsk://[^/\x00-\x1f]+/[^\x00-\x1f]+$`)

// readMusicRequest reads and checks a request, answering it if it is bad. A
// link is only taken when links are.
func readMusicRequest(w http.ResponseWriter, r *http.Request, links bool) (musicRequest, bool) {
	var req musicRequest
	if err := decodeBody(r, 64<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, `expected JSON body {"mode": "song"|"album", "query": "..."}`)
		return req, false
	}
	if req.Link != "" && links {
		if !musicLink.MatchString(req.Link) || len(req.Link) > 4096 {
			writeError(w, http.StatusBadRequest, `link must be one search result's "slsk://user/path" link`)
			return req, false
		}
		if scopeOf(r) == scopeAgent {
			writeError(w, http.StatusForbidden, "this session can only download by name")
			return req, false
		}
		return req, true
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Mode != "song" && req.Mode != "album" {
		writeError(w, http.StatusBadRequest, `mode must be "song" or "album"`)
		return req, false
	}
	if req.Query == "" || strings.HasPrefix(req.Query, "-") || strings.ContainsAny(req.Query, "\x00\r\n") {
		writeError(w, http.StatusBadRequest, `query must be one line of text, not starting with "-"`)
		return req, false
	}
	return req, true
}

func (m *musicService) run(w http.ResponseWriter, r *http.Request) {
	req, ok := readMusicRequest(w, r, true)
	if !ok {
		return
	}
	m.stream(w, r, func(engine string, out *musicStream) any {
		res := m.download(r.Context(), engine, req, out)
		return &res
	})
}

func (m *musicService) search(w http.ResponseWriter, r *http.Request) {
	req, ok := readMusicRequest(w, r, false)
	if !ok {
		return
	}
	m.stream(w, r, func(engine string, out *musicStream) any {
		res := m.find(r.Context(), engine, req.Mode, req.Query, out)
		return &res
	})
}

// stream answers a checked request with a run: it waits for the turn, runs
// it, and sends what it returns as the last event.
func (m *musicService) stream(w http.ResponseWriter, r *http.Request, run func(engine string, out *musicStream) any) {
	engine, err := m.engine()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	out := newMusicStream(w)
	defer out.close()
	select {
	case m.turn <- struct{}{}:
	default:
		out.stderr("Another download is already running; waiting for it to finish.")
		select {
		case m.turn <- struct{}{}:
		case <-r.Context().Done():
			return
		}
	}
	defer func() { <-m.turn }()
	out.send(musicEvent{Done: run(engine, out)})
}

// musicSession is what an engine run needs: the account, and a private
// directory to run in holding the config with the login in it.
type musicSession struct {
	user, dir, config string
}

// args are the engine's arguments for a run on input.
func (s musicSession) args(input, inputType string, more ...string) []string {
	return append([]string{"--config", s.config, "--input", input, "--input-type", inputType}, more...)
}

// session makes one; done removes its directory.
func (m *musicService) session() (s musicSession, done func(), err error) {
	acct, err := m.account(false)
	if err != nil {
		return s, nil, err
	}
	conf, _, err := m.config.read()
	if err != nil {
		return s, nil, err
	}
	tmp, err := os.MkdirTemp("", "delta-music-*")
	if err != nil {
		return s, nil, err
	}
	s = musicSession{user: acct.Username, dir: tmp, config: filepath.Join(tmp, "music.conf")}
	if err := os.WriteFile(s.config, []byte(musicConfigWithLogin(conf, acct.Username, acct.Password)), 0o600); err != nil {
		os.RemoveAll(tmp)
		return s, nil, err
	}
	return s, func() { os.RemoveAll(tmp) }, nil
}

// download runs the engine for one request, with the turn held: a search by
// name that takes the best match, white-labeled, or the one search result a
// link names, plainly.
func (m *musicService) download(ctx context.Context, engine string, req musicRequest, out *musicStream) musicResult {
	m.sweepJobs()
	fail := func(err error) musicResult { return musicResult{Error: err.Error()} }
	s, done, err := m.session()
	if err != nil {
		return fail(err)
	}
	defer done()
	job := randomHex(16)
	jobDir := filepath.Join(m.dataDir, musicJobsDir, job)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return fail(err)
	}

	if req.Link != "" {
		args := s.args(req.Link, "soulseek", "--output-dir", jobDir, "--no-progress")
		err = runEngine(ctx, engine, s.dir, args, musicView{user: s.user, jobDir: jobDir, plain: true}, out)
	} else {
		view := musicView{user: s.user, jobDir: jobDir}
		args := s.args(req.Query, "string", "--"+req.Mode, "--output-dir", jobDir, "--no-progress")
		err = runEngine(ctx, engine, s.dir, args, view, out)
		if err == errMusicFailed {
			out.stdout("")
			out.stdout("No exact match found; trying a broader search on Amazon Music.")
			err = runEngine(ctx, engine, s.dir, append(args, musicBroader...), view, out)
		}
	}
	var res musicResult
	if err != nil {
		res.Error = err.Error()
	}
	if ctx.Err() == nil {
		res.Files = musicJobFiles(jobDir)
	}
	if len(res.Files) == 0 {
		os.RemoveAll(jobDir)
	} else {
		res.Job = job
	}
	return res
}

// musicSearchResult ends a search: the results, best first, or why there
// are none.
type musicSearchResult struct {
	Error   string     `json:"error,omitempty"`
	Results []musicHit `json:"results"`
}

// musicHit is one search result: a file for a song, a folder of files for an
// album. Link is what POST /music downloads it by.
type musicHit struct {
	Link   string         `json:"link"`
	User   string         `json:"user"`
	Speed  float64        `json:"speed"`            // their upload speed, in MB/s
	Free   bool           `json:"free"`             // whether they have an upload slot free
	Folder string         `json:"folder,omitempty"` // an album's folder, as shared
	Files  []musicHitFile `json:"files"`
}

type musicHitFile struct {
	Path       string `json:"path"` // as shared, with backslashes
	Size       int64  `json:"size"`
	Length     int    `json:"length,omitempty"` // seconds
	Bitrate    int    `json:"bitrate,omitempty"`
	SampleRate int    `json:"samplerate,omitempty"`
	BitDepth   int    `json:"bitdepth,omitempty"`
}

// find searches by name, with the turn held, for Source to choose from: the
// results the engine would choose among, best first. Like a download by name
// it tries once more, more broadly, when nothing turns up.
func (m *musicService) find(ctx context.Context, engine, mode, query string, out *musicStream) musicSearchResult {
	s, done, err := m.session()
	if err != nil {
		return musicSearchResult{Error: err.Error(), Results: []musicHit{}}
	}
	defer done()
	hits := []musicHit{}
	view := musicView{user: s.user, jobDir: s.dir, plain: true, take: func(line string) bool {
		found, ok := parseMusicHits(line)
		if ok {
			hits = found
		}
		return ok
	}}
	args := s.args(query, "string", "--"+mode,
		"--output-dir", filepath.Join(s.dir, "out"), "--no-write-index", "--no-progress", "--print", "json-all")
	err = runEngine(ctx, engine, s.dir, args, view, out)
	if len(hits) == 0 && err == errEngineFailed {
		out.stdout("")
		out.stdout("Nothing found; searching again with " + strings.Join(musicBroader, " ") + ".")
		err = runEngine(ctx, engine, s.dir, append(args, musicBroader...), view, out)
	}
	res := musicSearchResult{Results: hits}
	switch {
	case len(hits) > 0:
	case err != nil && err != errEngineFailed:
		res.Error = err.Error()
	default:
		res.Error = errMusicNoHits.Error()
	}
	return res
}

// engineHit is one entry of the engine's --print json-all output: a file for
// a song search, a folder's files for an album search.
type engineHit struct {
	User struct {
		Username          string
		UploadSpeed       float64
		HasFreeUploadSlot bool
	}
	File  *engineFile
	Files []engineFile
}

type engineFile struct {
	Filename                              string
	Size                                  int64
	Length, BitRate, SampleRate, BitDepth float64
}

// parseMusicHits reads the engine's --print json-all line, if line is it.
// Results it couldn't download by link are left out.
func parseMusicHits(line string) ([]musicHit, bool) {
	if !strings.HasPrefix(line, "[") {
		return nil, false
	}
	var found []engineHit
	if json.Unmarshal([]byte(line), &found) != nil {
		return nil, false
	}
	hits := []musicHit{}
	for _, e := range found {
		hit := musicHit{User: e.User.Username, Speed: e.User.UploadSpeed, Free: e.User.HasFreeUploadSlot}
		files := e.Files
		if e.File != nil {
			files = []engineFile{*e.File}
		}
		usable := hit.User != "" && !strings.Contains(hit.User, "/") && len(files) > 0
		for _, f := range files {
			if f.Filename == "" || strings.Contains(f.Filename, "/") {
				usable = false
			}
			hit.Files = append(hit.Files, musicHitFile{Path: f.Filename, Size: f.Size, Length: int(f.Length),
				Bitrate: int(f.BitRate), SampleRate: int(f.SampleRate), BitDepth: int(f.BitDepth)})
		}
		if !usable {
			continue
		}
		if e.File != nil {
			hit.Link = "slsk://" + hit.User + "/" + strings.ReplaceAll(e.File.Filename, `\`, "/")
		} else {
			hit.Folder = musicFolder(files)
			if hit.Folder == "" {
				continue
			}
			hit.Link = "slsk://" + hit.User + "/" + strings.ReplaceAll(hit.Folder, `\`, "/") + "/"
		}
		hits = append(hits, hit)
		if len(hits) == musicMaxHits {
			break
		}
	}
	return hits, true
}

// musicFolder is the folder an album's files share.
func musicFolder(files []engineFile) string {
	var common []string
	for i, f := range files {
		parts := strings.Split(f.Filename, `\`)
		parts = parts[:len(parts)-1]
		if i == 0 {
			common = parts
			continue
		}
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	return strings.Join(common, `\`)
}

// musicStream writes a run's events. Writes come from the engine's two
// output streams and the keepalive at once.
type musicStream struct {
	mu   sync.Mutex
	enc  *json.Encoder
	rc   *http.ResponseController
	stop chan struct{}
	wg   sync.WaitGroup
}

func newMusicStream(w http.ResponseWriter) *musicStream {
	s := &musicStream{enc: json.NewEncoder(w), rc: http.NewResponseController(w), stop: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.send(musicEvent{})
			case <-s.stop:
				return
			}
		}
	}()
	return s
}

func (s *musicStream) send(e musicEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enc.Encode(e) == nil {
		s.rc.Flush()
	}
}

func (s *musicStream) stdout(line string) { s.send(musicEvent{Stdout: &line}) }
func (s *musicStream) stderr(line string) { s.send(musicEvent{Stderr: &line}) }

// close stops the keepalive; nothing may be sent afterwards.
func (s *musicStream) close() {
	close(s.stop)
	s.wg.Wait()
}

// musicJobFiles lists the music (and album extras) a job saved, leaving out
// the engine's bookkeeping.
func musicJobFiles(dir string) []musicFile {
	var files []musicFile
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || !musicResultFile(rel) {
			return nil
		}
		if info, err := d.Info(); err == nil {
			files = append(files, musicFile{Path: filepath.ToSlash(rel), Size: info.Size()})
		}
		return nil
	})
	slices.SortFunc(files, func(a, b musicFile) int { return strings.Compare(a.Path, b.Path) })
	return files
}

func musicResultFile(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	base := filepath.Base(rel)
	return !strings.HasSuffix(base, ".incomplete") && !strings.HasPrefix(base, "_index")
}

// sweepJobs removes jobs older than musicJobTTL.
func (m *musicService) sweepJobs() {
	dir := filepath.Join(m.dataDir, musicJobsDir)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > musicJobTTL {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

func (m *musicService) jobDir(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := hex.DecodeString(id); err == nil && len(id) == 32 {
		dir := filepath.Join(m.dataDir, musicJobsDir, id)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, true
		}
	}
	writeError(w, http.StatusNotFound, "no such music job")
	return "", false
}

func (m *musicService) getJob(w http.ResponseWriter, r *http.Request) {
	dir, ok := m.jobDir(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	tw := tar.NewWriter(w)
	for _, f := range musicJobFiles(dir) {
		file, err := os.Open(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return
		}
		info, err := file.Stat()
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.Path, Mode: 0o644, Size: info.Size(), ModTime: info.ModTime()})
		}
		if err == nil {
			_, err = io.Copy(tw, file)
		}
		file.Close()
		if err != nil {
			// The client sees a truncated tar.
			return
		}
	}
	tw.Close()
}

func (m *musicService) deleteJob(w http.ResponseWriter, r *http.Request) {
	dir, ok := m.jobDir(w, r)
	if !ok {
		return
	}
	writeResult(w, os.RemoveAll(dir), http.StatusOK, map[string]string{"deleted": r.PathValue("id")})
}

func (m *musicService) relogin(w http.ResponseWriter, r *http.Request) {
	_, err := m.account(true)
	writeResult(w, err, http.StatusOK, map[string]bool{"renewed": true})
}

// ---- the account ----

type musicAccount struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// account returns the command center's Soulseek account, making one up if
// there is none yet or renew is set.
func (m *musicService) account(renew bool) (musicAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var acct musicAccount
	p := filepath.Join(m.dataDir, musicLoginFile)
	if !renew {
		if data, err := os.ReadFile(p); err == nil && json.Unmarshal(data, &acct) == nil && acct.Username != "" && acct.Password != "" {
			return acct, nil
		}
	}
	acct = newMusicAccount()
	return acct, writeJSONFile(p, acct)
}

// newMusicAccount makes up a login. Usernames are 16 characters from a
// 36-letter alphabet (about 82 bits), so nobody else on the network will
// pick the same one; Soulseek registers it on first login.
func newMusicAccount() musicAccount {
	const lower = "abcdefghijklmnopqrstuvwxyz"
	const digits = "0123456789"
	return musicAccount{
		Username: randomString(lower, 1) + randomString(lower+digits, 15),
		Password: randomString(lower+strings.ToUpper(lower)+digits, 24),
	}
}

// musicConfigWithLogin returns the config with the given login as its first
// lines, where they apply to every profile. checkMusicConfig keeps logins out
// of the stored config, but strip them anyway: the login must be ours.
func musicConfigWithLogin(text, user, pass string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "username = %s\npassword = %s\n", user, pass)
	for _, line := range strings.Split(text, "\n") {
		key, _, ok := strings.Cut(line, "=")
		if ok && musicLoginKeys[strings.ToLower(strings.TrimSpace(key))] {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// ---- the engine ----

// engine returns the path of the bundled engine, unpacking it into the data
// directory the first time.
func (m *musicService) engine() (string, error) {
	version, err := musicEngineFS.ReadFile("music-engine/engine.version")
	if err != nil {
		return "", fmt.Errorf("music downloads are not available on this command center (%s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	fields := strings.Fields(string(version))
	if len(fields) != 2 || len(fields[1]) < 12 {
		return "", errors.New("this build of the command center has a damaged music engine")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	dir := filepath.Join(m.dataDir, "music-engine-"+fields[1][:12])
	engine := filepath.Join(dir, "engine")
	if info, err := os.Stat(engine); err == nil && info.Mode().IsRegular() {
		return engine, nil
	}

	tmp, err := os.MkdirTemp(m.dataDir, ".music-engine-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := unpackMusicEngine(tmp); err != nil {
		return "", fmt.Errorf("unpacking the music engine: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	// Remove engines left by older versions of delta-cc.
	old, _ := filepath.Glob(filepath.Join(m.dataDir, "music-engine-*"))
	for _, d := range old {
		if d != dir {
			os.RemoveAll(d)
		}
	}
	return engine, nil
}

// unpackMusicEngine extracts the engine (and its license) into dir.
func unpackMusicEngine(dir string) error {
	f, err := musicEngineFS.Open("music-engine/engine.tar.gz")
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		var name string
		var mode os.FileMode = 0o644
		switch path.Base(h.Name) {
		case "sockseek":
			name, mode, found = "engine", 0o755, true
		case "LICENSE":
			name = "LICENSE"
		default:
			continue
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	if !found {
		return errors.New("engine binary missing from the bundle")
	}
	return nil
}

// ---- running the engine ----

var (
	ansiEscape  = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	engineNoise = []string{"[cli] Starting CLI session", "[soulseek] Logging in as"}
	// The network's name, with the space after it so "No Soulseek file
	// results" reads "No file results".
	networkName = regexp.MustCompile(`(?i)\[soulseek\] |soulseek ?`)
	// Lines that continue an exception's stack trace.
	stackTrace = regexp.MustCompile(`^\s+(at |--- End of |---> )`)
)

// musicLoginTries is how many failed Soulseek logins in a row make a run give
// up. The engine itself retries forever, with backoff, so without a limit a
// run against an unreachable server never ends. Four tries take about a
// minute.
const musicLoginTries = 4

var errMusicLogin = errors.New("couldn't reach Amazon Music (" + fmt.Sprint(musicLoginTries) +
	" tries). It may be down or rate-limiting downloads; try again in a few minutes")

var errSoulseekLogin = errors.New("couldn't log in to Soulseek (" + fmt.Sprint(musicLoginTries) +
	" tries). The server may be down or throttling this account; try again in a few minutes, " +
	"and reconnect the account only if it keeps happening")

// isLoginFailure reports whether an engine log line says a login attempt
// failed.
func isLoginFailure(line string) bool {
	return strings.HasPrefix(line, "[warn] [soulseek] Connection lost") ||
		strings.HasPrefix(line, "[error] [soulseek] Failed to ensure Soulseek connection") ||
		strings.HasPrefix(line, "[error] [soulseek] Initial Soulseek login failed")
}

var (
	// A remote path as the engine logs it: user\\..\\folder\\file or
	// user\\folder\\file, up to the end of the line or a quote.
	remotePath    = regexp.MustCompile(`[^\s:'"\\]+(\\[^\\'"\n]+)+`)
	engineLink    = regexp.MustCompile(`slsk://[^/\s]+/(.*?)/?$`)
	jobPrefix     = regexp.MustCompile(`^\[\d+\]\s+(\w+)Job:\s*`)
	jobStatus     = regexp.MustCompile(`^([A-Za-z][A-Za-z-]*).*?:(.*)$`)
	errorLine     = regexp.MustCompile(`^(\s*)(Error|Reason):`)
	completedLine = regexp.MustCompile(`^Completed:\s*(.*)$`)
	// Words that would give away where the files come from.
	sourceWords = regexp.MustCompile(`(?i)\bpeers?\b|upload|\bshares?\b|\bshared\b|\busers?(name)?\b|queued \([rl]\)|slsk|soulseek|sockseek`)
	// amazonReplacements recast engine wording as Amazon Music wording, so
	// progress can stay visible without leaking the source.
	amazonReplacements = []struct {
		re   *regexp.Regexp
		repl string
	}{
		{regexp.MustCompile(`(?i)\bpeers?\b`), "results"},
		{regexp.MustCompile(`(?i)\bshares?\b`), "results"},
		{regexp.MustCompile(`(?i)\bshared\b`), "found"},
		{regexp.MustCompile(`(?i)\busers?(names?)?\b`), "results"},
		{regexp.MustCompile(`(?i)upload`), "download"},
		{regexp.MustCompile(`(?i)queued\s*\([rl]\)`), "queued"},
		{regexp.MustCompile(`(?i)\bslsk\b|soulseek|sockseek`), "Amazon Music"},
		{regexp.MustCompile(`(?i)\bfile results\b`), "results"},
		{regexp.MustCompile(`(?i)\bNo results found\b`), "No match found on Amazon Music"},
	}
)

// musicJobWords is how a job line of one status reads: bare with no name to
// give, named (a format given the quoted name) otherwise, each with an
// album's wording where it differs.
type musicJobWords struct {
	bare, bareAlbum, named, namedAlbum string
}

func (w musicJobWords) say(album bool, display, name string) string {
	switch {
	case display == "" && album && w.bareAlbum != "":
		return w.bareAlbum
	case display == "":
		return w.bare
	case album && w.namedAlbum != "":
		return fmt.Sprintf(w.namedAlbum, name)
	}
	return fmt.Sprintf(w.named, name)
}

var (
	// A download is a purchase, which this words, then the file's download.
	musicJobDownload = musicJobWords{"Purchasing on Amazon Music...", "Purchasing an album on Amazon Music...", "Purchasing %q...", "Purchasing album %q..."}
	musicJobWorking  = musicJobWords{bare: "Working...", named: "Working on %q..."}
	musicJobStatuses = map[string]musicJobWords{}
)

func init() {
	for _, w := range []struct {
		statuses string
		musicJobWords
	}{
		{"searching search", musicJobWords{"Searching Amazon Music...", "Searching Amazon Music for an album...", "Searching Amazon Music for %q...", "Searching Amazon Music for album %q..."}},
		{"downloading download", musicJobDownload},
		{"succeeded success succeed downloaded", musicJobWords{bare: "Downloaded from Amazon Music.", named: "Downloaded %q."}},
		{"failed fail failure not-found notfound missing", musicJobWords{"No match found on Amazon Music.", "", "No match found for %q on Amazon Music.", "No match found for album %q on Amazon Music."}},
		{"queued queue waiting wait", musicJobWords{bare: "Waiting for Amazon Music...", named: "Waiting for %q..."}},
		{"retrying retry", musicJobWords{bare: "Retrying on Amazon Music...", named: "Retrying %q..."}},
		{"cancelled canceled cancel", musicJobWords{bare: "Cancelled on Amazon Music.", named: "Cancelled %q."}},
		{"skipped skip", musicJobWords{bare: "Skipped on Amazon Music.", named: "Skipped %q."}},
		{"already existing exists", musicJobWords{bare: "Already downloaded from Amazon Music.", named: "Already downloaded %q."}},
	} {
		for _, status := range strings.Fields(w.statuses) {
			musicJobStatuses[status] = w.musicJobWords
		}
	}
}

// rewriteMusicJobLine recasts a song/album job line the way an Amazon Music
// CLI would log it ("[002] SongJob: searching: ..." -> `Searching Amazon
// Music for "..."...`). Every other job kind (ExtractJob, SearchJob, ...)
// is internal and dropped, so nothing engine-specific leaks.
func rewriteMusicJobLine(line string) ([]string, bool) {
	m := jobPrefix.FindStringSubmatch(line)
	if m == nil || (m[1] != "Song" && m[1] != "Album") {
		return nil, false
	}
	album := m[1] == "Album"
	rest := jobPrefix.ReplaceAllString(line, "")
	for _, r := range amazonReplacements {
		rest = r.re.ReplaceAllString(rest, r.repl)
	}
	status, query, display := splitJobRest(rest)
	// A word we didn't anticipate still gives the source away: hide the
	// line rather than leak it.
	if sourceWords.MatchString(status + " " + query + " " + display) {
		return nil, false
	}
	words, ok := musicJobStatuses[status]
	if !ok {
		words = musicJobWorking
	}
	if words != musicJobDownload {
		return []string{words.say(album, display, display)}, true
	}
	// The resolved track is purchased, then its file downloads: the purchase
	// names the file without its extension.
	download := musicJobWords{bare: "Downloading from Amazon Music...", named: "Downloading %q..."}
	return []string{
		words.say(album, display, strings.TrimSuffix(display, filepath.Ext(display))),
		download.say(album, display, display),
	}, true
}

// splitJobRest splits "<status> [detail]: <query> [: <file>]" into the
// lowercased status word, the query, and the display name (the file when one
// is named, otherwise the query).
func splitJobRest(rest string) (status, query, display string) {
	payload := rest
	if sm := jobStatus.FindStringSubmatch(rest); sm != nil {
		status = strings.ToLower(sm[1])
		payload = sm[2]
	} else if f := strings.Fields(rest); len(f) > 0 {
		status = strings.ToLower(f[0])
		payload = strings.TrimSpace(strings.TrimPrefix(rest, f[0]))
	}
	parts := strings.Split(payload, ":")
	query = strings.Trim(strings.TrimSpace(parts[0]), `"`)
	display = strings.TrimSpace(parts[len(parts)-1])
	display = strings.Trim(display, `"`)
	return status, query, display
}

// musicLogLine rewrites one line of engine output for the white-labeled log. A job line
// can become two lines (a purchase, then its download). The engine's name,
// the network's, the login, and the people files come from never get through.
func musicLogLine(line, user string) ([]string, bool) {
	line = strings.TrimRight(ansiEscape.ReplaceAllString(line, ""), "\r")
	for _, p := range engineNoise {
		if strings.HasPrefix(line, p) {
			return nil, false
		}
	}
	if isLoginFailure(line) || stackTrace.MatchString(line) || strings.HasPrefix(line, "[debug]") || strings.HasPrefix(line, "[trace]") {
		return nil, false
	}
	if rest, ok := strings.CutPrefix(line, "[error] [cli] "); ok {
		line = "error: " + strings.TrimPrefix(rest, "Input error: ")
	}
	line = strings.TrimPrefix(line, "[cli] ")
	line = networkName.ReplaceAllString(line, "")
	if user != "" {
		line = strings.ReplaceAll(line, user, "")
	}
	line = engineLink.ReplaceAllStringFunc(line, func(m string) string {
		return path.Base(engineLink.FindStringSubmatch(m)[1])
	})
	line = remotePath.ReplaceAllStringFunc(line, func(p string) string {
		parts := strings.Split(strings.TrimRight(p, "\\"), "\\")
		return parts[len(parts)-1]
	})
	// Song/album job lines read like an Amazon Music CLI; anything else
	// that still hints at the source is not passed on.
	if jobPrefix.MatchString(line) {
		return rewriteMusicJobLine(line)
	}
	if sourceWords.MatchString(line) {
		if m := errorLine.FindStringSubmatch(line); m != nil {
			return []string{m[1] + m[2] + ": the download failed"}, true
		}
		return nil, false
	}
	if cm := completedLine.FindStringSubmatch(line); cm != nil {
		return []string{"Done: " + strings.ReplaceAll(cm[1], "succeeded", "downloaded")}, true
	}
	for _, r := range amazonReplacements {
		line = r.re.ReplaceAllString(line, r.repl)
	}
	return []string{line}, true
}

// plainLogLine is a line of engine output for Source, which sees the log as
// the engine wrote it, without its colours and debug lines.
func plainLogLine(line string) (string, bool) {
	line = strings.TrimRight(ansiEscape.ReplaceAllString(line, ""), "\r")
	if strings.HasPrefix(line, "[debug]") || strings.HasPrefix(line, "[trace]") {
		return "", false
	}
	return line, true
}

// musicView is how a run's log reaches whoever asked for it.
type musicView struct {
	user   string // the account, which the white-labeled log leaves out
	jobDir string // paths in it are made relative to it
	// plain passes the log on as the engine wrote it (plainLogLine), for
	// Source; otherwise it is white-labeled (musicLogLine).
	plain bool
	// take, if set, is offered each line of output first, and keeps the
	// ones it takes out of the log.
	take func(line string) bool
}

// runEngine runs the engine once in dir, with its stdin closed, and sends its
// output to out as v says, with paths in v.jobDir made relative to it (they
// mean nothing to the client, whose copies are elsewhere). If
// logins keep failing it stops the engine and returns errMusicLogin
// (errSoulseekLogin, plainly); if the engine reports that it couldn't do what
// it was asked, errMusicFailed (errEngineFailed).
func runEngine(ctx context.Context, engine, dir string, args []string, v musicView, out *musicStream) error {
	cmd := exec.CommandContext(ctx, engine, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the music engine: %w", err)
	}

	// The login log lines arrive on both streams.
	var mu sync.Mutex
	failures, gaveUp := 0, false
	watch := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasPrefix(line, "[warn] [soulseek] Connection lost"):
			failures++
			if failures == 1 && !v.plain {
				out.stderr("Couldn't reach Amazon Music; retrying.")
			}
			if failures >= musicLoginTries && !gaveUp {
				gaveUp = true
				cmd.Process.Kill()
			}
		case strings.HasPrefix(line, "[soulseek] Logging in as"), isLoginFailure(line), stackTrace.MatchString(line):
		default:
			failures = 0
		}
	}
	forward := func(r io.Reader, max int, send func(string)) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), max)
		for sc.Scan() {
			watch(sc.Text())
			if v.take != nil && v.take(sc.Text()) {
				continue
			}
			var lines []string
			if v.plain {
				if line, ok := plainLogLine(sc.Text()); ok {
					lines = []string{line}
				}
			} else {
				lines, _ = musicLogLine(sc.Text(), v.user)
			}
			for _, line := range lines {
				line = strings.ReplaceAll(line, v.jobDir+string(filepath.Separator), "")
				send(strings.ReplaceAll(line, v.jobDir, "."))
			}
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		forward(stderr, 16<<20, out.stderr)
	}()
	forward(stdout, 64<<20, out.stdout)
	<-done
	err = cmd.Wait()
	switch {
	case gaveUp && v.plain:
		return errSoulseekLogin
	case gaveUp:
		return errMusicLogin
	case ctx.Err() != nil:
		return ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if exit.ExitCode() == 2 {
			return errors.New("the music download was given invalid input (see above)")
		}
		if v.plain {
			return errEngineFailed
		}
		return errMusicFailed
	}
	return err
}
