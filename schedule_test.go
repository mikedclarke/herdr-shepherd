package main

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestCronNext(t *testing.T) {
	cases := []struct {
		expr, after, want string
	}{
		{"0 6 * * 1-5", "2026-07-24 05:00", "2026-07-24 06:00"}, // Friday
		{"0 6 * * 1-5", "2026-07-24 06:00", "2026-07-27 06:00"}, // strictly after; skips weekend
		{"15 6 * * 1-5", "2026-07-26 12:00", "2026-07-27 06:15"},
		{"0 9,17 * * *", "2026-07-26 09:00", "2026-07-26 17:00"},
		{"30 8 1 * *", "2026-07-26 12:00", "2026-08-01 08:30"},
		{"*/15 * * * *", "2026-07-26 12:07", "2026-07-26 12:15"},
		{"0 0 * * 0", "2026-07-26 12:00", "2026-08-02 00:00"}, // 26th is a Sunday
		{"0 0 * * 7", "2026-07-26 12:00", "2026-08-02 00:00"}, // 7 == Sunday
	}
	for _, c := range cases {
		sched, err := parseCron(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		got, err := sched.Next(mustTime(t, c.after))
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if want := mustTime(t, c.want); !got.Equal(want) {
			t.Errorf("%s after %s: got %s, want %s", c.expr, c.after, got, want)
		}
	}
}

func TestCronNextFindsALeapDay(t *testing.T) {
	// The scan runs four years, so Feb 29 resolves from a non-leap year
	// instead of looking unsatisfiable.
	sched, err := parseCron("0 9 29 2 *")
	if err != nil {
		t.Fatal(err)
	}
	got, err := sched.Next(mustTime(t, "2026-07-26 12:00"))
	if err != nil {
		t.Fatal(err)
	}
	if want := mustTime(t, "2028-02-29 09:00"); !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestCronNextReportsAnUnsatisfiableExpression(t *testing.T) {
	sched, err := parseCron("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sched.Next(mustTime(t, "2026-07-26 12:00")); err == nil {
		t.Error("February 30 never comes around")
	}
}

func TestNextOccurrenceCoversBothKinds(t *testing.T) {
	hb := &Action{Kind: KindHeartbeat, Heartbeat: HeartbeatSpec{IntervalMinutes: 30}}
	got, err := nextOccurrence(hb, mustTime(t, "2026-07-27 09:00"))
	if err != nil {
		t.Fatal(err)
	}
	if want := mustTime(t, "2026-07-27 09:30"); !got.Equal(want) {
		t.Errorf("heartbeat: got %s, want %s", got, want)
	}

	routine := &Action{Kind: KindRoutine, Routine: RoutineSpec{Preset: "daily", Hours: []int{6}, Minute: 15}}
	got, err = nextOccurrence(routine, mustTime(t, "2026-07-27 09:00"))
	if err != nil {
		t.Fatal(err)
	}
	if want := mustTime(t, "2026-07-28 06:15"); !got.Equal(want) {
		t.Errorf("routine: got %s, want %s", got, want)
	}
}

func TestCronDomDowEitherMatches(t *testing.T) {
	// Standard cron: with both day fields restricted, either may match.
	sched, err := parseCron("0 0 15 * 1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := sched.Next(mustTime(t, "2026-07-26 12:00"))
	if err != nil {
		t.Fatal(err)
	}
	// Monday the 27th comes before the 15th of August.
	if want := mustTime(t, "2026-07-27 00:00"); !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestCronRejectsBadExpressions(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "a * * * *", "*/0 * * * *", "5-1 * * * *"} {
		if _, err := parseCron(expr); err == nil {
			t.Errorf("%q: expected error", expr)
		}
	}
}

func TestWorkingHours(t *testing.T) {
	wh := &WorkingHours{Days: []int{1, 2, 3, 4, 5}, StartHour: 9, EndHour: 17}
	if wh.Contains(mustTime(t, "2026-07-26 10:00")) {
		t.Error("Sunday should be outside")
	}
	if !wh.Contains(mustTime(t, "2026-07-27 09:00")) {
		t.Error("Monday 09:00 should be inside")
	}
	if wh.Contains(mustTime(t, "2026-07-27 17:00")) {
		t.Error("end hour is exclusive")
	}

	if err := (&WorkingHours{StartHour: 9, EndHour: 9}).validate(); err == nil {
		t.Error("an empty hour range should be rejected, not read as all day")
	}
	if err := (&WorkingHours{StartHour: 0, EndHour: 24}).validate(); err != nil {
		t.Errorf("0-24 is the all-day range: %v", err)
	}

	overnight := &WorkingHours{StartHour: 22, EndHour: 6}
	if !overnight.Contains(mustTime(t, "2026-07-27 23:00")) || !overnight.Contains(mustTime(t, "2026-07-27 05:00")) {
		t.Error("overnight range should span midnight")
	}
	if overnight.Contains(mustTime(t, "2026-07-27 12:00")) {
		t.Error("midday should be outside an overnight range")
	}
}

func TestNextHeartbeatClampsIntoWorkingHours(t *testing.T) {
	spec := HeartbeatSpec{
		IntervalMinutes: 60,
		WorkingHours:    &WorkingHours{Days: []int{1, 2, 3, 4, 5}, StartHour: 9, EndHour: 16},
	}
	// Friday 15:30 + 60m lands outside hours; next slot is Monday 09:00.
	got := spec.NextHeartbeat(mustTime(t, "2026-07-24 15:30"))
	if want := mustTime(t, "2026-07-27 09:00"); !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}

	noHours := HeartbeatSpec{IntervalMinutes: 30}
	got = noHours.NextHeartbeat(mustTime(t, "2026-07-24 15:30"))
	if want := mustTime(t, "2026-07-24 16:00"); !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestRoutinePresets(t *testing.T) {
	r := RoutineSpec{Preset: "weekdays", Hours: []int{6}, Minute: 15}
	got, err := r.NextRoutine(mustTime(t, "2026-07-26 12:00"))
	if err != nil {
		t.Fatal(err)
	}
	if want := mustTime(t, "2026-07-27 06:15"); !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}

	if _, err := (RoutineSpec{Preset: "cron", Cron: "bad"}).NextRoutine(time.Now()); err == nil {
		t.Error("bad custom cron should error")
	}
}
