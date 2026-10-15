package main

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Backup sessions (▲ in the app) run on the command center itself, with a
// model reached through an API key kept there (PUT /agent-key). The key is
// never shown again: there is no command to print it.

func runBackupKey(args []string) error {
	if len(args) != 2 || args[0] != "set" {
		return errors.New("usage: delta backup-key set <key|->")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	key := args[1]
	if key == "-" {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("reading the key from stdin: %w", err)
		}
		key = line
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("the key is empty")
	}
	if _, err := do(cfg, http.MethodPut, "/agent-key", strings.NewReader(key)); err != nil {
		return err
	}
	fmt.Println("Key for backup sessions saved on the command center.")
	return nil
}
