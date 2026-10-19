package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/mail"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The SMTP relay: a small SMTP server, on this machine only, that sends what
// it is given through the user's Gmail, with the Gmail login `delta gmail
// login` stored (gmail.go). It is for programs that can send mail only over
// SMTP, above all rmfakecloud, whose "send by email" on the reMarkable would
// otherwise need an SMTP password for Gmail, which accounts without app
// passwords can't have. rmfakecloud is pointed at it in its environment file:
//
//	RM_SMTP_SERVER=127.0.0.1:2525
//	RM_SMTP_NOTLS=true
//	RM_SMTP_USERNAME=delta
//	RM_SMTP_PASSWORD=<from `delta-cc smtp-password`>
//
// It speaks plain ESMTP with AUTH PLAIN and LOGIN, and no TLS: it listens on
// the loopback interface (-smtp-addr), where Go's SMTP client, rmfakecloud's,
// sends a password without TLS. Every message needs the username and
// password `delta-cc smtp-password` made; only the password's SHA-256 is
// kept, in smtp-login.json, and it is read on every login, so a new one
// works at once. A message is sent as it is, with Gmail's messages.send, to
// the envelope's recipients: any not named in its To, Cc or Bcc are added
// as Bcc, which Gmail sends to and strips. Gmail sends it from the login's
// address (or one of its send-as addresses, if the From names one).
const (
	smtpLoginFile   = "smtp-login.json"
	smtpMaxMessage  = 35 << 20 // Gmail's upload limit
	smtpMaxRcpts    = 100
	smtpMaxLine     = 4096
	smtpMaxAuthFail = 3
	smtpIdle        = 5 * time.Minute
)

// smtpAuthDelay slows down guessing passwords.
var smtpAuthDelay = time.Second

// gmailUpstream is where the relay reaches Google; the self-test points it
// at a fake.
var gmailUpstream = struct{ tokenURL, sendURL string }{
	tokenURL: "https://oauth2.googleapis.com/token",
	sendURL:  "https://gmail.googleapis.com/upload/gmail/v1/users/me/messages/send",
}

type smtpLogin struct {
	Username       string `json:"username"`
	PasswordSHA256 string `json:"password_sha256"`
}

// genSMTPPassword makes a new SMTP password, replacing any earlier one, and
// prints it with the lines rmfakecloud's environment file needs.
func genSMTPPassword(args []string) {
	fs := flag.NewFlagSet("smtp-password", flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory")
	user := fs.String("user", "delta", "the username to log in with")
	addr := fs.String("smtp-addr", "127.0.0.1:2525", "the relay's address, for the printed configuration")
	fs.Parse(args)

	password, err := writeSMTPLogin(*dataDir, *user)
	if err != nil {
		log.Fatal(err)
	}
	path := filepath.Join(*dataDir, smtpLoginFile)
	fmt.Fprintf(os.Stderr, "SMTP login written to %s; it works at once, with no restart.\n", path)
	fmt.Fprintln(os.Stderr, "Save the password now; it cannot be shown again. For rmfakecloud's environment file:")
	fmt.Printf("RM_SMTP_SERVER=%s\nRM_SMTP_NOTLS=true\nRM_SMTP_USERNAME=%s\nRM_SMTP_PASSWORD=%s\n", *addr, *user, password)
}

// writeSMTPLogin makes a new password for user, stores its hash in dataDir
// and returns it.
func writeSMTPLogin(dataDir, user string) (string, error) {
	if user == "" || strings.ContainsAny(user, " \t\r\n\x00") {
		return "", errors.New("the username must be non-empty and contain no spaces")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	password := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(password))
	data, _ := json.Marshal(smtpLogin{Username: user, PasswordSHA256: hex.EncodeToString(sum[:])})
	return password, writePrivateFile(filepath.Join(dataDir, smtpLoginFile), append(data, '\n'))
}

type smtpRelay struct {
	dataDir string
	client  *http.Client

	mu      sync.Mutex // guards the token and ln
	token   string     // the current Gmail access token
	refresh string     // the refresh token it came from
	expires time.Time  // when it stops working

	ln       net.Listener
	sessions sync.WaitGroup
}

func newSMTPRelay(dataDir string) *smtpRelay {
	return &smtpRelay{dataDir: dataDir, client: &http.Client{Timeout: 5 * time.Minute}}
}

// serve accepts connections on ln until shutdown closes it.
func (r *smtpRelay) serve(ln net.Listener) error {
	r.mu.Lock()
	r.ln = ln
	r.mu.Unlock()
	for {
		conn, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return nil
		} else if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return err
		}
		r.sessions.Add(1)
		go func() {
			defer r.sessions.Done()
			r.session(conn)
		}()
	}
}

