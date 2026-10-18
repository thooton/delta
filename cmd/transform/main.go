// transform: the CLI of a transform job's cloud session (see
// cmd/delta-cc/transform.go). The session gets one document from the user's
// reMarkable, makes the new document its instructions ask for, and submits
// it, which puts it on the tablet. The job's key, in the session's first
// message, reaches that one document and nothing else on the command center,
// whose address is in TRANSFORM_SERVER.
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
	"time"

	"github.com/thooton/delta/internal/docx"
	"github.com/thooton/delta/internal/remarkable"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "transform:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(help)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(help)
		return nil
	case "docx":
		return runDocx(rest)
	case "start":
		if len(rest) != 1 {
			return errors.New("usage: transform start <key>")
		}
		return start(rest[0])
	case "get":
		return get(rest)
	case "preview":
		return preview(rest)
	case "submit":
		return submit(rest)
	case "fail":
		if len(rest) == 0 || strings.TrimSpace(strings.Join(rest, " ")) == "" {
			return errors.New(`usage: transform fail "<why>"`)
		}
		return fail(strings.Join(rest, " "))
	}
	return fmt.Errorf("unknown command %q (run `transform help`)", cmd)
}

// ---------------------------------------------------------------------------
// The command center
// ---------------------------------------------------------------------------

func keyFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "transform", "key"), nil
}

func server() (string, error) {
	s := strings.TrimRight(os.Getenv("TRANSFORM_SERVER"), "/")
	if s == "" {
		return "", errors.New("TRANSFORM_SERVER isn't set: this session wasn't started as a transform job")
	}
	return s, nil
}

func key() (string, error) {
	path, err := keyFile()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("run `transform start <key>` first, with the key from your first message")
	}
	return strings.TrimSpace(string(data)), nil
}

var client = &http.Client{Timeout: 5 * time.Minute}

// call makes a request with the job's key and returns the body of a 2xx
// response, or the command center's error.
func call(k, method, path string, body []byte) ([]byte, error) {
	base, err := server()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach the command center: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct{ Error string }
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, errors.New(e.Error)
		}
		return nil, fmt.Errorf("the command center answered HTTP %d", resp.StatusCode)
	}
	return data, nil
}

func do(method, path string, body []byte) ([]byte, error) {
	k, err := key()
	if err != nil {
		return nil, err
	}
	return call(k, method, path, body)
}

type job struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Pages int    `json:"pages"`
}

