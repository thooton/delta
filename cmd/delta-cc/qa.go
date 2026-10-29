package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the schedule's time zone, whatever the VPS has installed
)

// Nightly QA: agents that go through every phone and tablet app, find what
// is wrong with it, and fix it, without anyone watching.
//
// A run (on the schedule, or started from the website, for every app
// or some) takes each app through a pipeline of cloud sessions, each a job
// with a key of its own (scopeQA):
//
//  1. spec, for an app with no SPEC.md yet: writes one, from the code, its
//     AGENTS.md and its history, for the user to skim. That is all the app
//     gets that night; QA tests against the spec from the next run on.
//  2. explore: drives the app in an emulator (a phone app on one of the
//     emulators a GitHub Actions job runs for the run, through the device
//     relay in qa_device.go; a tablet app in the session itself, with
//     tablet's tools/emulator.py) and files findings: bugs, problems,
//     inconsistencies, and, for an app the user lets grow, features.
//  3. fix: for the app's open findings (and grown features the user turned
//     down), decides what to fix, fixes it, checks the fix on the emulator,
//     and pushes to master; the rest it passes on to the user or declines.
//
// When every app is through (on a run of every app), one refactor job per
// repository, one after another, sweeps the code for complexity. Then the
// run writes the nightly report the website shows.
//
// Nothing is gated: fixes go to master and the Updaters offer them, and the
// user installs them when they choose. The ledger (findings) remembers what
// was found and what became of it, so a later run doesn't report the same
// thing again, and holds the user's 👍/👎 on grown features.
//
// Routes, for the delta token (the website):
//
//	GET    /qa                     The overview: settings, apps, the running run, recent runs.
//	PUT    /qa/settings            {"enabled"?, "hour"?, "time_zone"?, "server"?}: the nightly schedule,
//	                               and the command center's address as sessions reach it.
//	PUT    /qa/apps/{repo}/{name}  {"grow": bool}: whether the app may grow features.
//	POST   /qa/runs                {"apps"?: ["sonim/list", ...], "server"?}: start a run (every app by default).
//	GET    /qa/runs/{id}           A run with its jobs.
//	DELETE /qa/runs/{id}           Cancel a running run; its jobs' keys stop working.
//	GET    /qa/findings            The ledger, newest first. Query: app, status, run, limit.
//	PUT    /qa/findings/{id}/verdict {"verdict": "up"|"down"|""}: the user on a grown feature.
//	GET    /qa/shots/{name}        A finding's screenshot.
//	POST   /qa/device-token        Make the device token (replacing any), for the emulator jobs.
//
// For a job's key, the /qa/job routes (qa_jobs.go); for the device token,
// the /qa/device routes (qa_device.go).
const (
	qaDir           = "qa"
	qaStateFile     = "state.json"
	qaShotsDir      = "shots"
	qaKeyStart      = "delta_qa_"
	qaDeviceStart   = "delta_qadevice_"
	maxQARuns       = 60
	maxQAFindings   = 3000
	maxQAShots      = 4       // a finding's screenshots
	maxQAShotBytes  = 2 << 20 // each
	qaDefaultHour   = 13
	qaDefaultZone   = "America/Phoenix"
	qaMaxSessions   = 8 // sessions a run has going at once
	qaTick          = 20 * time.Second
	qaDevicesPerJob = 3 // emulators an Actions job runs
	qaMaxRunners    = 3
)

// The repositories QA covers, and refactors in this order.
var qaRepos = []string{"sonim", "tablet", "delta"}

// qaOwner owns them.
const qaOwner = "thooton"

type qaSettings struct {
	Enabled  bool            `json:"enabled"`
	Hour     int             `json:"hour"`      // local hour the nightly run starts
	TimeZone string          `json:"time_zone"` // IANA name
	Apps     map[string]bool `json:"grow"`      // app id -> may grow
	// The command center's address as sessions reach it, as the website or the
	// CLI last sent it (setServer).
	Server string `json:"server,omitempty"`
}

