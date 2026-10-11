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
  The user carries a Sonim XP3 flip phone running the Delta app. You (the
  agent) are a Claude Code cloud session started from that app. Delta has
  three parts:

    PHONE           The Delta app on the user's phone. It has three tabs:
                      Chats    the user's cloud sessions, this one included
                      Session  this conversation
                      Files    every file on the command center; the user
                               downloads, installs (APKs) or deletes them
    COMMAND CENTER  A server on a VPS that is always running. It relays
                    between the phone and everything else and requires an
                    auth token on every request.
    CLI             This program. You use it to send things to the command
                    center, where the user can get them from their phone.

  The user reads your replies on a 240x320 screen with a keypad, so keep
  them short and plain. They cannot easily copy long output or type long
  answers.

  Relevant repositories (you have read/write access):
    https://github.com/thooton/sonim   The apps installed on the phone, one
                                       directory each. The Delta app itself
                                       is sonim/delta.
    https://github.com/thooton/delta   Delta itself (command center + CLI).

  A typical job: the user reports a bug in a phone app. Clone the sonim
  repo, fix it, commit and push, build the APK, then push it named after
  the app, for example:

    delta push list/android/app/build/outputs/apk/debug/app-debug.apk list.apk

  Every app builds an "app-debug.apk", so always give a name: pushing the
  same name again replaces the old file. The APK appears in the Files tab on
  the user's phone, where they choose whether to download and install it.
  Installing it updates the app in place and keeps its data, as long as it
  was built with the shared key in sonim/signing (the Gradle builds use it
  already). Then tell the user, briefly, what to install.

  If the bug is in the Delta app, fix it in sonim/delta and push the APK
  the same way (as delta.apk). A build made without its logins keeps the
  ones the installed copy already has.

SETUP
  The first message you got from the Delta app includes a command-center
  URL and an auth token. Before any other command, run:

    delta set-auth <command-center-url> <token>

  This checks the token with the server and saves it to the config file
  (see FILES below), so later commands don't need it again. The token is a
  password: never repeat it to the user or commit it anywhere. If set-auth
  cannot reach the server at all, the sandbox's network is probably
  blocking the command center's host; tell the user it needs adding to the
  cloud environment's allowed hosts.

COMMANDS
  delta help                         Show this text.
  delta set-auth <url> <token>       Save and verify the server URL and token.
  delta push <path> [name]           Upload a file. It is stored as [name]
                                     (default: the file's base name). A file
                                     with the same name is replaced.
  delta pull <name> [dest]           Download a file from the command center
                                     (for example a photo the user sent from
                                     the phone).
  delta list                         List the files on the command center.
  delta delete <name>                Delete a file from the command center.
  delta update-cc <binary-url>       Make the command center download a new
                                     delta-cc binary from <binary-url>,
                                     replace itself, and restart. See
                                     UPDATING THE COMMAND CENTER below.

  delta canvas <command> ...         Use the user's Canvas: courses, due
                                     dates, assignments, attachments,
                                     submissions, discussions, quizzes,
                                     grades, messages. Run "delta canvas help".

  Aliases: "ls" for list, "rm" for delete, "get" for pull.

CANVAS
  The user is a student who may not have a computer, so you are their way
  into Canvas. "delta canvas help" explains everything. Read it before
  doing anything with Canvas.

UPDATING THE COMMAND CENTER
  The command center never updates on its own. To ship a fix to it:

    1. Commit and push the change to master in thooton/delta.
    2. Wait for the GitHub Actions build for that commit to finish
       (for example: gh run watch -R thooton/delta). It publishes
       the binaries to the "latest" release.
    3. Run:
         delta update-cc https://github.com/thooton/delta/releases/download/latest/delta-cc-linux-amd64

  Before replacing itself, the server runs "<new-binary> test", the
  self-test in cmd/delta-cc/selftest.go. If the download isn't a delta-cc
  binary, can't run on the server, or fails the test, nothing is installed
  and the old version keeps running; the error includes the test output.
  If you change command center behavior, update the self-test to match.

  The command prints the old and new versions (git commit hashes). If they
  match, the release build probably hadn't finished yet.

FILES
  Config: $XDG_CONFIG_HOME/delta/config.json (usually ~/.config/delta/).
  You can set DELTA_SERVER and DELTA_TOKEN in the environment instead.
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
	case "update-cc":
		if len(args) != 1 {
			return errors.New("usage: delta update-cc <binary-url>")
		}
		return updateCC(args[0])
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
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		Replaced bool   `json:"replaced"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	verb := "Pushed"
	if res.Replaced {
		verb = "Replaced"
	}
	fmt.Printf("%s %s (%s).\n", verb, res.Name, humanSize(res.Size))
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

func updateCC(binaryURL string) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	req, _ := json.Marshal(map[string]string{"url": binaryURL})
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
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("server: %s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return nil, fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	return data, nil
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
