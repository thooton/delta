package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"embed"
	"encoding/base64"
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
	"syscall"
	"time"
)

// `delta music` downloads music from the Soulseek network. The work is done
// by an engine bundled into this binary (an unmodified sockseek release, see
// scripts/fetch-music-engine.sh), which the agent never runs or sees
// directly: we parse our own arguments, translate them into engine options,
// and pass on its log with its own name taken out.
//
// The engine's configuration comes from the command center on every run
// (GET /music-config) and can only be changed there. It never holds a
// Soulseek login. Each CLI instance makes up its own account instead (see
// musicLogin), so agents downloading at the same time never share one.

//go:embed music-engine
var musicEngineFS embed.FS

// musicValueFlags and musicBoolFlags are the options passed on to the engine
// under the same names. Short forms and our own options are handled in
// musicEngineArgs.
const (
	musicValueFlags = "output-dir format pref-format min-bitrate max-bitrate pref-min-bitrate pref-max-bitrate " +
		"length-tol album-track-count min-album-track-count max-album-track-count number offset name-format " +
		"cond pref input-type search-timeout max-stale-time skip-music-dir album-art regex " +
		"spotify-id spotify-secret spotify-token spotify-refresh youtube-key"
	musicBoolFlags = "strict-title strict-artist strict-album strict-conditions strict-album-quality fast-search " +
		"desperate remove-ft remove-brackets artist-maybe-wrong upgrade-to-album reverse no-skip-existing " +
		"write-playlist"
	// Our own options, plus short forms: -o output-dir, -n number, -d desperate.
	musicOwnValueFlags = "o n limit"
	musicOwnBoolFlags  = "d dry-run"
)

// musicPathFlags are resolved against the current directory before the
// engine sees them.
var musicPathFlags = map[string]bool{"output-dir": true, "skip-music-dir": true}

