package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// The form: everything the agent decides about the new document, and
// nothing about how it is laid out. `transform build` reads it and makes the
// document from a fixed template for its format.
type form struct {
	Title      string   `json:"title"`  // the job's title for the user, and the result's name
	Format     string   `json:"format"` // mla, apa or plain
	Author     string   `json:"author"`
	Instructor string   `json:"instructor"`
	Course     string   `json:"course"`
	School     string   `json:"school"` // apa's title page
	Date       string   `json:"date"`
	Heading    string   `json:"heading"` // the title printed on the document
	Font       string   `json:"font"`    // plain: serif or sans
	Blocks     []block  `json:"blocks"`
	Sources    []string `json:"sources"` // works cited or references
}

type block struct {
	Type    string     `json:"type"` // paragraph, heading, list, quote, table, figure, page_break
	Text    string     `json:"text,omitempty"`
	Level   int        `json:"level,omitempty"`   // heading: 1 to 3
	Ordered bool       `json:"ordered,omitempty"` // list
	Items   []listItem `json:"items,omitempty"`   // list
	Rows    [][]string `json:"rows,omitempty"`    // table
	Header  bool       `json:"header,omitempty"`  // table: the first row is a header
	Page    int        `json:"page,omitempty"`    // figure: the page it is on, from 1
	Box     []float64  `json:"box,omitempty"`     // figure: left, top, right, bottom, as fractions of the page
	Caption string     `json:"caption,omitempty"` // figure
}

// listItem is a list's line: a string, or {"text", "items"} for one with
// lines under it.
type listItem struct {
	Text  string     `json:"text"`
	Items []listItem `json:"items,omitempty"`
}

func (l *listItem) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		*l = listItem{Text: s}
		return nil
	}
	type plain listItem
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("a list item is a string or {\"text\", \"items\"}: %w", err)
	}
	*l = listItem(p)
	return nil
}

// readForm reads and checks the form, saying everything wrong with it at
// once.
func readForm(path string) (*form, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f form
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s isn't a valid form: %w (see `transform help`)", path, err)
	}
	if probs := f.check(); len(probs) > 0 {
		return nil, fmt.Errorf("fix %s and build again:\n  - %s", path, strings.Join(probs, "\n  - "))
	}
	return &f, nil
}

func (f *form) check() []string {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }
	f.Format = strings.ToLower(strings.TrimSpace(f.Format))
	switch f.Format {
	case "mla", "apa", "plain":
	case "":
		add(`"format" is missing: "mla", "apa" or "plain"`)
	default:
		add(`"format" is %q: it must be "mla", "apa" or "plain"`, f.Format)
	}
	if strings.TrimSpace(f.Title) == "" {
		add(`"title" is missing: a few words naming the result, like "MLA essay on Hamlet"`)
	}
	switch strings.ToLower(f.Font) {
	case "", "serif", "sans":
	default:
		add(`"font" is %q: it must be "serif" or "sans"`, f.Font)
	}
	if f.Font != "" && f.Format != "plain" {
		add(`"font" is only for "plain": %s has its own`, strings.ToUpper(f.Format))
	}
	if len(f.Blocks) == 0 {
		add(`"blocks" is empty: the document's content goes there`)
	}
	for i, b := range f.Blocks {
		at := fmt.Sprintf("blocks[%d] (%s)", i, b.Type)
		needText := func() {
			if strings.TrimSpace(b.Text) == "" {
				add(`%s has no "text"`, at)
			}
		}
		switch b.Type {
		case "paragraph", "quote":
			needText()
		case "heading":
			needText()
			if b.Level < 1 || b.Level > 3 {
				add(`%s needs a "level" of 1, 2 or 3`, at)
			}
		case "list":
			if len(b.Items) == 0 {
				add(`%s has no "items"`, at)
			}
			if depth(b.Items) > 3 {
				add(`%s is nested more than 3 deep`, at)
			}
		case "table":
			if len(b.Rows) == 0 {
				add(`%s has no "rows"`, at)
			}
			for r, row := range b.Rows {
				if len(row) == 0 {
					add(`%s: row %d is empty`, at, r)
				}
			}
		case "figure":
			if b.Page < 1 {
				add(`%s needs the "page" it is on, from 1`, at)
			}
			if len(b.Box) != 4 {
				add(`%s needs a "box": [left, top, right, bottom], fractions of the page from 0 to 1`, at)
			} else if !(0 <= b.Box[0] && b.Box[0] < b.Box[2] && b.Box[2] <= 1 && 0 <= b.Box[1] && b.Box[1] < b.Box[3] && b.Box[3] <= 1) {
				add(`%s: "box" %v must be [left, top, right, bottom] with left < right and top < bottom, from 0 to 1`, at, b.Box)
			}
		case "page_break":
		case "":
			add(`blocks[%d] has no "type"`, i)
		default:
			add(`%s: the type must be paragraph, heading, list, quote, table, figure or page_break`, at)
		}
	}
	for i, s := range f.Sources {
		if strings.TrimSpace(s) == "" {
			add(`sources[%d] is empty`, i)
		}
	}
	if len(f.Sources) > 0 && f.Format == "plain" {
		add(`"sources" is for "mla" (Works Cited) or "apa" (References); in "plain", put them in blocks`)
	}
	return p
}

func depth(items []listItem) int {
	d := 0
	for _, it := range items {
		d = max(d, 1+depth(it.Items))
	}
	return d
}

// placeholder is what a field the document doesn't give is shown as.
func placeholder(v, name string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return "[" + name + "]"
}

// date is the form's date, or today's in the format's style.
func (f *form) date(now time.Time) string {
	if d := strings.TrimSpace(f.Date); d != "" {
		return d
	}
	if f.Format == "mla" {
		return now.Format("2 January 2006")
	}
	return now.Format("January 2, 2006")
}

var errNoForm = errors.New("usage: transform build <form.json>")
