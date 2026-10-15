package main

import (
	"errors"
	"fmt"
	"html"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxGmailResults = 50
	// Gmail takes messages up to about 35 MB once encoded, which is about
	// 25 MB of attachments.
	maxGmailMessage = 35 << 20
	// Bodies longer than this are cut when read, unless --full.
	maxGmailBody = 20000
)

var messageIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{6,32}$`)

func needMessageID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !messageIDPattern.MatchString(s) {
		return "", fmt.Errorf("%q isn't a message id (ids are like 19a2b3c4d5e6f708; get them from `delta gmail search`)", s)
	}
	return strings.ToLower(s), nil
}

// ---- search ----

func gmailSearchCmd(args []string) error {
	// Anything but search's own options is the query, which may hold
	// exclusions such as -category:promotions.
	var flags, terms []string
	for i := 0; i < len(args); i++ {
		name, _, hasVal := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if !strings.HasPrefix(args[i], "-") || !slices.Contains([]string{"n", "limit", "page", "json"}, name) {
			terms = append(terms, args[i])
			continue
		}
		flags = append(flags, args[i])
		if name != "json" && !hasVal && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	o, err := parseOpts(flags, "n limit page", "")
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(terms, " "))
	if query == "" {
		query = "in:inbox"
	}
	limit, err := o.int("limit", 0)
	if err == nil && limit == 0 {
		limit, err = o.int("n", 10)
	}
	if err != nil {
		return err
	}
	if limit < 1 || limit > maxGmailResults {
		return fmt.Errorf("--limit must be between 1 and %d", maxGmailResults)
	}
	c, err := newGmailClient()
	if err != nil {
		return err
	}
	q := url.Values{"q": {query}, "maxResults": {strconv.Itoa(limit)}}
	if p := o.str("page"); p != "" {
		q.Set("pageToken", p)
	}
	var res struct {
		Threads []struct {
			ID string `json:"id"`
		} `json:"threads"`
		NextPageToken string `json:"nextPageToken"`
		Estimate      int    `json:"resultSizeEstimate"`
	}
	if err := c.get("/threads", q, &res); err != nil {
		return err
	}
	threads := make([]*gmailThread, len(res.Threads))
	err = forEach(res.Threads, func(i int, t struct {
		ID string `json:"id"`
	}) error {
		th, err := c.thread(t.ID, "metadata")
		if isNotFound(err) { // deleted meanwhile
			return nil
		}
		threads[i] = th
		return err
	})
	if err != nil {
		return err
	}
	labels, _ := c.labelList()
	if o.bools["json"] {
		return printJSON(map[string]any{"threads": threads, "nextPageToken": res.NextPageToken})
	}

	loc := gmailUserLocation()
	fmt.Printf("%s: %s\n", c.email, query)
	shown := 0
	for _, t := range threads {
		if t == nil || t.latest() == nil {
			continue
		}
		shown++
		m := t.latest()
		fmt.Println()
		flags := ""
		for _, id := range t.labelIDs() {
			if id == "UNREAD" {
				flags = "  UNREAD"
			}
		}
		fmt.Printf("%s  %s%s\n", m.ID, fmtMailTime(m.time(), loc), flags)
		fmt.Printf("  %s\n", participants(c, t))
		fmt.Printf("  Subject: %s\n", firstNonEmpty(displayHeader(m.header("Subject")), "(no subject)"))
		if s := strings.TrimSpace(html.UnescapeString(m.Snippet)); s != "" {
			fmt.Printf("  %s\n", shorten(s, 160))
		}
		if names := labelNames(t.labelIDs(), labels); len(names) > 0 {
			fmt.Printf("  Labels: %s\n", strings.Join(names, ", "))
		}
	}
	if shown == 0 {
		fmt.Println("\nNo conversations found.")
		return nil
	}
	if res.NextPageToken != "" {
		fmt.Printf("\nMore: delta gmail search %s --page %s\n", shellQuote(query), res.NextPageToken)
	}
	return nil
}

// participants describes who is in a conversation, as Gmail's list does:
// the latest sender, and how many messages there are.
func participants(c *gmailClient, t *gmailThread) string {
	m := t.latest()
	from := headerAddresses(m.header("From"))
	s := displayHeader(m.header("From"))
	if len(from) > 0 {
		s = displayAddresses(from[:1])
		if c.isMe(from[0].Address) {
			s = "From me to " + firstNonEmpty(displayAddresses(headerAddresses(m.header("To"))), "(no recipients)")
		} else {
			s = "From " + s
		}
	}
	if n := len(t.Messages); n > 1 {
		s += fmt.Sprintf("  (%d messages)", n)
	}
	return s
}

// systemLabels names the system labels worth showing.
var systemLabels = map[string]string{
	"INBOX": "Inbox", "STARRED": "Starred", "IMPORTANT": "Important", "SENT": "Sent",
	"DRAFT": "Draft", "SPAM": "Spam", "TRASH": "Trash",
	"CATEGORY_SOCIAL": "Social", "CATEGORY_PROMOTIONS": "Promotions",
	"CATEGORY_UPDATES": "Updates", "CATEGORY_FORUMS": "Forums",
}

func labelNames(ids []string, labels []gmailLabel) []string {
	byID := map[string]string{}
	for _, l := range labels {
		if l.Type == "user" {
			byID[l.ID] = l.Name
		}
	}
	var sys, user []string
	for _, id := range ids {
		if n, ok := systemLabels[id]; ok {
			sys = append(sys, n)
		} else if n, ok := byID[id]; ok {
			user = append(user, n)
		}
	}
	sort.Strings(sys)
	sort.Strings(user)
	return append(sys, user...)
}

func fmtMailTime(t time.Time, loc *time.Location) string {
	t = t.In(loc)
	layout := "Mon Jan 2 3:04pm MST"
	if t.Year() != time.Now().Year() {
		layout = "Mon Jan 2 2006 3:04pm MST"
	}
	return t.Format(layout) + " (" + relTime(time.Until(t)) + ")"
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_:@./-]+$`)

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---- read ----

