package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestGitEnvBeatsCloudRewrites runs git with a Claude Code cloud session's
// own rewrite in place and checks that every way of writing a GitHub remote
// still reaches the proxy, and that the environment's other settings stay.
func TestGitEnvBeatsCloudRewrites(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cloud := []string{
		"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=url.https://github.com/.insteadOf", "GIT_CONFIG_VALUE_0=git@github.com:",
		"GIT_CONFIG_KEY_1=url.https://github.com/.insteadOf", "GIT_CONFIG_VALUE_1=ssh://git@github.com/",
		"GIT_CONFIG_KEY_2=credential.interactive", "GIT_CONFIG_VALUE_2=false",
	}
	env := gitEnv(cloud, srv.URL+"/github.com/")
	for _, remote := range []string{
		"https://github.com/thooton/delta",
		"git@github.com:thooton/delta",
		"ssh://git@github.com/thooton/delta",
	} {
		got = nil
		cmd := exec.Command("git", "ls-remote", remote)
		cmd.Env = env
		cmd.Dir = t.TempDir()
		out, _ := cmd.CombinedOutput()
		if len(got) == 0 || got[0] != "/github.com/thooton/delta/info/refs" {
			t.Errorf("%s: proxy saw %q; git said %s", remote, got, out)
		}
	}

	cmd := exec.Command("git", "config", "credential.interactive")
	cmd.Env = env
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "false" {
		t.Errorf("credential.interactive = %q (%v), want the environment's false kept", out, err)
	}
}
