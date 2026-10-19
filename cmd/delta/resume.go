package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"
	"time"
)

// `delta resume` fetches a conversation carried over from another session.
// When a session's account runs out of usage and the user moves the
// conversation to a new session, the client stores the old session's whole
// history on the command center under an id, and the new session's first
// message says to run `delta resume <id> <file>`. It goes to a file rather
// than the terminal because an agent's shell cuts long output short, and
// this has to be read in full.

func runResume(args []string) error {
	switch {
	case len(args) == 0:
		return listConversations()
	case len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help"):
		fmt.Println("usage: delta resume <id> <file>   save a carried-over conversation, in full, to <file>\n       delta resume                 list the stored ones")
		return nil
	case len(args) == 2:
		return saveConversation(args[0], args[1])
	default:
		return errors.New("usage: delta resume <id> <file>   (the conversation is written to <file>; read all of it)")
	}
}

func saveConversation(id, dest string) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	resp, err := stream(c, http.MethodGet, "/conversations/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	lines := &lineCounter{}
	n, err := io.Copy(io.MultiWriter(f, lines), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	fmt.Printf("Saved the conversation to %s (%d lines, %s). Read all of it, from the first line to the last, before doing anything else.\n",
		dest, lines.n, humanSize(n))
	return nil
}

type lineCounter struct{ n int }

func (l *lineCounter) Write(p []byte) (int, error) {
	l.n += bytes.Count(p, []byte{'\n'})
	return len(p), nil
}

func listConversations() error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	body, err := do(c, http.MethodGet, "/conversations", nil)
	if err != nil {
		return err
	}
	var conversations []struct {
		ID       string    `json:"id"`
		Size     int64     `json:"size"`
		Modified time.Time `json:"modified"`
		Title    string    `json:"title"`
	}
	if err := json.Unmarshal(body, &conversations); err != nil {
		return err
	}
	if len(conversations) == 0 {
		fmt.Println("No carried-over conversations.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSAVED\tSIZE\tTITLE")
	for _, conv := range conversations {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", conv.ID, conv.Modified.Local().Format("2006-01-02 15:04"), humanSize(conv.Size), conv.Title)
	}
	return tw.Flush()
}
