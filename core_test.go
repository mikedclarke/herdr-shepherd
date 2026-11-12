package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedRun stands in for hc and herdr-app: it records every argv and
// answers from reply, which sees the calls made so far. The zero reply
// answers every call with empty output and exit 0.
type scriptedRun struct {
	mu    sync.Mutex
	calls [][]string
	reply func(argv []string, n int) (string, int)
}

func (r *scriptedRun) run(_ context.Context, name string, args ...string) ([]byte, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	argv := append([]string{name}, args...)
	r.calls = append(r.calls, argv)
	if r.reply == nil {
		return nil, 0, nil
	}
	out, code := r.reply(argv, len(r.calls))
	return []byte(out), code, nil
}

func (r *scriptedRun) argvs() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.calls...)
}

func testCore(t *testing.T, r *scriptedRun) *coreClient {
	t.Helper()
	n := 0
	return &coreClient{
		hc: "hc", app: "herdr-app", stateDir: t.TempDir(), run: r.run,
		ids: func(size int) string {
			n++
			return strings.Repeat(string(rune('a'+n-1)), size*2)
		},
		poll: time.Millisecond, tabWait: 50 * time.Millisecond, now: time.Now,
	}
}

func wantCalls(t *testing.T, r *scriptedRun, want ...[]string) {
	t.Helper()
	got := r.argvs()
	if !reflect.DeepEqual(got, want) {
		var g, w strings.Builder
		for _, c := range got {
			g.WriteString("\n  " + strings.Join(c, " "))
		}
		for _, c := range want {
			w.WriteString("\n  " + strings.Join(c, " "))
		}
		t.Fatalf("calls:%s\nwant:%s", g.String(), w.String())
	}
}

func argv(s ...string) []string { return s }

func is(a []string, prefix ...string) bool {
	return len(a) >= len(prefix) && reflect.DeepEqual(a[:len(prefix)], prefix)
}

// The replies below are trimmed from a live herdr-core and Herdr app.
const (
	recordedLs = `{"spaces":[{"id":"spdf60318b1c8a6383d9d7aba7169623ae","name":"workspace","folder":"/home/u","machine":null,"active":true,` +
		`"tabs":[{"id":"tba020e0475ac4e1f568e76251588c45d6","name":"1","active":false,"settled":false,"terminals":[{"terminal":"t1",` +
		`"pane":"pe0fbd3fd985383c7e9494beb0ed7dee0","status":"working","app":"claude-code","name":null,"sub_agents":[]}]},` +
		`{"id":"tb4c15d29608db6ba920009b7dbcf5f964","name":"2","active":true,"settled":false,"terminals":[]}]},` +
		`{"id":"sp7c43fb08553e547f61014f5508ebcab0","name":"scheduled","folder":"/home/u","machine":null,"active":false,` +
		`"tabs":[{"id":"tb901b812489e0010025e067dbcc4746f6","name":"1","active":true,"settled":false,"terminals":[{"terminal":"t1f",` +
		`"pane":"p1","status":null,"app":null,"name":null,"sub_agents":[]}]}]}]}`
	recordedRun     = `{"address":"t1h","agent":"zsh","cwd":"/home/u","name":"sh-1a2b3c4d","state":"live","turn":0}`
	recordedNoSuch  = `{"error":{"hint":"see ` + "`hc ls --all`" + `","kind":"not_found","message":"no terminal \"nosuch\""}}`
	recordedNoTab   = `{"error":"no tab \"tbnosuch\""}`
	recordedNewSpc  = `{"space":"sp7c43fb08553e547f61014f5508ebcab0","tab":"tb901b812489e0010025e067dbcc4746f6","terminal":"t1f"}`
	recordedSend    = `{"started":null,"status":null,"turn":0}`
	recordedWorking = `{"address":"t1j","argv":["/bin/zsh","-l"],"cwd":"/home/u","exit_code":null,"foreground":{"argv":["claude"],` +
		`"cwd":"/home/u","pid":76330},"keep":true,"labels":{"herdr.space":"sp...","herdr.tab":"tb..."},"name":"sh-1a2b3c4d",` +
		`"state":"live","status":{"app":"claude-code","state":"working"},"turn":21}`
)

