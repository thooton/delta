package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// `delta gmail login` is for the user, on a computer with a browser: it
// runs Google's OAuth flow for installed apps with the OAuth client they
// made in Google Cloud, and saves the client and the refresh token it gets
// on the command center. Google redirects the browser to a listener on
// 127.0.0.1; when the browser is on another machine, pasting the address it
// was sent to works too.

// oauthClient reads an OAuth client's ID and secret from the JSON file
// Google Cloud downloads for it ({"installed": {...}}).
func oauthClient(path string) (id, secret string, err error) {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return "", "", err
	}
	var f struct {
		Installed, Web *struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return "", "", fmt.Errorf("%s isn't an OAuth client file: %w", path, err)
	}
	switch {
	case f.Installed != nil:
		id, secret = f.Installed.ClientID, f.Installed.ClientSecret
	case f.Web != nil:
		return "", "", errors.New("that is a web application client; create an OAuth client of type \"Desktop app\" and download its JSON")
	default:
		id, secret = f.ClientID, f.ClientSecret
	}
	if id == "" || secret == "" {
		return "", "", fmt.Errorf("%s has no client_id and client_secret", path)
	}
	return id, secret, nil
}

func randomString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func gmailLoginCmd(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: delta gmail login <client-secret.json>   (the OAuth client file from Google Cloud; see `delta gmail help`)")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	clientID, clientSecret, err := oauthClient(args[0])
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	state := randomString(16)
	verifier := randomString(48)
	sum := sha256.Sum256([]byte(verifier))
	authURL := googleAuthURL + "?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"scope":                 {strings.Join(append([]string{gmailScope}, peopleScopes...), " ")},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}.Encode()

	// The code arrives at the listener, or pasted on stdin.
	codes := make(chan url.Values, 2)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code") == "" && q.Get("error") == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if q.Get("error") != "" {
			fmt.Fprint(w, "<h2>Delta: Gmail was not connected.</h2><p>Go back to the terminal for details.</p>")
		} else {
			fmt.Fprint(w, "<h2>Delta: done.</h2><p>You can close this tab and go back to the terminal.</p>")
		}
		codes <- q
	}), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if u, err := url.Parse(line); err == nil && (u.Query().Get("code") != "" || u.Query().Get("error") != "") {
				codes <- u.Query()
			} else {
				codes <- url.Values{"code": {line}, "state": {state}}
			}
			return
		}
	}()

	fmt.Println("Open this address in a browser and allow Delta to use the Gmail account:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	fmt.Println("If the browser is on another device, it ends on a page that won't load")
	fmt.Println("(127.0.0.1); copy that page's address from the address bar and paste it here.")
	openBrowser(authURL)

	var q url.Values
	select {
	case q = <-codes:
	case <-time.After(10 * time.Minute):
		return errors.New("gave up waiting for the browser after 10 minutes")
	}
	if e := q.Get("error"); e != "" {
		if e == "access_denied" {
			return errors.New("access was denied in the browser; nothing was saved")
		}
		return fmt.Errorf("Google returned an error: %s", e)
	}
	if q.Get("state") != state {
		return errors.New("that address came from a different login attempt; nothing was saved")
	}
	if !strings.Contains(" "+q.Get("scope")+" ", " "+gmailScope+" ") && q.Get("scope") != "" {
		return errors.New("the Gmail permission wasn't granted (leave its box checked on Google's consent page); nothing was saved")
	}

	t, err := requestToken(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {q.Get("code")},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	})
	if errors.Is(err, errGmailLoginExpired) {
		return errors.New("Google refused the sign-in code (expired or already used); run the login again")
	} else if err != nil {
		return err
	}
	if t.RefreshToken == "" {
		return errors.New("Google gave no refresh token; remove Delta's access at https://myaccount.google.com/permissions and run the login again")
	}
	if !strings.Contains(" "+t.Scope+" ", " "+gmailScope+" ") {
		return errors.New("the Gmail permission wasn't granted (leave its box checked on Google's consent page); nothing was saved")
	}
	email, err := gmailAddress(t.AccessToken)
	if err != nil {
		return err
	}
	login := gmailLogin{ClientID: clientID, ClientSecret: clientSecret, RefreshToken: t.RefreshToken, Email: email}
	if err := callJSON(cfg, http.MethodPut, "/gmail-login", login, nil); err != nil {
		return err
	}
	fmt.Printf("\nGmail account %s saved on the command center.\n", email)
	return nil
}

// openBrowser tries to open u in the desktop's browser, quietly.
func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", u)
	default:
		return
	}
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

func gmailLogoutCmd(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: delta gmail logout")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var login gmailLogin
	if err := callJSON(cfg, http.MethodGet, "/gmail-login", nil, &login); err != nil {
		return err
	}
	// Revoke the grant too, so the token is useless wherever it was seen.
	revoked := false
	if resp, err := (&http.Client{Timeout: 15 * time.Second}).PostForm(googleRevokeURL, url.Values{"token": {login.RefreshToken}}); err == nil {
		resp.Body.Close()
		revoked = resp.StatusCode == http.StatusOK
	}
	if _, err := do(cfg, http.MethodDelete, "/gmail-login", nil); err != nil {
		return err
	}
	if !revoked {
		fmt.Printf("Removed the Gmail login (%s) from the command center. Google couldn't be told to revoke it; the user can at https://myaccount.google.com/permissions.\n", login.Email)
		return nil
	}
	fmt.Printf("Removed the Gmail login (%s) from the command center and revoked it.\n", login.Email)
	return nil
}
