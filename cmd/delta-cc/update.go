package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxBinarySize = 256 << 20

// version identifies this build by its git commit, which `go build` embeds
// automatically when building inside the repository.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return rev + dirty
}

// updater replaces the running binary with the latest official build, then
// signals restart so serve can shut down cleanly and re-exec itself.
type updater struct {
	mu      sync.Mutex
	restart chan struct{}
}

// releaseURL is where the official build of this command center for this
// machine is published. It is the only binary an update ever installs: no
// session can point the command center at one of its own.
func releaseURL() string {
	return "https://github.com/thooton/delta/releases/download/latest/delta-cc-" + runtime.GOOS + "-" + runtime.GOARCH
}

// handle installs the latest official build. Any request body is ignored
// (older CLIs send a URL; it is not used).
func (u *updater) handle(w http.ResponseWriter, r *http.Request) {
	if !u.mu.TryLock() {
		writeError(w, http.StatusConflict, "an update is already in progress")
		return
	}
	defer u.mu.Unlock()

	newVersion, err := install(r.Context(), releaseURL())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"old": version(), "new": newVersion})
	select {
	case u.restart <- struct{}{}:
	default:
	}
}

// install downloads the binary at url, checks that it runs and is a delta-cc
// build, and atomically moves it over the current executable.
func install(ctx context.Context, url string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(exe), ".delta-cc-update-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxBinarySize+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	if n > maxBinarySize {
		return "", fmt.Errorf("binary exceeds %d bytes", maxBinarySize)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}

	// The new binary must pass its own self-test, so a bad URL or a broken
	// build can't replace a working server.
	testCtx, testCancel := context.WithTimeout(ctx, time.Minute)
	defer testCancel()
	if out, err := exec.CommandContext(testCtx, tmp.Name(), "test").CombinedOutput(); err != nil {
		return "", fmt.Errorf("new binary failed `delta-cc test` (%v): %s", err, strings.TrimSpace(string(out)))
	}
	out, err := exec.CommandContext(testCtx, tmp.Name(), "version").Output()
	newVersion, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "delta-cc ")
	if err != nil || !ok {
		return "", fmt.Errorf("new binary did not report a delta-cc version")
	}

	if err := os.Rename(tmp.Name(), exe); err != nil {
		return "", err
	}
	return newVersion, nil
}

// reexec replaces the current process with the (new) binary on disk, keeping
// the same arguments, environment, and PID.
func reexec() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
