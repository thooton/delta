package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The GitHub personal access token lives on the command center
// (GET/PUT /personal-access-token). The CLI never keeps a copy: `delta git`
// and `delta gh` fetch it on every run and hand it to that one process
// through its environment, so no git config, remote URL or file ever holds
// it, and a new token takes effect immediately.

func fetchAccessToken() (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	data, err := do(cfg, http.MethodGet, "/personal-access-token", nil)
	if err != nil {
		return "", err
	}
	var res struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return "", err
	}
	return res.Token, nil
}

func getAccessToken() error {
	token, err := fetchAccessToken()
	if err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

// runWithAccessToken runs git (sending the token to github.com as a basic
// auth header, the way actions/checkout does) or gh (as GH_TOKEN), with the
// caller's stdio, and exits with its status.
func runWithAccessToken(prog string, args []string) error {
	token, err := fetchAccessToken()
	if err != nil {
		return err
	}
	cmd := exec.Command(prog, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	switch prog {
	case "git":
		// Added after any GIT_CONFIG_* entries already in the environment.
		n, _ := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		cmd.Env = append(cmd.Env,
			fmt.Sprintf("GIT_CONFIG_COUNT=%d", n+1),
			fmt.Sprintf("GIT_CONFIG_KEY_%d=http.https://github.com/.extraheader", n),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=Authorization: Basic %s", n, auth),
		)
	case "gh":
		cmd.Env = append(cmd.Env, "GH_TOKEN="+token, "GITHUB_TOKEN="+token)
	}
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	return err
}

func setAccessToken(token string) error {
	cfg, err := loadConfig()
	if err != nil {
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
	if _, err := do(cfg, http.MethodPut, "/personal-access-token", strings.NewReader(token)); err != nil {
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
