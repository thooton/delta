package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestCloneConfig(t *testing.T) {
	got := cloneConfig([]string{"-c", "x=y", "clone", "-q", "https://github.com/thooton/sonim"}, "/h")
	want := []string{"-c", "x=y", "clone", "--config", "core.hooksPath=/h", "--config", "user.name=" + commitName,
		"--config", "user.email=" + commitEmail, "-q", "https://github.com/thooton/sonim"}
	if !slices.Equal(got, want) {
		t.Errorf("clone: got %q", got)
	}
	push := []string{"push", "origin", "HEAD:master"}
	if got := cloneConfig(push, "/h"); !slices.Equal(got, push) {
		t.Errorf("push: got %q", got)
	}
}

func TestGitEnvExtra(t *testing.T) {
	got := gitEnv([]string{"A=1", "GIT_CONFIG_KEY_0=k", "GIT_CONFIG_VALUE_0=v", "GIT_CONFIG_COUNT=1"}, "P", [2]string{"core.hooksPath", "/h"})
	n := len(githubURLPrefixes)
	want := fmt.Sprintf("GIT_CONFIG_KEY_%d=core.hooksPath GIT_CONFIG_VALUE_%d=/h GIT_CONFIG_COUNT=%d", n+1, n+1, n+2)
	if got[0] != "A=1" || got[1] != "GIT_CONFIG_KEY_0=k" || strings.Join(got[len(got)-3:], " ") != want {
		t.Errorf("got %q", got)
	}
}
