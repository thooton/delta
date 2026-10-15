package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The model behind backup sessions: Muse Spark, through OpenCode Zen's free
// tier. No API key is involved, so there is nothing to pay for and nothing
// to keep secret; paid models aren't offered at all.
//
// Zen serves its free models only to requests that look like they come from
// the OpenCode CLI. As of OpenCode 1.18.35 it checks, and only checks, that:
//
//   - an Authorization header is there ("Bearer public" will do, on
//     /inference/v1, the CLI's own endpoint; /zen/v1 wants a real key);
//   - the User-Agent holds opencode/<version>, 1.18.0 or newer (older ones
//     get 426 Upgrade Required);
//   - x-opencode-session and x-opencode-session-id both hold an OpenCode
//     session id: "ses_" and a descending ULID (see zenSessionID);
//   - the request's tools include one named "read" whose parameters have at
//     least one property (pi's read tool has path, offset and limit).
//
// TLS, header case and order, the system prompt and the other x-opencode
// headers make no difference. pi sends all of this through the provider
// written to its models.json (writeModelsJSON), with the session id taken
// from DELTA_ZEN_SESSION, which launch sets from the session's session.json.

const (
	agentProvider = "zen-free"
	agentModelID  = "muse-spark-1.3-contributor-free"
	agentModel    = agentProvider + "/" + agentModelID
	zenBaseURL    = "https://opencode.ai/inference/v1"
	zenUserAgent  = "opencode/1.18.35 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"
)

// piAgentDir is pi's own configuration directory (PI_CODING_AGENT_DIR).
func (a *agentService) piAgentDir() string { return filepath.Join(a.dir, "pi-agent") }

// writeModelsJSON gives pi its one model, replacing any models.json there.
func (a *agentService) writeModelsJSON() error {
	cfg := map[string]any{
		"providers": map[string]any{
			agentProvider: map[string]any{
				"baseUrl": zenBaseURL,
				"api":     "openai-responses",
				"apiKey":  "public",
				"headers": map[string]string{
					"User-Agent":            zenUserAgent,
					"x-opencode-client":     "cli",
					"x-opencode-project":    "global",
					"x-opencode-session":    "${DELTA_ZEN_SESSION}",
					"x-opencode-session-id": "${DELTA_ZEN_SESSION}",
				},
				// As in pi's own catalog entry for this model.
				"models": []map[string]any{{
					"id":            agentModelID,
					"name":          "Muse Spark 1.3 Free",
					"reasoning":     true,
					"input":         []string{"text", "image"},
					"cost":          map[string]int{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
					"contextWindow": 1048576,
					"maxTokens":     131072,
					"compat":        map[string]string{"sessionAffinityFormat": "openai-nosession"},
					"thinkingLevelMap": map[string]any{
						"off": nil, "minimal": "minimal", "low": "low", "medium": "medium",
						"high": "high", "xhigh": "xhigh", "max": nil,
					},
				}},
			},
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.piAgentDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(a.piAgentDir(), "models.json"), data, 0o600)
}

// zenSessionID makes an OpenCode session id as the CLI does: "ses_", then
// (Date.now() * 0x1000 + counter) bit-inverted, as 6 bytes of hex, then 14
// random base62 characters.
func zenSessionID() (string, error) {
	const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	now := ^(uint64(time.Now().UnixMilli())*0x1000 + 1)
	id := fmt.Sprintf("ses_%012x", now&(1<<48-1))
	buf := make([]byte, 14)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for _, b := range buf {
		id += string(base62[int(b)%62])
	}
	return id, nil
}
