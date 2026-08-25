package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// farScript writes a script action whose schedule is half a day away, so only
// a wake can make it fire inside a test.
func farScript(t *testing.T, d *daemon, name, command string, extra string) {
	t.Helper()
	dir := d.paths.ActionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	farHour := (time.Now().Hour() + 12) % 24
	writeAction(t, dir, name+".toml", fmt.Sprintf(
		"name = %q\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = %q\n%s[schedule]\nhours = [%d]\n",
		name, command, extra, farHour))
}

// dueScript writes a script action whose schedule fired two minutes ago (inside
// catchUpGrace whatever the clock reads), so the next tick finds it due.
func dueScript(t *testing.T, d *daemon, name, command string) {
	t.Helper()
	dir := d.paths.ActionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	occurrence := time.Now().Add(-2 * time.Minute)
	writeAction(t, dir, name+".toml", fmt.Sprintf(
		"name = %q\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = %q\n[schedule]\nhours = [%d]\nminute = %d\n",
		name, command, occurrence.Hour(), occurrence.Minute()))
}

func waitForRuns(t *testing.T, d *daemon, want int, within time.Duration) []runRecord {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		recs := readRunLog(t, d.paths.RunLogFile())
		if len(recs) >= want {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("wanted %d run record(s) within %s, got %+v", want, within, recs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWakeFiresOutsideSchedule(t *testing.T) {
	d := testDaemon(t)
	farScript(t, d, "nightly", "echo woke", "")
	if err := requestWake(d.paths.StateDir, "nightly", "test"); err != nil {
		t.Fatal(err)
	}
	d.tick()
	recs := waitForRuns(t, d, 1, 5*time.Second)
	if recs[0].Trigger != triggerWake || recs[0].Status != "completed" || recs[0].Detail != "woke" {
		t.Fatalf("expected one completed wake record, got %+v", recs)
	}
	if wakePending(d.paths.StateDir, "nightly") {
		t.Error("the wake file should be consumed by the run")
	}
}

func TestWakeWaitsForARunningAction(t *testing.T) {
	d := testDaemon(t)
	farScript(t, d, "nightly", "echo woke", "")
	release, ok, err := tryRunLock(d.paths.StateDir, "nightly")
	if err != nil || !ok {
		t.Fatalf("lock setup: ok=%v err=%v", ok, err)
	}
	if err := requestWake(d.paths.StateDir, "nightly", "test"); err != nil {
		t.Fatal(err)
	}
	d.tick()
	d.tick()
	time.Sleep(150 * time.Millisecond)
	if recs := readRunLog(t, d.paths.RunLogFile()); len(recs) != 0 {
		t.Fatalf("a wake must not fire over a running run, got %+v", recs)
	}
	if !wakePending(d.paths.StateDir, "nightly") {
		t.Fatal("the wake should stay queued while the run lock is held")
	}
	release()
	d.tick()
	recs := waitForRuns(t, d, 1, 5*time.Second)
	if recs[0].Trigger != triggerWake {
		t.Fatalf("expected the queued wake to fire once the lock was released, got %+v", recs)
	}
}

func TestWakeDebouncesWithinTwentySeconds(t *testing.T) {
	dir := t.TempDir()
	if err := requestWake(dir, "nightly", "first"); err != nil {
		t.Fatal(err)
	}
	if err := requestWake(dir, "nightly", "second"); !errors.Is(err, errWakeDebounced) {
		t.Fatalf("second request inside the debounce should be refused, got %v", err)
	}
	entries, err := os.ReadDir(wakeDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one wake file, got %d", len(entries))
	}
	source, err := consumeWake(dir, "nightly")
	if err != nil || source != "first" {
		t.Fatalf("the first request's source should survive the debounced one: %q %v", source, err)
	}
	// An old leftover (past the debounce) is replaced, not refused.
	if err := requestWake(dir, "nightly", "old"); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-wakeDebounce - time.Second)
	if err := os.Chtimes(wakePath(dir, "nightly"), past, past); err != nil {
		t.Fatal(err)
	}
	if err := requestWake(dir, "nightly", "fresh"); err != nil {
		t.Fatalf("a request after the debounce window should replace the file, got %v", err)
	}
}

func TestWakeIgnoredWhenDisabled(t *testing.T) {
	d := testDaemon(t)
	farScript(t, d, "paused", "echo woke", "enabled = false\n")
	if err := requestWake(d.paths.StateDir, "paused", "test"); err != nil {
		t.Fatal(err)
	}
	d.tick()
	time.Sleep(150 * time.Millisecond)
	if recs := readRunLog(t, d.paths.RunLogFile()); len(recs) != 0 {
		t.Fatalf("a disabled action must not fire on a wake, got %+v", recs)
	}
	if got := d.state.lastRun("paused"); !got.IsZero() {
		t.Errorf("a disabled action must not be stamped, got %s", got)
	}
}

func TestWakeRestampsHeartbeat(t *testing.T) {
	fake := &scriptedHerdr{waits: []waitStep{{state: "working"}, {state: "done"}}}
	d := agentTestDaemon(t, fake)
	dir := d.paths.ActionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, dir, "beat.toml", "name = \"beat\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[heartbeat]\ninterval_minutes = 60\n")
	// Ran a minute ago: not due for another 59 minutes.
	before := time.Now().Add(-time.Minute)
	d.state.setLastRun("beat", before)
	if err := requestWake(d.paths.StateDir, "beat", "chat"); err != nil {
		t.Fatal(err)
	}
	d.tick()
	recs := waitForRuns(t, d, 2, 5*time.Second)
	if recs[0].Status != "started" || recs[0].Trigger != triggerWake {
		t.Fatalf("expected a started record with trigger wake first, got %+v", recs)
	}
	if recs[1].Status != "completed" || recs[1].Trigger != triggerWake {
		t.Fatalf("expected a completed wake record, got %+v", recs)
	}
	if got := d.state.lastRun("beat"); !got.After(before) {
		t.Errorf("a woken heartbeat counts as the beat: lastRun should move past %s, got %s", before, got)
	}
}

func TestWakeFileConsumedOnce(t *testing.T) {
	d := testDaemon(t)
	farScript(t, d, "nightly", "sleep 0.3; echo woke", "")
	if err := requestWake(d.paths.StateDir, "nightly", "test"); err != nil {
		t.Fatal(err)
	}
	d.tick()
	d.tick()
	waitForRuns(t, d, 1, 5*time.Second)
	time.Sleep(600 * time.Millisecond)
	d.tick()
	time.Sleep(600 * time.Millisecond)
	recs := readRunLog(t, d.paths.RunLogFile())
	terminal := 0
	for _, r := range recs {
		if r.Status == "completed" {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("one wake must run exactly once, got %d completed records: %+v", terminal, recs)
	}
}

func TestScheduledFireAbsorbsWake(t *testing.T) {
	// A wake queued just before an occurrence must not buy a second full run:
	// the scheduled run serves it, says so in the history, and consumes the
	// ledger's wake file, while its own schedule stamp still advances.
	d := testDaemon(t)
	dueScript(t, d, "pulse", "echo beat")
	before := time.Now().Add(-2 * time.Hour)
	d.state.setLastRun("pulse", before)
	if err := requestWake(d.paths.StateDir, "pulse", "chat"); err != nil {
		t.Fatal(err)
	}
	d.tick()
	recs := waitForRuns(t, d, 1, 5*time.Second)
	if recs[0].Trigger != triggerWake || recs[0].Status != "completed" || recs[0].Detail != "beat" {
		t.Fatalf("the scheduled run should be recorded as serving the wake, got %+v", recs)
	}
	if wakePending(d.paths.StateDir, "pulse") {
		t.Error("the scheduled run should have consumed the wake file")
	}
	if got := d.state.lastRun("pulse"); !got.After(before) {
		t.Errorf("the schedule stamp should advance as for a scheduled run, got %s", got)
	}
	// Nothing is left to fire: no wake-only run behind the scheduled one.
	d.tick()
	time.Sleep(300 * time.Millisecond)
	if recs := readRunLog(t, d.paths.RunLogFile()); len(recs) != 1 {
		t.Fatalf("a due action with a pending wake must fire exactly once, got %+v", recs)
	}
}

func TestStaleWakeDropped(t *testing.T) {
	d := testDaemon(t)
	farScript(t, d, "nightly", "echo woke", "")
	if err := requestWake(d.paths.StateDir, "nightly", "test"); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(wakePath(d.paths.StateDir, "nightly"), past, past); err != nil {
		t.Fatal(err)
	}
	d.tick()
	time.Sleep(150 * time.Millisecond)
	if recs := readRunLog(t, d.paths.RunLogFile()); len(recs) != 0 {
		t.Fatalf("a wake older than an hour must be dropped, got %+v", recs)
	}
	if _, err := os.Stat(wakePath(d.paths.StateDir, "nightly")); !os.IsNotExist(err) {
		t.Error("the stale wake file should have been removed")
	}
}

func TestPruneWakesDropsDeletedActions(t *testing.T) {
	dir := t.TempDir()
	if err := requestWake(dir, "gone", "test"); err != nil {
		t.Fatal(err)
	}
	if err := requestWake(dir, "kept", "test"); err != nil {
		t.Fatal(err)
	}
	pruneWakes(dir, map[string]bool{"kept": true})
	if _, err := os.Stat(filepath.Join(wakeDir(dir), "gone.wake")); !os.IsNotExist(err) {
		t.Error("a wake for a deleted action should be pruned")
	}
	if !wakePending(dir, "kept") {
		t.Error("a wake for a live action must survive pruning")
	}
}

func TestLaunchAgentWorkspaceInjectsTrigger(t *testing.T) {
	fake := &scriptedHerdr{}
	if _, _, err := launchAgentWorkspace(fake, watchedAction(), 0, triggerWake); err != nil {
		t.Fatal(err)
	}
	if fake.env["SHEPHERD_TRIGGER"] != triggerWake || fake.env["SHEPHERD_ACTION"] != "nightly-report" {
		t.Fatalf("the pane should learn its trigger and action, got %v", fake.env)
	}
}
