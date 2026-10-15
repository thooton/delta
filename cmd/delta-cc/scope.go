package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// Two kinds of token reach the command center.
//
// The delta token (data/token.sha256) is the phone's and the Claude Code
// sessions': it can do everything, and it is accepted only on the public
// listener (-addr).
//
// A backup session's token (see agents.go) is made for that one session and
// travels in its first message. It is accepted only on the agent socket,
// which is reachable from inside the session containers and nowhere else, so
// a copy that leaks out of a transcript is useless. Even there it is limited
// to what a backup agent needs (agentAllowed): the user's Canvas and Gmail
// stay out of its reach, and it can't change the command center's settings.

type scope string

const (
	scopeFull  scope = "full"
	scopeAgent scope = "agent"
)

type scopeKey struct{}

// scopeOf says which kind of token authorized r.
func scopeOf(r *http.Request) scope {
	if s, ok := r.Context().Value(scopeKey{}).(scope); ok {
		return s
	}
	return scopeFull
}

func withScope(r *http.Request, s scope) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), scopeKey{}, s))
}

func bearer(r *http.Request) (string, bool) {
	return strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// requireAuth rejects any request without the delta token.
func requireAuth(tokenHash []byte, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r)
		sum := sha256.Sum256([]byte(token))
		if !ok || subtle.ConstantTimeCompare(sum[:], tokenHash) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid or missing auth token")
			return
		}
		next.ServeHTTP(w, withScope(r, scopeFull))
	})
}

// requireAgentToken rejects any request without a live backup session's
// token, and any request that token isn't allowed to make.
func requireAgentToken(valid func(token string) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r)
		if !ok || !valid(token) {
			writeError(w, http.StatusUnauthorized, "invalid or missing auth token")
			return
		}
		if why := agentAllowed(r); why != "" {
			writeError(w, http.StatusForbidden, why)
			return
		}
		next.ServeHTTP(w, withScope(r, scopeAgent))
	})
}

// agentAllowed returns "" if a backup session may make request r, and
// otherwise why not, in words for the agent.
func agentAllowed(r *http.Request) string {
	p, m := r.URL.Path, r.Method
	first, _, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	switch first {
	case "auth", "version", "files", "music", "music-jobs", "github", "update":
		return ""
	case "music-login":
		return ""
	case "music-config", "books-login", "maps-key":
		if m == http.MethodGet {
			return ""
		}
		return "this session can't change the command center's settings"
	case "canvas", "canvas-upload", "canvas-status":
		return "Canvas isn't available in this session; the user needs a regular (●) session for Canvas"
	case "gmail-login":
		return "Gmail isn't available in this session; the user needs a regular (●) session for Gmail"
	case "personal-access-token":
		return "this session can't change the GitHub token; ask the user to save a new one from a regular (●) session"
	}
	return "this session's token can't use " + p
}