func hcGet(state, status string) string {
	st := "null"
	if status != "" {
		st = `{"app":"claude-code","state":"` + status + `"}`
	}
	return `{"address":"t1j","state":"` + state + `","status":` + st + `}`
}

// lsWith is a listing whose one space holds the given tab with one terminal.
func lsWith(tabID, terminal string) string {
	return `{"spaces":[{"id":"spX","name":"scheduled","active":false,"tabs":[{"id":"` + tabID +
		`","name":"1","active":false,"terminals":[{"terminal":"` + terminal + `","pane":"pY"}]}]}]}`
}

func TestCoreWorkspaceListNumbersSpacesByPosition(t *testing.T) {
	r := &scriptedRun{reply: func([]string, int) (string, int) { return recordedLs, 0 }}
	c := testCore(t, r)
	spaces, err := c.workspaceList()
	if err != nil {
		t.Fatal(err)
	}
	want := []workspaceInfo{
		{WorkspaceID: "spdf60318b1c8a6383d9d7aba7169623ae", Label: "workspace", Number: 1},
		{WorkspaceID: "sp7c43fb08553e547f61014f5508ebcab0", Label: "scheduled", Number: 2},
	}
	if !reflect.DeepEqual(spaces, want) {
		t.Errorf("got %+v", spaces)
	}
	wantCalls(t, r, argv("herdr-app", "ls"))
	if got := pickWorkspace(spaces, scheduledWorkspace); got != "sp7c43fb08553e547f61014f5508ebcab0" {
		t.Errorf("pickWorkspace on the core listing: %q", got)
	}
}

func TestCoreTabCreateArgv(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	tab := "tb" + strings.Repeat("a", 32)
	lsCalls := 0
	r := &scriptedRun{reply: func(a []string, _ int) (string, int) {
		switch {
		case is(a, "hc", "run"):
			return recordedRun, 0
		case is(a, "herdr-app", "ls"):
			lsCalls++
			if lsCalls < 2 {
				return `{"spaces":[]}`, 0
			}
			return lsWith(tab, "t1h"), 0
		}
		return "{}", 0
	}}
	c := testCore(t, r)
	gotTab, pane, err := c.tabCreate("spX", "/home/u", "nightly 2026-11-12 06:00", map[string]string{"SHEPHERD_TRIGGER": "schedule", "SHEPHERD_ACTION": "nightly"})
	if err != nil || gotTab != tab || pane != "t1h" {
		t.Fatalf("got %q %q %v", gotTab, pane, err)
	}
	wantCalls(t, r,
		argv("hc", "run", "--bg", "--keep", "--no-parent", "--json", "--name", "sh-bbbbbbbb", "--cwd", "/home/u",
			"--label", "herdr.space=spX", "--label", "herdr.tab="+tab, "--",
			"/usr/bin/env", "SHEPHERD_ACTION=nightly", "SHEPHERD_TRIGGER=schedule", "/bin/bash", "-l"),
		argv("herdr-app", "ls"),
		argv("herdr-app", "ls"),
		argv("herdr-app", "do", "tab.rename", `{"id":"`+tab+`","name":"nightly 2026-11-12 06:00"}`),
	)
}

func TestCoreTabCreateWithoutEnvHasNoPrefix(t *testing.T) {
	t.Setenv("SHELL", "")
	tab := "tb" + strings.Repeat("a", 32)
	r := &scriptedRun{reply: func(a []string, _ int) (string, int) {
		switch {
		case is(a, "hc", "run"):
			return recordedRun, 0
		case is(a, "herdr-app", "ls"):
			return lsWith(tab, "t1h"), 0
		}
		return "{}", 0
	}}
	c := testCore(t, r)
	if _, _, err := c.tabCreate("spX", "/home/u", "x", nil); err != nil {
		t.Fatal(err)
	}
	first := r.argvs()[0]
	if tail := first[len(first)-3:]; !reflect.DeepEqual(tail, []string{"--", "/bin/zsh", "-l"}) {
		t.Errorf("argv tail %v", tail)
	}
}

