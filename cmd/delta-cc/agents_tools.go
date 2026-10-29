package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The agent's four tools (read, write, edit and bash). Everything they do
// happens in the session's container, through `<runtime> exec`, with a fixed
// environment: nothing of the command center's reaches it.

const (
	agentHome     = "/root"
	toolMaxLines  = 2000     // what a read or a command's output shows at most…
	toolMaxBytes  = 50 << 10 // …whichever comes first
	toolMaxFile   = 20 << 20 // files read or edited whole
	toolMaxOutput = 100 << 20
	imageMaxSide  = 1568 // pictures read are scaled down to fit this
	imageMaxBytes = 4 << 20
)

// agentSystemPrompt is the system prompt, ahead of the date and the working
// directory (see developerMessage).
const agentSystemPrompt = `You are Delta, a coding assistant the user reaches from the Delta website. You help them by reading files, running commands, editing code and writing new files.

Tools:
- read: read a file's contents
- bash: run a bash command
- edit: change a file by replacing exact text
- write: create or overwrite a file

Guidelines:
- Use bash to list, search and find files (ls, grep, find).
- Read a file before editing it.
- Show file paths clearly when working with files.

You run in your own Linux container (Debian, as root, home /root), made for this session on the user's command center. It keeps its files until the user deletes the session, but it is stopped while the session is idle, so don't leave anything running in the background that the user relies on. Install whatever you need with apt-get.`

func developerMessage() json.RawMessage {
	text := agentSystemPrompt + "\n\nCurrent date: " + time.Now().UTC().Format("2006-01-02") +
		"\nCurrent working directory: " + agentHome
	data, _ := json.Marshal(map[string]string{"role": "developer", "content": text})
	return data
}

var agentTools = []map[string]any{
	{
		"type": "function", "name": "read", "strict": false,
		"description": fmt.Sprintf("Read the contents of a file. Supports text files and images (jpg, png, gif, webp). Images are sent as attachments. For text files, output is truncated to %d lines or %dKB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.", toolMaxLines, toolMaxBytes>>10),
		"parameters": schema(map[string]any{
			"path":   prop("string", "Path to the file to read (relative or absolute)"),
			"offset": prop("number", "Line number to start reading from (1-indexed)"),
			"limit":  prop("number", "Maximum number of lines to read"),
		}, "path"),
	},
	{
		"type": "function", "name": "bash", "strict": false,
		"description": fmt.Sprintf("Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to the last %d lines or %dKB (whichever is hit first). If truncated, the full output is saved to a file. Optionally provide a timeout in seconds.", toolMaxLines, toolMaxBytes>>10),
		"parameters": schema(map[string]any{
			"command": prop("string", "Shell command to execute"),
			"timeout": prop("number", "Timeout in seconds (optional, no default timeout)"),
		}, "command"),
	},
	{
		"type": "function", "name": "edit", "strict": false,
		"description": "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes.",
		"parameters": schema(map[string]any{
			"path": prop("string", "Path to the file to edit (relative or absolute)"),
			"edits": map[string]any{
				"type":        "array",
				"description": "One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead.",
				"items": schema(map[string]any{
					"oldText": prop("string", "Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."),
					"newText": prop("string", "Replacement text for this targeted edit."),
				}, "oldText", "newText"),
			},
		}, "path", "edits"),
	},
	{
		"type": "function", "name": "write", "strict": false,
		"description": "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories.",
		"parameters": schema(map[string]any{
			"path":    prop("string", "Path to the file to write (relative or absolute)"),
			"content": prop("string", "Content to write to the file"),
		}, "path", "content"),
	},
}

