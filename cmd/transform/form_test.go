package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeForm(t *testing.T, form string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "form.json")
	if err := os.WriteFile(path, []byte(form), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFormProblems(t *testing.T) {
	for form, want := range map[string]string{
		`{"title": "x", "format": "mla", "blocks": [{"type": "paragraph", "text": "a"}], "colour": "red"}`: `unknown field "colour"`,
		`{"format": "memo", "blocks": []}`:                                                                      `"format" is "memo"`,
		`{"title": "x", "format": "plain", "blocks": [{"type": "heading", "text": "a"}]}`:                       `"level" of 1, 2 or 3`,
		`{"title": "x", "format": "plain", "blocks": [{"type": "figure", "page": 1, "box": [0.5, 0, 0.2, 1]}]}`: `left < right`,
		`{"title": "x", "format": "plain", "blocks": [{"type": "list", "items": [{"txt": "a"}]}]}`:              `a list item is a string`,
		`{"title": "x", "format": "plain", "sources": ["a"], "blocks": [{"type": "paragraph", "text": "a"}]}`:   `"sources" is for`,
	} {
		_, err := readForm(writeForm(t, form))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", form, err, want)
		}
	}
}

// TestBuild makes a document in each format with every kind of block, and
// checks that each part is well-formed XML with the content in it.
func TestBuild(t *testing.T) {
	page := image.NewRGBA(image.Rect(0, 0, 1275, 1650))
	figure := func(n int, box []float64) (image.Image, error) {
		return page.SubImage(image.Rect(100, 100, 600, 400)), nil
	}
	for _, format := range []string{"mla", "apa", "plain"} {
		f, err := readForm(writeForm(t, `{"title": "Essay", "format": "`+format+`", "author": "Ada Lovelace", "heading": "On Engines & <Looms>",
			"blocks": [
				{"type": "paragraph", "text": "It **weaves** *algebraic* patterns, 3 \\* 4.\nNext line."},
				{"type": "heading", "level": 2, "text": "Notes"},
				{"type": "list", "ordered": true, "items": ["One", {"text": "Two", "items": ["Two a"]}]},
				{"type": "list", "items": ["Bullet"]},
				{"type": "quote", "text": "Quoted"},
				{"type": "table", "header": true, "rows": [["A", "B"], ["1"]]},
				{"type": "figure", "page": 1, "box": [0.1, 0.1, 0.5, 0.3], "caption": "An engine"},
				{"type": "page_break"}
			],
			"sources": `+map[bool]string{true: `["Zed, Z. *Z*.", "Babbage, C. *Passages*."]`, false: `[]`}[format != "plain"]+`}`))
		if err != nil {
			t.Fatal(err)
		}
		data, err := makeDocx(f, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), figure)
		if err != nil {
			t.Fatal(err)
		}
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		parts := map[string]string{}
		for _, file := range z.File {
			r, _ := file.Open()
			b, _ := io.ReadAll(r)
			parts[file.Name] = string(b)
			if !strings.HasSuffix(file.Name, ".xml") && !strings.HasSuffix(file.Name, ".rels") {
				continue
			}
			d := xml.NewDecoder(bytes.NewReader(b))
			for {
				if _, err := d.Token(); err == io.EOF {
					break
				} else if err != nil {
					t.Fatalf("%s %s: %v", format, file.Name, err)
				}
			}
		}
		doc := parts["word/document.xml"]
		for _, want := range []string{"On Engines &amp; &lt;Looms&gt;", "<w:b/></w:rPr><w:t xml:space=\"preserve\">weaves", "3 * 4.", "<w:br/>", `<w:numId w:val="2"/>`, "rImg1", "[Instructor]"} {
			if format == "plain" && want == "[Instructor]" {
				continue
			}
			if !strings.Contains(doc, want) {
				t.Errorf("%s: document.xml lacks %q", format, want)
			}
		}
		if format != "plain" && strings.Index(doc, "Babbage") > strings.Index(doc, "Zed") {
			t.Errorf("%s: sources aren't sorted", format)
		}
		if format == "mla" && !strings.Contains(parts["word/header1.xml"], "Lovelace ") {
			t.Errorf("mla: header %s", parts["word/header1.xml"])
		}
		if parts["word/media/figure1.png"] == "" || parts["word/numbering.xml"] == "" {
			t.Errorf("%s: missing the figure or the numbering", format)
		}
	}
}