func TestCoreTabCreateRemovesTerminalWhenTabNeverAppears(t *testing.T) {
	r := &scriptedRun{reply: func(a []string, _ int) (string, int) {
		switch {
		case is(a, "hc", "run"):
			return recordedRun, 0
		case is(a, "herdr-app", "ls"):
			return `{"spaces":[]}`, 0
		}
		return "{}", 0
	}}
	c := testCore(t, r)
	if _, _, err := c.tabCreate("spX", "/home/u", "x", nil); err == nil {
		t.Fatal("expected an error")
	}
	calls := r.argvs()
	got := calls[len(calls)-2:]
	if want := [][]string{argv("hc", "kill", "t1h", "--json"), argv("hc", "rm", "t1h", "--json")}; !reflect.DeepEqual(got, want) {
		t.Errorf("cleanup %v", got)
	}
	for _, a := range calls {
		if is(a, "herdr-app", "do", "tab.rename") {
			t.Error("a tab that never appeared was renamed")
		}
	}
}

func TestCoreWorkspaceCreateRestoresScreenAndClosesFirstTab(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	newTab := "tb" + strings.Repeat("a", 32)
	lsCalls := 0
	r := &scriptedRun{reply: func(a []string, _ int) (string, int) {
		switch {
		case is(a, "herdr-app", "ls"):
			lsCalls++
			if lsCalls == 1 {
				return recordedLs, 0
			}
			return `{"spaces":[{"id":"sp7c43fb08553e547f61014f5508ebcab0","name":"scheduled","tabs":[` +
				`{"id":"tb901b812489e0010025e067dbcc4746f6","terminals":[{"terminal":"t1f"}]},` +
				`{"id":"` + newTab + `","terminals":[{"terminal":"t1h"}]}]}]}`, 0
		case is(a, "herdr-app", "do", "space.new"):
			return recordedNewSpc, 0
		case is(a, "hc", "run"):
			return recordedRun, 0
		}
		return "{}", 0
	}}
	c := testCore(t, r)
	ws, tab, pane, err := c.workspaceCreate("/home/u", "scheduled", map[string]string{"SHEPHERD_ACTION": "nightly"})
	if err != nil || ws != "sp7c43fb08553e547f61014f5508ebcab0" || tab != newTab || pane != "t1h" {
		t.Fatalf("got %q %q %q %v", ws, tab, pane, err)
	}
	wantCalls(t, r,
		argv("herdr-app", "ls"),
		argv("herdr-app", "do", "space.new", `{"name":"scheduled","folder":"/home/u","tab":"1"}`),
		argv("herdr-app", "do", "space.select", `{"id":"spdf60318b1c8a6383d9d7aba7169623ae"}`),
		argv("herdr-app", "do", "tab.select", `{"id":"tb4c15d29608db6ba920009b7dbcf5f964"}`),
		argv("hc", "run", "--bg", "--keep", "--no-parent", "--json", "--name", "sh-bbbbbbbb", "--cwd", "/home/u",
			"--label", "herdr.space=sp7c43fb08553e547f61014f5508ebcab0", "--label", "herdr.tab="+newTab, "--",
			"/usr/bin/env", "SHEPHERD_ACTION=nightly", "/bin/zsh", "-l"),
		argv("herdr-app", "ls"),
		argv("herdr-app", "do", "tab.rename", `{"id":"`+newTab+`","name":"scheduled"}`),
		argv("herdr-app", "ls"),
		argv("herdr-app", "do", "tab.close", `{"id":"tb901b812489e0010025e067dbcc4746f6","confirmed":true}`),
		argv("hc", "kill", "t1f", "--json"),
		argv("hc", "rm", "t1f", "--json"),
	)
}

