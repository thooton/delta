package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const shorthandSample = `{"lines": [
 {"grascii": "TH G '-EU-M-R", "english": "The good humour", "draft": "The good humor", "postedited": %s, "box": [10, 20, 300, 60],
  "outlines": [{"outline": "TH", "words": ["the", "there"], "exact": true}, {"outline": "G", "words": ["good", "go"], "exact": true},
               {"outline": "'-EU-M-R", "words": ["humor", "humour"], "exact": false}]},
 {"grascii": "¶", "english": "", "draft": "", "postedited": %s, "box": [10, 70, 40, 90], "outlines": [{"outline": "¶"}]}],
 "image": {"width": 960, "height": 1295}, "seconds": 84.2}`

func sampleShorthand(t *testing.T, first, second string) shorthandResult {
	var r shorthandResult
	if err := json.Unmarshal([]byte(strings.Replace(strings.Replace(shorthandSample, "%s", first, 1), "%s", second, 1)), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFormatShorthand(t *testing.T) {
	out := formatShorthand("p1.png", sampleShorthand(t, "true", "true"))
	for _, want := range []string{"2 lines, read in 84 s", "TRANSCRIBED:", "  1  The good humour\n",
		"     TH G ~'-EU-M-R (humor/humour)  [x 10-300, y 20-60]\n", "  2  (no English)\n", "     ¶  [x"} {
		if !strings.Contains(out, want) {
			t.Errorf("post-edited: missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "the/there") || strings.Contains(out, "draft:") {
		t.Errorf("post-edited: exact outlines' words or a draft mark shown:\n%s", out)
	}

	out = formatShorthand("p1.png", sampleShorthand(t, "false", "true"))
	for _, want := range []string{"1 line marked \"draft\"", "  1  draft: The good humour\n", "TH (the/there) G (good/go) ~'-EU-M-R (humor/humour)"} {
		if !strings.Contains(out, want) {
			t.Errorf("mixed: missing %q in\n%s", want, out)
		}
	}

	r := sampleShorthand(t, "true", "true")
	r.Lines[0].Postedit = "salvaged"
	if out := formatShorthand("p1.png", r); !strings.Contains(out, "  1  salvaged: The good humour\n") || !strings.Contains(out, "finished in a hurry") {
		t.Errorf("salvaged: %s", out)
	}

	out = formatShorthand("p1.png", sampleShorthand(t, "false", "false"))
	if !strings.Contains(out, "DRAFT ONLY") || strings.Contains(out, "draft: ") {
		t.Errorf("all drafts: want the DRAFT ONLY header and no per-line marks:\n%s", out)
	}

	// An older worker, without postedited: its English is the draft.
	var old shorthandResult
	json.Unmarshal([]byte(`{"lines": [{"grascii": "T", "english": "it", "outlines": [{"outline": "T", "words": ["it", "at"], "exact": true}]}]}`), &old)
	if out := formatShorthand("x.png", old); !strings.Contains(out, "DRAFT ONLY") || !strings.Contains(out, "T (it/at)") {
		t.Errorf("old worker: %s", out)
	}

	if out := formatShorthand("x.png", shorthandResult{}); !strings.Contains(out, "No lines of shorthand") {
		t.Errorf("empty: %s", out)
	}
}
