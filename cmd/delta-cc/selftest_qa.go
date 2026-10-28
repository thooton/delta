package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// qaSelfTest takes a QA run through every stage with stand-ins for GitHub,
// the cloud and the agents: the schedule's apps, the device relay, the keys'
// reach, findings and triage, the refactors and the summary.
func qaSelfTest(expect expectFunc, token string, qa *qaService) error {
	var mu sync.Mutex
	keys := map[string]string{} // "role app-or-repo" -> key
	var dispatched []string
	qa.listApps = func(ctx context.Context) ([]qaApp, error) {
		return appsFromTree("sonim", []string{"list/android/app/build.gradle", "list/SPEC.md", "mail/android/app/build.gradle", "common/core/build.gradle", "launcher/AndroidManifest.xml", "launcher/SPEC.md"}), nil
	}
	keyIn := regexp.MustCompile(`delta qa start (` + qaKeyStart + `[0-9a-f]+)`)
	qa.launch = func(ctx context.Context, run *qaRun, j *qaJob, prompt string) (string, string, error) {
		m := keyIn.FindStringSubmatch(prompt)
		if m == nil {
			return "", "", errors.New("the prompt has no key")
		}
		mu.Lock()
		keys[j.Role+" "+firstNonEmptyString(j.App, j.Repo)] = m[1]
		mu.Unlock()
		return "", "session_" + j.ID, nil
	}
	qa.idle = func(ctx context.Context, j qaJob) (bool, error) { return false, nil }
	qa.dispatch = func(ctx context.Context, run qaRun, apps []string, runners int) error {
		dispatched = append(dispatched, strings.Join(apps, " "))
		return nil
	}
	key := func(role, app string) string {
		mu.Lock()
		defer mu.Unlock()
		return keys[role+" "+app]
	}
	ctx := context.Background()
	body := func(v any) []byte { b, _ := json.Marshal(v); return b }

	if len(qa.state.Runs) != 0 {
		return errors.New("a new command center has QA runs")
	}
	apps := appsFromTree("sonim", []string{"list/android/app/build.gradle", "list/SPEC.md", "dictionary/app/build.gradle", "common/core/build.gradle", "launcher/AndroidManifest.xml"})
	if len(apps) != 3 || apps[0].ID != "sonim/dictionary" || apps[2].ID != "sonim/list" || !apps[2].Spec || apps[1].Spec || apps[1].ID != "sonim/launcher" {
		return fmt.Errorf("appsFromTree found %+v", apps)
	}
	if got := appsFromTree("tablet", []string{"updater/app/manifest.json", "tools/appbuild.py"}); len(got) != 1 || got[0].Phone {
		return fmt.Errorf("appsFromTree found %+v in tablet", got)
	}

	if _, err := expect("QA needs the delta token", 401, "GET", "/qa", "", nil); err != nil {
		return err
	}
	if _, err := expect("an unknown app can't grow", 404, "PUT", "/qa/apps/sonim/nope", token, body(map[string]bool{"grow": true})); err != nil {
		return err
	}
	if _, err := expect("an app may grow", 200, "PUT", "/qa/apps/sonim/list", token, body(map[string]bool{"grow": true})); err != nil {
		return err
	}
	if _, err := expect("a bad hour is refused", 400, "PUT", "/qa/settings", token, body(map[string]int{"hour": 24})); err != nil {
		return err
	}
	if _, err := expect("the address must be https", 400, "PUT", "/qa/settings", token, body(map[string]string{"server": "http://delta.example"})); err != nil {
		return err
	}
	if _, err := expect("the command center's address", 200, "PUT", "/qa/settings", token, body(map[string]string{"server": "https://delta.example/api/delta/"})); err != nil {
		return err
	}
	qa.mu.Lock()
	if qa.state.Settings.Server != "https://delta.example/api/delta" {
		qa.mu.Unlock()
		return fmt.Errorf("the address was saved as %q", qa.state.Settings.Server)
	}
	// Apps explored least recently go first: the list before the launcher.
	qa.state.Explored["sonim/launcher"] = time.Now()
	qa.mu.Unlock()
	if _, err := expect("no device token yet", 201, "POST", "/qa/device-token", token, nil); err != nil {
		return err
	}
	data, err := expect("a device token", 201, "POST", "/qa/device-token", token, nil)
	if err != nil {
		return err
	}
	var dt struct{ Token string }
	json.Unmarshal(data, &dt)
	device := dt.Token

	if _, err := expect("a run of an unknown app is refused", 404, "POST", "/qa/runs", token, body(map[string][]string{"apps": {"sonim/nope"}})); err != nil {
		return err
	}
	data, err = expect("start a run", 201, "POST", "/qa/runs", token, nil)
	if err != nil {
		return err
	}
	var run qaRun
	json.Unmarshal(data, &run)
	if len(run.Apps) != 3 || run.Devices != "dispatched" || len(dispatched) != 1 || dispatched[0] != "list launcher" {
		return fmt.Errorf("the run began as %+v, dispatching %q", run, dispatched)
	}
	if _, err := expect("one run at a time", 409, "POST", "/qa/runs", token, nil); err != nil {
		return err
	}

	qa.step(ctx)
	if key("spec", "sonim/mail") == "" {
		return errors.New("the app with no spec didn't get a spec job")
	}
	if key("explore", "sonim/list") != "" {
		return errors.New("a phone app was explored before there was an emulator")
	}
	spec := key("spec", "sonim/mail")
	if _, err := expect("a job's key reaches its job", 200, "GET", "/qa/job", spec, nil); err != nil {
		return err
	}
	for _, p := range []string{"/files", "/update", "/qa", "/otp", "/qa/device/poll?runner=x"} {
		if _, err := expect("a job's key can't reach "+p, 403, "GET", p, spec, nil); err != nil {
			return err
		}
	}
	if _, err := expect("a spec job has no emulator", 403, "POST", "/qa/job/device", spec, body(map[string]any{"op": "shot"})); err != nil {
		return err
	}
	if _, err := expect("the device token reaches only the relay", 403, "GET", "/files", device, nil); err != nil {
		return err
	}
	// Late emulators take back the apps skipped for want of them.
	qa.mu.Lock()
	qa.runLocked(run.ID).Apps[2].Status, qa.runLocked(run.ID).Apps[2].Why = "skipped", qaNoEmulators
	qa.mu.Unlock()
	if _, err := expect("the emulators say hello", 200, "POST", "/qa/device/hello", device, body(map[string]any{"runner": "r1", "run": run.ID, "serials": []string{"emulator-5554"}})); err != nil {
		return err
	}
	qa.mu.Lock()
	back := qa.runLocked(run.ID).Apps[2].Status == "waiting"
	qa.mu.Unlock()
	if !back {
		return errors.New("an app skipped for want of emulators didn't get them back when they came")
	}
	qa.step(ctx)
	explore := key("explore", "sonim/list")
	if explore == "" || key("explore", "sonim/launcher") != "" {
		return errors.New("with one emulator, exactly one phone app should be explored")
	}
	data, err = expect("the explorer's instructions", 200, "GET", "/qa/job", explore, nil)
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), "EXPLORE LIST") || !strings.Contains(string(data), "GROWING") {
		return fmt.Errorf("the explorer's instructions are wrong: %.300s", data)
	}

	// A command goes to the emulator and its result back to the agent.
	png := "\x89PNG\r\n\x1a\nfake"
	got := make(chan error, 1)
	go func() {
		data, err := expect("a screenshot", 200, "POST", "/qa/job/device", explore, body(map[string]any{"op": "shot"}))
		var res qaDeviceResult
		if err == nil {
			json.Unmarshal(data, &res)
			if f, _ := base64.StdEncoding.DecodeString(res.File); string(f) != png {
				err = fmt.Errorf("the screenshot came back as %q", res.File)
			}
		}
		got <- err
	}()
	var cmd qaCommand
	for range 50 {
		data, err := expect("the emulators poll", 200, "GET", "/qa/device/poll?runner=r1", device, nil)
		if err != nil {
			return err
		}
		var p struct {
			Commands []qaCommand
			Quit     bool
		}
		json.Unmarshal(data, &p)
		if p.Quit {
			return errors.New("the emulators were told to quit mid-run")
		}
		if len(p.Commands) > 0 {
			cmd = p.Commands[0]
			break
		}
	}
	if cmd.Op != "shot" || cmd.App != "list" || cmd.Serial != "emulator-5554" {
		return fmt.Errorf("the emulators got %+v", cmd)
	}
	if _, err := expect("the emulators answer", 200, "POST", "/qa/device/result", device, body(map[string]any{"id": cmd.ID, "ok": true, "file": base64.StdEncoding.EncodeToString([]byte(png))})); err != nil {
		return err
	}
	select {
	case err := <-got:
		if err != nil {
			return err
		}
	case <-time.After(10 * time.Second):
		return errors.New("the screenshot never came back")
	}

	shot := base64.StdEncoding.EncodeToString([]byte(png))
	data, err = expect("file a finding", 201, "POST", "/qa/job/find", explore, body(map[string]any{"kind": "bug", "title": "Saving cuts the last word", "detail": "Steps: ...", "shots": []string{shot}}))
	if err != nil {
		return err
	}
	var bug qaFinding
	json.Unmarshal(data, &bug)
	if _, err := expect("its screenshot", 200, "GET", "/qa/shots/"+bug.Shots[0], token, nil); err != nil {
		return err
	}
	if _, err := expect("a finding needs details", 400, "POST", "/qa/job/find", explore, body(map[string]any{"kind": "bug", "title": "x"})); err != nil {
		return err
	}
	data, err = expect("propose a feature", 201, "POST", "/qa/job/find", explore, body(map[string]any{"kind": "feature", "title": "Due dates", "detail": "Why it fits: ..."}))
	if err != nil {
		return err
	}
	var feature qaFinding
	json.Unmarshal(data, &feature)
	if _, err := expect("only the fix job triages", 403, "POST", "/qa/job/triage", explore, body(map[string]string{"id": bug.ID, "status": "fixed", "reason": "x", "commit": "abcdef1"})); err != nil {
		return err
	}
	if _, err := expect("the explorer is done", 200, "POST", "/qa/job/done", explore, body(map[string]string{"summary": "Went through every screen."})); err != nil {
		return err
	}
	if _, err := expect("a finished job's key is dead", 401, "GET", "/qa/job", explore, nil); err != nil {
		return err
	}

	qa.step(ctx)
	fix := key("fix", "sonim/list")
	if fix == "" {
		return errors.New("the findings got no fix job")
	}
	if key("explore", "sonim/launcher") != "" {
		return errors.New("the launcher took the emulator the list's fix job holds")
	}
	if _, err := expect("triage needs the commit", 400, "POST", "/qa/job/triage", fix, body(map[string]string{"id": bug.ID, "status": "fixed", "reason": "Fixed."})); err != nil {
		return err
	}
	if _, err := expect("a fix is fixed, not grown", 400, "POST", "/qa/job/triage", fix, body(map[string]string{"id": bug.ID, "status": "grown", "reason": "x", "commit": "abcdef1"})); err != nil {
		return err
	}
	if _, err := expect("not done with findings open", 409, "POST", "/qa/job/done", fix, body(map[string]string{"summary": "Fixed it."})); err != nil {
		return err
	}
	if _, err := expect("triage the bug", 200, "POST", "/qa/job/triage", fix, body(map[string]string{"id": bug.ID, "status": "fixed", "reason": "Long tasks keep their last word.", "commit": "abcdef1"})); err != nil {
		return err
	}
	if _, err := expect("grow the feature", 200, "POST", "/qa/job/triage", fix, body(map[string]string{"id": feature.ID, "status": "grown", "reason": "Tasks can have due dates.", "commit": "abcdef2"})); err != nil {
		return err
	}
	if _, err := expect("the fixer is done", 200, "POST", "/qa/job/done", fix, body(map[string]string{"summary": "Fixed one, grew one."})); err != nil {
		return err
	}
	if _, err := expect("a verdict on the grown feature", 200, "PUT", "/qa/findings/"+feature.ID+"/verdict", token, body(map[string]string{"verdict": "down"})); err != nil {
		return err
	}
	if _, err := expect("a bug takes no verdict", 409, "PUT", "/qa/findings/"+bug.ID+"/verdict", token, body(map[string]string{"verdict": "up"})); err != nil {
		return err
	}

	qa.step(ctx)
	launcher := key("explore", "sonim/launcher")
	if launcher == "" {
		return errors.New("the emulator didn't pass to the next app")
	}
	for _, k := range []string{spec, launcher} {
		if _, err := expect("a job is done", 200, "POST", "/qa/job/done", k, body(map[string]string{"summary": "Done."})); err != nil {
			return err
		}
	}
	qa.step(ctx) // the launcher found nothing, so the apps are through and the refactors begin
	for _, repo := range qaRepos {
		k := key("refactor", repo)
		if k == "" {
			return fmt.Errorf("%s wasn't refactored", repo)
		}
		if _, err := expect("a refactor files nothing", 403, "POST", "/qa/job/find", k, body(map[string]any{"kind": "bug", "title": "x", "detail": "y"})); err != nil {
			return err
		}
		if _, err := expect("a refactor is done", 200, "POST", "/qa/job/done", k, body(map[string]string{"summary": "Simpler."})); err != nil {
			return err
		}
		qa.step(ctx)
	}
	data, err = expect("the run", 200, "GET", "/qa/runs/"+run.ID, token, nil)
	if err != nil {
		return err
	}
	json.Unmarshal(data, &run)
	if run.Status != "done" || !strings.Contains(run.Summary, "Fixed 1, grew 1 feature") || !strings.Contains(run.Summary, "Updates waiting: List.") || !strings.Contains(run.Summary, "wrote 1 spec") {
		return fmt.Errorf("the run ended as %s with the summary %q", run.Status, run.Summary)
	}
	if strings.Contains(string(data), "key_hash") {
		return errors.New("the run shows its jobs' keys")
	}
	data, err = expect("the emulators poll after the run", 200, "GET", "/qa/device/poll?runner=r1", device, nil)
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), `"quit":true`) {
		return fmt.Errorf("the emulators weren't told to quit: %s", data)
	}

	// The next run's fix job is told to take out what the user turned down.
	if _, err := expect("a run of one app", 201, "POST", "/qa/runs", token, body(map[string][]string{"apps": {"sonim/list"}})); err != nil {
		return err
	}
	qa.mu.Lock()
	items := qa.fixItemsLocked("sonim/list", "")
	r := qa.runningLocked()
	all := r != nil && r.All
	qa.mu.Unlock()
	if len(items) != 1 || items[0].ID != feature.ID || all {
		return fmt.Errorf("the turned-down feature isn't waiting to be reverted (%+v), or a one-app run refactors", items)
	}
	data, err = expect("the overview", 200, "GET", "/qa", token, nil)
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), `"running":{`) {
		return fmt.Errorf("the overview doesn't show the running run: %.300s", data)
	}
	if _, err := expect("cancel it", 200, "DELETE", "/qa/runs/"+r.ID, token, nil); err != nil {
		return err
	}
	return nil
}