// shutdown stops accepting connections and waits for messages being sent,
// until ctx ends.
func (r *smtpRelay) shutdown(ctx context.Context) {
	r.mu.Lock()
	if r.ln != nil {
		r.ln.Close()
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.sessions.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

var errSMTPLineTooLong = errors.New("line too long")

type smtpConn struct {
	conn net.Conn
	br   *bufio.Reader
}

func (c *smtpConn) reply(code int, lines ...string) error {
	c.conn.SetWriteDeadline(time.Now().Add(smtpIdle))
	var b strings.Builder
	for i, line := range lines {
		sep := "-"
		if i == len(lines)-1 {
			sep = " "
		}
		fmt.Fprintf(&b, "%d%s%s\r\n", code, sep, line)
	}
	_, err := io.WriteString(c.conn, b.String())
	return err
}

// readLine reads one line, without its line ending, refusing (and skipping)
// lines longer than smtpMaxLine.
func (c *smtpConn) readLine() (string, error) {
	c.conn.SetReadDeadline(time.Now().Add(smtpIdle))
	line, err := c.br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = c.br.ReadSlice('\n')
		}
		if err != nil {
			return "", err
		}
		return "", errSMTPLineTooLong
	} else if err != nil {
		return "", err
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

func (r *smtpRelay) session(conn net.Conn) {
	defer conn.Close()
	c := &smtpConn{conn: conn, br: bufio.NewReaderSize(conn, smtpMaxLine)}
	if c.reply(220, "delta-cc ESMTP ready") != nil {
		return
	}
	var (
		user     string // who logged in; empty until then
		failures int
		from     *string
		rcpts    []string
	)
	reset := func() { from, rcpts = nil, nil }
	for {
		line, err := c.readLine()
		if errors.Is(err, errSMTPLineTooLong) {
			if c.reply(500, "5.5.2 Line too long") != nil {
				return
			}
			continue
		} else if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		arg = strings.TrimSpace(arg)
		switch strings.ToUpper(verb) {
		case "EHLO":
			reset()
			err = c.reply(250, "delta-cc", "AUTH PLAIN LOGIN", fmt.Sprintf("SIZE %d", smtpMaxMessage), "8BITMIME", "ENHANCEDSTATUSCODES", "PIPELINING")
		case "HELO":
			reset()
			err = c.reply(250, "delta-cc")
		case "AUTH":
			if user != "" {
				err = c.reply(503, "5.5.1 Already logged in")
				break
			}
			var ok bool
			if user, ok, err = r.auth(c, arg); err != nil || ok {
				if ok {
					err = c.reply(235, "2.7.0 Logged in")
				}
				break
			}
			if failures++; failures >= smtpMaxAuthFail {
				c.reply(421, "4.7.0 Too many failed logins")
				return
			}
		case "MAIL":
			addr, ok := smtpPath(arg, "FROM:")
			switch {
			case user == "":
				err = c.reply(530, "5.7.0 Log in first (AUTH); `delta-cc smtp-password` makes the login")
			case !ok:
				err = c.reply(501, "5.5.4 Expected MAIL FROM:<address>")
			case from != nil:
				err = c.reply(503, "5.5.1 MAIL already given")
			case smtpSizeParam(arg) > smtpMaxMessage:
				err = c.reply(552, fmt.Sprintf("5.3.4 Messages are limited to %d bytes", smtpMaxMessage))
			default:
				from = &addr
				err = c.reply(250, "2.1.0 OK")
			}
		case "RCPT":
			addr, ok := smtpPath(arg, "TO:")
			switch {
			case from == nil:
				err = c.reply(503, "5.5.1 MAIL first")
			case !ok || addr == "":
				err = c.reply(501, "5.5.4 Expected RCPT TO:<address>")
			case len(rcpts) >= smtpMaxRcpts:
				err = c.reply(452, "4.5.3 Too many recipients")
			default:
				if _, perr := mail.ParseAddress(addr); perr != nil {
					err = c.reply(553, "5.1.3 Malformed address")
					break
				}
				rcpts = append(rcpts, addr)
				err = c.reply(250, "2.1.5 OK")
			}
		case "DATA":
			if len(rcpts) == 0 {
				err = c.reply(503, "5.5.1 RCPT first")
				break
			}
			if err = c.reply(354, "End data with <CR><LF>.<CR><LF>"); err != nil {
				break
			}
			c.conn.SetReadDeadline(time.Now().Add(4 * smtpIdle))
			dr := textproto.NewReader(c.br).DotReader()
			msg, rerr := io.ReadAll(io.LimitReader(dr, smtpMaxMessage+1))
			if rerr == nil && len(msg) > smtpMaxMessage {
				_, rerr = io.Copy(io.Discard, dr)
				if rerr == nil {
					err = c.reply(552, fmt.Sprintf("5.3.4 Messages are limited to %d bytes", smtpMaxMessage))
					reset()
					break
				}
			}
			if rerr != nil {
				return
			}
			code, text := r.deliver(rcpts, msg)
			reset()
			err = c.reply(code, text)
		case "RSET":
			reset()
			err = c.reply(250, "2.0.0 OK")
		case "NOOP":
			err = c.reply(250, "2.0.0 OK")
		case "VRFY":
			err = c.reply(252, "2.5.0 Cannot verify")
		case "QUIT":
			c.reply(221, "2.0.0 Bye")
			return
		default:
			err = c.reply(502, "5.5.2 Command not recognized")
		}
		if err != nil {
			return
		}
	}
}

// auth runs an AUTH command (PLAIN or LOGIN). It returns who logged in, or
// ok false after replying with why not.
func (r *smtpRelay) auth(c *smtpConn, arg string) (user string, ok bool, err error) {
	mech, initial, _ := strings.Cut(arg, " ")
	// prompt asks the client for the next response, unless it sent one.
	prompt := func(challenge string) (string, bool, error) {
		if initial != "" {
			resp := initial
			initial = ""
			return resp, true, nil
		}
		if err := c.reply(334, challenge); err != nil {
			return "", false, err
		}
		line, err := c.readLine()
		if errors.Is(err, errSMTPLineTooLong) {
			return "", false, c.reply(500, "5.5.2 Line too long")
		} else if err != nil {
			return "", false, err
		}
		if line == "*" {
			return "", false, c.reply(501, "5.0.0 Login cancelled")
		}
		return line, true, nil
	}
	decode := func(s string) ([]byte, bool, error) {
		if s == "=" {
			return nil, true, nil
		}
		b, derr := base64.StdEncoding.DecodeString(s)
		if derr != nil {
			return nil, false, c.reply(501, "5.5.2 Malformed base64")
		}
		return b, true, nil
	}

	var password string
	switch strings.ToUpper(mech) {
	case "PLAIN":
		resp, more, err := prompt("")
		if !more {
			return "", false, err
		}
		b, more, err := decode(resp)
		if !more {
			return "", false, err
		}
		parts := strings.Split(string(b), "\x00")
		if len(parts) != 3 {
			return "", false, c.reply(501, "5.5.2 Malformed PLAIN response")
		}
		user, password = parts[1], parts[2]
	case "LOGIN":
		resp, more, err := prompt("VXNlcm5hbWU6") // "Username:"
		if !more {
			return "", false, err
		}
		b, more, err := decode(resp)
		if !more {
			return "", false, err
		}
		user = string(b)
		if resp, more, err = prompt("UGFzc3dvcmQ6"); !more { // "Password:"
			return "", false, err
		}
		if b, more, err = decode(resp); !more {
			return "", false, err
		}
		password = string(b)
	default:
		return "", false, c.reply(504, "5.5.4 Use AUTH PLAIN or AUTH LOGIN")
	}

	if why := r.checkLogin(user, password); why != "" {
		time.Sleep(smtpAuthDelay)
		return "", false, c.reply(535, "5.7.8 "+why)
	}
	return user, true, nil
}

// checkLogin says why a username and password don't log in, or "" if they do.
func (r *smtpRelay) checkLogin(user, password string) string {
	data, err := os.ReadFile(filepath.Join(r.dataDir, smtpLoginFile))
	if errors.Is(err, os.ErrNotExist) {
		return "No SMTP login is set; run `delta-cc smtp-password` on the command center"
	} else if err != nil {
		log.Printf("smtp relay: %v", err)
		return "Can't read the SMTP login"
	}
	var l smtpLogin
	want, herr := []byte(nil), json.Unmarshal(data, &l)
	if herr == nil {
		want, herr = hex.DecodeString(l.PasswordSHA256)
	}
	if herr != nil || len(want) != sha256.Size {
		log.Printf("smtp relay: malformed %s; run `delta-cc smtp-password` again", smtpLoginFile)
		return "Can't read the SMTP login"
	}
	got := sha256.Sum256([]byte(password))
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(l.Username)) == 1
	if subtle.ConstantTimeCompare(got[:], want) != 1 || !userOK {
		return "Wrong username or password"
	}
	return ""
}

// smtpPath reads the address out of "FROM:<a@b.com> SIZE=123" (prefix
// "FROM:") or "TO:<a@b.com>", tolerating a space after the colon and a
// missing pair of angle brackets. The null path "<>" is "".
func smtpPath(arg, prefix string) (string, bool) {
	if len(arg) < len(prefix) || !strings.EqualFold(arg[:len(prefix)], prefix) {
		return "", false
	}
	rest := strings.TrimSpace(arg[len(prefix):])
	if strings.HasPrefix(rest, "<") {
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			return "", false
		}
		return rest[1:end], true
	}
	addr, _, _ := strings.Cut(rest, " ")
	return addr, addr != ""
}

