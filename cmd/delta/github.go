package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// The GitHub personal access token lives on the command center, which never
// hands it out: `delta set-personal-access-token` replaces it, and
// `delta git` and `delta gh` reach GitHub through the command center's relay
// (relay.go), which adds it to each request on the way out.

func setAccessToken(token string) error {
	if _, err := loadConfig(); err != nil {
		return err
	}
	if token == "-" {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("reading the token from stdin: %w", err)
		}
		token = line
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("the token is empty")
	}

	// Try it first, so a typo isn't saved. If GitHub can't be reached from
	// here, save it anyway.
	login, cerr := checkAccessToken(token)
	if errors.Is(cerr, errBadAccessToken) {
		return fmt.Errorf("not saved: %w", cerr)
	}
	if _, err := do(http.MethodPut, "/personal-access-token", strings.NewReader(token)); err != nil {
		return err
	}
	if cerr != nil {
		fmt.Printf("Token saved on the command center, but it couldn't be checked: %v\n", cerr)
	} else {
		fmt.Printf("Token for GitHub user %s saved on the command center.\n", login)
	}
	return nil
}

var errBadAccessToken = errors.New("GitHub rejected the token (expired, revoked or mistyped?)")

// checkAccessToken asks GitHub who the token belongs to.
func checkAccessToken(token string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errBadAccessToken
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub returned HTTP %d", resp.StatusCode)
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil || u.Login == "" {
		return "", errors.New("unexpected response from GitHub")
	}
	return u.Login, nil
}
