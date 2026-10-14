package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// `delta music` downloads music from the Soulseek network, and the command
// center does the downloading: agents usually run in sandboxes that can't
// reach the network. The work is done by an engine bundled into this binary
// (an unmodified sockseek release, see scripts/fetch-music-engine.sh), which
// the agent never runs or sees directly: we run it and pass on its log with
// its own name taken out.
//
// A run searches by name, downloads the best match into a job directory
// under data/music-jobs/, and if nothing was downloaded tries once more with
// a broader search. Its response is a stream of JSON lines (musicEvent): log
// lines for the CLI's stdout and stderr, an empty object now and then to
// keep proxies from timing out, and finally the result, naming the job and
// its files. The CLI then fetches the files as a tar into its current
// directory and deletes the job.
//
// The command center has one Soulseek account, kept in data/music-login.json.
// It is made up on first use and kept, because registering a new account on
// every run gets the machine throttled by the Soulseek server; only
// POST /music-login makes up a new one. One account can only be logged in
// once, so runs take turns.
//
// Routes (all behind the delta bearer token):
//
//	POST   /music             {"mode": "song"|"album", "query": "..."}; streams the run.
//	GET    /music-jobs/{id}   The job's files, as a tar.
//	DELETE /music-jobs/{id}   Remove the job.
//	POST   /music-login       Replace the account with a new one.

//go:embed music-engine
var musicEngineFS embed.FS

const (
	musicLoginFile = "music-login.json"
	musicJobsDir   = "music-jobs"
	// Jobs whose files the CLI never fetched are removed after this.
	musicJobTTL = 24 * time.Hour
)

// musicBroader is what the second try adds: search for the artist and the
// title separately, and leave "feat. ..." and [bracketed] text out.
var musicBroader = []string{"--desperate", "--remove-ft", "--remove-brackets"}

var errMusicFailed = errors.New(`No match found on Amazon Music (see above). Try the name written differently: ` +
	`a shorter title, without "feat. ..." or remix and edition names, or without the artist if it is "Various Artists"`)

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
	mux.HandleFunc("GET /music-jobs/{id}", m.getJob)
	mux.HandleFunc("DELETE /music-jobs/{id}", m.deleteJob)
	mux.HandleFunc("POST /music-login", m.relogin)
}

