package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// `delta gmail` reads and sends the user's Gmail with the Gmail API. The
// login (the user's OAuth client and the refresh token they granted it)
// lives on the command center (GET/PUT/DELETE /gmail-login). The CLI fetches
// it on every run, trades the refresh token for an hour-long access token
// with Google, and calls Gmail itself, so nothing is kept locally.
//
// Every id the agent sees is a message id. search shows each conversation by
// its latest message, read shows the whole conversation, and trash, archive,
// mark and label act on the whole conversation, as the Gmail app does; reply,
// forward and attachment work on the one message.

var (
	gmailAPI        = "https://gmail.googleapis.com"
	googleTokenURL  = "https://oauth2.googleapis.com/token"
	googleAuthURL   = "https://accounts.google.com/o/oauth2/v2/auth"
	googleRevokeURL = "https://oauth2.googleapis.com/revoke"
)

// gmail.modify covers reading, sending, drafts, labels and trash, but not
// deleting mail for good.
const gmailScope = "https://www.googleapis.com/auth/gmail.modify"

func runGmail(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(gmailHelp)
		return nil
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "search", "list", "ls", "inbox":
		return gmailSearchCmd(args)
	case "read", "show", "open":
		return gmailReadCmd(args)
	case "attachment", "attachments":
		return gmailAttachmentCmd(args)
	case "send":
		return gmailSendCmd(args)
	case "reply":
		return gmailReplyCmd(args)
	case "forward", "fwd":
		return gmailForwardCmd(args)
	case "send-draft":
		return gmailSendDraftCmd(args)
	case "trash", "delete", "rm":
		return gmailModifyCmd("trash", args)
	case "untrash", "restore":
		return gmailModifyCmd("untrash", args)
	case "archive":
		return gmailModifyCmd("archive", args)
	case "unarchive":
		return gmailModifyCmd("unarchive", args)
	case "mark":
		if len(args) == 0 {
			return errors.New("usage: delta gmail mark <read|unread|starred|unstarred|important|unimportant|spam|not-spam> <id>...")
		}
		return gmailModifyCmd(args[0], args[1:])
	case "label", "unlabel":
		return gmailLabelCmd(cmd, args)
	case "labels":
		return gmailLabelsCmd(args)
	case "account", "whoami":
		return gmailAccountCmd(args)
	case "login":
		return gmailLoginCmd(args)
	case "logout":
		return gmailLogoutCmd(args)
	default:
		return fmt.Errorf("unknown gmail command %q (run `delta gmail help`)", cmd)
	}
}

// ---- the login and the API client ----

type gmailLogin struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email"`
}

func fetchGmailLogin() (gmailLogin, error) {
	var l gmailLogin
	cfg, err := loadConfig()
	if err != nil {
		return l, err
	}
	data, err := do(cfg, http.MethodGet, "/gmail-login", nil)
	if err != nil {
		return l, err
	}
	err = json.Unmarshal(data, &l)
	return l, err
}

var errGmailLoginExpired = errors.New("the Gmail login has expired or was revoked (a password change does this); ask the user to run `delta gmail login` again on a computer")

// googleToken is a response from Google's OAuth token endpoint.
type googleToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

// requestToken posts form to the token endpoint.
func requestToken(form url.Values) (*googleToken, error) {
	resp, err := (&http.Client{Timeout: 30 * time.Second}).PostForm(googleTokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("Google can't be reached: %w", stripURL(err))
	}
	defer resp.Body.Close()
	var t googleToken
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return nil, fmt.Errorf("unexpected response from Google (HTTP %d)", resp.StatusCode)
	}
	switch {
	case t.Error == "invalid_grant":
		return nil, errGmailLoginExpired
	case t.Error == "invalid_client" || t.Error == "unauthorized_client":
		return nil, errors.New("Google no longer accepts the Gmail login's OAuth client (deleted, or its secret was reset); ask the user to run `delta gmail login` again on a computer")
	case t.Error != "":
		return nil, fmt.Errorf("Google refused the Gmail login: %s", firstNonEmpty(t.Description, t.Error))
	case t.AccessToken == "":
		return nil, fmt.Errorf("unexpected response from Google (HTTP %d)", resp.StatusCode)
	}
	return &t, nil
}

func stripURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

type gmailClient struct {
	http  *http.Client
	token string
	email string

	mu     sync.Mutex
	labels []gmailLabel // cached by labelList
	sendAs []gmailSendAs
}

