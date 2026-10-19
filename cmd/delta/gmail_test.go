package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStripQuoted(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Sounds good.\n\nOn Mon, Oct 12, 2026 at 3:04 PM Amy <amy@x.com> wrote:\n> Lunch?\n>\n> Amy\n", "Sounds good."},
		// Gmail wraps a long attribution line.
		{"Yes.\n\nOn Mon, Oct 12, 2026 at 3:04 PM Amy Longname <\namy.longname@example.com> wrote:\n\n> Lunch?", "Yes."},
		{"Thanks!\n\n-----Original Message-----\nFrom: Bob\nSent: Monday\n\nHi", "Thanks!"},
		{"Thanks!\n\n________________________________\nFrom: Bob <b@x.com>\nSent: Monday", "Thanks!"},
		{"Thanks!\n\nFrom: Bob <b@x.com>\nSent: Monday, October 12\nTo: me", "Thanks!"},
	}
	for _, c := range cases {
		got, ok := stripQuoted(c.in)
		if !ok || got != c.want {
			t.Errorf("stripQuoted(%q) = %q, %v; want %q", c.in, got, ok, c.want)
		}
	}
	for _, keep := range []string{
		"No quotes here.\nJust text.",
		"> a quote in the middle\nand an answer below it",
		"---------- Forwarded message ---------\nFrom: Amy\n\nHello",
	} {
		if got, ok := stripQuoted(keep); ok || got != keep {
			t.Errorf("stripQuoted(%q) = %q, %v; want it unchanged", keep, got, ok)
		}
	}
}

func TestQuote(t *testing.T) {
	if got, want := quote("Hi\n\n> earlier\nBye\n"), "> Hi\n>\n>> earlier\n> Bye"; got != want {
		t.Errorf("quote = %q, want %q", got, want)
	}
}

func TestToUTF8(t *testing.T) {
	if got := toUTF8([]byte("caf\xe9 \x93ok\x94"), "windows-1252"); got != "café “ok”" {
		t.Errorf("windows-1252 = %q", got)
	}
	if got := toUTF8([]byte("caf\xc3\xa9"), "ISO-8859-1"); got != "cafÃ©" {
		t.Errorf("latin-1 = %q", got)
	}
	if got := toUTF8([]byte("caf\xc3\xa9 \xff"), "utf-8"); got != "café \uFFFD" {
		t.Errorf("utf-8 = %q", got)
	}
}

