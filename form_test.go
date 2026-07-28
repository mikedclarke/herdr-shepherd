package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
	f.values["name"] = "nightly-report"
	f.values["prompt"] = "write the nightly report"
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
	if a.Name != "nightly-report" || a.Kind != KindRoutine || a.IsEnabled() {
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
	writeFile(t, dir, "nightly-report.toml", "name = \"nightly-report\"\n")
	f := newFormModel(dir)
	f.values["name"] = "nightly-report"
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
	for _, r := range "nightly-report" {
		f.press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	f.press(tea.KeyMsg{Type: tea.KeyEnter})
	if f.editing || f.values["name"] != "nightly-report" {
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
	click := func(x, y int) {
		m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: x, Y: y})
	}
	click(0, boardRowsTop+1)
	if m.cursor != 1 || m.detail != "" {
		t.Fatalf("first click should only select: cursor=%d detail=%q", m.cursor, m.detail)
	}
	click(0, boardRowsTop+1)
	if m.detail != "sync-files" {
		t.Fatalf("second click should open details, got %q", m.detail)
	}
	click(0, boardRowsTop+10)
	if m.detail != "" {
		t.Error("click outside the button bar should close details")
	}
	// A single click on the "+ new action" row opens the form.
	click(0, boardRowsTop+3)
	if m.form == nil {
		t.Fatal("click on the new-action row did not open the form")
	}
	m.form = nil
}

func TestDetailButtons(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	m.press("enter") // nightly-report details
	if m.detail != "nightly-report" {
		t.Fatal("fixture: details not open")
	}
	view := m.viewDetail()
	if !strings.Contains(view, "[ edit ]") || !strings.Contains(view, "[ run now ]") {
		t.Errorf("detail button bar missing:\n%s", view)
	}
	// Click [ edit ] on the button bar row: zone 0 starts at x=0.
	m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 1, Y: detailButtonsY})
	if m.form == nil || m.form.editPath == "" {
		t.Fatal("edit button did not open the form in edit mode")
	}
	m.form = nil

	// Space in the detail view pauses the action.
	m.detail = "nightly-report"
	m.press(" ")
	if a := m.detailAction(); a == nil || a.IsEnabled() {
		t.Error("space in detail view did not pause the action")
	}
	if !strings.Contains(m.viewDetail(), "[ resume ]") {
		t.Error("button bar should offer resume once paused")
	}
	if key := detailButtonKeyAt(len([]rune("[ edit ]  [ run now ]  ")), true); key != " " {
		t.Errorf("pause/resume zone wrong: %q", key)
	}
}

func TestBoardEOpensFormForActions(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	m.press("e")
	if m.form == nil || m.form.editPath == "" || m.form.values["name"] != "nightly-report" {
		t.Fatalf("e did not open the edit form for nightly-report: %+v", m.form)
	}
	m.form = nil
	// Broken rows cannot load into the form; e falls back to the raw editor.
	m.cursor = 2
	if cmd := m.editSelected(); cmd == nil || m.form != nil {
		t.Error("e on a broken row should return an editor command, not a form")
	}
}