func schema(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

// toolArgs holds every tool's arguments.
type toolArgs struct {
	Path    string     `json:"path"`
	Offset  float64    `json:"offset"`
	Limit   float64    `json:"limit"`
	Content *string    `json:"content"`
	Edits   []textEdit `json:"edits"`
	OldText *string    `json:"oldText"` // a single edit, given without edits[]
	NewText *string    `json:"newText"`
	Command string     `json:"command"`
	Timeout float64    `json:"timeout"`
}

type textEdit struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// runTool runs one tool call and returns what goes back to the model as its
// output: text, or a list of parts when there is a picture.
func (s *agentSession) runTool(ctx context.Context, name, arguments string) any {
	var args toolArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return fmt.Sprintf("Error: the arguments aren't valid JSON: %v", err)
	}
	if strings.HasPrefix(args.Path, "~/") {
		args.Path = agentHome + args.Path[1:]
	}
	var out any
	var err error
	switch name {
	case "read":
		out, err = s.toolRead(ctx, args)
	case "write":
		out, err = s.toolWrite(ctx, args)
	case "edit":
		out, err = s.toolEdit(ctx, args)
	case "bash":
		out, err = s.toolBash(ctx, args)
	default:
		err = fmt.Errorf("there is no tool named %q", name)
	}
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

// ---------------------------------------------------------------------------
// Running things in the container

// containerCmd makes a command that runs argv in the session's container,
// in dir.
func (s *agentSession) containerCmd(ctx context.Context, dir string, argv ...string) *exec.Cmd {
	args := []string{"exec", "-i", "-w", dir, "-e", "HOME=" + agentHome, "-e", "LANG=C.UTF-8", "-e", "TERM=dumb",
		containerName(s.meta.ID)}
	cmd := exec.CommandContext(ctx, s.svc.cfg.Runtime, append(args, argv...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	if xdg := runtimeDir(); xdg != "" {
		cmd.Env = append(cmd.Env, "XDG_RUNTIME_DIR="+xdg)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// inContainer runs argv in the container with stdin, and returns its
// output, or an error with what it said on stderr if it failed. At most max
// bytes of output are kept.
func (s *agentSession) inContainer(ctx context.Context, stdin io.Reader, max int64, argv ...string) ([]byte, error) {
	cmd := s.containerCmd(ctx, agentHome, argv...)
	cmd.Stdin = stdin
	var out, errOut bytes.Buffer
	cmd.Stdout = &cappedWriter{w: &out, left: max}
	cmd.Stderr = &cappedWriter{w: &errOut, left: 64 << 10}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("interrupted")
		}
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return nil, errors.New(lastLine(msg))
		}
		return nil, err
	}
	return out.Bytes(), nil
}

// cappedWriter passes on the first `left` bytes and quietly drops the rest,
// so a command with endless output can't fill memory or the disk.
type cappedWriter struct {
	w     io.Writer
	left  int64
	total int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.total += int64(len(p))
	if c.left > 0 {
		n := min(int64(len(p)), c.left)
		c.left -= n
		if _, err := c.w.Write(p[:n]); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// readWhole reads a file in the container, up to toolMaxFile bytes, saying
// whether there was more.
func (s *agentSession) readWhole(ctx context.Context, p string) ([]byte, bool, error) {
	if p == "" {
		return nil, false, errors.New("no path was given")
	}
	data, err := s.inContainer(ctx, nil, toolMaxFile+1, "head", "-c", fmt.Sprint(toolMaxFile+1), "--", p)
	if err != nil {
		return nil, false, err
	}
	if len(data) > toolMaxFile {
		return data[:toolMaxFile], true, nil
	}
	return data, false, nil
}

func (s *agentSession) writeWhole(ctx context.Context, p string, data io.Reader) error {
	_, err := s.inContainer(ctx, data, 4096, "sh", "-c", `mkdir -p -- "$(dirname -- "$1")" && cat > "$1"`, "sh", p)
	return err
}

// ---------------------------------------------------------------------------
// read

func (s *agentSession) toolRead(ctx context.Context, args toolArgs) (any, error) {
	data, more, err := s.readWhole(ctx, args.Path)
	if err != nil {
		return nil, err
	}
	if mime := http.DetectContentType(data); strings.HasPrefix(mime, "image/") {
		if more {
			return nil, fmt.Errorf("the picture is over %d MB", toolMaxFile>>20)
		}
		return readImage(data, mime)
	}
	if bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 {
		return fmt.Sprintf("%s is a binary file. Use bash to inspect it.", args.Path), nil
	}
	text := strings.ToValidUTF8(string(data), "�")
	lines := strings.Split(text, "\n")
	start := 0
	if args.Offset > 1 {
		start = int(args.Offset) - 1
	}
	if start >= len(lines) {
		return nil, fmt.Errorf("offset %d is beyond the end of the file (%d lines in all)", start+1, len(lines))
	}
	end, limited := len(lines), false
	if args.Limit >= 1 && start+int(args.Limit) < end {
		end, limited = start+int(args.Limit), true
	}
	selected := lines[start:end]
	var b strings.Builder
	shown := 0
	for _, line := range selected {
		add := len(line)
		if shown > 0 {
			add++
		}
		if shown == toolMaxLines || b.Len()+add > toolMaxBytes {
			break
		}
		if shown > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		shown++
	}
	total := fmt.Sprint(len(lines))
	if more {
		total = fmt.Sprintf("the first %d MB", toolMaxFile>>20)
	}
	switch {
	case shown == 0 && len(selected) > 0:
		return fmt.Sprintf("[Line %d is longer than %dKB. Use bash: sed -n '%dp' %s | head -c %d]",
			start+1, toolMaxBytes>>10, start+1, args.Path, toolMaxBytes), nil
	case shown < len(selected):
		fmt.Fprintf(&b, "\n\n[Showing lines %d-%d of %s. Use offset=%d to continue.]", start+1, start+shown, total, start+shown+1)
	case limited:
		fmt.Fprintf(&b, "\n\n[%d more lines in file. Use offset=%d to continue.]", len(lines)-end, end+1)
	case more:
		fmt.Fprintf(&b, "\n\n[The file goes on past %d MB. Use bash to read the rest.]", toolMaxFile>>20)
	}
	return b.String(), nil
}

// readImage returns a picture as an attachment, scaled down if it is large.
func readImage(data []byte, mime string) (any, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err == nil {
		b := img.Bounds()
		if max(b.Dx(), b.Dy()) > imageMaxSide || len(data) > imageMaxBytes {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, scaleDown(img, imageMaxSide), &jpeg.Options{Quality: 85}); err != nil {
				return nil, err
			}
			data, mime = buf.Bytes(), "image/jpeg"
		}
	} else if len(data) > imageMaxBytes {
		return nil, fmt.Errorf("the picture is too large to show (%d MB)", len(data)>>20)
	}
	return []map[string]string{
		{"type": "input_text", "text": "Read image file [" + mime + "]"},
		{"type": "input_image", "detail": "auto", "image_url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)},
	}, nil
}

// scaleDown shrinks img to fit side×side by averaging the pixels each new
// pixel covers, on white (JPEG has no transparency).
func scaleDown(img image.Image, side int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := float64(side) / float64(max(w, h))
	if scale > 1 {
		scale = 1
	}
	nw, nh := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(src, src.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(src, src.Bounds(), img, b.Min, draw.Over)
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := y*h/nh, max((y+1)*h/nh, y*h/nh+1)
		for x := 0; x < nw; x++ {
			x0, x1 := x*w/nw, max((x+1)*w/nw, x*w/nw+1)
			var r, g, bl, n uint32
			for sy := y0; sy < y1; sy++ {
				i := src.PixOffset(x0, sy)
				for sx := x0; sx < x1; sx++ {
					r += uint32(src.Pix[i])
					g += uint32(src.Pix[i+1])
					bl += uint32(src.Pix[i+2])
					i += 4
					n++
				}
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), 255})
		}
	}
	return dst
}

// ---------------------------------------------------------------------------
// write

func (s *agentSession) toolWrite(ctx context.Context, args toolArgs) (any, error) {
	if args.Path == "" || args.Content == nil {
		return nil, errors.New("write needs a path and content")
	}
	if err := s.writeWhole(ctx, args.Path, strings.NewReader(*args.Content)); err != nil {
		return nil, err
	}
	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(*args.Content), args.Path), nil
}

// ---------------------------------------------------------------------------
// edit

func (s *agentSession) toolEdit(ctx context.Context, args toolArgs) (any, error) {
	edits := args.Edits
	if len(edits) == 0 && args.OldText != nil && args.NewText != nil {
		edits = []textEdit{{*args.OldText, *args.NewText}}
	}
	if args.Path == "" || len(edits) == 0 {
		return nil, errors.New("edit needs a path and at least one edit")
	}
	data, more, err := s.readWhole(ctx, args.Path)
	if err != nil {
		return nil, fmt.Errorf("could not edit %s: %w", args.Path, err)
	}
	if more {
		return nil, fmt.Errorf("%s is over %d MB; change it with bash instead", args.Path, toolMaxFile>>20)
	}
	updated, err := applyEdits(string(data), edits, args.Path)
	if err != nil {
		return nil, err
	}
	if err := s.writeWhole(ctx, args.Path, strings.NewReader(updated)); err != nil {
		return nil, err
	}
	return fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(edits), args.Path), nil
}

