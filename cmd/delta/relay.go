package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The user's GitHub token stays on the command center, so `delta git` and
// `delta gh` never hold it. Instead the CLI runs a small proxy for the
// length of the command, and points git and gh at it:
//
//   - git, through url.<proxy>/github.com/.insteadOf=https://github.com/,
//     which rewrites every GitHub URL it fetches from or pushes to;
//   - gh, through its http_unix_socket setting, which makes it send every
//     request as plain HTTP over a unix socket (and a placeholder GH_TOKEN,
//     so it doesn't ask to log in).
//
// The proxy forwards GitHub requests to the command center's relay
// (/github/<host>/<path>), which adds the token on the way out. Anything
// else gh fetches (signed download links on other hosts) it fetches
// directly, with no credentials.

// githubHosts are the hosts the command center relays to.
var githubHosts = map[string]bool{
	"github.com":          true,
	"api.github.com":      true,
	"uploads.github.com":  true,
	"codeload.github.com": true,
}

type githubProxy struct {
	cfg    config
	client *http.Client // returns redirects to git or gh, which follow them through the proxy
}

func (p *githubProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	path := r.URL.EscapedPath()
	if !githubHosts[host] {
		// From git: the GitHub host is the path's first segment.
		first, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
		host, path = first, "/"+rest
	}
	var target string
	relayed := githubHosts[host]
	switch {
	case relayed:
		target = p.cfg.Server + "/github/" + host + path
	case r.Host != "" && !strings.HasPrefix(r.Host, "127.0.0.1"):
		// gh following a link off GitHub, such as a log download.
		target = "https://" + r.Host + r.URL.EscapedPath()
	default:
		http.Error(w, "delta: not a GitHub URL", http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for k, vs := range r.Header {
		out.Header[k] = vs
	}
	out.Header.Del("Authorization")
	out.ContentLength = r.ContentLength
	if relayed {
		out.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	}
	resp, err := p.client.Do(out)
	if err != nil {
		http.Error(w, "delta: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
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

// runThroughRelay runs git or gh with the proxy in place, and exits with its status.
func runThroughRelay(cfg config, prog string, args []string) error {
	proxy := &githubProxy{cfg: cfg, client: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}

	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer tcp.Close()
	go http.Serve(tcp, proxy)
	port := tcp.Addr().(*net.TCPAddr).Port

	env := gitEnv(os.Environ(), fmt.Sprintf("http://127.0.0.1:%d/github.com/", port))
	if prog == "git" {
		// Keep assistant attribution out of commits (attribution.go).
		hooks, err := installAttributionHooks()
		if err != nil {
			return err
		}
		args = cloneConfig(args, hooks)
		env = hooksEnv(env, hooks)
	}

	cmd := exec.Command(prog, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// git, and the git gh runs for clones, go through the TCP proxy.
	cmd.Env = env
	if prog == "gh" {
		dir, err := os.MkdirTemp("", "delta-gh-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		sock := filepath.Join(dir, "gh.sock")
		unix, err := net.Listen("unix", sock)
		if err != nil {
			return err
		}
		defer unix.Close()
		go http.Serve(unix, proxy)
		conf := fmt.Sprintf("version: 1\ngit_protocol: https\nprompt: disabled\nhttp_unix_socket: %s\n", sock)
		if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(conf), 0o600); err != nil {
			return err
		}
		// The placeholder is dropped by the proxy; the command center sends the real one.
		cmd.Env = append(cmd.Env, "GH_CONFIG_DIR="+dir, "GH_TOKEN=delta-relay", "GITHUB_TOKEN=delta-relay")
	}
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	if errors.Is(err, exec.ErrNotFound) && prog == "gh" {
		return errors.New("gh isn't installed here; install it with `apt-get install -y gh`")
	}
	return err
}

// githubURLPrefixes are the ways a GitHub remote can be written. Each is
// rewritten to the proxy.
var githubURLPrefixes = []string{"https://github.com/", "http://github.com/", "git@github.com:", "ssh://git@github.com/"}

// gitEnv returns env with git told, through GIT_CONFIG_COUNT and its
// GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n pairs, to rewrite every GitHub remote
// to proxy.
//
// git applies one rewrite per URL (the longest matching prefix, the first
// defined on a tie), and never rewrites the result again. Claude Code cloud
// sessions already pass rewrites the same way (git@github.com: to
// https://github.com/, defined before ours), so on their own ours would lose
// for SSH-style remotes and git would reach GitHub directly. So the
// environment's own rewrites of GitHub prefixes are dropped, and everything
// else it sets is kept, in order.
func gitEnv(env []string, proxy string) []string {
	get := map[string]string{}
	var out []string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k == "GIT_CONFIG_COUNT" || strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			get[k] = v
			continue
		}
		out = append(out, kv)
	}
	n := 0
	add := func(k, v string) {
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n, k), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, v))
		n++
	}
	count, _ := strconv.Atoi(get["GIT_CONFIG_COUNT"])
	for i := 0; i < count; i++ {
		k, ok := get[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)]
		if !ok {
			continue
		}
		v := get[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)]
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "url.") && (strings.HasSuffix(lk, ".insteadof") || strings.HasSuffix(lk, ".pushinsteadof")) && slices.Contains(githubURLPrefixes, v) {
			continue
		}
		add(k, v)
	}
	for _, prefix := range githubURLPrefixes {
		add("url."+proxy+".insteadOf", prefix)
	}
	return append(out, fmt.Sprintf("GIT_CONFIG_COUNT=%d", n))
}
