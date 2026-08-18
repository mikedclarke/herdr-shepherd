package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// The tick is level-triggered: an action fires up to tickInterval after
	// its scheduled minute rather than being missed.
	tickInterval = 30 * time.Second
	startTimeout = 2 * time.Minute
	// agent.wait is sliced so the loop can re-check its watch deadline.
	waitSliceMS = 60_000
	// Phase-1 waits are sliced shorter so the start deadline stays responsive.
	startWaitSliceMS = 15_000
	// An idle seen mid-run is re-armed for this long before it is believed.
	idleConfirmMS = 10_000
	doneProbeMS   = 1_000
	// A routine occurrence older than this is dropped, not run late — waking
	// a slept machine at 08:55 must not fire the 06:00 jobs (see due).
	catchUpGrace = 10 * time.Minute
	// Retries for occurrences that failed before their agent/script started
	// (herdr restarting, socket briefly down).
	maxStartAttempts = 3
	shellSettle      = 1 * time.Second
	pollPause        = 1 * time.Second
	// A pane whose shell was not ready swallows the submitted line; the
	// command is sent once more after this long without a detected agent.
	resendAfter       = 20 * time.Second
	outputTailMax     = 4096
	daemonLogMaxBytes = 5 << 20
	// spawnDetached names the log file here so the daemon can rotate the file
	// it is writing to.
	logPathEnv = "HERDR_SHEPHERD_LOG"
)

// herdrAPI is the slice of the herdr socket API the daemon uses; it exists so
// agent-watch logic is testable against a scripted fake.
type herdrAPI interface {
	workspaceCreate(cwd, label string, env map[string]string) (workspaceID, paneID string, err error)
	workspaceClose(workspaceID string) error
	runCommand(paneID, command string) error
	agentWait(target string, until []string, timeoutMS int) (string, error)
	paneExists(paneID string) (bool, error)
	notify(title, body, sound string) error
}

type daemon struct {
	paths  paths
	client herdrAPI
	state  *daemonState

	// Timing knobs; runDaemon sets the real values, tests shrink them.
	startTimeout time.Duration
	settle       time.Duration
	pause        time.Duration
	resend       time.Duration

	// Log rotation state; only the tick loop touches it.
	logFile *os.File
	logPath string
	// notifiedErrors holds the previous tick's config errors; tick-only.
	notifiedErrors map[string]bool

	mu            sync.Mutex
	startFailures map[string]int
	// deferredSince holds, per action, when its current deferral spell began;
	// an entry means a retry is pending. In-memory only: a daemon restart
	// forgets the spell, like startFailures.
	deferredSince map[string]time.Time
}

func runDaemon() error {
	p := resolvePaths()
	client, err := newHerdrClient()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.StateDir, 0o755); err != nil {
		return err
	}
	release, err := acquireLock(p.LockFile())
	if err != nil {
		return err
	}
	defer release()
	seedExamples(p.ActionsDir())
	if err := markInterrupted(p.RunLogFile()); err != nil {
		log.Printf("interrupted-run scan: %v", err)
	}

	d := &daemon{
		paths:          p,
		client:         client,
		state:          loadState(p.StateFile()),
		startTimeout:   startTimeout,
		settle:         shellSettle,
		pause:          pollPause,
		resend:         resendAfter,
		notifiedErrors: map[string]bool{},
		startFailures:  map[string]int{},
		deferredSince:  map[string]time.Time{},
	}
	d.openLog(os.Getenv(logPathEnv))
	defer d.closeLog()
	log.Printf("shepherd %s: daemon started (config %s)", version, p.ConfigDir)
	if err := client.notify("Shepherd daemon started", "", "none"); err != nil {
		log.Printf("notify failed: %v", err)
	}

	// A signalled shutdown has to return through here, so the deferred
	// release actually runs and the next daemon can take the lock.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d.tick()
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.tick()
		case <-ctx.Done():
			log.Printf("shepherd: shutting down")
			return nil
		}
	}
}

// openLog takes ownership of the log file spawnDetached created, so the tick
// loop can rotate it. Without the env var the inherited fd redirect stands.
func (d *daemon) openLog(path string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("open log %s: %v", path, err)
		return
	}
	d.logFile, d.logPath = f, path
	log.SetOutput(f)
}

