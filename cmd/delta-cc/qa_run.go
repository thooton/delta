package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// The orchestrator: the schedule, and each run's way through its apps and
// then the refactors. It wakes every qaTick (and when poked), so everything
// here is a step from the saved state, which an update or restart resumes.

var (
	errQABusy       = errors.New("a QA run is already going; wait for it, or cancel it from the website's QA page")
	errQAUnknownApp = errors.New("no such app")
	errQANoRoom     = errors.New("no room on the Claude accounts")
)

// qaJobLife is how long a job of each role may run before it has failed.
var qaJobLife = map[string]time.Duration{
	"spec":     90 * time.Minute,
	"explore":  2 * time.Hour,
	"fix":      3 * time.Hour,
	"refactor": 5 * time.Hour,
}

const (
	qaIdleCheck   = 10 * time.Minute // how often a job's session is looked at
	qaDeviceWait  = 45 * time.Minute // for the emulators to come up
	qaWorkflow    = "qa-devices.yml" // in sonim
	qaEnvironment = "Delta QA "
	// Why a phone app was skipped when the emulators didn't connect in time.
	// Emulators that connect later, while the run is still on its apps,
	// take them back (qaDevices.hello).
	qaNoEmulators = "The emulators didn't come up."
)

// qaSetup is the QA sessions' environment's setup script: the delta CLI, as
// the website's sessions get it (SETUP_SCRIPT in web/app.js).
const qaSetup = "#!/bin/bash\nset -eo pipefail\ncurl -fsSL https://github.com/thooton/delta/releases/download/latest/install.sh | sh\n"

func (q *qaService) start() {
	go func() {
		t := time.NewTicker(qaTick)
		defer t.Stop()
		for {
			select {
			case <-t.C:
			case <-q.wake:
			}
			q.safely(q.schedule)
			q.safely(q.step)
		}
	}()
}

// safely runs f, logging a panic instead of taking the command center down.
func (q *qaService) safely(f func(ctx context.Context)) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("qa: panic: %v", p)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	f(ctx)
}

// schedule starts the night's run in its hour, once a night.
func (q *qaService) schedule(ctx context.Context) {
	q.mu.Lock()
	s := q.state.Settings
	loc, err := time.LoadLocation(s.TimeZone)
	if err != nil {
		loc = time.UTC
	}
	now := q.now().In(loc)
	night := now.Format("2006-01-02")
	due := s.Enabled && now.Hour() == s.Hour && q.state.Scheduled != night
	if due {
		q.state.Scheduled = night
		q.saveLocked()
	}
	q.mu.Unlock()
	if !due {
		return
	}
	if _, err := q.begin(ctx, "schedule", nil); err != nil {
		log.Printf("qa: the nightly run didn't start: %v", err)
	}
}

// begin starts a run of the apps named (every app when none are).
func (q *qaService) begin(ctx context.Context, trigger string, only []string) (qaRun, error) {
	q.mu.Lock()
	busy := q.runningLocked() != nil
	q.mu.Unlock()
	if busy {
		return qaRun{}, errQABusy
	}
	now := q.now().UTC()
	run := qaRun{
		ID: "qa_" + now.Format("20060102_1504") + "_" + randomHex(3), Trigger: trigger,
		All: len(only) == 0, Status: "running", Stage: "apps", Created: now,
	}
	apps, err := q.apps(ctx, true)
	if err == nil {
		err = q.pickApps(&run, apps, only)
	}
	q.mu.Lock()
	run.Server = q.state.Settings.Server
	if err == nil && run.Server == "" {
		err = errors.New("the command center doesn't know its own address yet: run `delta qa setup`, or start a run from the website")
	}
	if err == nil && q.runningLocked() != nil {
		err = errQABusy
	}
	if err != nil {
		if trigger == "schedule" && !errors.Is(err, errQABusy) {
			// The user sees on the website that the night's run didn't happen.
			run.Status, run.Stage, run.Error, run.Ended = "done", "done", err.Error(), now
			run.Apps = nil
			run.Summary = q.summaryLocked(&run)
			q.state.Runs = append(q.state.Runs, run)
			q.saveLocked()
		}
		q.mu.Unlock()
		return qaRun{}, err
	}
	var phone []string
	for _, a := range run.Apps {
		if a.Phone && a.Spec {
			phone = append(phone, a.Dir)
		}
	}
	if len(phone) > 0 {
		run.Devices = "dispatching"
		if q.state.DeviceHash == "" {
			run.Devices = "failed: the emulators have no device token; ask an agent to set QA up (`delta qa setup`)"
		}
	}
	q.state.Runs = append(q.state.Runs, run)
	q.saveLocked()
	q.mu.Unlock()

	if run.Devices == "dispatching" {
		runners := min(qaMaxRunners, (len(phone)+qaDevicesPerJob-1)/qaDevicesPerJob)
		err := q.dispatch(ctx, run, phone, runners)
		q.mu.Lock()
		if r := q.runLocked(run.ID); r != nil {
			if err != nil {
				r.Devices = "failed: " + err.Error()
			} else {
				r.Devices, r.Dispatch, r.Runners = "dispatched", q.now().UTC(), runners
			}
			run = *r
			q.saveLocked()
		}
		q.mu.Unlock()
	}
	q.poke()
	return run, nil
}

