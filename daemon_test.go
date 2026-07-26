package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testDaemon(t *testing.T, started time.Time) *daemon {
	t.Helper()
	return &daemon{
		state:   &daemonState{Actions: map[string]*actionState{}},
		started: started,
		running: map[string]bool{},
	}
}

func routineAction(name string) *Action {
	a := &Action{
		Name: name, Kind: KindRoutine, Directory: "/tmp", Prompt: "x",
		Routine: RoutineSpec{Preset: "weekdays", Hours: []int{6}, Minute: 15, MonthDay: 1},
	}
	return a
}

func TestDueRoutineFiresInsideGrace(t *testing.T) {
	// Monday 06:15 occurrence, checked at 06:15:20 (one tick late).
	now := mustTime(t, "2026-07-27 06:15").Add(20 * time.Second)
	d := testDaemon(t, mustTime(t, "2026-07-20 12:00"))
	a := routineAction("digest")
	d.state.setLastRun("digest", mustTime(t, "2026-07-24 06:15"))
	fire, stamp := d.due(a, now)
	if !fire {
		t.Fatal("routine should fire just after its minute")
	}
	if want := mustTime(t, "2026-07-27 06:15"); !stamp.Equal(want) {
		t.Errorf("stamp should be the occurrence time, got %s", stamp)
	}
}

func TestDueRoutineDropsMissedOccurrenceAfterSleep(t *testing.T) {
	// Daemon alive since the 20th, last ran Friday 06:15, machine slept
	// through Monday 06:15 and woke at 08:55: the occurrence is consumed
	// without firing.
	now := mustTime(t, "2026-07-27 08:55")
	d := testDaemon(t, mustTime(t, "2026-07-20 12:00"))
	a := routineAction("digest")
	d.state.setLastRun("digest", mustTime(t, "2026-07-24 06:15"))
	fire, _ := d.due(a, now)
	if fire {
		t.Fatal("missed occurrence beyond grace must not fire")
	}
	if got, want := d.state.lastRun("digest"), mustTime(t, "2026-07-27 06:15"); !got.Equal(want) {
		t.Errorf("missed occurrence should be consumed, lastRun=%s", got)
	}
	// The following tick must not fire either.
	if fire, _ := d.due(a, now.Add(30*time.Second)); fire {
		t.Fatal("consumed occurrence fired on the next tick")
	}
}

func TestDueRoutineShortWakeStillFires(t *testing.T) {
	// Wake at 06:20 — five minutes late is within grace and should run.
	now := mustTime(t, "2026-07-27 06:20")
	d := testDaemon(t, mustTime(t, "2026-07-20 12:00"))
	a := routineAction("digest")
	d.state.setLastRun("digest", mustTime(t, "2026-07-24 06:15"))
	if fire, _ := d.due(a, now); !fire {
		t.Fatal("occurrence within the grace window should fire")
	}
}

func TestDueRoutineNoBackfillOnRestart(t *testing.T) {
	// Daemon started at 08:55 with no recorded runs: today's 06:15 must not fire.
	now := mustTime(t, "2026-07-27 08:55")
	d := testDaemon(t, now.Add(-30*time.Second))
	if fire, _ := d.due(routineAction("digest"), now); fire {
		t.Fatal("restart must not backfill earlier occurrences")
	}
}

func TestDueClockStepBackwards(t *testing.T) {
	now := mustTime(t, "2026-07-27 06:16")
	d := testDaemon(t, mustTime(t, "2026-07-20 12:00"))
	a := routineAction("digest")
	d.state.setLastRun("digest", now.Add(2*time.Hour))
	fire, _ := d.due(a, now)
	if fire {
		t.Fatal("should not fire immediately after a backwards clock step")
	}
	if d.state.lastRun("digest").After(now) {
		t.Error("future last-run stamp should be reset to now")
	}
}

func TestDueHeartbeat(t *testing.T) {
	d := testDaemon(t, mustTime(t, "2026-07-20 12:00"))
	a := &Action{
		Name: "hb", Kind: KindHeartbeat, Directory: "/tmp", Prompt: "x",
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
	d.state.setLastRun("hb", mustTime(t, "2026-07-27 09:30"))
	if fire, _ := d.due(a, mustTime(t, "2026-07-27 10:00")); fire {
		t.Error("heartbeat before interval elapsed")
	}
	if fire, _ := d.due(a, mustTime(t, "2026-07-27 10:30")); !fire {
		t.Error("heartbeat after interval elapsed")
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
