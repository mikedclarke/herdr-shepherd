package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

func loadForCLI(p paths) ([]*Action, error) {
	actions, fileErrs, err := LoadActions(p.ActionsDir())
	if err != nil {
		return nil, err
	}
	for _, ferr := range fileErrs {
		fmt.Fprintln(os.Stderr, "warning:", ferr)
	}
	return actions, nil
}

func cmdList() error {
	p := resolvePaths()
	actions, err := loadForCLI(p)
	if err != nil {
		return err
	}
	if len(actions) == 0 {
		fmt.Printf("No actions. Add *.toml files to %s\n", p.ActionsDir())
		return nil
	}
	st := readState(p.StateFile())
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tENABLED\tSCHEDULE\tLAST RUN\tNEXT RUN")
	now := time.Now()
	for _, a := range actions {
		fmt.Fprintf(w, "%s\t%s\t%v\t%s\t%s\t%s\n",
			a.Name, a.Kind, a.IsEnabled(), scheduleSummary(a),
			fmtTime(st.lastRun(a.Name), st.lastStatus(a.Name)), fmtTime(nextRun(a, st.lastRun(a.Name), now), ""))
	}
	return w.Flush()
}

func scheduleSummary(a *Action) string {
	switch a.Kind {
	case KindHeartbeat:
		h := a.Heartbeat
		wh := h.WorkingHours
		if wh == nil {
			return intervalLabel(h.IntervalMinutes)
		}
		if h.IntervalMinutes >= windowMinutes(wh) {
			return fmt.Sprintf("%s ~%02d:00", dayLabel(wh.Days), wh.StartHour)
		}
		return fmt.Sprintf("%s, %s %02d-%02dh",
			intervalLabel(h.IntervalMinutes), dayLabel(wh.Days), wh.StartHour, wh.EndHour)
	default:
		r := a.Routine
		switch r.Preset {
		case "cron":
			return r.Cron
		case "monthly":
			return fmt.Sprintf("monthly day %d %s", r.MonthDay, hoursLabel(r.Hours, r.Minute))
		case "weekdays":
			return "weekdays " + hoursLabel(r.Hours, r.Minute)
		case "days":
			return dayLabel(r.Days) + " " + hoursLabel(r.Hours, r.Minute)
		default:
			return "daily " + hoursLabel(r.Hours, r.Minute)
		}
	}
}

// scheduleDetail is the long form for detail views: the summary plus the firing
// semantics that the compact form can't carry (heartbeats fire late when a run
// is missed; routines and scripts skip occurrences older than the catch-up grace).
func scheduleDetail(a *Action) string {
	s := scheduleSummary(a)
	if a.Kind == KindHeartbeat {
		h := a.Heartbeat
		if wh := h.WorkingHours; wh != nil {
			return fmt.Sprintf("%s  (%s within %02d-%02dh; a missed run fires late, never skipped)",
				s, intervalLabel(h.IntervalMinutes), wh.StartHour, wh.EndHour)
		}
		return s + "  (measured from the last run)"
	}
	return fmt.Sprintf("%s  (fixed times; a run missed by more than %dm is skipped)",
		s, int(catchUpGrace.Minutes()))
}

// windowMinutes is the working-hours span; an interval at least this long can
// fire at most once per window, which the summary renders as an approximate time.
func windowMinutes(wh *WorkingHours) int {
	span := ((wh.EndHour - wh.StartHour) + 24) % 24 * 60
	if span == 0 {
		span = 24 * 60
	}
	return span
}

func intervalLabel(minutes int) string {
	switch {
	case minutes == 60:
		return "hourly"
	case minutes > 60 && minutes%60 == 0:
		return fmt.Sprintf("every %dh", minutes/60)
	case minutes > 60:
		return fmt.Sprintf("every %dh%02dm", minutes/60, minutes%60)
	default:
		return fmt.Sprintf("every %dm", minutes)
	}
}

