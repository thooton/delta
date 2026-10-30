package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// A QA job's routes, for its key (scopeQA). The key also reaches the GitHub
// relay, for cloning and pushing, and /auth and /version, and nothing else:
// not /update, so no job can install a command center.
//
//	GET  /qa/job               The job and its instructions (`delta qa start` prints them).
//	POST /qa/job/find          {"kind", "title", "detail", "shots"?: [base64 PNG]}: file a finding.
//	POST /qa/job/triage        {"id", "status", "reason", "commit"?}: what a fix job made of a finding.
//	GET  /qa/job/findings      The app's findings, newest first.
//	GET  /qa/job/shots/{name}  A finding's screenshot.
//	POST /qa/job/device        {"op", "args"}: run a command on the app's emulator (qa_device.go).
//	POST /qa/job/done          {"summary", "force"?}: the job is over; the key stops working.
const (
	maxQAFindingsPerJob = 40
	maxQAFeaturesPerJob = 2
	maxQATitle          = 120
	maxQADetail         = 8000
	maxQAReason         = 400
	maxQASummary        = 2000
)

type qaJobKey struct{}

// jobKey finds the running job whose key this is: its run and job ids.
func (q *qaService) jobKey(key string) (string, string, bool) {
	if q == nil || !strings.HasPrefix(key, qaKeyStart) {
		return "", "", false
	}
	h := keyHash(key)
	q.mu.Lock()
	defer q.mu.Unlock()
	run := q.runningLocked()
	if run == nil {
		return "", "", false
	}
	for _, j := range run.Jobs {
		if j.KeyHash == h && j.Status == "running" {
			return run.ID, j.ID, true
		}
	}
	return "", "", false
}

// deviceKey says whether this is the emulators' device token.
func (q *qaService) deviceKey(token string) bool {
	if q == nil || !strings.HasPrefix(token, qaDeviceStart) {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.state.DeviceHash != "" && keyHash(token) == q.state.DeviceHash
}

// qaJobAllowed says whether a job's key may make request r.
func qaJobAllowed(r *http.Request) bool {
	p := r.URL.Path
	return p == "/qa/job" || strings.HasPrefix(p, "/qa/job/") || strings.HasPrefix(p, "/github/") || p == "/auth" || p == "/version"
}

func (q *qaService) registerJob(mux *http.ServeMux) {
	mux.HandleFunc("GET /qa/job", q.forJob(q.jobInfo))
	mux.HandleFunc("POST /qa/job/find", q.forJob(q.find))
	mux.HandleFunc("POST /qa/job/triage", q.forJob(q.triage))
	mux.HandleFunc("GET /qa/job/findings", q.forJob(q.jobFindings))
	mux.HandleFunc("GET /qa/job/shots/{name}", q.forJob(func(w http.ResponseWriter, r *http.Request, _ *qaRun, _ *qaJob) {
		q.serveShot(w, r)
	}))
	mux.HandleFunc("POST /qa/job/device", q.jobDevice)
	mux.HandleFunc("POST /qa/job/done", q.forJob(q.done))
}

// forJob runs h with the job the request's key is for, with q.mu held.
func (q *qaService) forJob(h func(w http.ResponseWriter, r *http.Request, run *qaRun, j *qaJob)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ids, ok := jobIDs(w, r)
		if !ok {
			return
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		if run, j := q.liveJobLocked(w, ids); j != nil {
			h(w, r, run, j)
		}
	}
}

// jobIDs returns the run and job ids of the request's key, and refuses
// requests made without a QA job's key.
func jobIDs(w http.ResponseWriter, r *http.Request) ([2]string, bool) {
	ids, _ := r.Context().Value(qaJobKey{}).([2]string)
	if scopeOf(r) != scopeQA || ids[1] == "" {
		writeError(w, http.StatusForbidden, "these routes are for a QA job's key")
		return ids, false
	}
	return ids, true
}

// liveJobLocked returns the running job with ids, or answers that it is
// over; q.mu is held.
func (q *qaService) liveJobLocked(w http.ResponseWriter, ids [2]string) (*qaRun, *qaJob) {
	run, j := q.jobLocked(ids[0], ids[1])
	if j == nil || j.Status != "running" || run.Status != "running" {
		writeError(w, http.StatusUnauthorized, "this QA job is over, so its key no longer works")
		return nil, nil
	}
	return run, j
}

// runAppLocked is the run's entry for the job's app.
func runAppLocked(run *qaRun, app string) *qaRunApp {
	return findPtr(run.Apps, func(ra *qaRunApp) bool { return ra.App == app })
}

func (q *qaService) jobInfo(w http.ResponseWriter, r *http.Request, run *qaRun, j *qaJob) {
	ra := runAppLocked(run, j.App)
	var app qaRunApp
	if ra != nil {
		app = *ra
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job":          map[string]any{"id": j.ID, "role": j.Role, "app": j.App, "repo": j.Repo, "dir": app.Dir, "run": run.ID},
		"instructions": q.briefLocked(run, j, app),
	})
}

