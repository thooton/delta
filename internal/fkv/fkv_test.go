package fkv

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	got, err := Parse([]byte("\uFEFFhola||hello\r\n\n  \nline\\none||a \\\\ b\nquote \"it's\"||\\|\\| and \\||\na|||b\n\\x41\\u00e9\\U0001F600||tab\\there\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Card{
		{"hola", "hello"},
		{"line\none", `a \ b`},
		{`quote "it's"`, "|| and ||"},
		{"a", "|b"},
		{"Aé😀", "tab\there"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	for _, c := range []struct{ in, why string }{
		{"ok||ok\nno separator", "line 2: no ||"},
		{"a||b||c", "a second ||"},
		{"||b", "the key is empty"},
		{"a||", "the value is empty"},
		{`a\q||b`, `\q isn't an escape`},
		{`a||b\`, `a lone \`},
		{`\x4||b`, `isn't a character`},
		{`\uD800||b`, `isn't a character`},
		{"a||\xff", "UTF-8"},
	} {
		_, err := Parse([]byte(c.in))
		if err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%q: got %v, want %q", c.in, err, c.why)
		}
	}
}
