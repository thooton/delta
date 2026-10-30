// Command delta-cc is the Delta command center: the always-on relay between
// the phone, the agent's CLI, and everything else.
//
// Usage:
//
//	delta-cc [-data DIR] [-addr ADDR] [-max-upload BYTES] [canvas flags]   run the server
//	delta-cc gen-token [-data DIR]                          create/rotate the auth token
//	delta-cc smtp-password [-data DIR] [-user NAME]         create/rotate the SMTP relay's login
//	delta-cc shorthand-secret [-data DIR]                   create/rotate the shorthand worker's secret
//	delta-cc version                                        print the build version
//	delta-cc test                                           run a self-test
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const tokenHashFile = "token.sha256"

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "gen-token":
			genToken(args[1:])
			return
		case "smtp-password":
			genSMTPPassword(args[1:])
			return
		case "shorthand-secret":
			genShorthandSecret(args[1:])
			return
		case "test":
			if err := selfTest(); err != nil {
				fmt.Fprintln(os.Stderr, "FAIL:", err)
				os.Exit(1)
			}
			fmt.Println("PASS")
			return
		case "version":
			fmt.Println("delta-cc " + version())
			return
		case "help", "-h", "--help":
			usage()
			return
		}
	}
	serve(args)
}

func usage() {
	fmt.Fprint(os.Stderr, `Delta command center

Usage:
  delta-cc [-data DIR] [-addr ADDR] [-max-upload BYTES]
                                Run the server.
  delta-cc gen-token [-data DIR]
                                Generate a new auth token. Only its hash is
                                stored; the token is printed once. Running
                                this again rotates the token.
  delta-cc smtp-password [-data DIR] [-user NAME]
                                Make a new login for the SMTP relay (default
                                username delta), replacing any earlier one,
                                and print the lines for rmfakecloud's
                                environment file. Only the password's hash is
                                stored; it is printed once.
  delta-cc shorthand-secret [-data DIR]
                                Make a new secret for the shorthand reader's
                                worker (see shorthand.go), replacing any
                                earlier one at once. Only its hash is stored;
                                it is printed once.
  delta-cc version              Print the build version.
  delta-cc test                 Exercise the server against a temporary data
                                directory. A new binary must pass this before
                                an update will install it.

Defaults: -data ./data  -addr :8080  -max-upload 1073741824 (1 GiB)

Backup sessions (see agents.go):
  -agent-runtime CMD         container runtime CLI (default podman)
  -agent-image IMAGE         image for each session's container
                             (default docker.io/library/buildpack-deps:trixie-scm)
  -agent-socket PATH         the socket containers reach the command center on
                             (default DATA/agent-socket/delta.sock)
  -agent-idle DURATION       stop idle sessions' containers after this (default 15m)
  -agent-memory SIZE         each container's memory limit (default 4g; empty: none)
  -agent-cpus N              each container's CPU limit (default 2; empty: none)
  -agent-pids N              each container's process limit (default 4096; 0: none)
  -agent-setup FILE          a Bash script each new container runs as root before
                             the agent starts (default: install the delta CLI)

reMarkable, through rmfakecloud on this machine (see remarkable.go):
  -remarkable-data DIR       rmfakecloud's data directory
                             (default /opt/delta/rmfakecloud/data; empty: off)
  -remarkable-user USER      whose tablet (default sam13703)
  -remarkable-env FILE       rmfakecloud's environment file; with its
                             JWT_SECRET_KEY the tablet is told to sync after
                             each change
                             (default /opt/delta/rmfakecloud/rmfakecloud.conf)

SMTP relay, sending mail through the user's Gmail (see smtp.go):
  -smtp-addr ADDR            where it listens; keep it on 127.0.0.1, since it
                             takes passwords without TLS
                             (default 127.0.0.1:2525; empty: off)

Canvas relay (see canvas.go):
  -canvas-url URL            Canvas origin (default https://canvas.asu.edu;
                             empty disables the relay)
  -canvas-cookie-service URL canvas-cookie service on this machine
                             (default http://127.0.0.1:4782)
  -canvas-cookie-token FILE  its API token (default /etc/canvas-cookie/api-token)
`)
}

func genToken(args []string) {
	genSecret("gen-token", args, "delta_", tokenHashFile,
		"Token hash written to %s. Restart the server if it is running.\n",
		"Save this token now; it cannot be shown again:")
}