func newGmailClient() (*gmailClient, error) {
	login, err := fetchGmailLogin()
	if err != nil {
		return nil, err
	}
	t, err := requestToken(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {login.RefreshToken},
		"client_id":     {login.ClientID},
		"client_secret": {login.ClientSecret},
	})
	if err != nil {
		return nil, err
	}
	return &gmailClient{http: &http.Client{Timeout: 5 * time.Minute}, token: t.AccessToken, email: login.Email}, nil
}

// gmailAPIError is an error response from Gmail.
type gmailAPIError struct {
	Code    int
	Message string
}

func (e *gmailAPIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Code)
	}
	switch e.Code {
	case http.StatusUnauthorized:
		return "Gmail rejected the login: " + msg + "; ask the user to run `delta gmail login` again on a computer"
	case http.StatusForbidden:
		if strings.Contains(strings.ToLower(msg), "scope") || strings.Contains(strings.ToLower(msg), "permission") {
			return "the Gmail login lacks a permission (" + msg + "); ask the user to run `delta gmail login` again and allow everything it asks for"
		}
	case http.StatusTooManyRequests:
		return "Gmail: too many requests; wait a minute and try again"
	}
	return fmt.Sprintf("Gmail: %s (HTTP %d)", msg, e.Code)
}

func isNotFound(err error) bool {
	var e *gmailAPIError
	return errors.As(err, &e) && e.Code == http.StatusNotFound
}

// do sends a request to the Gmail API (path is relative to
// /gmail/v1/users/me, or a full /upload/... path) and decodes the JSON
// response into out.
func (c *gmailClient) do(method, path string, query url.Values, contentType string, body io.Reader, out any) error {
	u := gmailAPI + path
	if !strings.HasPrefix(path, "/upload/") {
		u = gmailAPI + "/gmail/v1/users/me" + path
	}
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Gmail can't be reached: %w", stripURL(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(data, &e)
		return &gmailAPIError{Code: resp.StatusCode, Message: strings.TrimSpace(e.Error.Message)}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("unexpected response from Gmail: %w", err)
		}
	}
	return nil
}

func (c *gmailClient) get(path string, query url.Values, out any) error {
	return c.do(http.MethodGet, path, query, "", nil, out)
}

func (c *gmailClient) post(path string, body, out any) error {
	var rd io.Reader
	ct := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd, ct = bytes.NewReader(data), "application/json"
	}
	return c.do(http.MethodPost, path, nil, ct, rd, out)
}

// upload sends a raw message with its JSON metadata to a Gmail upload
// endpoint (messages/send or drafts), which takes messages up to 35 MB.
func (c *gmailClient) upload(path string, meta any, raw []byte, out any) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	pw, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	pw.Write(metaJSON)
	pw, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"message/rfc822"}})
	pw.Write(raw)
	mw.Close()
	q := url.Values{"uploadType": {"multipart"}}
	return c.do(http.MethodPost, "/upload/gmail/v1/users/me"+path, q, "multipart/related; boundary="+mw.Boundary(), &buf, out)
}

// ---- Gmail's resources ----

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailPart struct {
	PartID   string        `json:"partId"`
	MimeType string        `json:"mimeType"`
	Filename string        `json:"filename"`
	Headers  []gmailHeader `json:"headers"`
	Body     struct {
		AttachmentID string `json:"attachmentId"`
		Size         int64  `json:"size"`
		Data         string `json:"data"`
	} `json:"body"`
	Parts []gmailPart `json:"parts"`
}

