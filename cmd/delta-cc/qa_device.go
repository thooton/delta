package main

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// The device relay: phone apps are explored on Android emulators that a
// GitHub Actions workflow in sonim (.github/workflows/qa-devices.yml, its
// bridge in .github/qa/bridge.py) runs for each run, since cloud sessions
// can't run one. Each Actions job (a runner) boots a few emulators, installs
// the run's apps, says hello with the device token, and then long-polls for
// commands, running each with adb and posting the result back. A job's agent
// sends commands with `delta qa device ...` (POST /qa/job/device), which
// waits for the result. Nothing here is saved: emulators live for one run.
//
// An app holds an emulator (a lease) from its explore job to the end of its
// fix job, so the fix is checked on the device the problem was found on.
//
// Routes, for the device token (scopeQADevice):
//
//	POST /qa/device/hello   {"runner", "run", "serials"}: the runner's emulators are ready.
//	GET  /qa/device/poll    ?runner=: waits up to 25 seconds for commands:
//	                        {"commands": [{"id", "serial", "app", "op", "args"}]}, or {"quit": true}.
//	POST /qa/device/result  {"id", "ok", "output", "error", "file"?}: a command's result (file is base64).
const (
	qaRunnerGone = 2 * time.Minute // a runner not heard from for this long has gone
	qaPollWait   = 25 * time.Second
)

// qaDeviceOps are the commands a job may send, and how long each may take.
var qaDeviceOps = map[string]time.Duration{
	"key":     2 * time.Minute,
	"text":    time.Minute,
	"shot":    time.Minute,
	"ui":      time.Minute,
	"log":     time.Minute,
	"launch":  time.Minute,
	"stop":    time.Minute,
	"reset":   time.Minute,
	"shell":   5 * time.Minute,
	"install": 30 * time.Minute,
}

type qaCommand struct {
	ID     string   `json:"id"`
	Serial string   `json:"serial"`
	App    string   `json:"app"` // the app's directory in sonim
	Op     string   `json:"op"`
	Args   []string `json:"args"`
	runner string
	done   chan qaDeviceResult
}

type qaDeviceResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
	File   string `json:"file,omitempty"` // base64: a screenshot
}

type qaRunner struct {
	id, run string
	serials []string
	seen    time.Time
	queue   []*qaCommand
	wake    chan struct{}
}

type qaDevices struct {
	q       *qaService
	mu      sync.Mutex
	runners map[string]*qaRunner
	leases  map[string]string // app -> runner/serial
	sent    map[string]*qaCommand
}

func newQADevices(q *qaService) *qaDevices {
	return &qaDevices{q: q, runners: map[string]*qaRunner{}, leases: map[string]string{}, sent: map[string]*qaCommand{}}
}

func (d *qaDevices) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /qa/device/hello", d.hello)
	mux.HandleFunc("GET /qa/device/poll", d.poll)
	mux.HandleFunc("POST /qa/device/result", d.result)
}

func deviceOnly(w http.ResponseWriter, r *http.Request) bool {
	if scopeOf(r) != scopeQADevice {
		writeError(w, http.StatusForbidden, "these routes are for the emulators' device token")
		return false
	}
	return true
}

// expireLocked forgets runners that have gone quiet; d.mu is held.
func (d *qaDevices) expireLocked() {
	now := d.q.now()
	for id, rn := range d.runners {
		if now.Sub(rn.seen) < qaRunnerGone {
			continue
		}
		delete(d.runners, id)
		for app, dev := range d.leases {
			if strings.HasPrefix(dev, id+"/") {
				delete(d.leases, app)
			}
		}
		for cid, c := range d.sent {
			if c.runner == id {
				delete(d.sent, cid)
				c.done <- qaDeviceResult{Error: qaDeviceGone}
			}
		}
		for _, c := range rn.queue {
			c.done <- qaDeviceResult{Error: qaDeviceGone}
		}
	}
}

// qaDeviceGone is a command's error when its emulator's runner went away.
const qaDeviceGone = "the emulator went away"

// lease gives app an emulator of run's, if it hasn't one, and says whether
// it has one now.
func (d *qaDevices) lease(run, app string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked()
	if _, ok := d.leases[app]; ok {
		return true
	}
	taken := map[string]bool{}
	for _, dev := range d.leases {
		taken[dev] = true
	}
	for _, id := range slices.Sorted(maps.Keys(d.runners)) {
		rn := d.runners[id]
		if rn.run != run {
			continue
		}
		for _, s := range rn.serials {
			if dev := id + "/" + s; !taken[dev] {
				d.leases[app] = dev
				return true
			}
		}
	}
	return false
}

func (d *qaDevices) release(app string) {
	d.mu.Lock()
	delete(d.leases, app)
	d.mu.Unlock()
}

// any says whether run has emulators up.
func (d *qaDevices) any(run string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked()
	for _, rn := range d.runners {
		if rn.run == run {
			return true
		}
	}
	return false
}