func TestNeedMessageID(t *testing.T) {
	for _, ok := range []string{"19a2b3c4d5e6f708", "18C3F2A1B2C3D4E5"} {
		if _, err := needMessageID(ok); err != nil {
			t.Errorf("needMessageID(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../labels", "hello world", "19a2b3c4d5e6f708/x"} {
		if _, err := needMessageID(bad); err == nil {
			t.Errorf("needMessageID(%q) succeeded", bad)
		}
	}
}

func TestSafeFilename(t *testing.T) {
	cases := map[string]string{"report.pdf": "report.pdf", "../../etc/passwd": "passwd", "a:b?.txt": "a_b_.txt", "..": "attachment", ".hidden": "hidden"}
	for in, want := range cases {
		if got := safeFilename(in); got != want {
			t.Errorf("safeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

// parseRaw reads a message as a mail client would: headers decoded, and the
// text, HTML and attachments (by file name) of its MIME tree.
func parseRaw(t *testing.T, raw []byte) (mail.Header, map[string]string) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v\n%s", err, raw)
	}
	parts := map[string]string{}
	var walk func(h map[string][]string, body io.Reader)
	walk = func(h map[string][]string, body io.Reader) {
		get := func(k string) string {
			if v := h[k]; len(v) > 0 {
				return v[0]
			}
			return ""
		}
		mt, params, err := mime.ParseMediaType(get("Content-Type"))
		if err != nil {
			t.Fatalf("Content-Type %q: %v", get("Content-Type"), err)
		}
		if strings.HasPrefix(mt, "multipart/") {
			mr := multipart.NewReader(body, params["boundary"])
			for {
				p, err := mr.NextRawPart()
				if err == io.EOF {
					return
				} else if err != nil {
					t.Fatal(err)
				}
				walk(p.Header, p)
			}
		}
		var data []byte
		switch get("Content-Transfer-Encoding") {
		case "base64":
			data, err = io.ReadAll(base64.NewDecoder(base64.StdEncoding, body))
		default:
			if get("Content-Transfer-Encoding") == "quoted-printable" {
				body = quotedprintable.NewReader(body)
			}
			data, err = io.ReadAll(body)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, dp, _ := mime.ParseMediaType(get("Content-Disposition"))
		if name := dp["filename"]; name != "" {
			parts[name] = string(data)
		} else {
			parts[mt] = string(data)
		}
	}
	walk(msg.Header, msg.Body)
	return msg.Header, parts
}

func TestOutgoingBytes(t *testing.T) {
	m := &outgoing{
		From:    &mail.Address{Name: "Sam Ö", Address: "me@gmail.com"},
		To:      []*mail.Address{{Name: "Amy", Address: "amy@x.com"}},
		Bcc:     []*mail.Address{{Address: "hidden@x.com"}},
		Subject: "Grüße – a long subject line that goes on and on and on to test header encoding",
		Text:    "Hello Amy,\n\nA line with = and a very long line " + strings.Repeat("word ", 40) + "\nBye.",
		HTML:    "<p>Hello</p>",
		Attachments: []outAttachment{
			{Name: "résumé.pdf", Type: "application/pdf", Data: bytes.Repeat([]byte{0, 1, 2, 255}, 100)},
			{Name: "logo.png", Type: "image/png", ContentID: "logo@x", Inline: true, Data: []byte("png")},
		},
		InReplyTo: "<abc@mail.gmail.com>",
	}
	h, parts := parseRaw(t, m.bytes())
	dec := &mime.WordDecoder{}
	if s, _ := dec.DecodeHeader(h.Get("Subject")); s != m.Subject {
		t.Errorf("Subject = %q", s)
	}
	if from, err := h.AddressList("From"); err != nil || from[0].Name != "Sam Ö" {
		t.Errorf("From = %v, %v", from, err)
	}
	if h.Get("Bcc") != "<hidden@x.com>" || h.Get("In-Reply-To") != "<abc@mail.gmail.com>" {
		t.Errorf("headers = %v", h)
	}
	if got := strings.ReplaceAll(parts["text/plain"], "\r\n", "\n"); got != m.Text {
		t.Errorf("text = %q", got)
	}
	if parts["text/html"] != m.HTML {
		t.Errorf("html = %q", parts["text/html"])
	}
	if parts["résumé.pdf"] != string(m.Attachments[0].Data) || parts["logo.png"] != "png" {
		t.Errorf("attachments = %q", parts)
	}
	for _, line := range strings.Split(string(m.bytes()), "\r\n") {
		if len(line) > 998 {
			t.Errorf("line longer than 998 bytes: %q", line[:80])
		}
	}
}

// ---- end to end, against fake servers ----

type fakeGmail struct {
	t        *testing.T
	mu       sync.Mutex
	messages map[string]map[string]any // by id, in Gmail's format=full JSON
	sent     [][]byte                  // raw messages uploaded
	meta     []map[string]any          // their metadata
	modified []string                  // "threadID addLabels-removeLabels" or "threadID trash"

	exchanged url.Values // the login's code exchange
	saved     gmailLogin // the login it saved
}

// gmailFixture starts a command center serving a Gmail login, Google's
// token endpoint and a Gmail API holding the given messages.
func gmailFixture(t *testing.T, messages ...map[string]any) *fakeGmail {
	f := &fakeGmail{t: t, messages: map[string]map[string]any{}}
	for _, m := range messages {
		f.messages[m["id"].(string)] = m
	}
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gmail-login" || r.Header.Get("Authorization") != "Bearer delta_test" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			f.mu.Lock()
			json.NewDecoder(r.Body).Decode(&f.saved)
			f.mu.Unlock()
			w.Write([]byte(`{"email":"me@gmail.com"}`))
			return
		}
		json.NewEncoder(w).Encode(gmailLogin{ClientID: "cid", ClientSecret: "csecret", RefreshToken: "rtok", Email: "me@gmail.com"})
	}))
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") == "authorization_code" {
			f.mu.Lock()
			f.exchanged = r.Form
			f.mu.Unlock()
			if r.Form.Get("code") != "the-code" || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			w.Write([]byte(`{"access_token":"atok","refresh_token":"new-rtok","scope":"` + gmailScope + `"}`))
			return
		}
		if r.Form.Get("refresh_token") != "rtok" || r.Form.Get("client_secret") != "csecret" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Write([]byte(`{"access_token":"atok","expires_in":3599}`))
	}))
	api := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() { cc.Close(); token.Close(); api.Close() })
	t.Setenv("DELTA_SERVER", cc.URL)
	t.Setenv("DELTA_TOKEN", "delta_test")
	t.Setenv("DELTA_TZ", "America/Phoenix")
	oldAPI, oldToken := gmailAPI, googleTokenURL
	gmailAPI, googleTokenURL = api.URL, token.URL
	t.Cleanup(func() { gmailAPI, googleTokenURL = oldAPI, oldToken })
	return f
}

