package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestQA(t *testing.T) *qaService {
	q := newQAService(t.TempDir(), nil)
	q.listApps = func(ctx context.Context) ([]qaApp, error) {
		return []qaApp{
			{ID: "sonim/list", Repo: "sonim", Dir: "list", Phone: true, Spec: true},
			{ID: "tablet/updater", Repo: "tablet", Dir: "updater", Spec: true},
		}, nil
	}
	q.dispatch = func(context.Context, qaRun, []string, int) error { return nil }
	q.idle = func(context.Context, qaJob) (bool, error) { return false, nil }
	q.state.Settings.Server = "https://delta.example"
	q.state.DeviceHash = "x"
	return q
}

func TestQAScheduleOncePerNight(t *testing.T) {
	q := newTestQA(t)
	loc, _ := time.LoadLocation(qaDefaultZone)
	now := time.Date(2026, 10, 28, 1, 59, 0, 0, loc)
	q.now = func() time.Time { return now }
	q.schedule(context.Background())
	if len(q.state.Runs) != 0 {
		t.Fatal("a run started before its hour")
	}
	now = now.Add(2 * time.Minute)
	q.schedule(context.Background())
	if len(q.state.Runs) != 1 || q.state.Runs[0].Trigger != "schedule" {
		t.Fatalf("the nightly run didn't start: %+v", q.state.Runs)
	}
	q.finishLocked(&q.state.Runs[0], "done")
	now = now.Add(30 * time.Minute)
	q.schedule(context.Background())
	if len(q.state.Runs) != 1 {
		t.Fatal("a second run started the same night")
	}
	now = now.Add(24 * time.Hour)
	q.schedule(context.Background())
	if len(q.state.Runs) != 2 {
		t.Fatal("the next night's run didn't start")
	}
}

func TestQANoRoomSkipsApps(t *testing.T) {
	q := newTestQA(t)
	q.launch = func(context.Context, *qaRun, *qaJob, string) (string, string, error) {
		return "", "", errors.Join(errQANoRoom, errors.New("every Claude account is at its usage limit"))
	}
	run, err := q.begin(context.Background(), "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		q.step(context.Background())
	}
	r := q.runLocked(run.ID)
	if r.Status != "done" {
		t.Fatalf("the run is %s, at %s", r.Status, r.Stage)
	}
	for _, a := range r.Apps {
		if a.Status != "skipped" || !strings.Contains(a.Why, "no room") {
			t.Errorf("%s: %s (%s)", a.App, a.Status, a.Why)
		}
	}
	if !strings.Contains(r.Summary, "Skipped") || !strings.Contains(r.Summary, "Updater") {
		t.Errorf("the summary doesn't say what was skipped: %q", r.Summary)
	}
	if len(r.Jobs) != 1 {
		t.Errorf("after the first job found no room, %d jobs were tried", len(r.Jobs))
	}
}
