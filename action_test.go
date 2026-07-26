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

[schedule]
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
	actions, fileErrs, err := LoadActions(dir)
	if err != nil || len(fileErrs) > 0 {
		t.Fatalf("err=%v fileErrs=%v", err, fileErrs)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions", len(actions))
	}
	digest := actions[0]
	if digest.Name != "pm-digest" || digest.AutoClose || digest.CLI != "claude" {
		t.Errorf("unexpected digest action: %+v", digest)
	}
	if digest.Routine.Preset != "weekdays" || digest.Routine.Minute != 15 {
		t.Errorf("[schedule] table should populate the routine spec: %+v", digest.Routine)
	}
	if !strings.HasSuffix(digest.Dir(), "/work/pm") || strings.Contains(digest.Dir(), "~") {
		t.Errorf("Dir should expand ~: %s", digest.Dir())
	}
}

func TestLoadActionsMissingDirIsEmpty(t *testing.T) {
	actions, fileErrs, err := LoadActions(filepath.Join(t.TempDir(), "nope"))
	if err != nil || actions != nil || fileErrs != nil {
		t.Errorf("got %v, %v, %v", actions, fileErrs, err)
	}
}

func TestLoadActionsRejectsInvalidPerFile(t *testing.T) {
	cases := map[string]string{
		"no prompt":      "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\n",
		"bad kind":       "name = \"a\"\nkind = \"cronjob\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n",
		"bad cli":        "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\ncli = \"gpt\"\n",
		"bad permission": "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\npermission_mode = \"yolo\"\n",
		"no directory":   "name = \"a\"\nkind = \"heartbeat\"\nprompt = \"x\"\n",
		"bad cron":       "name = \"a\"\nkind = \"routine\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[schedule]\npreset = \"cron\"\ncron = \"nope\"\n",
		"impossible":     "name = \"a\"\nkind = \"routine\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[schedule]\npreset = \"cron\"\ncron = \"0 0 30 2 *\"\n",
		"unknown key":    "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\npermision_mode = \"skip\"\n",
		"bad minute":     "name = \"a\"\nkind = \"routine\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[schedule]\nminute = 90\n",
		"bad month_day":  "name = \"a\"\nkind = \"routine\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[schedule]\npreset = \"monthly\"\nmonth_day = 31\n",
		"bad hour":       "name = \"a\"\nkind = \"routine\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[schedule]\nhours = [25]\n",
	}
	for label, content := range cases {
		dir := t.TempDir()
		writeAction(t, dir, "a.toml", content)
		actions, fileErrs, err := LoadActions(dir)
		if err != nil {
			t.Fatalf("%s: unexpected dir error %v", label, err)
		}
		if len(fileErrs) != 1 || len(actions) != 0 {
			t.Errorf("%s: expected one file error and no actions, got errs=%v actions=%d", label, fileErrs, len(actions))
		}
	}
}

func TestLoadActionsBrokenFileDoesNotDisableOthers(t *testing.T) {
	dir := t.TempDir()
	writeAction(t, dir, "good.toml", "name = \"ok\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\n")
	writeAction(t, dir, "zz-broken.toml", "name = \"broken\nkind=")
	actions, fileErrs, err := LoadActions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Name != "ok" {
		t.Errorf("valid action should survive a broken sibling: %v", actions)
	}
	if len(fileErrs) != 1 {
		t.Errorf("expected one file error, got %v", fileErrs)
	}
}

func TestLoadActionsRejectsDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	base := "name = \"dup\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\n"
	writeAction(t, dir, "a.toml", base)
	writeAction(t, dir, "b.toml", base)
	actions, fileErrs, err := LoadActions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || len(fileErrs) != 1 || !strings.Contains(fileErrs[0].Error(), "duplicate") {
		t.Errorf("expected first kept + duplicate error, got actions=%d errs=%v", len(actions), fileErrs)
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
