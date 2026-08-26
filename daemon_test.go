package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func testDaemon(t *testing.T) *daemon {
	t.Helper()
	dir := t.TempDir()
	return &daemon{
		paths:          paths{ConfigDir: dir, StateDir: dir},
		client:         &scriptedHerdr{},
		state:          emptyState(),
		startTimeout:   time.Minute,
		pause:          time.Millisecond,
		resend:         time.Minute,
		notifiedErrors: map[string]bool{},
		startFailures:  map[string]int{},
	}
}

func routineAction(name string) *Action {
	return &Action{
		Name: name, Kind: KindRoutine, Directory: "/tmp", Prompt: "x",
		Routine: RoutineSpec{Preset: "weekdays", Hours: []int{6}, Minute: 15, MonthDay: 1},
	}
}

func TestDueRoutineFiresInsideGrace(t *testing.T) {
	// Monday 06:15 occurrence, checked at 06:15:20 (one tick late).
	now := mustTime(t, "2026-07-27 06:15").Add(20 * time.Second)
	d := testDaemon(t)
	a := routineAction("nightly-report")
	d.state.setLastRun(a.Name, mustTime(t, "2026-07-24 06:15"))
	fire, stamp := d.due(a, now)
	if !fire {
		t.Fatal("routine should fire just after its minute")
	}
	if want := mustTime(t, "2026-07-27 06:15"); !stamp.Equal(want) {
		t.Errorf("stamp should be the occurrence time, got %s", stamp)
	}
}

func TestDueRoutineDropsMissedOccurrenceAfterSleep(t *testing.T) {
	// Last ran Friday 06:15; the machine slept through Monday 06:15 and woke
	// at 08:55. The anchor clamp skips straight to the next occurrence: no
	// fire, no walk through the missed one, and the same answer every tick.
	now := mustTime(t, "2026-07-27 08:55")
	d := testDaemon(t)
	a := routineAction("nightly-report")
	last := mustTime(t, "2026-07-24 06:15")
	d.state.setLastRun(a.Name, last)
	for i := 0; i < 3; i++ {
		if fire, _ := d.due(a, now.Add(time.Duration(i)*30*time.Second)); fire {
			t.Fatalf("missed occurrence beyond grace must not fire (tick %d)", i)
		}
	}
	if got := d.state.lastRun(a.Name); !got.Equal(last) {
		t.Errorf("a skipped occurrence must not be stamped, lastRun=%s", got)
	}
}

func TestDueRoutineShortWakeStillFires(t *testing.T) {
	// Wake at 06:20 — five minutes late is within grace and should run.
	now := mustTime(t, "2026-07-27 06:20")
	d := testDaemon(t)
	a := routineAction("nightly-report")
	d.state.setLastRun(a.Name, mustTime(t, "2026-07-24 06:15"))
	if fire, _ := d.due(a, now); !fire {
		t.Fatal("occurrence within the grace window should fire")
	}
}

func TestDueRoutineNoBackfillOnRestart(t *testing.T) {
	// A restart with no recorded runs must not backfill: 08:55 is well past
	// the 06:15 occurrence, but a restart inside the grace window still runs
	// it, exactly as a daemon that had been up would have.
	d := testDaemon(t)
	a := routineAction("nightly-report")
	if fire, _ := d.due(a, mustTime(t, "2026-07-27 08:55")); fire {
		t.Fatal("restart must not backfill earlier occurrences")
	}
	if fire, stamp := d.due(a, mustTime(t, "2026-07-27 06:17")); !fire {
		t.Errorf("restart inside the grace window should still fire, stamp=%s", stamp)
	}
}

func TestDueRoutineNewActionFiresNextOccurrence(t *testing.T) {
	// An action added to a long-running daemon has no last run at all; it
	// must fire on its next occurrence rather than after a walk.
	d := testDaemon(t)
	a := routineAction("nightly-report")
	if fire, _ := d.due(a, mustTime(t, "2026-07-27 05:00")); fire {
		t.Fatal("a new action must not fire before its first occurrence")
	}
	fire, stamp := d.due(a, mustTime(t, "2026-07-27 06:15").Add(20*time.Second))
	if !fire {
		t.Fatal("a new action should fire on its next occurrence")
	}
	if want := mustTime(t, "2026-07-27 06:15"); !stamp.Equal(want) {
		t.Errorf("stamp should be the occurrence time, got %s", stamp)
	}
}

