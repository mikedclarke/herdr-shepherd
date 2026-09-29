package main

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// scriptedHerdr scripts agent.wait responses in call order and records what it
// was asked to do. The zero value is a herdr that accepts everything and times
// every wait out.
type scriptedHerdr struct {
	mu           sync.Mutex
	waits        []waitStep
	createErr    error
	runErr       error
	paneErr      error
	paneExistsFn func(string) bool
	waitSets     [][]string
	commands     []string
	notices      []string
	closed       []string
	env          map[string]string
	spaces       []workspaceInfo
	tabs         []string
	renamed      map[string]string
}

type waitStep struct {
	state string
	err   error
}

func (f *scriptedHerdr) workspaceList() ([]workspaceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	return append([]workspaceInfo(nil), f.spaces...), nil
}

// workspaceCreate opens ws1 labelled as asked; its root tab is ws1:t1.
func (f *scriptedHerdr) workspaceCreate(cwd, label string, env map[string]string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", "", "", f.createErr
	}
	f.env = env
	f.spaces = append(f.spaces, workspaceInfo{WorkspaceID: "ws1", Label: label, Number: len(f.spaces) + 1})
	f.tabs = append(f.tabs, "ws1:t1")
	return "ws1", "ws1:t1", "p1", nil
}

func (f *scriptedHerdr) tabCreate(workspaceID, cwd, label string, env map[string]string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", "", f.createErr
	}
	f.env = env
	tab := fmt.Sprintf("%s:t%d", workspaceID, len(f.tabs)+2)
	f.tabs = append(f.tabs, tab)
	f.renamed = mapSet(f.renamed, tab, label)
	return tab, "p1", nil
}

func (f *scriptedHerdr) tabRename(tabID, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renamed = mapSet(f.renamed, tabID, label)
	return nil
}

func (f *scriptedHerdr) tabClose(tabID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, tabID)
	return nil
}

func mapSet(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}

func (f *scriptedHerdr) runCommand(paneID, command string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	return f.runErr
}

func (f *scriptedHerdr) paneExists(paneID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paneErr != nil {
		return false, f.paneErr
	}
	if f.paneExistsFn != nil {
		return f.paneExistsFn(paneID), nil
	}
	return true, nil
}

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

func (f *scriptedHerdr) counts() (waitsLeft, commands, notices, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waits), len(f.commands), len(f.notices), len(f.closed)
}

func timeoutErr() error       { return &herdrError{Code: "timeout", Message: "t"} }
func agentNotFoundErr() error { return &herdrError{Code: "agent_not_found", Message: "n"} }
func paneNotFoundErr() error  { return &herdrError{Code: "pane_not_found", Message: "p"} }

func agentTestDaemon(t *testing.T, fake *scriptedHerdr) *daemon {
	t.Helper()
	d := testDaemon(t)
	d.client = fake
	return d
}

