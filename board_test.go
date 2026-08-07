package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func boardFixture(t *testing.T) paths {
	t.Helper()
	cfg := t.TempDir()
	state := t.TempDir()
	actions := filepath.Join(cfg, "actions")
	os.MkdirAll(actions, 0o755)
	writeFile(t, actions, "nightly-report.toml", `name = "nightly-report"
kind = "routine"
directory = "~"
prompt = "go"
enabled = true

[schedule]
preset = "weekdays"
hours = [6]
minute = 15
`)
	writeFile(t, actions, "sync-files.toml", `name = "sync-files"
kind = "script"
directory = "~"
command = "true"
enabled = false

[schedule]
preset = "daily"
`)
	writeFile(t, actions, "broken.toml", "name = \"broken\"\nkind = \"nope\"\n")
	return paths{ConfigDir: cfg, StateDir: state}
}

func TestBoardModelRowsAndCursor(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	if len(m.rows) != 4 {
		t.Fatalf("expected 4 rows (2 actions + 1 broken + new button), got %d", len(m.rows))
	}
	if m.rows[2].action != nil || m.rows[2].errFile != "broken.toml" {
		t.Fatalf("broken file should precede the new-action row: %+v", m.rows[2])
	}
	if !m.rows[3].isNew {
		t.Fatalf("last row should be the new-action button: %+v", m.rows[3])
	}

	m.press("k")
	if m.cursor != 0 {
		t.Errorf("cursor moved above the first row: %d", m.cursor)
	}
	for i := 0; i < 4; i++ {
		m.press("j")
	}
	if m.cursor != 3 {
		t.Errorf("cursor moved past the last row: %d", m.cursor)
	}
}

func TestBoardToggleWritesFile(t *testing.T) {
	p := boardFixture(t)
	m := newBoardModel(p)
	if m.rows[0].action.Name != "nightly-report" || !m.rows[0].action.IsEnabled() {
		t.Fatalf("fixture: expected enabled nightly-report first, got %+v", m.rows[0].action)
	}
	m.press(" ")
	data := mustRead(t, filepath.Join(p.ActionsDir(), "nightly-report.toml"))
	if !strings.Contains(string(data), "enabled = false") {
		t.Errorf("toggle did not reach the file:\n%s", data)
	}
	if m.rows[0].action.IsEnabled() {
		t.Error("model did not reload after toggle")
	}
	if m.cursor != 0 {
		t.Errorf("cursor jumped on reload: %d", m.cursor)
	}
}

func TestBoardToggleOnBrokenRowIsSafe(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	m.cursor = 2
	m.press(" ")
	if m.status == "" {
		t.Error("expected a hint status for a broken row")
	}
}

func TestBoardReloadKeepsSelectionByName(t *testing.T) {
	p := boardFixture(t)
	m := newBoardModel(p)
	m.cursor = 1 // sync-files
	// A new action that sorts first shifts row order; selection must follow
	// the name, not the index.
	writeFile(t, filepath.Join(p.ActionsDir()), "aaa.toml", "name = \"aaa\"\nkind = \"script\"\ndirectory = \"~\"\ncommand = \"true\"\n")
	m.reload()
	if got := m.rows[m.cursor].action; got == nil || got.Name != "sync-files" {
		t.Errorf("selection did not follow the action across reload: %+v", got)
	}
}

func TestBoardViewsRender(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	view := m.viewBoard()
	for _, want := range []string{"nightly-report", "sync-files", "broken.toml", "1/2 enabled", "daemon not running"} {
		if !strings.Contains(view, want) {
			t.Errorf("board view missing %q:\n%s", want, view)
		}
	}
	m.press("enter")
	if m.detail != "nightly-report" {
		t.Fatalf("enter did not open details: %q", m.detail)
	}
	detail := m.viewDetail()
	for _, want := range []string{"nightly-report", "weekdays", "Recent runs"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail view missing %q:\n%s", want, detail)
		}
	}
	m.press("esc")
	if m.detail != "" {
		t.Error("esc did not close details")
	}
}

