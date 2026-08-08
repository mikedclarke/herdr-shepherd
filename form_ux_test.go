package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func fieldIndex(f *formModel, key string) int {
	for i, fd := range f.fields {
		if fd.key == key {
			return i
		}
	}
	return -1
}

func TestModelPickerCyclesAndTakesCustom(t *testing.T) {
	f := newFormModel(t.TempDir()) // cli defaults to claude
	i := fieldIndex(f, "model")
	if i < 0 || f.fields[i].ftype != ftChoice {
		t.Fatalf("claude model field is not a picker: %+v", f.fields[i])
	}
	f.cursor = i

	// Default is the blank first choice; one step lands on the first real id.
	if f.values["model"] != "" {
		t.Fatalf("model should start blank, got %q", f.values["model"])
	}
	f.cycle(1)
	if f.values["model"] != claudeModelChoices[1].value {
		t.Fatalf("cycle did not advance model: %q", f.values["model"])
	}

	// Cycle to the trailing custom slot: it opens free-text entry, and typed
	// runes commit as any id we like.
	f.values["model"] = claudeModelChoices[len(claudeModelChoices)-1].value
	f.cursor = fieldIndex(f, "model")
	f.cycle(1)
	if !f.editing {
		t.Fatal("cycling onto custom… did not open text entry")
	}
	for _, r := range "claude-something-new" {
		f.press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	f.press(tea.KeyMsg{Type: tea.KeyEnter})
	if f.values["model"] != "claude-something-new" {
		t.Fatalf("custom model not committed: %q", f.values["model"])
	}

	// It survives a save round-trip as a plain model key.
	f.values["name"] = "custom-model"
	f.values["prompt"] = "do the thing"
	if done, saved := f.trySave(); !done || !saved {
		t.Fatalf("save failed: %s", f.err)
	}
	actions, fileErrs, _ := LoadActions(f.dir)
	if len(fileErrs) > 0 || len(actions) != 1 || actions[0].Model != "claude-something-new" {
		t.Fatalf("model not persisted: %v %v", fileErrs, actions)
	}
}

func TestModelFieldIsFreeTextForCodex(t *testing.T) {
	f := newFormModel(t.TempDir())
	f.values["cli"] = "codex"
	f.rebuild()
	i := fieldIndex(f, "model")
	if i < 0 || f.fields[i].ftype != ftText {
		t.Fatalf("codex model field should be free text, got %+v", f.fields[i])
	}
}

func TestDayChipsToggleTheStoredCSV(t *testing.T) {
	f := newFormModel(t.TempDir()) // routine, preset weekdays by default
	f.values["preset"] = "days"
	f.values["days"] = "1,2,3,4,5"
	f.rebuild()
	i := fieldIndex(f, "days")
	if i < 0 || f.fields[i].ftype != ftDays {
		t.Fatalf("days field is not chips: %+v", f.fields[i])
	}
	f.cursor = i

	// dayOrder is Mon-first: index 0 is Monday (day 1). Space removes it.
	f.dayCursor = 0
	f.press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if got := parseDaySet(f.values["days"]); got[1] {
		t.Fatalf("space did not clear Monday: %q", f.values["days"])
	}
	// Move to Saturday (dayOrder index 5 -> day 6) and add it.
	for j := 0; j < 5; j++ {
		f.press(tea.KeyMsg{Type: tea.KeyRight})
	}
	f.press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if got := parseDaySet(f.values["days"]); !got[6] {
		t.Fatalf("space did not add Saturday: %q", f.values["days"])
	}
	// Stored value stays a sorted numeric CSV the loader understands.
	if f.values["days"] != "2,3,4,5,6" {
		t.Fatalf("unexpected CSV after toggles: %q", f.values["days"])
	}
}

func TestSchedulePreviewNextRuns(t *testing.T) {
	f := newFormModel(t.TempDir())
	f.values["kind"] = "routine"
	f.values["preset"] = "daily"
	f.values["hours"] = "9"
	f.values["minute"] = "0"
	f.rebuild()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local) // past 09:00 today
	times := f.previewTimes(now, 3)
	if len(times) != 3 {
		t.Fatalf("want 3 preview times, got %d", len(times))
	}
	for i, tm := range times {
		if tm.Hour() != 9 || tm.Minute() != 0 {
			t.Errorf("preview %d not at 09:00: %v", i, tm)
		}
		if !tm.After(now) {
			t.Errorf("preview %d is not in the future: %v", i, tm)
		}
	}
	if times[1].Sub(times[0]) != 24*time.Hour {
		t.Errorf("daily runs should be a day apart: %v -> %v", times[0], times[1])
	}
}

func TestSchedulePreviewEmptyWhileHalfTyped(t *testing.T) {
	f := newFormModel(t.TempDir())
	f.values["kind"] = "routine"
	f.values["preset"] = "days"
	f.values["days"] = "" // no days chosen yet
	f.values["hours"] = "9"
	f.rebuild()
	if got := f.previewTimes(time.Now(), 3); got != nil {
		t.Errorf("preview should be empty for an unparseable schedule, got %v", got)
	}
}

func TestHumanRunsCollapsesSameDay(t *testing.T) {
	now := time.Date(2026, 1, 1, 8, 0, 0, 0, time.Local)
	times := []time.Time{
		time.Date(2026, 1, 1, 15, 0, 0, 0, time.Local),
		time.Date(2026, 1, 1, 16, 0, 0, 0, time.Local),
		time.Date(2026, 1, 2, 9, 0, 0, 0, time.Local),
	}
	got := humanRuns(times, now)
	if !strings.HasPrefix(got, "today 15:00, then 16:00, ") {
		t.Errorf("same-day times not collapsed: %q", got)
	}
	if !strings.Contains(got, "tomorrow 09:00") {
		t.Errorf("next-day time not labelled: %q", got)
	}
}