// A failed screen restore must not fail the launch: the space is made.
func TestCoreWorkspaceCreateIgnoresSelectErrors(t *testing.T) {
	newTab := "tb" + strings.Repeat("a", 32)
	r := &scriptedRun{reply: func(a []string, _ int) (string, int) {
		switch {
		case is(a, "herdr-app", "ls"):
			return strings.Replace(recordedLs, `"terminals":[]`, `"terminals":[]},{"id":"`+newTab+`","terminals":[]`, 1), 0
		case is(a, "herdr-app", "do", "space.new"):
			return recordedNewSpc, 0
		case is(a, "herdr-app", "do", "space.select"), is(a, "herdr-app", "do", "tab.select"):
			return `{"error":"no space"}`, 1
		case is(a, "hc", "run"):
			return recordedRun, 0
		}
		return "{}", 0
	}}
	c := testCore(t, r)
	if _, tab, _, err := c.workspaceCreate("/home/u", "scheduled", nil); err != nil || tab != newTab {
		t.Fatalf("select errors failed the launch: %q %v", tab, err)
	}
}

func TestCoreTabCloseArgvAndMissingTab(t *testing.T) {
	r := &scriptedRun{reply: func(a []string, _ int) (string, int) {
		switch {
		case is(a, "herdr-app", "ls"):
			return recordedLs, 0
		case is(a, "herdr-app", "do", "tab.close"):
			if strings.Contains(a[3], "tbnosuch") {
				return recordedNoTab, 1
			}
			return "{}", 0
		}
		return `{"error":{"kind":"finished","message":"already finished"}}`, 11
	}}
	c := testCore(t, r)
	if err := c.tabClose("tba020e0475ac4e1f568e76251588c45d6"); err != nil {
		t.Fatalf("kill and rm errors must be ignored: %v", err)
	}
	wantCalls(t, r,
		argv("herdr-app", "ls"),
		argv("herdr-app", "do", "tab.close", `{"id":"tba020e0475ac4e1f568e76251588c45d6","confirmed":true}`),
		argv("hc", "kill", "t1", "--json"),
		argv("hc", "rm", "t1", "--json"),
	)
	err := c.tabClose("tbnosuch")
	var he *herdrError
	if err == nil || !strings.Contains(err.Error(), `no tab "tbnosuch"`) || !asHerdrError(err, &he) {
		t.Errorf("got %v", err)
	}
}

func asHerdrError(err error, he **herdrError) bool {
	e, ok := err.(*herdrError)
	*he = e
	return ok
}

func TestCoreRunCommandArgv(t *testing.T) {
	r := &scriptedRun{reply: func([]string, int) (string, int) { return recordedSend, 0 }}
	c := testCore(t, r)
	if err := c.runCommand("t1j", "-x; echo hi"); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, argv("hc", "send", "--json", "--", "t1j", "-x; echo hi"))
}

func TestCorePaneReadArgv(t *testing.T) {
	r := &scriptedRun{reply: func([]string, int) (string, int) { return "$ claude\n> ", 0 }}
	c := testCore(t, r)
	if text, err := c.paneRead("t1j", "visible", 100); err != nil || text != "$ claude\n> " {
		t.Fatalf("got %q %v", text, err)
	}
	if _, err := c.paneRead("t1j", "recent", 100); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r,
		argv("hc", "read", "t1j", "--lines", "0"),
		argv("hc", "read", "t1j", "--lines", "100"),
	)
}

func TestCoreTabRenameArgv(t *testing.T) {
	r := &scriptedRun{}
	c := testCore(t, r)
	if err := c.tabRename("tb1", `a "quoted" name`); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, argv("herdr-app", "do", "tab.rename", `{"id":"tb1","name":"a \"quoted\" name"}`))
}

// getSequence answers successive `hc get` calls in order, repeating the last.
func getSequence(replies ...[2]any) *scriptedRun {
	i := 0
	return &scriptedRun{reply: func(a []string, _ int) (string, int) {
		r := replies[min(i, len(replies)-1)]
		i++
		return r[0].(string), r[1].(int)
	}}
}

func ok(s string) [2]any { return [2]any{s, 0} }

