package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const qaHelpText = `delta qa: the nightly QA of the phone and tablet apps

Every night (and whenever the user starts one from Tree on the phone) the
command center runs QA: for each app, a cloud session explores it in an
emulator and files findings, then another fixes what should be fixed and
pushes it; then one refactors each repository. Each session is a job with
a key of its own, which its first message gives it.

FOR A QA JOB
  delta qa start <key>               Sign in with the job's key and print
                                     its instructions. Do this first.
  delta qa brief                     Print the instructions again.
  delta qa findings                  The app's findings, as JSON.
  delta qa shot <name> [file]        Save a finding's screenshot.
  delta qa find --kind <kind> --title <title> --detail <text> [--shot <png>]...
                                     File a finding.
  delta qa triage <id> <status> --reason <text> [--commit <sha>]
                                     Say what became of a finding.
  delta qa device <op> [args]        Drive the job's phone emulator:
                                     launch, key, text, shot, ui, log,
                                     stop, reset, shell, install.
  delta qa done "<summary>" [--force]
                                     Finish the job.

WITH THE DELTA TOKEN (any other session)
  delta qa status                    The schedule, the apps and the last runs.
  delta qa run [app...]              Start a run now: every app, or those
                                     named (sonim/list, tablet/flashcards).
  delta qa cancel                    Cancel the running run.
  delta qa setup                     Tell the command center its address
                                     (for the sessions it starts), make the
                                     emulators' device token, and put both
                                     in sonim's Actions secrets
                                     (QA_DELTA_SERVER, QA_DEVICE_TOKEN).
                                     Once; again to rotate the token.
`

func runQA(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(qaHelpText)
		return nil
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "start":
		if len(args) != 1 {
			return errors.New("usage: delta qa start <key>")
		}
		return qaStart(args[0])
	case "brief":
		return qaBrief()
	case "findings":
		return qaGet("/qa/job/findings")
	case "shot":
		if len(args) < 1 || len(args) > 2 {
			return errors.New("usage: delta qa shot <name> [file]")
		}
		dest := args[0]
		if len(args) == 2 {
			dest = args[1]
		}
		return qaShot(args[0], dest)
	case "find":
		return qaFind(args)
	case "triage":
		return qaTriage(args)
	case "device":
		return qaDevice(args)
	case "done":
		return qaDone(args)
	case "status":
		return qaStatus()
	case "run":
		return qaRun(args)
	case "cancel":
		return qaCancel()
	case "setup":
		return qaSetup()
	}
	return fmt.Errorf("unknown command %q (run `delta qa help`)", cmd)
}

// qaStart signs in with a job's key, at the command center the environment
// names, and prints the job's instructions.
func qaStart(key string) error {
	server := os.Getenv("DELTA_SERVER")
	if server == "" {
		if c, err := loadConfig(); err == nil {
			server = c.Server
		}
	}
	if server == "" {
		return errors.New("DELTA_SERVER isn't set, so the command center's address isn't known")
	}
	if err := setAuth(server, key); err != nil {
		return err
	}
	return qaBrief()
}

func qaBrief() error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	body, err := do(c, http.MethodGet, "/qa/job", nil)
	if err != nil {
		return err
	}
	var res struct{ Instructions string }
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	fmt.Print(res.Instructions)
	return nil
}

func qaGet(path string) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	body, err := do(c, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	os.Stdout.Write(body)
	return nil
}

