package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- reading messages ----

// decodeData decodes Gmail's base64url body data, padded or not.
func decodeData(s string) ([]byte, error) {
	return base64.URLEncoding.DecodeString(s + strings.Repeat("=", (4-len(s)%4)%4))
}

// cp1252 maps Windows-1252's bytes 0x80-0x9F; the rest of it, and all of
// Latin-1, map straight to Unicode.
var cp1252 = [32]rune{
	'€', 0x81, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0x8d, 'Ž', 0x8f,
	0x90, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0x9d, 'ž', 'Ÿ',
}

// toUTF8 converts text in the given charset to UTF-8. Gmail passes bodies on
// in their original charset; UTF-8 and the Western European ones are by far
// the most common, and anything else is shown with invalid bytes replaced.
func toUTF8(data []byte, charset string) string {
	switch strings.ToLower(strings.Trim(charset, `" `)) {
	case "iso-8859-1", "latin1", "iso_8859-1", "windows-1252", "cp1252", "us-ascii", "ascii":
		if utf8.Valid(data) && !bytes.ContainsFunc(data, func(r rune) bool { return r >= 0x80 }) {
			return string(data)
		}
		var b strings.Builder
		for _, c := range data {
			if c >= 0x80 && c < 0xa0 {
				b.WriteRune(cp1252[c-0x80])
			} else {
				b.WriteRune(rune(c))
			}
		}
		return b.String()
	}
	return strings.ToValidUTF8(string(data), "�")
}

// gmailAttachment is a file attached to a message (or an inline image).
type gmailAttachment struct {
	Name, Type, ContentID string
	Size                  int64
	Inline                bool
	part                  *gmailPart
}

// isAttachment tells files (and inline images) apart from a message's text.
func (p *gmailPart) isAttachment() bool {
	if p.Filename != "" {
		return true
	}
	if len(p.Parts) > 0 || strings.HasPrefix(p.MimeType, "multipart/") {
		return false
	}
	disp, _, _ := mime.ParseMediaType(p.header("Content-Disposition"))
	return disp == "attachment" || (p.header("Content-ID") != "" && !strings.HasPrefix(p.MimeType, "text/"))
}

// messageContent is a message's text and HTML bodies and its attachments.
type messageContent struct {
	Text, HTML  string
	Attachments []gmailAttachment
}

func (m *gmailMessage) content() messageContent {
	var c messageContent
	if m.Payload != nil {
		c.walk(m.Payload)
	}
	return c
}

func (c *messageContent) walk(p *gmailPart) {
	if p.isAttachment() {
		name := p.Filename
		if name == "" {
			name = "attachment"
			if exts, _ := mime.ExtensionsByType(p.MimeType); len(exts) > 0 {
				name += exts[0]
			}
		}
		disp, _, _ := mime.ParseMediaType(p.header("Content-Disposition"))
		c.Attachments = append(c.Attachments, gmailAttachment{
			Name: name, Type: p.MimeType, Size: p.Body.Size, part: p,
			ContentID: strings.Trim(p.header("Content-ID"), "<> "), Inline: disp == "inline",
		})
		return
	}
	if strings.HasPrefix(p.MimeType, "multipart/") {
		for i := range p.Parts {
			c.walk(&p.Parts[i])
		}
		return
	}
	if p.MimeType != "text/plain" && p.MimeType != "text/html" {
		return
	}
	data, err := decodeData(p.Body.Data)
	if err != nil || len(data) == 0 {
		return
	}
	_, params, _ := mime.ParseMediaType(p.header("Content-Type"))
	text := toUTF8(data, params["charset"])
	// Keep the first of each; a later one is usually a forwarded copy.
	if p.MimeType == "text/plain" && c.Text == "" {
		c.Text = strings.ReplaceAll(text, "\r\n", "\n")
	} else if p.MimeType == "text/html" && c.HTML == "" {
		c.HTML = text
	}
}

// bodyText is the message's text to show: its plain text, or its HTML as
// Markdown when it has no real plain text.
func (c messageContent) bodyText() string {
	text := strings.TrimSpace(c.Text)
	if c.HTML != "" && len(text) < 40 {
		if h, _ := htmlToText(c.HTML, ""); strings.TrimSpace(h) != "" {
			return h
		}
	}
	return text
}

