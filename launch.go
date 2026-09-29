package main

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// scheduledWorkspace labels the one workspace every agent run opens a tab in,
// so scheduled sessions share one sidebar entry instead of one each.
const scheduledWorkspace = "scheduled"

// The two ways a launch fails: nothing was opened, or the tab was opened and
// then closed again. Only the first is safe to retry blindly.
var (
	errLaunchCreate = errors.New("open tab")
	errLaunchSubmit = errors.New("run command")
)

// launchMu serialises finding or creating the scheduled workspace, so two runs
// firing together in one process share it rather than each creating one.
var launchMu sync.Mutex

// runTabLabel names a run's tab by its action and start time.
func runTabLabel(name string, at time.Time) string {
	return name + " " + at.Format("2006-01-02 15:04")
}

// pickWorkspace returns the id of the workspace carrying exactly label, the
// lowest-numbered when several do, or "" when none does.
func pickWorkspace(spaces []workspaceInfo, label string) string {
	var found []workspaceInfo
	for _, w := range spaces {
		if w.Label == label && w.WorkspaceID != "" {
			found = append(found, w)
		}
	}
	if len(found) == 0 {
		return ""
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Number < found[j].Number })
	if len(found) > 1 {
		log.Printf("%d workspaces are labelled %q; using %s", len(found), label, found[0].WorkspaceID)
	}
	return found[0].WorkspaceID
}

// openRunTab opens a run's tab in the scheduled workspace, creating the
// workspace when none carries its label. A new workspace's root tab becomes
// the run's tab, so no empty shell tab is left behind.
func openRunTab(client herdrAPI, cwd, label string, env map[string]string) (tabID, paneID string, err error) {
	launchMu.Lock()
	defer launchMu.Unlock()
	spaces, err := client.workspaceList()
	if err != nil {
		return "", "", err
	}
	if wsID := pickWorkspace(spaces, scheduledWorkspace); wsID != "" {
		return client.tabCreate(wsID, cwd, label, env)
	}
	_, tabID, paneID, err = client.workspaceCreate(cwd, scheduledWorkspace, env)
	if err != nil {
		return "", "", err
	}
	if rerr := client.tabRename(tabID, label); rerr != nil {
		log.Printf("rename tab %s to %q: %v", tabID, label, rerr)
	}
	return tabID, paneID, nil
}

// launchAgentTab opens the run's tab and submits the agent command. settle
// covers the pane's shell coming up; a pane that gets input before its prompt
// exists silently drops it. trigger (schedule, wake, manual) reaches the pane
// as SHEPHERD_TRIGGER beside SHEPHERD_ACTION.
func launchAgentTab(client herdrAPI, a *Action, settle time.Duration, trigger string, at time.Time) (tabID, paneID string, err error) {
	command, err := a.AgentCommand()
	if err != nil {
		return "", "", err
	}
	env := a.EnvMap()
	env["SHEPHERD_ACTION"] = a.Name
	env["SHEPHERD_TRIGGER"] = trigger
	tabID, paneID, err = openRunTab(client, a.Dir(), runTabLabel(a.Name, at), env)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errLaunchCreate, err)
	}
	time.Sleep(settle)
	if err := client.runCommand(paneID, command); err != nil {
		if cerr := client.tabClose(tabID); cerr != nil {
			log.Printf("%s: close tab after failed submit: %v", a.Name, cerr)
		}
		return "", "", fmt.Errorf("%w: %w", errLaunchSubmit, err)
	}
	return tabID, paneID, nil
}