func (q *qaService) find(w http.ResponseWriter, r *http.Request, run *qaRun, j *qaJob) {
	if j.App == "" {
		writeError(w, http.StatusForbidden, "a refactor files no findings")
		return
	}
	var req struct {
		Kind, Title, Detail string
		Shots               []string
	}
	if err := decodeBody(r, 16<<20, &req); err != nil {
		writeError(w, http.StatusBadRequest, `expected {"kind", "title", "detail", "shots"?: [base64 PNG]}`)
		return
	}
	req.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
	req.Title, req.Detail = oneLine(req.Title), strings.TrimSpace(req.Detail)
	mine, features := 0, 0
	for _, f := range q.state.Findings {
		if f.Job == j.ID {
			mine++
			if f.Kind == "feature" {
				features++
			}
		}
	}
	switch {
	case !slices.Contains(qaKinds, req.Kind):
		writeError(w, http.StatusBadRequest, "kind is one of "+strings.Join(qaKinds, ", "))
		return
	case req.Title == "" || len([]rune(req.Title)) > maxQATitle:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("give it a title of at most %d characters", maxQATitle))
		return
	case req.Detail == "" || len([]rune(req.Detail)) > maxQADetail:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("give it details (steps, what you expected and why, what happened) of at most %d characters", maxQADetail))
		return
	case mine >= maxQAFindingsPerJob:
		writeError(w, http.StatusConflict, fmt.Sprintf("that's %d findings already; file only the ones that matter most", maxQAFindingsPerJob))
		return
	case req.Kind == "feature" && !q.state.Settings.Apps[j.App]:
		writeError(w, http.StatusForbidden, "the user hasn't let this app grow, so don't propose features")
		return
	case req.Kind == "feature" && features >= maxQAFeaturesPerJob:
		writeError(w, http.StatusConflict, fmt.Sprintf("propose at most %d features a night", maxQAFeaturesPerJob))
		return
	}
	shots, err := decodeShots(req.Shots)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := q.now().UTC()
	f := qaFinding{ID: "f_" + randomHex(5), App: j.App, Run: run.ID, Job: j.ID, Kind: req.Kind, Title: req.Title, Detail: req.Detail, Status: "open", Found: now, Updated: now}
	for i, data := range shots {
		name := fmt.Sprintf("%s-%d.png", f.ID, i+1)
		if err := os.WriteFile(filepath.Join(q.dir, qaShotsDir, name), data, 0o600); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		f.Shots = append(f.Shots, name)
	}
	q.state.Findings = append(q.state.Findings, f)
	q.saveLocked()
	writeJSON(w, http.StatusCreated, f)
}