var (
	quoteIntro    = regexp.MustCompile(`(?i)^(on\b.*\bwrote|le\b.*\ba écrit|am\b.*\bschrieb|el\b.*\bescribió)\s*:?\s*$`)
	outlookQuote  = regexp.MustCompile(`^(-{3,}\s*Original Message\s*-{3,}|_{10,})\s*$`)
	outlookHeader = regexp.MustCompile(`^\*?From:\*? `)
)

// stripQuoted cuts the quoted copy of earlier messages that replies end
// with ("On ... wrote:" and ">" lines, or Outlook's "From: ... Sent: ...").
// It reports whether it cut anything.
func stripQuoted(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	cut := -1
	// A trailing block of ">" lines (blank lines allowed).
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, ">") {
			break
		}
		cut = i
	}
	if cut >= 0 {
		// Take the "On ... wrote:" line above it too, which Gmail wraps
		// over two lines when it is long.
		j := cut - 1
		for j >= 0 && strings.TrimSpace(lines[j]) == "" {
			j--
		}
		if j >= 0 {
			if quoteIntro.MatchString(strings.TrimSpace(lines[j])) {
				cut = j
			} else if j > 0 && quoteIntro.MatchString(strings.TrimSpace(lines[j-1])+" "+strings.TrimSpace(lines[j])) {
				cut = j - 1
			}
		}
	} else {
		for i, l := range lines {
			l = strings.TrimSpace(l)
			if outlookQuote.MatchString(l) || (quoteIntro.MatchString(l) && i+1 < len(lines)) {
				cut = i
				break
			}
			if outlookHeader.MatchString(l) && i+1 < len(lines) && strings.HasPrefix(strings.TrimLeft(strings.TrimSpace(lines[i+1]), "*"), "Sent:") {
				cut = i
				break
			}
		}
	}
	if cut <= 0 {
		return text, false
	}
	return strings.TrimRight(strings.Join(lines[:cut], "\n"), " \n"), true
}

// quote prefixes each line with "> ", as replies quote the original.
func quote(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, ">") {
			lines[i] = ">" + l
		} else if l == "" {
			lines[i] = ">"
		} else {
			lines[i] = "> " + l
		}
	}
	return strings.Join(lines, "\n")
}

var bodyTag = regexp.MustCompile(`(?is)<body[^>]*>(.*)</body>`)

// htmlBody is the inside of an HTML document's <body>, for embedding.
func htmlBody(doc string) string {
	if m := bodyTag.FindStringSubmatch(doc); m != nil {
		return m[1]
	}
	return doc
}

// ---- addresses ----

// parseAddresses parses comma-separated address lists ("Jane <j@x.com>,
// bob@y.org"), as given to --to or found in a header.
func parseAddresses(lists ...string) ([]*mail.Address, error) {
	var out []*mail.Address
	for _, s := range lists {
		if strings.TrimSpace(s) == "" {
			continue
		}
		addrs, err := mail.ParseAddressList(s)
		if err != nil {
			// Headers sometimes hold names with unquoted commas or
			// encoded words the parser rejects; try each piece.
			for _, piece := range strings.Split(s, ",") {
				if strings.TrimSpace(piece) == "" {
					continue
				}
				a, perr := mail.ParseAddress(piece)
				if perr != nil {
					return nil, fmt.Errorf("invalid address %q", strings.TrimSpace(piece))
				}
				addrs = append(addrs, a)
			}
		}
		out = append(out, addrs...)
	}
	return out, nil
}

// headerAddresses parses a header's addresses, skipping any it can't.
func headerAddresses(v string) []*mail.Address {
	if addrs, err := mail.ParseAddressList(v); err == nil {
		return addrs
	}
	var out []*mail.Address
	for _, piece := range strings.Split(v, ",") {
		if a, err := mail.ParseAddress(piece); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// dedupe drops repeated addresses and any for which skip is true.
func dedupe(addrs []*mail.Address, skip func(string) bool) []*mail.Address {
	seen := map[string]bool{}
	var out []*mail.Address
	for _, a := range addrs {
		k := strings.ToLower(a.Address)
		if seen[k] || (skip != nil && skip(a.Address)) {
			continue
		}
		seen[k] = true
		out = append(out, a)
	}
	return out
}

func addressList(addrs []*mail.Address) string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// displayAddresses shows addresses for reading: "Jane Doe <jane@x.com>".
func displayAddresses(addrs []*mail.Address) string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		if a.Name != "" {
			s[i] = a.Name + " <" + a.Address + ">"
		} else {
			s[i] = a.Address
		}
	}
	return strings.Join(s, ", ")
}

