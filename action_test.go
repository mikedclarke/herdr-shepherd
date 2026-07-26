package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAction(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadActions(t *testing.T) {
	dir := t.TempDir()
	writeAction(t, dir, "digest.toml", `
name = "pm-digest"
kind = "routine"
directory = "~/work/pm"
prompt = "Run the morning digest"
permission_mode = "auto"
auto_close = false

[routine]
preset = "weekdays"
hours = [6]
minute = 15
`)
	writeAction(t, dir, "sync.toml", `
name = "git-sync"
kind = "script"
directory = "~/work"
command = "./git-sync.sh"

[routine]
preset = "weekdays"
hours = [6]
`)
	actions, err := LoadActions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions", len(actions))
	}
	digest := actions[0]
	if digest.Name != "pm-digest" || digest.AutoCloses() || digest.CLI != "claude" {
		t.Errorf("unexpected digest action: %+v", digest)
	}
	if !strings.HasSuffix(digest.Dir(), "/work/pm") || strings.Contains(digest.Dir(), "~") {
		t.Errorf("Dir should expand ~: %s", digest.Dir())
	}
}

func TestLoadActionsMissingDirIsEmpty(t *testing.T) {
	actions, err := LoadActions(filepath.Join(t.TempDir(), "nope"))
	if err != nil || actions != nil {
		t.Errorf("got %v, %v", actions, err)
	}
}

func TestLoadActionsRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"no prompt":      "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\n",
		"bad kind":       "name = \"a\"\nkind = \"cronjob\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n",
		"bad cli":        "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\ncli = \"gpt\"\n",
		"bad permission": "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\npermission_mode = \"yolo\"\n",
		"no directory":   "name = \"a\"\nkind = \"heartbeat\"\nprompt = \"x\"\n",
		"bad cron":       "name = \"a\"\nkind = \"routine\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[routine]\npreset = \"cron\"\ncron = \"nope\"\n",
	}
	for label, content := range cases {
		dir := t.TempDir()
		writeAction(t, dir, "a.toml", content)
		if _, err := LoadActions(dir); err == nil {
			t.Errorf("%s: expected error", label)
		}
	}
}

func TestLoadActionsRejectsDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	base := "name = \"dup\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\n"
	writeAction(t, dir, "a.toml", base)
	writeAction(t, dir, "b.toml", base)
	if _, err := LoadActions(dir); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got %v", err)
	}
}

func TestAgentCommand(t *testing.T) {
	a := &Action{Kind: KindHeartbeat, CLI: "claude", PermissionMode: "auto", Prompt: "check the queue", Model: "opus"}
	got, err := a.AgentCommand()
	if err != nil {
		t.Fatal(err)
	}
	if want := "claude --permission-mode auto --model 'opus' 'check the queue'"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	a = &Action{Kind: KindRoutine, CLI: "codex", PermissionMode: "skip", Prompt: "it's 9 o'clock"}
	got, err = a.AgentCommand()
	if err != nil {
		t.Fatal(err)
	}
	if want := `codex --dangerously-bypass-approvals-and-sandbox 'it'\''s 9 o'\''clock'`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	a = &Action{Kind: KindHeartbeat, CLI: "claude", PermissionMode: "default", Prompt: "hi"}
	if got, _ = a.AgentCommand(); got != "claude 'hi'" {
		t.Errorf("default mode should add no flags: %q", got)
	}
}
