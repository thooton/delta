package main

import (
	"os"
	"path/filepath"
	"strings"
)

// The user doesn't want commit messages or identities to name the assistant
// at all: no "Co-Authored-By: Claude ...", "Claude-Session: ..." or
// "Generated with Claude Code" lines, no Claude <noreply@anthropic.com>
// author (the git identity Claude Code cloud sessions start with), and no
// mention of Claude or Anthropic anywhere in a message. `delta help` asks the
// agent for that, and the CLI makes sure of it:
//
//   - every `delta git` runs with core.hooksPath set to attributionHooks:
//     commit-msg drops the attribution lines and refuses a message that
//     still names Claude or Anthropic, and pre-push refuses to push a commit
//     whose message, author or committer does;
//   - `delta git clone` also writes core.hooksPath and the commit identity
//     into the new repository's config, so a plain `git commit` in it is
//     covered too.

const (
	commitName  = "Delta"
	commitEmail = "saltedmatzos@gmail.com"
)

// attributionLines matches the lines commit-msg drops, and mentionPattern
// what no message or identity may contain (both grep -iE).
const (
	attributionLines = `^(co-authored-by:.*(claude|anthropic)|claude-session:)|generated with \[?claude code|claude\.ai/code/session_`
	mentionPattern   = `claude|anthropic`
)

var attributionHooks = map[string]string{
	"commit-msg": `#!/bin/sh
# Installed by the delta CLI: drop assistant attribution lines from the
# message, and the blank lines they leave at its end, then refuse a message
# that still names Claude or Anthropic.
tmp="$1.delta"
grep -viE '` + attributionLines + `' "$1" | awk '
  /^[[:space:]]*$/ { blank++; next }
  { while (blank > 0) { print ""; blank-- } print }
' >"$tmp" && mv "$tmp" "$1"
if grep -v '^#' "$1" | grep -qiE '` + mentionPattern + `'; then
  echo "delta: not committed: the message names Claude or Anthropic, which the user" >&2
  echo "doesn't want in the history. Reword it without them (say \"cloud sessions\"," >&2
  echo "\"cloud accounts\", \"the agent\") and commit again:" >&2
  grep -v '^#' "$1" | grep -niE '` + mentionPattern + `' | sed 's/^/  /' >&2
  exit 1
fi
`,
	"pre-push": `#!/bin/sh
# Installed by the delta CLI: refuse to push commits whose message, author or
# committer names Claude or Anthropic.
bad=
while read -r lref lsha rref rsha; do
  case "$lsha" in *[!0]*) ;; *) continue ;; esac
  case "$rsha" in
    *[!0]*) range="$rsha..$lsha" ;;
    *) range="$lsha --not --remotes" ;;
  esac
  for c in $(git rev-list $range); do
    if git log -1 --format='%an%n%ae%n%cn%n%ce%n%B' "$c" | grep -qiE '` + mentionPattern + `'; then
      bad="$bad $c"
    fi
  done
done
[ -z "$bad" ] && exit 0
echo "delta: not pushed: these commits name Claude or Anthropic (in the message, or as" >&2
echo "author or committer), which the user doesn't want in the history:" >&2
for c in $bad; do git log -1 --format='  %h %s' "$c" >&2; done
echo "Fix them, then push again: set the identity, and commit the work again with" >&2
echo "messages that don't name Claude or Anthropic, for example:" >&2
echo "  git config user.name ` + commitName + `; git config user.email ` + commitEmail + `" >&2
echo "  git reset --soft <upstream> && delta git commit -m '<a clean message>'" >&2
exit 1
`,
}

// installAttributionHooks writes the hooks into the CLI's config directory
// and returns that directory.
func installAttributionHooks() (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(path), "git-hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for name, body := range attributionHooks {
		file := filepath.Join(dir, name)
		if old, err := os.ReadFile(file); err == nil && string(old) == body {
			continue
		}
		if err := os.WriteFile(file, []byte(body), 0o755); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// cloneConfig returns args with, if they run `git clone`, the hooks and the
// commit identity written into the new repository's config.
func cloneConfig(args []string, hooks string) []string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-C" || a == "-c" {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if a != "clone" {
			return args
		}
		out := append([]string{}, args[:i+1]...)
		out = append(out,
			"--config", "core.hooksPath="+hooks,
			"--config", "user.name="+commitName,
			"--config", "user.email="+commitEmail)
		return append(out, args[i+1:]...)
	}
	return args
}
