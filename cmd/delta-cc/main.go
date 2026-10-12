// Command delta-cc is the Delta command center: the always-on relay between
// the phone, the agent's CLI, and everything else.
//
// Usage:
//
//	delta-cc [-data DIR] [-addr ADDR] [-max-upload BYTES] [canvas flags]   run the server
//	delta-cc gen-token [-data DIR]                          create/rotate the auth token
//	delta-cc version                                        print the build version
//	delta-cc test                                           run a self-test
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
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
  delta-cc version              Print the build version.
  delta-cc test                 Exercise the server against a temporary data
                                directory. A new binary must pass this before
                                an update will install it.

Defaults: -data ./data  -addr :8080  -max-upload 1073741824 (1 GiB)

Canvas relay (see canvas.go):
  -canvas-url URL            Canvas origin (default https://canvas.asu.edu;
                             empty disables the relay)
  -canvas-cookie-service URL canvas-cookie service on this machine
                             (default http://127.0.0.1:4782)
  -canvas-cookie-token FILE  its API token (default /etc/canvas-cookie/api-token)
`)
}

func genToken(args []string) {
	fs := flag.NewFlagSet("gen-token", flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory")
	fs.Parse(args)

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		log.Fatal(err)
	}
	token := "delta_" + base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	path := filepath.Join(*dataDir, tokenHashFile)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(sum[:])+"\n"), 0o600); err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "Token hash written to %s. Restart the server if it is running.\n", path)
	fmt.Fprintln(os.Stderr, "Save this token now; it cannot be shown again:")
	fmt.Println(token)
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
	fs.Parse(args)

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

	u := &updater{restart: make(chan struct{}, 1)}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(newHandler(tokenHash, filesDir, *maxUpload, u, canvas)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("command center %s listening on %s (data: %s)", version(), *addr, *dataDir)

	select {
	case err := <-errc:
		log.Fatal(err)
	case <-u.restart:
		// Let the update response and other in-flight requests finish first.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		log.Print("restarting into updated binary")
		log.Fatal(reexec())
	}
}

// newHandler builds the authenticated API. It is shared by serve and selfTest.
// A nil canvas disables the Canvas relay.
func newHandler(tokenHash []byte, filesDir string, maxUpload int64, u *updater, canvas *canvasProxy) http.Handler {
	s := &server{filesDir: filesDir, maxUpload: maxUpload}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": version()})
	})
	mux.HandleFunc("POST /update", u.handle)
	mux.HandleFunc("GET /files", s.listFiles)
	mux.HandleFunc("GET /files/{name}", s.getFile)
	mux.HandleFunc("PUT /files/{name}", s.putFile)
	mux.HandleFunc("DELETE /files/{name}", s.deleteFile)
	// The music config lives in the data directory, next to files/.
	(&musicConfig{path: filepath.Join(filepath.Dir(filesDir), musicConfigFile)}).register(mux)
	(&booksLoginStore{path: filepath.Join(filepath.Dir(filesDir), booksLoginFile)}).register(mux)
	if canvas != nil {
		canvas.register(mux)
	} else {
		registerCanvasDisabled(mux)
	}
	return requireAuth(tokenHash, mux)
}

// requireAuth rejects any request without a valid "Authorization: Bearer <token>" header.
func requireAuth(tokenHash []byte, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(token))
		if !ok || subtle.ConstantTimeCompare(sum[:], tokenHash) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid or missing auth token")
			return
		}
		next.ServeHTTP(w, r)
	})
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
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no such file")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (s *server) putFile(w http.ResponseWriter, r *http.Request) {
	name, path, ok := s.pathFor(w, r)
	if !ok {
		return
	}
	// Write to a hidden temp file and rename, so readers never see a partial upload.
	tmp, err := os.CreateTemp(s.filesDir, ".upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, s.maxUpload))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds %d bytes", s.maxUpload))
		} else {
			writeError(w, http.StatusBadRequest, "upload failed: "+err.Error())
		}
		return
	}
	_, statErr := os.Stat(path)
	replaced := statErr == nil
	if err := os.Rename(tmp.Name(), path); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusCreated
	if replaced {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"name": name, "size": n, "replaced": replaced})
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
