package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// selfTestRemarkable runs the reMarkable routes against an empty rmfakecloud
// store, and checks that what they write is what rmfakecloud and the tablet
// expect: blobs named by their hash, index hashes from their entries, and a
// history line per root.
func selfTestRemarkable(s *remarkableStore, expect func(desc string, want int, method, path string, body []byte) ([]byte, error)) error {
	id := func(data []byte) string {
		var r struct{ ID string }
		json.Unmarshal(data, &r)
		return r.ID
	}
	data, err := expect("empty list", 200, "GET", "/remarkable", nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "[]" {
		return fmt.Errorf("empty store listed %s", data)
	}
	data, err = expect("mkdir", 201, "POST", "/remarkable/folders", []byte(`{"name":"School","parent":""}`))
	if err != nil {
		return err
	}
	folder := id(data)
	pdf := []byte("%PDF-1.4\n% delta self-test\n")
	data, err = expect("upload", 201, "POST", "/remarkable/documents?name=Worksheet&parent="+url.QueryEscape(folder), pdf)
	if err != nil {
		return err
	}
	doc := id(data)
	steps := []struct {
		desc         string
		want         int
		method, path string
		body         []byte
	}{
		{"a file that's neither PDF nor EPUB is refused", 400, "POST", "/remarkable/documents?name=x", []byte("hello")},
		{"a missing parent is refused", 400, "POST", "/remarkable/documents?name=x&parent=nope", pdf},
		{"a document isn't a folder", 400, "POST", "/remarkable/folders", []byte(`{"name":"x","parent":"` + doc + `"}`)},
		{"a folder can't go inside itself", 400, "PATCH", "/remarkable/" + folder, []byte(`{"parent":"` + folder + `"}`)},
		{"rename", 200, "PATCH", "/remarkable/" + doc, []byte(`{"name":"Worksheet 1"}`)},
		{"move to the top", 200, "POST", "/remarkable/" + doc, []byte(`{"parent":""}`)},
		{"trash", 200, "PATCH", "/remarkable/" + folder, []byte(`{"parent":"trash"}`)},
		{"missing document", 404, "GET", "/remarkable/nope", nil},
	}
	for _, st := range steps {
		if _, err := expect(st.desc, st.want, st.method, st.path, st.body); err != nil {
			return err
		}
	}
	data, err = expect("list", 200, "GET", "/remarkable", nil)
	if err != nil {
		return err
	}
	var items []remarkableItem
	json.Unmarshal(data, &items)
	got := map[string]remarkableItem{}
	for _, it := range items {
		got[it.ID] = it
	}
	if d := got[doc]; len(items) != 2 || d.Name != "Worksheet 1" || d.Type != "pdf" || d.Parent != "" || got[folder].Parent != "trash" || got[folder].Type != "folder" {
		return fmt.Errorf("unexpected list %s", data)
	}
	data, err = expect("download", 200, "GET", "/remarkable/"+doc, nil)
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	found := false
	for _, f := range zr.File {
		if f.Name == doc+".pdf" {
			found = true
		}
	}
	if len(zr.File) != 3 || !found {
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
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) == name {
			continue
		}
		entries, err := parseRemarkableIndex(data)
		if err != nil || remarkableHash(entries) != name {
			return fmt.Errorf("blob %s isn't named by its hash", name)
		}
	}

	// Pages one at a time, for the phone's Tablet app.
	data, err = expect("upload a readable PDF", 201, "POST", "/remarkable/documents?name=Essay", selfTestPDF())
	if err != nil {
		return err
	}
	essay := id(data)
	data, err = expect("page 1", 200, "GET", "/remarkable/"+essay+"/pages/1", nil)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) || !bytes.Contains(data, []byte("/Count 1")) {
		return fmt.Errorf("page 1 isn't a one-page PDF")
	}
	for _, st := range []struct {
		desc string
		want int
		path string
	}{
		{"no page past the end", 404, "/remarkable/" + essay + "/pages/2"},
		{"pages count from 1", 400, "/remarkable/" + essay + "/pages/0"},
		{"an unreadable PDF has nothing to show", 404, "/remarkable/" + doc + "/pages/1"},
		{"a folder has no pages", 400, "/remarkable/" + folder + "/pages/1"},
		{"no such document's pages", 404, "/remarkable/nope/pages/1"},
	} {
		if _, err := expect(st.desc, st.want, "GET", st.path, nil); err != nil {
			return err
		}
	}
	return nil
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
