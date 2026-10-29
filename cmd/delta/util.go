package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	bf := map[string]bool{}
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
