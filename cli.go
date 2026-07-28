package main

import (
	"fmt"
	"io"
	"os"
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
		s := fmt.Sprintf("every %dm", a.Heartbeat.IntervalMinutes)
		if wh := a.Heartbeat.WorkingHours; wh != nil {
			s += fmt.Sprintf(" (%02d-%02dh)", wh.StartHour, wh.EndHour)
		}
		return s
	default:
		if a.Routine.Preset == "cron" {
			return a.Routine.Cron
		}
		return fmt.Sprintf("%s %s:%02d", a.Routine.Preset, joinInts(uniqueSorted(a.Routine.Hours, 0, 23)), a.Routine.Minute)
	}
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
		out := &tailBuffer{max: outputTailMax}
		runErr := runScriptOnce(action, io.MultiWriter(os.Stdout, out))
		rec := runRecord{
			At: time.Now(), Action: action.Name, Kind: action.Kind,
			Status: "completed", Detail: out.String(), Trigger: triggerManual,
		}
		if runErr != nil {
			rec.Status, rec.Detail = "error", runErr.Error()
		}
		if lerr := appendRunLog(p.RunLogFile(), rec); lerr != nil {
			fmt.Fprintln(os.Stderr, "warning: run log:", lerr)
		}
		return runErr
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
	wsID, _, err := launchAgentWorkspace(client, a, shellSettle)
	if err != nil {
		return "", err
	}
	return wsID, nil
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