func (d *qaDevices) hello(w http.ResponseWriter, r *http.Request) {
	if !deviceOnly(w, r) {
		return
	}
	var req struct {
		Runner, Run string
		Serials     []string
	}
	if err := decodeBody(r, 64<<10, &req); err != nil || req.Runner == "" || len(req.Serials) == 0 || len(req.Serials) > 8 {
		writeError(w, http.StatusBadRequest, `expected {"runner": ..., "run": ..., "serials": [...]}`)
		return
	}
	d.q.mu.Lock()
	running := d.q.runningLocked()
	live := running != nil && running.ID == req.Run && running.Stage == "apps"
	if live {
		// Apps skipped because the emulators were late get them after all.
		for i := range running.Apps {
			if a := &running.Apps[i]; a.Status == "skipped" && a.Why == qaNoEmulators {
				a.Status, a.Why = "waiting", ""
			}
		}
		d.q.saveLocked()
	}
	d.q.mu.Unlock()
	if !live {
		writeJSON(w, http.StatusOK, map[string]any{"quit": true})
		return
	}
	d.mu.Lock()
	rn := d.runners[req.Runner]
	if rn == nil {
		rn = &qaRunner{id: req.Runner, wake: make(chan struct{}, 1)}
		d.runners[req.Runner] = rn
	}
	rn.run, rn.serials, rn.seen = req.Run, slices.Clone(req.Serials), d.q.now()
	d.mu.Unlock()
	d.q.poke()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (d *qaDevices) poll(w http.ResponseWriter, r *http.Request) {
	if !deviceOnly(w, r) {
		return
	}
	id := r.URL.Query().Get("runner")
	deadline := time.NewTimer(qaPollWait)
	defer deadline.Stop()
	for {
		d.q.mu.Lock()
		running := d.q.runningLocked()
		var runID string
		apps := false
		if running != nil {
			runID, apps = running.ID, running.Stage == "apps"
		}
		d.q.mu.Unlock()

		d.mu.Lock()
		rn := d.runners[id]
		if rn == nil {
			d.mu.Unlock()
			writeError(w, http.StatusNotFound, "say hello first")
			return
		}
		rn.seen = d.q.now()
		leased := false
		for _, dev := range d.leases {
			leased = leased || strings.HasPrefix(dev, id+"/")
		}
		if rn.run != runID || !apps && !leased && len(rn.queue) == 0 {
			delete(d.runners, id)
			d.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"quit": true})
			return
		}
		if len(rn.queue) > 0 {
			cmds := rn.queue
			rn.queue = nil
			for _, c := range cmds {
				d.sent[c.ID] = c
			}
			d.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"commands": cmds})
			return
		}
		wake := rn.wake
		d.mu.Unlock()
		select {
		case <-wake:
		case <-deadline.C:
			writeJSON(w, http.StatusOK, map[string]any{"commands": []any{}})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (d *qaDevices) result(w http.ResponseWriter, r *http.Request) {
	if !deviceOnly(w, r) {
		return
	}
	var req struct {
		ID string `json:"id"`
		qaDeviceResult
	}
	if err := decodeBody(r, 16<<20, &req); err != nil || req.ID == "" {
		writeError(w, http.StatusBadRequest, `expected {"id": ..., "ok": ..., "output": ...}`)
		return
	}
	d.mu.Lock()
	c := d.sent[req.ID]
	delete(d.sent, req.ID)
	d.mu.Unlock()
	if c == nil {
		writeError(w, http.StatusNotFound, "no such command (it may have timed out)")
		return
	}
	c.done <- req.qaDeviceResult
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// do runs a command on app's emulator and waits for its result.
func (d *qaDevices) do(r *http.Request, run, app, dir, op string, args []string) (qaDeviceResult, int, error) {
	limit, ok := qaDeviceOps[op]
	if !ok {
		ops := slices.Sorted(maps.Keys(qaDeviceOps))
		return qaDeviceResult{}, http.StatusBadRequest, fmt.Errorf("%q isn't something the emulator does (%s)", op, strings.Join(ops, ", "))
	}
	if !d.lease(run, app) {
		return qaDeviceResult{}, http.StatusServiceUnavailable, fmt.Errorf("no emulator is free for this app right now; try again in a minute")
	}
	d.mu.Lock()
	dev := d.leases[app]
	runnerID, serial, _ := strings.Cut(dev, "/")
	rn := d.runners[runnerID]
	if rn == nil {
		d.mu.Unlock()
		return qaDeviceResult{}, http.StatusServiceUnavailable, fmt.Errorf("the emulator went away; try again")
	}
	c := &qaCommand{ID: "cmd_" + randomHex(8), Serial: serial, App: dir, Op: op, Args: args, runner: runnerID, done: make(chan qaDeviceResult, 1)}
	rn.queue = append(rn.queue, c)
	select {
	case rn.wake <- struct{}{}:
	default:
	}
	d.mu.Unlock()

	timer := time.NewTimer(limit)
	defer timer.Stop()
	forget := func() {
		d.mu.Lock()
		delete(d.sent, c.ID)
		if rn := d.runners[runnerID]; rn != nil {
			rn.queue = slices.DeleteFunc(rn.queue, func(x *qaCommand) bool { return x == c })
		}
		d.mu.Unlock()
	}
	select {
	case res := <-c.done:
		if res.Error == qaDeviceGone {
			return res, http.StatusServiceUnavailable, fmt.Errorf("the emulator went away; try again, and you'll get another (install the app's branch again if you had)")
		}
		return res, http.StatusOK, nil
	case <-timer.C:
		forget()
		return qaDeviceResult{}, http.StatusGatewayTimeout, fmt.Errorf("the emulator didn't answer within %s", limit)
	case <-r.Context().Done():
		forget()
		return qaDeviceResult{}, http.StatusGatewayTimeout, r.Context().Err()
	}
}