func gmailReadCmd(args []string) error {
	o, err := parseOpts(args, "", "full")
	if err != nil {
		return err
	}
	if len(o.pos) != 1 {
		return errors.New("usage: delta gmail read <id> [--full]")
	}
	c, err := newGmailClient()
	if err != nil {
		return err
	}
	m, err := c.message(o.pos[0], "minimal")
	if err != nil {
		return err
	}
	t, err := c.thread(m.ThreadID, "full")
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(t)
	}
	labels, _ := c.labelList()
	loc := gmailUserLocation()
	subject := ""
	if len(t.Messages) > 0 {
		subject = displayHeader(t.Messages[0].header("Subject"))
	}
	fmt.Printf("Conversation: %s\n", firstNonEmpty(subject, "(no subject)"))
	fmt.Printf("%d message(s)", len(t.Messages))
	if names := labelNames(t.labelIDs(), labels); len(names) > 0 {
		fmt.Printf("  Labels: %s", strings.Join(names, ", "))
	}
	fmt.Println()
	for i := range t.Messages {
		printMessage(&t.Messages[i], i, len(t.Messages), subject, m.ID, loc, o.bools["full"])
	}
	return nil
}

func printMessage(m *gmailMessage, i, n int, subject, asked string, loc *time.Location, full bool) {
	var notes []string
	if m.hasLabel("DRAFT") {
		notes = append(notes, "draft")
	}
	if m.hasLabel("UNREAD") {
		notes = append(notes, "unread")
	}
	if m.ID == asked && n > 1 {
		notes = append(notes, "the id you asked for")
	}
	note := ""
	if len(notes) > 0 {
		note = "  (" + strings.Join(notes, ", ") + ")"
	}
	fmt.Printf("\n======== [%d/%d] %s%s\n", i+1, n, m.ID, note)
	fmt.Printf("From: %s\n", displayAddressHeader(m.header("From")))
	for _, h := range []string{"To", "Cc", "Bcc", "Reply-To"} {
		if v := m.header(h); v != "" {
			fmt.Printf("%s: %s\n", h, displayAddressHeader(v))
		}
	}
	fmt.Printf("Date: %s\n", fmtMailTime(m.time(), loc))
	if s := displayHeader(m.header("Subject")); bareSubject(s) != bareSubject(subject) {
		fmt.Printf("Subject: %s\n", s)
	}
	content := m.content()
	if len(content.Attachments) > 0 {
		fmt.Println("Attachments (save with `delta gmail attachment " + m.ID + " [n]`):")
		for j, a := range content.Attachments {
			fmt.Printf("  %d. %s (%s, %s)\n", j+1, a.Name, a.Type, humanSize(a.Size))
		}
	}
	body := content.bodyText()
	if !full {
		if i > 0 {
			if cut, ok := stripQuoted(body); ok {
				body = cut + "\n\n[quoted text hidden]"
			}
		}
		if r := []rune(body); len(r) > maxGmailBody {
			body = string(r[:maxGmailBody]) + "\n\n[cut here; read it with --full for the rest]"
		}
	}
	fmt.Println()
	if body == "" {
		fmt.Println("(no text)")
	} else {
		fmt.Println(body)
	}
}

