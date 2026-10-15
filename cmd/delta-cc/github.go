package main

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The GitHub personal access token the agent uses for git and gh on the
// user's repositories. No session ever sees it: it can be replaced
// (`delta set-personal-access-token`) but not read back, and `delta git` and
// `delta gh` send everything through the relay, which adds it on the way
// out. So someone who copies the delta token out of a transcript can use
// GitHub only through the command center, and rotating the delta token cuts
// them off.
//
// Routes (all behind the delta bearer token):
//
//	PUT /personal-access-token   Replace it with the token as the raw body.
//	ANY /github/{host}/{path...} Relayed to https://{host}/{path} with the
//	                             token, for the GitHub hosts git and gh use.
const accessTokenFile = "personal-access-token"

func accessTokenStore(dataDir string) *secretStore {
	return &secretStore{
		path:      filepath.Join(dataDir, accessTokenFile),
		route:     "/personal-access-token",
		field:     "token",
		what:      "token",
		missing:   "no personal access token is set; ask the user for one and run `delta set-personal-access-token -`",
		writeOnly: true,
	}
}

// githubHosts are the hosts the relay forwards to: git's smart HTTP on
// github.com, and the REST, GraphQL, upload and archive hosts gh uses.
var githubHosts = map[string]bool{
	"github.com":          true,
	"api.github.com":      true,
	"uploads.github.com":  true,
	"codeload.github.com": true,
}

type githubRelay struct {
	tokenFile string
	client    *http.Client
}

func newGitHubRelay(dataDir string) *githubRelay {
	return &githubRelay{
		tokenFile: filepath.Join(dataDir, accessTokenFile),
		// Redirects go back to the caller, which sends them through the
		// relay again if they lead to GitHub.
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

func (g *githubRelay) register(mux *http.ServeMux) {
	mux.HandleFunc("/github/{host}/{path...}", g.relay)
}

// hopHeaders are dropped in both directions, along with credentials.
var hopHeaders = []string{
	"Authorization", "Cookie", "Set-Cookie", "Connection", "Keep-Alive", "Proxy-Authorization",
	"Proxy-Authenticate", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		dst[k] = append([]string(nil), vs...)
	}
	for _, k := range hopHeaders {
		dst.Del(k)
	}
}

func (g *githubRelay) relay(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	if !githubHosts[host] {
		writeError(w, http.StatusNotFound, "the GitHub relay only reaches "+strings.Join(sortedKeys(githubHosts), ", "))
		return
	}
	data, err := os.ReadFile(g.tokenFile)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusServiceUnavailable, "no GitHub token is set on the command center; ask the user for one and save it with `delta set-personal-access-token -` (from a regular (●) session)")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	token := strings.TrimSpace(string(data))

	// The escaped path, so %2F and friends reach GitHub as sent.
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/github/"+host)
	target := "https://" + host + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	copyHeaders(out.Header, r.Header)
	out.ContentLength = r.ContentLength
	if host == "github.com" {
		// git's smart HTTP takes basic auth, as actions/checkout sends it.
		out.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
	} else {
		out.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := g.client.Do(out)
	if err != nil {
		writeError(w, http.StatusBadGateway, "GitHub could not be reached: "+err.Error())
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
}

// flushCopy streams body to w, flushing as it goes so a long git transfer
// or log stream reaches the client while it runs.
func flushCopy(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
