package main

import (
	"errors"
	"fmt"
	"log"
	"time"
)

// The two ways a launch fails: nothing was opened, or the workspace was opened
// and then closed again. Only the first is safe to retry blindly.
var (
	errLaunchCreate = errors.New("workspace create")
	errLaunchSubmit = errors.New("run command")
)

// launchAgentWorkspace opens the run's workspace and submits the agent
// command. settle covers the pane's shell coming up; a pane that gets input
// before its prompt exists silently drops it. trigger (schedule, wake, manual)
// reaches the pane as SHEPHERD_TRIGGER beside SHEPHERD_ACTION.
func launchAgentWorkspace(client herdrAPI, a *Action, settle time.Duration, trigger string) (wsID, paneID string, err error) {
	command, err := a.AgentCommand()
	if err != nil {
		return "", "", err
	}
	wsID, paneID, err = client.workspaceCreate(a.Dir(), "Shepherd · "+a.Name, map[string]string{
		"SHEPHERD_ACTION":  a.Name,
		"SHEPHERD_TRIGGER": trigger,
	})
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errLaunchCreate, err)
	}
	time.Sleep(settle)
	if err := client.runCommand(paneID, command); err != nil {
		if cerr := client.workspaceClose(wsID); cerr != nil {
			log.Printf("%s: close workspace after failed submit: %v", a.Name, cerr)
		}
		return "", "", fmt.Errorf("%w: %w", errLaunchSubmit, err)
	}
	return wsID, paneID, nil
}