// smtpSizeParam is the SIZE= a MAIL command declares, or 0.
func smtpSizeParam(arg string) int64 {
	for _, f := range strings.Fields(arg) {
		if len(f) > 5 && strings.EqualFold(f[:5], "SIZE=") {
			var n int64
			if _, err := fmt.Sscan(f[5:], &n); err == nil {
				return n
			}
		}
	}
	return 0
}

// deliver sends a message through Gmail to rcpts. msg is as a DotReader
// gives it, with LF line endings. It returns the SMTP reply.
func (r *smtpRelay) deliver(rcpts []string, msg []byte) (int, string) {
	m, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		return 554, "5.6.0 Malformed message: " + err.Error()
	}
	// Gmail sends to the header's recipients, so name any others the
	// envelope has in a Bcc, which it strips.
	named := map[string]bool{}
	for _, h := range []string{"To", "Cc", "Bcc"} {
		addrs, _ := m.Header.AddressList(h)
		for _, a := range addrs {
			named[strings.ToLower(a.Address)] = true
		}
	}
	var bcc []string
	for _, a := range rcpts {
		if !named[strings.ToLower(a)] {
			named[strings.ToLower(a)] = true
			bcc = append(bcc, a)
		}
	}
	if len(bcc) > 0 {
		msg = append([]byte("Bcc: "+strings.Join(bcc, ", ")+"\n"), msg...)
	}
	// Back to CRLF line endings, as a message on the wire has.
	msg = bytes.ReplaceAll(msg, []byte("\n"), []byte("\r\n"))

	id, err := r.send(msg)
	if err != nil {
		var perm *smtpPermanent
		if errors.As(err, &perm) {
			log.Printf("smtp relay: not sent: %v", err)
			return 554, "5.0.0 " + smtpText(err.Error())
		}
		log.Printf("smtp relay: not sent, for now: %v", err)
		return 451, "4.0.0 " + smtpText(err.Error())
	}
	log.Printf("smtp relay: sent Gmail message %s to %d recipients (%d bytes)", id, len(rcpts), len(msg))
	return 250, "2.0.0 Sent as Gmail message " + id
}

