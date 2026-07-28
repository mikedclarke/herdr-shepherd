package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
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
	// A routine occurrence older than this is dropped, not run late — waking
	// a slept machine at 08:55 must not fire the 06:00 jobs (see due).
	catchUpGrace = 10 * time.Minute
	// Retries for occurrences that failed before their agent/script started
	// (herdr restarting, socket briefly down).
	maxStartAttempts = 3
	shellSettle      = 1 * time.Second
	pollPause        = 1 * time.Second
	outputTailMax    = 4096
)

// herdrAPI is the slice of the herdr socket API the daemon uses; it exists so
// agent-watch logic is testable against a scripted fake.
type herdrAPI interface {
	workspaceCreate(cwd, label string, env map[string]string) (workspaceID, paneID string, err error)
	workspaceClose(workspaceID string) error
	runCommand(paneID, command string) error
	agentWait(target string, until []string, timeoutMS int) (string, error)
	paneExists(paneID string) bool
	notify(title, body, sound string) error
}

type daemon struct {
	paths   paths
	client  herdrAPI
	state   *daemonState
	started time.Time

	// Timing knobs; runDaemon sets the real values, tests shrink them.
	startTimeout time.Duration
	settle       time.Duration
	pause        time.Duration

	mu             sync.Mutex
	running        map[string]bool
	startFailures  map[string]int
	notifiedErrors map[string]bool
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

	d := &daemon{
		paths:          p,
		client:         client,
		state:          loadState(p.StateFile()),
		started:        time.Now(),
		startTimeout:   startTimeout,
		settle:         shellSettle,
		pause:          pollPause,
		running:        map[string]bool{},
		startFailures:  map[string]int{},
		notifiedErrors: map[string]bool{},
	}
	log.Printf("shepherd %s: daemon started (config %s)", version, p.ConfigDir)
	if err := client.notify("Shepherd daemon started", "", "none"); err != nil {
		log.Printf("notify failed: %v", err)
	}

	d.tick()
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for range ticker.C {
		d.tick()
	}
	return nil
}

func (d *daemon) tick() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("tick panic: %v\n%s", r, debug.Stack())
		}
	}()
	now := time.Now()
	actions, fileErrs, err := LoadActions(d.paths.ActionsDir())
	if err != nil {
		log.Printf("config error: %v", err)
		d.state.beat(now)
		d.saveState()
		return
	}
	for _, ferr := range fileErrs {
		d.notifyConfigError(ferr)
	}

	names := map[string]bool{}
	for _, a := range actions {
		names[a.Name] = true
		if !a.IsEnabled() || d.isRunning(a.Name) {
			continue
		}
		if fire, stamp := d.due(a, now); fire {
			prev := d.state.lastRun(a.Name)
			d.setRunning(a.Name, true)
			d.state.setLastRun(a.Name, stamp)
			go d.fire(a, prev)
		}
	}
	d.state.prune(names)
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
		anchor := d.started
		if last.After(anchor) {
			anchor = last
		}
		next, err := a.Routine.NextRoutine(anchor)
		if err != nil {
			return false, time.Time{}
		}
		if !last.IsZero() && sameWallClock(next, last) {
			// DST fall-back repeats an hour; this occurrence already ran.
			next, err = a.Routine.NextRoutine(next)
			if err != nil {
				return false, time.Time{}
			}
		}
		if now.Before(next) {
			return false, time.Time{}
		}
		if now.Sub(next) > catchUpGrace {
			log.Printf("%s: dropping missed occurrence %s", a.Name, next.Format(time.RFC3339))
			d.state.setLastRun(a.Name, next)
			return false, time.Time{}
		}
		return true, next
	}
	return false, time.Time{}
}

func (d *daemon) isRunning(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running[name]
}

func (d *daemon) setRunning(name string, v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if v {
		d.running[name] = true
	} else {
		delete(d.running, name)
	}
}

func (d *daemon) notifyConfigError(err error) {
	key := err.Error()
	d.mu.Lock()
	seen := d.notifiedErrors[key]
	d.notifiedErrors[key] = true
	d.mu.Unlock()
	log.Printf("config error: %v", err)
	if !seen {
		if nerr := d.client.notify("Shepherd: config error", key, "none"); nerr != nil {
			log.Printf("notify failed: %v", nerr)
		}
	}
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
		d.setRunning(a.Name, false)
	}()

	var status, detail string
	var startFailed bool
	switch a.Kind {
	case KindScript:
		status, detail = d.runScript(a)
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
	appendRunLog(d.paths.RunLogFile(), runRecord{
		At: time.Now(), Action: a.Name, Kind: a.Kind, Status: status, Detail: detail,
	})
	log.Printf("%s: %s (%s)", a.Name, status, detail)
}

