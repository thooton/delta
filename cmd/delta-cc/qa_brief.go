package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The instructions `delta qa start` prints for a job: what its role does,
// how, and what the ledger already knows about its app. q.mu is held.
func (q *qaService) briefLocked(run *qaRun, j *qaJob, app qaRunApp) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	grow := j.App != "" && q.state.Settings.Apps[j.App]
	name := appName(j.App)

	if j.Role == "refactor" {
		w("NIGHTLY QA: SWEEPING REFACTOR OF thooton/%s\n\n", j.Repo)
	} else {
		verb := map[string]string{"spec": "WRITE THE SPEC FOR", "explore": "EXPLORE", "fix": "TRIAGE AND FIX"}[j.Role]
		w("NIGHTLY QA: %s %s (%s, directory %s/)\n\n", verb, strings.ToUpper(name), j.App, app.Dir)
	}
	w(`Every night, agents go through each of the user's phone apps (thooton/sonim,
for their Sonim XP3 flip phone) and tablet apps (thooton/tablet, for their
reMarkable 2), find what is wrong, and fix it, with nobody watching. For
each app, a spec job writes SPEC.md once (the user skims it); after that an
explore job drives the app in an emulator and files findings, and a fix job
decides what to fix, fixes it and pushes to master. When every app is
through, a refactor job sweeps each repository. Nothing waits for review:
the phone and tablet never update by themselves, and the user installs
updates from their Updater apps when they choose.

You are the %s job of run %s. Nobody will answer questions: decide for
yourself, as the user would want, and finish.

`, j.Role, run.ID)

	w(`GROUND RULES
- Use "delta git" and "delta gh" for anything that talks to GitHub (they
  carry the user's token); "delta help" explains them. Clone with
    delta git clone https://github.com/thooton/%s
- Read the repository's AGENTS.md first, and follow it: it holds rules
  every agent has learnt the hard way.
- Never name Claude or Anthropic in a commit, and add no attribution lines
  ("delta git" refuses them).
- %s
- Your key reaches the QA routes and GitHub through the command center, and
  nothing else. Never repeat or commit it.

`, j.Repo, map[bool]string{
		true:  "You run alone: every app's jobs are over for the night, and the whole\n  repository is yours.",
		false: "Other QA jobs are working in the same repositories right now, each on\n  its own app. Stay inside yours: change only " + app.Dir + "/ (and, if you must,\n  files only it uses).",
	}[j.App == ""])

	switch j.Role {
	case "spec":
		q.briefSpec(w, app, name)
	case "explore":
		q.briefExplore(w, app, name, grow)
	case "fix":
		q.briefFix(w, j, app, name, grow)
	case "refactor":
		q.briefRefactor(w, run, j)
	}

	if j.App != "" && j.Role != "spec" {
		q.briefHistory(w, j)
	}

	w(`
COMMANDS
  delta qa brief                    Print these instructions again.
  delta qa findings                 The app's findings so far, as JSON.
  delta qa shot <name> [file]       Save a finding's screenshot, to look at.
`)
	if j.Role == "explore" || j.Role == "fix" {
		w(`  delta qa find --kind <kind> --title <title> --detail <text> [--shot <png>]...
                                    File a finding: kind is bug, problem,
                                    inconsistency or feature; up to %d
                                    screenshots. Prints its id.
`, maxQAShots)
	}
	if j.Role == "fix" {
		w(`  delta qa triage <id> <status> --reason <one line> [--commit <sha>]
                                    Say what became of a finding: fixed,
                                    passed, declined, grown or reverted. The
                                    reason is for the user; fixed, grown and
                                    reverted need the commit on master.
`)
	}
	if app.Phone && (j.Role == "explore" || j.Role == "fix") {
		w(`  delta qa device <op> [args]       Drive your emulator (see THE EMULATOR).
`)
	}
	w(`  delta qa done "<summary>"         Finish: a few sentences for the user on
                                    what you did. Your key stops working.

When you are finished, run "delta qa done". Until then the job counts as
running; after %s it fails.
`, humanHours(qaJobLife[j.Role]))
	return b.String()
}