// pickApps puts the apps the run covers into it: the ones named, or all,
// least recently explored first, so apps skipped for want of room one night
// go first the next.
func (q *qaService) pickApps(run *qaRun, apps []qaApp, only []string) error {
	byID := map[string]qaApp{}
	for _, a := range apps {
		byID[a.ID] = a
	}
	var chosen []qaApp
	if len(only) == 0 {
		chosen = apps
	}
	for _, id := range only {
		a, ok := byID[id]
		if !ok {
			return fmt.Errorf("%w: %s", errQAUnknownApp, id)
		}
		if !slices.ContainsFunc(chosen, func(c qaApp) bool { return c.ID == id }) {
			chosen = append(chosen, a)
		}
	}
	if len(chosen) == 0 {
		return errors.New("there are no apps to check")
	}
	q.mu.Lock()
	explored := q.state.Explored
	sort.SliceStable(chosen, func(i, j int) bool {
		return explored[chosen[i].ID].Before(explored[chosen[j].ID])
	})
	q.mu.Unlock()
	for _, a := range chosen {
		run.Apps = append(run.Apps, qaRunApp{App: a.ID, Repo: a.Repo, Dir: a.Dir, Phone: a.Phone, Spec: a.Spec, Status: "waiting"})
	}
	return nil
}

// qaLaunch is a job step decided to start, with its key, which only the
// session's prompt carries.
type qaLaunch struct{ run, job, key string }

