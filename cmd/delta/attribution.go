package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The user doesn't want commits to say an assistant wrote them: no
// "Co-Authored-By: Claude ...", "Claude-Session: ..." or "Generated with
// Claude Code" lines, and no Claude <noreply@anthropic.com> author, which is
// the git identity Claude Code cloud sessions start with. `delta help` asks
// the agent not to add them, and the CLI makes sure of it:
//
//   - every `delta git` runs with core.hooksPath set to attributionHooks,
//     whose commit-msg hook drops those lines and whose pre-push hook
//     refuses to push a commit that still has them, or that is authored or
//     committed as anyone at anthropic.com;
//   - `delta git clone` also writes core.hooksPath and the commit identity
//     into the new repository's config, so a plain `git commit` in it is
//     covered too.

const (
	commitName  = "Delta"
	commitEmail = "saltedmatzos@gmail.com"
)

// attributionPattern matches the lines the hooks remove or refuse (grep -iE).
const attributionPattern = `^(co-authored-by:.*(claude|anthropic)|claude-session:|[^[:alnum:]]*generated with \[?claude code|[[:space:]]*https://claude\.ai/code/session_)`

var attributionHooks = map[string]string{
	"commit-msg": `#!/bin/sh
# Installed by the delta CLI: drop assistant attribution lines from the
# message, and the blank lines they leave at its end.
pattern='` + attributionPattern + `'
tmp="$1.delta"
grep -viE "$pattern" "$1" | awk '
  /^[[:space:]]*$/ { blank++; next }
  { while (blank > 0) { print ""; blank-- } print }
' >"$tmp" && mv "$tmp" "$1"
`,
	"pre-push": `#!/bin/sh
# Installed by the delta CLI: refuse to push commits that credit an
# assistant, in their message or as their author or committer.
pattern='` + attributionPattern + `'
bad=
while read -r lref lsha rref rsha; do
  case "$lsha" in *[!0]*) ;; *) continue ;; esac
  case "$rsha" in
    *[!0]*) range="$rsha..$lsha" ;;
    *) range="$lsha --not --remotes" ;;
  esac
  for c in $(git rev-list $range); do
    if git log -1 --format='%ae%n%ce' "$c" | grep -qi 'anthropic\.com' ||
      git log -1 --format=%B "$c" | grep -qiE "$pattern"; then
      bad="$bad $c"
    fi
  done
done
[ -z "$bad" ] && exit 0
echo "delta: not pushed: these commits credit an assistant (Co-Authored-By, Claude-Session" >&2
echo "or a Claude <noreply@anthropic.com> author), which the user doesn't want:" >&2
for c in $bad; do git log -1 --format='  %h %s' "$c" >&2; done
echo "Fix them, then push again:" >&2
echo "  git config user.name ` + commitName + `; git config user.email ` + commitEmail + `" >&2
echo "  delta git rebase --exec 'git commit --amend --allow-empty --no-edit --reset-author' <upstream>" >&2
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

// hooksEnv appends core.hooksPath to the GIT_CONFIG_* pairs in env, which
// gitEnv has already numbered.
func hooksEnv(env []string, hooks string) []string {
	n := 0
	out := env[:0:0]
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			fmt.Sscan(v, &n)
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		fmt.Sprintf("GIT_CONFIG_KEY_%d=core.hooksPath", n),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, hooks),
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", n+1))
}