func (q *qaService) briefSpec(w func(string, ...any), app qaRunApp, name string) {
	device := "the Sonim XP3: a 240x320 screen, a d-pad, soft keys and a T9\nkeypad. The user drives it one-handed, with keys, never touch"
	if !app.Phone {
		device = "the reMarkable 2: a 1404x1872 e-ink screen, touch and pen, no\nkeyboard"
	}
	w(`YOUR JOB
%s has no SPEC.md yet. Write %s/SPEC.md: how the app should behave, precise
enough that a QA agent who has never seen it can test it screen by screen,
and short enough that the user can skim it over coffee. From the next run
on, QA tests the app against it, and agents that grow the app add to it.

It runs on %s.

Where to learn what the app should do:
- the code, all of it;
- %s/AGENTS.md and README.md, where earlier agents wrote down what the
  user asked for and decided against;
- the history: "git log --stat -- %s" (clone without --depth). Commit
  subjects are written for the user and say what was asked for and fixed;%s
Write down what the app is meant to do, not every accident of the code. If
the code looks wrong, write what was clearly intended and say so under Open
questions.

Shape it like this:

  # %s

  ## Purpose
  What it is for, who uses it and how (one hand on a flip phone, or a pen
  on e-ink), and what it must stay: small, quick, etc. This is what agents
  judge new features against, so say plainly what it must never become.

  ## Not wanted
  What the user decided against (AGENTS.md and the history say), so nobody
  proposes it again.

  ## Screens
  Each screen: what is on it, and what every key (or tap) does there,
  including Back, and what happens at the edges (first and last row,
  empty lists, long text).

  ## Behaviour
  Data and where it lives, what survives a restart, network use and what
  happens offline or on errors, permissions, anything timed.

  ## Look
  Only what DESIGN.md (or the app's AGENTS.md) doesn't already say.

  ## Open questions
  Where you couldn't tell what is intended. The user answers these.

Write statements a tester can check ("Down on the last row stays there",
"An empty list shows No tasks"), not aspirations. Keep it under about 250
lines.

Change nothing else. SPEC.md isn't part of the app's build, so it needs no
version bump. Commit it as "%s: spec for nightly QA", pull with
"delta git pull --rebase origin master", push with
"delta git push origin HEAD:master", and finish with "delta qa done",
naming the open questions you left for the user.
`, name, app.Dir, device, app.Dir, app.Dir,
		map[bool]string{true: "\n- DESIGN.md at the repository's root: the family look most apps share.", false: ""}[app.Phone],
		name, name)
}

func (q *qaService) briefExplore(w func(string, ...any), app qaRunApp, name string, grow bool) {
	w(`YOUR JOB
Use %s the way the user would, in an emulator, and file a finding for each
thing that is wrong with it. You change no code and push nothing: a fix job
after you decides what to do about what you found.

First read %s/SPEC.md (what the app should do), %s/AGENTS.md, the code
as far as you need it, and the app's findings below.

Look for:
- bugs: crashes, freezes, wrong results, lost data, anything that
  contradicts SPEC.md;
- problems: things that work, but badly for the user: text cut off or
  overlapping, focus lost or not visible, dead ends you can't get out of,
  slow screens, confusing wording, keys that surprise;
- inconsistencies: with SPEC.md's Look, %s, and the user's
  other apps (the same key doing different things in two apps, say).

Go through every screen and flow in SPEC.md, then the edges: empty and full
lists, long text, Back in the middle of something, leaving and relaunching
(is the state kept?), cleared data, no network. Look at a screenshot after
every few steps; that's how you'll see most problems.

A good finding is one problem, with:
- a title in the user's terms ("Saving a long task cuts its last word");
- details: the steps from launch, what you expected (quote SPEC.md where it
  says), what happened, and anything you learnt from the code or the log;
- a screenshot or two that show it.
Leave out nitpicks the user wouldn't care about. Don't file what the
emulator can't do (a camera, a SIM, the real SD card) as the app's fault.
Spend about an hour; file the findings that matter most first.

`, name, app.Dir, app.Dir, map[bool]string{true: "DESIGN.md (the family look)", false: "the tablet's\n  conventions in AGENTS.md (black on white, large targets, no\n  explanations on screen)"}[app.Phone])
	if grow {
		w(`GROWING
The user lets %s grow. Besides what is wrong, you may propose up to %d
features (kind "feature") that would make it more useful to this user: in
keeping with SPEC.md's Purpose, nothing under Not wanted, small enough to
build and check in a night, and something you're confident they'd want,
judging by how the app is built and used. Better none than a doubtful one:
an app that slowly fills with features nobody asked for is worse, not
better. Learn from the grown features they turned down, below. Say in the
details what it does, why it fits, and how it works with the %s.

`, name, maxQAFeaturesPerJob, map[bool]string{true: "keys", false: "pen and touch"}[app.Phone])
	} else {
		w("The user hasn't let %s grow, so propose no features.\n\n", name)
	}
	if app.Phone {
		q.briefDevice(w, false)
	} else {
		q.briefTablet(w, app)
	}
	w(`
Finish with "delta qa done" and a few sentences: what you covered, and what
you found.
`)
}