// step moves the running run along.
func (q *qaService) step(ctx context.Context) {
	q.mu.Lock()
	run := q.runningLocked()
	if run == nil {
		q.mu.Unlock()
		return
	}
	now := q.now().UTC()
	var launches []qaLaunch
	var checks []qaJob
	running := 0
	for i := range run.Jobs {
		j := &run.Jobs[i]
		if j.Status != "running" {
			continue
		}
		switch {
		case now.Sub(j.Created) > qaJobLife[j.Role]:
			q.endJobLocked(run, j, "failed", "It timed out.")
			continue
		case j.Session != "" && now.Sub(j.Created) >= 15*time.Minute && now.Sub(j.Checked) >= qaIdleCheck:
			j.Checked = now
			checks = append(checks, *j)
		}
		running++
	}
	launch := func(app, repo, role string) {
		key := qaKeyStart + randomHex(32)
		j := qaJob{ID: "job_" + randomHex(6), App: app, Repo: repo, Role: role, Status: "running", Created: now, KeyHash: keyHash(key)}
		run.Jobs = append(run.Jobs, j)
		launches = append(launches, qaLaunch{run.ID, j.ID, key})
		running++
	}

	if run.Stage == "apps" {
		for i := range run.Apps {
			ra := &run.Apps[i]
			switch ra.Status {
			case "waiting":
				role := "explore"
				if !ra.Spec {
					role = "spec"
				}
				switch {
				case run.NoRoom:
					q.settleAppLocked(ra, "skipped", "There was no room left on the Claude accounts.")
					continue
				case running >= qaMaxSessions:
					continue
				case role == "explore" && ra.Phone:
					if why, failed := strings.CutPrefix(run.Devices, "failed: "); failed {
						q.settleAppLocked(ra, "skipped", "The emulators didn't start: "+why)
						continue
					}
					if !q.devices.lease(run.ID, ra.App) {
						// Dispatch is set once GitHub has taken the dispatch; until
						// then the emulators haven't even been asked for.
						if !run.Dispatch.IsZero() && now.Sub(run.Dispatch) > qaDeviceWait && !q.devices.any(run.ID) {
							q.settleAppLocked(ra, "skipped", qaNoEmulators)
						}
						continue
					}
				}
				ra.Status = "running"
				launch(ra.App, ra.Repo, role)
			case "running":
				last := q.lastJobLocked(run, ra.App)
				if last == nil {
					q.settleAppLocked(ra, "done", "")
					continue
				}
				if last.Status == "running" {
					continue
				}
				if last.Role == "explore" && last.Status == "done" && len(q.fixItemsLocked(ra.App, "")) > 0 {
					switch {
					case run.NoRoom:
						q.settleAppLocked(ra, "done", "There was no room left for the fixes; they wait for the next run.")
					case running < qaMaxSessions:
						launch(ra.App, ra.Repo, "fix")
					}
					continue
				}
				why := ""
				if last.Status != "done" {
					why = last.Summary
				}
				q.settleAppLocked(ra, "done", why)
			}
		}
		if !slices.ContainsFunc(run.Apps, func(a qaRunApp) bool { return a.Status == "waiting" || a.Status == "running" }) {
			run.Stage = "refactor"
			if run.All && !run.NoRoom {
				run.Repos = slices.Clone(qaRepos)
			}
		}
	}
	if run.Stage == "refactor" {
		if running == 0 {
			if len(run.Repos) > 0 && !run.NoRoom {
				repo := run.Repos[0]
				run.Repos = run.Repos[1:]
				launch("", repo, "refactor")
			} else {
				q.finishLocked(run, "done")
			}
		}
	}
	q.saveLocked()
	q.mu.Unlock()

	for _, l := range launches {
		q.startJob(ctx, l)
	}
	for _, j := range checks {
		idle, err := q.idle(ctx, j)
		if err != nil {
			continue
		}
		q.mu.Lock()
		if r := q.runLocked(runOf(q, j.ID)); r != nil {
			if job := jobIn(r, j.ID); job != nil && job.Status == "running" {
				switch {
				case !idle:
					job.IdleSince = time.Time{}
				case job.IdleSince.IsZero():
					job.IdleSince = q.now().UTC()
				case q.now().Sub(job.IdleSince) >= qaIdleCheck:
					q.endJobLocked(r, job, "done", "The session stopped without saying it was done.")
				}
				q.saveLocked()
			}
		}
		q.mu.Unlock()
	}
	if len(launches) > 0 {
		q.poke()
	}
}

// runOf is the id of the run job id is in; q.mu is held.
func runOf(q *qaService, job string) string {
	for _, r := range q.state.Runs {
		if jobIn(&r, job) != nil {
			return r.ID
		}
	}
	return ""
}

func jobIn(run *qaRun, id string) *qaJob {
	for i := range run.Jobs {
		if run.Jobs[i].ID == id {
			return &run.Jobs[i]
		}
	}
	return nil
}

// lastJobLocked is the app's newest job in the run.
func (q *qaService) lastJobLocked(run *qaRun, app string) *qaJob {
	for i := len(run.Jobs) - 1; i >= 0; i-- {
		if run.Jobs[i].App == app {
			return &run.Jobs[i]
		}
	}
	return nil
}

// fixItemsLocked are what a fix job for app works through: its open
// findings, and its grown features the user turned down. Findings filed by
// job skip (a fix job's own follow-ups) are left for the next run.
func (q *qaService) fixItemsLocked(app, skip string) []qaFinding {
	var out []qaFinding
	for _, f := range q.state.Findings {
		if f.App != app || skip != "" && f.Job == skip {
			continue
		}
		if f.Status == "open" || f.Status == "grown" && f.Verdict == "down" {
			out = append(out, f)
		}
	}
	return out
}

func (q *qaService) settleAppLocked(ra *qaRunApp, status, why string) {
	ra.Status, ra.Why = status, why
	q.devices.release(ra.App)
}

// endJobLocked ends a job: its key stops working, and its session leaves
// the user's chats.
func (q *qaService) endJobLocked(run *qaRun, j *qaJob, status, summary string) {
	j.Status, j.Ended, j.KeyHash = status, q.now().UTC(), ""
	if summary != "" {
		j.Summary = summary
	}
	if j.Role == "explore" && status == "done" {
		q.state.Explored[j.App] = j.Ended
	}
	if j.Session != "" && j.Account != "" {
		account, session := j.Account, j.Session
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if q.cloud != nil {
				if err := q.cloud.archiveSession(ctx, account, session); err != nil {
					log.Printf("qa: archiving %s: %v", session, err)
				}
			}
		}()
	}
}