func TestActionHistoryFiltersAndCaps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.jsonl")
	f, _ := os.Create(path)
	base := time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		rec := runRecord{At: base.Add(time.Duration(i) * time.Hour), Action: "nightly-report", Kind: KindRoutine, Status: "completed"}
		data, _ := json.Marshal(rec)
		f.Write(append(data, '\n'))
	}
	f.WriteString(`{"action":"other","status":"completed"}` + "\n")
	f.WriteString("not json\n")
	f.Close()

	recs := actionHistory(path, "nightly-report", 8)
	if len(recs) != 8 {
		t.Fatalf("expected 8 records, got %d", len(recs))
	}
	if !recs[0].At.After(recs[len(recs)-1].At) {
		t.Error("history is not newest-first")
	}
	for _, r := range recs {
		if r.Action != "nightly-report" {
			t.Errorf("foreign action leaked into history: %q", r.Action)
		}
	}
	if got := actionHistory(path, "missing", 8); len(got) != 0 {
		t.Errorf("expected empty history, got %d", len(got))
	}
}

func TestBoardIgnoresTempFiles(t *testing.T) {
	p := boardFixture(t)
	// What a killed board session leaves behind; the loader must not see them
	// as actions.
	writeFile(t, p.ActionsDir(), ".form-123.tmp", "half a file")
	writeFile(t, p.ActionsDir(), ".enabled-456.tmp", "half a file")
	m := newBoardModel(p)
	if len(m.rows) != 4 {
		t.Fatalf("temp files surfaced as rows: %d rows", len(m.rows))
	}
	for _, r := range m.rows {
		if strings.Contains(r.errFile, ".tmp") {
			t.Errorf("temp file reported as a broken action: %+v", r)
		}
	}
}

func TestBoardRunScriptLocksLogsAndReleases(t *testing.T) {
	p := boardFixture(t)
	m := newBoardModel(p)
	m.cursor = 1
	if a := m.rows[1].action; a == nil || a.Kind != KindScript {
		t.Fatalf("fixture: expected a script at row 1, got %+v", m.rows[1])
	}
	cmd := m.runSelected()
	if cmd == nil {
		t.Fatal("run produced no command")
	}
	if !m.running["sync-files"] {
		t.Error("run did not mark the action as running")
	}
	if !runLockHeld(p.StateDir, "sync-files") {
		t.Error("run lock not held while the script runs")
	}
	msg, ok := cmd().(scriptDoneMsg)
	if !ok {
		t.Fatalf("unexpected message type %T", cmd())
	}
	if msg.err != nil || msg.logErr != nil {
		t.Fatalf("script run failed: %v / %v", msg.err, msg.logErr)
	}
	m.Update(msg)
	if m.running["sync-files"] {
		t.Error("running flag survived completion")
	}
	if runLockHeld(p.StateDir, "sync-files") {
		t.Error("run lock not released after completion")
	}
	recs := actionHistory(p.RunLogFile(), "sync-files", 4)
	if len(recs) != 1 {
		t.Fatalf("expected one run record, got %d", len(recs))
	}
	if recs[0].Status != "completed" || recs[0].Trigger != triggerManual {
		t.Errorf("wrong run record: %+v", recs[0])
	}
	if recs[0].DurationSecs <= 0 {
		t.Errorf("a board run must record its duration, got %v", recs[0].DurationSecs)
	}
}

func TestFmtRunDuration(t *testing.T) {
	cases := []struct {
		secs float64
		want string
	}{
		{0.412, "0.4s"},
		{12.6, "13s"},
		{123, "2m03s"},
		{3845, "1h04m"},
	}
	for _, c := range cases {
		if got := fmtRunDuration(c.secs); got != c.want {
			t.Errorf("fmtRunDuration(%v) = %q, want %q", c.secs, got, c.want)
		}
	}
}

func TestBoardRunRefusesWhileLockHeld(t *testing.T) {
	p := boardFixture(t)
	release, ok, err := tryRunLock(p.StateDir, "sync-files")
	if err != nil || !ok {
		t.Fatalf("could not take the lock: %v %v", err, ok)
	}
	defer release()
	m := newBoardModel(p)
	m.cursor = 1
	if cmd := m.runSelected(); cmd != nil {
		t.Fatal("started a second run while the lock was held")
	}
	if !strings.Contains(m.status, "already running") {
		t.Errorf("no refusal surfaced: %q", m.status)
	}
	// A run any process holds shows on the row, scheduled runs included.
	m.reload()
	if !strings.Contains(m.renderRow(m.rows[1]), "running…") {
		t.Errorf("held lock not shown on the row: %q", m.renderRow(m.rows[1]))
	}
}