// bareSubject is a subject without its "Re:" and "Fwd:" prefixes.
func bareSubject(s string) string {
	for {
		t := strings.TrimSpace(s)
		if loc := replyPrefix.FindStringIndex(t); loc != nil {
			t = t[loc[1]:]
		} else if loc := forwardPrefix.FindStringIndex(t); loc != nil {
			t = t[loc[1]:]
		}
		if t == s {
			return t
		}
		s = t
	}
}

func displayAddressHeader(v string) string {
	if addrs := headerAddresses(v); len(addrs) > 0 {
		return displayAddresses(addrs)
	}
	return displayHeader(v)
}

// ---- attachments ----

func gmailAttachmentCmd(args []string) error {
	o, err := parseOpts(args, "o output-dir", "push")
	if err != nil {
		return err
	}
	if len(o.pos) < 1 {
		return errors.New("usage: delta gmail attachment <id> [n...] [-o dir] [--push]")
	}
	c, err := newGmailClient()
	if err != nil {
		return err
	}
	m, err := c.message(o.pos[0], "full")
	if err != nil {
		return err
	}
	atts := m.content().Attachments
	if len(atts) == 0 {
		return fmt.Errorf("message %s has no attachments", m.ID)
	}
	var picked []gmailAttachment
	for _, a := range o.pos[1:] {
		for _, f := range strings.Split(a, ",") {
			if f == "" {
				continue
			}
			i, err := strconv.Atoi(f)
			if err != nil || i < 1 || i > len(atts) {
				return fmt.Errorf("invalid attachment number %q (the message has 1-%d)", f, len(atts))
			}
			picked = append(picked, atts[i-1])
		}
	}
	if len(picked) == 0 {
		picked = atts
	}
	outDir, err := filepath.Abs(expandHome(firstNonEmpty(o.str("output-dir"), o.str("o"), ".")))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	for _, a := range picked {
		data, err := c.attachmentData(m.ID, a)
		if err != nil {
			return fmt.Errorf("%s: %w", a.Name, err)
		}
		dest, err := saveUnique(outDir, safeFilename(a.Name), data)
		if err != nil {
			return err
		}
		fmt.Printf("Saved %s (%s).\n", dest, humanSize(int64(len(data))))
		if o.bools["push"] {
			if err := push(dest, filepath.Base(dest)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *gmailClient) attachmentData(messageID string, a gmailAttachment) ([]byte, error) {
	if a.part.Body.AttachmentID == "" {
		return decodeData(a.part.Body.Data)
	}
	var res struct {
		Data string `json:"data"`
	}
	if err := c.get("/messages/"+messageID+"/attachments/"+url.PathEscape(a.part.Body.AttachmentID), nil, &res); err != nil {
		return nil, err
	}
	return decodeData(res.Data)
}

// saveUnique writes data to name in dir, adding " (2)", " (3)", ... if the
// name is taken.
func saveUnique(dir, name string, data []byte) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		dest := filepath.Join(dir, name)
		if i > 1 {
			dest = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		}
		f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dest)
			return "", err
		}
		return dest, nil
	}
}

// ---- trash, archive, mark, label ----