func (d *daemon) closeLog() {
	if d.logFile == nil {
		return
	}
	log.SetOutput(os.Stderr)
	d.logFile.Close()
	d.logFile = nil
}

func (d *daemon) rotateLog() {
	if d.logFile == nil {
		return
	}
	info, err := d.logFile.Stat()
	if err != nil || info.Size() <= daemonLogMaxBytes {
		return
	}
	d.logFile.Close()
	renameErr := os.Rename(d.logPath, d.logPath+".1")
	f, err := os.OpenFile(d.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.SetOutput(os.Stderr)
		d.logFile = nil
		log.Printf("reopen log %s: %v", d.logPath, err)
		return
	}
	d.logFile = f
	log.SetOutput(f)
	if renameErr != nil {
		log.Printf("rotate log %s: %v", d.logPath, renameErr)
	}
}

func (d *daemon) tick() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("tick panic: %v\n%s", r, debug.Stack())
		}
	}()
	d.rotateLog()
	now := time.Now()
	actions, fileErrs, err := LoadActions(d.paths.ActionsDir())
	if err != nil {
		log.Printf("config error: %v", err)
		d.state.beat(now)
		d.saveState()
		return
	}
	d.notifyConfigErrors(fileErrs)

	names := map[string]bool{}
	for _, a := range actions {
		names[a.Name] = true
		if !a.IsEnabled() || runLockHeld(d.paths.StateDir, a.Name) {
			continue
		}
		if fire, stamp := d.due(a, now); fire {
			prev := d.state.lastRun(a.Name)
			d.state.setLastRun(a.Name, stamp)
			// A fresh occurrence supersedes a pending deferral retry.
			d.clearDefer(a.Name)
			go d.fire(a, prev)
		} else if d.deferPending(a.Name) {
			// A deferred script is retried every tick until it runs or its
			// retry window closes.
			go d.fire(a, d.state.lastRun(a.Name))
		}
	}
	if len(fileErrs) == 0 {
		// A file that failed to parse is missing from names; pruning on that
		// tick would drop its schedule and re-fire it early once fixed.
		d.state.prune(names)
		d.pruneDefers(names)
	}
	d.state.beat(now)
	d.saveState()
}

func (d *daemon) saveState() {
	if err := d.state.save(d.paths.StateFile()); err != nil {
		log.Printf("state save error: %v", err)
	}
}

// due reports whether the action should fire now, and with which last-run
// stamp. Routines are stamped with the occurrence time itself so the schedule
// never drifts; heartbeats with now.
//
// Missed-run policy: a routine occurrence more than catchUpGrace in the past
// is consumed without running — both on daemon restart and after a system
// sleep. Overdue heartbeats simply fire at the next opportunity inside
// working hours.
func (d *daemon) due(a *Action, now time.Time) (bool, time.Time) {
	last := d.state.lastRun(a.Name)
	if last.After(now) {
		// Clock stepped backwards; a future stamp would suppress the
		// schedule until wall time catches up.
		d.state.setLastRun(a.Name, now)
		last = now
	}
	switch a.Kind {
	case KindHeartbeat:
		if wh := a.Heartbeat.WorkingHours; wh != nil && !wh.Contains(now) {
			return false, time.Time{}
		}
		if last.IsZero() {
			return true, now
		}
		return !now.Before(a.Heartbeat.NextHeartbeat(last)), now
	case KindRoutine, KindScript:
		// Clamping the anchor to the grace window is what drops missed
		// occurrences: a daemon that was asleep or stopped resumes at the
		// next one instead of walking every occurrence it was away for.
		anchor := now.Add(-catchUpGrace)
		if last.After(anchor) {
			anchor = last
		}
		next, err := nextOccurrence(a, anchor)
		if err != nil {
			return false, time.Time{}
		}
		if !last.IsZero() && sameWallClock(next, last) {
			// DST fall-back repeats an hour; this occurrence already ran.
			next, err = nextOccurrence(a, next)
			if err != nil {
				return false, time.Time{}
			}
		}
		if now.Before(next) {
			return false, time.Time{}
		}
		return true, next
	}
	return false, time.Time{}
}

