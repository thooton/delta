package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Backup sessions' containers, through the container runtime's CLI. Podman
// and Docker take the same commands here; run rootless Podman as the delta
// user, so a container is no more than that user (see AGENTS.md).

func containerName(id string) string {
	return "delta-" + strings.ReplaceAll(strings.TrimPrefix(id, agentIDPrefix), "_", "-")
}

// runtime runs the container runtime's CLI and returns its trimmed output.
func (a *agentService) runtime(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.cfg.Runtime, args...)
	if xdg := runtimeDir(); xdg != "" {
		cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+xdg)
	}
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, errors.New(lastLine(text))
	}
	return text, nil
}

// ensureContainer makes the session's container if there is none yet, and
// starts it if it is stopped.
func (a *agentService) ensureContainer(s *agentSession) error {
	name := containerName(s.meta.ID)
	out, err := a.runtime(time.Minute, "container", "inspect", "--format", "{{.State.Running}}", name)
	switch {
	case err == nil && out == "true":
		return nil
	case err == nil:
		s.status("Starting this session's computer…")
		if _, err := a.runtime(2*time.Minute, "start", name); err != nil {
			return fmt.Errorf("this session's computer wouldn't start: %w", err)
		}
		return nil
	case !strings.Contains(strings.ToLower(out), "no such"):
		return fmt.Errorf("the container runtime (%s) isn't working on the command center: %w", a.cfg.Runtime, err)
	}
	s.mu.Lock()
	made := s.meta.Container
	s.mu.Unlock()
	if made {
		s.status("This session's computer was gone, so it starts again empty…")
	} else {
		s.status("Setting up this session's computer…")
	}
	if err := os.MkdirAll(filepath.Dir(a.cfg.Socket), 0o755); err != nil {
		return err
	}
	// No --privileged, no host network and no runtime socket: the container
	// reaches this machine only through the agent socket.
	args := []string{"run", "-d",
		"--name", name,
		"--hostname", "delta",
		"--label", "delta-session=" + s.meta.ID,
		"--init",
		// Read-only, so no session can swap the socket for one of its own;
		// connecting to a socket still works.
		"-v", filepath.Dir(a.cfg.Socket) + ":" + agentSocketMount + ":ro",
	}
	if a.cfg.Memory != "" {
		args = append(args, "--memory", a.cfg.Memory)
	}
	if a.cfg.CPUs != "" {
		args = append(args, "--cpus", a.cfg.CPUs)
	}
	if a.cfg.PIDs > 0 {
		args = append(args, "--pids-limit", fmt.Sprint(a.cfg.PIDs))
	}
	args = append(args, a.cfg.Image, "sleep", "infinity")
	_, err = a.runtime(15*time.Minute, args...)
	if err != nil {
		return fmt.Errorf("this session's computer couldn't be made: %w", err)
	}
	s.mu.Lock()
	s.meta.Container = true
	s.saveMeta()
	s.mu.Unlock()
	return nil
}

func (a *agentService) stopContainer(id string) error {
	_, err := a.runtime(time.Minute, "stop", "-t", "5", containerName(id))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such") {
		return nil
	}
	return err
}

func (a *agentService) removeContainer(id string) error {
	out, err := a.runtime(2*time.Minute, "rm", "-f", containerName(id))
	if err != nil && strings.Contains(strings.ToLower(out), "no such") {
		return nil
	}
	return err
}

// runtimeDir is XDG_RUNTIME_DIR, which rootless Podman needs and a systemd
// service doesn't get: the user's /run/user/<uid> when it exists.
func runtimeDir() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return v
	}
	dir := fmt.Sprintf("/run/user/%d", os.Getuid())
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return ""
}