func TestDueClockStepBackwards(t *testing.T) {
	now := mustTime(t, "2026-07-27 06:16")
	d := testDaemon(t)
	a := routineAction("nightly-report")
	d.state.setLastRun(a.Name, now.Add(2*time.Hour))
	fire, _ := d.due(a, now)
	if fire {
		t.Fatal("should not fire immediately after a backwards clock step")
	}
	if d.state.lastRun(a.Name).After(now) {
		t.Error("future last-run stamp should be reset to now")
	}
}

func TestDueHeartbeat(t *testing.T) {
	d := testDaemon(t)
	a := &Action{
		Name: "hourly-check", Kind: KindHeartbeat, Directory: "/tmp", Prompt: "x",
		Heartbeat: HeartbeatSpec{
			IntervalMinutes: 60,
			WorkingHours:    &WorkingHours{Days: []int{1, 2, 3, 4, 5}, StartHour: 9, EndHour: 16},
		},
	}
	if fire, _ := d.due(a, mustTime(t, "2026-07-26 10:00")); fire {
		t.Error("Sunday is outside working hours")
	}
	if fire, _ := d.due(a, mustTime(t, "2026-07-27 09:30")); !fire {
		t.Error("never-run heartbeat should fire inside working hours")
	}
	d.state.setLastRun(a.Name, mustTime(t, "2026-07-27 09:30"))
	if fire, _ := d.due(a, mustTime(t, "2026-07-27 10:00")); fire {
		t.Error("heartbeat before interval elapsed")
	}
	if fire, _ := d.due(a, mustTime(t, "2026-07-27 10:30")); !fire {
		t.Error("heartbeat after interval elapsed")
	}
}

func TestNextRunMatchesDue(t *testing.T) {
	// The board's NEXT RUN column and the daemon's due check share one
	// computation, so a due occurrence never displays as already past.
	now := mustTime(t, "2026-07-27 06:17")
	d := testDaemon(t)
	a := routineAction("nightly-report")
	a.applyDefaults()
	fire, stamp := d.due(a, now)
	if !fire {
		t.Fatal("occurrence inside grace should fire")
	}
	if got := nextRun(a, time.Time{}, now); !got.Equal(stamp) {
		t.Errorf("nextRun %s should match the firing occurrence %s", got, stamp)
	}
	disabled := false
	a.Enabled = &disabled
	if got := nextRun(a, time.Time{}, now); !got.IsZero() {
		t.Errorf("a disabled action has no next run, got %s", got)
	}
}

func TestTickSkipsDisabledAndRunningActions(t *testing.T) {
	d := testDaemon(t)
	dir := d.paths.ActionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, dir, "off.toml", "name = \"paused\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\nenabled = false\n[schedule]\npreset = \"cron\"\ncron = \"* * * * *\"\n")
	writeAction(t, dir, "busy.toml", "name = \"busy\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\n[schedule]\npreset = \"cron\"\ncron = \"* * * * *\"\n")
	release, ok, err := tryRunLock(d.paths.StateDir, "busy")
	if err != nil || !ok {
		t.Fatalf("lock setup: ok=%v err=%v", ok, err)
	}
	defer release()

	d.tick()
	if got := d.state.lastRun("paused"); !got.IsZero() {
		t.Errorf("a disabled action must not be stamped, got %s", got)
	}
	if got := d.state.lastRun("busy"); !got.IsZero() {
		t.Errorf("an action already running must not be stamped, got %s", got)
	}
	if recs := readRunLog(t, d.paths.RunLogFile()); len(recs) != 0 {
		t.Errorf("no run should have happened, got %+v", recs)
	}
}

func TestTickKeepsStateWhileAFileIsBroken(t *testing.T) {
	// Pruning on a tick that could not read every file drops the schedule of
	// whatever failed to parse, and the heartbeat fires again the moment the
	// file is fixed.
	d := testDaemon(t)
	d.startTimeout = 10 * time.Millisecond
	dir := d.paths.ActionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ran := time.Now()
	d.state.setLastRun("hourly-check", ran)
	d.state.setLastRun("gone", mustTime(t, "2026-07-27 06:15"))

	// The heartbeat's own file is the unreadable one.
	writeAction(t, dir, "check.toml", "name = \"hourly-check\nkind=")
	d.tick()
	if got := d.state.lastRun("hourly-check"); !got.Equal(ran) {
		t.Fatalf("state must survive a tick with a parse error, got %s", got)
	}
	if got := d.state.lastRun("gone"); got.IsZero() {
		t.Fatal("no state may be pruned on a tick with a parse error")
	}

	writeAction(t, dir, "check.toml", "name = \"hourly-check\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[heartbeat]\ninterval_minutes = 1440\n")
	d.tick()
	if got := d.state.lastRun("hourly-check"); !got.Equal(ran) {
		t.Errorf("the heartbeat should not be due again, got %s", got)
	}
	if got := d.state.lastRun("gone"); !got.IsZero() {
		t.Errorf("a clean tick should prune state for actions that are gone, got %s", got)
	}
}

