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
// to what a backup agent needs (agentAllowed): the user's Canvas, Gmail and
// Claude accounts stay out of its reach, and it can't change the command
// center's settings.
//
// A transform job's key (see transform.go) is made for that job and travels
// in its cloud session's first message. It reaches the job's own routes
// (/transform/...) on the public listener and nothing else, and dies when
// the job ends.

type scope string

const (
	scopeFull  scope = "full"
	scopeAgent scope = "agent"
	// A transform job's key.
	scopeTransform scope = "transform"
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

// requireAuth rejects any request without the delta token, other than a
// running transform job's to its own routes.
func requireAuth(tokenHash []byte, jobs *transformService, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r)
		sum := sha256.Sum256([]byte(token))
		if ok && subtle.ConstantTimeCompare(sum[:], tokenHash) == 1 {
			next.ServeHTTP(w, withScope(r, scopeFull))
			return
		}
		if id, ok := jobs.job(token); ok {
			if !strings.HasPrefix(r.URL.Path, "/transform/") {
				writeError(w, http.StatusForbidden, "a transform job's key reaches only its own document")
				return
			}
			r = withScope(r, scopeTransform)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), transformJobKey{}, id)))
			return
		}
		if strings.HasPrefix(token, transformKeyStart) {
			writeError(w, http.StatusUnauthorized, "this transform job is over (its result was submitted, or it failed, was cancelled or timed out), so its key no longer works")
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid or missing auth token")
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
	case "auth", "version", "files", "music", "music-jobs", "github", "update", "remarkable":
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
	case "cloud-accounts":
		return "the Claude accounts aren't available in this session; the user manages them from a regular (●) session"
	case "conversations":
		// A conversation carried over from a session that ran out of usage
		// can be resumed here too; storing one is the client's job.
		if m == http.MethodGet {
			return ""
		}
		return "this session can't store conversations"
	}
	return "this session's token can't use " + p
}