// gmailActions are the changes mark (and its shortcuts) can make to a
// conversation: labels to add and to remove.
var gmailActions = map[string]struct {
	add, remove []string
	done        string
}{
	"archive":     {nil, []string{"INBOX"}, "Archived"},
	"unarchive":   {[]string{"INBOX"}, nil, "Moved to the inbox"},
	"read":        {nil, []string{"UNREAD"}, "Marked read"},
	"unread":      {[]string{"UNREAD"}, nil, "Marked unread"},
	"starred":     {[]string{"STARRED"}, nil, "Starred"},
	"unstarred":   {nil, []string{"STARRED"}, "Unstarred"},
	"important":   {[]string{"IMPORTANT"}, nil, "Marked important"},
	"unimportant": {nil, []string{"IMPORTANT"}, "Marked not important"},
	"spam":        {[]string{"SPAM"}, []string{"INBOX"}, "Reported as spam"},
	"not-spam":    {[]string{"INBOX"}, []string{"SPAM"}, "Moved out of spam to the inbox"},
	"trash":       {nil, nil, "Moved to the trash"},
	"untrash":     {nil, nil, "Restored from the trash"},
}

var actionAliases = map[string]string{
	"star": "starred", "unstar": "unstarred", "not-important": "unimportant",
	"notspam": "not-spam", "not_spam": "not-spam", "ham": "not-spam", "inbox": "unarchive",
}

func gmailModifyCmd(action string, ids []string) error {
	if a, ok := actionAliases[action]; ok {
		action = a
	}
	act, ok := gmailActions[action]
	if !ok {
		return fmt.Errorf("unknown mark %q (read, unread, starred, unstarred, important, unimportant, spam or not-spam)", action)
	}
	if len(ids) == 0 {
		return fmt.Errorf("usage: delta gmail %s <id>...", action)
	}
	c, err := newGmailClient()
	if err != nil {
		return err
	}
	return c.eachConversation(ids, func(m *gmailMessage) error {
		var err error
		switch action {
		case "trash", "untrash":
			err = c.post("/threads/"+m.ThreadID+"/"+action, nil, nil)
		default:
			err = c.post("/threads/"+m.ThreadID+"/modify", map[string][]string{"addLabelIds": act.add, "removeLabelIds": act.remove}, nil)
		}
		if err == nil {
			fmt.Printf("%s: %s\n", act.done, describe(m))
		}
		return err
	})
}

// eachConversation looks up each message and runs fn on it, once per
// conversation, reporting failures as it goes.
func (c *gmailClient) eachConversation(ids []string, fn func(*gmailMessage) error) error {
	seen := map[string]bool{}
	failed := 0
	for _, id := range ids {
		m, err := c.message(id, "metadata")
		if err == nil && seen[m.ThreadID] {
			continue
		}
		if err == nil {
			seen[m.ThreadID] = true
			err = fn(m)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", id, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d failed", failed, len(ids))
	}
	return nil
}

func describe(m *gmailMessage) string {
	from := displayAddressHeader(m.header("From"))
	if addrs := headerAddresses(m.header("From")); len(addrs) > 0 {
		from = firstNonEmpty(addrs[0].Name, addrs[0].Address)
	}
	return fmt.Sprintf("%q from %s (%s)", shorten(firstNonEmpty(displayHeader(m.header("Subject")), "(no subject)"), 60), from, m.ID)
}

func gmailLabelCmd(cmd string, args []string) error {
	o, err := parseOpts(args, "", "create")
	if err != nil {
		return err
	}
	if len(o.pos) < 2 {
		return fmt.Errorf("usage: delta gmail %s <label> <id>...", cmd)
	}
	c, err := newGmailClient()
	if err != nil {
		return err
	}
	label, err := c.findLabel(o.pos[0], cmd == "label" && o.bools["create"])
	if err != nil {
		return err
	}
	body := map[string][]string{"addLabelIds": {label.ID}}
	done := "Labeled " + label.Name
	if cmd == "unlabel" {
		body = map[string][]string{"removeLabelIds": {label.ID}}
		done = "Removed " + label.Name + " from"
	}
	return c.eachConversation(o.pos[1:], func(m *gmailMessage) error {
		err := c.post("/threads/"+m.ThreadID+"/modify", body, nil)
		if err == nil {
			fmt.Printf("%s: %s\n", done, describe(m))
		}
		return err
	})
}