// header returns the first header with this name.
func (p *gmailPart) header(name string) string {
	if p == nil {
		return ""
	}
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

type gmailMessage struct {
	ID           string     `json:"id"`
	ThreadID     string     `json:"threadId"`
	LabelIDs     []string   `json:"labelIds"`
	Snippet      string     `json:"snippet"`
	InternalDate string     `json:"internalDate"`
	Payload      *gmailPart `json:"payload"`
}

func (m *gmailMessage) header(name string) string { return m.Payload.header(name) }

func (m *gmailMessage) hasLabel(id string) bool {
	for _, l := range m.LabelIDs {
		if l == id {
			return true
		}
	}
	return false
}

// time is when Gmail received (or sent) the message.
func (m *gmailMessage) time() time.Time {
	var ms int64
	fmt.Sscan(m.InternalDate, &ms)
	return time.UnixMilli(ms)
}

type gmailThread struct {
	ID       string         `json:"id"`
	Snippet  string         `json:"snippet"`
	Messages []gmailMessage `json:"messages"`
}

// latest returns the conversation's latest message that isn't a draft (or
// its latest draft, if that is all it has).
func (t *gmailThread) latest() *gmailMessage {
	for i := len(t.Messages) - 1; i >= 0; i-- {
		if !t.Messages[i].hasLabel("DRAFT") {
			return &t.Messages[i]
		}
	}
	if len(t.Messages) > 0 {
		return &t.Messages[len(t.Messages)-1]
	}
	return nil
}

// labelIDs is every label on any message in the conversation.
func (t *gmailThread) labelIDs() []string {
	seen := map[string]bool{}
	var ids []string
	for _, m := range t.Messages {
		for _, l := range m.LabelIDs {
			if !seen[l] {
				seen[l] = true
				ids = append(ids, l)
			}
		}
	}
	return ids
}

type gmailLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type gmailSendAs struct {
	Email       string `json:"sendAsEmail"`
	DisplayName string `json:"displayName"`
	IsDefault   bool   `json:"isDefault"`
	IsPrimary   bool   `json:"isPrimary"`
	Status      string `json:"verificationStatus"`
}

func (c *gmailClient) message(id, format string) (*gmailMessage, error) {
	id, err := needMessageID(id)
	if err != nil {
		return nil, err
	}
	var m gmailMessage
	q := url.Values{"format": {format}}
	if format == "metadata" {
		q["metadataHeaders"] = []string{"Subject", "From", "To", "Date"}
	}
	if err := c.get("/messages/"+id, q, &m); isNotFound(err) {
		return nil, fmt.Errorf("no message with id %s (ids come from `delta gmail search` or `read`)", id)
	} else if err != nil {
		return nil, err
	}
	return &m, nil
}

func (c *gmailClient) thread(id, format string) (*gmailThread, error) {
	var t gmailThread
	q := url.Values{"format": {format}}
	if format == "metadata" {
		q["metadataHeaders"] = []string{"Subject", "From", "To", "Date"}
	}
	if err := c.get("/threads/"+url.PathEscape(id), q, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *gmailClient) labelList() ([]gmailLabel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.labels == nil {
		var res struct{ Labels []gmailLabel }
		if err := c.get("/labels", nil, &res); err != nil {
			return nil, err
		}
		c.labels = append([]gmailLabel{}, res.Labels...)
	}
	return c.labels, nil
}

// sendAsList returns the addresses the user can send from.
func (c *gmailClient) sendAsList() ([]gmailSendAs, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendAs == nil {
		var res struct {
			SendAs []gmailSendAs `json:"sendAs"`
		}
		if err := c.get("/settings/sendAs", nil, &res); err != nil {
			return nil, err
		}
		for _, s := range res.SendAs {
			if s.Status == "" || s.Status == "accepted" {
				c.sendAs = append(c.sendAs, s)
			}
		}
		if len(c.sendAs) == 0 {
			c.sendAs = []gmailSendAs{{Email: c.email, IsDefault: true, IsPrimary: true}}
		}
	}
	return c.sendAs, nil
}

// isMe reports whether addr is one of the user's own addresses.
func (c *gmailClient) isMe(addr string) bool {
	addr = strings.ToLower(addr)
	if addr == strings.ToLower(c.email) {
		return true
	}
	list, _ := c.sendAsList()
	for _, s := range list {
		if strings.ToLower(s.Email) == addr {
			return true
		}
	}
	return false
}

// gmailUserLocation is the user's time zone: DELTA_TZ, or the one their
// Canvas profile gave if `delta canvas` has run here, or this machine's.
func gmailUserLocation() *time.Location {
	tz := os.Getenv("DELTA_TZ")
	if tz == "" {
		if data, err := os.ReadFile(canvasCachePath()); err == nil {
			var cache canvasCache
			if json.Unmarshal(data, &cache) == nil {
				tz = cache.TimeZone
			}
		}
	}
	if tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

// forEach runs fn on each item, a few at a time (Gmail limits how fast one
// user may make requests), and returns their errors.
func forEach[T any](items []T, fn func(int, T) error) error {
	sem := make(chan struct{}, 5)
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = fn(i, it)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}
