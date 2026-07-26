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
	// A wedged server would otherwise pin the caller (and its action's
	// running flag) forever.
	conn.SetDeadline(time.Now().Add(deadline))

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

// workspaceCreate opens an unfocused workspace at cwd and returns its id plus
// the root pane's id.
func (c *herdrClient) workspaceCreate(cwd, label string, env map[string]string) (workspaceID, paneID string, err error) {
	var out struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	params := map[string]any{
		"cwd":   cwd,
		"label": label,
		"focus": false,
	}
	if len(env) > 0 {
		params["env"] = env
	}
	if err := c.call("workspace.create", params, &out); err != nil {
		return "", "", err
	}
	return out.Workspace.WorkspaceID, out.RootPane.PaneID, nil
}

func (c *herdrClient) workspaceClose(workspaceID string) error {
	return c.call("workspace.close", map[string]any{"workspace_id": workspaceID}, nil)
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
// gone" after an agent_not_found.
func (c *herdrClient) paneExists(paneID string) bool {
	err := c.call("pane.get", map[string]any{"pane_id": paneID}, nil)
	return err == nil
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
