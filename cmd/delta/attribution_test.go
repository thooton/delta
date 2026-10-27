package main

import (
	"slices"
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

func TestHooksEnv(t *testing.T) {
	got := hooksEnv([]string{"A=1", "GIT_CONFIG_KEY_0=k", "GIT_CONFIG_VALUE_0=v", "GIT_CONFIG_COUNT=1"}, "/h")
	want := []string{"A=1", "GIT_CONFIG_KEY_0=k", "GIT_CONFIG_VALUE_0=v",
		"GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=/h", "GIT_CONFIG_COUNT=2"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
}
