package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readRunLog(t *testing.T, path string) []runRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	defer f.Close()
	var recs []runRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r runRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("corrupt run log line %q: %v", sc.Text(), err)
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestStateSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := emptyState()
	when := time.Date(2026, 7, 24, 6, 0, 0, 0, time.Local)
	st.setLastRun("build-sync", when)
	st.setStatus("build-sync", "completed")
	st.beat(when)
	if err := st.save(path); err != nil {
		t.Fatal(err)
	}
	got := loadState(path)
	if !got.lastRun("build-sync").Equal(when) || got.lastStatus("build-sync") != "completed" || !got.heartbeatAt().Equal(when) {
		t.Errorf("round trip lost data: %+v", got)
	}
}

// Mirrors the daemon's real call pattern: the tick goroutine stamps and saves
// while fire goroutines set statuses. scripts/check.sh runs this under -race.
func TestStateConcurrentAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := emptyState()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := string(rune('a' + n%4))
			for j := 0; j < 200; j++ {
				st.setLastRun(name, time.Now())
				st.setStatus(name, "completed")
				st.lastRun(name)
				st.beat(time.Now())
				if err := st.save(path); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	if got := loadState(path); got.Actions == nil {
		t.Fatal("state unreadable after concurrent saves")
	}
}

func TestLoadStatePreservesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte("{not json"), 0o644)
	st := loadState(path)
	if len(st.Actions) != 0 {
		t.Errorf("corrupt state should load empty")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("corrupt file should be preserved: %v", err)
	}
}

func TestReadStateLeavesTheFileAlone(t *testing.T) {
	// A reader that quarantines the daemon's state file resets every
	// schedule as a side effect of running `list`.
	path := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(path, []byte("{not json"), 0o644)
	st := readState(path)
	if len(st.Actions) != 0 {
		t.Errorf("unreadable state should load empty")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the state file should be left in place: %v", err)
	}
	if _, err := os.Stat(path + ".corrupt"); err == nil {
		t.Error("a reader must not quarantine")
	}
}

func TestStatePrune(t *testing.T) {
	st := emptyState()
	st.setLastRun("keep", time.Now())
	st.setLastRun("gone", time.Now())
	st.prune(map[string]bool{"keep": true})
	if !st.lastRun("gone").IsZero() || st.lastRun("keep").IsZero() {
		t.Errorf("prune kept the wrong entries: %+v", st.Actions)
	}
}

func TestAppendRunLogConcurrentWriters(t *testing.T) {
	// The daemon, the board and the CLI all append to one file.
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rec := runRecord{At: time.Now(), Action: "build-sync", Kind: KindScript, Status: "completed"}
				if err := appendRunLog(path, rec); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	if got := readRunLog(t, path); len(got) != 400 {
		t.Fatalf("got %d records, want 400", len(got))
	}
}

func TestAppendRunLogRotatesAndCapsDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(path, make([]byte, runLogMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	detail := strings.Repeat("x", 4096)
	err := appendRunLog(path, runRecord{
		At: time.Now(), Action: "build-sync", Kind: KindScript, Status: "error",
		Detail: detail, Trigger: triggerManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("an oversized log should be rotated: %v", err)
	}
	recs := readRunLog(t, path)
	if len(recs) != 1 || recs[0].Trigger != triggerManual {
		t.Fatalf("got %+v", recs)
	}
	if len(recs[0].Detail) > runDetailMaxLen+len(tailMarker) {
		t.Errorf("detail should be capped, got %d bytes", len(recs[0].Detail))
	}
}

func TestRunRecordDurationOmittedWhenZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	write := func(r runRecord) {
		if err := appendRunLog(path, r); err != nil {
			t.Fatal(err)
		}
	}
	write(runRecord{At: time.Now(), Action: "nightly-report", Kind: KindRoutine, Status: "started"})
	write(runRecord{At: time.Now(), Action: "nightly-report", Kind: KindRoutine, Status: "completed", DurationSecs: 92.5})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if strings.Contains(lines[0], "duration_secs") {
		t.Errorf("a record without a duration must omit the field: %s", lines[0])
	}
	recs := readRunLog(t, path)
	if len(recs) != 2 || recs[1].DurationSecs != 92.5 {
		t.Fatalf("duration should round-trip, got %+v", recs)
	}
}

func TestMarkInterruptedClosesDanglingRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	write := func(r runRecord) {
		if err := appendRunLog(path, r); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	write(runRecord{At: now, Action: "nightly-report", Kind: KindRoutine, Status: "started", Detail: "workspace ws1"})
	write(runRecord{At: now, Action: "nightly-report", Kind: KindRoutine, Status: "completed", Detail: "session finished; workspace ws1"})
	write(runRecord{At: now, Action: "nightly-report", Kind: KindRoutine, Status: "started", Detail: "workspace ws2"})
	write(runRecord{At: now, Action: "hourly-check", Kind: KindHeartbeat, Status: "started", Detail: "workspace ws3", Trigger: triggerManual})

	if err := markInterrupted(path); err != nil {
		t.Fatal(err)
	}
	recs := readRunLog(t, path)
	if len(recs) != 5 {
		t.Fatalf("expected one appended record, got %d", len(recs))
	}
	last := recs[4]
	if last.Status != "interrupted" || last.Action != "nightly-report" || last.Detail != "workspace ws2" {
		t.Fatalf("got %+v", last)
	}

	// Running again must not report the same run twice: the interrupted
	// record itself closes it out.
	if err := markInterrupted(path); err != nil {
		t.Fatal(err)
	}
	if got := readRunLog(t, path); len(got) != 5 {
		t.Errorf("a second scan should append nothing, got %d records", len(got))
	}
}

func TestMarkInterruptedMissingFile(t *testing.T) {
	if err := markInterrupted(filepath.Join(t.TempDir(), "runs.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestRunRecordWorkspaceID(t *testing.T) {
	cases := map[string]string{
		"workspace ws1":                          "ws1",
		"session finished; workspace ws1":        "ws1",
		"agent exited; closed workspace; ws1":    "",
		"session finished; closed workspace; wo": "",
		"timed out after 30m":                    "",
	}
	for detail, want := range cases {
		if got := (runRecord{Detail: detail}).workspaceID(); got != want {
			t.Errorf("%q: got %q, want %q", detail, got, want)
		}
	}
}

func TestResolvePathsEnvPrecedence(t *testing.T) {
	// A herdr that cannot be asked must not reach the real one on this host.
	t.Setenv("HERDR_BIN_PATH", filepath.Join(t.TempDir(), "no-such-herdr"))

	cfg, state := t.TempDir(), t.TempDir()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", cfg)
	t.Setenv("HERDR_PLUGIN_STATE_DIR", state)
	p := resolvePaths()
	if p.ConfigDir != cfg || p.StateDir != state {
		t.Fatalf("injected plugin dirs should win: %+v", p)
	}
	if p.ActionsDir() != filepath.Join(cfg, "actions") {
		t.Errorf("got %s", p.ActionsDir())
	}

	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	t.Setenv("HERDR_PLUGIN_STATE_DIR", "")
	xdgCfg, xdgState := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgCfg)
	t.Setenv("XDG_STATE_HOME", xdgState)
	p = resolvePaths()
	if p.ConfigDir != filepath.Join(xdgCfg, "herdr-shepherd") {
		t.Errorf("XDG config fallback: got %s", p.ConfigDir)
	}
	if p.StateDir != filepath.Join(xdgState, "herdr-shepherd") {
		t.Errorf("XDG state fallback: got %s", p.StateDir)
	}
}
