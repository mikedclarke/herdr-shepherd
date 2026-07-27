package main

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func fieldKeys(f *formModel) []string {
	keys := make([]string, len(f.fields))
	for i, fd := range f.fields {
		keys[i] = fd.key
	}
	return keys
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestFormFieldsFollowKind(t *testing.T) {
	f := newFormModel(t.TempDir())
	keys := fieldKeys(f)
	for _, want := range []string{"name", "kind", "directory", "prompt", "preset", "hours", "minute"} {
		if !contains(keys, want) {
			t.Errorf("routine form missing %q: %v", want, keys)
		}
	}
	if contains(keys, "command") || contains(keys, "interval_minutes") {
		t.Errorf("routine form shows foreign fields: %v", keys)
	}

	f.values["kind"] = "script"
	f.rebuild()
	keys = fieldKeys(f)
	if !contains(keys, "command") || contains(keys, "prompt") {
		t.Errorf("script form fields wrong: %v", keys)
	}

	f.values["kind"] = "heartbeat"
	f.rebuild()
	keys = fieldKeys(f)
	if !contains(keys, "interval_minutes") || contains(keys, "preset") {
		t.Errorf("heartbeat form fields wrong: %v", keys)
	}
}

func TestFormPresetFieldsFollowPreset(t *testing.T) {
	f := newFormModel(t.TempDir())
	f.values["preset"] = "cron"
	f.rebuild()
	if keys := fieldKeys(f); !contains(keys, "cron") || contains(keys, "hours") {
		t.Errorf("cron preset fields wrong: %v", keys)
	}
	f.values["preset"] = "days"
	f.rebuild()
	if keys := fieldKeys(f); !contains(keys, "days") || !contains(keys, "hours") {
		t.Errorf("days preset fields wrong: %v", keys)
	}
}

func TestFormSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f := newFormModel(dir)
	f.values["name"] = "digest"
	f.values["prompt"] = "do the digest"
	f.values["preset"] = "weekdays"
	f.values["hours"] = "6, 18"
	f.values["minute"] = "15"
	f.rebuild()

	done, saved := f.trySave()
	if !done || !saved {
		t.Fatalf("save failed: %s", f.err)
	}
	actions, fileErrs, err := LoadActions(dir)
	if err != nil || len(fileErrs) > 0 || len(actions) != 1 {
		t.Fatalf("saved file does not load: %v %v", err, fileErrs)
	}
	a := actions[0]
	if a.Name != "digest" || a.Kind != KindRoutine || a.IsEnabled() {
		t.Errorf("wrong action written: %+v", a)
	}
	if len(a.Routine.Hours) != 2 || a.Routine.Hours[0] != 6 || a.Routine.Minute != 15 {
		t.Errorf("schedule not preserved: %+v", a.Routine)
	}
}

func TestFormSaveQuotesTrickyStrings(t *testing.T) {
	dir := t.TempDir()
	f := newFormModel(dir)
	f.values["name"] = "tricky"
	f.values["prompt"] = "say \"hi\"\nthen exit — carefully"
	f.rebuild()
	if done, saved := f.trySave(); !done || !saved {
		t.Fatalf("save failed: %s", f.err)
	}
	actions, fileErrs, _ := LoadActions(dir)
	if len(fileErrs) > 0 || len(actions) != 1 {
		t.Fatalf("quoted file does not parse: %v", fileErrs)
	}
	if actions[0].Prompt != f.values["prompt"] {
		t.Errorf("prompt mangled: %q", actions[0].Prompt)
	}
}

func TestFormRejectsInvalid(t *testing.T) {
	f := newFormModel(t.TempDir())
	f.values["name"] = "bad name" // whitespace
	if done, _ := f.trySave(); done {
		t.Fatal("saved an invalid name")
	}
	if f.err == "" {
		t.Error("no error surfaced to the form")
	}

	f.values["name"] = "ok"
	f.values["prompt"] = "p"
	f.values["minute"] = "not-a-number"
	if done, _ := f.trySave(); done {
		t.Fatal("saved a non-numeric minute")
	}
}

func TestFormNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "digest.toml", "name = \"digest\"\n")
	f := newFormModel(dir)
	f.values["name"] = "digest"
	f.values["prompt"] = "p"
	if done, _ := f.trySave(); done {
		t.Fatal("overwrote an existing action file")
	}
	if !strings.Contains(f.err, "exists") {
		t.Errorf("unexpected error: %s", f.err)
	}
}

func TestFormEditingAndCycle(t *testing.T) {
	f := newFormModel(t.TempDir())
	// Field 0 is name (text): enter starts editing, runes append, enter commits.
	f.press(tea.KeyMsg{Type: tea.KeyEnter})
	if !f.editing {
		t.Fatal("enter did not start editing the name")
	}
	for _, r := range "digest" {
		f.press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	f.press(tea.KeyMsg{Type: tea.KeyEnter})
	if f.editing || f.values["name"] != "digest" {
		t.Fatalf("edit not committed: editing=%v name=%q", f.editing, f.values["name"])
	}
	// Field 1 is kind (enum): space cycles routine → heartbeat and rebuilds.
	f.press(tea.KeyMsg{Type: tea.KeyDown})
	f.press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if f.values["kind"] != "heartbeat" {
		t.Errorf("cycle did not advance kind: %q", f.values["kind"])
	}
	if !contains(fieldKeys(f), "interval_minutes") {
		t.Error("fields not rebuilt after kind change")
	}
}

func TestBoardNOpensForm(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	m.press("n")
	if m.form == nil {
		t.Fatal("n did not open the form")
	}
	view := m.View()
	if !strings.Contains(view, "New action") {
		t.Errorf("form view not rendered:\n%s", view)
	}
	// esc closes without writing anything.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.form != nil {
		t.Error("esc did not close the form")
	}
}

func TestFooterKeyAt(t *testing.T) {
	// Mouse x positions are terminal columns (runes), not byte offsets — the
	// "·" separators are multi-byte, so convert before probing.
	text := footerText()
	col := func(sub string) int {
		return len([]rune(text[:strings.Index(text, sub)]))
	}
	if at := footerKeyAt(col("r run")); at != "r" {
		t.Errorf("r zone wrong: %q", at)
	}
	if at := footerKeyAt(col("q quit")); at != "q" {
		t.Errorf("q zone wrong: %q", at)
	}
	if at := footerKeyAt(0); at != "" {
		t.Errorf("↑↓ segment should be inert, got %q", at)
	}
}

func TestMouseSelectsAndOpensDetails(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	click := func(y int, btn tea.MouseButton) {
		m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: btn, Y: y})
	}
	click(boardRowsTop+1, tea.MouseButtonLeft)
	if m.cursor != 1 || m.detail != "" {
		t.Fatalf("first click should only select: cursor=%d detail=%q", m.cursor, m.detail)
	}
	click(boardRowsTop+1, tea.MouseButtonLeft)
	if m.detail != "sync" {
		t.Fatalf("second click should open details, got %q", m.detail)
	}
	click(boardRowsTop, tea.MouseButtonLeft)
	if m.detail != "" {
		t.Error("click in detail view should close it")
	}
	click(boardRowsTop, tea.MouseButtonRight)
	if m.detail != "digest" {
		t.Errorf("right click should open details directly, got %q", m.detail)
	}
}

func TestEditorCommandPrefersConfigured(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "myeditor")
	if got := editorCommand(); got != "myeditor" {
		t.Errorf("EDITOR not honoured: %q", got)
	}
	t.Setenv("EDITOR", "")
	got := editorCommand()
	if got != "nano" && got != "vi" {
		t.Errorf("unexpected fallback editor: %q", got)
	}
	if _, err := os.Stat("/usr/bin/nano"); err == nil && got != "nano" {
		t.Errorf("nano available but not chosen: %q", got)
	}
}
