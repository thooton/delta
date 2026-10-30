package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// selfTestRemarkable runs the reMarkable routes against an empty rmfakecloud
// store, and checks that what they write is what rmfakecloud and the tablet
// expect: blobs named by their hash, index hashes from their entries, and a
// history line per root.
func selfTestRemarkable(s *remarkableStore, expect expectFunc, token string) error {
	var created struct{ ID string }
	data, err := expect("empty list", 200, "GET", "/remarkable", token, nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "[]" {
		return fmt.Errorf("empty store listed %s", data)
	}
	if _, err := expectJSON(expect, &created, "mkdir", 201, "POST", "/remarkable/folders", token, []byte(`{"name":"School","parent":""}`)); err != nil {
		return err
	}
	folder := created.ID
	pdf := []byte("%PDF-1.4\n% delta self-test\n")
	if _, err := expectJSON(expect, &created, "upload", 201, "POST", "/remarkable/documents?name=Worksheet&parent="+url.QueryEscape(folder), token, pdf); err != nil {
		return err
	}
	doc := created.ID
	if err := runSteps(expect,
		step{"a file that's neither PDF nor EPUB is refused", 400, "POST", "/remarkable/documents?name=x", token, []byte("hello")},
		step{"a missing parent is refused", 400, "POST", "/remarkable/documents?name=x&parent=nope", token, pdf},
		step{"a document isn't a folder", 400, "POST", "/remarkable/folders", token, []byte(`{"name":"x","parent":"` + doc + `"}`)},
		step{"a folder can't go inside itself", 400, "PATCH", "/remarkable/" + folder, token, []byte(`{"parent":"` + folder + `"}`)},
		step{"rename", 200, "PATCH", "/remarkable/" + doc, token, []byte(`{"name":"Worksheet 1"}`)},
		step{"move to the top", 200, "POST", "/remarkable/" + doc, token, []byte(`{"parent":""}`)},
		step{"trash", 200, "PATCH", "/remarkable/" + folder, token, []byte(`{"parent":"trash"}`)},
		step{"missing document", 404, "GET", "/remarkable/nope", token, nil},
	); err != nil {
		return err
	}
	var items []remarkableItem
	data, err = expectJSON(expect, &items, "list", 200, "GET", "/remarkable", token, nil)
	if err != nil {
		return err
	}
	got := map[string]remarkableItem{}
	for _, it := range items {
		got[it.ID] = it
	}
	if d := got[doc]; len(items) != 2 || d.Name != "Worksheet 1" || d.Type != "pdf" || d.Parent != "" || got[folder].Parent != "trash" || got[folder].Type != "folder" {
		return fmt.Errorf("unexpected list %s", data)
	}
	data, err = expect("download", 200, "GET", "/remarkable/"+doc, token, nil)
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if len(zr.File) != 3 || !slices.ContainsFunc(zr.File, func(f *zip.File) bool { return f.Name == doc+".pdf" }) {
		return fmt.Errorf("download: unexpected files")
	}

	// Every blob is named by its hash, except indexes, named by their
	// entries'; the root is the last history line's.
	root, err := os.ReadFile(filepath.Join(s.dir, "root"))
	if err != nil {
		return err
	}
	hist, err := os.ReadFile(filepath.Join(s.dir, remarkableHistory))
	if err != nil {
		return err
	}
	if len(hist) != 5*86 || !strings.HasSuffix(string(hist), " "+string(root)+"\n") {
		return fmt.Errorf("history doesn't end with the root (%d bytes)", len(hist))
	}
	blobs, _ := filepath.Glob(filepath.Join(s.dir, "*"))
	for _, b := range blobs {
		name := filepath.Base(b)
		if len(name) != 64 {
			continue
		}
		data, _ := os.ReadFile(b)
		if keyHash(string(data)) == name {
			continue
		}
		entries, err := parseRemarkableIndex(data)
		if err != nil || remarkableHash(entries) != name {
			return fmt.Errorf("blob %s isn't named by its hash", name)
		}
	}

	// Pages one at a time, for the phone's Tablet app.
	if _, err := expectJSON(expect, &created, "upload a readable PDF", 201, "POST", "/remarkable/documents?name=Essay", token, selfTestPDF()); err != nil {
		return err
	}
	essay := created.ID
	data, err = expect("page 1", 200, "GET", "/remarkable/"+essay+"/pages/1", token, nil)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) || !bytes.Contains(data, []byte("/Count 1")) {
		return fmt.Errorf("page 1 isn't a one-page PDF")
	}
	data, err = expect("whole PDF", 200, "GET", "/remarkable/"+essay+"/pdf", token, nil)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) || !bytes.Contains(data, []byte("/Count 1")) {
		return fmt.Errorf("the whole PDF isn't the document's one page")
	}
	return runSteps(expect,
		step{"no page past the end", 404, "GET", "/remarkable/" + essay + "/pages/2", token, nil},
		step{"pages count from 1", 400, "GET", "/remarkable/" + essay + "/pages/0", token, nil},
		step{"an unreadable PDF has nothing to show", 404, "GET", "/remarkable/" + doc + "/pages/1", token, nil},
		step{"a folder has no pages", 400, "GET", "/remarkable/" + folder + "/pages/1", token, nil},
		step{"no such document's pages", 404, "GET", "/remarkable/nope/pages/1", token, nil},
		step{"a folder has no PDF", 400, "GET", "/remarkable/" + folder + "/pdf", token, nil},
		step{"no such document's PDF", 404, "GET", "/remarkable/nope/pdf", token, nil},
	)
}

// selfTestPDF is the smallest readable PDF: one blank Letter page.
func selfTestPDF() []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << >> /Contents 4 0 R >>",
		"<< /Length 0 >>\nstream\n\nendstream",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}
