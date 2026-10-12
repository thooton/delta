package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/rand"
	"embed"
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
// directly: we run it for them and pass on its log with its own name taken
// out.
//
// There are two commands, song and album, and no options. Agents got lost in
// the engine's many settings and modes, so the CLI makes every choice: it
// searches by name, downloads the best match into ~/Music, and if nothing
// was downloaded tries once more with a broader search.
//
// The engine's configuration comes from the command center on every run
// (GET /music-config). It never holds a Soulseek login. Each CLI instance
// makes up its own account instead (see musicLogin), so agents downloading
// at the same time never share one.

//go:embed music-engine
var musicEngineFS embed.FS

// musicDir is where downloads go, whatever the config says.
const musicDir = "~/Music"

// musicBroader is what the second try adds: search for the artist and the
// title separately, and leave "feat. ..." and [bracketed] text out.
var musicBroader = []string{"--desperate", "--remove-ft", "--remove-brackets"}

var errMusicFailed = errors.New(`nothing was downloaded (see above). Try the name written differently: ` +
	`a shorter title, without "feat. ..." or remix and edition names, or without the artist if it is "Various Artists"`)

func runMusic(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(musicHelp)
		return nil
	}
	cmd, args := args[0], args[1:]
	what := map[string]string{"song": "artist - title", "album": "artist - album"}[cmd]
	if what == "" {
		return fmt.Errorf("unknown music command %q; there are only \"song\" and \"album\" (run `delta music help`)", cmd)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			return fmt.Errorf("delta music takes no options (got %s); give only %q", a, what)
		}
	}
	input := strings.TrimSpace(strings.Join(args, " "))
	if input == "" {
		return fmt.Errorf("usage: delta music %s \"<%s>\"", cmd, what)
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
	outDir, err := filepath.Abs(expandHome(musicDir))
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

	engineArgs := []string{"--config", confPath, "--input", input, "--input-type", "string", "--" + cmd,
		"--output-dir", outDir, "--no-progress"}
	before := snapshotDir(outDir)
	err = musicRun(engine, engineArgs, user)
	if err == errMusicFailed {
		fmt.Println("\nNothing was downloaded; trying a broader search.")
		err = musicRun(engine, append(engineArgs, musicBroader...), user)
	}
	if err != errMusicLogin {
		printNewFiles(outDir, before)
	}
	return err
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
	engineLink = regexp.MustCompile(`slsk://[^/\s]+/(.*?)/?$`)
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
		return path.Base(engineLink.FindStringSubmatch(m)[1])
	})
	line = remotePath.ReplaceAllStringFunc(line, func(p string) string {
		parts := strings.Split(strings.TrimRight(p, "\\"), "\\")
		return parts[len(parts)-1]
	})
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

// musicRun runs one download. It returns errMusicFailed when the engine
// reports that it couldn't download what it was asked for.
func musicRun(engine string, args []string, user string) error {
	err := startEngine(engine, args, user, func(line string) {
		if line, ok := musicLogLine(line, user); ok {
			fmt.Println(line)
		}
	})
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if exit.ExitCode() == 2 {
			return errors.New("the music download was given invalid input (see above)")
		}
		return errMusicFailed
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
