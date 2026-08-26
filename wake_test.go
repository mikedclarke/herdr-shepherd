package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
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

// wakeCLI runs the wake command exactly as main does, against the daemon's own
// directories, and returns what it printed on stdout.
func wakeCLI(t *testing.T, d *daemon, args ...string) (string, error) {
	t.Helper()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", d.paths.ConfigDir)
	t.Setenv("HERDR_PLUGIN_STATE_DIR", d.paths.StateDir)
	out, err := os.CreateTemp(t.TempDir(), "wake-out-*")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = out
	at, err := parseWakeAt(args[1:])
	if err == nil {
		err = cmdWake(args[0], at)
	}
	os.Stdout = saved
	out.Close()
	printed, readErr := os.ReadFile(out.Name())
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(printed), err
}

func TestScheduledWakeFiresAtItsInstant(t *testing.T) {
	d := testDaemon(t)
	farScript(t, d, "nightly", "echo woke", "")
	at := time.Now().Add(1 * time.Second)
	if err := scheduleWake(d.paths.StateDir, "nightly", at, "calendar"); err != nil {
		t.Fatal(err)
	}
	d.tick()
	time.Sleep(150 * time.Millisecond)
	if recs := readRunLog(t, d.paths.RunLogFile()); len(recs) != 0 {
		t.Fatalf("a scheduled wake must not fire before its instant, got %+v", recs)
	}
	if _, err := os.Stat(wakeAtPath(d.paths.StateDir, "nightly")); err != nil {
		t.Fatalf("the schedule should still be waiting for its instant: %v", err)
	}
	time.Sleep(time.Until(at) + 50*time.Millisecond)
	d.tick()
	recs := waitForRuns(t, d, 1, 5*time.Second)
	if recs[0].Trigger != triggerWake || recs[0].Status != "completed" || recs[0].Detail != "woke" {
		t.Fatalf("expected one completed wake record, got %+v", recs)
	}
	if _, err := os.Stat(wakeAtPath(d.paths.StateDir, "nightly")); !os.IsNotExist(err) {
		t.Error("the promoted schedule file should have been removed")
	}
}

