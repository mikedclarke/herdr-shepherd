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
	writeAction(t, dir, "report.toml", `
name = "nightly-report"
kind = "routine"
directory = "~/reports"
prompt = "Write up yesterday's numbers"
permission_mode = "auto"

[schedule]
preset = "weekdays"
hours = [6]
minute = 15
`)
	writeAction(t, dir, "sync.toml", `
name = "build-sync"
kind = "script"
directory = "~/builds"
command = "./sync.sh"

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
	report := actions[0]
	if report.Name != "nightly-report" || report.AutoClose || report.CLI != "claude" {
		t.Errorf("unexpected action: %+v", report)
	}
	if report.Routine.Preset != "weekdays" || report.Routine.Minute != 15 {
		t.Errorf("[schedule] table should populate the routine spec: %+v", report.Routine)
	}
	if !strings.HasSuffix(report.Dir(), "/reports") || strings.Contains(report.Dir(), "~") {
		t.Errorf("Dir should expand ~: %s", report.Dir())
	}
}

func TestLoadActionsAcceptsPiAndDeferRetry(t *testing.T) {
	dir := t.TempDir()
	writeAction(t, dir, "pulse.toml", `
name = "pulse"
kind = "heartbeat"
directory = "~/local-agent"
prompt = "anything for me to do?"
cli = "pi"

[heartbeat]
interval_minutes = 90
`)
	writeAction(t, dir, "triage.toml", `
name = "email-triage"
kind = "script"
directory = "~/local-agent"
command = "./run.sh email-triage"
defer_retry_minutes = 45
`)
	actions, fileErrs, err := LoadActions(dir)
	if err != nil || len(fileErrs) > 0 || len(actions) != 2 {
		t.Fatalf("err=%v fileErrs=%v actions=%d", err, fileErrs, len(actions))
	}
	if actions[0].CLI != "pi" || actions[0].PermissionMode != "default" {
		t.Errorf("unexpected pi action: %+v", actions[0])
	}
	if actions[1].DeferRetryMinutes != 45 {
		t.Errorf("defer_retry_minutes should load, got %d", actions[1].DeferRetryMinutes)
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
		// pi has no permission flags; auto/skip must fail loudly, not be dropped.
		"pi with auto":   "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\ncli = \"pi\"\npermission_mode = \"auto\"\n",
		"pi with skip":   "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\ncli = \"pi\"\npermission_mode = \"skip\"\n",
		"defer on agent": "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\ndefer_retry_minutes = 10\n",
		"negative defer": "name = \"a\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\ndefer_retry_minutes = -1\n",
		"long defer":     "name = \"a\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\ndefer_retry_minutes = 1441\n",
		"no directory":   "name = \"a\"\nkind = \"heartbeat\"\nprompt = \"x\"\n",
		"bad cron":       "name = \"a\"\nkind = \"routine\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[schedule]\npreset = \"cron\"\ncron = \"nope\"\n",
		"impossible":     "name = \"a\"\nkind = \"routine\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[schedule]\npreset = \"cron\"\ncron = \"0 0 30 2 *\"\n",
		"unknown key":    "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\npermision_mode = \"skip\"\n",
		"bad minute":     "name = \"a\"\nkind = \"routine\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[schedule]\nminute = 90\n",
		"bad month_day":  "name = \"a\"\nkind = \"routine\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[schedule]\npreset = \"monthly\"\nmonth_day = 31\n",
		"bad hour":       "name = \"a\"\nkind = \"routine\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[schedule]\nhours = [25]\n",
		// The name becomes the run lock's file name.
		"name with slash":     "name = \"a/b\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\n",
		"name with backslash": "name = \"a\\\\b\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\n",
		"dotted name":         "name = \".hidden\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\n",
		"long timeout":        "name = \"a\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\ntimeout_minutes = 1441\n",
		"long watch":          "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\nwatch_minutes = 1441\n",
		"empty hour range":    "name = \"a\"\nkind = \"heartbeat\"\ndirectory = \"/tmp\"\nprompt = \"x\"\n[heartbeat.working_hours]\nstart_hour = 9\nend_hour = 9\n",
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

func TestLoadActionsAcceptsALeapDayCron(t *testing.T) {
	// Feb 29 is more than a year away in three years out of four.
	dir := t.TempDir()
	writeAction(t, dir, "leap.toml", "name = \"leap\"\nkind = \"script\"\ndirectory = \"/tmp\"\ncommand = \"true\"\n[schedule]\npreset = \"cron\"\ncron = \"0 9 29 2 *\"\n")
	actions, fileErrs, err := LoadActions(dir)
	if err != nil || len(fileErrs) != 0 || len(actions) != 1 {
		t.Fatalf("err=%v fileErrs=%v actions=%d", err, fileErrs, len(actions))
	}
}

// The flag strings are what actually reaches the pane; `--permission-mode
// auto` is what the installed claude CLI accepts.
func TestAgentCommandFlags(t *testing.T) {
	cases := []struct {
		cli, mode, want string
	}{
		{"claude", "default", "claude 'hi'"},
		{"claude", "auto", "claude --permission-mode auto 'hi'"},
		{"claude", "skip", "claude --dangerously-skip-permissions 'hi'"},
		{"codex", "default", "codex 'hi'"},
		{"codex", "auto", "codex --ask-for-approval on-request --sandbox workspace-write 'hi'"},
		{"codex", "skip", "codex --dangerously-bypass-approvals-and-sandbox 'hi'"},
		{"pi", "default", "pi 'hi'"},
	}
	for _, c := range cases {
		a := &Action{Kind: KindHeartbeat, CLI: c.cli, PermissionMode: c.mode, Prompt: "hi"}
		got, err := a.AgentCommand()
		if err != nil {
			t.Fatalf("%s/%s: %v", c.cli, c.mode, err)
		}
		if got != c.want {
			t.Errorf("%s/%s: got %q, want %q", c.cli, c.mode, got, c.want)
		}
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

	// pi takes the prompt as a bare argument (interactive; -p would be
	// headless) and honours --model like the others.
	a = &Action{Kind: KindHeartbeat, CLI: "pi", PermissionMode: "default", Prompt: "check the queue", Model: "qwen3.8-27b"}
	got, err = a.AgentCommand()
	if err != nil {
		t.Fatal(err)
	}
	if want := "pi --model 'qwen3.8-27b' 'check the queue'"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	a = &Action{Kind: KindHeartbeat, CLI: "pi", PermissionMode: "skip", Prompt: "hi"}
	if _, err := a.AgentCommand(); err == nil {
		t.Error("pi has no permission flags; a non-default mode must be rejected, not silently ignored")
	}
}

func TestContractPath(t *testing.T) {
	t.Setenv("HOME", "/home/shep")
	cases := map[string]string{
		"/home/shep/.config/herdr/actions": "~/.config/herdr/actions",
		"/home/shep":                       "~",
		"/home/shepherd/actions":           "/home/shepherd/actions",
		"/etc/herdr":                       "/etc/herdr",
	}
	for in, want := range cases {
		if got := contractPath(in); got != want {
			t.Errorf("contractPath(%q) = %q, want %q", in, got, want)
		}
	}
}
