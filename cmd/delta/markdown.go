package main

import (
	"html"
	"regexp"
	"strings"
)

// markdownToHTML converts the Markdown the agent writes into the HTML Canvas
// stores for submissions, discussion posts and pages. It covers what students
// need: headings, paragraphs, emphasis, links, images, lists (nested by
// indentation), block quotes, code, rules and pipe tables. Inline HTML tags
// pass through untouched, so anything Markdown can't express (centered text,
// colors) can still be written as HTML. Newlines inside a paragraph are kept
// as line breaks, since students write poems, addresses and headers whose
// line structure matters.
func markdownToHTML(src string) string {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\t", "    "), "\n")
	var b strings.Builder
	mdBlocks(&b, lines)
	return strings.TrimSpace(b.String()) + "\n"
}

var (
	mdHeading   = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	mdRule      = regexp.MustCompile(`^\s{0,3}(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$`)
	mdBullet    = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	mdOrdered   = regexp.MustCompile(`^(\s*)(\d+)[.)]\s+(.*)$`)
	mdFence     = regexp.MustCompile("^\\s*(```|~~~)\\s*(\\S*)")
	mdTableSep  = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
	mdHTMLBlock = regexp.MustCompile(`^\s*</?(p|div|table|ul|ol|h[1-6]|blockquote|pre|hr|section|figure|center|iframe)\b`)
)

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func mdBlocks(b *strings.Builder, lines []string) {
	for i := 0; i < len(lines); {
		line := lines[i]
		switch {
		case blank(line):
			i++
		case mdFence.MatchString(line):
			fence := mdFence.FindStringSubmatch(line)[1]
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fence) {
				code = append(code, lines[i])
				i++
			}
			i++
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
		case mdHeading.MatchString(line):
			m := mdHeading.FindStringSubmatch(line)
			n := string(rune('0' + len(m[1])))
			b.WriteString("<h" + n + ">" + mdInline(m[2]) + "</h" + n + ">\n")
			i++
		case mdRule.MatchString(line):
			b.WriteString("<hr>\n")
			i++
		case mdHTMLBlock.MatchString(line):
			for i < len(lines) && !blank(lines[i]) {
				b.WriteString(lines[i] + "\n")
				i++
			}
		case strings.HasPrefix(strings.TrimSpace(line), ">"):
			var inner []string
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">") {
				l := strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")
				inner = append(inner, strings.TrimPrefix(l, " "))
				i++
			}
			b.WriteString("<blockquote>\n")
			mdBlocks(b, inner)
			b.WriteString("</blockquote>\n")
		case mdBullet.MatchString(line) || mdOrdered.MatchString(line):
			i = mdList(b, lines, i)
		case strings.Contains(line, "|") && i+1 < len(lines) && mdTableSep.MatchString(lines[i+1]):
			i = mdTable(b, lines, i)
		default:
			var para []string
			for i < len(lines) && !blank(lines[i]) && (len(para) == 0 || !startsBlock(lines, i)) {
				para = append(para, strings.TrimSpace(lines[i]))
				i++
			}
			for j := range para {
				para[j] = mdInline(para[j])
			}
			b.WriteString("<p>" + strings.Join(para, "<br>\n") + "</p>\n")
		}
	}
}

func startsBlock(lines []string, i int) bool {
	l := lines[i]
	return mdHeading.MatchString(l) || mdFence.MatchString(l) || mdRule.MatchString(l) ||
		mdBullet.MatchString(l) || mdOrdered.MatchString(l) || strings.HasPrefix(strings.TrimSpace(l), ">") ||
		(strings.Contains(l, "|") && i+1 < len(lines) && mdTableSep.MatchString(lines[i+1]))
}

func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

// mdList renders a list starting at lines[i] and returns the next line index.
// Items indented further than the list's own markers form nested blocks.
func mdList(b *strings.Builder, lines []string, i int) int {
	base := indentOf(lines[i])
	ordered := !mdBullet.MatchString(lines[i])
	tag := "ul"
	if ordered {
		tag = "ol"
		if m := mdOrdered.FindStringSubmatch(lines[i]); m[2] != "1" {
			b.WriteString(`<ol start="` + m[2] + `">` + "\n")
		} else {
			b.WriteString("<ol>\n")
		}
	} else {
		b.WriteString("<ul>\n")
	}
	for i < len(lines) {
		line := lines[i]
		var text string
		if m := mdBullet.FindStringSubmatch(line); m != nil && indentOf(line) == base && !ordered {
			text = m[2]
		} else if m := mdOrdered.FindStringSubmatch(line); m != nil && indentOf(line) == base && ordered {
			text = m[3]
		} else {
			break
		}
		i++
		// Gather continuation and nested lines belonging to this item.
		var sub []string
		for i < len(lines) {
			l := lines[i]
			if blank(l) {
				if i+1 < len(lines) && indentOf(lines[i+1]) > base && !blank(lines[i+1]) {
					sub = append(sub, "")
					i++
					continue
				}
				break
			}
			if indentOf(l) <= base {
				break
			}
			sub = append(sub, l)
			i++
		}
		b.WriteString("<li>" + mdInline(text))
		if len(sub) > 0 {
			// Plain continuation lines join the item's text; anything
			// block-like (a nested list) is rendered as blocks.
			j := 0
			for j < len(sub) && !blank(sub[j]) && !startsBlock(sub, j) {
				b.WriteString("<br>\n" + mdInline(strings.TrimSpace(sub[j])))
				j++
			}
			if j < len(sub) {
				b.WriteString("\n")
				mdBlocks(b, sub[j:])
			}
		}
		b.WriteString("</li>\n")
		if i < len(lines) && blank(lines[i]) && i+1 < len(lines) && indentOf(lines[i+1]) == base &&
			(mdBullet.MatchString(lines[i+1]) || mdOrdered.MatchString(lines[i+1])) {
			i++ // a blank line between items doesn't end the list
		}
	}
	b.WriteString("</" + tag + ">\n")
	return i
}