var qaCommit = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func (q *qaService) triage(w http.ResponseWriter, r *http.Request, run *qaRun, j *qaJob) {
	if j.Role != "fix" {
		writeError(w, http.StatusForbidden, "only a fix job triages")
		return
	}
	var req struct {
		ID, Status, Reason, Commit string
	}
	if err := decodeBody(r, 64<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, `expected {"id", "status", "reason", "commit"?}`)
		return
	}
	req.Reason, req.Commit = oneLine(req.Reason), strings.ToLower(strings.TrimSpace(req.Commit))
	f := q.findingLocked(req.ID)
	if f == nil || f.App != j.App {
		writeError(w, http.StatusNotFound, "no such finding for this app")
		return
	}
	grownTonight := 0
	for _, g := range q.state.Findings {
		if g.App == j.App && g.Status == "grown" && g.TriagedIn == run.ID && g.ID != f.ID {
			grownTonight++
		}
	}
	turnedDown := f.Status == "grown" && f.Verdict == "down"
	switch {
	case !slices.Contains([]string{"fixed", "passed", "declined", "grown", "reverted"}, req.Status):
		writeError(w, http.StatusBadRequest, "status is one of fixed, passed, declined, grown, reverted")
		return
	case req.Reason == "" || len([]rune(req.Reason)) > maxQAReason:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("give a reason, one line for the user, of at most %d characters", maxQAReason))
		return
	case req.Commit != "" && !qaCommit.MatchString(req.Commit):
		writeError(w, http.StatusBadRequest, "commit is the commit's hash")
		return
	case turnedDown && req.Status != "reverted":
		writeError(w, http.StatusConflict, "the user turned this feature down: take it out (spec and code) and mark it reverted")
		return
	case req.Status == "reverted" && !turnedDown:
		writeError(w, http.StatusConflict, "only a grown feature the user turned down is reverted")
		return
	case req.Status == "grown" && f.Kind != "feature":
		writeError(w, http.StatusBadRequest, "only a feature is grown; a fix is fixed")
		return
	case req.Status == "grown" && !q.state.Settings.Apps[j.App]:
		writeError(w, http.StatusForbidden, "the user hasn't let this app grow; decline the feature")
		return
	case req.Status == "grown" && grownTonight > 0:
		writeError(w, http.StatusConflict, "an app grows at most one feature a night; decline or pass on the rest")
		return
	case (req.Status == "fixed" || req.Status == "grown" || req.Status == "reverted") && req.Commit == "":
		writeError(w, http.StatusBadRequest, "give the commit on master that did it")
		return
	}
	f.Status, f.Reason, f.Commit, f.TriagedIn, f.Updated = req.Status, req.Reason, req.Commit, run.ID, q.now().UTC()
	if req.Status == "reverted" {
		f.Verdict = "down"
	}
	q.saveLocked()
	writeJSON(w, http.StatusOK, f)
}

func (q *qaService) jobFindings(w http.ResponseWriter, r *http.Request, run *qaRun, j *qaJob) {
	out := []qaFinding{}
	for i := len(q.state.Findings) - 1; i >= 0; i-- {
		if f := q.state.Findings[i]; j.App == "" || f.App == j.App {
			out = append(out, f)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (q *qaService) done(w http.ResponseWriter, r *http.Request, run *qaRun, j *qaJob) {
	var req struct {
		Summary string
		Force   bool
	}
	if err := decodeBody(r, 64<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, `expected {"summary", "force"?}`)
		return
	}
	req.Summary = strings.TrimSpace(req.Summary)
	if req.Summary == "" || len([]rune(req.Summary)) > maxQASummary {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("give a summary of what you did, at most %d characters", maxQASummary))
		return
	}
	if j.Role == "fix" && !req.Force {
		if left := q.fixItemsLocked(j.App, j.ID); len(left) > 0 {
			var ids []string
			for _, f := range left {
				ids = append(ids, f.ID)
			}
			writeError(w, http.StatusConflict, "triage these first (`delta qa triage`): "+strings.Join(ids, ", ")+"; or, if you truly can't, finish with --force")
			return
		}
	}
	q.endJobLocked(j, "done", req.Summary)
	q.saveLocked()
	q.poke()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (q *qaService) jobDevice(w http.ResponseWriter, r *http.Request) {
	ids, ok := jobIDs(w, r)
	if !ok {
		return
	}
	var req struct {
		Op   string
		Args []string
	}
	if err := decodeBody(r, 1<<20, &req); err != nil {
		writeError(w, http.StatusBadRequest, `expected {"op", "args"}`)
		return
	}
	q.mu.Lock()
	run, j := q.liveJobLocked(w, ids)
	if j == nil {
		q.mu.Unlock()
		return
	}
	ra := runAppLocked(run, j.App)
	if ra == nil || !ra.Phone || j.Role == "spec" {
		q.mu.Unlock()
		writeError(w, http.StatusForbidden, "only phone apps' explore and fix jobs have an emulator")
		return
	}
	runID, app, dir := run.ID, ra.App, ra.Dir
	q.mu.Unlock()
	res, status, err := q.devices.do(r, runID, app, dir, req.Op, req.Args)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