func TestBoardAgentStartedReleasesTheLaunchLock(t *testing.T) {
	p := boardFixture(t)
	m := newBoardModel(p)
	release, ok, err := tryRunLock(p.StateDir, "nightly-report")
	if err != nil || !ok {
		t.Fatalf("could not take the lock: %v %v", err, ok)
	}
	m.running["nightly-report"] = true
	m.locks["nightly-report"] = release
	m.Update(agentStartedMsg{name: "nightly-report", ws: "ws-1"})
	if m.running["nightly-report"] {
		t.Error("running flag survived the launch")
	}
	if runLockHeld(p.StateDir, "nightly-report") {
		t.Error("launch lock not released once the workspace was up")
	}
	if !strings.Contains(m.status, "ws-1") {
		t.Errorf("workspace not reported: %q", m.status)
	}
}

func TestBoardEditorDoneReportsFailure(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	m.Update(editorDoneMsg{err: errors.New("editor blew up")})
	if !strings.Contains(m.status, "editor blew up") {
		t.Errorf("editor failure not surfaced: %q", m.status)
	}
}

func TestStatusGlyphCoversLaunchStates(t *testing.T) {
	for _, status := range []string{"started", "interrupted"} {
		if got := statusGlyph(status); got == status {
			t.Errorf("%s has no glyph", status)
		}
	}
}

func TestTickSuppressesReloadWhileFormOpen(t *testing.T) {
	p := boardFixture(t)
	m := newBoardModel(p)
	before := len(m.rows)
	m.form = newFormModel(p.ActionsDir())
	writeFile(t, p.ActionsDir(), "z-extra.toml",
		"name = \"z-extra\"\nkind = \"script\"\ndirectory = \"~\"\ncommand = \"true\"\n")
	m.Update(tickMsg(time.Now()))
	if len(m.rows) != before {
		t.Errorf("rows reloaded under an open form: %d → %d", before, len(m.rows))
	}
	m.form = nil
	m.Update(tickMsg(time.Now()))
	if len(m.rows) != before+1 {
		t.Errorf("rows did not reload once the form closed: %d", len(m.rows))
	}
}

func TestTickExpiresStatus(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	m.note("something happened")
	m.Update(tickMsg(time.Now()))
	if m.status == "" {
		t.Fatal("status cleared before it could be read")
	}
	m.statusAt = time.Now().Add(-2 * statusLinger)
	m.Update(tickMsg(time.Now()))
	if m.status != "" {
		t.Errorf("status outlived its linger: %q", m.status)
	}
}

func TestReloadSkipsUnchangedActionFiles(t *testing.T) {
	p := boardFixture(t)
	m := newBoardModel(p)
	m.rows[0].action.Name = "in-memory-only"
	m.reload()
	if m.rows[0].action.Name != "in-memory-only" {
		t.Error("reload re-parsed action files that had not changed")
	}
	writeFile(t, p.ActionsDir(), "z-extra.toml",
		"name = \"z-extra\"\nkind = \"script\"\ndirectory = \"~\"\ncommand = \"true\"\n")
	m.reload()
	if m.rows[0].action.Name != "nightly-report" {
		t.Errorf("reload missed a changed actions dir: %q", m.rows[0].action.Name)
	}
}