func runMusic(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(musicHelp)
		return nil
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "config":
		return musicConfigCmd(args)
	case "song", "album", "artist", "get", "search":
	default:
		return fmt.Errorf("unknown music command %q (run `delta music help`)", cmd)
	}

	o, err := parseOpts(args, musicValueFlags+" "+musicOwnValueFlags, musicBoolFlags+" "+musicOwnBoolFlags+" album")
	if err != nil {
		return err
	}
	input := strings.TrimSpace(strings.Join(o.pos, " "))
	if input == "" {
		return fmt.Errorf("usage: delta music %s <%s> [options]", cmd, map[string]string{
			"song": "artist - title", "album": "artist - album", "artist": "artist",
			"get": "url|music-link|file", "search": "artist - title",
		}[cmd])
	}
	if o.bools["album"] && cmd != "search" {
		return errors.New("--album only applies to search; use `delta music album` to download one")
	}
	if cmd != "search" && (o.str("limit") != "" || o.bools["json"]) {
		return errors.New("--limit and --json only apply to search")
	}
	if cmd == "search" && o.bools["dry-run"] {
		return errors.New("search never downloads anything; --dry-run is not needed")
	}

	limit := 0
	if cmd == "search" {
		// In a search, -n is the number of results to show.
		if limit, err = o.int("limit", 0); err == nil && limit == 0 {
			limit, err = o.int("n", 10)
		}
		if err != nil {
			return err
		}
		delete(o.vals, "n")
	}
	if cmd == "get" && strings.HasPrefix(input, musicLinkPrefix) {
		if input, err = decodeMusicLink(input); err != nil {
			return err
		}
	}
	engineArgs, err := musicEngineArgs(o)
	if err != nil {
		return err
	}
	switch cmd {
	case "song":
		engineArgs = append(engineArgs, "--song")
	case "album":
		engineArgs = append(engineArgs, "--album")
	case "artist":
		input = "artist=" + input
		engineArgs = append(engineArgs, "--album", "--aggregate")
	case "search":
		if o.bools["album"] {
			engineArgs = append(engineArgs, "--album")
		} else {
			engineArgs = append(engineArgs, "--song")
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	confText, err := do(cfg, http.MethodGet, "/music-config", nil)
	if err != nil {
		return fmt.Errorf("fetching the music config from the command center: %w", err)
	}
	engine, err := musicEngine()
	if err != nil {
		return err
	}
	acct, release, err := musicLogin()
	if err != nil {
		return err
	}
	defer release()
	user, pass := acct.Username, acct.Password
	dir, err := os.MkdirTemp("", "delta-music-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	confPath := filepath.Join(dir, "music.conf")
	if err := os.WriteFile(confPath, []byte(musicConfigWithLogin(string(confText), user, pass)), 0o600); err != nil {
		return err
	}
	engineArgs = append([]string{"--config", confPath, "--input", input}, engineArgs...)

	if cmd == "search" {
		return musicSearch(engine, engineArgs, user, input, o.bools["album"], limit, o.bools["json"])
	}

	outDir := o.str("output-dir")
	if outDir == "" {
		outDir = o.str("o")
	}
	if outDir == "" {
		outDir = musicConfigOutputDir(string(confText))
	}
	if outDir, err = filepath.Abs(expandHome(outDir)); err != nil {
		return err
	}
	if o.bools["dry-run"] {
		return musicRun(engine, append(engineArgs, "--print", "jobs", "--no-progress"), user, "")
	}
	return musicRun(engine, append(engineArgs, "--no-progress"), user, outDir)
}

// musicEngineArgs translates our options into engine options.
func musicEngineArgs(o *opts) ([]string, error) {
	var out []string
	add := func(name, val string) error {
		if musicPathFlags[name] {
			abs, err := filepath.Abs(expandHome(val))
			if err != nil {
				return err
			}
			val = abs
		}
		if name == "input-type" && val == "link" {
			val = "soulseek"
		} else if name == "input-type" && val == "soulseek" {
			return fmt.Errorf("unknown --input-type %q", val)
		}
		out = append(out, "--"+name, toEngineNames(val))
		return nil
	}
	alias := map[string]string{"o": "output-dir", "n": "number"}
	names := make([]string, 0, len(o.vals))
	for name := range o.vals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "limit" {
			continue
		}
		engineName := name
		if a, ok := alias[name]; ok {
			engineName = a
		}
		for _, v := range o.vals[name] {
			if err := add(engineName, v); err != nil {
				return nil, err
			}
		}
	}
	bools := make([]string, 0, len(o.bools))
	for name := range o.bools {
		bools = append(bools, name)
	}
	sort.Strings(bools)
	for _, name := range bools {
		switch name {
		case "d":
			out = append(out, "--desperate")
		case "dry-run", "json", "album":
		default:
			out = append(out, "--"+name)
		}
	}
	return out, nil
}

// The agent never sees the network's or the engine's names, or anything
// that shows where files come from. Search results are music:<id> links that
// hide the engine's slsk://user/path links, and the name-format tags
// slsk-foldername and slsk-filename are original-foldername and
// original-filename.
const musicLinkPrefix = "music:"

var (
	toEngine   = strings.NewReplacer("original-foldername", "slsk-foldername", "original-filename", "slsk-filename")
	fromEngine = strings.NewReplacer("slsk-foldername", "original-foldername", "slsk-filename", "original-filename")
)

func toEngineNames(s string) string   { return toEngine.Replace(s) }
func fromEngineNames(s string) string { return fromEngine.Replace(s) }

// musicLink makes the link for one file, or for a folder when path ends in "/".
func musicLink(user, path string) string {
	return musicLinkPrefix + base64.RawURLEncoding.EncodeToString([]byte(user+"\x00"+path))
}

func decodeMusicLink(link string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(link, musicLinkPrefix))
	user, path, ok := strings.Cut(string(raw), "\x00")
	if err != nil || !ok || user == "" || path == "" {
		return "", fmt.Errorf("%q is not a link from delta music search", link)
	}
	return "slsk://" + user + "/" + path, nil
}

// musicLoginKeys are the config keys that set the Soulseek login. The command
// center never stores them, but strip them anyway: the login must be ours.
var musicLoginKeys = map[string]bool{
	"user": true, "username": true, "pass": true, "password": true, "login": true, "l": true,
}

// musicConfigWithLogin returns the config with the given login as its first
// lines, where they apply to every profile.
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