// finishLocked ends the run.
func (q *qaService) finishLocked(run *qaRun, status string) {
	for i := range run.Jobs {
		if j := &run.Jobs[i]; j.Status == "running" {
			q.endJobLocked(run, j, "cancelled", "The run was cancelled.")
		}
	}
	for i := range run.Apps {
		if ra := &run.Apps[i]; ra.Status == "waiting" || ra.Status == "running" {
			q.settleAppLocked(ra, "skipped", "The run was cancelled.")
		}
	}
	run.Repos = nil
	run.Status, run.Stage, run.Ended = status, "done", q.now().UTC()
	run.Summary = q.summaryLocked(run)
}

// startJob starts a job's cloud session.
func (q *qaService) startJob(ctx context.Context, l qaLaunch) {
	q.mu.Lock()
	run := q.runLocked(l.run)
	var job *qaJob
	if run != nil {
		job = jobIn(run, l.job)
	}
	if job == nil {
		q.mu.Unlock()
		return
	}
	r, j := *run, *job
	q.mu.Unlock()

	account, session, err := q.launch(ctx, &r, &j, qaPrompt(l.key, j))

	q.mu.Lock()
	defer q.mu.Unlock()
	run = q.runLocked(l.run)
	if run == nil {
		return
	}
	job = jobIn(run, l.job)
	if job == nil {
		return
	}
	job.Account, job.Session = account, session
	switch {
	case job.Status != "running":
		// Cancelled while it started.
		q.endJobLocked(run, job, job.Status, "")
	case err != nil:
		log.Printf("qa: %s %s: %v", job.Role, firstNonEmptyString(job.App, job.Repo), err)
		q.endJobLocked(run, job, "failed", "Couldn't start it: "+err.Error())
		if errors.Is(err, errQANoRoom) {
			run.NoRoom = true
			for i := range run.Apps {
				if ra := &run.Apps[i]; ra.App == job.App && job.App != "" {
					q.settleAppLocked(ra, "skipped", "There was no room left on the Claude accounts.")
				}
			}
		}
	}
	q.saveLocked()
}

func firstNonEmptyString(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// qaTitle is a job's session's title.
func qaTitle(j qaJob) string {
	what := map[string]string{"spec": "Spec", "explore": "Explore", "fix": "Fix", "refactor": "Refactor"}[j.Role]
	if j.App == "" {
		return "QA: " + what + " " + j.Repo
	}
	return "QA: " + what + " " + appName(j.App)
}

// appName is how an app is named to the user: "sonim/list" is List.
func appName(id string) string {
	_, dir, _ := strings.Cut(id, "/")
	if dir == "" {
		return id
	}
	return strings.ToUpper(dir[:1]) + dir[1:]
}

// qaPrompt is a job's session's first message. Its instructions come from
// `delta qa start`, so they can't fall out of step with the command center.
func qaPrompt(key string, j qaJob) string {
	what := map[string]string{
		"spec":     "writing the spec for " + appName(j.App) + " (" + j.App + ")",
		"explore":  "exploring " + appName(j.App) + " (" + j.App + ") in an emulator and reporting what is wrong with it",
		"fix":      "triaging and fixing what QA found in " + appName(j.App) + " (" + j.App + ")",
		"refactor": "a sweeping refactor of thooton/" + j.Repo,
	}[j.Role]
	return `<delta-qa>
This session is one job in the user's nightly QA of their phone and tablet apps: ` + what + `.

First run ` + "`delta qa start " + key + "`" + `. It signs this session in to the Delta command center and prints your instructions. Read all of them before doing anything else, and follow them. Nobody is watching or will answer questions: decide for yourself, and finish with ` + "`delta qa done`" + ` as they say. The key is a password: never repeat or commit it.
</delta-qa>

` + qaTitle(j)
}

// cloudLaunch starts a job's session on an account with room.
func (q *qaService) cloudLaunch(ctx context.Context, run *qaRun, j *qaJob, prompt string) (account, session string, err error) {
	account, err = q.cloud.pick(ctx)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", errQANoRoom, err)
	}
	// The delta CLI, and the whole network, as the website's sessions have
	// (they build and test the apps, which reach for package registries of
	// every kind).
	session, err = q.cloud.launch(ctx, account, cloudEnv{
		prefix:      qaEnvironment,
		description: "Delta QA - the nightly QA of the phone and tablet apps",
		setup:       qaSetup,
		server:      run.Server,
		config: map[string]any{
			"environment":    map[string]string{"DELTA_SERVER": run.Server},
			"network_config": map[string]any{"allowed_hosts": []string{"*"}, "allow_default_hosts": true},
		},
	}, qaTitle(*j), prompt)
	return account, session, err
}

