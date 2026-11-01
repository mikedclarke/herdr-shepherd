package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
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
	// paneText is what a pane read shows; paneFn, when set, builds it from
	// the commands submitted so far instead.
	paneText string
	paneFn   func(sent []string) string
	// recentFn, when set, builds what a recent read shows.
	recentFn    func(sent []string) string
	paneReadErr error
	waitSets    [][]string
	commands    []string
	notices     []string
	closed      []string
	env         map[string]string
	spaces      []workspaceInfo
	tabs        []string
	renamed     map[string]string
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

func (f *scriptedHerdr) paneRead(paneID, source string, lines int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if source == "recent" && f.recentFn != nil {
		return f.recentFn(f.commands), nil
	}
	if f.paneReadErr != nil {
		return "", f.paneReadErr
	}
	if f.paneFn != nil {
		return f.paneFn(f.commands), nil
	}
	return f.paneText, nil
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

func autoCloseAction() *Action {
	a := watchedAction()
	a.AutoClose = true
	return a
}

func TestRunAgentProbeAgentNotFoundIsNotCompleted(t *testing.T) {
	// The agent vanished during the done-probe after a confirmed idle: that
	// is an exit to the shell, not a finished session.
	fake := &scriptedHerdr{waits: []waitStep{
		{state: "working"}, {state: "idle"},
		{err: timeoutErr()},       // re-arm
		{err: agentNotFoundErr()}, // done-probe: the agent is gone
		{err: agentNotFoundErr()}, // shared wait
	}}
	d := agentTestDaemon(t, fake)
	status, detail, _ := d.runAgent(autoCloseAction(), "")
	if status != "attention" || !strings.HasPrefix(detail, "agent exited") {
		t.Fatalf("got status=%q detail=%q", status, detail)
	}
}

func TestRunAgentProbePaneNotFoundIsCancelled(t *testing.T) {
	fake := &scriptedHerdr{
		waits: []waitStep{
			{state: "working"}, {state: "idle"},
			{err: timeoutErr()},
			{err: paneNotFoundErr()},
			{err: agentNotFoundErr()},
		},
		paneExistsFn: func(string) bool { return false },
	}
	d := agentTestDaemon(t, fake)
	status, detail, _ := d.runAgent(autoCloseAction(), "")
	if status != "cancelled" {
		t.Fatalf("got status=%q detail=%q", status, detail)
	}
	if _, _, _, closed := fake.counts(); closed != 0 {
		t.Fatalf("a cancelled run must not close anything: %v", fake.closed)
	}
}

func TestRunAgentProbeTransportErrorKeepsWatching(t *testing.T) {
	// One failed probe is no observation at all; a healthy session that goes
	// back to work and finishes still completes.
	fake := &scriptedHerdr{waits: []waitStep{
		{state: "working"}, {state: "idle"},
		{err: timeoutErr()},
		{err: errors.New("socket closed")},
		{state: "working"},
		{state: "done"},
	}}
	d := agentTestDaemon(t, fake)
	status, detail, _ := d.runAgent(autoCloseAction(), "")
	if status != "completed" {
		t.Fatalf("got status=%q detail=%q", status, detail)
	}
	if left, _, _, _ := fake.counts(); left != 0 {
		t.Fatalf("the watch ended %d steps early", left)
	}
}

func TestRunAgentProbeTransportErrorThenFailureIsError(t *testing.T) {
	fake := &scriptedHerdr{waits: []waitStep{
		{state: "working"}, {state: "idle"},
		{err: timeoutErr()},
		{err: errors.New("socket closed")},
		{err: errors.New("socket closed")},
	}}
	d := agentTestDaemon(t, fake)
	status, detail, _ := d.runAgent(autoCloseAction(), "")
	if status != "error" {
		t.Fatalf("got status=%q detail=%q", status, detail)
	}
	if _, _, _, closed := fake.counts(); closed != 0 {
		t.Fatalf("an errored run must not close its tab: %v", fake.closed)
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

func agentNotFoundSteps(n int) []waitStep {
	steps := make([]waitStep, n)
	for i := range steps {
		steps[i] = waitStep{err: agentNotFoundErr()}
	}
	return steps
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestRunAgentDoesNotResendIntoADetectedAgent(t *testing.T) {
	// Every wait times out: herdr found an agent, it just never reported
	// working. Typing the start command again would land in that agent.
	fake := &scriptedHerdr{}
	d := agentTestDaemon(t, fake)
	d.resend, d.startTimeout = 0, 30*time.Millisecond
	status, detail, _ := d.runAgent(watchedAction(), "")
	if status != "attention" || !strings.Contains(detail, "startup uncertain") || !strings.Contains(detail, "command not resent") {
		t.Fatalf("got status=%q detail=%q", status, detail)
	}
	if _, commands, notices, _ := fake.counts(); commands != 1 || notices != 1 {
		t.Fatalf("want one submit and one notice, got %d commands, %v", commands, fake.notices)
	}
}

func TestRunAgentDoesNotResendAfterTransportErrors(t *testing.T) {
	// A socket that failed for a while says nothing about whether the shell
	// took the line.
	fake := &scriptedHerdr{waits: []waitStep{
		{err: errors.New("socket closed")},
		{err: errors.New("socket closed")},
	}}
	fake.waits = append(fake.waits, agentNotFoundSteps(40)...)
	d := agentTestDaemon(t, fake)
	d.resend, d.startTimeout = 0, 30*time.Millisecond
	if status, _, _ := d.runAgent(watchedAction(), ""); status != "attention" {
		t.Fatalf("got status=%q", status)
	}
	if _, commands, _, _ := fake.counts(); commands != 1 {
		t.Fatalf("want the initial submit only, got %d commands", commands)
	}
}

func TestRunAgentDoesNotResendWhenThePaneHasTheLine(t *testing.T) {
	// The shell shows the line at its prompt but has not run it yet: a
	// second copy would run straight after the first.
	fake := &scriptedHerdr{waits: agentNotFoundSteps(40), paneFn: func(sent []string) string {
		return "$ " + sent[0]
	}}
	d := agentTestDaemon(t, fake)
	d.resend, d.startTimeout = 0, 30*time.Millisecond
	_, detail, _ := d.runAgent(watchedAction(), "")
	if _, commands, _, _ := fake.counts(); commands != 1 {
		t.Fatalf("want the initial submit only, got %d commands", commands)
	}
	if !strings.Contains(detail, "has the command but has not run it") {
		t.Fatalf("detail should say what the pane showed: %q", detail)
	}
}

func TestRunAgentDoesNotResendWhenTheShellRanTheLine(t *testing.T) {
	// The marker's output is on screen: the shell ran the line and the agent
	// is still loading, so it must not be typed into.
	fake := &scriptedHerdr{waits: agentNotFoundSteps(40), paneFn: func(sent []string) string {
		id := strings.TrimSuffix(strings.Fields(sent[0])[2], ";")
		return "$ " + sent[0] + "\nshepherd-run-A-" + id + "\n"
	}}
	d := agentTestDaemon(t, fake)
	d.resend, d.startTimeout = 0, 30*time.Millisecond
	_, detail, _ := d.runAgent(watchedAction(), "")
	if _, commands, _, _ := fake.counts(); commands != 1 {
		t.Fatalf("want the initial submit only, got %d commands", commands)
	}
	if !strings.Contains(detail, "the shell ran the command") {
		t.Fatalf("detail should say what the pane showed: %q", detail)
	}
}

func TestRunAgentDoesNotResendWhenTheMarkerScrolledOffScreen(t *testing.T) {
	// The screen is all agent output; only the recent read still holds the
	// line the shell ran.
	fake := &scriptedHerdr{waits: agentNotFoundSteps(40), paneText: "agent output\n> ", recentFn: func(sent []string) string {
		id := strings.TrimSuffix(strings.Fields(sent[0])[2], ";")
		return "$ " + sent[0] + "\nshepherd-run-A-" + id + "\nagent output\n> "
	}}
	d := agentTestDaemon(t, fake)
	d.resend, d.startTimeout = 0, 30*time.Millisecond
	d.runAgent(watchedAction(), "")
	if _, commands, _, _ := fake.counts(); commands != 1 {
		t.Fatalf("want the initial submit only, got %d commands", commands)
	}
}

func TestRunAgentDoesNotResendWhenThePaneCannotBeRead(t *testing.T) {
	fake := &scriptedHerdr{waits: agentNotFoundSteps(40), paneReadErr: errors.New("socket closed")}
	d := agentTestDaemon(t, fake)
	d.resend, d.startTimeout = 0, 30*time.Millisecond
	_, detail, _ := d.runAgent(watchedAction(), "")
	if _, commands, _, _ := fake.counts(); commands != 1 {
		t.Fatalf("want the initial submit only, got %d commands", commands)
	}
	if !strings.Contains(detail, "could not be read") {
		t.Fatalf("detail should say the pane could not be read: %q", detail)
	}
}

func TestRunAgentDoesNotResendIntoABlankPane(t *testing.T) {
	// A pane with nothing on it yet may be a shell still starting with the
	// line in its input buffer; only a drawn prompt without the line is proof.
	fake := &scriptedHerdr{waits: agentNotFoundSteps(40), paneText: "  \n"}
	d := agentTestDaemon(t, fake)
	d.resend, d.startTimeout = 0, 30*time.Millisecond
	d.runAgent(watchedAction(), "")
	if _, commands, _, _ := fake.counts(); commands != 1 {
		t.Fatalf("want the initial submit only, got %d commands", commands)
	}
}

func TestRunAgentResendsOnlyIntoAnUntouchedShell(t *testing.T) {
	// A shell that came up after the line was sent shows its prompt and no
	// trace of the line: the one case where sending it again is safe.
	logs := captureLog(t)
	fake := &scriptedHerdr{waits: agentNotFoundSteps(40), paneText: "$ "}
	d := agentTestDaemon(t, fake)
	d.resend, d.startTimeout = 0, 30*time.Millisecond
	if status, _, _ := d.runAgent(watchedAction(), ""); status != "attention" {
		t.Fatalf("got status=%q", status)
	}
	if len(fake.commands) != 2 || fake.commands[0] != fake.commands[1] {
		t.Fatalf("want the submit plus one identical resend, got %q", fake.commands)
	}
	if !strings.HasPrefix(fake.commands[0], `printf 'shepherd-run-\101-%s\n' `) {
		t.Fatalf("the command should carry the run marker: %q", fake.commands[0])
	}
	if n := strings.Count(logs.String(), `resent command; pane showed: "$ "`); n != 1 {
		t.Fatalf("want one resend log line naming the pane text, got %d in %q", n, logs.String())
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