// genSecret is a command that makes a new secret starting with prefix,
// keeps its hash in file in the data directory, and prints it once, after
// saying where the hash is (saved, a format for the path) and what to do
// with the secret (keep).
func genSecret(name string, args []string, prefix, file, saved, keep string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory")
	fs.Parse(args)
	secret := prefix + randomToken(32)
	path, err := writeKeyHash(*dataDir, file, secret)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, saved, path)
	fmt.Fprintln(os.Stderr, keep)
	fmt.Println(secret)
}

func serve(args []string) {
	fs := flag.NewFlagSet("delta-cc", flag.ExitOnError)
	fs.Usage = usage
	dataDir := fs.String("data", "data", "data directory")
	addr := fs.String("addr", ":8080", "listen address")
	maxUpload := fs.Int64("max-upload", 1<<30, "maximum upload size in bytes")
	canvasURL := fs.String("canvas-url", "https://canvas.asu.edu", "Canvas origin; empty disables the Canvas relay")
	cookieService := fs.String("canvas-cookie-service", "http://127.0.0.1:4782", "canvas-cookie service URL")
	cookieToken := fs.String("canvas-cookie-token", "/etc/canvas-cookie/api-token", "canvas-cookie service API token file")
	var agentCfg agentConfig
	fs.StringVar(&agentCfg.Runtime, "agent-runtime", "podman", "container runtime CLI for backup sessions")
	fs.StringVar(&agentCfg.Image, "agent-image", "docker.io/library/buildpack-deps:trixie-scm", "image for backup sessions' containers")
	// No longer used, but an update keeps the command line it was started with.
	fs.String("agent-pi", "", "ignored")
	fs.StringVar(&agentCfg.Socket, "agent-socket", "", "socket backup sessions reach the command center on")
	fs.DurationVar(&agentCfg.Idle, "agent-idle", 15*time.Minute, "stop idle backup sessions after this long")
	fs.StringVar(&agentCfg.Memory, "agent-memory", "4g", "memory limit for each backup session's container; empty for none")
	fs.StringVar(&agentCfg.CPUs, "agent-cpus", "2", "CPU limit for each backup session's container; empty for none")
	fs.IntVar(&agentCfg.PIDs, "agent-pids", 4096, "process limit for each backup session's container; 0 for none")
	rmData := fs.String("remarkable-data", "/opt/delta/rmfakecloud/data", "rmfakecloud's data directory; empty turns the reMarkable off")
	rmUser := fs.String("remarkable-user", "sam13703", "the rmfakecloud user whose tablet delta uses")
	rmEnv := fs.String("remarkable-env", "/opt/delta/rmfakecloud/rmfakecloud.conf", "rmfakecloud's environment file, for its JWT_SECRET_KEY and PORT")
	smtpAddr := fs.String("smtp-addr", "127.0.0.1:2525", "the SMTP relay's listen address; empty turns it off")
	agentSetupFile := fs.String("agent-setup", "", "Bash script each new backup session's container runs as root; empty installs the delta CLI")
	fs.Parse(args)
	if *agentSetupFile != "" {
		// An update keeps the command line, so a script that has gone
		// missing falls back to the default rather than stopping the server.
		if data, err := os.ReadFile(*agentSetupFile); err != nil {
			log.Printf("backup sessions: -agent-setup: %v; using the default setup script", err)
		} else {
			agentCfg.Setup = string(data)
		}
	}
	if agentCfg.Socket == "" {
		agentCfg.Socket = filepath.Join(*dataDir, "agent-socket", "delta.sock")
	}
	if abs, err := filepath.Abs(agentCfg.Socket); err == nil {
		agentCfg.Socket = abs
	}

	canvas, err := newCanvasProxy(*canvasURL, *cookieService, *cookieToken, *maxUpload)
	if err != nil {
		log.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(*dataDir, tokenHashFile))
	if err != nil {
		log.Fatalf("no auth token configured (%v); run `delta-cc gen-token` first", err)
	}
	tokenHash, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(tokenHash) != sha256.Size {
		log.Fatalf("malformed %s; run `delta-cc gen-token` again", tokenHashFile)
	}

	filesDir := filepath.Join(*dataDir, "files")
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		log.Fatal(err)
	}

	// Backup sessions failing to set up must never keep the rest of the
	// command center from starting: it would then be out of reach for the
	// fix too.
	agents, err := newAgentService(*dataDir, agentCfg)
	var agentListener net.Listener
	if err == nil {
		agentListener, err = listenAgentSocket(agentCfg.Socket)
	}
	if err != nil {
		log.Printf("backup sessions are off: %v", err)
		agents = disabledAgentService(*dataDir, fmt.Sprintf("backup sessions are off on the command center: %v", err))
	}

	u := &updater{restart: make(chan struct{}, 1)}
	remarkable := newRemarkableStore(*rmData, *rmUser, *rmEnv, *maxUpload)
	public, agent, qa := newHandler(tokenHash, filesDir, *maxUpload, u, canvas, remarkable, agents)
	qa.start()
	qa.cloud.watchUsage()
	srv := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(public),
		ReadHeaderTimeout: 10 * time.Second,
	}
	agentSrv := &http.Server{Handler: logRequests(agent), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	listening := *addr
	if agentListener != nil {
		go func() { errc <- agentSrv.Serve(agentListener) }()
		agents.start()
		listening += " and " + agentCfg.Socket
	}
	log.Printf("command center %s listening on %s (data: %s)", version(), listening, *dataDir)
	// Like backup sessions, the SMTP relay failing to start leaves the rest
	// running.
	relay := newSMTPRelay(*dataDir)
	if *smtpAddr != "" {
		if !isLoopback(*smtpAddr) {
			log.Printf("smtp relay: %s isn't on the loopback interface, so passwords cross the network without TLS", *smtpAddr)
		}
		if ln, err := net.Listen("tcp", *smtpAddr); err != nil {
			log.Printf("smtp relay is off: %v", err)
		} else {
			log.Printf("smtp relay listening on %s", ln.Addr())
			go func() {
				if err := relay.serve(ln); err != nil {
					log.Printf("smtp relay stopped: %v", err)
				}
			}()
		}
	}

	select {
	case err := <-errc:
		log.Fatal(err)
	case <-u.restart:
		// Let the update response and other in-flight requests finish first.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		agentSrv.Shutdown(ctx)
		relay.shutdown(ctx)
		log.Print("restarting into updated binary")
		log.Fatal(reexec())
	}
}

