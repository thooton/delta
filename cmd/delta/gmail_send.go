package main

import (
	"errors"
	"fmt"
	"html"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The options send, reply and forward share.
const (
	sendValueFlags = "to cc bcc subject body body-file attach"
	sendBoolFlags  = "draft"
)

// gmailBody reads the message text from --body or --body-file ("-" for
// stdin).
func gmailBody(o *opts, required bool) (string, error) {
	text, file := o.vals["body"], o.str("body-file")
	if len(text) > 0 && file != "" {
		return "", errors.New("use either --body or --body-file, not both")
	}
	body := strings.Join(text, "\n\n")
	if file != "" {
		var data []byte
		var err error
		if file == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(expandHome(file))
		}
		if err != nil {
			return "", err
		}
		body = string(data)
	}
	body = strings.TrimRight(strings.ReplaceAll(body, "\r\n", "\n"), " \n")
	if required && strings.TrimSpace(body) == "" {
		return "", errors.New("no message text: give --body \"...\" or --body-file FILE (- reads stdin, e.g. a heredoc)")
	}
	return body, nil
}

// gmailAttachments reads the --attach files.
func gmailAttachments(o *opts) ([]outAttachment, error) {
	var out []outAttachment
	for _, p := range o.vals["attach"] {
		p = expandHome(p)
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a file", p)
		}
		if info.Size() > maxGmailMessage {
			return nil, fmt.Errorf("%s is %s; Gmail takes about 25 MB of attachments per message", p, humanSize(info.Size()))
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, outAttachment{Name: filepath.Base(p), Type: contentTypeFor(p), Data: data})
	}
	return out, nil
}

// recipients parses --to, --cc and --bcc.
func recipients(o *opts) (to, cc, bcc []*mail.Address, err error) {
	if to, err = parseAddresses(o.vals["to"]...); err != nil {
		return
	}
	if cc, err = parseAddresses(o.vals["cc"]...); err != nil {
		return
	}
	bcc, err = parseAddresses(o.vals["bcc"]...)
	return
}

func gmailSendCmd(args []string) error {
	o, err := parseOpts(args, sendValueFlags, sendBoolFlags)
	if err != nil {
		return err
	}
	usage := errors.New("usage: delta gmail send --to <addresses> --subject <subject> --body <text> | --body-file <file|-> [--cc ...] [--bcc ...] [--attach <file>]... [--draft]")
	if len(o.pos) != 0 {
		return usage
	}
	m := &outgoing{Subject: strings.TrimSpace(o.str("subject"))}
	if m.To, m.Cc, m.Bcc, err = recipients(o); err != nil {
		return err
	}
	if len(m.To)+len(m.Cc)+len(m.Bcc) == 0 {
		return errors.New("no recipients: give --to (and --cc, --bcc as needed)")
	}
	if m.Subject == "" {
		return errors.New("no subject: give --subject")
	}
	if m.Text, err = gmailBody(o, true); err != nil {
		return err
	}
	if m.Attachments, err = gmailAttachments(o); err != nil {
		return err
	}
	c, err := newGmailClient()
	if err != nil {
		return err
	}
	if m.From, err = c.fromFor(nil); err != nil {
		return err
	}
	return c.deliver(m, "", o.bools["draft"])
}

var (
	replyPrefix   = regexp.MustCompile(`(?i)^\s*(re|aw|sv)\s*:`)
	forwardPrefix = regexp.MustCompile(`(?i)^\s*(fwd?|wg|tr)\s*:`)
)

func gmailReplyCmd(args []string) error {
	o, err := parseOpts(args, sendValueFlags, sendBoolFlags+" all")
	if err != nil {
		return err
	}
	if len(o.pos) != 1 {
		return errors.New("usage: delta gmail reply <id> --body <text> | --body-file <file|-> [--all] [--to ...] [--cc ...] [--bcc ...] [--attach <file>]... [--draft]")
	}
	if o.str("subject") != "" {
		return errors.New("a reply keeps the conversation's subject; use `delta gmail send` for a new one")
	}
	text, err := gmailBody(o, true)
	if err != nil {
		return err
	}
	atts, err := gmailAttachments(o)
	if err != nil {
		return err
	}
	to, cc, bcc, err := recipients(o)
	if err != nil {
		return err
	}
	c, err := newGmailClient()
	if err != nil {
		return err
	}
	orig, err := c.message(o.pos[0], "full")
	if err != nil {
		return err
	}
	m, err := c.replyTo(orig, o.bools["all"])
	if err != nil {
		return err
	}
	m.To = dedupe(append(m.To, to...), nil)
	m.Cc = dedupe(append(m.Cc, cc...), func(a string) bool { return containsAddress(m.To, a) })
	m.Bcc = bcc
	if len(m.To) == 0 {
		return errors.New("couldn't tell whom to reply to; give --to")
	}
	m.Text = text + "\n\n" + attribution(orig) + "\n" + quote(orig.content().bodyText()) + "\n"
	m.Attachments = atts
	return c.deliver(m, orig.ThreadID, o.bools["draft"])
}

