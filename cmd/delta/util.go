package main

import (
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// opts holds parsed command-line arguments. Flags may appear anywhere and
// be written --name value, --name=value, or -name value.
type opts struct {
	pos   []string
	vals  map[string][]string
	bools map[string]bool
}

func parseOpts(args []string, valueFlags, boolFlags string) (*opts, error) {
	vf := map[string]bool{}
	for _, f := range strings.Fields(valueFlags) {
		vf[f] = true
	}
	bf := map[string]bool{"json": true}
	for _, f := range strings.Fields(boolFlags) {
		bf[f] = true
	}
	o := &opts{vals: map[string][]string{}, bools: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			o.pos = append(o.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" || isNumber(a) {
			o.pos = append(o.pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch {
		case bf[name] && !hasVal:
			o.bools[name] = true
		case vf[name]:
			if !hasVal {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--%s needs a value", name)
				}
				i++
				val = args[i]
			}
			o.vals[name] = append(o.vals[name], val)
		default:
			return nil, fmt.Errorf("unknown option %s", a)
		}
	}
	return o, nil
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func (o *opts) str(name string) string {
	if v := o.vals[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func (o *opts) int(name string, def int) (int, error) {
	s := o.str(name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("--%s must be a whole number", name)
	}
	return n, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func relTime(d time.Duration) string {
	future := d >= 0
	if !future {
		d = -d
	}
	var s string
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		s = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		h := int(d.Hours())
		s = fmt.Sprintf("%dh", h)
		if m := int(d.Minutes()) % 60; h < 10 && m > 0 {
			s += fmt.Sprintf(" %dm", m)
		}
	default:
		days := int(d.Hours()) / 24
		s = fmt.Sprintf("%dd", days)
		if h := int(d.Hours()) % 24; days < 7 && h > 0 {
			s += fmt.Sprintf(" %dh", h)
		}
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// extraTypes are the types an attachment is sent as where mime's own table
// may not know them.
var extraTypes = map[string]string{
	".doc": "application/msword", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls": "application/vnd.ms-excel", ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt": "application/vnd.ms-powerpoint", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt": "application/vnd.oasis.opendocument.text", ".ods": "application/vnd.oasis.opendocument.spreadsheet",
	".odp": "application/vnd.oasis.opendocument.presentation", ".rtf": "application/rtf",
	".txt": "text/plain", ".md": "text/markdown", ".csv": "text/csv", ".pdf": "application/pdf",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif", ".heic": "image/heic", ".webp": "image/webp",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".wav": "audio/wav", ".mp4": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm",
	".zip": "application/zip", ".py": "text/x-python", ".java": "text/x-java", ".c": "text/x-c", ".cpp": "text/x-c++", ".html": "text/html",
}

func contentTypeFor(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if t, ok := extraTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}