// musicEvent is one line of a run's response. An event with no fields only
// keeps the connection alive.
type musicEvent struct {
	Stdout *string      `json:"stdout,omitempty"`
	Stderr *string      `json:"stderr,omitempty"`
	Done   *musicResult `json:"done,omitempty"`
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

func (m *musicService) run(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode  string `json:"mode"`
		Query string `json:"query"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, `expected JSON body {"mode": "song"|"album", "query": "..."}`)
		return
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Mode != "song" && req.Mode != "album" {
		writeError(w, http.StatusBadRequest, `mode must be "song" or "album"`)
		return
	}
	if req.Query == "" || strings.HasPrefix(req.Query, "-") || strings.ContainsAny(req.Query, "\x00\r\n") {
		writeError(w, http.StatusBadRequest, `query must be one line of text, not starting with "-"`)
		return
	}
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
	res := m.download(r.Context(), engine, req.Mode, req.Query, out)
	out.send(musicEvent{Done: &res})
}

// download runs the engine for one request, with the turn held.
func (m *musicService) download(ctx context.Context, engine, mode, query string, out *musicStream) musicResult {
	m.sweepJobs()
	fail := func(err error) musicResult { return musicResult{Error: err.Error()} }
	acct, err := m.account(false)
	if err != nil {
		return fail(err)
	}
	conf, err := m.config.text()
	if err != nil {
		return fail(err)
	}
	id := make([]byte, 16)
	rand.Read(id)
	job := hex.EncodeToString(id)
	jobDir := filepath.Join(m.dataDir, musicJobsDir, job)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return fail(err)
	}
	tmp, err := os.MkdirTemp("", "delta-music-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)
	confPath := filepath.Join(tmp, "music.conf")
	if err := os.WriteFile(confPath, []byte(musicConfigWithLogin(conf, acct.Username, acct.Password)), 0o600); err != nil {
		return fail(err)
	}

	args := []string{"--config", confPath, "--input", query, "--input-type", "string", "--" + mode,
		"--output-dir", jobDir, "--no-progress"}
	err = runEngine(ctx, engine, tmp, args, acct.Username, jobDir, out)
	if err == errMusicFailed {
		out.stdout("")
		out.stdout("No exact match found; trying a broader search on Amazon Music.")
		err = runEngine(ctx, engine, tmp, append(args, musicBroader...), acct.Username, jobDir, out)
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
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
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
			// The CLI sees a truncated tar.
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
	if err := os.RemoveAll(dir); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": r.PathValue("id")})
}

func (m *musicService) relogin(w http.ResponseWriter, r *http.Request) {
	if _, err := m.account(true); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"renewed": true})
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
	data, _ := json.MarshalIndent(acct, "", "  ")
	tmp, err := os.CreateTemp(m.dataDir, ".music-login-*")
	if err != nil {
		return acct, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	return acct, err
}

// newMusicAccount makes up a login. Usernames are 16 characters from a
// 36-letter alphabet (about 82 bits), so nobody else on the network will
// pick the same one; Soulseek registers it on first login.
func newMusicAccount() musicAccount {
	const lower = "abcdefghijklmnopqrstuvwxyz"
	const digits = "0123456789"
	randString := func(alphabet string, n int) string {
		b := make([]byte, n)
		for i := range b {
			k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				panic(err)
			}
			b[i] = alphabet[k.Int64()]
		}
		return string(b)
	}
	return musicAccount{
		Username: randString(lower, 1) + randString(lower+digits, 15),
		Password: randString(lower+strings.ToUpper(lower)+digits, 24),
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
	if old, _ := filepath.Glob(filepath.Join(m.dataDir, "music-engine-*")); len(old) > 0 {
		for _, d := range old {
			if d != dir {
				os.RemoveAll(d)
			}
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
	remotePath = regexp.MustCompile(`[^\s:'"\\]+(\\[^\\'"\n]+)+`)
	engineLink = regexp.MustCompile(`slsk://[^/\s]+/(.*?)/?$`)
	jobPrefix  = regexp.MustCompile(`^\[\d+\]\s+(\w+)Job:\s*`)
	jobStatus  = regexp.MustCompile(`^([A-Za-z][A-Za-z-]*).*?:(.*)$`)
	errorLine  = regexp.MustCompile(`^(\s*)(Error|Reason):`)
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
	if sourceWords.MatchString(status+" "+query+" "+display) {
		return nil, false
	}
	quote := func() string { return fmt.Sprintf("%q", display) }
	switch status {
	case "searching", "search":
		if display == "" {
			if album {
				return []string{"Searching Amazon Music for an album..."}, true
			}
			return []string{"Searching Amazon Music..."}, true
		}
		if album {
			return []string{fmt.Sprintf("Searching Amazon Music for album %q...", display)}, true
		}
		return []string{fmt.Sprintf("Searching Amazon Music for %q...", display)}, true
	case "downloading", "download":
		// The resolved track is purchased, then its file downloads: the
		// purchase names the file without its extension.
		buy := display
		if ext := filepath.Ext(buy); ext != "" {
			buy = strings.TrimSuffix(buy, ext)
		}
		if display == "" {
			if album {
				return []string{"Purchasing an album on Amazon Music...", "Downloading from Amazon Music..."}, true
			}
			return []string{"Purchasing on Amazon Music...", "Downloading from Amazon Music..."}, true
		}
		var purchase string
		if album {
			purchase = fmt.Sprintf("Purchasing album %q...", buy)
		} else {
			purchase = fmt.Sprintf("Purchasing %q...", buy)
		}
		return []string{purchase, fmt.Sprintf("Downloading %s...", quote())}, true
	case "succeeded", "success", "succeed", "downloaded":
		if display == "" {
			return []string{"Downloaded from Amazon Music."}, true
		}
		return []string{fmt.Sprintf("Downloaded %s.", quote())}, true
	case "failed", "fail", "failure", "not-found", "notfound", "missing":
		if display == "" {
			return []string{"No match found on Amazon Music."}, true
		}
		if album {
			return []string{fmt.Sprintf("No match found for album %s on Amazon Music.", quote())}, true
		}
		return []string{fmt.Sprintf("No match found for %s on Amazon Music.", quote())}, true
	case "queued", "queue", "waiting", "wait":
		if display == "" {
			return []string{"Waiting for Amazon Music..."}, true
		}
		return []string{fmt.Sprintf("Waiting for %s...", quote())}, true
	case "retrying", "retry":
		if display == "" {
			return []string{"Retrying on Amazon Music..."}, true
		}
		return []string{fmt.Sprintf("Retrying %s...", quote())}, true
	case "cancelled", "canceled", "cancel":
		if display == "" {
			return []string{"Cancelled on Amazon Music."}, true
		}
		return []string{fmt.Sprintf("Cancelled %s.", quote())}, true
	case "skipped", "skip":
		if display == "" {
			return []string{"Skipped on Amazon Music."}, true
		}
		return []string{fmt.Sprintf("Skipped %s.", quote())}, true
	case "already", "existing", "exists":
		if display == "" {
			return []string{"Already downloaded from Amazon Music."}, true
		}
		return []string{fmt.Sprintf("Already downloaded %s.", quote())}, true
	default:
		if display == "" {
			return []string{"Working..."}, true
		}
		return []string{fmt.Sprintf("Working on %s...", quote())}, true
	}
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

// musicLogLine rewrites one line of engine output for the agent. A job line
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

// runEngine runs the engine once in dir, with its stdin closed, and sends its
// filtered output to out, with paths in jobDir made relative to it (they mean
// nothing to the agent, whose copies are in its own directory). If logins keep failing it stops the engine and
// returns errMusicLogin; if the engine reports that it couldn't download
// what it was asked for, errMusicFailed.
func runEngine(ctx context.Context, engine, dir string, args []string, user, jobDir string, out *musicStream) error {
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
			if failures == 1 {
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
			if lines, ok := musicLogLine(sc.Text(), user); ok {
				for _, line := range lines {
					line = strings.ReplaceAll(line, jobDir+string(filepath.Separator), "")
					send(strings.ReplaceAll(line, jobDir, "."))
				}
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
		return errMusicFailed
	}
	return err
}
