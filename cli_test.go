package main

import (
	"strings"
	"testing"
)

func summaryHeartbeat(interval int, wh *WorkingHours) *Action {
	return &Action{Kind: KindHeartbeat, Heartbeat: HeartbeatSpec{IntervalMinutes: interval, WorkingHours: wh}}
}

func summaryRoutine(kind Kind, r RoutineSpec) *Action {
	return &Action{Kind: kind, Routine: r}
}

func TestScheduleSummary(t *testing.T) {
	weekdays := []int{1, 2, 3, 4, 5}
	cases := []struct {
		name string
		a    *Action
		want string
	}{
		{"heartbeat no window", summaryHeartbeat(30, nil), "every 30m"},
		{"heartbeat hourly no window", summaryHeartbeat(60, nil), "hourly"},
		{"heartbeat odd interval", summaryHeartbeat(90, nil), "every 1h30m"},
		{"heartbeat once per window", summaryHeartbeat(1200, &WorkingHours{Days: weekdays, StartHour: 6, EndHour: 16}),
			"weekdays ~06:00"},
		{"heartbeat once per window any day", summaryHeartbeat(1200, &WorkingHours{StartHour: 6, EndHour: 16}),
			"daily ~06:00"},
		{"heartbeat repeating in window", summaryHeartbeat(60, &WorkingHours{Days: weekdays, StartHour: 9, EndHour: 16}),
			"hourly, weekdays 09-16h"},
		{"heartbeat overnight window", summaryHeartbeat(600, &WorkingHours{StartHour: 22, EndHour: 6}),
			"daily ~22:00"},
		{"script daily", summaryRoutine(KindScript, RoutineSpec{Preset: "daily", Hours: []int{2}, Minute: 0}),
			"daily 02:00"},
		{"script weekdays", summaryRoutine(KindScript, RoutineSpec{Preset: "weekdays", Hours: []int{6}, Minute: 0}),
			"weekdays 06:00"},
		{"script hourly range", summaryRoutine(KindScript, RoutineSpec{Preset: "days", Days: weekdays, Hours: []int{8, 9, 10, 11, 12, 13, 14, 15, 16}, Minute: 40}),
			"weekdays hourly 08:40-16:40"},
		{"script sparse hours", summaryRoutine(KindScript, RoutineSpec{Preset: "days", Days: weekdays, Hours: []int{8, 12}, Minute: 0}),
			"weekdays 08:00, 12:00"},
		{"script weekend days", summaryRoutine(KindScript, RoutineSpec{Preset: "days", Days: []int{0, 6}, Hours: []int{10}, Minute: 15}),
			"weekends 10:15"},
		{"script named days", summaryRoutine(KindScript, RoutineSpec{Preset: "days", Days: []int{1, 3, 5}, Hours: []int{7}, Minute: 30}),
			"Mon,Wed,Fri 07:30"},
		{"routine every hour of day", summaryRoutine(KindRoutine, RoutineSpec{Preset: "daily", Minute: 40}),
			"daily hourly at :40"},
		{"monthly", summaryRoutine(KindScript, RoutineSpec{Preset: "monthly", MonthDay: 11, Hours: []int{2}, Minute: 0}),
			"monthly day 11 02:00"},
		{"cron passthrough", summaryRoutine(KindScript, RoutineSpec{Preset: "cron", Cron: "0 2 * * *"}),
			"0 2 * * *"},
	}
	for _, tc := range cases {
		if got := scheduleSummary(tc.a); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestScheduleDetail(t *testing.T) {
	weekdays := []int{1, 2, 3, 4, 5}
	hb := summaryHeartbeat(1200, &WorkingHours{Days: weekdays, StartHour: 6, EndHour: 16})
	got := scheduleDetail(hb)
	if !strings.HasPrefix(got, "weekdays ~06:00") {
		t.Errorf("heartbeat detail should start with the summary, got %q", got)
	}
	for _, want := range []string{"every 20h", "06-16h", "fires late"} {
		if !strings.Contains(got, want) {
			t.Errorf("heartbeat detail missing %q: %q", want, got)
		}
	}

	hbFree := summaryHeartbeat(30, nil)
	if got := scheduleDetail(hbFree); !strings.Contains(got, "measured from the last run") {
		t.Errorf("windowless heartbeat detail should explain its anchor, got %q", got)
	}

	script := summaryRoutine(KindScript, RoutineSpec{Preset: "daily", Hours: []int{2}, Minute: 0})
	got = scheduleDetail(script)
	if !strings.HasPrefix(got, "daily 02:00") || !strings.Contains(got, "skipped") {
		t.Errorf("script detail should state the missed-run policy, got %q", got)
	}
}