func (f *fakeGmail) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer atok" {
		w.WriteHeader(401)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me")
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	thread := func(id string) map[string]any {
		var msgs []any
		for _, m := range f.messages {
			if m["threadId"] == id {
				msgs = append(msgs, m)
			}
		}
		if len(msgs) == 0 {
			return nil
		}
		// In order of internalDate, as Gmail returns them.
		for i := range msgs {
			for j := i + 1; j < len(msgs); j++ {
				if msgs[j].(map[string]any)["internalDate"].(string) < msgs[i].(map[string]any)["internalDate"].(string) {
					msgs[i], msgs[j] = msgs[j], msgs[i]
				}
			}
		}
		return map[string]any{"id": id, "messages": msgs}
	}
	switch {
	case path == "/profile":
		reply(map[string]any{"emailAddress": "me@gmail.com", "messagesTotal": 10, "threadsTotal": 5})
	case path == "/settings/sendAs":
		reply(map[string]any{"sendAs": []any{
			map[string]any{"sendAsEmail": "me@gmail.com", "displayName": "Sam", "isDefault": true, "isPrimary": true},
			map[string]any{"sendAsEmail": "sam@school.edu", "displayName": "Sam S", "verificationStatus": "accepted"},
		}})
	case path == "/labels":
		reply(map[string]any{"labels": []any{
			map[string]any{"id": "INBOX", "name": "INBOX", "type": "system"},
			map[string]any{"id": "UNREAD", "name": "UNREAD", "type": "system"},
			map[string]any{"id": "Label_1", "name": "Work", "type": "user"},
		}})
	case path == "/threads" && r.Method == "GET":
		seen := map[string]bool{}
		var ts []any
		for _, m := range f.messages {
			if id := m["threadId"].(string); !seen[id] {
				seen[id] = true
				ts = append(ts, map[string]any{"id": id})
			}
		}
		reply(map[string]any{"threads": ts})
	case strings.HasPrefix(path, "/threads/") && r.Method == "GET":
		th := thread(strings.TrimPrefix(path, "/threads/"))
		if th == nil {
			w.WriteHeader(404)
			return
		}
		reply(th)
	case strings.HasPrefix(path, "/threads/") && r.Method == "POST":
		parts := strings.Split(strings.TrimPrefix(path, "/threads/"), "/")
		if parts[1] == "modify" {
			var body struct{ AddLabelIds, RemoveLabelIds []string }
			json.NewDecoder(r.Body).Decode(&body)
			f.modified = append(f.modified, parts[0]+" +"+strings.Join(body.AddLabelIds, ",")+" -"+strings.Join(body.RemoveLabelIds, ","))
		} else {
			f.modified = append(f.modified, parts[0]+" "+parts[1])
		}
		reply(map[string]any{"id": parts[0]})
	case strings.Contains(path, "/attachments/"):
		reply(map[string]any{"data": base64.URLEncoding.EncodeToString([]byte("attached bytes"))})
	case strings.HasPrefix(path, "/messages/"):
		m, ok := f.messages[strings.TrimPrefix(path, "/messages/")]
		if !ok {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"code":404,"message":"Requested entity was not found."}}`))
			return
		}
		reply(m)
	case r.URL.Path == "/upload/gmail/v1/users/me/messages/send" || r.URL.Path == "/upload/gmail/v1/users/me/drafts":
		if r.URL.Query().Get("uploadType") != "multipart" {
			f.t.Errorf("upload type = %q", r.URL.Query().Get("uploadType"))
		}
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		p, _ := mr.NextPart()
		var meta map[string]any
		json.NewDecoder(p).Decode(&meta)
		p, _ = mr.NextPart()
		if ct := p.Header.Get("Content-Type"); ct != "message/rfc822" {
			f.t.Errorf("message part type = %q", ct)
		}
		raw, _ := io.ReadAll(p)
		f.sent = append(f.sent, raw)
		f.meta = append(f.meta, meta)
		if strings.HasSuffix(r.URL.Path, "/drafts") {
			reply(map[string]any{"id": "r123", "message": map[string]any{"id": "aaaa0000bbbb1111"}})
		} else {
			reply(map[string]any{"id": "cccc2222dddd3333", "threadId": meta["threadId"]})
		}
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(404)
	}
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// testMessage builds a message in Gmail's format=full shape.
func testMessage(id, thread string, minute int, labels []string, headers map[string]string, payload map[string]any) map[string]any {
	var hs []any
	for k, v := range headers {
		hs = append(hs, map[string]any{"name": k, "value": v})
	}
	payload["headers"] = hs
	date := time.Date(2026, 10, 13, 15, minute, 0, 0, time.UTC)
	return map[string]any{
		"id": id, "threadId": thread, "labelIds": labels, "snippet": "Snippet &amp; more",
		"internalDate": stringInt(date.UnixMilli()), "payload": payload,
	}
}

func stringInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func textPayload(text string) map[string]any {
	return map[string]any{"mimeType": "text/plain", "body": map[string]any{"data": b64(text), "size": len(text)}}
}

// captureStdout runs fn and returns what it printed.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { data, _ := io.ReadAll(r); done <- string(data) }()
	err = fn()
	w.Close()
	os.Stdout = old
	return <-done, err
}

// A conversation: Amy writes to me and Bob, copying Carol and my school
// address; I reply; Amy replies again.
func conversation() []map[string]any {
	return []map[string]any{
		testMessage("1000000000000001", "1000000000000001", 0, []string{"INBOX"}, map[string]string{
			"From": "Amy Adams <amy@x.com>", "To": "Bob <bob@y.org>", "Cc": "carol@z.net, sam@school.edu",
			"Subject": "Lunch?", "Message-ID": "<m1@x.com>", "Date": "Tue, 13 Oct 2026 08:00:00 -0700",
		}, textPayload("Lunch on Thursday?\n\nAmy")),
		testMessage("1000000000000002", "1000000000000001", 5, []string{"SENT"}, map[string]string{
			"From": "Sam <me@gmail.com>", "To": "Amy Adams <amy@x.com>", "Subject": "Re: Lunch?",
			"Message-ID": "<m2@gmail.com>", "References": "<m1@x.com>",
		}, textPayload("Sure.\n\nOn Tue, Oct 13, 2026 at 8:00 AM Amy Adams <amy@x.com> wrote:\n> Lunch on Thursday?\n>\n> Amy")),
		testMessage("1000000000000003", "1000000000000001", 10, []string{"INBOX", "UNREAD", "Label_1"}, map[string]string{
			"From": "Amy Adams <amy@x.com>", "Reply-To": "amy.adams@work.com", "To": "Sam <me@gmail.com>", "Cc": "Bob <bob@y.org>",
			"Subject": "Re: Lunch?", "Message-ID": "<m3@x.com>", "References": "<m1@x.com> <m2@gmail.com>",
		}, map[string]any{"mimeType": "multipart/mixed", "parts": []any{
			map[string]any{"mimeType": "multipart/alternative", "parts": []any{
				textPayload("Great, noon at the usual place.\n\n> Sure."),
				map[string]any{"mimeType": "text/html", "headers": []any{map[string]any{"name": "Content-Type", "value": "text/html; charset=UTF-8"}},
					"body": map[string]any{"data": b64("<html><body><p>Great, <b>noon</b> at the usual place.</p></body></html>")}},
			}},
			map[string]any{"mimeType": "application/pdf", "filename": "menu.pdf", "body": map[string]any{"attachmentId": "att1", "size": 14}},
		}}),
	}
}

func TestGmailSearchAndRead(t *testing.T) {
	gmailFixture(t, conversation()...)
	if _, err := captureStdout(t, func() error {
		return run([]string{"gmail", "search", "is:unread", "-category:promotions", "-n", "5"})
	}); err != nil {
		t.Errorf("search with an exclusion: %v", err)
	}
	out, err := captureStdout(t, func() error { return run([]string{"gmail", "search"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"me@gmail.com: in:inbox",
		"1000000000000003  Tue Oct 13 8:10am MST",
		"UNREAD",
		"  From Amy Adams <amy@x.com>  (3 messages)\n  Subject: Re: Lunch?\n  Snippet & more\n  Labels: Inbox, Sent, Work\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("search output lacks %q:\n%s", want, out)
		}
	}

	out, err = captureStdout(t, func() error { return run([]string{"gmail", "read", "1000000000000003"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Subject: Re: Lunch?") {
		t.Errorf("read repeats the subject:\n%s", out)
	}
	for _, want := range []string{
		"Conversation: Lunch?\n3 message(s)",
		"======== [2/3] 1000000000000002\nFrom: Sam <me@gmail.com>",
		"Sure.\n\n[quoted text hidden]",
		"======== [3/3] 1000000000000003  (unread, the id you asked for)",
		"Reply-To: amy.adams@work.com",
		"  1. menu.pdf (application/pdf, 14 B)",
		"Great, noon at the usual place.\n\n[quoted text hidden]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("read output lacks %q:\n%s", want, out)
		}
	}
}

func TestGmailReplyAll(t *testing.T) {
	f := gmailFixture(t, conversation()...)
	out, err := captureStdout(t, func() error {
		return run([]string{"gmail", "reply", "1000000000000001", "--all", "--body", "Thursday works.", "--cc", "dave@d.com"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.meta[0]["threadId"] != "1000000000000001" {
		t.Fatalf("sent %d, meta %v", len(f.sent), f.meta)
	}
	h, parts := parseRaw(t, f.sent[0])
	// Sent from the school address, which the message was copied to.
	checks := map[string]string{
		"From": `"Sam S" <sam@school.edu>`, "To": `"Amy Adams" <amy@x.com>`,
		"Cc": `"Bob" <bob@y.org>, <carol@z.net>, <dave@d.com>`, "Subject": "Re: Lunch?",
		"In-Reply-To": "<m1@x.com>", "References": "<m1@x.com>",
	}
	for k, want := range checks {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	text := strings.ReplaceAll(parts["text/plain"], "\r\n", "\n")
	if want := "Thursday works.\n\nOn Tue, Oct 13, 2026 at 8:00 AM Amy Adams <amy@x.com> wrote:\n> Lunch on Thursday?\n>\n> Amy\n"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if !strings.Contains(out, `Sent "Re: Lunch?" from sam@school.edu to Amy Adams <amy@x.com>, cc Bob <bob@y.org>, carol@z.net, dave@d.com`) {
		t.Errorf("output = %q", out)
	}
}

func TestGmailReplyTargets(t *testing.T) {
	f := gmailFixture(t, conversation()...)
	// Replying to my own message goes to its recipients.
	if _, err := captureStdout(t, func() error {
		return run([]string{"gmail", "reply", "1000000000000002", "--body", "Also, Friday?"})
	}); err != nil {
		t.Fatal(err)
	}
	// Reply-To wins over From; replying (not all) copies no one.
	if _, err := captureStdout(t, func() error {
		return run([]string{"gmail", "reply", "1000000000000003", "--body-file", "-"})
	}); err == nil {
		t.Error("an empty body was accepted")
	}
	if _, err := captureStdout(t, func() error {
		return run([]string{"gmail", "reply", "1000000000000003", "--body", "See you", "--draft"})
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 2 {
		t.Fatalf("sent %d", len(f.sent))
	}
	h, _ := parseRaw(t, f.sent[0])
	if h.Get("To") != `"Amy Adams" <amy@x.com>` || h.Get("From") != `"Sam" <me@gmail.com>` || h.Get("References") != "<m1@x.com> <m2@gmail.com>" {
		t.Errorf("reply to own message: %v", h)
	}
	h, _ = parseRaw(t, f.sent[1])
	if h.Get("To") != "<amy.adams@work.com>" || h.Get("Cc") != "" {
		t.Errorf("reply with Reply-To: %v", h)
	}
	if f.meta[1]["message"].(map[string]any)["threadId"] != "1000000000000001" {
		t.Errorf("draft metadata = %v", f.meta[1])
	}
}

func TestGmailForward(t *testing.T) {
	f := gmailFixture(t, conversation()...)
	extra := t.TempDir() + "/notes.txt"
	os.WriteFile(extra, []byte("my notes"), 0o644)
	if _, err := captureStdout(t, func() error {
		return run([]string{"gmail", "forward", "1000000000000003", "--to", "Eve <eve@e.com>", "--body", "FYI", "--attach", extra})
	}); err != nil {
		t.Fatal(err)
	}
	h, parts := parseRaw(t, f.sent[0])
	if h.Get("Subject") != "Fwd: Re: Lunch?" || h.Get("To") != `"Eve" <eve@e.com>` {
		t.Errorf("headers = %v", h)
	}
	text := strings.ReplaceAll(parts["text/plain"], "\r\n", "\n")
	for _, want := range []string{"FYI\n\n---------- Forwarded message ---------\nFrom: Amy Adams <amy@x.com>\n", "Subject: Re: Lunch?\nTo: Sam <me@gmail.com>\nCc: Bob <bob@y.org>\n\nGreat, noon"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(parts["text/html"], "Great, <b>noon</b>") || strings.Contains(parts["text/html"], "<html>") {
		t.Errorf("html = %q", parts["text/html"])
	}
	if parts["menu.pdf"] != "attached bytes" || parts["notes.txt"] != "my notes" {
		t.Errorf("attachments = %v", parts)
	}
}

func TestGmailModify(t *testing.T) {
	f := gmailFixture(t, conversation()...)
	out, err := captureStdout(t, func() error {
		return run([]string{"gmail", "archive", "1000000000000002", "1000000000000003"})
	})
	if err != nil {
		t.Fatal(err)
	}
	// Both ids are in one conversation, so it is archived once.
	if len(f.modified) != 1 || f.modified[0] != "1000000000000001 + -INBOX" {
		t.Errorf("modified = %v", f.modified)
	}
	if !strings.Contains(out, `Archived: "Re: Lunch?" from Sam (1000000000000002)`) {
		t.Errorf("output = %q", out)
	}
	for _, c := range [][]string{{"mark", "unread", "1000000000000001"}, {"trash", "1000000000000001"}, {"label", "work", "1000000000000001"}} {
		if _, err := captureStdout(t, func() error { return run(append([]string{"gmail"}, c...)) }); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"1000000000000001 + -INBOX", "1000000000000001 +UNREAD -", "1000000000000001 trash", "1000000000000001 +Label_1 -"}
	if strings.Join(f.modified, "|") != strings.Join(want, "|") {
		t.Errorf("modified = %q, want %q", f.modified, want)
	}
	_, err = captureStdout(t, func() error { return run([]string{"gmail", "label", "Nope", "1000000000000001"}) })
	if err == nil || !strings.Contains(err.Error(), `no label named "Nope" (the user's labels: Work); add --create`) {
		t.Errorf("unknown label: %v", err)
	}
	_, err = captureStdout(t, func() error { return run([]string{"gmail", "trash", "ffff000000000000"}) })
	if err == nil {
		t.Error("trashing a missing message succeeded")
	}
}

