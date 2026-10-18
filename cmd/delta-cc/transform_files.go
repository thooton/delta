package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The transform CLI, for transform sessions to download (GET /transform/cli).
// The release workflow builds it into transform-cli/ before building the
// command center; a plain `go build` has none, and the route says so.
//
//go:embed transform-cli
var transformCLIFS embed.FS

const transformCLIPath = "transform-cli/transform-linux-amd64"

func serveTransformCLI(w http.ResponseWriter, r *http.Request) {
	data, err := transformCLIFS.ReadFile(transformCLIPath)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "this command center was built without the transform CLI")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="transform"`)
	w.Write(data)
}

// transformCLISelfTest checks that the bundled CLI runs, where it can: on
// the machine it was built for. Builds without one pass.
func transformCLISelfTest(dir string) error {
	data, err := transformCLIFS.ReadFile(transformCLIPath)
	if err != nil {
		return nil
	}
	path := filepath.Join(dir, "transform")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "help").Output()
	var exe *exec.Error
	if errors.As(err, &exe) || errors.Is(err, syscall.ENOEXEC) {
		return nil // another architecture
	}
	if err != nil || !strings.HasPrefix(string(out), "transform: ") {
		return fmt.Errorf("transform CLI: help printed %q (%v)", out, err)
	}
	return nil
}

// The fonts most documents ask for, Times New Roman above all, which the
// cloud sessions' image doesn't have (it has metric-compatible stand-ins).
// They are Microsoft's core fonts for the web, served as Microsoft
// published them, the installers its licence lets anyone pass on unchanged:
// the command center downloads each from SourceForge's mirror of them once,
// checks it against the hash pinned here, and keeps it. The setup script
// unpacks them (they are cabinet archives) into the session's fonts.
//
//	GET /transform/fonts/{name}   One installer, by its file name. No token needed.
var transformFonts = map[string]string{
	"times32.exe":  "db56595ec6ef5d3de5c24994f001f03b2a13e37cee27bc25c58f6f43e8f807ab", // Times New Roman
	"arial32.exe":  "85297a4d146e9c87ac6f74822734bdee5f4b2a722d7eaa584b7f2cbf76f478f6", // Arial
	"courie32.exe": "bb511d861655dde879ae552eb86b134d6fae67cb58502e6ff73ec5d9151f3384", // Courier New
	"georgi32.exe": "2c2c7dcda6606ea5cf08918fb7cd3f3359e9e84338dc690013f20cd42e930301", // Georgia
	"verdan32.exe": "c1cb61255e363166794e47664e2f21af8e3a26cb6346eb8d2ae2fa85dd5aad96", // Verdana
}

const transformFontsDir = "transform-fonts"

// transformFontSources are where an installer is downloaded from, in turn.
var transformFontSources = []string{
	"https://downloads.sourceforge.net/corefonts/",
	"https://master.dl.sourceforge.net/project/corefonts/the%20fonts/final/",
}

// fetchTransformFont downloads an installer. The self-test stands in for it.
var fetchTransformFont = func(ctx context.Context, name string) ([]byte, error) {
	client := &http.Client{Timeout: 2 * time.Minute}
	var errs []error
	for _, base := range transformFontSources {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+name, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err == nil && resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("%s answered HTTP %d", base, resp.StatusCode)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		return data, nil
	}
	return nil, errors.Join(errs...)
}

var errNoSuchFont = errors.New("no such font")

type transformFontStore struct {
	dir string
	mu  sync.Mutex // one download at a time
}

func newTransformFontStore(dataDir string) *transformFontStore {
	return &transformFontStore{dir: filepath.Join(dataDir, transformFontsDir)}
}

// get returns an installer, downloading it the first time.
func (f *transformFontStore) get(ctx context.Context, name string) ([]byte, error) {
	want, ok := transformFonts[name]
	if !ok {
		return nil, errNoSuchFont
	}
	path := filepath.Join(f.dir, name)
	matches := func(data []byte) bool {
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:]) == want
	}
	if data, err := os.ReadFile(path); err == nil && matches(data) {
		return data, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if data, err := os.ReadFile(path); err == nil && matches(data) {
		return data, nil
	}
	data, err := fetchTransformFont(ctx, name)
	if err != nil {
		return nil, err
	}
	if !matches(data) {
		return nil, fmt.Errorf("the download of %s isn't the file it should be", name)
	}
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path+".part", data, 0o600); err != nil {
		return nil, err
	}
	return data, os.Rename(path+".part", path)
}

func (f *transformFontStore) serve(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	data, err := f.get(ctx, r.PathValue("name"))
	if errors.Is(err, errNoSuchFont) {
		writeError(w, http.StatusNotFound, "no such font")
		return
	} else if err != nil {
		log.Printf("transform fonts: %v", err)
		writeError(w, http.StatusBadGateway, "couldn't get the font: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}