// musicConfigOutputDir finds the output-dir before the first [profile].
func musicConfigOutputDir(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			break
		}
		key, val, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "output-dir" {
			val = strings.TrimSpace(val)
			if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
				val = val[1 : len(val)-1]
			}
			return val
		}
	}
	return "."
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// musicLogin is this CLI's Soulseek account. It is made up on first use and
// saved next to the CLI config, so each machine (each agent session) has its
// own and logs in with it every time; registering a new account on every run
// gets the machine throttled by the Soulseek server. One account can only be
// logged in once, so runs on the same machine take turns: the returned
// function releases the account.
type musicAccount struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func musicLogin() (acct musicAccount, release func(), err error) {
	cfgPath, err := configPath()
	if err != nil {
		return acct, nil, err
	}
	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return acct, nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "music.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return acct, nil, err
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		fmt.Fprintln(os.Stderr, "Another delta music run is using this machine's music login; waiting for it to finish.")
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			lock.Close()
			return acct, nil, err
		}
	}
	release = func() { lock.Close() }

	path := filepath.Join(dir, "music-login.json")
	if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &acct) == nil && acct.Username != "" && acct.Password != "" {
		return acct, release, nil
	}
	acct = newMusicAccount()
	data, _ := json.MarshalIndent(acct, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		release()
		return acct, nil, err
	}
	return acct, release, nil
}

// newMusicAccount makes up a login. Usernames are 16 characters from a
// 36-letter alphabet (about 82 bits), so no two CLIs, or anyone else on the
// network, will pick the same one; Soulseek registers it on first login.
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

// musicEngine returns the path of the bundled engine, unpacking it into the
// user cache directory the first time.
func musicEngine() (string, error) {
	version, err := musicEngineFS.ReadFile("music-engine/engine.version")
	if err != nil {
		return "", fmt.Errorf("music downloads are not available in this build of delta (%s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	fields := strings.Fields(string(version))
	if len(fields) != 2 || len(fields[1]) < 12 {
		return "", errors.New("this build of delta has a damaged music engine")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	base := filepath.Join(cache, "delta")
	dir := filepath.Join(base, "music-"+fields[1][:12])
	engine := filepath.Join(dir, "engine")
	if info, err := os.Stat(engine); err == nil && info.Mode().IsRegular() {
		return engine, nil
	}

	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(base, ".music-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := unpackMusicEngine(tmp); err != nil {
		return "", fmt.Errorf("unpacking the music engine: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another delta may have unpacked it at the same time.
		if _, serr := os.Stat(engine); serr != nil {
			return "", err
		}
	}
	// Remove engines left by older versions of delta.
	if old, _ := filepath.Glob(filepath.Join(base, "music-*")); len(old) > 0 {
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

var errMusicLogin = errors.New("couldn't connect (" + fmt.Sprint(musicLoginTries) +
	" tries). The service may be down or limiting this machine; try again in a few minutes")

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
	engineLink = regexp.MustCompile(`slsk://([^/\s]+)/(.*)$`)
	jobLine    = regexp.MustCompile(`^\[\d+\] \w+Job: `)
	errorLine  = regexp.MustCompile(`^(\s*)(Error|Reason):`)
	// Words that would give away where the files come from.
	sourceWords = regexp.MustCompile(`(?i)\bpeers?\b|upload|\bshares?\b|\bshared\b|\busers?(name)?\b|queued \([rl]\)|slsk|soulseek|sockseek`)
)

// musicLogLine rewrites one line of engine output for the agent, or drops it.
// The engine's name, the network's, the login, and the people files come
// from never get through.
func musicLogLine(line, user string) (string, bool) {
	line = strings.TrimRight(ansiEscape.ReplaceAllString(line, ""), "\r")
	for _, p := range engineNoise {
		if strings.HasPrefix(line, p) {
			return "", false
		}
	}
	if isLoginFailure(line) || stackTrace.MatchString(line) || strings.HasPrefix(line, "[debug]") || strings.HasPrefix(line, "[trace]") {
		return "", false
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
		sub := engineLink.FindStringSubmatch(m)
		return musicLink(sub[1], sub[2])
	})
	line = remotePath.ReplaceAllStringFunc(line, func(p string) string {
		parts := strings.Split(strings.TrimRight(p, "\\"), "\\")
		return parts[len(parts)-1]
	})
	line = fromEngineNames(line)
	// Job lines are left with a status, the search and a file or folder
	// name; anything else that still hints at the source is not passed on.
	if !jobLine.MatchString(line) && sourceWords.MatchString(line) {
		if m := errorLine.FindStringSubmatch(line); m != nil {
			return m[1] + m[2] + ": the download failed", true
		}
		return "", false
	}
	return line, true
}

// startEngine runs the engine with its stdin closed. Each line of its stdout
// goes to onStdout; its stderr is filtered onto ours. If logins keep failing
// it stops the engine and returns errMusicLogin.
func startEngine(engine string, args []string, user string, onStdout func(string)) error {
	cmd := exec.Command(engine, args...)
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
				fmt.Fprintln(os.Stderr, "Connecting failed; retrying.")
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

	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			watch(sc.Text())
			if line, ok := musicLogLine(sc.Text(), user); ok {
				fmt.Fprintln(os.Stderr, line)
			}
		}
	}()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		watch(sc.Text())
		onStdout(sc.Text())
	}
	<-done
	err = cmd.Wait()
	if gaveUp {
		return errMusicLogin
	}
	return err
}

// musicRun downloads, then lists what arrived in outDir (if given).
func musicRun(engine string, args []string, user, outDir string) error {
	before := snapshotDir(outDir)
	err := startEngine(engine, args, user, func(line string) {
		if line, ok := musicLogLine(line, user); ok {
			fmt.Println(line)
		}
	})
	if outDir != "" && err != errMusicLogin {
		printNewFiles(outDir, before)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if exit.ExitCode() == 2 {
			return errors.New("the music download was given invalid input (see above)")
		}
		return errors.New("not everything was downloaded (see above)")
	}
	return err
}

type fileStamp struct {
	size int64
	mod  time.Time
}

// snapshotDir records the files under dir, so printNewFiles can tell what a
// run added or replaced.
func snapshotDir(dir string) map[string]fileStamp {
	m := map[string]fileStamp{}
	if dir == "" {
		return m
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				m[p] = fileStamp{info.Size(), info.ModTime()}
			}
		}
		return nil
	})
	return m
}

