package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

	cmd := exec.Command(prog, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// git, and the git gh runs for clones, go through the TCP proxy.
	n, _ := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", n+1),
		fmt.Sprintf("GIT_CONFIG_KEY_%d=url.http://127.0.0.1:%d/github.com/.insteadOf", n, port),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=https://github.com/", n),
	)
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