type qaFinding struct {
	ID      string    `json:"id"`
	App     string    `json:"app"`
	Run     string    `json:"run"`
	Job     string    `json:"job"`  // that filed it
	Kind    string    `json:"kind"` // bug, problem, inconsistency, feature
	Title   string    `json:"title"`
	Detail  string    `json:"detail"`
	Shots   []string  `json:"shots,omitempty"`
	Status  string    `json:"status"` // open, fixed, passed, declined, grown, reverted
	Reason  string    `json:"reason,omitempty"`
	Commit  string    `json:"commit,omitempty"`
	Verdict string    `json:"verdict,omitempty"` // the user's on a grown feature: up or down
	Found   time.Time `json:"found"`
	Updated time.Time `json:"updated"`
	// The run that last triaged it.
	TriagedIn string `json:"triaged_in,omitempty"`
}

var qaKinds = []string{"bug", "problem", "inconsistency", "feature"}

type qaJob struct {
	ID      string    `json:"id"`
	App     string    `json:"app,omitempty"` // "" for a refactor
	Repo    string    `json:"repo"`
	Role    string    `json:"role"`   // spec, explore, fix, refactor
	Status  string    `json:"status"` // running, done, failed, cancelled
	Account string    `json:"account,omitempty"`
	Session string    `json:"session,omitempty"`
	Created time.Time `json:"created"`
	Ended   time.Time `json:"ended,omitzero"`
	Summary string    `json:"summary,omitempty"` // the agent's, or why it failed
	KeyHash string    `json:"key_hash,omitempty"`
	// When the session was last seen idle without the job being done.
	IdleSince time.Time `json:"idle_since,omitzero"`
	Checked   time.Time `json:"checked,omitzero"`
}

type qaRunApp struct {
	App    string `json:"app"`
	Repo   string `json:"repo"`
	Dir    string `json:"dir"`
	Phone  bool   `json:"phone"`
	Spec   bool   `json:"spec"`   // had a SPEC.md when the run began
	Status string `json:"status"` // waiting, running, done, skipped
	Why    string `json:"why,omitempty"`
}

type qaRun struct {
	ID       string     `json:"id"`
	Trigger  string     `json:"trigger"` // schedule or manual
	All      bool       `json:"all"`     // every app, so the refactors follow
	Status   string     `json:"status"`  // running, done, cancelled
	Stage    string     `json:"stage"`   // apps, refactor, done
	Created  time.Time  `json:"created"`
	Ended    time.Time  `json:"ended,omitzero"`
	Apps     []qaRunApp `json:"apps"`
	Jobs     []qaJob    `json:"jobs"`
	Repos    []string   `json:"repos,omitempty"` // still to refactor
	Summary  string     `json:"summary,omitempty"`
	Server   string     `json:"server"`
	Devices  string     `json:"devices,omitempty"` // the emulators: "", dispatched, failed: ...
	Runners  int        `json:"runners,omitempty"`
	NoRoom   bool       `json:"no_room,omitempty"`
	Error    string     `json:"error,omitempty"`
	Dispatch time.Time  `json:"dispatched,omitzero"`
}

type qaState struct {
	Settings   qaSettings           `json:"settings"`
	Runs       []qaRun              `json:"runs"`
	Findings   []qaFinding          `json:"findings"`
	DeviceHash string               `json:"device_hash,omitempty"`
	Scheduled  string               `json:"scheduled,omitempty"` // the last night a run was scheduled, 2006-01-02
	Explored   map[string]time.Time `json:"explored,omitempty"`  // app -> last explored, so skipped apps go first
}

// qaApp is an app in one of the repositories, as its tree on GitHub shows it.
type qaApp struct {
	ID    string `json:"id"` // repo/dir
	Repo  string `json:"repo"`
	Dir   string `json:"dir"`
	Phone bool   `json:"phone"`
	Spec  bool   `json:"spec"` // has a SPEC.md
}

type qaService struct {
	dir       string
	cloud     *cloudAccountStore
	tokenFile string // the GitHub token's

	mu    sync.Mutex
	state qaState
	wake  chan struct{}

	devices *qaDevices

	// What the orchestrator calls out to; the self-test replaces them.
	listApps  func(ctx context.Context) ([]qaApp, error)
	launch    func(ctx context.Context, run *qaRun, job *qaJob, prompt string) (account, session string, err error)
	idle      func(ctx context.Context, job qaJob) (bool, error)
	dispatch  func(ctx context.Context, run qaRun, apps []string, runners int) error
	now       func() time.Time
	appsCache struct {
		at   time.Time
		apps []qaApp
	}
}