func (q *qaService) briefDevice(w func(string, ...any), fix bool) {
	w(`THE EMULATOR
An Android 8.1 (API 27) emulator, as the XP3 runs, with its 240x320 mdpi
screen and a d-pad and keypad. It runs on a GitHub Actions machine, kept for
you until your app's jobs are done; the commands reach it through the
command center, so each takes a second or two. Every app is installed on it,
built from master when the run began. The XP3 is driven with keys, never
touch, so use keys only.

  delta qa device launch                  Start the app (from its launcher icon).
  delta qa device key <KEY>...            Press keys, in order: Android's KEYCODE_
                                          names without the prefix: DPAD_UP,
                                          DPAD_DOWN, DPAD_LEFT, DPAD_RIGHT,
                                          DPAD_CENTER, BACK, MENU, ENTER, DEL,
                                          0-9, STAR, POUND, CALL, ENDCALL,
                                          SOFT_LEFT, SOFT_RIGHT, HOME.
  delta qa device text "<text>"           Type text into the focused field.
  delta qa device shot [file.png]         Screenshot (default shot.png); then
                                          look at it.
  delta qa device ui [file.xml]           The screen's view hierarchy: its text,
                                          and what is focused.
  delta qa device log [--clear]           The log's warnings and errors: crashes
                                          show as FATAL EXCEPTION, freezes as ANR.
  delta qa device stop                    Force-stop the app.
  delta qa device reset                   Clear the app's data, as if new.
  delta qa device shell "<command>"       Any adb shell command.
`)
	if fix {
		w(`  delta qa device install [--ref <ref>]   Build the app from sonim at that branch
                                          or commit (default master) on the
                                          emulator's machine and install it: a
                                          compile error comes back as the error.
                                          Takes a few minutes.
`)
	}
	w(`
Apps that sign in to the command center have no login on the emulator, and
must not get one: never type a token or address into them. Check what works
signed out (the sign-in screen itself, what works offline, how errors look);
"it isn't signed in" is not a finding.
`)
}

func (q *qaService) briefTablet(w func(string, ...any), app qaRunApp) {
	w(`THE EMULATOR
tablet's tools/emulator.py runs the app's real QML and Go backend in a
1404x1872 window, as AppLoad does on the tablet. In this session:

  pip install PySide6                      (once; Go is already here)
  apt-get install -y libegl1 libgl1 libxkbcommon0 libfontconfig1 libdbus-1-3
                                           (once, if Qt says a library is missing)
  python3 %[1]s/tools/build.py               (once, if the app's .qrc is generated)
  python3 tools/emulator.py %[1]s --offscreen --control 47011 > /tmp/emulator.log 2>&1 &
  python3 tools/drive.py tap 700 400       Tap at screen pixels.
  python3 tools/drive.py swipe 700 1500 700 400 [ms]
  python3 tools/drive.py type "text"       Type into the focused field.
  python3 tools/drive.py key Return        A key, by Qt's name (Return, Backspace, Tab).
  python3 tools/drive.py shot shot.png     Screenshot; then look at it.
  python3 tools/drive.py quit              Close the app (Escape), to relaunch.

/tmp/emulator.log has the backend's output and QML's warnings and errors.
--env NAME=VALUE passes settings to the backend (the app's AGENTS.md says
which); point any data folder at a fresh temporary one. The tablet is touch
and pen only: no keys. E-ink: anything that only works with animation or
colour is a problem.
`, app.Dir)
}