// displayHeader decodes a header's encoded words for display.
func displayHeader(v string) string {
	if d, err := (&mime.WordDecoder{}).DecodeHeader(v); err == nil {
		return d
	}
	return v
}

// ---- writing messages ----

type outgoing struct {
	From                  *mail.Address
	To, Cc, Bcc           []*mail.Address
	Subject               string
	InReplyTo, References string
	Text                  string
	HTML                  string // optional alternative to Text
	Attachments           []outAttachment
	Date                  time.Time
}

type outAttachment struct {
	Name, Type, ContentID string
	Inline                bool
	Data                  []byte
}

// bytes renders the message in RFC 5322 form, as Gmail's send and draft
// uploads take it.
func (m *outgoing) bytes() []byte {
	var b bytes.Buffer
	h := func(name, value string) {
		if value != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", name, value)
		}
	}
	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}
	h("Date", date.Format(time.RFC1123Z))
	if m.From != nil {
		h("From", m.From.String())
	}
	h("To", addressList(m.To))
	h("Cc", addressList(m.Cc))
	h("Bcc", addressList(m.Bcc))
	h("Subject", mime.QEncoding.Encode("UTF-8", m.Subject))
	h("In-Reply-To", m.InReplyTo)
	h("References", m.References)
	h("MIME-Version", "1.0")

	body := func(w *bytes.Buffer, contentType, text string) {
		w.WriteString("Content-Type: " + contentType + "; charset=\"UTF-8\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		qp := quotedprintable.NewWriter(w)
		qp.Write([]byte(strings.ReplaceAll(text, "\r\n", "\n")))
		qp.Close()
	}
	// alternative writes the text (and HTML) part(s) into w as one entity.
	alternative := func(w *bytes.Buffer) {
		if m.HTML == "" {
			body(w, "text/plain", m.Text)
			return
		}
		var inner bytes.Buffer
		mw := multipart.NewWriter(&inner)
		for _, p := range []struct{ t, s string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
			var part bytes.Buffer
			body(&part, p.t, p.s)
			writeRawPart(mw, part.Bytes())
		}
		mw.Close()
		w.WriteString("Content-Type: multipart/alternative; boundary=\"" + mw.Boundary() + "\"\r\n\r\n")
		w.Write(inner.Bytes())
	}
	if len(m.Attachments) == 0 {
		alternative(&b)
		return b.Bytes()
	}
	var inner bytes.Buffer
	mw := multipart.NewWriter(&inner)
	var first bytes.Buffer
	alternative(&first)
	writeRawPart(mw, first.Bytes())
	for _, a := range m.Attachments {
		ct := a.Type
		if ct == "" {
			ct = contentTypeFor(a.Name)
		}
		disp := "attachment"
		if a.Inline {
			disp = "inline"
		}
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Type", mime.FormatMediaType(ct, map[string]string{"name": a.Name}))
		hdr.Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": a.Name}))
		hdr.Set("Content-Transfer-Encoding", "base64")
		if a.ContentID != "" {
			hdr.Set("Content-ID", "<"+a.ContentID+">")
		}
		if hdr.Get("Content-Type") == "" { // a type FormatMediaType rejects
			hdr.Set("Content-Type", mime.FormatMediaType("application/octet-stream", map[string]string{"name": a.Name}))
		}
		pw, _ := mw.CreatePart(hdr)
		enc := base64.StdEncoding.EncodeToString(a.Data)
		for len(enc) > 76 {
			pw.Write([]byte(enc[:76] + "\r\n"))
			enc = enc[76:]
		}
		pw.Write([]byte(enc + "\r\n"))
	}
	mw.Close()
	b.WriteString("Content-Type: multipart/mixed; boundary=\"" + mw.Boundary() + "\"\r\n\r\n")
	b.Write(inner.Bytes())
	return b.Bytes()
}

// writeRawPart adds an entity (its headers, a blank line and its body) to a
// multipart writer.
func writeRawPart(mw *multipart.Writer, entity []byte) {
	head, body, _ := bytes.Cut(entity, []byte("\r\n\r\n"))
	hdr := textproto.MIMEHeader{}
	for _, line := range strings.Split(string(head), "\r\n") {
		if k, v, ok := strings.Cut(line, ": "); ok {
			hdr.Add(k, v)
		}
	}
	pw, _ := mw.CreatePart(hdr)
	pw.Write(body)
}

// safeFilename turns an attachment's name into one every file system takes.
func safeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < ' ' || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimSpace(name), ".")
	if name == "" {
		name = "attachment"
	}
	return name
}