func TestBoardHistoryCacheSkipsReread(t *testing.T) {
	p := boardFixture(t)
	m := newBoardModel(p)
	if err := appendRunLog(p.RunLogFile(), runRecord{
		At: time.Now(), Action: "nightly-report", Kind: KindRoutine, Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	m.detail = "nightly-report"
	m.history = m.historyFor("nightly-report")
	if len(m.history) != 1 {
		t.Fatalf("expected one record, got %d", len(m.history))
	}
	// A cache hit hands back what the model already holds; a re-read would
	// drop this marker.
	m.history = append(m.history, runRecord{Action: "nightly-report", Status: "marker"})
	got := m.historyFor("nightly-report")
	if len(got) != 2 || got[1].Status != "marker" {
		t.Errorf("history re-parsed an unchanged log: %+v", got)
	}
	if err := appendRunLog(p.RunLogFile(), runRecord{
		At: time.Now(), Action: "nightly-report", Kind: KindRoutine, Status: "error",
	}); err != nil {
		t.Fatal(err)
	}
	m.history = m.historyFor("nightly-report")
	if len(m.history) != 2 || m.history[0].Status != "error" {
		t.Errorf("history not refreshed after the log grew: %+v", m.history)
	}
}

func TestActionHistoryReadsTailAndRotatedLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.jsonl")
	write := func(f *os.File, r runRecord) {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(append(data, '\n'))
	}
	base := time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC)

	rotated, err := os.Create(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		write(rotated, runRecord{At: base.Add(time.Duration(i) * time.Hour),
			Action: "nightly-report", Kind: KindRoutine, Status: "completed",
			Detail: fmt.Sprintf("old-%d", i)})
	}
	rotated.Close()

	current, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	write(current, runRecord{At: base, Action: "nightly-report", Kind: KindRoutine,
		Status: "completed", Detail: "buried"})
	filler := strings.Repeat("x", 300)
	for i := 0; i < 600; i++ {
		write(current, runRecord{At: base, Action: "other", Kind: KindScript,
			Status: "completed", Detail: filler})
	}
	write(current, runRecord{At: base.Add(24 * time.Hour), Action: "nightly-report",
		Kind: KindRoutine, Status: "error", Detail: "new"})
	current.Close()

	recs := actionHistory(path, "nightly-report", 8)
	if len(recs) != 5 {
		t.Fatalf("expected 1 tail record + 4 rotated, got %d: %+v", len(recs), recs)
	}
	if recs[0].Detail != "new" || recs[4].Detail != "old-0" {
		t.Errorf("wrong order across the rotation boundary: %+v", recs)
	}
	for _, r := range recs {
		if r.Detail == "buried" {
			t.Error("a record beyond the tail window was read")
		}
	}
}

func TestRenderRowUsesDisplayWidth(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	row := func(name string) string {
		a := &Action{Name: name, Kind: KindScript, Directory: "~", Command: "true"}
		a.applyDefaults()
		return m.renderRow(boardRow{action: a})
	}
	ascii, wide := row("build-sync"), row("夜間レポート")
	if lipgloss.Width(ascii) != lipgloss.Width(wide) {
		t.Errorf("multi-byte name shifted the columns: %d vs %d\n%s\n%s",
			lipgloss.Width(ascii), lipgloss.Width(wide), ascii, wide)
	}
	long := m.renderRow(boardRow{errFile: strings.Repeat("é", 40), errText: strings.Repeat("ü", 90)})
	if !utf8.ValidString(long) {
		t.Error("truncation cut a rune in half")
	}
	if got := truncate(strings.Repeat("夜", 30), 10); lipgloss.Width(got) > 10 {
		t.Errorf("truncate overshot the cell budget: %q (%d)", got, lipgloss.Width(got))
	}
}

func TestRenderRowClampsToTerminalWidth(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	m.width = 30
	for _, r := range m.rows {
		if w := lipgloss.Width(m.renderRow(r)); w > 30 {
			t.Errorf("row wider than the terminal: %d", w)
		}
	}
}

func TestBoardScrollsToKeepSelectionVisible(t *testing.T) {
	p := boardFixture(t)
	for i := 0; i < 8; i++ {
		writeFile(t, p.ActionsDir(), fmt.Sprintf("job-%02d.toml", i), fmt.Sprintf(
			"name = \"job-%02d\"\nkind = \"script\"\ndirectory = \"~\"\ncommand = \"true\"\n", i))
	}
	m := newBoardModel(p)
	m.height = boardRowsTop + 3 + boardFooterLines
	if m.visibleRows() != 3 {
		t.Fatalf("window size wrong: %d", m.visibleRows())
	}
	m.cursor = len(m.rows) - 1
	view := m.viewBoard()
	if !strings.Contains(view, "+ new action…") {
		t.Errorf("selection scrolled out of view:\n%s", view)
	}
	if m.top != len(m.rows)-3 {
		t.Errorf("window did not follow the cursor: top=%d", m.top)
	}
	// The click arithmetic follows the window: the first drawn line is m.top.
	m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: boardRowsTop})
	if m.cursor != len(m.rows)-3 {
		t.Errorf("click mapped to the wrong row: cursor=%d top=%d", m.cursor, m.top)
	}
	m.cursor = 0
	m.viewBoard()
	if m.top != 0 {
		t.Errorf("window did not scroll back: top=%d", m.top)
	}
}