// notifyConfigErrors notifies once per distinct error. The set is rebuilt from
// this tick's errors, so it stays bounded and an error that returns after a
// fix notifies again.
func (d *daemon) notifyConfigErrors(fileErrs []error) {
	seen := make(map[string]bool, len(fileErrs))
	for _, ferr := range fileErrs {
		key := ferr.Error()
		seen[key] = true
		log.Printf("config error: %v", ferr)
		if !d.notifiedErrors[key] {
			d.notify("Shepherd: config error", key, "none")
		}
	}
	d.notifiedErrors = seen
}

func (d *daemon) notify(title, body, sound string) {
	if err := d.client.notify(title, body, sound); err != nil {
		log.Printf("notify failed (%s): %v", title, err)
	}
}

func (d *daemon) fire(a *Action, prevLast time.Time) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s: panic: %v\n%s", a.Name, r, debug.Stack())
		}
	}()
	release, ok, err := tryRunLock(d.paths.StateDir, a.Name)
	if err != nil {
		log.Printf("%s: run lock: %v", a.Name, err)
		return
	}
	if !ok {
		log.Printf("%s: skipped, a run is already in progress", a.Name)
		return
	}
	defer release()

	began := time.Now()
	var status, detail string
	var startFailed bool
	switch a.Kind {
	case KindScript:
		status, detail = d.runScript(a)
		if status == "deferred" {
			record, expired := d.noteDefer(a)
			if expired {
				status = "deferred-expired"
				d.notify("Shepherd: "+a.Name+" never ran",
					fmt.Sprintf("Kept deferring past its %dm retry window", a.DeferRetryMinutes), "none")
			}
			if !record {
				// A retry is pending; only the spell's first deferral and its
				// final outcome reach the run history.
				log.Printf("%s: deferred, will retry (%s)", a.Name, detail)
				return
			}
		} else {
			d.clearDefer(a.Name)
		}
	default:
		status, detail, startFailed = d.runAgent(a)
	}

	if startFailed {
		d.mu.Lock()
		d.startFailures[a.Name]++
		attempts := d.startFailures[a.Name]
		d.mu.Unlock()
		if attempts < maxStartAttempts {
			// Give the occurrence back so the next tick retries it.
			d.state.setLastRun(a.Name, prevLast)
			log.Printf("%s: start failed (attempt %d/%d), will retry: %s", a.Name, attempts, maxStartAttempts, detail)
			return
		}
		d.notify("Shepherd: "+a.Name+" could not start", detail, "none")
	}
	d.mu.Lock()
	delete(d.startFailures, a.Name)
	d.mu.Unlock()

	if a.Kind == KindHeartbeat {
		// Stamp completion, not start: a run longer than the interval must
		// not become due again the moment it finishes.
		d.state.setLastRun(a.Name, time.Now())
	}
	d.state.setStatus(a.Name, status)
	d.saveState()
	d.appendRun(runRecord{
		At: time.Now(), Action: a.Name, Kind: a.Kind, Status: status, Detail: detail,
		DurationSecs: durationSecs(began),
	})
	log.Printf("%s: %s (%s)", a.Name, status, detail)
}

func (d *daemon) appendRun(r runRecord) {
	if err := appendRunLog(d.paths.RunLogFile(), r); err != nil {
		log.Printf("%s: run log: %v", r.Action, err)
	}
}

func (d *daemon) runScript(a *Action) (status, detail string) {
	out := &tailBuffer{max: outputTailMax}
	err := runScriptOnce(a, out)
	switch {
	case err == nil:
		return "completed", out.String()
	case errors.Is(err, errScriptTimeout):
		d.notify("Shepherd: "+a.Name+" timed out", "", "none")
		return "error", err.Error()
	case isDeferExit(err):
		// The script asked to be retried later; that is not a failure and
		// must not raise a notification.
		return "deferred", out.String()
	default:
		d.notify("Shepherd: "+a.Name+" failed", tailString(out.String(), 200), "none")
		return "error", fmt.Sprintf("%v: %s", err, out.String())
	}
}