// qaPost sends v as JSON and decodes the answer into out (if not nil).
func qaPost(method, path string, v, out any) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	data, _ := json.Marshal(v)
	body, err := do(c, method, path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

func qaShot(name, dest string) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	data, err := do(c, http.MethodGet, "/qa/job/shots/"+url.PathEscape(name), nil)
	if err != nil {
		// With the delta token, the user's route.
		if data, err = do(c, http.MethodGet, "/qa/shots/"+url.PathEscape(name), nil); err != nil {
			return err
		}
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("Saved %s.\n", dest)
	return nil
}

func qaFind(args []string) error {
	o, err := parseOpts(args, "kind title detail shot", "")
	if err != nil {
		return err
	}
	if len(o.pos) != 0 || o.str("kind") == "" || o.str("title") == "" || o.str("detail") == "" {
		return errors.New("usage: delta qa find --kind <bug|problem|inconsistency|feature> --title <title> --detail <text> [--shot <png>]...")
	}
	var shots []string
	for _, path := range o.vals["shot"] {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		shots = append(shots, base64.StdEncoding.EncodeToString(data))
	}
	var f struct{ ID string }
	if err := qaPost(http.MethodPost, "/qa/job/find", map[string]any{
		"kind": o.str("kind"), "title": o.str("title"), "detail": o.str("detail"), "shots": shots,
	}, &f); err != nil {
		return err
	}
	fmt.Printf("Filed %s.\n", f.ID)
	return nil
}

func qaTriage(args []string) error {
	o, err := parseOpts(args, "reason commit", "")
	if err != nil {
		return err
	}
	if len(o.pos) != 2 || o.str("reason") == "" {
		return errors.New("usage: delta qa triage <id> <fixed|passed|declined|grown|reverted> --reason <one line> [--commit <sha>]")
	}
	if err := qaPost(http.MethodPost, "/qa/job/triage", map[string]string{
		"id": o.pos[0], "status": o.pos[1], "reason": o.str("reason"), "commit": o.str("commit"),
	}, nil); err != nil {
		return err
	}
	fmt.Printf("%s is %s.\n", o.pos[0], o.pos[1])
	return nil
}

func qaDone(args []string) error {
	o, err := parseOpts(args, "", "force")
	if err != nil {
		return err
	}
	if len(o.pos) != 1 {
		return errors.New(`usage: delta qa done "<summary>" [--force]`)
	}
	if err := qaPost(http.MethodPost, "/qa/job/done", map[string]any{"summary": o.pos[0], "force": o.bools["force"]}, nil); err != nil {
		return err
	}
	fmt.Println("Done. This job's key no longer works; there is nothing more to do here.")
	return nil
}

// qaDevice sends a command to the job's emulator. Screenshots and the view
// hierarchy are saved to a file; everything else is printed.
func qaDevice(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: delta qa device <launch|key|text|shot|ui|log|stop|reset|shell|install> [args]")
	}
	op, rest := args[0], args[1:]
	file := ""
	switch op {
	case "shot":
		file = "shot.png"
		if len(rest) > 0 {
			file, rest = rest[0], nil
		}
	case "ui":
		file = "ui.xml"
		if len(rest) > 0 {
			file, rest = rest[0], nil
		}
	case "install":
		o, err := parseOpts(rest, "ref", "")
		if err != nil {
			return err
		}
		rest = nil
		if ref := o.str("ref"); ref != "" {
			rest = []string{ref}
		}
	case "log":
		o, err := parseOpts(rest, "", "clear")
		if err != nil {
			return err
		}
		rest = nil
		if o.bools["clear"] {
			rest = []string{"--clear"}
		}
	case "text", "shell":
		if len(rest) == 0 {
			return fmt.Errorf("usage: delta qa device %s \"<%s>\"", op, map[string]string{"text": "text", "shell": "command"}[op])
		}
		rest = []string{strings.Join(rest, " ")}
	}
	start := time.Now()
	var res struct {
		OK     bool
		Output string
		Error  string
		File   string
	}
	if err := qaPost(http.MethodPost, "/qa/job/device", map[string]any{"op": op, "args": rest}, &res); err != nil {
		return err
	}
	if !res.OK {
		if res.Output != "" {
			fmt.Println(strings.TrimRight(res.Output, "\n"))
		}
		return fmt.Errorf("the emulator: %s", firstNonEmpty(res.Error, "it failed"))
	}
	if file != "" {
		data := []byte(res.Output)
		if res.File != "" {
			var err error
			if data, err = base64.StdEncoding.DecodeString(res.File); err != nil {
				return err
			}
		}
		if err := os.WriteFile(file, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("Saved %s.\n", file)
		return nil
	}
	if out := strings.TrimRight(res.Output, "\n"); out != "" {
		fmt.Println(out)
	} else {
		fmt.Printf("OK (%s).\n", time.Since(start).Round(100*time.Millisecond))
	}
	return nil
}

func qaStatus() error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	body, err := do(c, http.MethodGet, "/qa", nil)
	if err != nil {
		return err
	}
	var o struct {
		Settings struct {
			Enabled  bool
			Hour     int
			TimeZone string `json:"time_zone"`
		}
		Devices bool
		Error   string
		Apps    []struct {
			ID                  string
			Grow, Spec          bool
			Open, Passed, Grown int
		}
		Running *struct {
			ID, Stage string
			Apps      []struct{ App, Status string }
		}
		Runs []struct {
			ID, Status, Summary, Error string
			Created                    time.Time
		}
	}
	if err := json.Unmarshal(body, &o); err != nil {
		return err
	}
	if o.Settings.Enabled {
		fmt.Printf("Nightly at %02d:00 (%s).", o.Settings.Hour, o.Settings.TimeZone)
	} else {
		fmt.Print("The nightly run is off.")
	}
	if !o.Devices {
		fmt.Print(" The emulators have no device token: run `delta qa setup`.")
	}
	fmt.Println()
	if o.Error != "" {
		fmt.Println("Apps:", o.Error)
	}
	for _, a := range o.Apps {
		var notes []string
		if !a.Spec {
			notes = append(notes, "no spec yet")
		}
		if a.Grow {
			notes = append(notes, "grows")
		}
		if a.Open > 0 {
			notes = append(notes, fmt.Sprintf("%d open", a.Open))
		}
		if a.Passed > 0 {
			notes = append(notes, fmt.Sprintf("%d left for the user", a.Passed))
		}
		if a.Grown > 0 {
			notes = append(notes, fmt.Sprintf("%d grown, awaiting a verdict", a.Grown))
		}
		fmt.Printf("  %-24s %s\n", a.ID, strings.Join(notes, ", "))
	}
	if o.Running != nil {
		fmt.Printf("\nRunning: %s (%s)\n", o.Running.ID, o.Running.Stage)
		for _, a := range o.Running.Apps {
			fmt.Printf("  %-24s %s\n", a.App, a.Status)
		}
	}
	for i, r := range o.Runs {
		if i == 3 {
			break
		}
		fmt.Printf("\n%s (%s)\n%s\n", r.ID, r.Status, firstNonEmpty(r.Summary, r.Error))
	}
	return nil
}