func TestGmailExpiredLogin(t *testing.T) {
	gmailFixture(t)
	old := googleTokenURL
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
	}))
	defer ts.Close()
	googleTokenURL = ts.URL
	defer func() { googleTokenURL = old }()
	if _, err := newGmailClient(); !errors.Is(err, errGmailLoginExpired) {
		t.Errorf("err = %v", err)
	}
}

func TestGmailLogin(t *testing.T) {
	f := gmailFixture(t)
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	secret := filepath.Join(t.TempDir(), "client_secret.json")
	os.WriteFile(secret, []byte(`{"installed":{"client_id":"cid","client_secret":"csecret","redirect_uris":["http://localhost"]}}`), 0o600)

	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	done := make(chan error, 1)
	go func() { done <- run([]string{"gmail", "login", secret}) }()
	// Play the browser: open the printed address's redirect with a code.
	var auth *url.URL
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, googleAuthURL) {
			auth, _ = url.Parse(line)
			break
		}
	}
	go io.Copy(io.Discard, r)
	if auth == nil {
		os.Stdout = old
		t.Fatal("no sign-in address printed")
	}
	q := auth.Query()
	if q.Get("scope") != gmailScope+" "+strings.Join(peopleScopes, " ") || q.Get("access_type") != "offline" || q.Get("code_challenge_method") != "S256" {
		t.Errorf("auth URL = %s", auth)
	}
	resp, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{"code": {"the-code"}, "state": {q.Get("state")}, "scope": {gmailScope}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	err = <-done
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	if f.exchanged.Get("redirect_uri") != q.Get("redirect_uri") {
		t.Errorf("exchange = %v", f.exchanged)
	}
	if want := (gmailLogin{ClientID: "cid", ClientSecret: "csecret", RefreshToken: "new-rtok", Email: "me@gmail.com"}); f.saved != want {
		t.Errorf("saved %+v, want %+v", f.saved, want)
	}
}

func TestOAuthClientFile(t *testing.T) {
	dir := t.TempDir()
	web := filepath.Join(dir, "web.json")
	os.WriteFile(web, []byte(`{"web":{"client_id":"a","client_secret":"b"}}`), 0o600)
	if _, _, err := oauthClient(web); err == nil || !strings.Contains(err.Error(), "Desktop app") {
		t.Errorf("web client: %v", err)
	}
	empty := filepath.Join(dir, "empty.json")
	os.WriteFile(empty, []byte(`{}`), 0o600)
	if _, _, err := oauthClient(empty); err == nil {
		t.Error("empty client file accepted")
	}
}