// replyTo addresses a reply to orig as Gmail does: to its Reply-To or
// sender, or, for the user's own message, to its recipients again. With all,
// everyone else on it is copied.
func (c *gmailClient) replyTo(orig *gmailMessage, all bool) (*outgoing, error) {
	from, err := c.fromFor(orig)
	if err != nil {
		return nil, err
	}
	m := &outgoing{From: from}
	sender := headerAddresses(orig.header("From"))
	mine := len(sender) > 0 && c.isMe(sender[0].Address)
	notMe := func(a string) bool { return c.isMe(a) }
	if mine {
		m.To = headerAddresses(orig.header("To"))
		if all {
			m.Cc = headerAddresses(orig.header("Cc"))
		}
	} else {
		m.To = headerAddresses(orig.header("Reply-To"))
		if len(m.To) == 0 {
			m.To = sender
		}
		if all {
			m.Cc = append(headerAddresses(orig.header("To")), headerAddresses(orig.header("Cc"))...)
		}
		m.To = dedupe(m.To, notMe)
	}
	m.Cc = dedupe(m.Cc, func(a string) bool { return c.isMe(a) || containsAddress(m.To, a) })

	subject := displayHeader(orig.header("Subject"))
	if !replyPrefix.MatchString(subject) {
		subject = "Re: " + subject
	}
	m.Subject = subject
	threadHeaders(m, orig)
	return m, nil
}

// threadHeaders sets In-Reply-To and References so the message joins orig's
// conversation for everyone on it.
func threadHeaders(m *outgoing, orig *gmailMessage) {
	id := strings.TrimSpace(orig.header("Message-ID"))
	if id == "" {
		id = strings.TrimSpace(orig.header("Message-Id"))
	}
	if id == "" {
		return
	}
	m.InReplyTo = id
	m.References = strings.TrimSpace(strings.Join(strings.Fields(orig.header("References")+" "+id), " "))
}

func containsAddress(addrs []*mail.Address, a string) bool {
	for _, x := range addrs {
		if strings.EqualFold(x.Address, a) {
			return true
		}
	}
	return false
}

// attribution is the line above a reply's quote, as Gmail writes it: "On
// Tue, Oct 13, 2026 at 3:04 PM Jane Doe <jane@x.com> wrote:".
func attribution(orig *gmailMessage) string {
	t := orig.time()
	if d, err := mail.ParseDate(orig.header("Date")); err == nil {
		t = d
	}
	who := displayAddressHeader(orig.header("From"))
	return fmt.Sprintf("On %s %s wrote:", t.Format("Mon, Jan 2, 2006 at 3:04 PM"), who)
}

func gmailForwardCmd(args []string) error {
	o, err := parseOpts(args, sendValueFlags, sendBoolFlags)
	if err != nil {
		return err
	}
	if len(o.pos) != 1 {
		return errors.New("usage: delta gmail forward <id> --to <addresses> [--body <note> | --body-file <file|->] [--cc ...] [--bcc ...] [--attach <file>]... [--draft]")
	}
	if o.str("subject") != "" {
		return errors.New("a forward keeps the message's subject (with Fwd:)")
	}
	note, err := gmailBody(o, false)
	if err != nil {
		return err
	}
	extra, err := gmailAttachments(o)
	if err != nil {
		return err
	}
	m := &outgoing{}
	if m.To, m.Cc, m.Bcc, err = recipients(o); err != nil {
		return err
	}
	if len(m.To)+len(m.Cc)+len(m.Bcc) == 0 {
		return errors.New("no recipients: give --to")
	}
	c, err := newGmailClient()
	if err != nil {
		return err
	}
	orig, err := c.message(o.pos[0], "full")
	if err != nil {
		return err
	}
	if m.From, err = c.fromFor(orig); err != nil {
		return err
	}
	subject := displayHeader(orig.header("Subject"))
	if !forwardPrefix.MatchString(subject) {
		subject = "Fwd: " + subject
	}
	m.Subject = subject
	threadHeaders(m, orig)

	content := orig.content()
	// The original's header block, as Gmail writes it.
	fields := [][2]string{
		{"From", displayAddressHeader(orig.header("From"))},
		{"Date", attributionDate(orig)},
		{"Subject", displayHeader(orig.header("Subject"))},
		{"To", displayAddressHeader(orig.header("To"))},
	}
	if cc := orig.header("Cc"); cc != "" {
		fields = append(fields, [2]string{"Cc", displayAddressHeader(cc)})
	}
	var text, head strings.Builder
	if note != "" {
		text.WriteString(note + "\n\n")
	}
	text.WriteString("---------- Forwarded message ---------\n")
	for _, f := range fields {
		fmt.Fprintf(&text, "%s: %s\n", f[0], f[1])
		fmt.Fprintf(&head, "%s: %s<br>", f[0], html.EscapeString(f[1]))
	}
	text.WriteString("\n" + content.bodyText() + "\n")
	m.Text = text.String()
	if content.HTML != "" {
		var h strings.Builder
		if note != "" {
			h.WriteString("<div>" + strings.ReplaceAll(html.EscapeString(note), "\n", "<br>") + "</div><br>")
		}
		h.WriteString(`<div class="gmail_quote"><div class="gmail_attr">---------- Forwarded message ---------<br>`)
		h.WriteString(head.String() + "</div><br><br>")
		h.WriteString(htmlBody(content.HTML) + "</div>")
		m.HTML = h.String()
	}
	for _, a := range content.Attachments {
		data, err := c.attachmentData(orig.ID, a)
		if err != nil {
			return fmt.Errorf("fetching the attachment %s: %w", a.Name, err)
		}
		m.Attachments = append(m.Attachments, outAttachment{Name: a.Name, Type: a.Type, ContentID: a.ContentID, Inline: a.Inline && a.ContentID != "", Data: data})
	}
	m.Attachments = append(m.Attachments, extra...)
	return c.deliver(m, orig.ThreadID, o.bools["draft"])
}

