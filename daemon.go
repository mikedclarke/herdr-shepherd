package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	tickInterval  = 30 * time.Second
	startTimeout  = 2 * time.Minute
	waitSliceMS   = 60_000
	outputTailMax = 4096
)

type daemon struct {
	paths   paths
	client  *herdrClient
	state   *daemonState
	started time.Time

	mu      sync.Mutex
	running map[string]bool
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

	d := &daemon{
		paths:   p,
		client:  client,
		state:   loadState(p.StateFile()),
		started: time.Now(),
		running: map[string]bool{},
	}
	log.Printf("shepherd %s: daemon started (config %s)", version, p.ConfigDir)

	d.tick()
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for range ticker.C {
		d.tick()
	}
	return nil
}

func (d *daemon) tick() {
	now := time.Now()
	actions, err := LoadActions(d.paths.ActionsDir())
	if err != nil {
		log.Printf("config error: %v", err)
		d.state.HeartbeatAt = now
		d.state.save(d.paths.StateFile())
		return
	}
	for _, a := range actions {
		if !a.IsEnabled() || d.isRunning(a.Name) {
			continue
		}
		if d.due(a, now) {
			d.setRunning(a.Name, true)
			d.state.forAction(a.Name).LastRunAt = now
			go d.fire(a)
		}
	}
	d.state.HeartbeatAt = now
	if err := d.state.save(d.paths.StateFile()); err != nil {
		log.Printf("state save error: %v", err)
	}
}

// due reports whether the action's next scheduled run is in the past. Routines
// anchor on the later of last-run and daemon start, so occurrences missed while
// the daemon was down are dropped rather than backfilled. Heartbeats anchor on
// last-run alone: an overdue heartbeat should fire as soon as it can.
func (d *daemon) due(a *Action, now time.Time) bool {
	last := d.state.forAction(a.Name).LastRunAt
	switch a.Kind {
	case KindHeartbeat:
		if wh := a.Heartbeat.WorkingHours; wh != nil && !wh.Contains(now) {
			return false
		}
		if last.IsZero() {
			return true
		}
		return !now.Before(a.Heartbeat.NextHeartbeat(last))
	case KindRoutine, KindScript:
		anchor := d.started
		if last.After(anchor) {
			anchor = last
		}
		next, err := a.Routine.NextRoutine(anchor)
		if err != nil {
			return false
		}
		return !now.Before(next)
	}
	return false
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

func (d *daemon) fire(a *Action) {
	defer d.setRunning(a.Name, false)
	var status, detail string
	switch a.Kind {
	case KindScript:
		status, detail = d.runScript(a)
	default:
		status, detail = d.runAgent(a)
	}
	d.state.forAction(a.Name).LastStatus = status
	d.state.save(d.paths.StateFile())
	appendRunLog(d.paths.RunLogFile(), runRecord{
		At: time.Now(), Action: a.Name, Kind: a.Kind, Status: status, Detail: detail,
	})
	log.Printf("%s: %s (%s)", a.Name, status, detail)
}

func (d *daemon) runScript(a *Action) (status, detail string) {
	cmd := exec.Command("sh", "-c", a.Command)
	cmd.Dir = a.Dir()
	done := make(chan error, 1)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return "error", err.Error()
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			d.client.notify("Shepherd: "+a.Name+" failed", tail(out.String(), 200), "request")
			return "error", fmt.Sprintf("%v: %s", err, tail(out.String(), outputTailMax))
		}
		return "completed", tail(out.String(), outputTailMax)
	case <-time.After(time.Duration(a.TimeoutMinutes) * time.Minute):
		cmd.Process.Kill()
		d.client.notify("Shepherd: "+a.Name+" timed out", "", "request")
		return "error", fmt.Sprintf("timed out after %dm", a.TimeoutMinutes)
	}
}

func (d *daemon) runAgent(a *Action) (status, detail string) {
	command, err := a.AgentCommand()
	if err != nil {
		return "error", err.Error()
	}
	wsID, paneID, err := d.client.workspaceCreate(a.Dir(), a.Name)
	if err != nil {
		return "error", "workspace create: " + err.Error()
	}
	if err := d.client.runCommand(paneID, command); err != nil {
		return "error", "run command: " + err.Error()
	}

	first, err := d.client.agentWait(paneID, []string{"working", "done", "blocked"}, int(startTimeout.Milliseconds()))
	if err != nil {
		if isTimeout(err) {
			d.client.notify("Shepherd: "+a.Name+" did not start", "No agent detected in its pane", "request")
			return "attention", "agent not detected within start timeout"
		}
		return "error", "agent wait: " + err.Error()
	}

	deadline := time.Now().Add(time.Duration(a.WatchMinutes) * time.Minute)
	state := first
	notified := false
	for state != "done" {
		if state == "blocked" && !notified {
			notified = true
			d.client.notify("Shepherd: "+a.Name+" needs attention", "The session is waiting on input", "request")
		}
		if time.Now().After(deadline) {
			d.client.notify("Shepherd: "+a.Name+" still running", fmt.Sprintf("Exceeded watch window of %dm", a.WatchMinutes), "request")
			return "attention", "watch window exceeded; session left open"
		}
		state, err = d.client.agentWait(paneID, []string{"done", "blocked"}, waitSliceMS)
		if err != nil {
			if isTimeout(err) {
				state = ""
				continue
			}
			// The pane may have been closed by hand mid-run.
			if _, gerr := d.client.agentStatus(paneID); gerr != nil {
				return "cancelled", "pane went away: " + gerr.Error()
			}
			return "error", "agent wait: " + err.Error()
		}
	}

	if notified {
		status = "attention"
		detail = "completed after needing attention"
	} else {
		status = "completed"
		detail = "workspace " + wsID
	}
	if a.AutoCloses() {
		if err := d.client.workspaceClose(wsID); err != nil {
			return status, detail + "; close failed: " + err.Error()
		}
		detail = strings.TrimSuffix(detail, "workspace "+wsID) + "closed workspace"
	}
	return status, strings.TrimSpace(detail)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// acquireLock takes a pid-file lock so two daemons never double-fire. A lock
// whose pid is dead is stale and replaced.
func acquireLock(path string) (func(), error) {
	if data, err := os.ReadFile(path); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			if syscall.Kill(pid, 0) == nil {
				return nil, fmt.Errorf("daemon already running (pid %d)", pid)
			}
		}
		os.Remove(path)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, err
	}
	return func() { os.Remove(path) }, nil
}