func TestTickNotifiesEachConfigErrorOnce(t *testing.T) {
	d := testDaemon(t)
	fake := d.client.(*scriptedHerdr)
	dir := d.paths.ActionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, dir, "broken.toml", "name = \"broken\nkind=")

	d.tick()
	d.tick()
	if _, _, notices, _ := fake.counts(); notices != 1 {
		t.Fatalf("a repeated config error should notify once, got %v", fake.notices)
	}
	writeAction(t, dir, "broken.toml", "name = \"fixed\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\nenabled = false\n")
	d.tick()
	writeAction(t, dir, "broken.toml", "name = \"broken\nkind=")
	d.tick()
	if _, _, notices, _ := fake.counts(); notices != 2 {
		t.Fatalf("an error that comes back after a fix should notify again, got %v", fake.notices)
	}
}

func scriptAction(name, command string) *Action {
	a := &Action{Name: name, Kind: KindScript, Directory: "/tmp", Command: command}
	a.applyDefaults()
	return a
}

func TestFireRecordsScriptRun(t *testing.T) {
	d := testDaemon(t)
	a := scriptAction("build-sync", "echo done")
	d.fire(a, time.Time{}, "", true)
	if got := d.state.lastStatus(a.Name); got != "completed" {
		t.Fatalf("got status %q", got)
	}
	recs := readRunLog(t, d.paths.RunLogFile())
	if len(recs) != 1 || recs[0].Status != "completed" || recs[0].Detail != "done" {
		t.Fatalf("expected one completed record with the script output, got %+v", recs)
	}
	if recs[0].DurationSecs <= 0 {
		t.Errorf("a finished run must record its duration, got %v", recs[0].DurationSecs)
	}
	if runLockHeld(d.paths.StateDir, a.Name) {
		t.Error("the run lock must be released once the run finishes")
	}
}

func TestFireRecordsDeferredScriptWithoutNotifying(t *testing.T) {
	// Exit 75 means "retry later"; with no retry window it is recorded as
	// deferred and, unlike an error, raises no notification.
	d := testDaemon(t)
	fake := d.client.(*scriptedHerdr)
	a := scriptAction("email-triage", "echo slot busy; exit 75")
	d.fire(a, time.Time{}, "", true)
	if got := d.state.lastStatus(a.Name); got != "deferred" {
		t.Fatalf("got status %q", got)
	}
	recs := readRunLog(t, d.paths.RunLogFile())
	if len(recs) != 1 || recs[0].Status != "deferred" || recs[0].Detail != "slot busy" {
		t.Fatalf("expected one deferred record with the script output, got %+v", recs)
	}
	if _, _, notices, _ := fake.counts(); notices != 0 {
		t.Errorf("a deferral must not notify, got %v", fake.notices)
	}
	if d.deferPending(a.Name) {
		t.Error("no retry window means no pending retry")
	}
}

func TestFireDeferredScriptRetriesInsideItsWindow(t *testing.T) {
	// First run defers and leaves a retry pending; only that first deferral is
	// recorded. The retry that succeeds records completed and ends the spell.
	d := testDaemon(t)
	dir := t.TempDir()
	a := scriptAction("email-triage", "if [ -e ready ]; then echo ok; else exit 75; fi")
	a.Directory = dir
	a.DeferRetryMinutes = 30

	d.fire(a, time.Time{}, "", true)
	if !d.deferPending(a.Name) {
		t.Fatal("a deferral inside the window should leave a retry pending")
	}
	d.fire(a, time.Time{}, "", true) // still deferring: no new record
	if recs := readRunLog(t, d.paths.RunLogFile()); len(recs) != 1 {
		t.Fatalf("retries that defer again must not append history, got %+v", recs)
	}

	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d.fire(a, time.Time{}, "", true)
	if got := d.state.lastStatus(a.Name); got != "completed" {
		t.Fatalf("the retry should complete, got %q", got)
	}
	if d.deferPending(a.Name) {
		t.Error("a completed run must end the deferral spell")
	}
	recs := readRunLog(t, d.paths.RunLogFile())
	if len(recs) != 2 || recs[0].Status != "deferred" || recs[1].Status != "completed" {
		t.Fatalf("expected deferred then completed, got %+v", recs)
	}
}