func (d *daemon) runScript(a *Action) (status, detail string) {
	out := &tailBuffer{max: outputTailMax}
	cmd := exec.Command("sh", "-c", a.Command)
	cmd.Dir = a.Dir()
	cmd.Stdout = out
	cmd.Stderr = out
	// Its own process group, so a timeout kill reaches grandchildren — and
	// releases the output pipe cmd.Wait blocks on.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return "error", err.Error()
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			d.notify("Shepherd: "+a.Name+" failed", tailString(out.String(), 200), "none")
			return "error", fmt.Sprintf("%v: %s", err, out.String())
		}
		return "completed", out.String()
	case <-time.After(time.Duration(a.TimeoutMinutes) * time.Minute):
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		d.notify("Shepherd: "+a.Name+" timed out", "", "none")
		return "error", fmt.Sprintf("timed out after %dm", a.TimeoutMinutes)
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
	wsID, paneID, err := d.client.workspaceCreate(a.Dir(), "Shepherd · "+a.Name, map[string]string{
		"SHEPHERD_ACTION": a.Name,
	})
	if err != nil {
		return "error", "workspace create: " + err.Error(), true
	}
	time.Sleep(d.settle)
	if err := d.client.runCommand(paneID, command); err != nil {
		d.client.workspaceClose(wsID)
		return "error", "run command: " + err.Error(), true
	}

	// Phase 1: wait for herdr to detect the agent and see it actually start.
	// agent.wait errors with agent_not_found (instantly, not a timeout) until
	// detection — and a freshly launched agent then reports idle for several
	// seconds before its first turn registers as working. Accepting idle or
	// done here ends the watch seconds into a real run (a stalled routine was
	// once logged "completed" 17s after firing, silencing every later alert),
	// so only working/blocked count as started. The short done-only probe on
	// timeout keeps a run that finishes between waits from being misreported
	// as never starting; a pane that stays idle past the deadline is surfaced
	// as attention, not success.
	startDeadline := time.Now().Add(d.startTimeout)
	state := ""
	for state == "" {
		s, err := d.client.agentWait(paneID, []string{"working", "blocked"}, startWaitSliceMS)
		switch {
		case err == nil:
			state = s
		case isTimeout(err):
			if probe, perr := d.client.agentWait(paneID, []string{"done"}, 1_000); perr == nil {
				state = probe
			}
		case isAgentNotFound(err):
			time.Sleep(d.pause)
		default:
			d.client.workspaceClose(wsID)
			return "error", "agent wait: " + err.Error(), true
		}
		if state == "" && time.Now().After(startDeadline) {
			d.notify("Shepherd: "+a.Name+" did not start", "No agent activity in its pane", "none")
			return "attention", "agent never started working within start timeout", false
		}
	}

	// Phase 2: follow the session. blocked leaves the until-set once
	// notified — waiting on a state the agent is already in returns
	// instantly, and re-arming it would spin the socket.
	deadline := time.Now().Add(time.Duration(a.WatchMinutes) * time.Minute)
	until := []string{"done", "idle", "blocked"}
	notified := false
	exited := false
	for state != "done" && state != "idle" {
		if state == "blocked" && !notified {
			notified = true
			until = []string{"done", "idle"}
			d.notify("Shepherd: "+a.Name+" needs attention", "The session is waiting on input", "request")
		}
		if time.Now().After(deadline) {
			d.notify("Shepherd: "+a.Name+" still running", fmt.Sprintf("Exceeded watch window of %dm", a.WatchMinutes), "none")
			return "attention", "watch window exceeded; session left open", false
		}
		time.Sleep(d.pause)
		state, err = d.client.agentWait(paneID, until, waitSliceMS)
		if err != nil {
			if isTimeout(err) {
				state = ""
				continue
			}
			if isAgentNotFound(err) {
				if !d.client.paneExists(paneID) {
					return "cancelled", "pane closed during run", false
				}
				// The agent process exited (crash or clean exit to shell).
				exited = true
				break
			}
			return "error", "agent wait: " + err.Error(), false
		}
	}

	switch {
	case exited:
		status, detail = "attention", "agent exited"
	case notified:
		status, detail = "attention", "completed after needing attention"
	default:
		status, detail = "completed", "workspace "+wsID
	}
	if a.AutoClose {
		if err := d.client.workspaceClose(wsID); err != nil {
			return status, detail + "; close failed: " + err.Error(), false
		}
		detail = "closed workspace"
		if notified {
			detail = "completed after needing attention; closed workspace"
		}
	}
	return status, detail, false
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

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
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
	f.Truncate(0)
	f.Seek(0, 0)
	f.WriteString(strconv.Itoa(os.Getpid()))
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
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
		return
	}
	os.WriteFile(actionsDir+"/example-heartbeat.toml", []byte(exampleAction), 0o644)
}
