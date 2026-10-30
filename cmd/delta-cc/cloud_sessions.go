package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// What transform jobs and QA runs share for starting cloud sessions of their
// own: an environment found by its name on the account (or made), and a
// session in it whose first message is the job's prompt, in auto mode.

// cloudSessionsBeta is the cloud's beta for environments and sessions, as
// the app sends it.
const cloudSessionsBeta = "ccr-byoc-2025-07-29"

// A cloudEnv is the environment a kind of job runs its sessions in: one
// per account and command center address, found by its name (prefix and a
// hash of the setup script and the address), or made.
type cloudEnv struct {
	prefix      string // e.g. "Delta QA "
	description string
	setup       string // its init_script
	server      string // the command center's address as sessions reach it
	// config is the rest of the "config" of an anthropic_cloud environment.
	config map[string]any
}

func (e cloudEnv) name() string {
	return e.prefix + keyHash(e.setup + "\n" + e.server)[:8]
}

// launch starts a session on account in env, titled title, whose first
// message is prompt, and returns its id.
func (s *cloudAccountStore) launch(ctx context.Context, account string, env cloudEnv, title, prompt string) (string, error) {
	org, err := s.organization(ctx, account)
	if err != nil {
		return "", err
	}
	name := env.name()
	s.mu.Lock()
	id := s.envs[account+" "+name]
	s.mu.Unlock()
	if id == "" {
		config := map[string]any{
			"environment_type": "anthropic",
			"cwd":              "/home/user",
			"init_script":      env.setup,
			"languages":        []any{map[string]string{"name": "python", "version": "3.11"}, map[string]string{"name": "node", "version": "20"}},
		}
		for k, v := range env.config {
			config[k] = v
		}
		if id, err = s.environment(ctx, account, org, name, env.description, config); err != nil {
			return "", err
		}
		s.mu.Lock()
		s.envs[account+" "+name] = id
		s.mu.Unlock()
	}
	return s.startSession(ctx, account, org, id, title, prompt)
}

// environment finds the account's cloud environment called name, or makes it
// with config (the "config" of an anthropic_cloud environment).
func (s *cloudAccountStore) environment(ctx context.Context, account, org, name, description string, config map[string]any) (string, error) {
	headers := map[string]string{"x-organization-uuid": org}
	status, data, err := s.request(ctx, account, http.MethodGet, "/v1/environment_providers", headers, nil)
	if err != nil {
		return "", err
	}
	var listed struct {
		Environments []struct {
			Kind, Name    string
			EnvironmentID string `json:"environment_id"`
		}
	}
	if status != http.StatusOK || json.Unmarshal(data, &listed) != nil {
		return "", fmt.Errorf("listing the environments answered HTTP %d", status)
	}
	for _, e := range listed.Environments {
		if e.Kind == "anthropic_cloud" && e.Name == name {
			return e.EnvironmentID, nil
		}
	}
	body, _ := json.Marshal(map[string]any{
		"name":        name,
		"kind":        "anthropic_cloud",
		"description": description,
		"config":      config,
	})
	headers["anthropic-beta"] = cloudSessionsBeta
	status, data, err = s.request(ctx, account, http.MethodPost, "/v1/environment_providers/cloud/create", headers, body)
	if err != nil {
		return "", err
	}
	var made struct {
		EnvironmentID string `json:"environment_id"`
	}
	if status/100 != 2 || json.Unmarshal(data, &made) != nil || made.EnvironmentID == "" {
		return "", fmt.Errorf("making the environment answered HTTP %d", status)
	}
	return made.EnvironmentID, nil
}

// startSession starts a session in environment env whose first message is
// prompt, and returns its id. Titles are cut to 60 characters.
func (s *cloudAccountStore) startSession(ctx context.Context, account, org, env, title, prompt string) (string, error) {
	if r := []rune(title); len(r) > 60 {
		title = string(r[:59]) + "…"
	}
	body, _ := json.Marshal(map[string]any{
		"title": title,
		"events": []any{
			map[string]any{"type": "event", "data": map[string]any{
				"type": "control_request", "request_id": randomUUID(), "uuid": randomUUID(),
				"request": map[string]any{"subtype": "set_permission_mode", "mode": "auto"},
			}},
			map[string]any{"type": "event", "data": map[string]any{
				"uuid": randomUUID(), "session_id": "", "type": "user", "parent_tool_use_id": nil,
				"message": map[string]any{"role": "user", "content": prompt},
			}},
		},
		"session_context": map[string]any{"sources": []any{}, "outcomes": []any{}},
		"environment_id":  env,
	})
	headers := map[string]string{"x-organization-uuid": org, "anthropic-beta": cloudSessionsBeta}
	status, data, err := s.request(ctx, account, http.MethodPost, "/v1/sessions", headers, body)
	if err != nil {
		return "", err
	}
	var made struct{ ID string }
	if status/100 != 2 || json.Unmarshal(data, &made) != nil || !cloudID(made.ID) {
		return "", fmt.Errorf("starting the session answered HTTP %d", status)
	}
	return made.ID, nil
}

// codeSession is a session's id as the code sessions routes take it.
func codeSession(id string) string {
	if rest, ok := strings.CutPrefix(id, "session_"); ok {
		return "cse_" + rest
	}
	return id
}

// sessionIdle says whether a session has stopped working: archived, or its
// worker idle. An error means it couldn't be told.
func (s *cloudAccountStore) sessionIdle(ctx context.Context, account, session string) (bool, error) {
	status, data, err := s.request(ctx, account, http.MethodGet, "/v1/code/sessions/"+codeSession(session), nil, nil)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("the session lookup answered HTTP %d", status)
	}
	var got struct {
		Status       string `json:"status"`
		WorkerStatus string `json:"worker_status"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		return false, err
	}
	return got.Status == "archived" || got.WorkerStatus == "idle", nil
}

// archiveSession archives a session, as the website's Archive does, so it
// leaves the chats list.
func (s *cloudAccountStore) archiveSession(ctx context.Context, account, session string) error {
	status, _, err := s.request(ctx, account, http.MethodPost, "/v1/code/sessions/"+codeSession(session)+"/archive", nil, []byte("{}"))
	if err != nil {
		return err
	}
	if status/100 != 2 && status != http.StatusConflict {
		return fmt.Errorf("archiving the session answered HTTP %d", status)
	}
	return nil
}