func (q *qaService) briefFix(w func(string, ...any), j *qaJob, app qaRunApp, name string, grow bool) {
	items := q.fixItemsLocked(j.App, j.ID)
	w(`YOUR JOB
An explore job drove %s in an emulator tonight and filed findings. Below are
every finding still open (some may be from earlier nights)%s.
For each one, decide, then act:

- fixed: a real problem you understand, whose fix stays inside %s/.
  Fix it.
- passed: real, but for the user to decide or do: it needs their taste or a
  decision SPEC.md doesn't make, hardware or a login the emulator lacks, a
  redesign too big for one night, or a change to shared code (common/,
  gradle/, tools/) that other jobs are working beside tonight.
- declined: not a problem: it works as SPEC.md says and that's right, it
  repeats another finding, it's the emulator's doing, or it isn't worth a
  change.
`, name, map[bool]string{true: ",\nand grown features the user turned down", false: ""}[grow || hasTurnedDown(items)], app.Dir)
	if grow {
		w(`- grown: a proposed feature worth having. At most one a night: pick the
  one the user would most want, only if it truly fits SPEC.md's Purpose and
  nothing under Not wanted. Add it to SPEC.md where it belongs (so QA tests
  it from now on) and build it. Decline the rest, saying why.
`)
	} else {
		w(`- The user hasn't let this app grow: decline any feature.
`)
	}
	w(`- reverted: for a grown feature the user turned down (👎): take it out of
  the code and SPEC.md, and add a line under SPEC.md's Not wanted so it
  isn't proposed again.

Every reason is one line, for the user, in plain words.

Work like this:
1. Read %s/SPEC.md, %s/AGENTS.md and the code. Reproduce each bug%s
   before you fix it.
2. Fix on a branch, qa/%s. Follow AGENTS.md: raise the app's version in
   the same commit (patch for fixes, minor when a feature grew), and lead
   the subject with the app and its new version, then a sentence for the
   user: "%s 1.4.2: long tasks no longer lose their last word". One commit
   for the night's changes is fine.
3. Check it: %s
4. Put it on master: "delta git pull --rebase origin master", then
   "delta git push origin HEAD:master" (other jobs push too: if it's
   refused, pull and push again). Delete your branch
   ("delta git push origin --delete qa/%s").
5. Watch the build ("delta gh run watch -R thooton/%s"). A red build stops
   every app's release, not just yours: if yours broke it, fix it at once.
6. Triage every finding with "delta qa triage", giving the commit as it is
   on master.

`, app.Dir, app.Dir, map[bool]string{true: " on the\n   emulator (THE EMULATOR, below)", false: " in the\n   emulator (THE EMULATOR, below)"}[app.Phone],
		j.ID, name,
		map[bool]string{
			true:  "push the branch, run\n   \"delta qa device install --ref qa/" + j.ID + "\", and follow each\n   finding's steps again on the emulator, checking the log. Fix what\n   isn't fixed.",
			false: "\"go test ./...\" in the backend, \"python3 " + app.Dir + "/tools/build.py\",\n   and each finding's steps again in the emulator. Fix what isn't fixed.",
		}[app.Phone], j.ID, j.Repo)
	if len(items) == 0 {
		w("THE FINDINGS\nNone are open now. Finish with \"delta qa done\".\n\n")
	} else {
		w("THE FINDINGS (%d)\n", len(items))
		for _, f := range items {
			state := "open"
			if f.Status == "grown" {
				state = "grown, and the user turned it down: revert it"
			}
			w("\n[%s] %s (%s; %s; found %s)\n", f.ID, f.Title, f.Kind, state, f.Found.Format("Jan 2"))
			if len(f.Shots) > 0 {
				w("Screenshots: %s\n", strings.Join(f.Shots, ", "))
			}
			w("%s\n", indent(f.Detail))
		}
		w("\n")
	}
	if app.Phone {
		q.briefDevice(w, true)
	} else {
		q.briefTablet(w, app)
	}
	w(`
"delta qa done" refuses until every finding above is triaged. Finish with a
few sentences: what you fixed, grew, passed on and declined.
`)
}

