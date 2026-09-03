//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gatedHeartbeat(t *testing.T, gate string) *Action {
	t.Helper()
	a := &Action{Name: "inbox-triage", Kind: KindHeartbeat, Directory: t.TempDir(), Prompt: "x", Gate: gate}
	a.applyDefaults()
	if err := a.validate(); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGateValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(a *Action)
		want string // "" means valid
	}{
		{"gate on a script", func(a *Action) { a.Kind = KindScript; a.Command = "true"; a.Prompt = ""; a.Gate = "./gate.sh" }, "gate only applies to agent actions"},
		{"gate timeout on a script", func(a *Action) { a.Kind = KindScript; a.Command = "true"; a.Prompt = ""; a.GateTimeoutMinutes = 5 }, "gate only applies to agent actions"},
		{"timeout without a gate", func(a *Action) { a.GateTimeoutMinutes = 5 }, "gate_timeout_minutes needs a gate"},
		{"timeout too large", func(a *Action) { a.Gate = "./gate.sh"; a.GateTimeoutMinutes = 1441 }, "gate_timeout_minutes must be 1-1440"},
		{"timeout negative", func(a *Action) { a.Gate = "./gate.sh"; a.GateTimeoutMinutes = -1 }, "gate_timeout_minutes must be 1-1440"},
		{"whitespace gate is absent", func(a *Action) { a.Gate = "   " }, ""},
		{"gate with the default timeout", func(a *Action) { a.Gate = "./gate.sh" }, ""},
	}
	for _, c := range cases {
		a := &Action{Name: "n", Kind: KindHeartbeat, Directory: "/tmp", Prompt: "x"}
		c.mut(a)
		a.applyDefaults()
		err := a.validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
	a := &Action{Name: "n", Kind: KindHeartbeat, Directory: "/tmp", Prompt: "x", Gate: " ./gate.sh "}
	a.applyDefaults()
	if a.Gate != "./gate.sh" || a.GateTimeoutMinutes != defaultGateTimeoutMinutes {
		t.Errorf("defaults: gate=%q timeout=%d", a.Gate, a.GateTimeoutMinutes)
	}
	b := &Action{Name: "n", Kind: KindHeartbeat, Directory: "/tmp", Prompt: "x", Gate: "  "}
	b.applyDefaults()
	if b.Gate != "" || b.GateTimeoutMinutes != 0 {
		t.Errorf("a blank gate must stay absent: gate=%q timeout=%d", b.Gate, b.GateTimeoutMinutes)
	}
}

func TestLoadActionsAcceptsGateKeys(t *testing.T) {
	dir := t.TempDir()
	writeAction(t, dir, "triage.toml", `
name = "inbox-triage"
kind = "heartbeat"
directory = "~/projects/support"
prompt = "Triage the inbox."
cli = "pi"
gate = "./check-inbox.sh"
gate_timeout_minutes = 3

[heartbeat]
interval_minutes = 15
`)
	actions, fileErrs, err := LoadActions(dir)
	if err != nil || len(fileErrs) > 0 || len(actions) != 1 {
		t.Fatalf("err=%v fileErrs=%v actions=%d", err, fileErrs, len(actions))
	}
	if actions[0].Gate != "./check-inbox.sh" || actions[0].GateTimeoutMinutes != 3 {
		t.Errorf("gate keys not loaded: %+v", actions[0])
	}
}

func TestRunGateVerdicts(t *testing.T) {
	cases := []struct {
		command string
		want    gateVerdict
		detail  string
	}{
		{"echo nothing due; exit 75", gateSkip, "nothing due"},
		{"echo go", gateRun, "go"},
		{"echo broke >&2; exit 3", gateFailed, "gate failed (exit status 3): broke"},
	}
	for _, c := range cases {
		a := gatedHeartbeat(t, c.command)
		verdict, detail := runGate(a, triggerSchedule)
		if verdict != c.want || detail != c.detail {
			t.Errorf("%q: got %v %q, want %v %q", c.command, verdict, detail, c.want, c.detail)
		}
	}
}