func watchedAction() *Action {
	a := &Action{Name: "nightly-report", Kind: KindRoutine, Directory: "/tmp", Prompt: "x", WatchMinutes: 5}
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
	d := agentTestDaemon(t, fake)
	status, detail, startFailed := d.runAgent(watchedAction(), "")
	if status != "completed" || startFailed {
		t.Fatalf("got status=%q startFailed=%v detail=%q", status, startFailed, detail)
	}
	if left, _, _, _ := fake.counts(); left != 0 {
		t.Fatalf("run completed %d scripted steps early", left)
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
	d := agentTestDaemon(t, fake)
	status, _, startFailed := d.runAgent(watchedAction(), "")
	if status != "completed" || startFailed {
		t.Fatalf("got status=%q startFailed=%v", status, startFailed)
	}
}

func TestRunAgentNeverWorkingReportsAttention(t *testing.T) {
	// An agent that sits idle forever (command never submitted, stalled pane)
	// must surface as attention, not success.
	fake := &scriptedHerdr{}
	d := agentTestDaemon(t, fake)
	d.startTimeout = 10 * time.Millisecond
	status, detail, startFailed := d.runAgent(watchedAction(), "")
	if status != "attention" || startFailed {
		t.Fatalf("got status=%q startFailed=%v detail=%q", status, startFailed, detail)
	}
	if _, _, notices, _ := fake.counts(); notices == 0 {
		t.Fatal("a run that never starts must notify")
	}
}

func TestRunAgentBlockedNotifiesThenCompletes(t *testing.T) {
	fake := &scriptedHerdr{waits: []waitStep{
		{state: "blocked"},
		{state: "done"},
	}}
	d := agentTestDaemon(t, fake)
	status, detail, _ := d.runAgent(watchedAction(), "")
	if status != "attention" || detail != "completed after needing attention; tab ws1:t1" {
		t.Fatalf("got status=%q detail=%q", status, detail)
	}
	if _, _, notices, _ := fake.counts(); notices != 1 {
		t.Fatalf("expected one blocked notification, got %v", fake.notices)
	}
}

func TestRunAgentTransientIdleKeepsWatching(t *testing.T) {
	// idle mid-run means "between turns" far more often than "finished": the
	// re-arm sees the agent go back to working, and only the later done ends
	// the watch.
	fake := &scriptedHerdr{waits: []waitStep{
		{state: "working"},
		{state: "idle"},
		{state: "working"}, // re-arm: back to work
		{state: "done"},
	}}
	a := watchedAction()
	a.AutoClose = true
	d := agentTestDaemon(t, fake)
	status, detail, _ := d.runAgent(a, "")
	if status != "completed" || detail != "session finished; closed; tab ws1:t1" {
		t.Fatalf("got status=%q detail=%q", status, detail)
	}
	left, _, _, closed := fake.counts()
	if left != 0 {
		t.Fatalf("transient idle ended the watch %d steps early", left)
	}
	if closed != 1 {
		t.Fatalf("auto_close must fire once, at the terminal state: %v", fake.closed)
	}
}

func TestRunAgentConfirmedIdleCompletes(t *testing.T) {
	// An idle that survives the re-arm and the done-probe is the real thing.
	fake := &scriptedHerdr{waits: []waitStep{
		{state: "working"},
		{state: "idle"},
		{err: timeoutErr()}, // re-arm: still idle
		{err: timeoutErr()}, // done-probe: not done either
	}}
	d := agentTestDaemon(t, fake)
	status, detail, startFailed := d.runAgent(watchedAction(), "")
	if status != "completed" || startFailed {
		t.Fatalf("got status=%q startFailed=%v detail=%q", status, startFailed, detail)
	}
}

func TestRunAgentWorkspaceCreateFailureIsRetryable(t *testing.T) {
	fake := &scriptedHerdr{createErr: errors.New("socket down")}
	d := agentTestDaemon(t, fake)
	status, detail, startFailed := d.runAgent(watchedAction(), "")
	if status != "error" || !startFailed {
		t.Fatalf("got status=%q startFailed=%v detail=%q", status, startFailed, detail)
	}
}

func TestRunAgentSubmitFailureClosesWorkspace(t *testing.T) {
	// Nothing was launched, but a pane that will not take input needs a
	// person; retrying it every tick would open a tab each time.
	fake := &scriptedHerdr{runErr: errors.New("pane busy")}
	d := agentTestDaemon(t, fake)
	status, detail, startFailed := d.runAgent(watchedAction(), "")
	if status != "attention" || startFailed {
		t.Fatalf("got status=%q startFailed=%v detail=%q", status, startFailed, detail)
	}
	if len(fake.closed) != 1 || fake.closed[0] != "ws1:t1" {
		t.Fatalf("failed submit must close the tab, closed=%v", fake.closed)
	}
}

func TestRunAgentPaneClosedDuringStartupIsCancelled(t *testing.T) {
	fake := &scriptedHerdr{waits: []waitStep{
		{err: agentNotFoundErr()},
		{err: paneNotFoundErr()},
	}}
	d := agentTestDaemon(t, fake)
	status, detail, startFailed := d.runAgent(watchedAction(), "")
	if status != "cancelled" || startFailed {
		t.Fatalf("got status=%q startFailed=%v detail=%q", status, startFailed, detail)
	}
}

func TestRunAgentResendsCommandOnce(t *testing.T) {
	// A pane whose shell was not ready swallows the submitted line; one
	// resend recovers it, and only one is ever sent.
	fake := &scriptedHerdr{}
	d := agentTestDaemon(t, fake)
	d.resend = 0
	d.startTimeout = 30 * time.Millisecond
	if status, _, _ := d.runAgent(watchedAction(), ""); status != "attention" {
		t.Fatalf("got status=%q", status)
	}
	if _, commands, _, _ := fake.counts(); commands != 2 {
		t.Fatalf("expected the initial submit plus one resend, got %d", commands)
	}
}

func TestRunAgentUnreachablePaneKeepsWatching(t *testing.T) {
	// agent_not_found plus a pane check that cannot complete is not evidence
	// the pane is gone.
	fake := &scriptedHerdr{
		waits:   []waitStep{{state: "working"}, {err: agentNotFoundErr()}, {state: "done"}},
		paneErr: errors.New("connect herdr socket"),
	}
	d := agentTestDaemon(t, fake)
	status, detail, _ := d.runAgent(watchedAction(), "")
	if status != "completed" {
		t.Fatalf("got status=%q detail=%q", status, detail)
	}
}

func TestRunAgentClosedPaneDuringRunIsCancelled(t *testing.T) {
	fake := &scriptedHerdr{
		waits:        []waitStep{{state: "working"}, {err: agentNotFoundErr()}},
		paneExistsFn: func(string) bool { return false },
	}
	d := agentTestDaemon(t, fake)
	status, detail, _ := d.runAgent(watchedAction(), "")
	if status != "cancelled" {
		t.Fatalf("got status=%q detail=%q", status, detail)
	}
}

func TestRunAgentExitedAgentNeedsAttention(t *testing.T) {
	fake := &scriptedHerdr{
		waits:        []waitStep{{state: "working"}, {err: agentNotFoundErr()}},
		paneExistsFn: func(string) bool { return true },
	}
	d := agentTestDaemon(t, fake)
	status, detail, _ := d.runAgent(watchedAction(), "")
	if status != "attention" || detail != "agent exited; tab ws1:t1" {
		t.Fatalf("got status=%q detail=%q", status, detail)
	}
}

func TestRunAgentLogsStartedRecord(t *testing.T) {
	fake := &scriptedHerdr{waits: []waitStep{{state: "working"}, {state: "done"}}}
	d := agentTestDaemon(t, fake)
	d.runAgent(watchedAction(), "")
	recs := readRunLog(t, d.paths.RunLogFile())
	if len(recs) != 1 || recs[0].Status != "started" || recs[0].Detail != "tab ws1:t1" {
		t.Fatalf("expected one started record naming the workspace, got %+v", recs)
	}
	if recs[0].Trigger != "" {
		t.Errorf("a scheduled run must not be marked manual: %q", recs[0].Trigger)
	}
}