func newQAService(dataDir string, cloud *cloudAccountStore) *qaService {
	q := &qaService{
		dir:       filepath.Join(dataDir, qaDir),
		cloud:     cloud,
		tokenFile: filepath.Join(dataDir, accessTokenFile),
		wake:      make(chan struct{}, 1),
		now:       time.Now,
	}
	q.devices = newQADevices(q)
	q.listApps = q.githubApps
	q.launch = q.cloudLaunch
	q.idle = q.cloudIdle
	q.dispatch = q.githubDispatch
	if err := os.MkdirAll(filepath.Join(q.dir, qaShotsDir), 0o700); err != nil {
		log.Printf("qa: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(q.dir, qaStateFile))
	if err == nil {
		if err := json.Unmarshal(data, &q.state); err != nil {
			log.Printf("qa: %s: %v; starting afresh", qaStateFile, err)
			q.state = qaState{}
		}
	}
	if q.state.Settings.Hour == 0 && q.state.Settings.TimeZone == "" {
		q.state.Settings = qaSettings{Enabled: true, Hour: qaDefaultHour, TimeZone: qaDefaultZone, Apps: q.state.Settings.Apps}
	}
	if q.state.Settings.Apps == nil {
		q.state.Settings.Apps = map[string]bool{}
	}
	if q.state.Explored == nil {
		q.state.Explored = map[string]time.Time{}
	}
	return q
}

func (q *qaService) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /qa", q.overview)
	mux.HandleFunc("PUT /qa/settings", q.putSettings)
	mux.HandleFunc("PUT /qa/apps/{repo}/{name}", q.putApp)
	mux.HandleFunc("POST /qa/runs", q.startRun)
	mux.HandleFunc("GET /qa/runs/{id}", q.getRun)
	mux.HandleFunc("DELETE /qa/runs/{id}", q.cancelRun)
	mux.HandleFunc("GET /qa/findings", q.listFindings)
	mux.HandleFunc("PUT /qa/findings/{id}/verdict", q.putVerdict)
	mux.HandleFunc("GET /qa/shots/{name}", q.shot)
	mux.HandleFunc("POST /qa/device-token", q.deviceToken)
	q.registerJob(mux)
	q.devices.register(mux)
}

// full refuses requests not made with the delta token.
func full(w http.ResponseWriter, r *http.Request) bool {
	if scopeOf(r) != scopeFull {
		writeError(w, http.StatusForbidden, "this needs the delta token")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// The state
// ---------------------------------------------------------------------------

// saveLocked writes the state; q.mu is held.
func (q *qaService) saveLocked() {
	s := &q.state
	sort.SliceStable(s.Runs, func(a, b int) bool { return s.Runs[a].Created.After(s.Runs[b].Created) })
	if len(s.Runs) > maxQARuns {
		s.Runs = s.Runs[:maxQARuns]
	}
	if len(s.Findings) > maxQAFindings {
		// The oldest settled ones go first; open ones are kept.
		keep := s.Findings[:0]
		drop := len(s.Findings) - maxQAFindings
		for _, f := range s.Findings {
			if drop > 0 && f.Status != "open" {
				drop--
				for _, shot := range f.Shots {
					os.Remove(filepath.Join(q.dir, qaShotsDir, shot))
				}
				continue
			}
			keep = append(keep, f)
		}
		s.Findings = keep
	}
	if err := writeJSONFile(filepath.Join(q.dir, qaStateFile), s); err != nil {
		log.Printf("qa: saving: %v", err)
	}
}

func (q *qaService) poke() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// runLocked returns the run with id; q.mu is held.
func (q *qaService) runLocked(id string) *qaRun {
	for i := range q.state.Runs {
		if q.state.Runs[i].ID == id {
			return &q.state.Runs[i]
		}
	}
	return nil
}

// runningLocked returns the running run, if any; q.mu is held.
func (q *qaService) runningLocked() *qaRun {
	for i := range q.state.Runs {
		if q.state.Runs[i].Status == "running" {
			return &q.state.Runs[i]
		}
	}
	return nil
}

func (q *qaService) findingLocked(id string) *qaFinding {
	for i := range q.state.Findings {
		if q.state.Findings[i].ID == id {
			return &q.state.Findings[i]
		}
	}
	return nil
}

// listedRun is a run as the routes show it, without its jobs' keys.
func listedRun(r qaRun) qaRun {
	r.Jobs = slices.Clone(r.Jobs)
	for i := range r.Jobs {
		r.Jobs[i].KeyHash = ""
	}
	r.Apps = slices.Clone(r.Apps)
	return r
}

// ---------------------------------------------------------------------------
// The apps
// ---------------------------------------------------------------------------

var (
	// sonim's Gradle apps (<dir>/android/app or <dir>/app) and the launcher.
	qaPhoneApp  = regexp.MustCompile(`^([a-z0-9_-]+)/(?:android/)?app/build\.gradle$`)
	qaLauncher  = regexp.MustCompile(`^(launcher)/AndroidManifest\.xml$`)
	qaTabletApp = regexp.MustCompile(`^([a-z0-9_-]+)/app/manifest\.json$`)
	qaSpec      = regexp.MustCompile(`^([a-z0-9_-]+)/SPEC\.md$`)
)

// appsFromTree finds the apps in a repository's file list.
func appsFromTree(repo string, paths []string) []qaApp {
	found := map[string]*qaApp{}
	specs := map[string]bool{}
	for _, p := range paths {
		if m := qaSpec.FindStringSubmatch(p); m != nil {
			specs[m[1]] = true
		}
		var m []string
		switch repo {
		case "sonim":
			if m = qaPhoneApp.FindStringSubmatch(p); m == nil {
				m = qaLauncher.FindStringSubmatch(p)
			}
		case "tablet":
			m = qaTabletApp.FindStringSubmatch(p)
		}
		if m != nil && found[m[1]] == nil {
			found[m[1]] = &qaApp{ID: repo + "/" + m[1], Repo: repo, Dir: m[1], Phone: repo == "sonim"}
		}
	}
	var apps []qaApp
	for dir, a := range found {
		a.Spec = specs[dir]
		apps = append(apps, *a)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	return apps
}

// githubApps lists the apps from the repositories' trees on GitHub.
func (q *qaService) githubApps(ctx context.Context) ([]qaApp, error) {
	var apps []qaApp
	for _, repo := range []string{"sonim", "tablet"} {
		var tree struct {
			Tree []struct{ Path, Type string }
		}
		if err := q.github(ctx, http.MethodGet, "/repos/"+qaOwner+"/"+repo+"/git/trees/HEAD?recursive=1", nil, &tree); err != nil {
			return nil, fmt.Errorf("listing %s's apps: %w", repo, err)
		}
		var paths []string
		for _, t := range tree.Tree {
			if t.Type == "blob" {
				paths = append(paths, t.Path)
			}
		}
		apps = append(apps, appsFromTree(repo, paths)...)
	}
	return apps, nil
}

// apps lists the apps, from a cache of a few minutes.
func (q *qaService) apps(ctx context.Context, fresh bool) ([]qaApp, error) {
	q.mu.Lock()
	cached := q.appsCache
	q.mu.Unlock()
	if !fresh && cached.apps != nil && q.now().Sub(cached.at) < 10*time.Minute {
		return cached.apps, nil
	}
	apps, err := q.listApps(ctx)
	if err != nil {
		if cached.apps != nil && !fresh {
			return cached.apps, nil
		}
		return nil, err
	}
	q.mu.Lock()
	q.appsCache.at, q.appsCache.apps = q.now(), apps
	q.mu.Unlock()
	return apps, nil
}

// github calls GitHub's REST API with the user's token, decoding the answer
// into out (if not nil).
func (q *qaService) github(ctx context.Context, method, path string, body any, out any) error {
	data, err := os.ReadFile(q.tokenFile)
	if err != nil {
		return errors.New("no GitHub token is set on the command center")
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(data)))
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode/100 != 2 {
		var e struct{ Message string }
		json.Unmarshal(raw, &e)
		return fmt.Errorf("GitHub answered HTTP %d %s", resp.StatusCode, e.Message)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The user's routes
// ---------------------------------------------------------------------------

// setServer remembers the command center's address as sessions reach it
// (it can't tell itself: it sits behind a proxy, under a path), from what
// the website or the CLI sends.
func (q *qaService) setServer(server string) error {
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	if server == "" {
		return nil
	}
	if u, err := url.Parse(server); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("server must be the command center's https:// address")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.state.Settings.Server != server {
		q.state.Settings.Server = server
		q.saveLocked()
	}
	return nil
}

func (q *qaService) overview(w http.ResponseWriter, r *http.Request) {
	if !full(w, r) {
		return
	}
	apps, appsErr := q.apps(r.Context(), r.URL.Query().Get("fresh") != "")
	type appOut struct {
		qaApp
		Grow     bool        `json:"grow"`
		Explored time.Time   `json:"explored,omitzero"`
		Open     int         `json:"open"`   // findings waiting for triage
		Passed   int         `json:"passed"` // left for the user
		Grown    int         `json:"grown"`  // grown features without a verdict
		Last     *qaRunApp   `json:"last,omitempty"`
		Recent   []qaFinding `json:"recent,omitempty"`
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	out := []appOut{}
	for _, a := range apps {
		o := appOut{qaApp: a, Grow: q.state.Settings.Apps[a.ID], Explored: q.state.Explored[a.ID]}
		for _, f := range q.state.Findings {
			if f.App != a.ID {
				continue
			}
			switch {
			case f.Status == "open":
				o.Open++
			case f.Status == "passed":
				o.Passed++
			case f.Status == "grown" && f.Verdict == "":
				o.Grown++
			}
		}
		for _, run := range q.state.Runs {
			if o.Last != nil {
				break
			}
			for _, ra := range run.Apps {
				if ra.App == a.ID {
					ra := ra
					o.Last = &ra
				}
			}
		}
		out = append(out, o)
	}
	var running *qaRun
	if r := q.runningLocked(); r != nil {
		lr := listedRun(*r)
		running = &lr
	}
	runs := []map[string]any{}
	for _, run := range q.state.Runs {
		if len(runs) == 10 {
			break
		}
		runs = append(runs, map[string]any{
			"id": run.ID, "trigger": run.Trigger, "status": run.Status, "stage": run.Stage,
			"created": run.Created, "ended": run.Ended, "summary": run.Summary, "error": run.Error,
		})
	}
	resp := map[string]any{
		"settings": map[string]any{"enabled": q.state.Settings.Enabled, "hour": q.state.Settings.Hour, "time_zone": q.state.Settings.TimeZone},
		"apps":     out,
		"running":  running,
		"runs":     runs,
		"devices":  q.state.DeviceHash != "",
	}
	if appsErr != nil {
		resp["error"] = appsErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (q *qaService) putSettings(w http.ResponseWriter, r *http.Request) {
	if !full(w, r) {
		return
	}
	var req struct {
		Enabled  *bool   `json:"enabled"`
		Hour     *int    `json:"hour"`
		TimeZone *string `json:"time_zone"`
		Server   string  `json:"server"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, `expected {"enabled"?: bool, "hour"?: 0-23, "time_zone"?: "America/Phoenix", "server"?: "https://..."}`)
		return
	}
	if err := q.setServer(req.Server); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Hour != nil && (*req.Hour < 0 || *req.Hour > 23) {
		writeError(w, http.StatusBadRequest, "the hour is 0 to 23")
		return
	}
	if req.TimeZone != nil {
		if _, err := time.LoadLocation(*req.TimeZone); err != nil || *req.TimeZone == "" {
			writeError(w, http.StatusBadRequest, "that isn't a time zone (try America/Phoenix)")
			return
		}
	}
	q.mu.Lock()
	s := &q.state.Settings
	if req.Enabled != nil {
		s.Enabled = *req.Enabled
	}
	if req.Hour != nil {
		s.Hour = *req.Hour
	}
	if req.TimeZone != nil {
		s.TimeZone = *req.TimeZone
	}
	q.saveLocked()
	out := map[string]any{"enabled": s.Enabled, "hour": s.Hour, "time_zone": s.TimeZone}
	q.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (q *qaService) putApp(w http.ResponseWriter, r *http.Request) {
	if !full(w, r) {
		return
	}
	id := r.PathValue("repo") + "/" + r.PathValue("name")
	var req struct {
		Grow *bool `json:"grow"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil || req.Grow == nil {
		writeError(w, http.StatusBadRequest, `expected {"grow": true|false}`)
		return
	}
	apps, err := q.apps(r.Context(), false)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !slices.ContainsFunc(apps, func(a qaApp) bool { return a.ID == id }) {
		writeError(w, http.StatusNotFound, "no such app")
		return
	}
	q.mu.Lock()
	if *req.Grow {
		q.state.Settings.Apps[id] = true
	} else {
		delete(q.state.Settings.Apps, id)
	}
	q.saveLocked()
	q.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"app": id, "grow": *req.Grow})
}

func (q *qaService) startRun(w http.ResponseWriter, r *http.Request) {
	if !full(w, r) {
		return
	}
	var req struct {
		Apps   []string `json:"apps"`
		Server string   `json:"server"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, `expected {"apps"?: ["sonim/list", ...], "server"?: "https://..."}`)
			return
		}
	}
	if err := q.setServer(req.Server); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	run, err := q.begin(r.Context(), "manual", req.Apps)
	switch {
	case errors.Is(err, errQABusy):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, errQAUnknownApp):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, listedRun(run))
}

func (q *qaService) getRun(w http.ResponseWriter, r *http.Request) {
	if !full(w, r) {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	run := q.runLocked(r.PathValue("id"))
	if run == nil {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	writeJSON(w, http.StatusOK, listedRun(*run))
}

func (q *qaService) cancelRun(w http.ResponseWriter, r *http.Request) {
	if !full(w, r) {
		return
	}
	q.mu.Lock()
	run := q.runLocked(r.PathValue("id"))
	if run == nil {
		q.mu.Unlock()
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	if run.Status == "running" {
		q.finishLocked(run, "cancelled")
		q.saveLocked()
	}
	out := listedRun(*run)
	q.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (q *qaService) listFindings(w http.ResponseWriter, r *http.Request) {
	if !full(w, r) {
		return
	}
	v := r.URL.Query()
	limit := 200
	fmt.Sscan(v.Get("limit"), &limit)
	q.mu.Lock()
	defer q.mu.Unlock()
	out := []qaFinding{}
	for i := len(q.state.Findings) - 1; i >= 0 && len(out) < limit; i-- {
		f := q.state.Findings[i]
		if (v.Get("app") == "" || f.App == v.Get("app")) && (v.Get("status") == "" || f.Status == v.Get("status")) && (v.Get("run") == "" || f.Run == v.Get("run") || f.TriagedIn == v.Get("run")) {
			out = append(out, f)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (q *qaService) putVerdict(w http.ResponseWriter, r *http.Request) {
	if !full(w, r) {
		return
	}
	var req struct {
		Verdict *string `json:"verdict"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil || req.Verdict == nil || !slices.Contains([]string{"up", "down", ""}, *req.Verdict) {
		writeError(w, http.StatusBadRequest, `expected {"verdict": "up"|"down"|""}`)
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	f := q.findingLocked(r.PathValue("id"))
	if f == nil {
		writeError(w, http.StatusNotFound, "no such finding")
		return
	}
	if f.Status != "grown" {
		writeError(w, http.StatusConflict, "only a grown feature takes a verdict")
		return
	}
	f.Verdict, f.Updated = *req.Verdict, q.now().UTC()
	q.saveLocked()
	writeJSON(w, http.StatusOK, f)
}

var qaShotName = regexp.MustCompile(`^[a-z0-9_]+-[0-9]\.png$`)

func (q *qaService) shot(w http.ResponseWriter, r *http.Request) {
	if !full(w, r) {
		return
	}
	q.serveShot(w, r)
}

func (q *qaService) serveShot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !qaShotName.MatchString(name) {
		writeError(w, http.StatusNotFound, "no such screenshot")
		return
	}
	data, err := os.ReadFile(filepath.Join(q.dir, qaShotsDir, name))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such screenshot")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(data)
}

func (q *qaService) deviceToken(w http.ResponseWriter, r *http.Request) {
	if !full(w, r) {
		return
	}
	token := qaDeviceStart + randomHex(32)
	q.mu.Lock()
	q.state.DeviceHash = keyHash(token)
	q.saveLocked()
	q.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"token": token})
}

// decodeShots reads base64 PNGs.
func decodeShots(in []string) ([][]byte, error) {
	if len(in) > maxQAShots {
		return nil, fmt.Errorf("at most %d screenshots a finding", maxQAShots)
	}
	var out [][]byte
	for i, s := range in {
		data, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(data) > maxQAShotBytes || !strings.HasPrefix(string(data), "\x89PNG") {
			return nil, fmt.Errorf("screenshot %d isn't a PNG of at most %d bytes", i+1, maxQAShotBytes)
		}
		out = append(out, data)
	}
	return out, nil
}
