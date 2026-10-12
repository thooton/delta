package main

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// docxHelp tells the agent how to make Word documents. It is Markdown, kept
// in its own file because it is full of backticks.
//
//go:embed docx_help.md
var docxHelp string

// docxBundle holds the scripts the guide uses, under docx/ (MIT, from
// Anthropic's docx skill; its LICENSE is extracted with them). "all:" keeps
// the __init__.py files.
//
//go:embed all:docx
var docxBundle embed.FS

func runDocx(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(docxHelp)
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
		return docxExtract(dir)
	default:
		return fmt.Errorf("unknown docx command %q (run `delta docx`)", args[0])
	}
}

// docxExtract writes the bundled scripts and their LICENSE into dir,
// replacing any files of the same names.
func docxExtract(dir string) error {
	root, err := fs.Sub(docxBundle, "docx")
	if err != nil {
		return err
	}
	n := 0
	err = fs.WalkDir(root, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := fs.ReadFile(root, path)
		if err != nil {
			return err
		}
		n++
		return os.WriteFile(dest, data, 0o644)
	})
	if err != nil {
		return err
	}
	fmt.Printf("extracted %d files into %s: scripts/ and its LICENSE\n", n, dir)
	return nil
}