func TestCoreAgentWaitReturnsAStateInUntil(t *testing.T) {
	r := getSequence(ok(hcGet("live", "idle")), ok(recordedWorking))
	c := testCore(t, r)
	state, err := c.agentWait("t1j", []string{"working", "blocked"}, 1000)
	if err != nil || state != "working" {
		t.Fatalf("got %q %v", state, err)
	}
	wantCalls(t, r, argv("hc", "get", "t1j", "--json"), argv("hc", "get", "t1j", "--json"))
}

func TestCoreAgentWaitErrorStateIsDone(t *testing.T) {
	c := testCore(t, getSequence(ok(hcGet("live", "error"))))
	state, err := c.agentWait("t1j", []string{"done", "idle", "blocked"}, 1000)
	if err != nil || state != "done" {
		t.Fatalf("got %q %v", state, err)
	}
}

func TestCoreAgentWaitNoAgentIsAgentNotFound(t *testing.T) {
	r := getSequence(ok(hcGet("live", "")))
	c := testCore(t, r)
	_, err := c.agentWait("t1j", []string{"working", "blocked"}, 1000)
	if !isAgentNotFound(err) {
		t.Errorf("a live shell with no agent reporting: got %v, want agent_not_found", err)
	}
	if n := len(r.argvs()); n != 1 {
		t.Errorf("agent_not_found must come at once, took %d calls", n)
	}
}

func TestCoreAgentWaitGoneTerminalIsPaneNotFound(t *testing.T) {
	cases := map[string][2]any{
		"finished terminal": ok(hcGet("finished", "done")),
		"missing terminal":  {recordedNoSuch, 3},
	}
	for name, reply := range cases {
		r := getSequence(reply)
		c := testCore(t, r)
		_, err := c.agentWait("t1j", []string{"working", "blocked"}, 1000)
		if !isPaneNotFound(err) {
			t.Errorf("%s: got %v, want pane_not_found", name, err)
		}
		if n := len(r.argvs()); n != 1 {
			t.Errorf("%s: pane_not_found must come at once, took %d calls", name, n)
		}
	}
}

func TestCoreAgentWaitTimesOut(t *testing.T) {
	c := testCore(t, getSequence(ok(hcGet("live", "idle"))))
	began := time.Now()
	_, err := c.agentWait("t1j", []string{"working", "blocked"}, 30)
	if !isTimeout(err) {
		t.Fatalf("got %v, want timeout", err)
	}
	if time.Since(began) > 2*time.Second {
		t.Errorf("timeout took %s", time.Since(began))
	}
}

func TestCoreAgentWaitPassesOtherFailuresThrough(t *testing.T) {
	c := testCore(t, getSequence([2]any{`{"error":{"kind":"unavailable","message":"core down"}}`, 1}))
	_, err := c.agentWait("t1j", []string{"done"}, 1000)
	if err == nil || isAgentNotFound(err) || isTimeout(err) {
		t.Fatalf("a core that does not answer is neither a missing agent nor a timeout: %v", err)
	}
}

func TestCorePaneExists(t *testing.T) {
	cases := []struct {
		name    string
		reply   [2]any
		alive   bool
		wantErr bool
	}{
		{"live", ok(recordedWorking), true, false},
		{"live bare shell", ok(hcGet("live", "")), true, false},
		{"finished", ok(hcGet("finished", "")), false, false},
		{"not found", [2]any{recordedNoSuch, 3}, false, false},
		{"core unreachable", [2]any{`{"error":{"kind":"unavailable","message":"x"}}`, 1}, false, true},
		{"garbage", [2]any{"core panicked", 101}, false, true},
	}
	for _, tc := range cases {
		r := getSequence(tc.reply)
		alive, err := testCore(t, r).paneExists("t1j")
		if alive != tc.alive || (err != nil) != tc.wantErr {
			t.Errorf("%s: got %v %v", tc.name, alive, err)
		}
		wantCalls(t, r, argv("hc", "get", "t1j", "--json"))
	}
}

