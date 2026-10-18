package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
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

var errNoLibreOffice = errors.New("the command center can't convert Word documents: LibreOffice isn't installed there. Submit a PDF instead")

// libreOfficeMu runs one conversion at a time: each takes a few hundred MB.
var libreOfficeMu sync.Mutex

// libreOffice converts a Word document to PDF with LibreOffice, headless,
// in a profile of its own that is thrown away after.
func libreOffice(ctx context.Context, docx []byte) ([]byte, error) {
	bin := ""
	for _, name := range []string{"soffice", "libreoffice", "/usr/lib/libreoffice/program/soffice", "/opt/libreoffice/program/soffice"} {
		if p, err := exec.LookPath(name); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		return nil, errNoLibreOffice
	}
	libreOfficeMu.Lock()
	defer libreOfficeMu.Unlock()
	dir, err := os.MkdirTemp("", "delta-docx-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "document.docx")
	if err := os.WriteFile(in, docx, 0o600); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin,
		"-env:UserInstallation=file://"+filepath.Join(dir, "profile"),
		"--headless", "--norestore", "--nolockcheck", "--nologo", "--nodefault",
		"--convert-to", "pdf:writer_pdf_Export", "--outdir", dir, in)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir)
	// soffice starts soffice.bin; a timeout stops both.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	out, err := cmd.CombinedOutput()
	pdf, rerr := os.ReadFile(filepath.Join(dir, "document.pdf"))
	if rerr != nil || len(pdf) == 0 {
		if err == nil {
			err = errors.New(strings.TrimSpace(string(out)))
		}
		return nil, fmt.Errorf("LibreOffice: %v", err)
	}
	return pdf, nil
}
