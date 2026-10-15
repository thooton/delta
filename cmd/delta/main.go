// Command delta is the CLI that agents use to talk to the Delta command center.
// Run `delta help` for the full description.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

const helpText = `delta: the CLI for the Delta command center

WHAT THIS IS
  You are a Claude Code cloud session started from the Delta app on the
  user's Sonim XP3 flip phone. The app has three tabs: Chats (their
  sessions), Session (this one) and Files (the files on the command
  center, which they can download, install, open, move into a folder on
  the phone such as Music or Download, upload from the phone, or delete).
  The command center is an always-on server; this CLI talks to it.

  FILES FROM THE PHONE: the user can send you files. The app uploads each
  one to the command center and ends their message with a block like

    <delta-files>
    ...
    - notes.pdf
    </delta-files>

  Fetch each named file with "delta pull <name>" before working on it.
  The phone shows that block to the user only as "Attached: notes.pdf".

  Repositories (see GITHUB):
    thooton/sonim   The phone's apps, one directory each (the Delta app is
                    sonim/delta).
    thooton/delta   Delta itself: command center (cmd/delta-cc) and CLI.

  A typical job: fix a bug in a phone app in sonim, commit and push, then
  build the release APK and push it named after the app:

    (cd list/android && ./gradlew :app:assembleRelease)
    delta push list/android/app/build/outputs/apk/release/app-release.apk list.apk

  (If list.apk is already there, push it as list-2.apk and so on.)

  Always push release builds; debug builds run slowly on the phone. It
  shows up in the Files tab, and installing it updates the app in place
  because every build is signed with the key in sonim/signing. Never
  change or bypass that key. Then tell the user what to install.
  (The Delta app is delta.apk; a build without its logins keeps the
  installed copy's.)

SETUP
  Your first message included a command-center URL and token. Run first:

    delta set-auth <url> <token>

  The token is a password: never repeat or commit it. If the server can't
  be reached, its host probably needs adding to the cloud environment's
  allowed hosts; tell the user.

COMMANDS
  delta set-auth <url> <token>       Verify and save the URL and token.
  delta push <path> [name]           Upload a file. A name already taken is
                                     refused: pick a different one.
  delta pull <name> [dest]           Download a file, e.g. one the user
                                     sent from the phone.
  delta list                         List files.
  delta delete <name>                Delete a file.
  delta git <args>                   Run git with the user's GitHub token.
  delta gh <args>                    Run gh with the user's GitHub token.
  delta set-personal-access-token <token|->
                                     Check and save a new GitHub token ("-"
                                     reads stdin).
  delta update-cc                    Update the command center (see below).
  delta canvas ...                   The user's Canvas (they are a student).
  delta music ...                    Purchase and download a song or an album.
  delta books ...                    Find and download ebooks.
  delta maps ...                     Find places (restaurants, stores, ...)
                                     and their hours with Google Maps.
  delta gmail ...                    The user's Gmail: search, read, send,
                                     reply, forward, archive, trash, label.
  delta docx                         How to make Word documents (.docx).
  delta docx extract [dir]           Extract the scripts that guide uses.

  Read "delta canvas|music|books|maps|gmail help" before using those, and
  "delta docx" before making or editing a Word document.
  Aliases: ls = list, rm = delete, get = pull.

GITHUB
  The user has given you access to their repositories through a GitHub
  personal access token kept on the command center. You never see it: use
  "delta git" and "delta gh" in place of git and gh for anything that
  talks to GitHub, and the command center adds the token to their requests
  on the way:

    delta git clone https://github.com/thooton/sonim
    delta git push
    delta gh run watch -R thooton/delta

  gh may need installing first. If GitHub says the token is missing or
  expired, ask the user for a new one and save it with
  set-personal-access-token (it can't be read back afterwards).

UPDATING THE COMMAND CENTER
  Push the change to master in thooton/delta, wait for its GitHub Actions
  build, then run:

    delta update-cc

  It takes no link: the command center always installs the latest
  official build of itself from the thooton/delta releases, and nothing
  else. The new binary must pass its self-test (cmd/delta-cc/selftest.go; keep it
  up to date) or nothing is installed. If the old and new versions printed
  match, the build hadn't finished yet.

FILES
  Config is in ~/.config/delta/ ($XDG_CONFIG_HOME). DELTA_SERVER and
  DELTA_TOKEN override it.
`

