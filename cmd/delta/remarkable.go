package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// delta remarkable: the user's reMarkable tablet, through the command
// center (see cmd/delta-cc/remarkable.go). Documents and folders are named by
// their path ("School/Math/Worksheet 3") or by id.

type rmItem struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Type     string    `json:"type"`
	Parent   string    `json:"parent"`
	Pages    int       `json:"pages"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type rmTree struct {
	items map[string]*rmItem
}

func runRemarkable(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(remarkableHelp)
		return nil
	}
	cmd, args := args[0], args[1:]
	c, err := loadConfig()
	if err != nil {
		return err
	}
	switch cmd {
	case "ls", "list":
		return rmList(c, args)
	case "upload", "put", "push":
		return rmUpload(c, args)
	case "get", "download", "pull":
		return rmGet(c, args)
	case "mkdir":
		if len(args) != 1 {
			return errors.New("usage: delta remarkable mkdir <folder>")
		}
		t, err := rmLoad(c)
		if err != nil {
			return err
		}
		_, err = t.mkdirAll(c, args[0])
		return err
	case "mv", "move", "rename":
		return rmMove(c, args)
	case "rm", "trash", "delete":
		if len(args) != 1 {
			return errors.New("usage: delta remarkable rm <document|folder>")
		}
		t, err := rmLoad(c)
		if err != nil {
			return err
		}
		it, err := t.find(args[0])
		if err != nil {
			return err
		}
		if err := rmPatch(c, it.ID, map[string]string{"parent": "trash"}); err != nil {
			return err
		}
		fmt.Printf("Moved %s to the trash.\n", t.path(it))
		return nil
	default:
		return fmt.Errorf("unknown command %q (run `delta remarkable help`)", cmd)
	}
}

func rmLoad(c config) (*rmTree, error) {
	body, err := do(c, http.MethodGet, "/remarkable", nil)
	if err != nil {
		return nil, err
	}
	var items []*rmItem
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, err
	}
	t := &rmTree{items: map[string]*rmItem{}}
	for _, it := range items {
		t.items[it.ID] = it
	}
	return t, nil
}

// path is where an item is, "trash/..." for the trash.
func (t *rmTree) path(it *rmItem) string {
	parts := []string{it.Name}
	for p, n := it.Parent, 0; p != "" && n < 100; n++ {
		if p == "trash" {
			parts = append(parts, "trash")
			break
		}
		parent := t.items[p]
		if parent == nil {
			break
		}
		parts = append(parts, parent.Name)
		p = parent.Parent
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "/")
}

func (t *rmTree) inTrash(it *rmItem) bool {
	return strings.HasPrefix(t.path(it), "trash/")
}

// find looks an item up by id or path, case-insensitively if need be.
func (t *rmTree) find(name string) (*rmItem, error) {
	if it, ok := t.items[name]; ok {
		return it, nil
	}
	want := strings.Trim(name, "/")
	for _, fold := range []bool{false, true} {
		var found []*rmItem
		for _, it := range t.items {
			p := t.path(it)
			if p == want || fold && strings.EqualFold(p, want) {
				found = append(found, it)
			}
		}
		if len(found) == 1 {
			return found[0], nil
		}
		if len(found) > 1 {
			var b strings.Builder
			fmt.Fprintf(&b, "%d items are called %q; use an id:", len(found), want)
			for _, it := range found {
				fmt.Fprintf(&b, "\n  %s  %s, modified %s", it.ID, it.Type, it.Modified.Local().Format("2006-01-02 15:04"))
			}
			return nil, errors.New(b.String())
		}
	}
	return nil, fmt.Errorf("nothing called %q on the reMarkable (see `delta remarkable ls`)", name)
}

// folder looks up a folder; "" and "/" are the top level.
func (t *rmTree) folder(name string) (string, error) {
	if strings.Trim(name, "/") == "" {
		return "", nil
	}
	it, err := t.find(name)
	if err != nil {
		return "", err
	}
	if it.Type != "folder" {
		return "", fmt.Errorf("%s is a document, not a folder", t.path(it))
	}
	return it.ID, nil
}

func (t *rmTree) children(parent string) []*rmItem {
	var out []*rmItem
	for _, it := range t.items {
		if it.Parent == parent {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == "folder") != (out[j].Type == "folder") {
			return out[i].Type == "folder"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func rmList(c config, args []string) error {
	root, trash := "", false
	for _, a := range args {
		switch {
		case a == "--trash":
			trash = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown option %s", a)
		default:
			root = a
		}
	}
	t, err := rmLoad(c)
	if err != nil {
		return err
	}
	start := ""
	if trash {
		start = "trash"
	} else if root != "" {
		if start, err = t.folder(root); err != nil {
			return err
		}
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	n := 0
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, it := range t.children(parent) {
			n++
			name := strings.Repeat("  ", depth) + it.Name
			kind := it.Type
			if it.Type == "folder" {
				name += "/"
			} else if it.Pages > 0 {
				kind = fmt.Sprintf("%s, %d page%s", it.Type, it.Pages, plural(it.Pages))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, kind, it.Modified.Local().Format("2006-01-02 15:04"), it.ID)
			if it.Type == "folder" && depth < 50 {
				walk(it.ID, depth+1)
			}
		}
	}
	walk(start, 0)
	if n == 0 {
		if trash {
			fmt.Println("The trash is empty.")
		} else {
			fmt.Println("Nothing here.")
		}
		return nil
	}
	return tw.Flush()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func rmUpload(c config, args []string) error {
	var name string
	var pos []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--name" && i+1 < len(args) {
			name = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "-") {
			return fmt.Errorf("unknown option %s", args[i])
		} else {
			pos = append(pos, args[i])
		}
	}
	if len(pos) < 1 || len(pos) > 2 {
		return errors.New("usage: delta remarkable upload <file.pdf|file.epub> [folder] [--name NAME]")
	}
	data, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(pos[0]), filepath.Ext(pos[0]))
	}
	t, err := rmLoad(c)
	if err != nil {
		return err
	}
	parent := ""
	if len(pos) == 2 {
		if parent, err = t.folder(pos[1]); err != nil {
			return err
		}
	}
	q := url.Values{"name": {name}, "parent": {parent}}
	if _, err := do(c, http.MethodPost, "/remarkable/documents?"+q.Encode(), bytes.NewReader(data), int64(len(data))); err != nil {
		return err
	}
	where := "the top level"
	if parent != "" {
		where = t.path(t.items[parent])
	}
	fmt.Printf("Put %q in %s on the reMarkable (%s).\n", name, where, humanSize(int64(len(data))))
	return nil
}

func (t *rmTree) mkdirAll(c config, p string) (string, error) {
	parent := ""
	made := false
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == "" {
			continue
		}
		var next string
		for _, it := range t.children(parent) {
			if it.Name == part {
				if it.Type != "folder" {
					return "", fmt.Errorf("%s is a document, not a folder", t.path(it))
				}
				next = it.ID
				break
			}
		}
		if next == "" {
			req, _ := json.Marshal(map[string]string{"name": part, "parent": parent})
			body, err := do(c, http.MethodPost, "/remarkable/folders", bytes.NewReader(req))
			if err != nil {
				return "", err
			}
			var res struct{ ID string }
			json.Unmarshal(body, &res)
			next = res.ID
			t.items[next] = &rmItem{ID: next, Name: part, Type: "folder", Parent: parent}
			made = true
		}
		parent = next
	}
	if made {
		fmt.Printf("Made %s.\n", strings.Trim(p, "/"))
	} else {
		fmt.Printf("%s already exists.\n", strings.Trim(p, "/"))
	}
	return parent, nil
}

func rmPatch(c config, id string, change map[string]string) error {
	req, _ := json.Marshal(change)
	_, err := do(c, http.MethodPatch, "/remarkable/"+url.PathEscape(id), bytes.NewReader(req))
	return err
}

func rmMove(c config, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: delta remarkable mv <document|folder> <folder|new path>")
	}
	t, err := rmLoad(c)
	if err != nil {
		return err
	}
	it, err := t.find(args[0])
	if err != nil {
		return err
	}
	from := t.path(it)
	change := map[string]string{}
	if parent, err := t.folder(args[1]); err == nil {
		change["parent"] = parent // into a folder
	} else {
		dir, base := path.Split(strings.Trim(args[1], "/"))
		parent, err := t.folder(dir)
		if err != nil {
			return err
		}
		change["parent"], change["name"] = parent, base
	}
	if err := rmPatch(c, it.ID, change); err != nil {
		return err
	}
	if n, ok := change["name"]; ok {
		it.Name = n
	}
	it.Parent = change["parent"]
	fmt.Printf("Moved %s to %s.\n", from, t.path(it))
	return nil
}

// rmContent is the part of a document's .content file that orders its
// pages: cPages in firmware 3, pages and redirectionPageMap before.
type rmContent struct {
	FileType string   `json:"fileType"`
	Pages    []string `json:"pages"`
	Redirect []int    `json:"redirectionPageMap"`
	CPages   struct {
		Pages []struct {
			ID  string `json:"id"`
			Idx struct {
				Value string `json:"value"`
			} `json:"idx"`
			Redir *struct {
				Value int `json:"value"`
			} `json:"redir"`
			Deleted *struct {
				Value int `json:"value"`
			} `json:"deleted"`
		} `json:"pages"`
	} `json:"cPages"`
}

type rmPageRef struct {
	id    string
	redir int // the page of the PDF or EPUB it is on; -1 for an added page
}

// pageOrder lists a document's pages in order.
func (c *rmContent) pageOrder() []rmPageRef {
	var out []rmPageRef
	if len(c.CPages.Pages) > 0 {
		pages := c.CPages.Pages
		sort.SliceStable(pages, func(i, j int) bool { return pages[i].Idx.Value < pages[j].Idx.Value })
		for _, p := range pages {
			if p.Deleted != nil && p.Deleted.Value != 0 {
				continue
			}
			r := -1
			if p.Redir != nil {
				r = p.Redir.Value
			}
			out = append(out, rmPageRef{p.ID, r})
		}
		return out
	}
	for i, id := range c.Pages {
		r := i
		if i < len(c.Redirect) {
			r = c.Redirect[i]
		}
		out = append(out, rmPageRef{id, r})
	}
	return out
}

// safeFileName makes a document's name usable as a file name.
func safeFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < 32 {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = "document"
	}
	return name
}

func rmGet(c config, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: delta remarkable get <document> [dir]")
	}
	dir := "."
	if len(args) == 2 {
		dir = args[1]
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	t, err := rmLoad(c)
	if err != nil {
		return err
	}
	it, err := t.find(args[0])
	if err != nil {
		return err
	}
	if it.Type == "folder" {
		return fmt.Errorf("%s is a folder; get the documents in it one by one", t.path(it))
	}
	body, err := do(c, http.MethodGet, "/remarkable/"+url.PathEscape(it.ID), nil)
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return fmt.Errorf("reading the document: %w", err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		r, err := f.Open()
		if err != nil {
			continue
		}
		var b bytes.Buffer
		b.ReadFrom(r)
		r.Close()
		files[f.Name] = b.Bytes()
	}
	var content rmContent
	json.Unmarshal(files[it.ID+".content"], &content)
	base := filepath.Join(dir, safeFileName(it.Name))

	// The PDF or EPUB as it was put on the tablet.
	original := ""
	for _, ext := range []string{"pdf", "epub"} {
		if data, ok := files[it.ID+"."+ext]; ok {
			original = base + "." + ext
			if err := os.WriteFile(original, data, 0o644); err != nil {
				return err
			}
			fmt.Printf("Saved %s (the %s as uploaded, %s).\n", original, strings.ToUpper(ext), humanSize(int64(len(data))))
		}
	}

	// The tablet's pages: every page of a notebook, and the pages of a PDF
	// or EPUB that were written on.
	var pages []rmPDFPage
	var skipped []string
	for i, ref := range content.pageOrder() {
		data, ok := files[it.ID+"/"+ref.id+".rm"]
		var page *rmPage
		if ok {
			page, err = parseRM(data)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("page %d (%v)", i+1, err))
				continue
			}
		}
		written := page != nil && (len(page.Strokes) > 0 || page.Text != "")
		if original != "" && !written {
			continue
		}
		if page == nil {
			page = &rmPage{}
		}
		label := ""
		if original != "" {
			if ref.redir >= 0 {
				label = fmt.Sprintf("Handwriting on page %d of the %s", ref.redir+1, strings.ToUpper(content.FileType))
			} else {
				label = fmt.Sprintf("A page added after page %d of the %s", lastRedir(content.pageOrder()[:i])+1, strings.ToUpper(content.FileType))
			}
		}
		pages = append(pages, rmPDFPage{page: page, label: label})
	}
	if original == "" && len(pages) == 0 {
		pages = append(pages, rmPDFPage{page: &rmPage{}}) // a notebook never opened
	}
	for _, s := range skipped {
		fmt.Printf("Skipped %s.\n", s)
	}
	if len(pages) == 0 {
		fmt.Println("Nothing is written on it.")
		return nil
	}
	out := base + ".pdf"
	what := "its pages"
	if original != "" {
		out = base + " (handwriting).pdf"
		what = "the handwriting, one page per page written on, each labelled with the page it is on"
	}
	if err := os.WriteFile(out, writePagesPDF(pages), 0o644); err != nil {
		return err
	}
	fmt.Printf("Saved %s (%s; %d page%s).\n", out, what, len(pages), plural(len(pages)))
	return nil
}

func lastRedir(refs []rmPageRef) int {
	for i := len(refs) - 1; i >= 0; i-- {
		if refs[i].redir >= 0 {
			return refs[i].redir
		}
	}
	return -1
}