func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(strings.TrimSuffix(line, "|"), "|")
	cells := strings.Split(line, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func mdTable(b *strings.Builder, lines []string, i int) int {
	head := splitRow(lines[i])
	i += 2
	b.WriteString("<table>\n<thead><tr>")
	for _, c := range head {
		b.WriteString("<th>" + mdInline(c) + "</th>")
	}
	b.WriteString("</tr></thead>\n<tbody>\n")
	for i < len(lines) && strings.Contains(lines[i], "|") && !blank(lines[i]) {
		b.WriteString("<tr>")
		for _, c := range splitRow(lines[i]) {
			b.WriteString("<td>" + mdInline(c) + "</td>")
		}
		b.WriteString("</tr>\n")
		i++
	}
	b.WriteString("</tbody>\n</table>\n")
	return i
}

var (
	mdCode     = regexp.MustCompile("`([^`]+)`")
	mdImage    = regexp.MustCompile(`!\[([^\]]*)\]\(\s*<?([^)\s>]+)>?(?:\s+"([^"]*)")?\s*\)`)
	mdLink     = regexp.MustCompile(`\[([^\]]+)\]\(\s*<?([^)\s>]+)>?(?:\s+"([^"]*)")?\s*\)`)
	mdAutolink = regexp.MustCompile(`<(https?://[^>\s]+)>`)
	mdBold     = regexp.MustCompile(`\*\*([^*\s](?:[^*]*[^*\s])?)\*\*|__([^_\s](?:[^_]*[^_\s])?)__`)
	mdItalic   = regexp.MustCompile(`\*([^*\s](?:[^*]*[^*\s])?)\*|(^|[^\w])_([^_\s](?:[^_]*[^_\s])?)_([^\w]|$)`)
	mdStrike   = regexp.MustCompile(`~~([^~]+)~~`)
	mdTag      = regexp.MustCompile(`</?[a-zA-Z][a-zA-Z0-9-]*(\s[^<>]*)?/?>|&(#\d+|#x[0-9a-fA-F]+|[a-zA-Z]+);`)
	mdEscape   = regexp.MustCompile(`\\([\\` + "`" + `*_{}\[\]()#+\-.!|~>])`)
)

func mdEmphasis(s string) string {
	s = mdBold.ReplaceAllString(s, "<strong>$1$2</strong>")
	s = mdItalic.ReplaceAllString(s, "<em>$1</em>${2}<em>$3</em>$4")
	s = strings.ReplaceAll(s, "<em></em>", "")
	return mdStrike.ReplaceAllString(s, "<del>$1</del>")
}

// mdInline renders inline Markdown. Code spans, escapes and HTML tags are
// swapped for placeholders first so their contents aren't reinterpreted.
func mdInline(s string) string {
	var saved []string
	hold := func(v string) string {
		saved = append(saved, v)
		return "\x00" + string(rune('A'+len(saved)-1)) + "\x00"
	}
	s = mdCode.ReplaceAllStringFunc(s, func(m string) string {
		return hold("<code>" + html.EscapeString(m[1:len(m)-1]) + "</code>")
	})
	s = mdEscape.ReplaceAllStringFunc(s, func(m string) string { return hold(html.EscapeString(m[1:])) })
	s = mdAutolink.ReplaceAllStringFunc(s, func(m string) string {
		u := m[1 : len(m)-1]
		return hold(`<a href="` + html.EscapeString(u) + `">` + html.EscapeString(u) + `</a>`)
	})
	s = mdTag.ReplaceAllStringFunc(s, hold)
	s = html.EscapeString(s)
	s = mdImage.ReplaceAllStringFunc(s, func(m string) string {
		p := mdImage.FindStringSubmatch(m)
		return hold(`<img src="` + p[2] + `" alt="` + p[1] + `">`)
	})
	s = mdLink.ReplaceAllStringFunc(s, func(m string) string {
		p := mdLink.FindStringSubmatch(m)
		return hold(`<a href="` + p[2] + `">` + mdEmphasis(p[1]) + `</a>`)
	})
	s = mdEmphasis(s)
	for i := len(saved) - 1; i >= 0; i-- {
		s = strings.ReplaceAll(s, "\x00"+string(rune('A'+i))+"\x00", saved[i])
	}
	return s
}