// noteDefer registers a deferred script run and decides its fate. record
// reports whether a run record should be written now: the spell's first
// deferral and its expiry are recorded, retries in between are not. expired
// means the retry window is spent and this deferral is final.
func (d *daemon) noteDefer(a *Action) (record, expired bool) {
	window := time.Duration(a.DeferRetryMinutes) * time.Minute
	if window <= 0 {
		return true, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deferredSince == nil {
		d.deferredSince = map[string]time.Time{}
	}
	first, seen := d.deferredSince[a.Name]
	if !seen {
		d.deferredSince[a.Name] = time.Now()
		return true, false
	}
	if time.Since(first) < window {
		return false, false
	}
	delete(d.deferredSince, a.Name)
	return true, true
}

func (d *daemon) clearDefer(name string) {
	d.mu.Lock()
	delete(d.deferredSince, name)
	d.mu.Unlock()
}

func (d *daemon) deferPending(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.deferredSince[name]
	return ok
}

func (d *daemon) pruneDefers(names map[string]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for name := range d.deferredSince {
		if !names[name] {
			delete(d.deferredSince, name)
		}
	}
}

// runAgent launches the action's agent session in a fresh workspace and
// follows it to completion. startFailed reports failures before the agent was
// ever detected — those occurrences are retryable; later failures are not,
// because the session may be doing real work.
func (d *daemon) runAgent(a *Action) (status, detail string, startFailed bool) {
	command, err := a.AgentCommand()
	if err != nil {
		return "error", err.Error(), false
	}
	wsID, paneID, err := launchAgentWorkspace(d.client, a, d.settle)
	switch {
	case err == nil:
	case errors.Is(err, errLaunchCreate):
		return "error", err.Error(), true
	case errors.Is(err, errLaunchSubmit):
		// The workspace is already closed again. Nothing ran, but a pane that
		// rejects input needs a person, not another attempt.
		return "attention", err.Error(), false
	default:
		return "error", err.Error(), false
	}
	d.appendRun(runRecord{
		At: time.Now(), Action: a.Name, Kind: a.Kind, Status: "started", Detail: "workspace " + wsID,
	})

	// Phase 1: agent.wait errors with agent_not_found until herdr detects the
	// agent, and a fresh one reports idle before its first turn registers —
	// so only working/blocked count as started.
	startDeadline := time.Now().Add(d.startTimeout)
	resendAt := time.Now().Add(d.resend)
	resent := false
	state := ""
	for state == "" {
		s, werr := d.client.agentWait(paneID, []string{"working", "blocked"}, startWaitSliceMS)
		switch {
		case werr == nil:
			state = s
		case isTimeout(werr):
			// A run that finished between waits never showed working.
			if probe, perr := d.client.agentWait(paneID, []string{"done"}, doneProbeMS); perr == nil {
				state = probe
			}
		case isPaneNotFound(werr):
			return "cancelled", "pane closed before the agent started; workspace " + wsID, false
		case isAgentNotFound(werr):
			time.Sleep(d.pause)
		default:
			// Transport errors are transient; keep waiting rather than tear
			// down a workspace that may be running the agent perfectly well.
			time.Sleep(d.pause)
		}
		if state != "" {
			break
		}
		if !resent && time.Now().After(resendAt) {
			resent = true
			if rerr := d.client.runCommand(paneID, command); rerr != nil {
				log.Printf("%s: resend command: %v", a.Name, rerr)
			}
		}
		if time.Now().After(startDeadline) {
			d.notify("Shepherd: "+a.Name+" did not start", "No agent activity in its pane", "none")
			return "attention", "agent never started working within start timeout; workspace " + wsID, false
		}
	}

	// Phase 2: follow the session. blocked leaves the until-set once
	// notified — waiting on a state the agent is already in returns
	// instantly, and re-arming it would spin the socket.
	deadline := time.Now().Add(time.Duration(a.WatchMinutes) * time.Minute)
	until := []string{"done", "idle", "blocked"}
	notified := false
	exited := false
	idled := false
	for state != "done" && !idled {
		switch state {
		case "blocked":
			if !notified {
				notified = true
				until = []string{"done", "idle"}
				d.notify("Shepherd: "+a.Name+" needs attention", "The session is waiting on input", "request")
			}
		case "idle":
			s, cerr := d.confirmIdle(paneID)
			if cerr != nil {
				// Hand it to the shared wait path below, which can tell a
				// closed pane from a transport error.
				state = ""
				continue
			}
			if s == "idle" {
				idled = true
			}
			state = s
			continue
		}
		if time.Now().After(deadline) {
			d.notify("Shepherd: "+a.Name+" still running", fmt.Sprintf("Exceeded watch window of %dm", a.WatchMinutes), "none")
			return "attention", "watch window exceeded; session left open; workspace " + wsID, false
		}
		time.Sleep(d.pause)
		state, err = d.client.agentWait(paneID, until, waitSliceMS)
		if err != nil {
			if isTimeout(err) {
				state = ""
				continue
			}
			if isAgentNotFound(err) {
				alive, perr := d.client.paneExists(paneID)
				if perr != nil {
					// Only a confirmed missing pane ends the run.
					state = ""
					continue
				}
				if !alive {
					return "cancelled", "pane closed during run; workspace " + wsID, false
				}
				// The agent process exited (crash or clean exit to shell).
				exited = true
				break
			}
			return "error", "agent wait: " + err.Error() + "; workspace " + wsID, false
		}
	}

	switch {
	case exited:
		status, detail = "attention", "agent exited"
	case notified:
		status, detail = "attention", "completed after needing attention"
	default:
		status, detail = "completed", "session finished"
	}
	if a.AutoClose {
		if cerr := d.client.workspaceClose(wsID); cerr != nil {
			detail += "; close failed: " + cerr.Error()
		} else {
			detail += "; closed"
		}
	}
	return status, detail + "; workspace " + wsID, false
}

// confirmIdle re-arms after a phase-2 idle. An agent between turns reports
// idle, so only an idle that survives the re-arm ends the watch; the done
// probe catches a session that finished inside the re-arm window.
func (d *daemon) confirmIdle(paneID string) (string, error) {
	s, err := d.client.agentWait(paneID, []string{"working", "blocked"}, idleConfirmMS)
	if err == nil {
		return s, nil
	}
	if !isTimeout(err) {
		return "", err
	}
	if probe, perr := d.client.agentWait(paneID, []string{"done"}, doneProbeMS); perr == nil {
		return probe, nil
	}
	return "idle", nil
}

// tailBuffer keeps only the last max bytes written, so a chatty script cannot
// grow daemon memory without bound.
type tailBuffer struct {
	max  int
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if len(b.data) > b.max {
		b.data = append(b.data[:0], b.data[len(b.data)-b.max:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return strings.TrimSpace(string(b.data)) }

const tailMarker = "…"

// tailString keeps the last n bytes of s behind an ellipsis, advanced to the
// next rune boundary so the result is never a broken UTF-8 sequence.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	return tailMarker + cut
}

// acquireLock takes a kernel flock on the lock file, held for the process
// lifetime. Unlike a pid file, the kernel releases it however the process
// dies — no stale locks after SIGKILL, no pid-reuse false positives. The pid
// inside is diagnostic only.
func acquireLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		data, _ := os.ReadFile(path)
		f.Close()
		return nil, fmt.Errorf("daemon already running (pid %s)", strings.TrimSpace(string(data)))
	}
	if err := f.Truncate(0); err != nil {
		log.Printf("lock file truncate: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		log.Printf("lock file seek: %v", err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		log.Printf("lock file write: %v", err)
	}
	return func() {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			log.Printf("lock file unlock: %v", err)
		}
		if err := f.Close(); err != nil {
			log.Printf("lock file close: %v", err)
		}
	}, nil
}

const exampleAction = `# Example shepherd action. Rename this file, set enabled = true, and it will
# be picked up within 30 seconds. One TOML file per action; see the README for
# heartbeat, routine, and script examples.
name = "example-heartbeat"
kind = "heartbeat"
directory = "~"
enabled = false
prompt = "Say hello, then exit."

[heartbeat]
interval_minutes = 60
`

func seedExamples(actionsDir string) {
	if _, err := os.Stat(actionsDir); err == nil {
		return
	}
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		log.Printf("seed examples: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(actionsDir, "example-heartbeat.toml"), []byte(exampleAction), 0o644); err != nil {
		log.Printf("seed examples: %v", err)
	}
}
