package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
	case len(args) == 1 && isHelp(args[0]):
		fmt.Println("usage: delta resume <id> <file>   save a carried-over conversation, in full, to <file>\n       delta resume                 list the stored ones")
		return nil
	case len(args) == 2:
		return saveConversation(args[0], args[1])
	default:
		return errors.New("usage: delta resume <id> <file>   (the conversation is written to <file>; read all of it)")
	}
}

func saveConversation(id, dest string) error {
	lines := &lineCounter{}
	n, err := download("/conversations/"+url.PathEscape(id), dest, lines)
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
	var conversations []struct {
		ID       string    `json:"id"`
		Size     int64     `json:"size"`
		Modified time.Time `json:"modified"`
		Title    string    `json:"title"`
	}
	if err := callJSON(http.MethodGet, "/conversations", nil, &conversations); err != nil {
		return err
	}
	if len(conversations) == 0 {
		fmt.Println("No carried-over conversations.")
		return nil
	}
	tw := newTable("ID\tSAVED\tSIZE\tTITLE")
	for _, conv := range conversations {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", conv.ID, conv.Modified.Local().Format("2006-01-02 15:04"), humanSize(conv.Size), conv.Title)
	}
	return tw.Flush()
}
