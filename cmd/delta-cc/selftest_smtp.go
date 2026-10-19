package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// smtpSelfTest runs the SMTP relay against a fake of Google, sending as
// rmfakecloud does (Go's SMTP client, no TLS, AUTH PLAIN), and checks that
// it needs the login, sends to every envelope recipient, and passes Gmail's
// refusals on.
func smtpSelfTest(dir string) error {
	dataDir := filepath.Join(dir, "smtp")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	var (
		mu       sync.Mutex
		sent     []string
		sendCode = http.StatusOK
		grant    = ""
	)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if grant != "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":%q}`, grant)
				return
			}
			if r.Form.Get("refresh_token") != "1//refresh" || r.Form.Get("client_secret") != "secret" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid_request"}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"ya29.access","expires_in":3599}`)
		case "/send":
			if r.Header.Get("Authorization") != "Bearer ya29.access" || r.URL.Query().Get("uploadType") != "media" || r.Header.Get("Content-Type") != "message/rfc822" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if sendCode != http.StatusOK {
				w.WriteHeader(sendCode)
				fmt.Fprint(w, `{"error":{"message":"Invalid To header"}}`)
				return
			}
			body, _ := io.ReadAll(r.Body)
			sent = append(sent, string(body))
			fmt.Fprintf(w, `{"id":"msg%d"}`, len(sent))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fake.Close()
	upstream, delay := gmailUpstream, smtpAuthDelay
	gmailUpstream.tokenURL, gmailUpstream.sendURL, smtpAuthDelay = fake.URL+"/token", fake.URL+"/send", 0
	defer func() { gmailUpstream, smtpAuthDelay = upstream, delay }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	relay := newSMTPRelay(dataDir)
	go relay.serve(ln)
	defer relay.shutdown(context.Background())
	addr := ln.Addr().String()

	// send sends as rmfakecloud does.
	send := func(user, password string, to []string, msg string) error {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return err
		}
		c, err := smtp.NewClient(conn, "127.0.0.1")
		if err != nil {
			return err
		}
		defer c.Close()
		if err := c.Auth(smtp.PlainAuth("", user, password, "127.0.0.1")); err != nil {
			return err
		}
		if err := c.Mail("tablet@example.com"); err != nil {
			return err
		}
		for _, a := range to {
			if err := c.Rcpt(a); err != nil {
				return err
			}
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		io.WriteString(w, msg)
		if err := w.Close(); err != nil {
			return err
		}
		return c.Quit()
	}
	const msg = "From: tablet@example.com\r\nTo: Amy <amy@example.com>\r\nSubject: Notes\r\n\r\nHello\r\n.leading dot\r\n"
	wantErr := func(desc, code string, err error) error {
		if err == nil || !strings.HasPrefix(err.Error(), code) {
			return fmt.Errorf("smtp relay: %s: want %s, got %v", desc, code, err)
		}
		return nil
	}

	if err := wantErr("no login set", "535", send("delta", "x", []string{"amy@example.com"}, msg)); err != nil {
		return err
	}
	password, err := writeSMTPLogin(dataDir, "delta")
	if err != nil {
		return err
	}
	if info, err := os.Stat(filepath.Join(dataDir, smtpLoginFile)); err != nil || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("smtp login file: %v %v", info, err)
	}
	if err := wantErr("wrong password", "535", send("delta", password+"x", []string{"amy@example.com"}, msg)); err != nil {
		return err
	}
	if err := wantErr("wrong username", "535", send("other", password, []string{"amy@example.com"}, msg)); err != nil {
		return err
	}
	if err := wantErr("no Gmail login", "554", send("delta", password, []string{"amy@example.com"}, msg)); err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(dataDir, gmailLoginFile), []byte(`{"client_id":"id","client_secret":"secret","refresh_token":"1//refresh","email":"me@gmail.com"}`)); err != nil {
		return err
	}
	if err := send("delta", password, []string{"amy@example.com", "bob@example.com"}, msg); err != nil {
		return fmt.Errorf("smtp relay: send: %v", err)
	}
	mu.Lock()
	got := strings.Join(sent, "")
	mu.Unlock()
	if want := "Bcc: bob@example.com\r\n" + msg; got != want {
		return fmt.Errorf("smtp relay: Gmail got %q, want %q", got, want)
	}

	// Without logging in, nothing is taken; AUTH LOGIN works too.
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	b64 := base64.StdEncoding.EncodeToString
	for _, step := range []struct{ send, want string }{
		{"", "220 "},
		{"EHLO test", "250-delta-cc"},
		{"MAIL FROM:<a@example.com>", "530 "},
		{"AUTH LOGIN", "334 VXNlcm5hbWU6"},
		{b64([]byte("delta")), "334 UGFzc3dvcmQ6"},
		{b64([]byte(password)), "235 "},
		{"RCPT TO:<amy@example.com>", "503 "},
		{"MAIL FROM:<a@example.com> SIZE=999999999", "552 "},
		{"QUIT", "221 "},
	} {
		if step.send != "" {
			fmt.Fprintf(conn, "%s\r\n", step.send)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := br.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, step.want) {
			return fmt.Errorf("smtp relay: after %q: want %q, got %q (%v)", step.send, step.want, line, err)
		}
		for strings.HasPrefix(line, step.want[:3]+"-") {
			line, _ = br.ReadString('\n')
		}
	}

	// Gmail's refusals: of the message, and of the login.
	mu.Lock()
	sendCode = http.StatusBadRequest
	mu.Unlock()
	if err := wantErr("Gmail refuses the message", "554", send("delta", password, []string{"amy@example.com"}, msg)); err != nil {
		return err
	}
	mu.Lock()
	sendCode = http.StatusServiceUnavailable
	mu.Unlock()
	if err := wantErr("Gmail is down", "451", send("delta", password, []string{"amy@example.com"}, msg)); err != nil {
		return err
	}
	mu.Lock()
	sendCode, grant = http.StatusOK, "invalid_grant"
	mu.Unlock()
	relay.mu.Lock()
	relay.expires = time.Time{}
	relay.mu.Unlock()
	return wantErr("Gmail login revoked", "554", send("delta", password, []string{"amy@example.com"}, msg))
}