func jobInfo(k string) (job, error) {
	var j job
	data, err := call(k, http.MethodGet, "/transform/job", nil)
	if err == nil {
		err = json.Unmarshal(data, &j)
	}
	return j, err
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

func start(k string) error {
	k = strings.TrimSpace(k)
	j, err := jobInfo(k)
	if err != nil {
		return err
	}
	path, err := keyFile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(k+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Printf("This job transforms %q, a %s on the user's tablet.\n", j.Name, kindName(j.Type))
	if err := save(".", false); err != nil {
		return err
	}
	fmt.Println("Next: run `transform help` and read all of it, then read the document.")
	return nil
}

func kindName(t string) string {
	switch t {
	case "pdf":
		return "PDF"
	case "epub":
		return "EPUB book"
	}
	return "notebook"
}

func get(args []string) error {
	var pos []string
	original := false
	for _, a := range args {
		switch {
		case a == "--original":
			original = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown option %s", a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) > 1 {
		return errors.New("usage: transform get [dir] [--original]")
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	return save(dir, original)
}

// save writes the document into dir as it looks on the tablet, or as it was
// uploaded.
func save(dir string, original bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	k, err := key()
	if err != nil {
		return err
	}
	j, err := jobInfo(k)
	if err != nil {
		return err
	}
	data, err := call(k, http.MethodGet, "/transform/document", nil)
	if err != nil {
		return err
	}
	var id string
	files, err := remarkable.ReadZip(data)
	if err != nil {
		return fmt.Errorf("reading the document: %w", err)
	}
	for name := range files {
		if strings.HasSuffix(name, ".content") && !strings.Contains(name, "/") {
			id = strings.TrimSuffix(name, ".content")
		}
	}
	r := remarkable.Render(id, files)
	base := filepath.Join(dir, remarkable.SafeFileName(j.Name))
	write := func(name string, data []byte, what string) error {
		if err := os.WriteFile(name, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("Saved %s (%s).\n", name, what)
		return nil
	}
	if original || r.Type == "epub" || r.Unreadable != nil {
		if r.Original == nil {
			return errors.New("the document is a notebook: there is no original file")
		}
		name := base + "." + r.Type
		if original && r.Type == "pdf" {
			name = base + " (original).pdf"
		}
		what := "the " + strings.ToUpper(r.Type) + " as uploaded, before anything was written on it"
		if r.Unreadable != nil {
			what = fmt.Sprintf("the PDF as uploaded: it couldn't be read to draw on it, %v", r.Unreadable)
		}
		if err := write(name, r.Original, what); err != nil || original {
			return err
		}
	}
	for _, p := range r.Problems {
		fmt.Println(p)
	}
	if r.PDF == nil {
		fmt.Println("Nothing is written on it.")
		return nil
	}
	if r.Separate {
		return write(base+" (handwriting).pdf", r.PDF, fmt.Sprintf("what the user wrote, alone, %s, each labelled with the page it is on", pages(r.Pages)))
	}
	what := fmt.Sprintf("as it looks on the tablet, %s, %d written on", pages(r.Pages), r.Written)
	if r.Type == "notebook" {
		what = "as it looks on the tablet, " + pages(r.Pages)
	}
	return write(base+".pdf", r.PDF, what)
}

func pages(n int) string {
	if n == 1 {
		return "1 page"
	}
	return fmt.Sprintf("%d pages", n)
}

func preview(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: transform preview <file.docx> [out.pdf]")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	out := strings.TrimSuffix(args[0], filepath.Ext(args[0])) + " (preview).pdf"
	if len(args) == 2 {
		out = args[1]
	}
	pdf, err := do(http.MethodPost, "/transform/preview", data)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, pdf, 0o644); err != nil {
		return err
	}
	fmt.Printf("Saved %s: the PDF the tablet will get for %s. Read it.\n", out, args[0])
	return nil
}

func submit(args []string) error {
	var pos []string
	name := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--name" && i+1 < len(args):
			i++
			name = args[i]
		case strings.HasPrefix(a, "--name="):
			name = strings.TrimPrefix(a, "--name=")
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown option %s", a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) != 1 {
		return errors.New("usage: transform submit <file.pdf|file.docx> [--name NAME]")
	}
	data, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	resp, err := do(http.MethodPost, "/transform/submit?name="+url.QueryEscape(name), data)
	if err != nil {
		return err
	}
	var r struct {
		Name  string   `json:"name"`
		Files []string `json:"files"`
	}
	json.Unmarshal(resp, &r)
	fmt.Printf("Submitted %q. It is going onto the user's tablet, next to the original, and into their files to download (%s). The job is done: say in a sentence what you made.\n", r.Name, strings.Join(r.Files, ", "))
	return nil
}

func fail(why string) error {
	body, _ := json.Marshal(map[string]string{"reason": strings.TrimSpace(why)})
	if _, err := do(http.MethodPost, "/transform/fail", body); err != nil {
		return err
	}
	fmt.Println("The job is marked as failed, and the user will see why. Nothing more to do.")
	return nil
}

func runDocx(args []string) error {
	if len(args) == 0 || args[0] == "help" {
		fmt.Print(docx.Help("transform docx"))
		return nil
	}
	if args[0] != "extract" || len(args) > 2 {
		return errors.New("usage: transform docx [extract [dir]]")
	}
	dir := "."
	if len(args) == 2 {
		dir = args[1]
	}
	n, err := docx.Extract(dir)
	if err != nil {
		return err
	}
	fmt.Printf("extracted %d files into %s: scripts/ and its LICENSE\n", n, dir)
	return nil
}