func attributionDate(orig *gmailMessage) string {
	t := orig.time()
	if d, err := mail.ParseDate(orig.header("Date")); err == nil {
		t = d
	}
	return t.Format("Mon, Jan 2, 2006 at 3:04 PM")
}

// deliver sends m (in threadID's conversation, if given), or saves it as a
// draft.
func (c *gmailClient) deliver(m *outgoing, threadID string, draft bool) error {
	raw := m.bytes()
	if len(raw) > maxGmailMessage {
		return fmt.Errorf("the message is %s; Gmail takes about 25 MB of attachments per message", humanSize(int64(len(raw))))
	}
	who := displayAddresses(m.To)
	if len(m.Cc) > 0 {
		who += ", cc " + displayAddresses(m.Cc)
	}
	if len(m.Bcc) > 0 {
		who += ", bcc " + displayAddresses(m.Bcc)
	}
	meta := map[string]any{}
	if threadID != "" {
		meta["threadId"] = threadID
	}
	if draft {
		var d struct {
			ID      string       `json:"id"`
			Message gmailMessage `json:"message"`
		}
		if err := c.upload("/drafts", map[string]any{"message": meta}, raw, &d); err != nil {
			return err
		}
		fmt.Printf("Saved a draft of %q to %s (id %s).\n", m.Subject, who, d.Message.ID)
		fmt.Printf("It is in the user's Gmail drafts; `delta gmail send-draft %s` sends it.\n", d.Message.ID)
		return nil
	}
	var sent gmailMessage
	if err := c.upload("/messages/send", meta, raw, &sent); err != nil {
		return err
	}
	fmt.Printf("Sent %q from %s to %s (id %s).\n", m.Subject, m.From.Address, who, sent.ID)
	return nil
}

func gmailSendDraftCmd(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: delta gmail send-draft <id>")
	}
	id, err := needMessageID(args[0])
	if err != nil {
		return err
	}
	c, err := newGmailClient()
	if err != nil {
		return err
	}
	draftID := ""
	page := ""
	for draftID == "" {
		var res struct {
			Drafts []struct {
				ID      string       `json:"id"`
				Message gmailMessage `json:"message"`
			} `json:"drafts"`
			NextPageToken string `json:"nextPageToken"`
		}
		q := map[string][]string{"maxResults": {"500"}}
		if page != "" {
			q["pageToken"] = []string{page}
		}
		if err := c.get("/drafts", q, &res); err != nil {
			return err
		}
		for _, d := range res.Drafts {
			if d.Message.ID == id {
				draftID = d.ID
			}
		}
		if page = res.NextPageToken; page == "" {
			break
		}
	}
	if draftID == "" {
		return fmt.Errorf("%s isn't a draft (it may have been sent or deleted already)", id)
	}
	var sent gmailMessage
	if err := c.post("/drafts/send", map[string]string{"id": draftID}, &sent); err != nil {
		return err
	}
	m, err := c.message(sent.ID, "metadata")
	if err != nil {
		fmt.Printf("Sent the draft (id %s).\n", sent.ID)
		return nil
	}
	fmt.Printf("Sent %q to %s (id %s).\n", displayHeader(m.header("Subject")), displayAddressHeader(m.header("To")), sent.ID)
	return nil
}
