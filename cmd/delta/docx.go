package main

import (
	"errors"
	"fmt"

	"github.com/thooton/delta/internal/docx"
)

// delta docx: how to make Word documents, and the scripts that guide uses
// (internal/docx).
func runDocx(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(docx.Help("delta docx"))
		return nil
	}
	switch args[0] {
	case "extract":
		if len(args) > 2 {
			return errors.New("usage: delta docx extract [dir]")
		}
		dir := "."
		if len(args) == 2 {
			dir = args[1]
		}
		n, err := docx.Extract(dir)
		if err != nil {
			return err
		}
		fmt.Printf("extracted %d files into %s: scripts/ and its LICENSE\n", n, dir)
		return nil
	default:
		return fmt.Errorf("unknown docx command %q (run `delta docx`)", args[0])
	}
}