func TestFireDeferredScriptExpiresAfterItsWindow(t *testing.T) {
	d := testDaemon(t)
	fake := d.client.(*scriptedHerdr)
	a := scriptAction("email-triage", "exit 75")
	a.DeferRetryMinutes = 1
	d.mu.Lock()
	d.deferredSince = map[string]time.Time{a.Name: time.Now().Add(-2 * time.Minute)}
	d.mu.Unlock()

	d.fire(a, time.Time{}, "", true)
	if got := d.state.lastStatus(a.Name); got != "deferred-expired" {
		t.Fatalf("a deferral past its window is final, got %q", got)
	}
	if d.deferPending(a.Name) {
		t.Error("an expired deferral must clear the pending retry")
	}
	if _, _, notices, _ := fake.counts(); notices != 1 {
		t.Errorf("a run that never happened should notify once, got %v", fake.notices)
	}
	recs := readRunLog(t, d.paths.RunLogFile())
	if len(recs) != 1 || recs[0].Status != "deferred-expired" {
		t.Fatalf("expected one deferred-expired record, got %+v", recs)
	}
}

func TestTickRetriesAPendingDeferral(t *testing.T) {
	// The schedule itself is not due (a daily hour half a day away), but a
	// pending deferral makes the tick fire the action again.
	d := testDaemon(t)
	dir := d.paths.ActionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	farHour := (time.Now().Hour() + 12) % 24
	writeAction(t, dir, "triage.toml", fmt.Sprintf("name = \"email-triage\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"echo ok\"\ndefer_retry_minutes = 30\n[schedule]\nhours = [%d]\n", farHour))
	d.mu.Lock()
	d.deferredSince = map[string]time.Time{"email-triage": time.Now()}
	d.mu.Unlock()
	d.state.setLastRun("email-triage", time.Now().Add(-time.Hour))

	d.tick()
	deadline := time.Now().Add(5 * time.Second)
	for d.state.lastStatus("email-triage") != "completed" {
		if time.Now().After(deadline) {
			t.Fatalf("the pending deferral was never retried, status %q", d.state.lastStatus("email-triage"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if d.deferPending("email-triage") {
		t.Error("the successful retry must clear the pending deferral")
	}
}

func TestFireSkipsAnActionAlreadyRunning(t *testing.T) {
	d := testDaemon(t)
	a := scriptAction("build-sync", "echo done")
	release, ok, err := tryRunLock(d.paths.StateDir, a.Name)
	if err != nil || !ok {
		t.Fatalf("lock setup: ok=%v err=%v", ok, err)
	}
	d.fire(a, time.Time{}, "", true)
	if got := d.state.lastStatus(a.Name); got != "" {
		t.Fatalf("a skipped run must not record a status, got %q", got)
	}
	if recs := readRunLog(t, d.paths.RunLogFile()); len(recs) != 0 {
		t.Fatalf("a skipped run must not append history, got %+v", recs)
	}
	release()
	d.fire(a, time.Time{}, "", true)
	if got := d.state.lastStatus(a.Name); got != "completed" {
		t.Fatalf("the action should run once the lock is free, got %q", got)
	}
}

func TestTickSkipsAnActionWhoseRecordedRunIsStillAlive(t *testing.T) {
	// The daemon that started the run is gone, so its flock is gone with it;
	// the pid the run recorded is what keeps the action from firing twice.
	d := testDaemon(t)
	dir := d.paths.ActionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, dir, "orphan.toml", "name = \"orphaned\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\n[schedule]\npreset = \"cron\"\ncron = \"* * * * *\"\n")
	lock, ok, err := openRunLock(d.paths.StateDir, "orphaned")
	if err != nil || !ok {
		t.Fatalf("lock setup: ok=%v err=%v", ok, err)
	}
	if err := lock.setPid(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	lock.release()

	d.tick()
	if got := d.state.lastRun("orphaned"); !got.IsZero() {
		t.Errorf("an action whose run outlived its daemon must not be stamped, got %s", got)
	}
	if recs := readRunLog(t, d.paths.RunLogFile()); len(recs) != 0 {
		t.Errorf("no second run should have happened, got %+v", recs)
	}
}

// blockingScript returns an action that runs until the returned release func is
// called, so a test can hold a run in flight for as long as it needs.
func blockingScript(t *testing.T, name string) (*Action, func()) {
	t.Helper()
	dir := t.TempDir()
	a := scriptAction(name, "while [ ! -e go ]; do sleep 0.02; done; echo done")
	a.Directory = dir
	return a, func() {
		if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o644); err != nil {
			t.Error(err)
		}
	}
}

func waitForRunning(t *testing.T, d *daemon, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(d.runningRuns()) != want {
		if time.Now().After(deadline) {
			t.Fatalf("expected %d run(s) in flight, got %v", want, d.runningRuns())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFireRecordsTheScriptChildPid(t *testing.T) {
	d := testDaemon(t)
	a, finish := blockingScript(t, "build-sync")
	d.startRun(a, time.Time{}, "", true)
	waitForRunning(t, d, 1)

	deadline := time.Now().Add(5 * time.Second)
	pid := 0
	for pid == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the run never recorded its child pid")
		}
		data, err := os.ReadFile(runLockPath(d.paths.StateDir, a.Name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if trimmed := strings.TrimSpace(string(data)); trimmed != "" {
			if pid, err = strconv.Atoi(trimmed); err != nil {
				t.Fatalf("the lock file should hold a pid, got %q", trimmed)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == os.Getpid() {
		t.Error("the recorded pid should be the run's child, not the daemon")
	}
	if !pidAlive(pid) {
		t.Errorf("the recorded child %d should be alive while the run is", pid)
	}

	finish()
	waitForRunning(t, d, 0)
	data, err := os.ReadFile(runLockPath(d.paths.StateDir, a.Name))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "" {
		t.Errorf("a finished run should clear its pid, got %q", got)
	}
}

func TestShutdownWaitsForAnInFlightRun(t *testing.T) {
	d := testDaemon(t)
	d.shutdownWait = 10 * time.Second
	a, finish := blockingScript(t, "build-sync")
	d.startRun(a, time.Time{}, "", true)
	waitForRunning(t, d, 1)

	returned := make(chan struct{})
	go func() {
		d.waitForRuns()
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("the shutdown wait returned while a run was still going")
	case <-time.After(200 * time.Millisecond):
	}

	finish()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the shutdown wait should return once the run finishes")
	}
	if got := d.state.lastStatus(a.Name); got != "completed" {
		t.Errorf("the run should have finished, status %q", got)
	}
}

func TestShutdownWaitEndsAtItsBound(t *testing.T) {
	// A run that outlives the bound is left running; the daemon still returns,
	// so the next one can take its lock.
	d := testDaemon(t)
	d.shutdownWait = 100 * time.Millisecond
	a, finish := blockingScript(t, "build-sync")
	d.startRun(a, time.Time{}, "", true)
	waitForRunning(t, d, 1)

	began := time.Now()
	d.waitForRuns()
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Errorf("the wait should end at its bound, took %s", elapsed)
	}
	if got := d.runningRuns(); len(got) != 1 {
		t.Errorf("the run should have been left running, got %v", got)
	}
	finish()
	waitForRunning(t, d, 0)
}

func TestFireReleasesLockAfterTimeoutKill(t *testing.T) {
	d := testDaemon(t)
	// Zero minutes is not reachable through applyDefaults; built directly it
	// makes the timeout path immediate.
	a := &Action{Name: "build-sync", Kind: KindScript, Directory: "/tmp", Command: "sleep 60"}
	d.fire(a, time.Time{}, "", true)
	if got := d.state.lastStatus(a.Name); got != "error" {
		t.Fatalf("a timed-out script should record an error, got %q", got)
	}
	if runLockHeld(d.paths.StateDir, a.Name) {
		t.Error("the run lock must be released after a timeout kill")
	}
}

func TestFireGivesTheOccurrenceBackWhileStartsFail(t *testing.T) {
	fake := &scriptedHerdr{createErr: errors.New("socket down")}
	d := agentTestDaemon(t, fake)
	a := watchedAction()
	prev := mustTime(t, "2026-07-27 06:15")
	stamp := mustTime(t, "2026-07-28 06:15")

	for attempt := 1; attempt < maxStartAttempts; attempt++ {
		d.state.setLastRun(a.Name, stamp)
		d.fire(a, prev, "", true)
		if got := d.state.lastRun(a.Name); !got.Equal(prev) {
			t.Fatalf("attempt %d should hand the occurrence back, lastRun=%s", attempt, got)
		}
		if got := d.state.lastStatus(a.Name); got != "" {
			t.Fatalf("attempt %d should not record a status, got %q", attempt, got)
		}
	}
	d.state.setLastRun(a.Name, stamp)
	d.fire(a, prev, "", true)
	if got := d.state.lastStatus(a.Name); got != "error" {
		t.Fatalf("the last attempt should record the failure, got %q", got)
	}
	if got := d.state.lastRun(a.Name); !got.Equal(stamp) {
		t.Errorf("the last attempt keeps the occurrence, lastRun=%s", got)
	}

	// A start failure count must not follow the action into its next run.
	fake.mu.Lock()
	fake.createErr = nil
	fake.waits = []waitStep{{state: "working"}, {state: "done"}}
	fake.mu.Unlock()
	d.fire(a, prev, "", true)
	d.mu.Lock()
	attempts := d.startFailures[a.Name]
	d.mu.Unlock()
	if attempts != 0 {
		t.Errorf("a successful run should clear the start-failure count, got %d", attempts)
	}
}

func TestFireStampsHeartbeatCompletion(t *testing.T) {
	// A heartbeat longer than its interval must not come due the moment it
	// finishes, so the stamp is the completion, not the start.
	fake := &scriptedHerdr{waits: []waitStep{{state: "working"}, {state: "done"}}}
	d := agentTestDaemon(t, fake)
	a := &Action{Name: "hourly-check", Kind: KindHeartbeat, Directory: "/tmp", Prompt: "x"}
	a.applyDefaults()
	started := time.Now()
	d.state.setLastRun(a.Name, mustTime(t, "2026-07-27 06:15"))
	d.fire(a, time.Time{}, "", true)
	if got := d.state.lastRun(a.Name); got.Before(started) {
		t.Errorf("heartbeat should be stamped with its completion, got %s", got)
	}
}

func TestSeedExamplesLoads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "actions")
	seedExamples(dir)
	actions, fileErrs, err := LoadActions(dir)
	if err != nil || len(fileErrs) != 0 {
		t.Fatalf("the seeded example must load cleanly: err=%v fileErrs=%v", err, fileErrs)
	}
	if len(actions) != 1 || actions[0].Name != "example-heartbeat" {
		t.Fatalf("got %d actions: %+v", len(actions), actions)
	}
	if actions[0].IsEnabled() {
		t.Error("the seeded example must arrive disabled")
	}
}

func TestRotateLog(t *testing.T) {
	defer log.SetOutput(os.Stderr)
	d := testDaemon(t)
	path := filepath.Join(t.TempDir(), "shepherd.log")
	d.openLog(path)
	defer d.closeLog()
	if _, err := d.logFile.Write(make([]byte, daemonLogMaxBytes+1)); err != nil {
		t.Fatal(err)
	}
	d.rotateLog()
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("the oversized log should be rotated: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatalf("a fresh log should be open: %v %+v", err, info)
	}
	log.Printf("after rotation")
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("logging should continue into the new file: %v", err)
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 8}
	b.Write([]byte("0123456789abcdef"))
	if got := b.String(); got != "89abcdef" {
		t.Errorf("got %q", got)
	}
	b2 := &tailBuffer{max: 64}
	for i := 0; i < 10; i++ {
		b2.Write([]byte("chunk-"))
	}
	if got := b2.String(); !strings.HasSuffix(got, "chunk-") || len(got) > 64 {
		t.Errorf("got %q", got)
	}
}

func TestTailStringKeepsRunesIntact(t *testing.T) {
	s := strings.Repeat("é", 20)
	got := tailString(s, 9)
	if !strings.HasPrefix(got, tailMarker) {
		t.Fatalf("a trimmed string should be marked: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("trimming split a rune: %q", got)
	}
	if got := tailString("short", 100); got != "short" {
		t.Errorf("a string inside the limit is returned as-is, got %q", got)
	}
}

func TestAcquireLockExcludesSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	release, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(path); err == nil {
		t.Fatal("second acquire should fail while the first holds the lock")
	}
	release()
	release2, err := acquireLock(path)
	if err != nil {
		t.Fatalf("lock should be reacquirable after release: %v", err)
	}
	release2()
}
