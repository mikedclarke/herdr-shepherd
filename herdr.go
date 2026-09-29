package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// herdrClient talks to the running herdr server over its unix socket. The
// protocol is newline-delimited JSON: one request object per line, one response
// per line, over a short-lived connection per call.
type herdrClient struct {
	socketPath string
}

func newHerdrClient() (*herdrClient, error) {
	path := os.Getenv("HERDR_SOCKET_PATH")
	if path == "" {
		return nil, errors.New("HERDR_SOCKET_PATH is not set; are you running inside herdr?")
	}
	return &herdrClient{socketPath: path}, nil
}

type herdrError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *herdrError) Error() string { return fmt.Sprintf("herdr: %s: %s", e.Code, e.Message) }

func hasCode(err error, code string) bool {
	var he *herdrError
	return errors.As(err, &he) && he.Code == code
}

func isTimeout(err error) bool       { return hasCode(err, "timeout") }
func isAgentNotFound(err error) bool { return hasCode(err, "agent_not_found") }
func isPaneNotFound(err error) bool  { return hasCode(err, "pane_not_found") }

// callDeadline is the ceiling for calls that respond immediately. Calls that
// legitimately block server-side (agent.wait) extend it by their own timeout.
const callDeadline = 30 * time.Second

func (c *herdrClient) call(method string, params map[string]any, out any) error {
	return c.callWithin(callDeadline, method, params, out)
}

func (c *herdrClient) callWithin(deadline time.Duration, method string, params map[string]any, out any) error {
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("connect herdr socket: %w", err)
	}
	defer conn.Close()
	// A wedged server would otherwise pin the caller (and its action's run
	// lock) forever.
	if err := conn.SetDeadline(time.Now().Add(deadline)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}

	req := map[string]any{"id": "herdr-shepherd", "method": method, "params": params}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("write request: %w", err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *herdrError     `json:"error"`
	}
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out != nil {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
	}
	return nil
}

// workspaceInfo is the part of a workspace listing the launch needs.
type workspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
}

func (c *herdrClient) workspaceList() ([]workspaceInfo, error) {
	var out struct {
		Workspaces []workspaceInfo `json:"workspaces"`
	}
	if err := c.call("workspace.list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out.Workspaces, nil
}

// createdTab is the tab and root pane that workspace.create and tab.create
// both return.
type createdTab struct {
	Workspace struct {
		WorkspaceID string `json:"workspace_id"`
	} `json:"workspace"`
	Tab struct {
		TabID string `json:"tab_id"`
	} `json:"tab"`
	RootPane struct {
		PaneID string `json:"pane_id"`
	} `json:"root_pane"`
}

// workspaceCreate opens an unfocused workspace at cwd and returns its id plus
// its root tab's and root pane's ids. env reaches the root pane only.
func (c *herdrClient) workspaceCreate(cwd, label string, env map[string]string) (workspaceID, tabID, paneID string, err error) {
	params := map[string]any{"cwd": cwd, "label": label, "focus": false}
	if len(env) > 0 {
		params["env"] = env
	}
	var out createdTab
	if err := c.call("workspace.create", params, &out); err != nil {
		return "", "", "", err
	}
	return out.Workspace.WorkspaceID, out.Tab.TabID, out.RootPane.PaneID, nil
}

// tabCreate opens an unfocused tab in a workspace and returns its id plus its
// root pane's id. env reaches that tab's pane only.
func (c *herdrClient) tabCreate(workspaceID, cwd, label string, env map[string]string) (tabID, paneID string, err error) {
	params := map[string]any{"workspace_id": workspaceID, "cwd": cwd, "label": label, "focus": false}
	if len(env) > 0 {
		params["env"] = env
	}
	var out createdTab
	if err := c.call("tab.create", params, &out); err != nil {
		return "", "", err
	}
	return out.Tab.TabID, out.RootPane.PaneID, nil
}

func (c *herdrClient) tabRename(tabID, label string) error {
	return c.call("tab.rename", map[string]any{"tab_id": tabID, "label": label}, nil)
}

// tabClose closes one tab; herdr closes a workspace together with its last tab.
func (c *herdrClient) tabClose(tabID string) error {
	return c.call("tab.close", map[string]any{"tab_id": tabID}, nil)
}

// runCommand submits a shell command in a pane: the command as pasted text plus
// a real Enter key press (an embedded newline would sit unexecuted at the
// prompt).
func (c *herdrClient) runCommand(paneID, command string) error {
	return c.call("pane.send_input", map[string]any{
		"pane_id": paneID,
		"text":    command,
		"keys":    []string{"Enter"},
	}, nil)
}

// agentWait blocks until the agent in target reaches one of the given states.
// It errors with code "timeout" on expiry and "agent_not_found" whenever the
// pane currently has no detected agent — including before one starts and after
// it exits.
func (c *herdrClient) agentWait(target string, until []string, timeoutMS int) (string, error) {
	var out struct {
		Agent struct {
			AgentStatus string `json:"agent_status"`
		} `json:"agent"`
	}
	err := c.callWithin(time.Duration(timeoutMS)*time.Millisecond+callDeadline, "agent.wait", map[string]any{
		"target":     target,
		"until":      until,
		"timeout_ms": timeoutMS,
	}, &out)
	if err != nil {
		return "", err
	}
	return out.Agent.AgentStatus, nil
}

// paneExists distinguishes "the agent exited" from "the pane/workspace is
// gone" after an agent_not_found. A transport error is neither, and is
// reported so the caller can keep watching instead of declaring a live
// session cancelled.
func (c *herdrClient) paneExists(paneID string) (bool, error) {
	err := c.call("pane.get", map[string]any{"pane_id": paneID}, nil)
	switch {
	case err == nil:
		return true, nil
	case isPaneNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

func (c *herdrClient) notify(title, body, sound string) error {
	params := map[string]any{"title": title}
	if body != "" {
		params["body"] = body
	}
	if sound != "" {
		params["sound"] = sound
	}
	return c.call("notification.show", params, nil)
}