func qaRun(apps []string) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	req := map[string]any{"apps": apps}
	if strings.HasPrefix(c.Server, "https://") {
		req["server"] = c.Server
	}
	var run struct{ ID string }
	if err := qaPost(http.MethodPost, "/qa/runs", req, &run); err != nil {
		return err
	}
	fmt.Printf("Started %s. `delta qa status` follows it.\n", run.ID)
	return nil
}

func qaCancel() error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	body, err := do(c, http.MethodGet, "/qa", nil)
	if err != nil {
		return err
	}
	var o struct{ Running *struct{ ID string } }
	if err := json.Unmarshal(body, &o); err != nil {
		return err
	}
	if o.Running == nil {
		return errors.New("no run is going")
	}
	if _, err := do(c, http.MethodDelete, "/qa/runs/"+url.PathEscape(o.Running.ID), nil); err != nil {
		return err
	}
	fmt.Printf("Cancelled %s.\n", o.Running.ID)
	return nil
}

// qaSetup makes the device token and stores it, and the command center's
// address, as sonim's Actions secrets, where the emulator workflow reads
// them. Neither ever appears in the workflow's public logs.
func qaSetup() error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if strings.HasPrefix(c.Server, "http://"+unixHost) {
		return errors.New("run this from a regular (●) session: the emulators reach the command center over the internet")
	}
	if err := qaPost(http.MethodPut, "/qa/settings", map[string]string{"server": c.Server}, nil); err != nil {
		return err
	}
	var res struct{ Token string }
	if err := qaPost(http.MethodPost, "/qa/device-token", nil, &res); err != nil {
		return err
	}
	for name, value := range map[string]string{"QA_DELTA_SERVER": c.Server, "QA_DEVICE_TOKEN": res.Token} {
		if err := runWithAccessToken("gh", []string{"secret", "set", name, "-R", "thooton/sonim", "--body", value}); err != nil {
			return fmt.Errorf("setting %s: %w", name, err)
		}
	}
	fmt.Println("The emulators are set up: sonim's QA workflow can reach the command center.")
	return nil
}
