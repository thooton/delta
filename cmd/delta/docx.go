package main

import (
	_ "embed"
	"errors"
	"fmt"
)

// docxHelp tells the agent how to make Word documents. It is Markdown, kept
// in its own file because it is full of backticks.
//
//go:embed docx_help.md
var docxHelp string

func runDocx(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(docxHelp)
		return nil
	}
	return errors.New("usage: delta docx")
}