// musicResultFile reports whether a file in the output directory is music
// (or album extras) rather than the engine's bookkeeping.
func musicResultFile(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	base := filepath.Base(rel)
	return !strings.HasSuffix(base, ".incomplete") && !strings.HasPrefix(base, "_index")
}

func printNewFiles(dir string, before map[string]fileStamp) {
	var added []string
	var total int64
	for p, st := range snapshotDir(dir) {
		rel, err := filepath.Rel(dir, p)
		if err != nil || !musicResultFile(rel) {
			continue
		}
		if old, ok := before[p]; ok && old == st {
			continue
		}
		added = append(added, rel)
		total += st.size
	}
	if len(added) == 0 {
		fmt.Printf("\nNo new files in %s.\n", dir)
		return
	}
	sort.Strings(added)
	fmt.Printf("\nSaved %d file(s), %s, in %s:\n", len(added), humanSize(total), dir)
	const max = 100
	for i, rel := range added {
		if i == max {
			fmt.Printf("  ... and %d more\n", len(added)-max)
			break
		}
		fmt.Println("  " + rel)
	}
}

// ---- search ----

type musicFile struct {
	Length     int    `json:"Length"`
	Filename   string `json:"Filename"`
	Size       int64  `json:"Size"`
	Bitrate    int    `json:"Bitrate"`
	SampleRate int    `json:"SampleRate"`
	BitDepth   int    `json:"BitDepth"`
}

type musicUser struct {
	Username          string  `json:"Username"`
	UploadSpeed       float64 `json:"UploadSpeed"`
	HasFreeUploadSlot bool    `json:"HasFreeUploadSlot"`
}

type musicResult struct {
	User  musicUser   `json:"User"`
	File  *musicFile  `json:"File"`  // song results
	Files []musicFile `json:"Files"` // album results
}

// musicHit is one search result as we show it.
type musicHit struct {
	Link      string `json:"link"`
	Name      string `json:"name"`
	Tracks    int    `json:"tracks,omitempty"`
	Seconds   int    `json:"seconds,omitempty"`
	Bitrate   int    `json:"bitrate_kbps,omitempty"`
	Format    string `json:"format,omitempty"`
	Bytes     int64  `json:"bytes"`
	SlowStart bool   `json:"slow_start"`
}

