package main

import (
	"errors"
	"testing"
	"time"
)

var launchAt = time.Date(2026, 9, 30, 6, 10, 0, 0, time.Local)

func TestLaunchAgentTabSubmitsTheCommand(t *testing.T) {
	fake := &scriptedHerdr{}
	a := watchedAction()
	tabID, paneID, err := launchAgentTab(fake, a, 0, triggerSchedule, launchAt)
	if err != nil || tabID != "ws1:t1" || paneID != "p1" {
		t.Fatalf("got %q %q %v", tabID, paneID, err)
	}
	want, err := a.AgentCommand()
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.commands) != 1 || fake.commands[0] != want {
		t.Errorf("got %v, want one %q", fake.commands, want)
	}
}

func TestLaunchAgentTabClosesAfterAFailedSubmit(t *testing.T) {
	// An open tab nobody asked for is worse than none at all.
	fake := &scriptedHerdr{runErr: errors.New("pane busy")}
	_, _, err := launchAgentTab(fake, watchedAction(), 0, triggerSchedule, launchAt)
	if !errors.Is(err, errLaunchSubmit) {
		t.Fatalf("got %v", err)
	}
	if len(fake.closed) != 1 || fake.closed[0] != "ws1:t1" {
		t.Errorf("the tab should have been closed, closed=%v", fake.closed)
	}
}

func TestLaunchAgentTabReportsCreateFailure(t *testing.T) {
	fake := &scriptedHerdr{createErr: errors.New("socket down")}
	_, _, err := launchAgentTab(fake, watchedAction(), 0, triggerSchedule, launchAt)
	if !errors.Is(err, errLaunchCreate) {
		t.Fatalf("got %v", err)
	}
	if len(fake.closed) != 0 {
		t.Errorf("nothing was opened, nothing to close: %v", fake.closed)
	}
}

func TestLaunchAgentTabRejectsABadCommand(t *testing.T) {
	fake := &scriptedHerdr{}
	a := &Action{Name: "nightly-report", Kind: KindRoutine, Directory: "/tmp", CLI: "claude"}
	if _, _, err := launchAgentTab(fake, a, 0, triggerSchedule, launchAt); err == nil {
		t.Fatal("an action with no prompt must not open a tab")
	}
	if len(fake.commands) != 0 {
		t.Errorf("nothing should have been submitted: %v", fake.commands)
	}
}

func TestLaunchAgentTabPassesTheActionEnv(t *testing.T) {
	t.Setenv("HOME", "/home/shep")
	fake := &scriptedHerdr{}
	a := watchedAction()
	a.Env = map[string]string{"AGENT_CONFIG_DIR": "~/agent-config", "SHEPHERD_ACTION": "spoofed"}
	if _, _, err := launchAgentTab(fake, a, 0, triggerSchedule, launchAt); err != nil {
		t.Fatal(err)
	}
	if fake.env["AGENT_CONFIG_DIR"] != "/home/shep/agent-config" {
		t.Errorf("the pane gets the action env with ~ expanded, got %v", fake.env)
	}
	if fake.env["SHEPHERD_ACTION"] != a.Name || fake.env["SHEPHERD_TRIGGER"] != triggerSchedule {
		t.Errorf("the daemon's own variables win over the table, got %v", fake.env)
	}
	if _, leaked := a.Env["SHEPHERD_TRIGGER"]; leaked {
		t.Error("launching must not write into the action's table")
	}
}

func TestRunsShareOneScheduledWorkspaceATabEach(t *testing.T) {
	fake := &scriptedHerdr{}
	a := watchedAction()
	first, _, err := launchAgentTab(fake, a, 0, triggerSchedule, launchAt)
	if err != nil {
		t.Fatal(err)
	}
	// No scheduled workspace yet: it is created and its root tab is the run's,
	// renamed, so no empty shell tab is left behind.
	if len(fake.spaces) != 1 || fake.spaces[0].Label != scheduledWorkspace || first != "ws1:t1" {
		t.Fatalf("spaces=%v first=%q", fake.spaces, first)
	}
	if got := fake.renamed["ws1:t1"]; got != a.Name+" 2026-09-30 06:10" {
		t.Errorf("root tab label = %q", got)
	}
	second, _, err := launchAgentTab(fake, a, 0, triggerSchedule, launchAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.spaces) != 1 || second == first || fake.renamed[second] != a.Name+" 2026-09-30 07:10" {
		t.Errorf("the second run must be a new tab in the same workspace: spaces=%v second=%q renamed=%v", fake.spaces, second, fake.renamed)
	}
}

func TestAnExistingScheduledWorkspaceIsAdoptedByItsExactLabel(t *testing.T) {
	fake := &scriptedHerdr{spaces: []workspaceInfo{
		{WorkspaceID: "wA", Label: "scheduled-old", Number: 1},
		{WorkspaceID: "wC", Label: scheduledWorkspace, Number: 4},
		{WorkspaceID: "wB", Label: scheduledWorkspace, Number: 2},
	}}
	tabID, _, err := launchAgentTab(fake, watchedAction(), 0, triggerSchedule, launchAt)
	if err != nil {
		t.Fatal(err)
	}
	if tabID != "wB:t2" || len(fake.spaces) != 3 {
		t.Errorf("the lowest-numbered workspace labelled %q takes the tab, got %q spaces=%v", scheduledWorkspace, tabID, fake.spaces)
	}
}

func TestPickWorkspace(t *testing.T) {
	spaces := []workspaceInfo{{WorkspaceID: "w9", Label: "x", Number: 9}, {WorkspaceID: "w3", Label: "x", Number: 3}, {WorkspaceID: "w1", Label: "X", Number: 1}}
	if got := pickWorkspace(spaces, "x"); got != "w3" {
		t.Errorf("got %q", got)
	}
	if got := pickWorkspace(spaces, "y"); got != "" {
		t.Errorf("got %q", got)
	}
}
