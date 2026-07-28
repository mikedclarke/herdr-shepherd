package main

import (
	"sync"
	"testing"
	"time"
)

// scriptedHerdr scripts agent.wait responses in call order and records every
// until-set it was asked to wait on.
type scriptedHerdr struct {
	mu       sync.Mutex
	waits    []waitStep
	waitSets [][]string
	notices  []string
	closed   []string
}

type waitStep struct {
	state string
	err   error
}

func (f *scriptedHerdr) workspaceCreate(cwd, label string, env map[string]string) (string, string, error) {
	return "ws1", "p1", nil
}

func (f *scriptedHerdr) workspaceClose(workspaceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, workspaceID)
	return nil
}

func (f *scriptedHerdr) runCommand(paneID, command string) error { return nil }
func (f *scriptedHerdr) paneExists(paneID string) bool           { return true }

func (f *scriptedHerdr) notify(title, body, sound string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notices = append(f.notices, title)
	return nil
}

func (f *scriptedHerdr) agentWait(target string, until []string, timeoutMS int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waitSets = append(f.waitSets, until)
	if len(f.waits) == 0 {
		return "", &herdrError{Code: "timeout", Message: "scripted steps exhausted"}
	}
	step := f.waits[0]
	f.waits = f.waits[1:]
	return step.state, step.err
}

func timeoutErr() error       { return &herdrError{Code: "timeout", Message: "t"} }
func agentNotFoundErr() error { return &herdrError{Code: "agent_not_found", Message: "n"} }

func agentTestDaemon(fake *scriptedHerdr) *daemon {
	return &daemon{
		client:       fake,
		state:        &daemonState{Actions: map[string]*actionState{}},
		started:      time.Now(),
		startTimeout: time.Minute,
		pause:        time.Millisecond,
		running:      map[string]bool{},
	}
}

func watchedAction() *Action {
	a := &Action{Name: "digest", Kind: KindRoutine, Directory: "/tmp", Prompt: "x", WatchMinutes: 5}
	a.applyDefaults()
	return a
}

func TestRunAgentStartupIdleDoesNotComplete(t *testing.T) {
	// A freshly launched agent reports idle before its first turn registers
	// as working. That must not end the watch: the run only completes after
	// working has been observed and the agent reaches done.
	fake := &scriptedHerdr{waits: []waitStep{
		{err: agentNotFoundErr()}, // agent not detected yet
		{err: timeoutErr()},       // detected but still idle: no working/blocked
		{err: timeoutErr()},       // done-probe: idle is not done
		{state: "working"},
		{state: "done"},
	}}
	d := agentTestDaemon(fake)
	status, detail, startFailed := d.runAgent(watchedAction())
	if status != "completed" || startFailed {
		t.Fatalf("got status=%q startFailed=%v detail=%q", status, startFailed, detail)
	}
	if len(fake.waits) != 0 {
		t.Fatalf("run completed %d scripted steps early", len(fake.waits))
	}
	for _, until := range fake.waitSets[:4] {
		for _, s := range until {
			if s == "idle" {
				t.Fatal("startup waits must not treat idle as terminal")
			}
		}
	}
}

func TestRunAgentFastRunCompletesViaDoneProbe(t *testing.T) {
	// A run that finishes inside the first wait slice never shows working to
	// the watcher; the done-probe must still report it completed.
	fake := &scriptedHerdr{waits: []waitStep{
		{err: timeoutErr()},
		{state: "done"}, // probe
	}}
	d := agentTestDaemon(fake)
	status, _, startFailed := d.runAgent(watchedAction())
	if status != "completed" || startFailed {
		t.Fatalf("got status=%q startFailed=%v", status, startFailed)
	}
}

func TestRunAgentNeverWorkingReportsAttention(t *testing.T) {
	// An agent that sits idle forever (command never submitted, stalled pane)
	// must surface as attention, not success.
	fake := &scriptedHerdr{}
	d := agentTestDaemon(fake)
	d.startTimeout = 10 * time.Millisecond
	status, detail, startFailed := d.runAgent(watchedAction())
	if status != "attention" || startFailed {
		t.Fatalf("got status=%q startFailed=%v detail=%q", status, startFailed, detail)
	}
	if len(fake.notices) == 0 {
		t.Fatal("a run that never starts must notify")
	}
}

func TestRunAgentBlockedNotifiesThenCompletes(t *testing.T) {
	fake := &scriptedHerdr{waits: []waitStep{
		{state: "blocked"},
		{state: "done"},
	}}
	d := agentTestDaemon(fake)
	status, detail, _ := d.runAgent(watchedAction())
	if status != "attention" || detail != "completed after needing attention" {
		t.Fatalf("got status=%q detail=%q", status, detail)
	}
	if len(fake.notices) != 1 {
		t.Fatalf("expected one blocked notification, got %v", fake.notices)
	}
}