func musicSearch(engine string, args []string, user, input string, album bool, limit int, asJSON bool) error {
	var results []musicResult
	var parseErr error
	err := startEngine(engine, append(args, "--print", "json-all", "--progress-json"), user, func(line string) {
		if strings.HasPrefix(line, "[") {
			parseErr = json.Unmarshal([]byte(line), &results)
		}
	})
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return fmt.Errorf("search failed: %w", err)
	}
	if parseErr != nil {
		return fmt.Errorf("reading search results: %w", parseErr)
	}
	hits := musicHits(results)
	if asJSON {
		if limit > 0 && len(hits) > limit {
			hits = hits[:limit]
		}
		return printJSON(hits)
	}
	if len(hits) == 0 {
		fmt.Printf("No results for %q that meet the music config's requirements.\n", input)
		return nil
	}
	fmt.Printf("%d result(s) for %q, best first:\n", len(hits), input)
	for i, h := range hits {
		if limit > 0 && i == limit {
			fmt.Printf("... %d more (--limit %d shows them)\n", len(hits)-limit, len(hits))
			break
		}
		var info []string
		if album {
			info = append(info, fmt.Sprintf("%d tracks", h.Tracks))
		}
		if h.Seconds > 0 {
			info = append(info, fmtDuration(h.Seconds))
		}
		if h.Bitrate > 0 {
			info = append(info, fmt.Sprintf("%dkbps", h.Bitrate))
		}
		if h.Format != "" {
			info = append(info, h.Format)
		}
		info = append(info, humanSize(h.Bytes))
		if h.SlowStart {
			info = append(info, "may start slowly")
		}
		fmt.Printf("%2d. %s\n    %s\n    %s\n", i+1, h.Name, strings.Join(info, ", "), h.Link)
	}
	return nil
}

func musicHits(results []musicResult) []musicHit {
	var hits []musicHit
	for _, r := range results {
		h := musicHit{SlowStart: !r.User.HasFreeUploadSlot}
		switch {
		case r.File != nil:
			p := strings.ReplaceAll(r.File.Filename, `\`, "/")
			h.Link, h.Name = musicLink(r.User.Username, p), path.Base(p)
			h.Seconds, h.Bitrate, h.Bytes = r.File.Length, r.File.Bitrate, r.File.Size
			h.Format = strings.TrimPrefix(strings.ToLower(path.Ext(r.File.Filename)), ".")
		case len(r.Files) > 0:
			names := make([]string, len(r.Files))
			formats := map[string]bool{}
			var rateSum, rated int
			for i, f := range r.Files {
				names[i] = f.Filename
				h.Seconds += f.Length
				h.Bytes += f.Size
				if f.Bitrate > 0 {
					rateSum += f.Bitrate
					rated++
				}
				if ext := strings.TrimPrefix(strings.ToLower(path.Ext(f.Filename)), "."); ext != "" {
					formats[ext] = true
				}
			}
			dir := strings.ReplaceAll(commonSlskDir(names), `\`, "/")
			dir = strings.TrimSuffix(dir, "/")
			h.Link, h.Name = musicLink(r.User.Username, dir+"/"), path.Base(dir)
			h.Tracks = len(r.Files)
			if rated > 0 {
				h.Bitrate = rateSum / rated
			}
			h.Format = strings.Join(sortedSet(formats), ",")
		default:
			continue
		}
		hits = append(hits, h)
	}
	return hits
}

// commonSlskDir is the deepest folder (backslash-separated, as Soulseek
// paths are) that holds every one of the files.
func commonSlskDir(files []string) string {
	if len(files) == 0 {
		return ""
	}
	dir := func(f string) []string {
		parts := strings.Split(f, `\`)
		return parts[:len(parts)-1]
	}
	common := dir(files[0])
	for _, f := range files[1:] {
		parts := dir(f)
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	return strings.Join(common, `\`)
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fmtDuration(sec int) string {
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// ---- config ----

func musicConfigCmd(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
	}
	switch {
	case sub == "show" && len(args) <= 1:
		req, err := http.NewRequest(http.MethodGet, cfg.Server+"/music-config", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("server returned HTTP %d", resp.StatusCode)
		}
		if resp.Header.Get("X-Delta-Default") == "true" {
			fmt.Println("# The default music config (never changed).")
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		fmt.Print(fromEngineNames(string(data)))
		return nil
	case sub == "set" && len(args) == 2:
		var data []byte
		if args[1] == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(args[1])
		}
		if err != nil {
			return err
		}
		data = []byte(toEngineNames(string(data)))
		if _, err := do(cfg, http.MethodPut, "/music-config", bytes.NewReader(data)); err != nil {
			return err
		}
		fmt.Println("Music config saved on the command center.")
		return nil
	case sub == "reset" && len(args) == 1:
		if _, err := do(cfg, http.MethodDelete, "/music-config", nil); err != nil {
			return err
		}
		fmt.Println("Music config reset to the default.")
		return nil
	default:
		return errors.New("usage: delta music config [show | set <file|-> | reset]")
	}
}