func TestFormEditPrefillSaveAndRename(t *testing.T) {
	p := boardFixture(t)
	m := newBoardModel(p)
	a := m.rows[0].action // nightly-report, enabled
	f := newFormModelForAction(a, p.ActionsDir())
	if f.values["name"] != "nightly-report" || f.values["enabled"] != "true" || f.values["preset"] != "weekdays" || f.values["hours"] != "6" {
		t.Fatalf("prefill wrong: %+v", f.values)
	}

	// Edit in place: change the minute, keep the name; enabled must survive.
	f.values["minute"] = "30"
	if done, saved := f.trySave(); !done || !saved {
		t.Fatalf("edit save failed: %s", f.err)
	}
	actions, fileErrs, _ := LoadActions(p.ActionsDir())
	if len(fileErrs) != 1 { // the fixture's broken.toml only
		t.Fatalf("unexpected file errors: %v", fileErrs)
	}
	var edited *Action
	for _, got := range actions {
		if got.Name == "nightly-report" {
			edited = got
		}
	}
	if edited == nil || edited.Routine.Minute != 30 || !edited.IsEnabled() {
		t.Fatalf("edit not persisted: %+v", edited)
	}

	// Rename: writes the new file and removes the old one.
	f2 := newFormModelForAction(edited, p.ActionsDir())
	f2.values["name"] = "nightly-report-am"
	if done, saved := f2.trySave(); !done || !saved {
		t.Fatalf("rename save failed: %s", f2.err)
	}
	if _, err := os.Stat(filepath.Join(p.ActionsDir(), "nightly-report.toml")); !os.IsNotExist(err) {
		t.Error("old file left behind after rename")
	}
	if _, err := os.Stat(filepath.Join(p.ActionsDir(), "nightly-report-am.toml")); err != nil {
		t.Error("renamed file missing")
	}

	// Renaming onto an existing action must refuse.
	actions, _, _ = LoadActions(p.ActionsDir())
	var am *Action
	for _, got := range actions {
		if got.Name == "nightly-report-am" {
			am = got
		}
	}
	f3 := newFormModelForAction(am, p.ActionsDir())
	f3.values["name"] = "sync-files"
	if done, _ := f3.trySave(); done {
		t.Fatal("rename onto an existing action was allowed")
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
	// nano lives in different places per platform and package manager; ask the
	// PATH the same way editorCommand does.
	if _, err := exec.LookPath("nano"); err == nil && got != "nano" {
		t.Errorf("nano available but not chosen: %q", got)
	}
}

func TestFormWorkingHoursSurviveAnUnrelatedEdit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hourly-check.toml", `name = "hourly-check"
kind = "heartbeat"
directory = "~"
prompt = "check the queue"
enabled = true

[heartbeat]
interval_minutes = 60

[heartbeat.working_hours]
days = [1, 2, 3, 4, 5]
start_hour = 9
end_hour = 18
`)
	actions, fileErrs, err := LoadActions(dir)
	if err != nil || len(fileErrs) > 0 || len(actions) != 1 {
		t.Fatalf("fixture does not load: %v %v", err, fileErrs)
	}
	f := newFormModelForAction(actions[0], dir)
	if f.values["wh_days"] != "1,2,3,4,5" || f.values["start_hour"] != "9" || f.values["end_hour"] != "18" {
		t.Fatalf("working hours not prefilled: %+v", f.values)
	}
	if !contains(fieldKeys(f), "start_hour") {
		t.Fatalf("heartbeat form has no working-hours fields: %v", fieldKeys(f))
	}

	f.values["name"] = "hourly-check-2"
	if done, saved := f.trySave(); !done || !saved {
		t.Fatalf("save failed: %s", f.err)
	}
	actions, fileErrs, _ = LoadActions(dir)
	if len(fileErrs) > 0 || len(actions) != 1 {
		t.Fatalf("saved file does not load: %v", fileErrs)
	}
	wh := actions[0].Heartbeat.WorkingHours
	if wh == nil || wh.StartHour != 9 || wh.EndHour != 18 || len(wh.Days) != 5 || wh.Days[0] != 1 {
		t.Fatalf("working hours lost across a name-only edit: %+v", wh)
	}
	if actions[0].Heartbeat.IntervalMinutes != 60 {
		t.Errorf("interval lost: %+v", actions[0].Heartbeat)
	}
}

func TestFormBlankWorkingHoursWriteNoTable(t *testing.T) {
	dir := t.TempDir()
	f := newFormModel(dir)
	f.values["kind"] = "heartbeat"
	f.values["name"] = "hourly-check"
	f.values["prompt"] = "check the queue"
	f.rebuild()
	if done, saved := f.trySave(); !done || !saved {
		t.Fatalf("save failed: %s", f.err)
	}
	data := mustRead(t, filepath.Join(dir, "hourly-check.toml"))
	if strings.Contains(string(data), "working_hours") {
		t.Errorf("blank fields wrote a working-hours table:\n%s", data)
	}
	actions, fileErrs, _ := LoadActions(dir)
	if len(fileErrs) > 0 || len(actions) != 1 {
		t.Fatalf("saved file does not load: %v", fileErrs)
	}
	if actions[0].Heartbeat.WorkingHours != nil {
		t.Errorf("unexpected working hours: %+v", actions[0].Heartbeat.WorkingHours)
	}
}

func TestFormRejectsUnusableWorkingHours(t *testing.T) {
	dir := t.TempDir()
	f := newFormModel(dir)
	f.values["kind"] = "heartbeat"
	f.values["name"] = "hourly-check"
	f.values["prompt"] = "check the queue"
	f.rebuild()

	// Days alone cannot be written: the hours would default to an empty window.
	f.values["wh_days"] = "1,2,3"
	if done, _ := f.trySave(); done {
		t.Fatal("saved a days-only working-hours block")
	}
	if !strings.Contains(f.err, "hours") {
		t.Errorf("unhelpful error: %s", f.err)
	}

	f.values["wh_days"] = ""
	f.values["start_hour"] = "9"
	if done, _ := f.trySave(); done {
		t.Fatal("saved a half-specified window")
	}

	// validate() rejects a window that starts and ends at the same hour.
	f.values["end_hour"] = "9"
	if done, _ := f.trySave(); done {
		t.Fatal("saved a zero-length window")
	}

	f.values["end_hour"] = "18"
	if done, saved := f.trySave(); !done || !saved {
		t.Fatalf("valid window rejected: %s", f.err)
	}
}

func TestFormErrorClearsOnTheNextEdit(t *testing.T) {
	f := newFormModel(t.TempDir())
	f.values["name"] = "bad name"
	if done, _ := f.trySave(); done {
		t.Fatal("saved an invalid name")
	}
	if f.err == "" {
		t.Fatal("no error surfaced to the form")
	}
	f.cursor = 0
	f.activate()
	if f.err != "" {
		t.Errorf("stale error survived into the edit: %s", f.err)
	}
}

func TestCtrlCQuitsFromAHalfTypedField(t *testing.T) {
	m := newBoardModel(boardFixture(t))
	m.press("n")
	if m.form == nil {
		t.Fatal("n did not open the form")
	}
	m.form.activate()
	if !m.form.editing {
		t.Fatal("fixture: the form is not mid-edit")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c produced no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("ctrl+c did not quit: %T", cmd())
	}
}