// smtpPermanent marks a failure that trying again won't fix.
type smtpPermanent struct{ msg string }

func (e *smtpPermanent) Error() string { return e.msg }

// smtpText makes s fit on one SMTP reply line.
func smtpText(s string) string {
	s = oneLine(s)
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}

// send sends a raw message with Gmail's messages.send and returns its id.
func (r *smtpRelay) send(msg []byte) (string, error) {
	for attempt := 0; ; attempt++ {
		token, err := r.accessToken(attempt > 0)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequest(http.MethodPost, gmailUpstream.sendURL+"?uploadType=media", bytes.NewReader(msg))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "message/rfc822")
		resp, err := r.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("Gmail can't be reached: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			continue // the access token was revoked early; get another
		}
		if resp.StatusCode != http.StatusOK {
			var e struct {
				Error struct{ Message string } `json:"error"`
			}
			why := strings.TrimSpace(string(body))
			if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
				why = e.Error.Message
			}
			err := fmt.Errorf("Gmail refused the message (%s): %s", resp.Status, why)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return "", &smtpPermanent{err.Error()}
			}
			return "", err
		}
		var sent struct{ ID string }
		json.Unmarshal(body, &sent)
		return sent.ID, nil
	}
}

// accessToken returns an access token for the stored Gmail login, renewing
// it when it is about to expire, when the login changed, or when renew is
// set.
func (r *smtpRelay) accessToken(renew bool) (string, error) {
	data, err := os.ReadFile(filepath.Join(r.dataDir, gmailLoginFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", &smtpPermanent{"Gmail isn't set up on the command center; run `delta gmail login`"}
	} else if err != nil {
		return "", err
	}
	var l gmailLogin
	if err := json.Unmarshal(data, &l); err != nil {
		return "", fmt.Errorf("malformed %s: %v", gmailLoginFile, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if !renew && r.refresh == l.RefreshToken && time.Until(r.expires) > time.Minute {
		return r.token, nil
	}
	resp, err := r.client.PostForm(gmailUpstream.tokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {l.ClientID},
		"client_secret": {l.ClientSecret},
		"refresh_token": {l.RefreshToken},
	})
	if err != nil {
		return "", fmt.Errorf("Google can't be reached: %w", err)
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t)
	if resp.StatusCode != http.StatusOK || t.AccessToken == "" {
		if t.Error == "invalid_grant" || t.Error == "invalid_client" || t.Error == "unauthorized_client" {
			return "", &smtpPermanent{fmt.Sprintf("the Gmail login no longer works (%s); run `delta gmail login` again", t.Error)}
		}
		return "", fmt.Errorf("Google didn't renew the Gmail login (%s): %s %s", resp.Status, t.Error, t.Description)
	}
	r.token, r.refresh = t.AccessToken, l.RefreshToken
	r.expires = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	return r.token, nil
}

// isLoopback says whether a listen address is on the loopback interface
// only.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