func dayLabel(days []int) string {
	d := uniqueSorted(days, 0, 6)
	switch {
	case len(d) == 0 || len(d) == 7:
		return "daily"
	case slices.Equal(d, []int{1, 2, 3, 4, 5}):
		return "weekdays"
	case slices.Equal(d, []int{0, 6}):
		return "weekends"
	}
	names := [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	parts := make([]string, len(d))
	for i, v := range d {
		parts[i] = names[v]
	}
	return strings.Join(parts, ",")
}

func hoursLabel(hours []int, minute int) string {
	hs := uniqueSorted(hours, 0, 23)
	switch {
	case len(hs) == 0:
		return fmt.Sprintf("hourly at :%02d", minute)
	case len(hs) == 1:
		return fmt.Sprintf("%02d:%02d", hs[0], minute)
	case hs[len(hs)-1]-hs[0] == len(hs)-1:
		return fmt.Sprintf("hourly %02d:%02d-%02d:%02d", hs[0], minute, hs[len(hs)-1], minute)
	}
	parts := make([]string, len(hs))
	for i, h := range hs {
		parts[i] = fmt.Sprintf("%02d:%02d", h, minute)
	}
	return strings.Join(parts, ", ")
}

// nextRun anchors exactly as due does, so the displayed next run is the one
// the daemon will actually fire.
func nextRun(a *Action, last time.Time, now time.Time) time.Time {
	if !a.IsEnabled() {
		return time.Time{}
	}
	anchor := last
	if a.Kind == KindHeartbeat {
		if last.IsZero() {
			return now
		}
	} else if grace := now.Add(-catchUpGrace); anchor.Before(grace) {
		anchor = grace
	}
	next, err := nextOccurrence(a, anchor)
	if err != nil {
		return time.Time{}
	}
	return next
}

func fmtTime(t time.Time, suffix string) string {
	if t.IsZero() {
		return "-"
	}
	s := t.Format("Mon 15:04")
	if time.Until(t) > 6*24*time.Hour || time.Since(t) > 6*24*time.Hour {
		s = t.Format("2006-01-02 15:04")
	}
	if suffix != "" {
		s += " (" + suffix + ")"
	}
	return s
}

func cmdStatus(notify bool) error {
	p := resolvePaths()
	st := readState(p.StateFile())
	beat := st.heartbeatAt()
	alive := !beat.IsZero() && time.Since(beat) < 3*tickInterval
	title := "Shepherd: daemon not running"
	if alive {
		title = "Shepherd: daemon running"
	}

	actions, err := loadForCLI(p)
	if err != nil {
		return err
	}
	now := time.Now()
	total, enabled := len(actions), 0
	var soonest time.Time
	var soonestName string
	for _, a := range actions {
		if !a.IsEnabled() {
			continue
		}
		enabled++
		if n := nextRun(a, st.lastRun(a.Name), now); !n.IsZero() && (soonest.IsZero() || n.Before(soonest)) {
			soonest, soonestName = n, a.Name
		}
	}
	var body string
	switch {
	case total == 0:
		body = "No actions configured in " + p.ActionsDir()
	case enabled == 0:
		body = fmt.Sprintf("%d action(s) configured, all disabled", total)
	case soonest.IsZero():
		body = fmt.Sprintf("%d of %d action(s) enabled; no next run computable", enabled, total)
	default:
		body = fmt.Sprintf("%d of %d action(s) enabled; next: %s at %s", enabled, total, soonestName, fmtTime(soonest, ""))
	}

	fmt.Println(title)
	fmt.Println(body)
	if notify {
		client, err := newHerdrClient()
		if err != nil {
			return err
		}
		return client.notify(title, body, "none")
	}
	return nil
}

// cmdRun fires one action immediately. Scripts run synchronously; agent actions
// are started in a workspace and left to the user (no watcher, no auto-close —
// a manual run means someone is present). Manual runs do not consult or update
// the daemon's schedule.
func cmdRun(name string) error {
	p := resolvePaths()
	actions, err := loadForCLI(p)
	if err != nil {
		return err
	}
	var action *Action
	var names []string
	for _, a := range actions {
		names = append(names, a.Name)
		if a.Name == name {
			action = a
		}
	}
	if action == nil {
		return fmt.Errorf("no action named %q; available: %v", name, names)
	}

	release, ok, err := tryRunLock(p.StateDir, action.Name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is already running (another process)", action.Name)
	}
	defer release()

	if action.Kind == KindScript {
		fmt.Printf("Running %s in %s\n", action.Name, action.Dir())
		began := time.Now()
		out := &tailBuffer{max: outputTailMax}
		runErr := runScriptOnce(action, io.MultiWriter(os.Stdout, out))
		rec := runRecord{
			At: time.Now(), Action: action.Name, Kind: action.Kind,
			Status: "completed", Detail: out.String(), Trigger: triggerManual,
			DurationSecs: durationSecs(began),
		}
		if runErr != nil {
			rec.Status, rec.Detail = "error", runErr.Error()
			if isDeferExit(runErr) {
				// The script asked to be retried later (exit 75); a manual run
				// has nobody to retry it, but it is a deferral, not a failure.
				rec.Status, rec.Detail = "deferred", out.String()
			}
		}
		if lerr := appendRunLog(p.RunLogFile(), rec); lerr != nil {
			fmt.Fprintln(os.Stderr, "warning: run log:", lerr)
		}
		return runErr
	}

	if action.Gate != "" {
		began := time.Now()
		verdict, gateDetail := runGate(action, triggerManual)
		switch verdict {
		case gateSkip:
			fmt.Printf("Skipped %s: %s\n", action.Name, gateDetail)
			if lerr := appendRunLog(p.RunLogFile(), skippedRecord(action, gateDetail, triggerManual, began)); lerr != nil {
				fmt.Fprintln(os.Stderr, "warning: run log:", lerr)
			}
			return nil
		case gateFailed:
			fmt.Fprintln(os.Stderr, "warning:", gateDetail, "(running the agent anyway)")
		}
	}

	// The lock goes with this process: a manual agent run is handed to the
	// user once its session is up, and nobody watches it after that.
	wsID, err := startAgentRun(action)
	if err != nil {
		return err
	}
	if lerr := recordManualStart(action, wsID); lerr != nil {
		fmt.Fprintln(os.Stderr, "warning: run log:", lerr)
	}
	fmt.Printf("Started %s in workspace %s\n", action.Name, wsID)
	return nil
}

// startAgentRun opens a manual run's workspace and submits the agent command.
// Shared by `run` and the board; both keep manual-run semantics (no watcher,
// no auto-close, no schedule coordination). It must not write to the
// terminal: the board calls it from inside the TUI's alternate screen.
func startAgentRun(a *Action) (workspaceID string, err error) {
	client, err := newHerdrClient()
	if err != nil {
		return "", err
	}
	wsID, _, err := launchAgentWorkspace(client, a, shellSettle, triggerManual)
	if err != nil {
		return "", err
	}
	return wsID, nil
}

// cmdWake queues a wake for one action. It never takes the run lock and never
// launches anything: the daemon fires the wake on its next tick, under the
// same lock and stamping rules as a scheduled occurrence, which is exactly
// what a manual `run` cannot offer. With an instant it schedules the wake
// instead: the daemon promotes it on the first tick at or after that instant.
func cmdWake(name string, at time.Time) error {
	p := resolvePaths()
	actions, err := loadForCLI(p)
	if err != nil {
		return err
	}
	var action *Action
	var names []string
	for _, a := range actions {
		names = append(names, a.Name)
		if a.Name == name {
			action = a
		}
	}
	if action == nil {
		return fmt.Errorf("no action named %q; available: %v", name, names)
	}
	if !action.IsEnabled() {
		return fmt.Errorf("%s is disabled", action.Name)
	}
	source := "cli"
	if u := os.Getenv("USER"); u != "" {
		source = "cli:" + u
	}
	// An instant already past by less than the wake's own lifetime is not worth
	// scheduling: the next tick would promote it anyway, so ask for the wake now.
	if !at.IsZero() && !at.After(time.Now()) && time.Since(at) <= wakeMaxAge {
		at = time.Time{}
	}
	if !at.IsZero() {
		switch err := scheduleWake(p.StateDir, action.Name, at, source); {
		case errors.Is(err, errWakeInPast):
			return fmt.Errorf("--at is %d min in the past", int(time.Since(at).Minutes()))
		case err != nil:
			return err
		}
		fmt.Printf("scheduled: %s at %s (fires on the first tick at or after it)\n", action.Name, at.Format(time.RFC3339))
		return nil
	}
	switch err := requestWake(p.StateDir, action.Name, source); {
	case errors.Is(err, errWakeDebounced):
		fmt.Printf("already queued: %s\n", action.Name)
	case err != nil:
		return err
	default:
		fmt.Printf("queued: %s (fires on the daemon's next tick, within %ds)\n", action.Name, int(tickInterval.Seconds()))
	}
	return nil
}

// recordManualStart writes the started record — the only trace a manual agent
// run leaves. Callers report a failure their own way (stderr or status line).
func recordManualStart(a *Action, wsID string) error {
	p := resolvePaths()
	return appendRunLog(p.RunLogFile(), runRecord{
		At: time.Now(), Action: a.Name, Kind: a.Kind, Status: "started",
		Detail: "workspace " + wsID, Trigger: triggerManual,
	})
}
