package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeHerdr serves the newline-delimited JSON protocol: one request per
// connection, answered from responses by method name.
func fakeHerdr(t *testing.T, responses map[string]string) *herdrClient {
	t.Helper()
	// Not t.TempDir(): long test names push the path past macOS's 104-byte
	// unix socket limit.
	dir, err := os.MkdirTemp("", "hs")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.RemoveAll(dir)
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var req struct {
					Method string `json:"method"`
				}
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				if json.Unmarshal(line, &req) != nil {
					return
				}
				resp, ok := responses[req.Method]
				if !ok {
					resp = `{"id":"x","error":{"code":"unknown_method","message":"?"}}`
				}
				if resp == "HANG" {
					time.Sleep(10 * time.Second)
					return
				}
				c.Write([]byte(resp + "\n"))
			}(conn)
		}
	}()
	return &herdrClient{socketPath: sock}
}

func TestClientErrorCodes(t *testing.T) {
	c := fakeHerdr(t, map[string]string{
		"agent.wait": `{"id":"x","error":{"code":"timeout","message":"timed out"}}`,
		"agent.get":  `{"id":"x","error":{"code":"agent_not_found","message":"none"}}`,
		"pane.get":   `{"id":"x","error":{"code":"pane_not_found","message":"gone"}}`,
	})
	if _, err := c.agentWait("p", []string{"done"}, 1000); !isTimeout(err) {
		t.Errorf("expected timeout, got %v", err)
	}
	err := c.call("agent.get", map[string]any{"target": "p"}, nil)
	if !isAgentNotFound(err) {
		t.Errorf("expected agent_not_found, got %v", err)
	}
	if c.paneExists("p") {
		t.Error("pane_not_found should report not existing")
	}
}

func TestClientWorkspaceCreateParsesIDs(t *testing.T) {
	c := fakeHerdr(t, map[string]string{
		"workspace.create": `{"id":"x","result":{"workspace":{"workspace_id":"w9"},"root_pane":{"pane_id":"w9:p1"}}}`,
	})
	ws, pane, err := c.workspaceCreate("/tmp", "Shepherd · test", map[string]string{"SHEPHERD_ACTION": "test"})
	if err != nil || ws != "w9" || pane != "w9:p1" {
		t.Errorf("got %q %q %v", ws, pane, err)
	}
}

func TestClientAgentWaitReturnsStatus(t *testing.T) {
	c := fakeHerdr(t, map[string]string{
		"agent.wait": `{"id":"x","result":{"type":"agent_info","agent":{"agent_status":"done"}}}`,
	})
	state, err := c.agentWait("p", []string{"done"}, 1000)
	if err != nil || state != "done" {
		t.Errorf("got %q %v", state, err)
	}
}

// A server that accepts and never replies must not pin the caller forever —
// that would leave the action's running flag set for the daemon's lifetime.
func TestClientDeadlineOnSilentServer(t *testing.T) {
	c := fakeHerdr(t, map[string]string{"pane.get": "HANG"})
	done := make(chan error, 1)
	go func() { done <- c.callWithin(500*time.Millisecond, "pane.get", map[string]any{"pane_id": "p"}, nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected deadline error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not respect its deadline")
	}
}
