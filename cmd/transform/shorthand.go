package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// shorthandResult is what the command center's shorthand reader returns
// (see cmd/delta-cc/shorthand.go and the worker in the topaz project).
type shorthandResult struct {
	Lines []struct {
		Grascii  string `json:"grascii"`
		English  string `json:"english"`
		Box      []int  `json:"box"`
		Outlines []struct {
			Outline string   `json:"outline"`
			Words   []string `json:"words"`
			Exact   *bool    `json:"exact"`
		} `json:"outlines"`
	} `json:"lines"`
	Image struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"image"`
	Seconds float64 `json:"seconds"`
}

const shorthandUsage = "usage: transform shorthand <image.png|.jpg> [--line] [--region x,y,w,h] [--json]"

func shorthand(args []string) error {
	var pos []string
	q := url.Values{}
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--line" || a == "--lines":
			q.Set("mode", "line")
		case a == "--json":
			asJSON = true
		case a == "--region" && i+1 < len(args):
			i++
			q.Set("region", args[i])
		case strings.HasPrefix(a, "--region="):
			q.Set("region", strings.TrimPrefix(a, "--region="))
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown option %s (%s)", a, shorthandUsage)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) != 1 {
		return errors.New(shorthandUsage)
	}
	image, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	if strings.HasPrefix(string(image), "%PDF-") {
		return errors.New("that's a PDF: render the page to a PNG first (SHORTHAND in `transform help` shows how)")
	}
	k, err := key()
	if err != nil {
		return err
	}
	path := "/transform/shorthand"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	data, err := callAs(k, http.MethodPost, path, "application/octet-stream", image)
	if err != nil {
		if strings.Contains(err.Error(), "shorthand service offline") {
			return errors.New("shorthand service offline: the shorthand reader isn't connected right now. Read the shorthand from the page images yourself, as best you can")
		}
		return err
	}
	if asJSON {
		os.Stdout.Write(data)
		return nil
	}
	var r shorthandResult
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("the shorthand reader's answer isn't readable: %w", err)
	}
	fmt.Print(formatShorthand(pos[0], r))
	return nil
}

// formatShorthand lays out a result for the agent: the English of every
// line first, then each line's Grascii with the dictionary's candidates.
func formatShorthand(name string, r shorthandResult) string {
	var b strings.Builder
	n := len(r.Lines)
	if n == 0 {
		fmt.Fprintf(&b, "No lines of shorthand were found in %s (%dx%d px). If there is shorthand on it, try a region around it, or --line for a single line.\n", name, r.Image.Width, r.Image.Height)
		return b.String()
	}
	plural := "s"
	if n == 1 {
		plural = ""
	}
	fmt.Fprintf(&b, "Shorthand in %s (%dx%d px): %d line%s, read in %.1f s.\n\n", name, r.Image.Width, r.Image.Height, n, plural, r.Seconds)
	b.WriteString("ENGLISH (the Grascii-to-English model's guess at each line: expect errors, often in a fifth to a third of the words)\n")
	for i, ln := range r.Lines {
		fmt.Fprintf(&b, "%3d  %s\n", i+1, ln.English)
	}
	b.WriteString("\nGRASCII (the strokes the reader guessed, each outline with the dictionary's words, likeliest first;\n")
	b.WriteString("~ marks an outline not in the dictionary, whose words are those of similar outlines; the strokes\n")
	b.WriteString("are a guess too, so check both parts against each other and the page image)\n")
	for i, ln := range r.Lines {
		var parts []string
		for _, o := range ln.Outlines {
			p := o.Outline
			if o.Exact != nil && !*o.Exact {
				p = "~" + p
			}
			if len(o.Words) > 0 {
				p += " (" + strings.Join(o.Words, "/") + ")"
			}
			parts = append(parts, p)
		}
		if len(parts) == 0 {
			parts = []string{"(nothing read)"}
		}
		box := ""
		if len(ln.Box) == 4 {
			box = fmt.Sprintf("  [x %d-%d, y %d-%d]", ln.Box[0], ln.Box[2], ln.Box[1], ln.Box[3])
		}
		fmt.Fprintf(&b, "%3d  %s%s\n", i+1, strings.Join(parts, " "), box)
	}
	return b.String()
}