func TestScheduledWakeReplacesTheEarlier(t *testing.T) {
	dir := t.TempDir()
	first := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	second := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	if err := scheduleWake(dir, "pulse", first, "calendar"); err != nil {
		t.Fatal(err)
	}
	if err := scheduleWake(dir, "pulse", second, "calendar"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(wakeDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one scheduled wake file, got %d", len(entries))
	}
	at, source, err := readScheduledWake(wakeAtPath(dir, "pulse"))
	if err != nil {
		t.Fatal(err)
	}
	if !at.Equal(second) || source != "calendar" {
		t.Fatalf("the latest schedule should win: got %s %q, want %s", at, source, second)
	}
}

func TestScheduledWakePastInstantQueuesNow(t *testing.T) {
	d := testDaemon(t)
	farScript(t, d, "nightly", "echo woke", "")
	out, err := wakeCLI(t, d, "nightly", "--at", time.Now().Add(-time.Minute).Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "queued: nightly") {
		t.Fatalf("an instant just past should queue a wake now, printed %q", out)
	}
	if !wakePending(d.paths.StateDir, "nightly") {
		t.Error("the wake should be queued for the next tick")
	}
	if _, err := os.Stat(wakeAtPath(d.paths.StateDir, "nightly")); !os.IsNotExist(err) {
		t.Error("nothing should have been scheduled for an instant already past")
	}
	// Past the hour the shepherd gives up on any wake, so the CLI says so.
	_, err = wakeCLI(t, d, "nightly", "--at", time.Now().Add(-2*time.Hour).Format(time.RFC3339))
	if err == nil || !strings.Contains(err.Error(), "min in the past") {
		t.Fatalf("an instant older than the wake's lifetime should error, got %v", err)
	}
}

func TestScheduledWakeStaleDropped(t *testing.T) {
	logs, err := os.CreateTemp(t.TempDir(), "log-*")
	if err != nil {
		t.Fatal(err)
	}
	log.SetOutput(logs)
	defer log.SetOutput(os.Stderr)
	d := testDaemon(t)
	farScript(t, d, "nightly", "echo woke", "")
	// Written by hand: scheduleWake refuses an instant this old outright, and
	// only a daemon that was down for two hours leaves one behind.
	if err := os.MkdirAll(wakeDir(d.paths.StateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.WriteFile(wakeAtPath(d.paths.StateDir, "nightly"),
		[]byte(past.Format(time.RFC3339)+"\ncalendar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.tick()
	time.Sleep(150 * time.Millisecond)
	if recs := readRunLog(t, d.paths.RunLogFile()); len(recs) != 0 {
		t.Fatalf("a schedule two hours past its instant must be dropped, got %+v", recs)
	}
	if _, err := os.Stat(wakeAtPath(d.paths.StateDir, "nightly")); !os.IsNotExist(err) {
		t.Error("the stale schedule file should have been removed")
	}
	if wakePending(d.paths.StateDir, "nightly") {
		t.Error("a dropped schedule must not leave a wake behind")
	}
	logs.Close()
	written, err := os.ReadFile(logs.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "nightly: scheduled wake dropped, 120 min past its instant") {
		t.Fatalf("the drop should be logged with its lateness, got %q", written)
	}
}

func TestScheduledWakeAbsorbedByDueRun(t *testing.T) {
	// The same rule a chat wake gets: an occurrence that comes due on the tick
	// that promotes the schedule serves it, and buys no second run.
	d := testDaemon(t)
	dueScript(t, d, "pulse", "echo beat")
	before := time.Now().Add(-2 * time.Hour)
	d.state.setLastRun("pulse", before)
	if err := scheduleWake(d.paths.StateDir, "pulse", time.Now().Add(-time.Second), "calendar"); err != nil {
		t.Fatal(err)
	}
	d.tick()
	recs := waitForRuns(t, d, 1, 5*time.Second)
	if recs[0].Trigger != triggerWake || recs[0].Status != "completed" || recs[0].Detail != "beat" {
		t.Fatalf("the due run should be recorded as serving the scheduled wake, got %+v", recs)
	}
	if wakePending(d.paths.StateDir, "pulse") {
		t.Error("the due run should have consumed the promoted wake")
	}
	d.tick()
	time.Sleep(300 * time.Millisecond)
	if recs := readRunLog(t, d.paths.RunLogFile()); len(recs) != 1 {
		t.Fatalf("a promoted schedule absorbed by a due run must fire once, got %+v", recs)
	}
}

func TestScheduledWakePrunedWithAction(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(time.Hour)
	if err := scheduleWake(dir, "gone", at, "calendar"); err != nil {
		t.Fatal(err)
	}
	if err := scheduleWake(dir, "kept", at, "calendar"); err != nil {
		t.Fatal(err)
	}
	pruneWakes(dir, map[string]bool{"kept": true})
	if _, err := os.Stat(wakeAtPath(dir, "gone")); !os.IsNotExist(err) {
		t.Error("a schedule for a deleted action should be pruned")
	}
	if _, err := os.Stat(wakeAtPath(dir, "kept")); err != nil {
		t.Errorf("a schedule for a live action must survive pruning: %v", err)
	}
}

func TestScheduledWakeIgnoredWhenDisabled(t *testing.T) {
	d := testDaemon(t)
	farScript(t, d, "paused", "echo woke", "enabled = false\n")
	if _, err := wakeCLI(t, d, "paused", "--at", time.Now().Add(time.Hour).Format(time.RFC3339)); err == nil ||
		!strings.Contains(err.Error(), "paused is disabled") {
		t.Fatalf("a disabled action should refuse a schedule, got %v", err)
	}
	if _, err := os.Stat(wakeAtPath(d.paths.StateDir, "paused")); !os.IsNotExist(err) {
		t.Fatal("a refused schedule must write no file")
	}
	// One written behind the CLI's back is promoted, but the action loop still
	// leaves a disabled action alone.
	if err := scheduleWake(d.paths.StateDir, "paused", time.Now().Add(-time.Second), "calendar"); err != nil {
		t.Fatal(err)
	}
	d.tick()
	time.Sleep(150 * time.Millisecond)
	if recs := readRunLog(t, d.paths.RunLogFile()); len(recs) != 0 {
		t.Fatalf("a disabled action must not fire on a scheduled wake, got %+v", recs)
	}
	if got := d.state.lastRun("paused"); !got.IsZero() {
		t.Errorf("a disabled action must not be stamped, got %s", got)
	}
}

func TestWakeAtFlagParses(t *testing.T) {
	local := func(y int, mo time.Month, d, h, m, s int) time.Time {
		return time.Date(y, mo, d, h, m, s, 0, time.Local)
	}
	cases := []struct {
		name string
		args []string
		want time.Time
	}{
		{"no flag", nil, time.Time{}},
		{"rfc3339", []string{"--at", "2027-01-06T14:05:00Z"}, time.Date(2027, time.January, 6, 14, 5, 0, 0, time.UTC)},
		{"date and time", []string{"--at", "2027-01-06 14:05:00"}, local(2027, time.January, 6, 14, 5, 0)},
		{"no seconds", []string{"--at", "2027-01-06 14:05"}, local(2027, time.January, 6, 14, 5, 0)},
	}
	for _, tc := range cases {
		got, err := parseWakeAt(tc.args)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
	for _, bad := range [][]string{{"--at", "half past two"}, {"--at", "6 Jan 2027"}, {"--at"}} {
		if _, err := parseWakeAt(bad); err == nil {
			t.Errorf("%v should not parse as an instant", bad)
		}
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
