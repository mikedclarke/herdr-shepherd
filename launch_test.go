package main

import (
	"errors"
	"testing"
)

func TestLaunchAgentWorkspaceSubmitsTheCommand(t *testing.T) {
	fake := &scriptedHerdr{}
	a := watchedAction()
	wsID, paneID, err := launchAgentWorkspace(fake, a, 0, triggerSchedule)
	if err != nil || wsID != "ws1" || paneID != "p1" {
		t.Fatalf("got %q %q %v", wsID, paneID, err)
	}
	want, err := a.AgentCommand()
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.commands) != 1 || fake.commands[0] != want {
		t.Errorf("got %v, want one %q", fake.commands, want)
	}
}

func TestLaunchAgentWorkspaceClosesAfterAFailedSubmit(t *testing.T) {
	// An open workspace nobody asked for is worse than none at all.
	fake := &scriptedHerdr{runErr: errors.New("pane busy")}
	_, _, err := launchAgentWorkspace(fake, watchedAction(), 0, triggerSchedule)
	if !errors.Is(err, errLaunchSubmit) {
		t.Fatalf("got %v", err)
	}
	if len(fake.closed) != 1 || fake.closed[0] != "ws1" {
		t.Errorf("the workspace should have been closed, closed=%v", fake.closed)
	}
}

func TestLaunchAgentWorkspaceReportsCreateFailure(t *testing.T) {
	fake := &scriptedHerdr{createErr: errors.New("socket down")}
	_, _, err := launchAgentWorkspace(fake, watchedAction(), 0, triggerSchedule)
	if !errors.Is(err, errLaunchCreate) {
		t.Fatalf("got %v", err)
	}
	if len(fake.closed) != 0 {
		t.Errorf("nothing was opened, nothing to close: %v", fake.closed)
	}
}

func TestLaunchAgentWorkspaceRejectsABadCommand(t *testing.T) {
	fake := &scriptedHerdr{}
	a := &Action{Name: "nightly-report", Kind: KindRoutine, Directory: "/tmp", CLI: "claude"}
	if _, _, err := launchAgentWorkspace(fake, a, 0, triggerSchedule); err == nil {
		t.Fatal("an action with no prompt must not open a workspace")
	}
	if len(fake.commands) != 0 {
		t.Errorf("nothing should have been submitted: %v", fake.commands)
	}
}