func TestRunGateSeesActionAndTrigger(t *testing.T) {
	a := gatedHeartbeat(t, `echo "$SHEPHERD_ACTION/$SHEPHERD_TRIGGER"; exit 75`)
	_, detail := runGate(a, triggerWake)
	if detail != "inbox-triage/wake" {
		t.Errorf("the gate must see the action and trigger, got %q", detail)
	}
}

func TestRunGateTimeoutKillsTheGroupAndFails(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "survived")
	a := &Action{Name: "inbox-triage", Kind: KindHeartbeat, Directory: dir, Prompt: "x",
		Gate: "(sleep 2; touch " + marker + ") & sleep 60"}
	a.applyDefaults()
	a.GateTimeoutMinutes = 0 // a zero timeout makes the kill immediate
	start := time.Now()
	verdict, detail := runGate(a, triggerSchedule)
	if verdict != gateFailed || !strings.Contains(detail, "timed out") {
		t.Fatalf("got %v %q", verdict, detail)
	}
	if time.Since(start) > waitDelay {
		t.Errorf("a killed gate should return promptly, took %s", time.Since(start))
	}
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the whole process group should have been killed")
	}
}

func TestFireSkipsWhenTheGateSays(t *testing.T) {
	d := testDaemon(t)
	fake := d.client.(*scriptedHerdr)
	a := gatedHeartbeat(t, "echo quiet, nothing to do; exit 75")
	before := time.Now()
	d.fire(a, time.Time{}, "", true)
	if got := d.state.lastStatus(a.Name); got != "skipped" {
		t.Fatalf("got status %q", got)
	}
	recs := readRunLog(t, d.paths.RunLogFile())
	if len(recs) != 1 || recs[0].Status != "skipped" || recs[0].Detail != "quiet, nothing to do" {
		t.Fatalf("expected one skipped record with the gate's output, got %+v", recs)
	}
	if recs[0].DurationSecs <= 0 {
		t.Errorf("a skipped occurrence still records its duration, got %v", recs[0].DurationSecs)
	}
	if last := d.state.lastRun(a.Name); last.Before(before) {
		t.Errorf("a skipped heartbeat must be stamped like a completed one, got %v", last)
	}
	if _, commands, notices, _ := fake.counts(); commands != 0 || notices != 0 {
		t.Errorf("a skip opens no workspace and notifies nobody: commands=%d notices=%d", commands, notices)
	}
	if fake.env != nil {
		t.Error("a skip must not create a workspace")
	}
	if runLockHeld(d.paths.StateDir, a.Name, time.Hour) {
		t.Error("the run lock must be released after a skip")
	}
}

func TestFireRunsTheAgentWhenTheGateFails(t *testing.T) {
	d := testDaemon(t)
	d.startTimeout = 10 * time.Millisecond
	fake := d.client.(*scriptedHerdr)
	a := gatedHeartbeat(t, "echo gate broke; exit 1")
	d.fire(a, time.Time{}, "", true)
	if _, commands, notices, _ := fake.counts(); commands == 0 || notices == 0 {
		t.Fatalf("a failed gate must notify and still start the agent: commands=%d notices=%d", commands, notices)
	}
	if !strings.Contains(fake.notices[0], "gate failed") {
		t.Errorf("the first notice names the gate, got %v", fake.notices)
	}
	for _, r := range readRunLog(t, d.paths.RunLogFile()) {
		if r.Status == "skipped" {
			t.Error("a failed gate is never a skip")
		}
	}
}

func TestFireRunsTheAgentWhenTheGatePasses(t *testing.T) {
	d := testDaemon(t)
	d.startTimeout = 10 * time.Millisecond
	fake := d.client.(*scriptedHerdr)
	a := gatedHeartbeat(t, "exit 0")
	if err := requestWake(d.paths.StateDir, a.Name, "test"); err != nil {
		t.Fatal(err)
	}
	d.fire(a, time.Time{}, triggerWake, false)
	if _, commands, _, _ := fake.counts(); commands != 1 {
		t.Fatalf("a passing gate starts the agent once, got %d commands", commands)
	}
	if fake.env["SHEPHERD_TRIGGER"] != triggerWake {
		t.Errorf("the pane still learns its trigger, got %v", fake.env)
	}
	for _, r := range readRunLog(t, d.paths.RunLogFile()) {
		if r.Status == "skipped" {
			t.Error("a passing gate leaves no skipped record")
		}
	}
}
