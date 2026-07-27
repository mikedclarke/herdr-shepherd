package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func boardFixture(t *testing.T) paths {
	t.Helper()
	cfg := t.TempDir()
	state := t.TempDir()
	actions := filepath.Join(cfg, "actions")
	os.MkdirAll(actions, 0o755)
	writeFile(t, actions, "digest.toml", `name = "digest"
kind = "routine"
directory = "~"
prompt = "go"
enabled = true

[schedule]
preset = "weekdays"
hours = [6]
minute = 15
`)
	writeFile(t, actions, "sync.toml", `name = "sync"
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
	if len(m.rows) != 3 {
		t.Fatalf("expected 3 rows (2 actions + 1 broken), got %d", len(m.rows))
	}
	if m.rows[2].action != nil || m.rows[2].errFile != "broken.toml" {
		t.Fatalf("broken file should be the last row: %+v", m.rows[2])
	}

	m.press("k")
	if m.cursor != 0 {
		t.Errorf("cursor moved above the first row: %d", m.cursor)
	}
	m.press("j")
	m.press("j")
	m.press("j")
	if m.cursor != 2 {
		t.Errorf("cursor moved past the last row: %d", m.cursor)
	}
}

func TestBoardToggleWritesFile(t *testing.T) {
	p := boardFixture(t)
	m := newBoardModel(p)
	if m.rows[0].action.Name != "digest" || !m.rows[0].action.IsEnabled() {
		t.Fatalf("fixture: expected enabled digest first, got %+v", m.rows[0].action)
	}
	m.press(" ")
	data := mustRead(t, filepath.Join(p.ActionsDir(), "digest.toml"))
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
	m.cursor = 1 // sync
	// A new action that sorts first shifts row order; selection must follow
	// the name, not the index.
	writeFile(t, filepath.Join(p.ActionsDir()), "aaa.toml", "name = \"aaa\"\nkind = \"script\"\ndirectory = \"~\"\ncommand = \"true\"\n")
	m.reload()
	if got := m.rows[m.cursor].action; got == nil || got.Name != "sync" {
		t.Errorf("selection did not follow the action across reload: %+v", got)
	}
}

func TestBoardViewsRender(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	view := m.viewBoard()
	for _, want := range []string{"digest", "sync", "broken.toml", "1/2 enabled", "daemon not running"} {
		if !strings.Contains(view, want) {
			t.Errorf("board view missing %q:\n%s", want, view)
		}
	}
	m.press("enter")
	if m.detail != "digest" {
		t.Fatalf("enter did not open details: %q", m.detail)
	}
	detail := m.viewDetail()
	for _, want := range []string{"digest", "weekdays", "Recent runs"} {
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
		rec := runRecord{At: base.Add(time.Duration(i) * time.Hour), Action: "digest", Kind: KindRoutine, Status: "completed"}
		data, _ := json.Marshal(rec)
		f.Write(append(data, '\n'))
	}
	f.WriteString(`{"action":"other","status":"completed"}` + "\n")
	f.WriteString("not json\n")
	f.Close()

	recs := actionHistory(path, "digest", 8)
	if len(recs) != 8 {
		t.Fatalf("expected 8 records, got %d", len(recs))
	}
	if !recs[0].At.After(recs[len(recs)-1].At) {
		t.Error("history is not newest-first")
	}
	for _, r := range recs {
		if r.Action != "digest" {
			t.Errorf("foreign action leaked into history: %q", r.Action)
		}
	}
	if got := actionHistory(path, "missing", 8); len(got) != 0 {
		t.Errorf("expected empty history, got %d", len(got))
	}
}
