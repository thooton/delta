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
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
	case "title":
		if len(rest) == 0 {
			return errors.New(`usage: transform title "<a few words>"`)
		}
		return update(map[string]any{"title": strings.Join(rest, " ")})
	case "status":
		return status(rest)
	case "percent":
		if len(rest) != 1 {
			return errors.New("usage: transform percent <0-100>")
		}
		n, err := percent(rest[0])
		if err != nil {
			return err
		}
		return update(map[string]any{"percent": n})
	case "shorthand":
		return shorthand(rest)
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
	return callWith(client, k, method, path, "", body)
}

// callWith is call through c, for a request that may take longer than
// client allows, with the body's Content-Type.
func callWith(c *http.Client, k, method, path, contentType string, body []byte) ([]byte, error) {
	base, err := server()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach the command center: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, serverError(resp.StatusCode, data)
	}
	return data, nil
}

// serverError is the error for a response that isn't a success: the command
// center's own, or its status.
func serverError(status int, data []byte) error {
	var e struct{ Error string }
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		return errors.New(e.Error)
	}
	return fmt.Errorf("the command center answered HTTP %d", status)
}

func do(method, path string, body []byte) ([]byte, error) {
	k, err := key()
	if err != nil {
		return nil, err
	}
	return call(k, method, path, body)
}

type job struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Formats []string `json:"formats"`
}

// formatNames are the formats a job makes, as the user is told of them.
var formatNames = map[string]string{
	"pdf":  "PDF",
	"docx": "Word document",
	"md":   "Markdown",
	"txt":  "plain text",
	"eml":  "email",
	"fkv":  "flashcards",
	"jpg":  "JPG images of the pages",
}

// formatsInWords lists formats: "PDF, Markdown and plain text".
func formatsInWords(formats []string) string {
	var names []string
	for _, f := range formats {
		names = append(names, formatNames[f])
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
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
	if len(j.Formats) > 0 {
		fmt.Printf("The user asked for: %s. Make exactly these (FORMATS in `transform help`).\n", formatsInWords(j.Formats))
	}
	if err := save(k, j, ".", false); err != nil {
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
	k, err := key()
	if err != nil {
		return err
	}
	j, err := jobInfo(k)
	if err != nil {
		return err
	}
	return save(k, j, dir, original)
}

// save writes the document into dir as it looks on the tablet, or as it was
// uploaded.
func save(k string, j job, dir string, original bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

const submitUsage = "usage: transform submit [file.pdf] [file.docx] [file.md] [file.txt] [file.eml] [file.fkv] [page images.jpg ...] [--name NAME]"

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
	if len(pos) < 1 {
		return errors.New(submitUsage)
	}
	// Each file's format, from its extension; the images in page order.
	paths := map[string][]string{}
	for _, path := range pos {
		f := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
		if f == "jpeg" {
			f = "jpg"
		}
		if _, ok := formatNames[f]; !ok {
			return fmt.Errorf("%s isn't a .pdf, .docx, .md, .txt, .eml, .fkv or .jpg (%s)", path, submitUsage)
		}
		if f != "jpg" && len(paths[f]) > 0 {
			return fmt.Errorf("submit one .%s, not several", f)
		}
		paths[f] = append(paths[f], path)
	}
	sort.SliceStable(paths["jpg"], func(a, b int) bool { return naturalLess(paths["jpg"][a], paths["jpg"][b]) })
	k, err := key()
	if err != nil {
		return err
	}
	// The command center checks too; this says it before sending anything.
	// A job from before there was a choice always makes a PDF.
	j, jobErr := jobInfo(k)
	if (jobErr != nil || len(j.Formats) == 0 || slices.Contains(j.Formats, "pdf")) && len(paths["pdf"]) == 0 {
		if docs := paths["docx"]; len(docs) > 0 {
			out := strings.TrimSuffix(docs[0], filepath.Ext(docs[0])) + ".pdf"
			return fmt.Errorf("submit the PDF with the Word document: it goes on the tablet. Convert it here (soffice --headless --convert-to pdf --outdir %q %q), read %s to check it, then submit it with the rest",
				filepath.Dir(out), docs[0], out)
		}
		return errors.New("submit a PDF: it goes on the tablet (" + submitUsage + ")")
	}
	if jobErr == nil && len(j.Formats) > 0 {
		want := map[string]bool{}
		for _, f := range j.Formats {
			want[f] = true
			if len(paths[f]) == 0 {
				return fmt.Errorf("the user asked for %s: make the %s too, and submit everything at once", formatsInWords(j.Formats), formatNames[f])
			}
		}
		for f := range paths {
			if !want[f] {
				return fmt.Errorf("the user asked only for %s: leave out the %s", formatsInWords(j.Formats), formatNames[f])
			}
		}
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, f := range []string{"pdf", "docx", "md", "txt", "eml", "fkv", "jpg"} {
		for _, path := range paths[f] {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if f == "pdf" && !bytes.HasPrefix(data, []byte("%PDF-")) {
				return fmt.Errorf("%s isn't a PDF", path)
			}
			part, err := form.CreateFormFile(f, filepath.Base(path))
			if err != nil {
				return err
			}
			part.Write(data)
		}
	}
	form.Close()
	resp, err := callWith(client, k, http.MethodPost, "/transform/submit?name="+url.QueryEscape(name), form.FormDataContentType(), body.Bytes())
	if err != nil {
		return err
	}
	var r struct {
		Name  string   `json:"name"`
		Files []string `json:"files"`
	}
	json.Unmarshal(resp, &r)
	onto := "Everything is going"
	if len(paths["pdf"]) > 0 {
		onto = "The PDF is going onto the user's tablet, next to the original, and everything"
	}
	fmt.Printf("Submitted %q. %s into their files to download (%s). The job is done: say in a sentence what you made.\n", r.Name, onto, strings.Join(r.Files, ", "))
	return nil
}

// naturalLess orders names as people number them: "page 2" before "page 10".
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		da, db := digits(a), digits(b)
		if da != "" && db != "" {
			na, nb := strings.TrimLeft(da, "0"), strings.TrimLeft(db, "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = a[len(da):], b[len(db):]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

// digits is the run of digits s starts with.
func digits(s string) string {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return s[:n]
}

// percent reads a percentage: "40" or "40%".
func percent(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if err != nil || n < 0 || n > 100 {
		return 0, fmt.Errorf("%q isn't a percentage from 0 to 100", s)
	}
	return n, nil
}

// status sets the status line, and the percentage if the last word is one.
func status(args []string) error {
	if len(args) == 0 {
		return errors.New(`usage: transform status "<what you're doing>" [percent]`)
	}
	change := map[string]any{}
	if len(args) > 1 {
		if n, err := percent(args[len(args)-1]); err == nil {
			change["percent"] = n
			args = args[:len(args)-1]
		}
	}
	change["status"] = strings.Join(args, " ")
	return update(change)
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