// applyEdits makes every edit to the file's text, each matched against the
// original. Text that doesn't match exactly is matched again ignoring
// trailing spaces and typographic quotes, dashes and spaces, which models
// often get wrong. Line endings and a byte order mark are kept.
func applyEdits(content string, edits []textEdit, name string) (string, error) {
	bom := ""
	if strings.HasPrefix(content, "\uFEFF") {
		bom, content = "\uFEFF", content[3:]
	}
	crlf := strings.Contains(content, "\r\n")
	content = toLF(content)
	type span struct {
		start, end int
		text       string
	}
	var spans []span
	var fuzzy string
	var at []int
	for i, e := range edits {
		old := toLF(e.OldText)
		which := ""
		if len(edits) > 1 {
			which = fmt.Sprintf(" (edit %d)", i+1)
		}
		if old == "" {
			return "", fmt.Errorf("oldText%s is empty", which)
		}
		var start, end int
		switch n := strings.Count(content, old); {
		case n == 1:
			start = strings.Index(content, old)
			end = start + len(old)
		case n > 1:
			return "", fmt.Errorf("found %d occurrences of oldText%s in %s; it must be unique, so include more of the text around it", n, which, name)
		default:
			if at == nil {
				fuzzy, at = fuzzyView(content)
			}
			needle, _ := fuzzyView(old)
			switch n := strings.Count(fuzzy, needle); {
			case n == 0 || needle == "":
				return "", fmt.Errorf("could not find oldText%s in %s; it must match the file exactly, including whitespace and line breaks", which, name)
			case n > 1:
				return "", fmt.Errorf("found %d occurrences of oldText%s in %s; it must be unique, so include more of the text around it", n, which, name)
			}
			i := strings.Index(fuzzy, needle)
			start = at[i]
			last := at[i+len(needle)-1]
			_, size := utf8.DecodeRuneInString(content[last:])
			end = last + size
		}
		spans = append(spans, span{start, end, toLF(e.NewText)})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return "", errors.New("two edits overlap; merge them into one")
		}
	}
	out := content
	for i := len(spans) - 1; i >= 0; i-- {
		out = out[:spans[i].start] + spans[i].text + out[spans[i].end:]
	}
	if out == content {
		return "", fmt.Errorf("no changes made to %s: the new text is the same as the old", name)
	}
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return bom + out, nil
}

func toLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// fuzzyView returns text with each line's trailing whitespace removed and
// typographic quotes, dashes and spaces made plain, along with where in
// text each of its bytes came from.
func fuzzyView(text string) (string, []int) {
	var b strings.Builder
	var at []int
	off := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		body := strings.TrimSuffix(line, "\n")
		for i, r := range strings.TrimRightFunc(body, unicode.IsSpace) {
			n := b.Len()
			b.WriteRune(plainRune(r))
			for ; n < b.Len(); n++ {
				at = append(at, off+i)
			}
		}
		if len(body) < len(line) {
			b.WriteByte('\n')
			at = append(at, off+len(body))
		}
		off += len(line)
	}
	return b.String(), at
}

func plainRune(r rune) rune {
	switch r {
	case '‘', '’', '‚', '‛':
		return '\''
	case '“', '”', '„', '‟':
		return '"'
	case '‐', '‑', '‒', '–', '—', '―', '−':
		return '-'
	case ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', '　':
		return ' '
	}
	return r
}

// ---------------------------------------------------------------------------
// bash

var setsidNote = regexp.MustCompile(`(?m)^setsid: child \d+ did not exit normally.*\n?`)

// toolBash runs a command in the container. It runs as the leader of its own
// process group, which writes its pid where a kill can find it: stopping the
// `exec` client alone would leave the command running in the container.
func (s *agentSession) toolBash(ctx context.Context, args toolArgs) (any, error) {
	if strings.TrimSpace(args.Command) == "" {
		return nil, errors.New("no command was given")
	}
	id := randomUUID()
	pidFile := "/tmp/.delta-exec-" + id
	f, err := os.CreateTemp(s.dir, "output-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	out := &cappedWriter{w: f, left: toolMaxOutput}
	// Not tied to ctx: an interrupt kills the command's process group instead.
	cmd := s.containerCmd(context.Background(), agentHome,
		"setsid", "-w", "-f", "bash", "-c", `echo $$ > "$1"; exec bash -lc "$2"`, "bash", pidFile, args.Command)
	// The same writer for both, so exec writes to it from one goroutine at a time.
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var timeout <-chan time.Time
	if args.Timeout > 0 {
		timer := time.NewTimer(time.Duration(args.Timeout * float64(time.Second)))
		defer timer.Stop()
		timeout = timer.C
	}
	kill := func() {
		kctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.inContainer(kctx, nil, 0, "sh", "-c", `kill -9 -- "-$(cat "$1")" 2>/dev/null; rm -f "$1"`, "sh", pidFile)
	}
	stopped := ""
	select {
	case <-done:
	case <-ctx.Done():
		stopped = "Command aborted"
		kill()
		<-done
	case <-timeout:
		stopped = fmt.Sprintf("Command timed out after %g seconds", args.Timeout)
		kill()
		<-done
	}
	go func() {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.inContainer(rctx, nil, 0, "rm", "-f", pidFile)
	}()

	text, shownFrom, shownTo, lines := tailOf(f)
	text = setsidNote.ReplaceAllString(text, "")
	var b strings.Builder
	b.WriteString(text)
	if shownFrom > 1 || out.total > toolMaxOutput {
		saved := "/tmp/delta-output-" + id[:8] + ".log"
		f.Seek(0, io.SeekStart)
		note := ""
		if out.total > toolMaxOutput {
			note = fmt.Sprintf(", the first %d MB of it", toolMaxOutput>>20)
		}
		sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if s.writeWhole(sctx, saved, f) == nil {
			fmt.Fprintf(&b, "\n\n[Showing lines %d-%d of %d. Full output%s: %s]", shownFrom, shownTo, lines, note, saved)
		} else {
			fmt.Fprintf(&b, "\n\n[Showing lines %d-%d of %d.]", shownFrom, shownTo, lines)
		}
		cancel()
	}
	if strings.TrimSpace(b.String()) == "" {
		b.Reset()
		b.WriteString("(no output)")
	}
	switch {
	case stopped != "":
		b.WriteString("\n\n" + stopped)
	case cmd.ProcessState != nil && cmd.ProcessState.ExitCode() != 0:
		fmt.Fprintf(&b, "\n\nCommand exited with code %d", cmd.ProcessState.ExitCode())
	}
	return b.String(), nil
}

// tailOf returns the end of a command's output, at most toolMaxLines lines
// and toolMaxBytes bytes, with the numbers of the first and last lines it
// shows and how many lines there are in all.
func tailOf(f *os.File) (text string, from, to, lines int) {
	f.Seek(0, io.SeekStart)
	var size int64
	var last byte
	buf := make([]byte, 64<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			lines += bytes.Count(buf[:n], []byte{'\n'})
			size += int64(n)
			last = buf[n-1]
		}
		if err != nil {
			break
		}
	}
	if size == 0 {
		return "", 1, 0, 0
	}
	if last != '\n' {
		lines++ // an unfinished last line
	}
	start := max(0, size-toolMaxBytes-1)
	data := make([]byte, size-start)
	n, _ := f.ReadAt(data, start)
	data = bytes.TrimSuffix(data[:n], []byte{'\n'})
	if start > 0 {
		// Drop the line cut in two, unless it is all there is.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = data[1:]
		}
	}
	all := strings.Split(strings.ToValidUTF8(string(data), "\uFFFD"), "\n")
	if len(all) > toolMaxLines {
		all = all[len(all)-toolMaxLines:]
	}
	return strings.Join(all, "\n"), lines - len(all) + 1, lines, lines
}
