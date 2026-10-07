package main

import (
	"testing"
	"time"
	_ "time/tzdata"
)

func london(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func utc(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestRepeatedWallClock(t *testing.T) {
	loc := london(t)
	// Clocks go back at 01:00 UTC on 25 Oct 2026: 01:00-01:59 local shows
	// first as BST (00:00-00:59 UTC), then again as GMT (01:00-01:59 UTC).
	cases := []struct {
		at   string
		want bool
	}{
		{"2026-10-24 23:59", false}, // 00:59 BST
		{"2026-10-25 00:00", false}, // 01:00 BST, first pass
		{"2026-10-25 00:59", false}, // 01:59 BST, first pass
		{"2026-10-25 01:00", true},  // 01:00 GMT, second pass
		{"2026-10-25 01:59", true},  // 01:59 GMT, second pass
		{"2026-10-25 02:00", false}, // 02:00 GMT
		{"2027-03-28 00:59", false}, // 00:59 GMT, before spring forward
		{"2027-03-28 01:00", false}, // 02:00 BST, after spring forward
		{"2026-07-27 06:15", false},
	}
	for _, c := range cases {
		if got := repeatedWallClock(utc(t, c.at).In(loc)); got != c.want {
			t.Errorf("%s UTC: got %v, want %v", c.at, got, c.want)
		}
	}
	if repeatedWallClock(utc(t, "2026-10-25 01:30")) {
		t.Error("a zone without clock changes never repeats")
	}
}

func TestCronNextAcrossClockChanges(t *testing.T) {
	loc := london(t)
	cases := []struct {
		expr, after, want string // UTC instants
	}{
		// Fall back: after the first-pass 01:30 BST the next 01:00 is a day
		// later, not the 01:00 GMT the clock shows half an hour on.
		{"0,30 1 * * *", "2026-10-25 00:00", "2026-10-25 00:30"},
		{"0,30 1 * * *", "2026-10-25 00:30", "2026-10-26 01:00"},
		{"*/15 * * * *", "2026-10-25 00:45", "2026-10-25 02:00"},
		{"0 2 * * *", "2026-10-25 00:30", "2026-10-25 02:00"},
		// Spring forward: 01:00-01:59 local never happens on 28 Mar 2027.
		{"30 1 * * *", "2027-03-28 00:00", "2027-03-29 00:30"},
		{"30 2 * * *", "2027-03-28 00:00", "2027-03-28 01:30"},
	}
	for _, c := range cases {
		sched, err := parseCron(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		got, err := sched.Next(utc(t, c.after).In(loc))
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if want := utc(t, c.want); !got.Equal(want) {
			t.Errorf("%s after %s UTC: got %s, want %s UTC", c.expr, c.after, got.UTC().Format("2006-01-02 15:04"), c.want)
		}
	}
}

// runNight ticks the daemon's due check every 30 seconds from start to end,
// stamping each fire as the tick loop does and saving and reloading the state
// file on every tick, so a stamp that only survives in memory cannot hide a
// repeat. It returns each action's fire stamps in UTC.
func runNight(t *testing.T, actions []*Action, start, end time.Time) map[string][]string {
	t.Helper()
	d := testDaemon(t)
	fires := map[string][]string{}
	for now := start; now.Before(end); now = now.Add(30 * time.Second) {
		for _, a := range actions {
			if fire, stamp := d.due(a, now); fire {
				d.state.setLastRun(a.Name, stamp)
				fires[a.Name] = append(fires[a.Name], stamp.UTC().Format("15:04"))
			}
		}
		d.saveState()
		d.state = loadState(d.paths.StateFile())
	}
	return fires
}

func scriptOn(name, cron string) *Action {
	return &Action{
		Name: name, Kind: KindScript, Directory: "/tmp", Command: "true",
		Routine: RoutineSpec{Preset: "cron", Cron: cron},
	}
}

func assertFires(t *testing.T, fires map[string][]string, name string, want ...string) {
	t.Helper()
	got := fires[name]
	if len(got) != len(want) {
		t.Errorf("%s: fired at %v UTC, want %v", name, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: fired at %v UTC, want %v", name, got, want)
			return
		}
	}
}

func TestDueClocksGoBackRunsEachTimeOnce(t *testing.T) {
	loc := london(t)
	actions := []*Action{
		scriptOn("twice-in-the-hour", "0,30 1 * * *"),
		scriptOn("once-in-the-hour", "15 1 * * *"),
		scriptOn("after-the-hour", "30 2 * * *"),
	}
	// 23:30 BST on Sat 24 Oct to 04:00 GMT on Sun 25 Oct 2026.
	start := utc(t, "2026-10-24 22:30").In(loc)
	end := utc(t, "2026-10-25 04:00").In(loc)
	fires := runNight(t, actions, start, end)
	// First pass of the repeated hour only: 01:00, 01:15, 01:30 BST are
	// 00:00, 00:15, 00:30 UTC; the GMT repeats at 01:xx UTC never run.
	assertFires(t, fires, "twice-in-the-hour", "00:00", "00:30")
	assertFires(t, fires, "once-in-the-hour", "00:15")
	assertFires(t, fires, "after-the-hour", "02:30")
}

func TestDueClocksGoForwardSkipsTheMissingHour(t *testing.T) {
	loc := london(t)
	actions := []*Action{
		scriptOn("in-the-missing-hour", "0,30 1 * * *"),
		scriptOn("after-the-hour", "30 2 * * *"),
		scriptOn("before-the-hour", "30 0 * * *"),
	}
	// 23:30 GMT on Sat 27 Mar to 05:00 BST on Sun 28 Mar 2027.
	start := utc(t, "2027-03-27 23:30").In(loc)
	end := utc(t, "2027-03-28 04:00").In(loc)
	fires := runNight(t, actions, start, end)
	// 01:00-01:59 local does not exist that night; 02:30 BST is 01:30 UTC.
	assertFires(t, fires, "in-the-missing-hour")
	assertFires(t, fires, "after-the-hour", "01:30")
	assertFires(t, fires, "before-the-hour", "00:30")
}
