package main

import (
	"fmt"
	"html"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// link is a hyperlink, image or embed found in Canvas HTML.
type link struct {
	kind, text, href string
}

var (
	attrPattern  = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*(?:=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?`)
	spacePattern = regexp.MustCompile(`[ \t\r\n\f]+`)
	blankLines   = regexp.MustCompile(`\n{3,}`)
	filePattern  = regexp.MustCompile(`/files/(\d+)`)
)

// htmlToText renders Canvas HTML as Markdown, the same format the agent
// writes back (see markdownToHTML). Links are kept inline as [text](url) and
// also returned, so files and pages referenced in
// instructions can be fetched. Relative URLs are resolved against base.
func htmlToText(src, base string) (string, []link) {
	var out strings.Builder
	var links []link
	baseURL, _ := url.Parse(base)
	resolve := func(href string) string {
		href = html.UnescapeString(strings.TrimSpace(href))
		if baseURL == nil || href == "" {
			return href
		}
		if u, err := baseURL.Parse(href); err == nil {
			return u.String()
		}
		return href
	}

	type anchor struct {
		href, apiHref string
		start         int
	}
	var anchors []anchor
	var lists []int // -1 for <ul>, else the next <ol> number
	pre := 0
	skip := ""
	// Emphasis markers are written lazily so whitespace inside a tag
	// ("<strong>word </strong>") lands outside the Markdown markers, and
	// empty pairs vanish.
	pending := ""
	flush := func() {
		out.WriteString(pending)
		pending = ""
	}
	openMark := func(m string) { pending += m }
	closeMark := func(m string) {
		if strings.HasSuffix(pending, m) {
			pending = strings.TrimSuffix(pending, m)
			return
		}
		str := out.String()
		trimmed := strings.TrimRight(str, " \n")
		out.Reset()
		out.WriteString(trimmed + m + str[len(trimmed):])
	}

	newline := func(n int) {
		s := out.String()
		trail := len(s) - len(strings.TrimRight(s, "\n"))
		if len(strings.TrimSpace(s)) == 0 {
			return
		}
		// Trim trailing spaces on the current line before breaking it.
		if t := strings.TrimRight(s, " "); len(t) != len(s) && trail == 0 {
			out.Reset()
			out.WriteString(t)
		}
		for ; trail < n; trail++ {
			out.WriteByte('\n')
		}
	}
	text := func(t string) {
		t = html.UnescapeString(t)
		if pre == 0 {
			t = spacePattern.ReplaceAllString(t, " ")
			s := out.String()
			if strings.HasPrefix(t, " ") && (s == "" || strings.HasSuffix(s, " ") || strings.HasSuffix(s, "\n")) {
				t = t[1:]
			}
		}
		if pending != "" {
			if lead := len(t) - len(strings.TrimLeft(t, " ")); lead < len(t) {
				out.WriteString(t[:lead])
				t = t[lead:]
				flush()
			}
		}
		out.WriteString(t)
	}

	for i := 0; i < len(src); {
		lt := strings.IndexByte(src[i:], '<')
		if lt < 0 {
			if skip == "" {
				text(src[i:])
			}
			break
		}
		if lt > 0 && skip == "" {
			text(src[i : i+lt])
		}
		i += lt
		if strings.HasPrefix(src[i:], "<!--") {
			end := strings.Index(src[i:], "-->")
			if end < 0 {
				break
			}
			i += end + 3
			continue
		}
		gt := strings.IndexByte(src[i:], '>')
		if gt < 0 {
			text(src[i:])
			break
		}
		tag := src[i+1 : i+gt]
		i += gt + 1
		closing := strings.HasPrefix(tag, "/")
		tag = strings.TrimPrefix(tag, "/")
		name := tagName(tag)
		if skip != "" {
			if closing && name == skip {
				skip = ""
			}
			continue
		}
		attrs := map[string]string{}
		if !closing {
			if sp := strings.IndexAny(tag, " \t\r\n"); sp > 0 {
				for _, m := range attrPattern.FindAllStringSubmatch(tag[sp:], -1) {
					attrs[strings.ToLower(m[1])] = m[2] + m[3] + m[4]
				}
			}
		}
		if !closing || name != "a" {
			if _, inline := map[string]bool{"strong": true, "b": true, "em": true, "i": true, "code": true, "del": true, "s": true, "strike": true, "a": true, "span": true, "font": true, "u": true, "sup": true, "sub": true}[name]; !inline {
				pending = ""
			}
		}
		switch name {
		case "script", "style", "head", "title":
			if !closing {
				skip = name
			}
		case "br":
			out.WriteByte('\n')
		case "p", "div", "section", "article", "header", "footer", "blockquote", "table", "figure", "details", "summary", "dl":
			newline(2)
		case "h1", "h2", "h3", "h4", "h5", "h6":
			newline(2)
			if !closing {
				out.WriteString(strings.Repeat("#", int(name[1]-'0')) + " ")
			}
		case "hr":
			newline(2)
			out.WriteString("---")
			newline(2)
		case "ul", "ol":
			if closing {
				if len(lists) > 0 {
					lists = lists[:len(lists)-1]
				}
				if len(lists) == 0 {
					newline(2)
				}
			} else {
				newline(1)
				if name == "ul" {
					lists = append(lists, -1)
				} else {
					lists = append(lists, 1)
				}
			}
		case "li":
			if !closing {
				newline(1)
				depth := len(lists)
				if depth == 0 {
					depth = 1
				}
				out.WriteString(strings.Repeat("  ", depth-1))
				if len(lists) > 0 && lists[len(lists)-1] > 0 {
					fmt.Fprintf(&out, "%d. ", lists[len(lists)-1])
					lists[len(lists)-1]++
				} else {
					out.WriteString("- ")
				}
			}
		case "dt", "dd", "tr", "caption":
			newline(1)
		case "td", "th":
			if closing {
				out.WriteString(" | ")
			}
		case "pre":
			newline(2)
			if closing {
				pre--
			} else {
				pre++
			}
		case "strong", "b", "em", "i", "code", "del", "s", "strike":
			mark := map[string]string{"strong": "**", "b": "**", "em": "*", "i": "*", "code": "`", "del": "~~", "s": "~~", "strike": "~~"}[name]
			if pre > 0 && name == "code" {
				break
			}
			if closing {
				closeMark(mark)
			} else {
				openMark(mark)
			}
		case "a":
			if closing {
				if len(anchors) == 0 {
					break
				}
				a := anchors[len(anchors)-1]
				anchors = anchors[:len(anchors)-1]
				s := out.String()
				start := min(a.start, len(s))
				label := strings.TrimSpace(s[start:])
				if a.href == "" || strings.HasPrefix(a.href, "#") || strings.HasPrefix(a.href, "javascript:") {
					break
				}
				// Rewrite the anchor text as a Markdown link.
				out.Reset()
				out.WriteString(s[:start])
				if label == "" || label == a.href {
					out.WriteString("<" + a.href + ">")
				} else {
					rest := s[start:]
					lead := rest[:len(rest)-len(strings.TrimLeft(rest, " \n"))]
					out.WriteString(lead + "[" + label + "](" + a.href + ")")
				}
				links = append(links, link{"link", label, firstNonEmpty(a.apiHref, a.href)})
			} else {
				anchors = append(anchors, anchor{resolve(attrs["href"]), resolve(attrs["data-api-endpoint"]), out.Len()})
			}
		case "img":
			src := resolve(attrs["src"])
			alt := strings.TrimSpace(firstNonEmpty(attrs["alt"], attrs["title"]))
			if strings.HasPrefix(src, "data:") {
				src = "(inline data)"
			}
			fmt.Fprintf(&out, "![%s](%s)", alt, src)
			links = append(links, link{"image", alt, src})
		case "iframe", "embed", "video", "audio", "source":
			if src := resolve(attrs["src"]); src != "" && !closing {
				fmt.Fprintf(&out, "[embedded %s: %s <%s>]", name, attrs["title"], src)
				links = append(links, link{"embed", attrs["title"], src})
			}
		case "input":
			if attrs["type"] == "checkbox" {
				out.WriteString("[ ] ")
			}
		}
	}

	lines := strings.Split(out.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")), links
}

func tagName(tag string) string {
	end := strings.IndexAny(tag, " \t\r\n/")
	if end < 0 {
		end = len(tag)
	}
	return strings.ToLower(tag[:end])
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// printHTML prints Canvas HTML as text, followed by a list of the files it
// links to so they can be downloaded.
func (c *canvasClient) printHTML(heading, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	text, links := htmlToText(body, c.base)
	if text == "" && len(links) == 0 {
		return
	}
	if heading != "" {
		fmt.Printf("\n%s:\n", heading)
	}
	fmt.Println(indent(text, "  "))
	printFileLinks(links)
}

func printFileLinks(links []link) {
	seen := map[string]bool{}
	var files []string
	for _, l := range links {
		if m := filePattern.FindStringSubmatch(l.href); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			label := l.text
			if label == "" {
				label = l.kind
			}
			files = append(files, fmt.Sprintf("  file %s  %q", m[1], label))
		}
	}
	if len(files) > 0 {
		fmt.Println("\n  Linked Canvas files (download with `delta canvas download <file-id>`):")
		for _, f := range files {
			fmt.Println("  " + f)
		}
	}
}

func indent(s, prefix string) string {
	if s == "" {
		return s
	}
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}

// bodyFromOpts reads content from --text or --text-file (Markdown; .html
// files are sent as-is) or --html, returning HTML.
func bodyFromOpts(o *opts, required bool) (string, error) {
	n := 0
	var body string
	if v := o.vals["text"]; len(v) > 0 {
		n++
		body = markdownToHTML(strings.Join(v, "\n\n"))
	}
	if v := o.str("html"); v != "" {
		n++
		body = v
	}
	if p := o.str("text-file"); p != "" {
		n++
		data, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		lower := strings.ToLower(p)
		if strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm") {
			body = string(data)
		} else {
			body = markdownToHTML(string(data))
		}
	}
	if n > 1 {
		return "", fmt.Errorf("use only one of --text, --text-file and --html")
	}
	if required && strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("no content: give --text \"...\", --text-file PATH, or --html \"...\"")
	}
	return body, nil
}
