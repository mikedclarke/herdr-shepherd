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
	if alive, err := c.paneExists("p"); alive || err != nil {
		t.Errorf("pane_not_found should report a missing pane, got %v %v", alive, err)
	}
}

func TestClientPaneExistsSeparatesMissingFromUnreachable(t *testing.T) {
	// Only a confirmed missing pane may end a run; anything else is a
	// transport problem the caller should keep watching through.
	c := fakeHerdr(t, map[string]string{
		"pane.get": `{"id":"x","result":{}}`,
	})
	if alive, err := c.paneExists("p"); !alive || err != nil {
		t.Errorf("got %v %v", alive, err)
	}

	broken := fakeHerdr(t, map[string]string{
		"pane.get": `{"id":"x","error":{"code":"internal","message":"boom"}}`,
	})
	if alive, err := broken.paneExists("p"); alive || err == nil {
		t.Errorf("an unreachable pane check must report its error, got %v %v", alive, err)
	}
}

// The replies below are trimmed from a live herdr 0.9.1 server.
func TestClientWorkspaceCreateParsesIDs(t *testing.T) {
	c := fakeHerdr(t, map[string]string{
		"workspace.create": `{"id":"x","result":{"type":"workspace_created","workspace":{"workspace_id":"w9","label":"scheduled","number":4},` +
			`"tab":{"tab_id":"w9:t1","workspace_id":"w9","label":"1","number":1},"root_pane":{"pane_id":"w9:p1","tab_id":"w9:t1","workspace_id":"w9"}}}`,
	})
	ws, tab, pane, err := c.workspaceCreate("/tmp", "scheduled", map[string]string{"SHEPHERD_ACTION": "test"})
	if err != nil || ws != "w9" || tab != "w9:t1" || pane != "w9:p1" {
		t.Errorf("got %q %q %q %v", ws, tab, pane, err)
	}
}

func TestClientTabCallsParseIDs(t *testing.T) {
	c := fakeHerdr(t, map[string]string{
		"workspace.list": `{"id":"x","result":{"type":"workspace_list","workspaces":[{"workspace_id":"wK8","number":2,"label":"main","tab_count":3},` +
			`{"workspace_id":"wKG","number":5,"label":"scheduled","tab_count":1}]}}`,
		"tab.create": `{"id":"x","result":{"type":"tab_created","tab":{"tab_id":"wKG:t2","workspace_id":"wKG","number":2,"label":"probe-tab"},` +
			`"root_pane":{"pane_id":"wKG:p2","workspace_id":"wKG","tab_id":"wKG:t2"}}}`,
		"tab.rename": `{"id":"x","result":{"type":"tab_info","tab":{"tab_id":"wKG:t2","label":"renamed"}}}`,
		"tab.close":  `{"id":"x","result":{"type":"ok"}}`,
	})
	spaces, err := c.workspaceList()
	if err != nil || len(spaces) != 2 || spaces[1] != (workspaceInfo{WorkspaceID: "wKG", Label: "scheduled", Number: 5}) {
		t.Fatalf("got %v %v", spaces, err)
	}
	tab, pane, err := c.tabCreate("wKG", "/tmp", "probe-tab", map[string]string{"SHEPHERD_ACTION": "test"})
	if err != nil || tab != "wKG:t2" || pane != "wKG:p2" {
		t.Errorf("got %q %q %v", tab, pane, err)
	}
	if err := c.tabRename("wKG:t2", "renamed"); err != nil {
		t.Error(err)
	}
	if err := c.tabClose("wKG:t2"); err != nil {
		t.Error(err)
	}
	gone := fakeHerdr(t, map[string]string{"tab.close": `{"id":"x","error":{"code":"tab_not_found","message":"tab wKG:t2 not found"}}`})
	if err := gone.tabClose("wKG:t2"); !hasCode(err, "tab_not_found") {
		t.Errorf("got %v", err)
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
