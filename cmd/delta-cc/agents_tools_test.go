package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyEdits(t *testing.T) {
	cases := []struct {
		name, content string
		edits         []textEdit
		want, err     string
	}{
		{"exact", "a\nb\nc\n", []textEdit{{"b", "B"}}, "a\nB\nc\n", ""},
		{"several, matched against the original", "one two three", []textEdit{{"three", "3"}, {"one", "1"}}, "1 two 3", ""},
		{"not unique", "x x", []textEdit{{"x", "y"}}, "", "2 occurrences"},
		{"missing", "abc", []textEdit{{"zzz", "y"}}, "", "could not find"},
		{"overlap", "abcdef", []textEdit{{"abcd", "1"}, {"cdef", "2"}}, "", "overlap"},
		{"no change", "abc", []textEdit{{"b", "b"}}, "", "no changes"},
		{"keeps CRLF", "a\r\nb\r\n", []textEdit{{"a\nb", "x\ny"}}, "x\r\ny\r\n", ""},
		{"keeps a BOM", "\uFEFFhello", []textEdit{{"hello", "bye"}}, "\uFEFFbye", ""},
		{"trailing spaces in the file", "if x:   \n    go()\n", []textEdit{{"if x:\n    go()", "if y:\n    run()"}}, "if y:\n    run()\n", ""},
		{"typographic quotes in the file", "say(“hi”) — now\n", []textEdit{{`say("hi") - now`, `say("bye")`}}, "say(\"bye\")\n", ""},
		{"fuzzy, then the rest kept", "a  \nb’s\nc\n", []textEdit{{"b's", "B"}}, "a  \nB\nc\n", ""},
	}
	for _, c := range cases {
		got, err := applyEdits(c.content, c.edits, "f")
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: got %q, %v; want an error with %q", c.name, got, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

func TestTailOf(t *testing.T) {
	write := func(content string) *os.File {
		f, err := os.Create(filepath.Join(t.TempDir(), "out"))
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(content)
		return f
	}
	text, from, to, lines := tailOf(write("a\nb\n"))
	if text != "a\nb" || from != 1 || to != 2 || lines != 2 {
		t.Errorf("short: %q %d-%d of %d", text, from, to, lines)
	}
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		b.WriteString(strings.Repeat("x", 3) + "\n")
	}
	text, from, to, _ = tailOf(write(b.String()))
	if from != 3001 || to != 5000 || strings.Count(text, "\n") != toolMaxLines-1 {
		t.Errorf("many lines: %d-%d, %d lines", from, to, strings.Count(text, "\n")+1)
	}
	text, from, _, lines = tailOf(write(strings.Repeat("y", 200<<10) + "\nlast"))
	if text != "last" || from != 2 || lines != 2 {
		t.Errorf("a long line then a short one: %.20q from %d of %d", text, from, lines)
	}
	text, _, _, _ = tailOf(write(strings.Repeat("z", 200<<10)))
	if len(text) != toolMaxBytes {
		t.Errorf("one long line: %d bytes", len(text))
	}
}
