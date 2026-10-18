// Package docx holds the guide to making Word documents and the scripts it
// uses, for `delta docx`.
package docx

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// help tells the agent how to make Word documents. It is Markdown, kept in
// its own file because it is full of backticks.
//
//go:embed help.md
var help string

// bundle holds the scripts the guide uses, under docx/ (MIT, from
// Anthropic's docx skill; its LICENSE is extracted with them). "all:" keeps
// the __init__.py files.
//
//go:embed all:docx
var bundle embed.FS

// Help is the guide, for the command cmd ("delta docx", say).
func Help(cmd string) string {
	return strings.ReplaceAll(help, "delta docx", cmd)
}

// Extract writes the bundled scripts and their LICENSE into dir, replacing
// any files of the same names, and says how many it wrote.
func Extract(dir string) (int, error) {
	root, err := fs.Sub(bundle, "docx")
	if err != nil {
		return 0, err
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
	return n, err
}
