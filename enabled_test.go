package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetActionEnabledTogglesInPlace(t *testing.T) {
	content := `# keep this comment
name = "digest"
kind = "routine"
enabled = true
directory = "~"
prompt = "go"

[schedule]
preset = "weekdays"
hours = [6]
minute = 15
`
	path := writeFile(t, t.TempDir(), "a.toml", content)
	if err := setActionEnabled(path, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := strings.Replace(content, "enabled = true", "enabled = false", 1)
	if string(got) != want {
		t.Errorf("toggle rewrote more than the enabled line:\n%s", got)
	}
	if err := setActionEnabled(path, true); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != content {
		t.Errorf("round-trip did not restore the original file:\n%s", got)
	}
}

func TestSetActionEnabledInsertsAfterName(t *testing.T) {
	content := `name = "sync"
kind = "script"
directory = "~"
command = "true"

[schedule]
preset = "daily"
`
	path := writeFile(t, t.TempDir(), "a.toml", content)
	if err := setActionEnabled(path, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	lines := strings.Split(string(got), "\n")
	if lines[1] != "enabled = false" {
		t.Errorf("expected enabled inserted after name, got line %q", lines[1])
	}
	actions, fileErrs, err := LoadActions(filepath.Dir(path))
	if err != nil || len(fileErrs) > 0 || len(actions) != 1 {
		t.Fatalf("edited file no longer loads: %v %v", err, fileErrs)
	}
	if actions[0].IsEnabled() {
		t.Error("action should be disabled after toggle")
	}
}

func TestSetActionEnabledIgnoresSectionKeys(t *testing.T) {
	// A same-named key inside a table must never be edited; the top-level key
	// is missing here, so the toggle inserts one before the section.
	content := `kind = "script"
name = "odd"
directory = "~"
command = "true"

[schedule]
preset = "daily"
enabled = true
`
	path := writeFile(t, t.TempDir(), "a.toml", content)
	if err := setActionEnabled(path, false); err != nil {
		t.Fatal(err)
	}
	got := string(mustRead(t, path))
	if !strings.Contains(got, "[schedule]\npreset = \"daily\"\nenabled = true") {
		t.Errorf("section-level key was modified:\n%s", got)
	}
	if !strings.Contains(got, "name = \"odd\"\nenabled = false") {
		t.Errorf("top-level enabled not inserted after name:\n%s", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSetActionEnabledPreservesMode(t *testing.T) {
	path := writeFile(t, t.TempDir(), "a.toml", "name = \"x\"\n")
	os.Chmod(path, 0o600)
	if err := setActionEnabled(path, false); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode not preserved: %v", info.Mode())
	}
}

func TestLoadActionsRecordsSourceFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "digest.toml", "name = \"digest\"\nkind = \"script\"\ndirectory = \"~\"\ncommand = \"true\"\n")
	actions, _, err := LoadActions(dir)
	if err != nil || len(actions) != 1 {
		t.Fatalf("load: %v", err)
	}
	if actions[0].SourceFile != path {
		t.Errorf("SourceFile = %q, want %q", actions[0].SourceFile, path)
	}
}