// humanHours says a job's time limit in words: "90 minutes", "2 hours".
func humanHours(d time.Duration) string {
	if d%time.Hour != 0 {
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
	return fmt.Sprintf("%d %s", int(d.Hours()), plural(int(d.Hours()), "hour", "hours"))
}

func hasTurnedDown(items []qaFinding) bool {
	return slices.ContainsFunc(items, func(f qaFinding) bool { return f.Status == "grown" })
}

func (q *qaService) briefRefactor(w func(string, ...any), run *qaRun, j *qaJob) {
	w(`YOUR JOB
A sweeping refactor of all of thooton/%s: make the code markedly simpler,
smaller and more consistent, without changing what it does. Dozens of
agents change these repositories every week, each looking only at its own
corner, so duplication, drift between apps, dead code and needless
complexity pile up. You are the one job that looks at the whole and cleans
it up. Small and timid won't keep up: go big.

What to go after:
- code duplicated between apps or files: move it into the shared code
  (%s) and use it everywhere;
- the same thing done different ways in different places: pick the best and
  use it throughout;
- dead code, unused resources, stale flags and leftovers of removed
  features;
- long functions and files, deep nesting, needless indirection and
  abstractions with one user;
- comments and docs (AGENTS.md files included) that no longer match the
  code.

What must not change: what the user sees and does, what is stored, and what
is sent anywhere. Tomorrow night's QA goes through every app and fixes
what a refactor broke, so don't let fear of breaking something keep you
small; but never change behaviour on purpose, and run every test there is.

`, j.Repo, map[string]string{"sonim": "common/core and common/ui", "tablet": "tools/, and a shared Go package if it earns its place", "delta": "internal/ and shared helpers in cmd/"}[j.Repo])
	switch j.Repo {
	case "sonim":
		w(`Rules for sonim (read AGENTS.md):
- Every app whose sources or resources change gets a patch version bump
  (versionName and versionCode) in the same commit, and so does every app
  using shared code you change. Commit per app where you can, the subject
  leading with the app and its new version: "List 1.4.3: internal
  cleanup".
- Java 8, API 27, no third-party dependencies. Never touch signing/.
- Check on CI, which runs every app's self-test and builds every APK:
  push a branch (qa/refactor-%s), open a pull request
  ("delta gh pr create"), and watch its checks
  ("delta gh pr checks --watch"). Fix until green, then merge it
  ("delta gh pr merge --rebase --delete-branch"). (You can also install
  the Android SDK here, from dl.google.com, to build as you go.)
`, run.ID)
	case "tablet":
		w(`Rules for tablet (read AGENTS.md):
- Every app whose build changes gets a patch bump of "version" in its
  app/manifest.json, in the same commit, subject leading with the app and
  its new version ("Biblioteca 1.0.4: internal cleanup").
- Run each backend's tests (go vet ./... and go test ./...) and every
  */tools/build.py, and look at a screenshot of any app whose QML you
  changed (tools/emulator.py --offscreen --shot).
- Then push a branch (qa/refactor-%s), open a pull request
  ("delta gh pr create"), watch its checks ("delta gh pr checks --watch"),
  and merge it when green ("delta gh pr merge --rebase --delete-branch").
`, run.ID)
	case "delta":
		w(`Rules for delta (read AGENTS.md):
- go vet ./..., go test ./... and the command center's self-test
  (go run ./cmd/delta-cc test) must pass.
- The command center installs a new build only when the user has an agent
  run "delta update-cc"; you can't, and mustn't try. Pushing to master only
  publishes the build. Keep "delta help", AGENTS.md and the website's
  setup block in step, as AGENTS.md says.
- Push to master when everything passes, and watch the build
  ("delta gh run watch -R thooton/delta").
`)
	}
	var changed []string
	for _, f := range q.state.Findings {
		if f.TriagedIn == run.ID && f.Commit != "" && strings.HasPrefix(f.App, j.Repo+"/") {
			changed = append(changed, fmt.Sprintf("  %s %s: %s (%s)", f.Commit[:min(7, len(f.Commit))], appName(f.App), f.Title, f.Status))
		}
	}
	if len(changed) > 0 {
		w("\nTONIGHT'S FIXES, a good place to start looking:\n%s\n", strings.Join(changed, "\n"))
	}
	w(`
Finish with "delta qa done" and a short summary for the user: what got
simpler, and roughly how many lines went.
`)
}

func (q *qaService) briefHistory(w func(string, ...any), j *qaJob) {
	var lines []string
	var turnedDown []string
	for i := len(q.state.Findings) - 1; i >= 0 && len(lines) < 60; i-- {
		f := q.state.Findings[i]
		if f.App != j.App || f.Job == j.ID {
			continue
		}
		line := fmt.Sprintf("  [%s] %s: %s (%s)", f.ID, f.Status, f.Title, f.Kind)
		if f.Reason != "" {
			line += ". " + f.Reason
		}
		if f.Verdict != "" {
			line += map[string]string{"up": " 👍 the user likes it", "down": " 👎 the user turned it down"}[f.Verdict]
		}
		lines = append(lines, line)
		if f.Verdict == "down" {
			turnedDown = append(turnedDown, "  "+f.Title)
		}
	}
	if len(lines) == 0 {
		w("\nTHE APP'S FINDINGS\nNone yet: this is its first night.\n")
		return
	}
	w(`
THE APP'S FINDINGS, newest first
Don't file again what is open, passed or declined here, unless you have
important news about it (then file it new and name the old id). Something
fixed that is broken again is a regression: file it, and say so.
%s
`, strings.Join(lines, "\n"))
	if len(turnedDown) > 0 {
		w("\nFeatures the user turned down:\n%s\n", strings.Join(turnedDown, "\n"))
	}
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}
