package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WorkingHours restricts when a heartbeat may fire. Days use 0=Sunday..6=Saturday;
// an empty day list means any day. StartHour > EndHour is an overnight range.
type WorkingHours struct {
	Days      []int `toml:"days"`
	StartHour int   `toml:"start_hour"`
	EndHour   int   `toml:"end_hour"`
}

func (w *WorkingHours) validate() error {
	for _, d := range w.Days {
		if d < 0 || d > 6 {
			return fmt.Errorf("working_hours day %d out of range 0-6", d)
		}
	}
	if w.StartHour < 0 || w.StartHour > 23 || w.EndHour < 0 || w.EndHour > 24 {
		return fmt.Errorf("working_hours hours out of range")
	}
	if w.StartHour == w.EndHour {
		return fmt.Errorf("working_hours start_hour and end_hour must differ (use 0 and 24 for all day)")
	}
	return nil
}

func (w *WorkingHours) Contains(t time.Time) bool {
	if len(w.Days) > 0 {
		day := int(t.Weekday())
		found := false
		for _, d := range w.Days {
			if d == day {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	h := t.Hour()
	switch {
	case w.StartHour < w.EndHour:
		return h >= w.StartHour && h < w.EndHour
	case w.StartHour > w.EndHour:
		return h >= w.StartHour || h < w.EndHour
	default:
		return true
	}
}

// NextHeartbeat returns the first instant at or after last+interval that falls
// inside working hours. The forward scan is minute-wise and capped at 14 days so
// day-set/hour combinations that never match cannot loop forever.
func (h HeartbeatSpec) NextHeartbeat(last time.Time) time.Time {
	interval := time.Duration(h.IntervalMinutes) * time.Minute
	candidate := last.Add(interval)
	if h.WorkingHours == nil {
		return candidate
	}
	for i := 0; i < 14*24*60; i++ {
		if h.WorkingHours.Contains(candidate) {
			return candidate
		}
		candidate = candidate.Add(time.Minute)
	}
	return last.Add(interval)
}

// cronExpression assumes a validated spec: validate rejects out-of-range
// fields rather than clamping them into a time the user never asked for.
func (r RoutineSpec) cronExpression() (*cronSchedule, error) {
	minute := r.Minute
	hours := "*"
	if len(r.Hours) > 0 {
		hours = joinInts(uniqueSorted(r.Hours, 0, 23))
	}
	var expr string
	switch r.Preset {
	case "daily":
		expr = fmt.Sprintf("%d %s * * *", minute, hours)
	case "weekdays":
		expr = fmt.Sprintf("%d %s * * 1-5", minute, hours)
	case "days":
		days := "*"
		if len(r.Days) > 0 {
			days = joinInts(uniqueSorted(r.Days, 0, 6))
		}
		expr = fmt.Sprintf("%d %s * * %s", minute, hours, days)
	case "monthly":
		expr = fmt.Sprintf("%d %s %d * *", minute, hours, r.MonthDay)
	case "cron":
		expr = strings.TrimSpace(r.Cron)
	default:
		return nil, fmt.Errorf("routine preset must be daily, weekdays, days, monthly, or cron, got %q", r.Preset)
	}
	return parseCron(expr)
}

// NextRoutine returns the first cron match strictly after t.
func (r RoutineSpec) NextRoutine(t time.Time) (time.Time, error) {
	sched, err := r.cronExpression()
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(t)
}

// nextOccurrence is the one schedule computation: the daemon's due check and
// the board's NEXT RUN column must not disagree about when an action runs.
func nextOccurrence(a *Action, anchor time.Time) (time.Time, error) {
	if a.Kind == KindHeartbeat {
		return a.Heartbeat.NextHeartbeat(anchor), nil
	}
	return a.Routine.NextRoutine(anchor)
}

// sameWallClock reports whether two instants show the same local date, hour,
// and minute. On a DST fall-back day the minute scan meets the repeated hour
// twice; comparing wall clocks lets the caller drop the second occurrence.
func sameWallClock(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd && a.Hour() == b.Hour() && a.Minute() == b.Minute()
}

func uniqueSorted(vals []int, lo, hi int) []int {
	seen := map[int]bool{}
	var out []int
	for _, v := range vals {
		if v >= lo && v <= hi && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

func joinInts(vals []int) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

// cronSchedule is a standard 5-field cron expression (minute hour day-of-month
// month day-of-week). Fields accept *, N, N-M, */S, N-M/S, and comma lists.
// Day-of-week uses 0=Sunday (7 also accepted as Sunday). Per convention, when
// both day fields are restricted a time matches if EITHER matches.
type cronSchedule struct {
	minute, hour, dom, month, dow map[int]bool
	domAny, dowAny                bool
}

func parseCron(expr string) (*cronSchedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields, got %d", expr, len(fields))
	}
	specs := []struct {
		lo, hi int
	}{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	sets := make([]map[int]bool, 5)
	for i, f := range fields {
		set, err := parseCronField(f, specs[i].lo, specs[i].hi)
		if err != nil {
			return nil, fmt.Errorf("cron %q field %d: %w", expr, i+1, err)
		}
		sets[i] = set
	}
	if sets[4][7] {
		sets[4][0] = true
		delete(sets[4], 7)
	}
	return &cronSchedule{
		minute: sets[0], hour: sets[1], dom: sets[2], month: sets[3], dow: sets[4],
		domAny: fields[2] == "*", dowAny: fields[4] == "*",
	}, nil
}

func parseCronField(field string, lo, hi int) (map[int]bool, error) {
	set := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		rangePart, step := part, 1
		if idx := strings.Index(part, "/"); idx >= 0 {
			rangePart = part[:idx]
			s, err := strconv.Atoi(part[idx+1:])
			if err != nil || s < 1 {
				return nil, fmt.Errorf("bad step in %q", part)
			}
			step = s
		}
		start, end := lo, hi
		switch {
		case rangePart == "*":
		case strings.Contains(rangePart, "-"):
			bounds := strings.SplitN(rangePart, "-", 2)
			a, err1 := strconv.Atoi(bounds[0])
			b, err2 := strconv.Atoi(bounds[1])
			if err1 != nil || err2 != nil || a > b {
				return nil, fmt.Errorf("bad range %q", rangePart)
			}
			start, end = a, b
		default:
			v, err := strconv.Atoi(rangePart)
			if err != nil {
				return nil, fmt.Errorf("bad value %q", rangePart)
			}
			start, end = v, v
			if strings.Index(part, "/") >= 0 {
				end = hi
			}
		}
		if start < lo || end > hi {
			return nil, fmt.Errorf("%q out of range %d-%d", part, lo, hi)
		}
		for v := start; v <= end; v += step {
			set[v] = true
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("empty field")
	}
	return set, nil
}

func (c *cronSchedule) matchesDay(t time.Time) bool {
	if !c.month[int(t.Month())] {
		return false
	}
	domMatch := c.dom[t.Day()]
	dowMatch := c.dow[int(t.Weekday())]
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dowMatch
	case c.dowAny:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}

func (c *cronSchedule) matches(t time.Time) bool {
	return c.minute[t.Minute()] && c.hour[t.Hour()] && c.matchesDay(t)
}

// scanYears bounds the search. Four years always contains a leap day, so
// `0 9 29 2 *` resolves from a non-leap year instead of looking unsatisfiable.
const scanYears = 4

// Next scans for the first match strictly after t, minute-by-minute inside
// days the expression can match and a day at a time through the rest.
func (c *cronSchedule) Next(t time.Time) (time.Time, error) {
	candidate := t.Truncate(time.Minute).Add(time.Minute)
	limit := candidate.AddDate(scanYears, 0, 0)
	for candidate.Before(limit) {
		if !c.matchesDay(candidate) {
			y, m, d := candidate.Date()
			candidate = time.Date(y, m, d+1, 0, 0, 0, 0, candidate.Location())
			continue
		}
		if c.matches(candidate) {
			return candidate, nil
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("no cron match within %d years", scanYears)
}