type config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "delta:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(helpText)
		return nil
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(helpText)
		return nil
	case "set-auth":
		if len(args) != 2 {
			return errors.New("usage: delta set-auth <command-center-url> <token>")
		}
		return setAuth(args[0], args[1])
	case "push":
		if len(args) < 1 || len(args) > 2 {
			return errors.New("usage: delta push <path> [name]")
		}
		name := filepath.Base(args[0])
		if len(args) == 2 {
			name = args[1]
		}
		return push(args[0], name)
	case "pull", "get":
		if len(args) < 1 || len(args) > 2 {
			return errors.New("usage: delta pull <name> [dest]")
		}
		dest := args[0]
		if len(args) == 2 {
			dest = args[1]
		}
		return pull(args[0], dest)
	case "canvas":
		return runCanvas(args)
	case "music":
		return runMusic(args)
	case "books":
		return runBooks(args)
	case "maps":
		return runMaps(args)
	case "gmail", "mail", "email":
		return runGmail(args)
	case "docx":
		return runDocx(args)
	case "list", "ls":
		if len(args) != 0 {
			return errors.New("usage: delta list")
		}
		return list()
	case "delete", "rm":
		if len(args) != 1 {
			return errors.New("usage: delta delete <name>")
		}
		return del(args[0])
	case "get-personal-access-token":
		return errors.New("the GitHub token can't be read back from the command center; use `delta git` and `delta gh`, which add it for you")
	case "set-personal-access-token":
		if len(args) != 1 {
			return errors.New("usage: delta set-personal-access-token <token|->")
		}
		return setAccessToken(args[0])
	case "git", "gh":
		return runWithAccessToken(cmd, args)
	case "update-cc":
		if len(args) != 0 {
			return errors.New("usage: delta update-cc (it takes no link: the command center always installs the latest official build)")
		}
		return updateCC()
	default:
		return fmt.Errorf("unknown command %q (run `delta help`)", cmd)
	}
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "delta", "config.json"), nil
}

func loadConfig() (config, error) {
	var c config
	if path, err := configPath(); err == nil {
		if data, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(data, &c); err != nil {
				return c, fmt.Errorf("reading %s: %w", path, err)
			}
		}
	}
	if v := os.Getenv("DELTA_SERVER"); v != "" {
		c.Server = v
	}
	if v := os.Getenv("DELTA_TOKEN"); v != "" {
		c.Token = v
	}
	if c.Server == "" || c.Token == "" {
		return c, errors.New("not authenticated; run `delta set-auth <command-center-url> <token>` first")
	}
	return c, nil
}

func setAuth(server, token string) error {
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid command-center URL %q (expected http(s)://host[:port])", server)
	}
	c := config{Server: strings.TrimRight(server, "/"), Token: token}
	if _, err := do(c, http.MethodGet, "/auth", nil); err != nil {
		return fmt.Errorf("could not verify token: %w", err)
	}

	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	fmt.Printf("Authenticated with %s. Saved to %s.\n", c.Server, path)
	return nil
}

func push(path, name string) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}

	body, err := do(c, http.MethodPut, "/files/"+url.PathEscape(name), f, info.Size())
	if err != nil {
		return err
	}
	var res struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	fmt.Printf("Pushed %s (%s).\n", res.Name, humanSize(res.Size))
	return nil
}

func pull(name, dest string) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		dest = filepath.Join(dest, filepath.Base(name))
	}
	req, err := http.NewRequest(http.MethodGet, c.Server+"/files/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("server: %s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	fmt.Printf("Saved %s (%s).\n", dest, humanSize(n))
	return nil
}

func list() error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	body, err := do(c, http.MethodGet, "/files", nil)
	if err != nil {
		return err
	}
	var files []struct {
		Name     string    `json:"name"`
		Size     int64     `json:"size"`
		Modified time.Time `json:"modified"`
	}
	if err := json.Unmarshal(body, &files); err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Println("No files.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSIZE\tMODIFIED")
	for _, f := range files {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", f.Name, humanSize(f.Size), f.Modified.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

func del(name string) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if _, err := do(c, http.MethodDelete, "/files/"+url.PathEscape(name), nil); err != nil {
		return err
	}
	fmt.Printf("Deleted %s.\n", name)
	return nil
}

// updateCC has the command center install the latest official build of
// itself. It picks the build; the URL sent along is only for command
// centers older than that, which required one.
func updateCC() error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	req, _ := json.Marshal(map[string]string{"url": "https://github.com/thooton/delta/releases/download/latest/delta-cc-linux-amd64"})
	body, err := do(c, http.MethodPost, "/update", bytes.NewReader(req))
	if err != nil {
		return err
	}
	var res struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	fmt.Printf("Installed delta-cc %s (was %s). Waiting for restart...\n", res.New, res.Old)
	if res.Old == res.New {
		fmt.Println("Warning: the new binary has the same version as the old one.")
	}

	// Poll until the restarted server answers with the new version.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		body, err := do(c, http.MethodGet, "/version", nil)
		if err != nil {
			continue
		}
		var v struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(body, &v) == nil && v.Version == res.New {
			fmt.Printf("Command center is running %s.\n", v.Version)
			return nil
		}
	}
	return errors.New("command center did not come back with the new version within 30s")
}

// do sends an authenticated request and returns the response body, turning
// non-2xx responses into errors. An optional content length may follow body.
func do(c config, method, path string, body io.Reader, contentLength ...int64) ([]byte, error) {
	resp, err := stream(c, method, path, body, contentLength...)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// stream is do for responses read as they arrive: it returns the response
// for the caller to read and close.
func stream(c config, method, path string, body io.Reader, contentLength ...int64) (*http.Response, error) {
	req, err := http.NewRequest(method, c.Server+path, body)
	if err != nil {
		return nil, err
	}
	if len(contentLength) > 0 {
		req.ContentLength = contentLength[0]
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("server: %s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return nil, fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