// findLabel finds a label by name (any case) or id, creating a user label
// if create is set and there is none.
func (c *gmailClient) findLabel(name string, create bool) (*gmailLabel, error) {
	labels, err := c.labelList()
	if err != nil {
		return nil, err
	}
	want := strings.TrimSpace(name)
	for _, l := range labels {
		display := l.Name
		if n, ok := systemLabels[l.ID]; ok {
			display = n
		}
		if strings.EqualFold(l.Name, want) || strings.EqualFold(display, want) || l.ID == want {
			l.Name = display
			return &l, nil
		}
	}
	if !create {
		var user []string
		for _, l := range labels {
			if l.Type == "user" {
				user = append(user, l.Name)
			}
		}
		sort.Strings(user)
		msg := fmt.Sprintf("no label named %q", want)
		if len(user) > 0 {
			msg += " (the user's labels: " + strings.Join(user, ", ") + ")"
		}
		return nil, errors.New(msg + "; add --create to make it")
	}
	var l gmailLabel
	err = c.post("/labels", map[string]string{"name": want, "labelListVisibility": "labelShow", "messageListVisibility": "show"}, &l)
	if err != nil {
		return nil, err
	}
	fmt.Printf("Created the label %s.\n", l.Name)
	return &l, nil
}

func gmailLabelsCmd(args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	if len(o.pos) != 0 {
		return errors.New("usage: delta gmail labels")
	}
	c, err := newGmailClient()
	if err != nil {
		return err
	}
	labels, err := c.labelList()
	if err != nil {
		return err
	}
	if o.bools["json"] {
		return printJSON(labels)
	}
	var user []string
	for _, l := range labels {
		if l.Type == "user" {
			user = append(user, l.Name)
		}
	}
	sort.Strings(user)
	fmt.Println("System: Inbox, Starred, Important, Sent, Draft, Spam, Trash, Unread")
	fmt.Println("Categories: Social, Promotions, Updates, Forums")
	if len(user) == 0 {
		fmt.Println("The user has no labels of their own.")
		return nil
	}
	fmt.Println("The user's labels:")
	for _, n := range user {
		fmt.Println("  " + n)
	}
	return nil
}

func gmailAccountCmd(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: delta gmail account")
	}
	c, err := newGmailClient()
	if err != nil {
		return err
	}
	var prof struct {
		Email    string `json:"emailAddress"`
		Messages int    `json:"messagesTotal"`
		Threads  int    `json:"threadsTotal"`
	}
	if err := c.get("/profile", nil, &prof); err != nil {
		return err
	}
	var inbox struct {
		ThreadsTotal  int `json:"threadsTotal"`
		ThreadsUnread int `json:"threadsUnread"`
	}
	if err := c.get("/labels/INBOX", nil, &inbox); err != nil {
		return err
	}
	fmt.Printf("Account: %s (%d conversations in all)\n", prof.Email, prof.Threads)
	fmt.Printf("Inbox: %d conversations, %d unread\n", inbox.ThreadsTotal, inbox.ThreadsUnread)
	sendAs, err := c.sendAsList()
	if err != nil {
		return err
	}
	fmt.Println("Sends as:")
	for _, s := range sendAs {
		def := ""
		if s.IsDefault {
			def = " (default)"
		}
		fmt.Printf("  %s%s\n", (&mail.Address{Name: s.DisplayName, Address: s.Email}).String(), def)
	}
	return nil
}

// fromFor picks the address to send from: one the original was sent to, if
// any, else the default.
func (c *gmailClient) fromFor(orig *gmailMessage) (*mail.Address, error) {
	list, err := c.sendAsList()
	if err != nil {
		return nil, err
	}
	pick := list[0]
	for _, s := range list {
		if s.IsDefault {
			pick = s
		}
	}
	if orig != nil {
		var to []*mail.Address
		for _, h := range []string{"To", "Cc", "Delivered-To"} {
			to = append(to, headerAddresses(orig.header(h))...)
		}
		var matched []gmailSendAs
		for _, a := range to {
			for _, s := range list {
				if strings.EqualFold(a.Address, s.Email) {
					matched = append(matched, s)
				}
			}
		}
		if len(matched) > 0 && !slices.ContainsFunc(matched, func(s gmailSendAs) bool { return s.IsDefault }) {
			pick = matched[0]
		}
	}
	return &mail.Address{Name: pick.DisplayName, Address: pick.Email}, nil
}