func TestCoreErrorShapes(t *testing.T) {
	r := &scriptedRun{reply: func(a []string, _ int) (string, int) {
		if a[0] == "hc" {
			return recordedNoSuch, 3
		}
		return recordedNoTab, 1
	}}
	c := testCore(t, r)
	_, err := c.call("hc", "get", "nosuch", "--json")
	if !hasCode(err, "not_found") || !strings.Contains(err.Error(), `no terminal "nosuch"`) {
		t.Errorf("hc error: %v", err)
	}
	err = c.appDo(nil, "tab.close", tabCloseArgs{ID: "tbnosuch", Confirmed: true})
	if !hasCode(err, "app_error") || !strings.Contains(err.Error(), `no tab "tbnosuch"`) {
		t.Errorf("herdr-app error: %v", err)
	}
}

func TestCoreNotifyWritesAFileAndPrunesOldOnes(t *testing.T) {
	r := &scriptedRun{}
	c := testCore(t, r)
	dir := filepath.Join(c.stateDir, "notify")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	stale, fresh := filepath.Join(dir, "1.json"), filepath.Join(dir, "2.json")
	for path, age := range map[string]time.Duration{stale: 25 * time.Hour, fresh: time.Hour} {
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.notify("Shepherd: nightly needs attention", "The session is waiting on input", "request"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a day-old notification was kept: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a recent notification was pruned: %v", err)
	}
	path := filepath.Join(dir, strconv.FormatInt(now.UnixNano(), 10)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"title": "Shepherd: nightly needs attention", "body": "The session is waiting on input",
		"sound": "request", "at": "2026-11-12T09:00:00Z"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
	if n := len(r.argvs()); n != 0 {
		t.Errorf("notify ran %d programs", n)
	}
}

func TestCoreStartDaemon(t *testing.T) {
	t.Run("no terminal of that name", func(t *testing.T) {
		r := &scriptedRun{reply: func(a []string, _ int) (string, int) {
			if is(a, "hc", "get") {
				return recordedNoSuch, 3
			}
			return `{"address":"t2a","name":"shepherd","state":"live"}`, 0
		}}
		c := testCore(t, r)
		addr, err := c.startDaemon("/opt/bin/herdr-shepherd")
		if err != nil || addr != "t2a" {
			t.Fatalf("got %q %v", addr, err)
		}
		wantCalls(t, r,
			argv("hc", "get", "shepherd", "--json"),
			argv("hc", "run", "--bg", "--keep", "--no-parent", "--json", "--name", "shepherd", "--", "/usr/bin/env", "HERDR_SHEPHERD_BACKEND=core",
				"HERDR_SHEPHERD_LOG="+filepath.Join(c.stateDir, "shepherd.log"), "/opt/bin/herdr-shepherd", "daemon"),
		)
	})
	t.Run("a terminal left behind by a dead daemon", func(t *testing.T) {
		r := &scriptedRun{reply: func(a []string, _ int) (string, int) {
			switch {
			case is(a, "hc", "get"):
				return `{"address":"t1z","name":"shepherd","state":"live","status":null}`, 0
			case is(a, "hc", "run"):
				return `{"address":"t2a","name":"shepherd","state":"live"}`, 0
			}
			return "{}", 0
		}}
		c := testCore(t, r)
		if _, err := c.startDaemon("/opt/bin/herdr-shepherd"); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, r,
			argv("hc", "get", "shepherd", "--json"),
			argv("hc", "kill", "t1z", "--json"),
			argv("hc", "rm", "t1z", "--json"),
			argv("hc", "run", "--bg", "--keep", "--no-parent", "--json", "--name", "shepherd", "--", "/usr/bin/env", "HERDR_SHEPHERD_BACKEND=core",
				"HERDR_SHEPHERD_LOG="+filepath.Join(c.stateDir, "shepherd.log"), "/opt/bin/herdr-shepherd", "daemon"),
		)
	})
	t.Run("lock held", func(t *testing.T) {
		dir := t.TempDir()
		p := paths{ConfigDir: dir, StateDir: dir}
		release, err := acquireLock(p.LockFile())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		r := &scriptedRun{}
		msg, err := startCoreDaemon(testCore(t, r), p, "/opt/bin/herdr-shepherd")
		if err != nil || msg != "daemon already running" {
			t.Fatalf("got %q %v", msg, err)
		}
		if n := len(r.argvs()); n != 0 {
			t.Errorf("a running daemon must be left alone, made %d calls", n)
		}
		pid, _ := os.ReadFile(p.LockFile())
		if string(pid) != strconv.Itoa(os.Getpid()) {
			t.Errorf("the probe rewrote the daemon's lock file: %q", pid)
		}
	})
	t.Run("lock free", func(t *testing.T) {
		dir := t.TempDir()
		p := paths{ConfigDir: dir, StateDir: dir}
		r := &scriptedRun{reply: func(a []string, _ int) (string, int) {
			if is(a, "hc", "get") {
				return recordedNoSuch, 3
			}
			return `{"address":"t2a","name":"shepherd","state":"live"}`, 0
		}}
		msg, err := startCoreDaemon(testCore(t, r), p, "/opt/bin/herdr-shepherd")
		if err != nil || !strings.Contains(msg, "t2a") {
			t.Fatalf("got %q %v", msg, err)
		}
		if release, err := acquireLock(p.LockFile()); err != nil {
			t.Errorf("the probe left the lock held: %v", err)
		} else {
			release()
		}
	})
}

// A core-backed run goes through the daemon's agent watch unchanged: the
// shell's null status reads as no agent yet, then working, then done.
func TestCoreRunAgentEndToEnd(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	tab := "tb" + strings.Repeat("a", 32)
	gets := []string{hcGet("live", ""), hcGet("live", "idle"), hcGet("live", "working"), hcGet("live", "done")}
	r := &scriptedRun{reply: func(a []string, _ int) (string, int) {
		switch {
		case is(a, "herdr-app", "ls"):
			return strings.Replace(lsWith(tab, "t1h"), `"id":"spX"`, `"id":"spS"`, 1), 0
		case is(a, "hc", "run"):
			return recordedRun, 0
		case is(a, "hc", "get"):
			g := gets[0]
			if len(gets) > 1 {
				gets = gets[1:]
			}
			return g, 0
		case is(a, "hc", "send"):
			return recordedSend, 0
		}
		return "{}", 0
	}}
	c := testCore(t, r)
	d := testDaemon(t)
	d.client = c
	status, detail, startFailed := d.runAgent(watchedAction(), "")
	if status != "completed" || startFailed {
		t.Fatalf("got %q %q %v", status, detail, startFailed)
	}
	if !strings.Contains(detail, "tab "+tab) {
		t.Errorf("detail %q", detail)
	}
	sent := 0
	for _, a := range r.argvs() {
		if is(a, "hc", "send", "--json", "--", "t1h") {
			sent++
		}
	}
	if sent != 1 {
		t.Errorf("command sent %d times", sent)
	}
}

func TestRunProgramDropsCallerIdentityAndReadsStderrOnFailure(t *testing.T) {
	t.Setenv("HC_TERMINAL_ID", "t9")
	t.Setenv("HC_SCOPE", "s")
	t.Setenv("SHEPHERD_PROBE", "kept")
	out, code, err := runProgram(context.Background(), "/usr/bin/env")
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	env := string(out)
	if strings.Contains(env, "HC_TERMINAL_ID=") || strings.Contains(env, "HC_SCOPE=") {
		t.Error("caller identity reached the program")
	}
	if !strings.Contains(env, "SHEPHERD_PROBE=kept") {
		t.Error("the rest of the environment was dropped")
	}
	out, code, err = runProgram(context.Background(), "/bin/sh", "-c", `echo '{"error":{"kind":"not_found","message":"m"}}' >&2; exit 3`)
	if err != nil || code != 3 || !strings.Contains(string(out), "not_found") {
		t.Errorf("got %q %d %v", out, code, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := runProgram(ctx, "/bin/sh", "-c", "sleep 5"); err == nil {
		t.Error("a program past its deadline must be an error")
	}
}