func (q *qaService) cloudIdle(ctx context.Context, j qaJob) (bool, error) {
	return q.cloud.sessionIdle(ctx, j.Account, j.Session)
}

// githubDispatch starts sonim's emulator workflow for the run: runners
// jobs, each booting qaDevicesPerJob emulators with the apps on them.
func (q *qaService) githubDispatch(ctx context.Context, run qaRun, apps []string, runners int) error {
	matrix := make([]string, runners)
	for i := range matrix {
		matrix[i] = fmt.Sprint(i + 1)
	}
	return q.github(ctx, http.MethodPost, "/repos/"+qaOwner+"/sonim/actions/workflows/"+qaWorkflow+"/dispatches", map[string]any{
		"ref": "master",
		"inputs": map[string]string{
			"run":     run.ID,
			"runners": "[" + strings.Join(matrix, ",") + "]",
			"apps":    strings.Join(apps, " "),
		},
	}, nil)
}

// ---------------------------------------------------------------------------
// The nightly report
// ---------------------------------------------------------------------------

func (q *qaService) summaryLocked(run *qaRun) string {
	loc, err := time.LoadLocation(q.state.Settings.TimeZone)
	if err != nil {
		loc = time.UTC
	}
	day := run.Created.In(loc).Format("Mon Jan 2")
	if run.Error != "" {
		return day + ": the run didn't start. " + run.Error
	}
	var lines []string
	checked, specs := 0, 0
	skipped := map[string][]string{}
	var whys []string
	for _, a := range run.Apps {
		switch {
		case a.Status == "skipped":
			if skipped[a.Why] == nil {
				whys = append(whys, a.Why)
			}
			skipped[a.Why] = append(skipped[a.Why], appName(a.App))
		case !a.Spec:
			specs++
		default:
			checked++
		}
	}
	head := fmt.Sprintf("%s: checked %d %s", day, checked, plural(checked, "app", "apps"))
	if specs > 0 {
		head += fmt.Sprintf(", wrote %d %s to skim", specs, plural(specs, "spec", "specs"))
	}
	if run.Status == "cancelled" {
		head += " (cancelled)"
	}
	lines = append(lines, head+".")
	for _, why := range whys {
		lines = append(lines, "Skipped "+strings.Join(skipped[why], ", ")+": "+why)
	}
	counts := map[string]int{}
	found := 0
	changed := map[string]bool{}
	var changedApps []string
	for _, f := range q.state.Findings {
		if f.Run == run.ID {
			found++
		}
		if f.TriagedIn != run.ID {
			continue
		}
		counts[f.Status]++
		if (f.Status == "fixed" || f.Status == "grown" || f.Status == "reverted") && !changed[f.App] {
			changed[f.App] = true
			changedApps = append(changedApps, appName(f.App))
		}
	}
	tally := fmt.Sprintf("Found %d. Fixed %d", found, counts["fixed"])
	if counts["grown"] > 0 {
		tally += fmt.Sprintf(", grew %d %s", counts["grown"], plural(counts["grown"], "feature", "features"))
	}
	if counts["reverted"] > 0 {
		tally += fmt.Sprintf(", took out %d you turned down", counts["reverted"])
	}
	tally += fmt.Sprintf(", left %d for you, declined %d.", counts["passed"], counts["declined"])
	lines = append(lines, tally)
	if len(changedApps) > 0 {
		sort.Strings(changedApps)
		lines = append(lines, "Updates waiting: "+strings.Join(changedApps, ", ")+".")
	}
	for _, j := range run.Jobs {
		if j.Role != "refactor" {
			continue
		}
		s := j.Summary
		if j.Status != "done" {
			s = j.Status + ": " + s
		}
		lines = append(lines, "Refactor of "+j.Repo+": "+s)
	}
	return strings.Join(lines, "\n")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
