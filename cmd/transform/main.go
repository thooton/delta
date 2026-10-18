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
	"image"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	case "start":
		if len(rest) != 1 {
			return errors.New("usage: transform start <key>")
		}
		return start(rest[0])
	case "get":
		return get(rest)
	case "build":
		if len(rest) != 1 {
			return errNoForm
		}
		return build(rest[0])
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
	pdf, err := save(".", false)
	if err != nil {
		return err
	}
	if pdf != "" {
		n, err := renderPages(pdf, pagesDir)
		if err != nil {
			return err
		}
		fmt.Printf("Drew its %s as %s/page-1.png and on: read those.\n", pages(n), pagesDir)
	}
	if err := update(map[string]any{"status": "Reading the document", "percent": 10}); err != nil {
		return err
	}
	fmt.Println("Next: run `transform help` and read all of it, then read the pages.")
	return nil
}

// Where start draws the document's pages, and build puts the result.
const (
	pagesDir   = "pages"
	resultDocx = "result.docx"
	resultPDF  = "result.pdf"
	previewDir = "result-pages"
)

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
	_, err := save(dir, original)
	return err
}

// save writes the document into dir as it looks on the tablet, or as it was
// uploaded, and returns the PDF of what is written on it ("" if nothing).
func save(dir string, original bool) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	k, err := key()
	if err != nil {
		return "", err
	}
	j, err := jobInfo(k)
	if err != nil {
		return "", err
	}
	data, err := call(k, http.MethodGet, "/transform/document", nil)
	if err != nil {
		return "", err
	}
	var id string
	files, err := remarkable.ReadZip(data)
	if err != nil {
		return "", fmt.Errorf("reading the document: %w", err)
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
	orig := ""
	if original || r.Type == "epub" || r.Unreadable != nil {
		if r.Original == nil {
			return "", errors.New("the document is a notebook: there is no original file")
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
			return "", err
		}
		if r.Type == "pdf" {
			orig = name
		}
	}
	for _, p := range r.Problems {
		fmt.Println(p)
	}
	if r.PDF == nil {
		fmt.Println("Nothing is written on it.")
		return orig, nil
	}
	if r.Separate {
		name := base + " (handwriting).pdf"
		return name, write(name, r.PDF, fmt.Sprintf("what the user wrote, alone, %s, each labelled with the page it is on", pages(r.Pages)))
	}
	what := fmt.Sprintf("as it looks on the tablet, %s, %d written on", pages(r.Pages), r.Written)
	if r.Type == "notebook" {
		what = "as it looks on the tablet, " + pages(r.Pages)
	}
	return base + ".pdf", write(base+".pdf", r.PDF, what)
}

func pages(n int) string {
	if n == 1 {
		return "1 page"
	}
	return fmt.Sprintf("%d pages", n)
}

// build makes the document from the form, by its format's template, and
// the PDF the tablet will get, drawn page by page for the agent to check.
// It keeps the user posted on its own.
func build(path string) error {
	f, err := readForm(path)
	if err != nil {
		return err
	}
	// The title first: the command center may refuse it, and say why.
	if err := update(map[string]any{"title": f.Title, "status": "Making the document", "percent": 60}); err != nil {
		return err
	}
	data, err := makeDocx(f, time.Now(), func(page int, box []float64) (image.Image, error) {
		return cropPage(pagesDir, page, box)
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(resultDocx, data, 0o644); err != nil {
		return err
	}
	pdf, err := do(http.MethodPost, "/transform/preview", data)
	if err != nil {
		return err
	}
	if err := os.WriteFile(resultPDF, pdf, 0o644); err != nil {
		return err
	}
	n, err := renderPages(resultPDF, previewDir)
	if err != nil {
		return err
	}
	if err := update(map[string]any{"status": "Checking the document", "percent": 85}); err != nil {
		return err
	}
	fmt.Printf("Built %s, and %s, the PDF the tablet will get: %s, drawn as %s/page-1.png and on.\n", resultDocx, resultPDF, pages(n), previewDir)
	if ph := placeholders(f); len(ph) > 0 {
		fmt.Printf("Placeholders for what the document doesn't give: %s.\n", strings.Join(ph, ", "))
	}
	fmt.Println("Read every page of it. If anything is wrong, fix the form and build again; when it is right, run `transform submit`.")
	return nil
}

// placeholders lists the fields the template fills with one.
func placeholders(f *form) []string {
	var out []string
	check := func(v, name string) {
		if strings.TrimSpace(v) == "" {
			out = append(out, "["+name+"]")
		}
	}
	switch f.Format {
	case "mla":
		check(f.Author, "Name")
		check(f.Instructor, "Instructor")
		check(f.Course, "Course")
		check(f.Heading, "Title")
	case "apa":
		check(f.Author, "Name")
		check(f.School, "School")
		check(f.Course, "Course")
		check(f.Instructor, "Instructor")
		check(f.Heading, "Title")
	}
	return out
}

// submit sends the last build, named by the job's title.
func submit(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: transform submit (it submits the last build)")
	}
	data, err := os.ReadFile(resultDocx)
	if err != nil {
		return errors.New("nothing is built yet: run `transform build <form.json>` first")
	}
	resp, err := do(http.MethodPost, "/transform/submit", data)
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

// update tells the user what the job is and how it is going.
func update(change map[string]any) error {
	body, _ := json.Marshal(change)
	data, err := do(http.MethodPost, "/transform/status", body)
	if err != nil {
		return err
	}
	var now struct {
		Title, Status string
		Percent       int
	}
	json.Unmarshal(data, &now)
	title := now.Title
	if title == "" {
		title = "(no title yet)"
	}
	line := now.Status
	if line == "" {
		line = "(no status yet)"
	}
	fmt.Printf("The user now sees: %s · %s · %d%%\n", title, line, now.Percent)
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