// listenAgentSocket listens on the socket mounted into backup sessions'
// containers. The directory is bind-mounted, not the socket, so a restarted
// command center's new socket shows up in containers that are already
// running. Anyone in a container may connect; the token decides the rest.
func listenAgentSocket(path string) (net.Listener, error) {
	// sun_path holds 108 bytes, with its terminating NUL.
	if len(path) > 107 {
		return nil, fmt.Errorf("the agent socket's path %s is too long; pick a shorter one with -agent-socket", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// newHandler builds the authenticated API: the public handler, for the delta
// token, and the agent handler, for backup sessions' tokens on the agent
// socket (see scope.go), and the nightly QA, whose schedule the caller
// starts. It is shared by serve and selfTest. A nil canvas disables the
// Canvas relay, and a nil remarkable the reMarkable.
func newHandler(tokenHash []byte, filesDir string, maxUpload int64, u *updater, canvas *canvasProxy, remarkable *remarkableStore, agents *agentService) (public, agent http.Handler, qa *qaService) {
	s := &server{filesDir: filesDir, maxUpload: maxUpload}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scope": scopeOf(r)})
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": version()})
	})
	mux.HandleFunc("POST /update", u.handle)
	mux.HandleFunc("GET /files", s.listFiles)
	mux.HandleFunc("GET /files/{name}", s.getFile)
	mux.HandleFunc("PUT /files/{name}", s.putFile)
	mux.HandleFunc("DELETE /files/{name}", s.deleteFile)
	// Music, the book and Gmail logins, the cloud accounts, the access
	// token and the maps key live in the data directory, next to files/.
	dataDir := filepath.Dir(filesDir)
	newMusicService(dataDir).register(mux)
	booksLoginStore(dataDir).register(mux)
	gmailLoginStore(dataDir).register(mux)
	cloud := newCloudAccountStore(filepath.Join(dataDir, cloudAccountsFile))
	cloud.register(mux)
	(&conversationStore{dir: filepath.Join(dataDir, conversationsDir), max: maxUpload}).register(mux)
	accessTokenStore(dataDir).register(mux)
	newGitHubRelay(dataDir).register(mux)
	mapsKeyStore(dataDir).register(mux)
	(&otpStore{path: filepath.Join(dataDir, otpFile)}).register(mux)
	cards := newFlashcardService(dataDir)
	cards.register(mux)
	if canvas != nil {
		canvas.register(mux)
	} else {
		registerCanvasDisabled(mux)
	}
	if remarkable != nil {
		remarkable.register(mux)
	} else {
		registerRemarkableDisabled(mux)
	}
	// Backup sessions and transform jobs are run from the public side only,
	// so the agent handler never reaches these routes (agentAllowed refuses
	// them too).
	publicMux := http.NewServeMux()
	publicMux.Handle("/", mux)
	agents.register(publicMux)
	transforms := newTransformService(dataDir, filesDir, remarkable, cloud, maxUpload)
	transforms.register(publicMux)
	shorthand := newShorthandService(dataDir)
	shorthand.register(publicMux, transforms)
	qa = newQAService(dataDir, cloud)
	qa.register(publicMux)
	// Transform sessions fetch their CLI and fonts before they have a key.
	open := http.NewServeMux()
	open.HandleFunc("GET /transform/cli", serveTransformCLI)
	open.HandleFunc("GET /transform/fonts/{name}", newTransformFontStore(dataDir).serve)
	// The shorthand reader's worker signs in with a secret of its own.
	shorthand.registerWorker(open)
	// A flashcards device pairs before it has a token.
	cards.registerOpen(open)
	open.Handle("/", requireAuth(tokenHash, transforms, cards, qa, publicMux))
	return withWebsite(open), requireAgentToken(agents.validToken, mux), qa
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s (%s)", r.RemoteAddr, r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

type server struct {
	filesDir  string
	maxUpload int64
}

type fileInfo struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// validName accepts plain file names only, so a name can never escape filesDir.
// Names starting with "." are reserved for in-progress uploads.
func validName(name string) bool {
	return name != "" && len(name) <= 255 &&
		!strings.HasPrefix(name, ".") &&
		!strings.ContainsAny(name, "/\\\x00")
}

func (s *server) pathFor(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	name := r.PathValue("name")
	if !validName(name) {
		writeError(w, http.StatusBadRequest, "invalid file name")
		return "", "", false
	}
	return name, filepath.Join(s.filesDir, name), true
}

func (s *server) listFiles(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.filesDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	files := []fileInfo{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !validName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{Name: e.Name(), Size: info.Size(), Modified: info.ModTime().UTC()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Modified.After(files[j].Modified) })
	writeJSON(w, http.StatusOK, files)
}

func (s *server) getFile(w http.ResponseWriter, r *http.Request) {
	name, path, ok := s.pathFor(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	serveFile(w, r, path, name, "no such file")
}

// serveFile serves the file at path as name, answering 404 with notFound
// when it is missing. An error drops the caller's Content-Disposition.
func serveFile(w http.ResponseWriter, r *http.Request, path, name, notFound string) {
	fail := func(status int, msg string) {
		w.Header().Del("Content-Disposition")
		writeError(w, status, msg)
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		fail(http.StatusNotFound, notFound)
		return
	} else if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (s *server) putFile(w http.ResponseWriter, r *http.Request) {
	name, path, ok := s.pathFor(w, r)
	if !ok {
		return
	}
	// Files are never overwritten, so a name always means one upload.
	// Checking first refuses a taken name without reading the body.
	if _, err := os.Lstat(path); err == nil {
		writeExists(w, name)
		return
	}
	// Write to a hidden temp file and link it into place, so readers never
	// see a partial upload.
	tmp, n, ok := receiveUpload(w, r, s.filesDir, s.maxUpload, "file")
	if !ok {
		return
	}
	defer os.Remove(tmp)
	// Unlike a rename, a link fails if another upload took the name meanwhile.
	if err := os.Link(tmp, path); errors.Is(err, os.ErrExist) {
		writeExists(w, name)
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": name, "size": n})
}

// receiveUpload writes the request's body, up to max bytes, to a new hidden
// file in dir, and returns its path, which the caller removes, and size. If
// that fails it answers the request, saying what was too big.
func receiveUpload(w http.ResponseWriter, r *http.Request, dir string, max int64, what string) (string, int64, bool) {
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return "", 0, false
	}
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, max))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("%s exceeds %d bytes", what, max))
		} else {
			writeError(w, http.StatusBadRequest, "upload failed: "+err.Error())
		}
		return "", 0, false
	}
	return tmp.Name(), n, true
}

func writeExists(w http.ResponseWriter, name string) {
	writeError(w, http.StatusConflict, fmt.Sprintf("%s already exists; upload it under a different name", name))
}

func (s *server) deleteFile(w http.ResponseWriter, r *http.Request) {
	name, path, ok := s.pathFor(w, r)
	if !ok {
		return
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no such file")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
