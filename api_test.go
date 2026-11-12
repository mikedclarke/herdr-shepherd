package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const apiHeartbeatTOML = `name = "inbox-triage"
kind = "heartbeat"
directory = "~/projects/support"
prompt = "Triage the support inbox per OPS.md. Exit when done."
permission_mode = "auto"        # default | auto | skip

[heartbeat]
interval_minutes = 60

[heartbeat.working_hours]       # optional; omit to run around the clock
days = [1, 2, 3, 4, 5]          # 0=Sunday .. 6=Saturday; empty = any day
start_hour = 9                  # start > end spans midnight
end_hour = 17
`

const apiRoutineTOML = `name = "morning-digest"
kind = "routine"
directory = "~/projects/ops"
prompt = "Prepare the morning digest per DIGEST.md."

[schedule]
preset = "weekdays"             # daily | weekdays | days | monthly | cron
hours = [6]
minute = 15
`

const apiScriptTOML = `name = "repo-sync"
kind = "script"
directory = "~/projects"
command = "./sync.sh"
timeout_minutes = 30

[schedule]
preset = "cron"
cron = "0 6 * * 1-5"            # 5-field cron: minute hour day-of-month month day-of-week
`

func apiFixture(t *testing.T) paths {
	t.Helper()
	p := paths{ConfigDir: t.TempDir(), StateDir: t.TempDir(), Backend: backendCore}
	os.MkdirAll(p.ActionsDir(), 0o755)
	writeFile(t, p.ActionsDir(), "inbox-triage.toml", apiHeartbeatTOML)
	writeFile(t, p.ActionsDir(), "morning-digest.toml", apiRoutineTOML)
	writeFile(t, p.ActionsDir(), "repo-sync.toml", apiScriptTOML)
	return p
}

// apiEnv points the verbs' own path resolution at the fixture.
func apiEnv(t *testing.T, p paths) {
	t.Helper()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", p.ConfigDir)
	t.Setenv("HERDR_PLUGIN_STATE_DIR", p.StateDir)
	t.Setenv(backendEnv, backendCore)
}

func runVerb(t *testing.T, p paths, stdin string, verb string, args ...string) (map[string]any, int) {
	t.Helper()
	apiEnv(t, p)
	var out, errOut bytes.Buffer
	code := runAPIVerb(&out, &errOut, strings.NewReader(stdin), verb, args)
	if strings.Count(strings.TrimSpace(out.String()), "\n") != 0 {
		t.Fatalf("%s: stdout is not one JSON line: %q", verb, out.String())
	}
	var body map[string]any
	if err := json.Unmarshal(out.Bytes(), &body); err != nil {
		t.Fatalf("%s: stdout is not JSON: %v\n%s", verb, err, out.String())
	}
	return body, code
}

func TestAPIFieldsBuildFromForm(t *testing.T) {
	fields, err := buildAPIFields()
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]apiField{}
	for _, f := range fields {
		if _, dup := byKey[f.Key]; dup {
			t.Fatalf("duplicate field %q", f.Key)
		}
		byKey[f.Key] = f
		switch f.Type {
		case "text", "multiline", "choice", "bool", "number":
		default:
			t.Errorf("%s: type %q is not in the contract", f.Key, f.Type)
		}
		if f.Type == "choice" && len(f.Options) == 0 {
			t.Errorf("%s: a choice with no options", f.Key)
		}
		if f.Type != "choice" && len(f.Options) > 0 {
			t.Errorf("%s: options on a %s field", f.Key, f.Type)
		}
		if len(f.Kinds) == 0 || f.Label == "" || f.Help == "" {
			t.Errorf("%s: kinds, label and help are all required: %+v", f.Key, f)
		}
	}
	// Every field the board's form can show, under any kind, preset or cli,
	// is either described or deliberately left out.
	for _, kind := range []string{"routine", "heartbeat", "script"} {
		for _, preset := range []string{"daily", "weekdays", "days", "monthly", "cron"} {
			for _, cli := range []string{"claude", "codex", "pi"} {
				f := apiForm("")
				f.values["kind"], f.values["preset"], f.values["cli"] = kind, preset, cli
				f.rebuild()
				for _, fd := range f.fields {
					_, described := byKey[fd.key]
					_, omitted := apiFieldOmitted[fd.key]
					if !described && !omitted {
						t.Errorf("form field %q (kind %s, preset %s, cli %s) has no fields entry", fd.key, kind, preset, cli)
					}
				}
			}
		}
	}
	if got := byKey["thinking"].ShowWhen["cli"]; len(got) != 1 || got[0] != "pi" {
		t.Errorf("thinking show_when = %v, want cli [pi]", byKey["thinking"].ShowWhen)
	}
	if got := byKey["cron"].ShowWhen["preset"]; len(got) != 1 || got[0] != "cron" {
		t.Errorf("cron show_when = %v", byKey["cron"].ShowWhen)
	}
	if byKey["prompt"].Type != "multiline" || byKey["wh_days"].Type != "choice" || !byKey["wh_days"].Multiple {
		t.Errorf("prompt or wh_days typed wrong: %+v %+v", byKey["prompt"], byKey["wh_days"])
	}
	if d := byKey["watch_minutes"].Default; d != 240 {
		t.Errorf("watch_minutes default = %v, want 240", d)
	}
}

func TestAPIFieldsRefusesUndescribedField(t *testing.T) {
	saved := apiFieldMeta["cron"]
	delete(apiFieldMeta, "cron")
	defer func() { apiFieldMeta["cron"] = saved }()
	if _, err := buildAPIFields(); err == nil || !strings.Contains(err.Error(), `"cron"`) {
		t.Fatalf("err = %v, want a refusal naming cron", err)
	}
}

func TestAPIFieldsVerb(t *testing.T) {
	body, code := runVerb(t, apiFixture(t), "", "fields", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, body)
	}
	kinds, _ := body["kinds"].([]any)
	if len(kinds) != 3 || kinds[0] != "heartbeat" || kinds[2] != "script" {
		t.Errorf("kinds = %v", body["kinds"])
	}
	if fs, _ := body["fields"].([]any); len(fs) < 20 {
		t.Errorf("fields = %d entries", len(fs))
	}
}

func TestAPIList(t *testing.T) {
	p := apiFixture(t)
	writeFile(t, p.ActionsDir(), "broken.toml", "name = \n")
	if err := appendRunLog(p.RunLogFile(), runRecord{
		At: time.Date(2026, 11, 10, 6, 0, 3, 0, time.UTC), Action: "repo-sync", Kind: KindScript,
		Status: "completed", Detail: "ok", DurationSecs: 2.5,
	}); err != nil {
		t.Fatal(err)
	}
	body, code := runVerb(t, p, "", "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, body)
	}
	if body["version"] != version || body["backend"] != "core" || body["config_dir"] != p.ConfigDir {
		t.Errorf("header = %v", body)
	}
	if d := body["daemon"].(map[string]any); d["running"] != false || d["pid"] != nil {
		t.Errorf("daemon = %v", d)
	}
	actions := body["actions"].([]any)
	var names []string
	for _, a := range actions {
		names = append(names, a.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "broken,inbox-triage,morning-digest,repo-sync" {
		t.Fatalf("names = %v", names)
	}
	broken := actions[0].(map[string]any)
	if broken["error"] == nil || broken["kind"] != nil || broken["enabled"] != nil || broken["running"] != nil {
		t.Errorf("broken = %v", broken)
	}
	if !strings.HasSuffix(broken["file"].(string), "broken.toml") {
		t.Errorf("broken file = %v", broken["file"])
	}
	sync := actions[3].(map[string]any)
	if sync["schedule"] != "0 6 * * 1-5" || sync["enabled"] != true || sync["running"] != false || sync["error"] != nil {
		t.Errorf("repo-sync = %v", sync)
	}
	last := sync["last_run"].(map[string]any)
	if last["at"] != "2026-11-10T06:00:03Z" || last["status"] != "completed" || last["trigger"] != "schedule" || last["duration_secs"] != 2.5 {
		t.Errorf("last_run = %v", last)
	}
	if _, err := time.Parse(time.RFC3339, sync["next_run"].(string)); err != nil {
		t.Errorf("next_run %v: %v", sync["next_run"], err)
	}
	if actions[2].(map[string]any)["last_run"] != nil {
		t.Errorf("morning-digest has no runs, got %v", actions[2])
	}
}

func TestAPIListEmptyIsAnArray(t *testing.T) {
	p := paths{ConfigDir: t.TempDir(), StateDir: t.TempDir(), Backend: backendCore}
	res, err := apiListActions(p)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(res)
	if !strings.Contains(string(data), `"actions":[]`) {
		t.Errorf("empty list = %s", data)
	}
}

func TestAPIGet(t *testing.T) {
	p := apiFixture(t)
	body, code := runVerb(t, p, "", "get", "inbox-triage", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, body)
	}
	if body["toml"] != apiHeartbeatTOML {
		t.Errorf("toml = %q", body["toml"])
	}
	values := body["values"].(map[string]any)
	fields, _ := apiFieldList()
	for _, f := range fields {
		_, has := values[f.Key]
		if want := indexOfKind(f.Kinds, KindHeartbeat) >= 0; has != want {
			t.Errorf("values has %q = %v, want %v", f.Key, has, want)
		}
	}
	if values["interval_minutes"] != 60.0 || values["wh_days"] != "1,2,3,4,5" || values["start_hour"] != 9.0 ||
		values["permission_mode"] != "auto" || values["auto_close"] != false {
		t.Errorf("values = %v", values)
	}
	if _, ok := values["enabled"]; ok {
		t.Error("values carries enabled, which apply refuses")
	}

	writeFile(t, p.ActionsDir(), "broken.toml", "name = \n")
	body, code = runVerb(t, p, "", "get", "broken", "--json")
	if code != 0 || body["values"] != nil || body["toml"] != "name = \n" || body["action"].(map[string]any)["error"] == nil {
		t.Errorf("broken get: exit %d %v", code, body)
	}
	body, code = runVerb(t, p, "", "get", "nope", "--json")
	if code != 1 || body["error"] == nil {
		t.Errorf("missing get: exit %d %v", code, body)
	}
}

func TestAPIGetValuesRoundTripThroughApply(t *testing.T) {
	p := apiFixture(t)
	normal := func(a *Action) Action {
		c := *a
		c.SourceFile, c.Enabled, c.Schedule = "", nil, nil
		return c
	}
	before, _ := loadActionSet(p)
	for _, name := range []string{"inbox-triage", "morning-digest", "repo-sync"} {
		got, err := apiGet(p, name)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := json.Marshal(map[string]any{"name": name, "previous_name": name, "values": got.Values})
		if _, err := apiApply(p, bytes.NewReader(req)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		after, _ := loadActionSet(p)
		a, b := before.find(name), after.find(name)
		if b == nil || !reflect.DeepEqual(normal(a), normal(b)) || a.IsEnabled() != b.IsEnabled() {
			t.Errorf("%s changed in a get/apply round trip:\n%+v\n%+v", name, a, b)
		}
	}
}

func applyJSON(t *testing.T, p paths, req map[string]any) (map[string]any, int) {
	t.Helper()
	data, _ := json.Marshal(req)
	return runVerb(t, p, string(data), "apply", "--json")
}

func TestAPIApplyCreate(t *testing.T) {
	p := apiFixture(t)
	body, code := applyJSON(t, p, map[string]any{
		"name": "weekly-report",
		"values": map[string]any{
			"kind": "routine", "directory": "~/reports", "prompt": "Write the weekly report.\nKeep it short.",
			"preset": "days", "days": []any{1, 4}, "hours": "8", "minute": 30,
		},
	})
	if code != 0 {
		t.Fatalf("exit %d: %v", code, body)
	}
	a := body["action"].(map[string]any)
	if a["name"] != "weekly-report" || a["enabled"] != false || a["schedule"] != "Mon,Thu 08:30" {
		t.Errorf("action = %v", a)
	}
	text, err := os.ReadFile(filepath.Join(p.ActionsDir(), "weekly-report.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "enabled = false") || !strings.Contains(string(text), `prompt = "Write the weekly report.\nKeep it short."`) {
		t.Errorf("file = %s", text)
	}
}

func TestAPIApplyCreateNeverOverwrites(t *testing.T) {
	p := apiFixture(t)
	writeFile(t, p.ActionsDir(), "elsewhere.toml", strings.Replace(apiScriptTOML, `"repo-sync"`, `"other-name"`, 1))
	for _, name := range []string{"repo-sync", "other-name"} {
		body, code := applyJSON(t, p, map[string]any{
			"name": name, "values": map[string]any{"kind": "script", "command": "true"},
		})
		fields, _ := body["fields"].(map[string]any)
		if code != 1 || fields["name"] == nil {
			t.Errorf("%s: exit %d %v", name, code, body)
		}
	}
	// A file of that name the loader refuses is still never replaced.
	writeFile(t, p.ActionsDir(), "half.toml", "name = \n")
	body, code := applyJSON(t, p, map[string]any{
		"name": "half", "values": map[string]any{"kind": "script", "command": "true"},
	})
	if code != 1 || !strings.Contains(body["error"].(string), "already exists") {
		t.Errorf("half: exit %d %v", code, body)
	}
	if got, _ := os.ReadFile(filepath.Join(p.ActionsDir(), "repo-sync.toml")); string(got) != apiScriptTOML {
		t.Errorf("repo-sync changed: %s", got)
	}
}

func TestAPIApplyEditKeepsEnabledAndCarriedKeys(t *testing.T) {
	p := apiFixture(t)
	writeFile(t, p.ActionsDir(), "morning-digest.toml", `name = "morning-digest"
kind = "routine"
directory = "~/projects/ops"
enabled = true
prompt = "old"
append_system_prompt = "be brief"
gate = "./has-mail.sh"
gate_timeout_minutes = 5

[env]
AGENT_HOME = "~/agent"

[schedule]
preset = "weekdays"
hours = [6]
minute = 15
`)
	body, code := applyJSON(t, p, map[string]any{
		"name": "morning-digest", "previous_name": "morning-digest",
		"values": map[string]any{"prompt": "new", "minute": "45"},
	})
	if code != 0 {
		t.Fatalf("exit %d: %v", code, body)
	}
	set, _ := loadActionSet(p)
	a := set.find("morning-digest")
	if a == nil || !a.IsEnabled() || a.Prompt != "new" || a.Routine.Minute != 45 ||
		a.AppendSystemPrompt != "be brief" || a.Gate != "./has-mail.sh" || a.GateTimeoutMinutes != 5 ||
		a.Env["AGENT_HOME"] != "~/agent" {
		t.Errorf("after edit: %+v", a)
	}
}

func TestAPIApplyRename(t *testing.T) {
	p := apiFixture(t)
	body, code := applyJSON(t, p, map[string]any{
		"name": "repo-sync-2", "previous_name": "repo-sync", "values": map[string]any{},
	})
	if code != 0 {
		t.Fatalf("exit %d: %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(p.ActionsDir(), "repo-sync.toml")); !os.IsNotExist(err) {
		t.Errorf("old file still there: %v", err)
	}
	set, _ := loadActionSet(p)
	if a := set.find("repo-sync-2"); a == nil || filepath.Base(a.SourceFile) != "repo-sync-2.toml" || a.Command != "./sync.sh" {
		t.Errorf("renamed = %+v", a)
	}

	body, code = applyJSON(t, p, map[string]any{
		"name": "inbox-triage", "previous_name": "repo-sync-2", "values": map[string]any{},
	})
	if code != 1 || body["fields"].(map[string]any)["name"] == nil {
		t.Errorf("rename onto an existing action: exit %d %v", code, body)
	}
	body, code = applyJSON(t, p, map[string]any{"name": "x", "previous_name": "gone", "values": map[string]any{}})
	if code != 1 || body["error"] == nil {
		t.Errorf("edit of a missing action: exit %d %v", code, body)
	}
}

func TestAPIApplyValidationErrors(t *testing.T) {
	agent := map[string]any{"kind": "routine", "prompt": "go"}
	heartbeat := map[string]any{"kind": "heartbeat", "prompt": "go"}
	script := map[string]any{"kind": "script", "command": "true"}
	with := func(base map[string]any, kv ...any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	cases := []struct {
		label  string
		name   string
		values map[string]any
		field  string
	}{
		{"blank name", "", agent, "name"},
		{"name with a space", "a b", agent, "name"},
		{"name with a slash", "a/b", agent, "name"},
		{"name with a leading dot", ".hidden", agent, "name"},
		{"values.name differs", "a", with(agent, "name", "b"), "name"},
		{"unknown kind", "a", with(agent, "kind", "cronjob"), "kind"},
		{"unknown field", "a", with(agent, "colour", "red"), "colour"},
		{"enabled", "a", with(agent, "enabled", true), "enabled"},
		{"blank prompt", "a", with(agent, "prompt", " "), "prompt"},
		{"blank directory", "a", with(agent, "directory", ""), "directory"},
		{"unknown cli", "a", with(agent, "cli", "gpt"), "cli"},
		{"pi with skip", "a", with(agent, "cli", "pi", "permission_mode", "skip"), "permission_mode"},
		{"watch too long", "a", with(agent, "watch_minutes", 5000), "watch_minutes"},
		{"watch not a number", "a", with(agent, "watch_minutes", "soon"), "watch_minutes"},
		{"watch a fraction", "a", with(agent, "watch_minutes", 1.5), "watch_minutes"},
		{"auto_close not a bool", "a", with(agent, "auto_close", "yes"), "auto_close"},
		{"control character", "a", with(agent, "prompt", "go\x01"), "prompt"},
		{"blank command", "a", with(script, "command", ""), "command"},
		{"timeout too long", "a", with(script, "timeout_minutes", 2000), "timeout_minutes"},
		{"timeout not a number", "a", with(script, "timeout_minutes", "x"), "timeout_minutes"},
		{"defer too long", "a", with(script, "defer_retry_minutes", 2000), "defer_retry_minutes"},
		{"negative interval", "a", with(heartbeat, "interval_minutes", -1), "interval_minutes"},
		{"from hour without to hour", "a", with(heartbeat, "start_hour", 9), "end_hour"},
		{"to hour without from hour", "a", with(heartbeat, "end_hour", 9), "start_hour"},
		{"days without hours", "a", with(heartbeat, "wh_days", "1,2"), "start_hour"},
		{"equal hours", "a", with(heartbeat, "start_hour", 9, "end_hour", 9), "end_hour"},
		{"from hour out of range", "a", with(heartbeat, "start_hour", 30, "end_hour", 9), "start_hour"},
		{"to hour out of range", "a", with(heartbeat, "start_hour", 9, "end_hour", 30), "end_hour"},
		{"unknown day", "a", with(heartbeat, "wh_days", "1,9", "start_hour", 9, "end_hour", 17), "wh_days"},
		{"unknown preset", "a", with(agent, "preset", "yearly"), "preset"},
		{"bad cron", "a", with(agent, "preset", "cron", "cron", "every day"), "cron"},
		{"impossible cron", "a", with(agent, "preset", "cron", "cron", "0 0 30 2 *"), "cron"},
		{"minute out of range", "a", with(agent, "minute", 70), "minute"},
		{"hour out of range", "a", with(agent, "hours", "25"), "hours"},
		{"hours not numbers", "a", with(agent, "hours", "nine"), "hours"},
		{"no hours", "a", with(agent, "hours", ""), "hours"},
		{"days preset with no days", "a", with(agent, "preset", "days", "days", ""), "days"},
		{"month day out of range", "a", with(agent, "preset", "monthly", "month_day", 30), "month_day"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			p := apiFixture(t)
			body, code := applyJSON(t, p, map[string]any{"name": c.name, "values": c.values})
			fields, _ := body["fields"].(map[string]any)
			if code != 1 || fields[c.field] == nil || body["error"] == "" {
				t.Errorf("exit %d, body %v; want a %q field error", code, body, c.field)
			}
			if _, err := os.Stat(filepath.Join(p.ActionsDir(), "a.toml")); err == nil {
				t.Error("a refused apply wrote a file")
			}
		})
	}
}

func TestAPIApplyAppendSystemPromptNeedsCLI(t *testing.T) {
	p := apiFixture(t)
	writeFile(t, p.ActionsDir(), "morning-digest.toml", strings.Replace(apiRoutineTOML,
		"prompt = ", "append_system_prompt = \"x\"\nprompt = ", 1))
	body, code := applyJSON(t, p, map[string]any{
		"name": "morning-digest", "previous_name": "morning-digest", "values": map[string]any{"cli": "codex"},
	})
	if code != 1 || body["fields"].(map[string]any)["cli"] == nil {
		t.Errorf("exit %d %v", code, body)
	}
}

func TestAPIApplyBadStdin(t *testing.T) {
	p := apiFixture(t)
	for _, in := range []string{"", "{", `{"name": "a", "colour": 1}`} {
		body, code := runVerb(t, p, in, "apply")
		if code != 1 || body["error"] == nil {
			t.Errorf("%q: exit %d %v", in, code, body)
		}
	}
}

func TestAPIPutTOML(t *testing.T) {
	p := apiFixture(t)
	path := filepath.Join(p.ActionsDir(), "repo-sync.toml")
	for _, bad := range []string{
		"name = \"repo-sync\"\nkind = \n",
		strings.Replace(apiScriptTOML, "timeout_minutes = 30", "timeout_minutes = 30\ncolour = 1", 1),
		strings.Replace(apiScriptTOML, `cron = "0 6 * * 1-5"`, `cron = "0 0 30 2 *"`, 1),
		strings.Replace(apiScriptTOML, `"repo-sync"`, `"another"`, 1),
	} {
		body, code := runVerb(t, p, bad, "put-toml", "repo-sync")
		if code != 1 || body["error"] == nil {
			t.Errorf("accepted %q: %v", bad, body)
		}
		if got, _ := os.ReadFile(path); string(got) != apiScriptTOML {
			t.Fatalf("a refused put-toml changed the file: %s", got)
		}
	}
	good := "# synced every weekday\n" + strings.Replace(apiScriptTOML, "./sync.sh", "./sync.sh --quiet", 1)
	body, code := runVerb(t, p, good, "put-toml", "repo-sync")
	if code != 0 || body["action"].(map[string]any)["name"] != "repo-sync" {
		t.Fatalf("exit %d %v", code, body)
	}
	if got, _ := os.ReadFile(path); string(got) != good {
		t.Errorf("file = %s", got)
	}
	entries, _ := os.ReadDir(p.ActionsDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestAPIPutTOMLFixesABrokenFile(t *testing.T) {
	p := apiFixture(t)
	writeFile(t, p.ActionsDir(), "later.toml", "name = \n")
	fixed := strings.Replace(apiScriptTOML, `"repo-sync"`, `"later"`, 1)
	body, code := runVerb(t, p, fixed, "put-toml", "later", "--json")
	if code != 0 || body["action"].(map[string]any)["error"] != nil {
		t.Fatalf("exit %d %v", code, body)
	}
	body, code = runVerb(t, p, fixed, "put-toml", "missing")
	if code != 1 || body["error"] == nil {
		t.Errorf("put-toml created an action: %v", body)
	}
}

func TestAPIEnableDisableKeepComments(t *testing.T) {
	p := apiFixture(t)
	path := filepath.Join(p.ActionsDir(), "inbox-triage.toml")
	body, code := runVerb(t, p, "", "disable", "inbox-triage", "--json")
	if code != 0 || body["action"].(map[string]any)["enabled"] != false || body["action"].(map[string]any)["next_run"] != nil {
		t.Fatalf("disable: exit %d %v", code, body)
	}
	got, _ := os.ReadFile(path)
	if want := strings.Replace(apiHeartbeatTOML, "kind = ", "enabled = false\nkind = ", 1); string(got) != want {
		t.Errorf("after disable:\n%s", got)
	}
	body, code = runVerb(t, p, "", "enable", "inbox-triage", "--json")
	if code != 0 || body["action"].(map[string]any)["enabled"] != true {
		t.Fatalf("enable: exit %d %v", code, body)
	}
	got, _ = os.ReadFile(path)
	if !strings.Contains(string(got), "# optional; omit to run around the clock") || !strings.Contains(string(got), "enabled = true") {
		t.Errorf("after enable:\n%s", got)
	}

	writeFile(t, p.ActionsDir(), "broken.toml", "name = \n")
	if body, code := runVerb(t, p, "", "enable", "broken", "--json"); code != 1 || !strings.Contains(body["error"].(string), "put-toml") {
		t.Errorf("enable of a broken file: exit %d %v", code, body)
	}
}

func TestAPIRuns(t *testing.T) {
	p := apiFixture(t)
	base := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	os.WriteFile(p.RunLogFile()+".1", []byte(`{"at":"2026-10-31T06:00:00Z","action":"repo-sync","kind":"script","status":"completed","duration_secs":1}`+"\n"), 0o644)
	for i := 0; i < 5; i++ {
		name, trigger := "repo-sync", ""
		if i%2 == 1 {
			name, trigger = "morning-digest", triggerManual
		}
		appendRunLog(p.RunLogFile(), runRecord{At: base.Add(time.Duration(i) * time.Hour), Action: name, Kind: KindScript, Status: "completed", Trigger: trigger})
	}
	f, _ := os.OpenFile(p.RunLogFile(), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("{half a reco\n")
	f.Close()

	body, code := runVerb(t, p, "", "runs", "--json")
	runs := body["runs"].([]any)
	if code != 0 || len(runs) != 6 {
		t.Fatalf("exit %d, %d runs", code, len(runs))
	}
	first := runs[0].(map[string]any)
	if first["at"] != "2026-11-01T10:00:00Z" || first["trigger"] != "schedule" || first["duration_secs"] != nil {
		t.Errorf("newest = %v", first)
	}
	if last := runs[5].(map[string]any); last["at"] != "2026-10-31T06:00:00Z" || last["duration_secs"] != 1.0 {
		t.Errorf("oldest = %v", last)
	}
	body, _ = runVerb(t, p, "", "runs", "--json", "--action", "morning-digest")
	if runs := body["runs"].([]any); len(runs) != 2 || runs[0].(map[string]any)["trigger"] != "manual" {
		t.Errorf("filtered = %v", runs)
	}
	body, _ = runVerb(t, p, "", "runs", "--json", "--limit=2")
	if runs := body["runs"].([]any); len(runs) != 2 {
		t.Errorf("limited = %d", len(runs))
	}
	if body, code := runVerb(t, p, "", "runs", "--json", "--limit", "0"); code != 1 || body["error"] == nil {
		t.Errorf("limit 0: exit %d %v", code, body)
	}
}

func TestAPIRunsEmpty(t *testing.T) {
	body, code := runVerb(t, apiFixture(t), "", "runs", "--json")
	if code != 0 || len(body["runs"].([]any)) != 0 {
		t.Errorf("exit %d %v", code, body)
	}
}

func TestAPIRunScriptJSON(t *testing.T) {
	p := apiFixture(t)
	dir := t.TempDir()
	writeFile(t, p.ActionsDir(), "say-hi.toml", `name = "say-hi"
kind = "script"
directory = "`+dir+`"
command = "echo hi"
`)
	body, code := runVerb(t, p, "", "run", "say-hi", "--json")
	if code != 0 || body["started"] != true || body["name"] != "say-hi" || body["status"] != "completed" {
		t.Fatalf("exit %d %v", code, body)
	}
	writeFile(t, p.ActionsDir(), "fail.toml", `name = "fail"
kind = "script"
directory = "`+dir+`"
command = "exit 3"
`)
	body, code = runVerb(t, p, "", "run", "--json", "fail")
	if code != 0 || body["status"] != "error" {
		t.Errorf("failing script: exit %d %v", code, body)
	}
	body, code = runVerb(t, p, "", "run", "nope", "--json")
	if code != 1 || body["error"] == nil {
		t.Errorf("unknown action: exit %d %v", code, body)
	}
}

func TestAPIVerbArgs(t *testing.T) {
	p := apiFixture(t)
	cases := [][]string{
		{"get", "--json"},
		{"get", "a", "b", "--json"},
		{"enable", "../escape", "--json"},
		{"disable", ".hidden", "--json"},
		{"list", "--json", "--bogus"},
		{"runs", "--json", "--action"},
	}
	for _, c := range cases {
		body, code := runVerb(t, p, "", c[0], c[1:]...)
		if code != 1 || body["error"] == nil {
			t.Errorf("%v: exit %d %v", c, code, body)
		}
	}
}

func TestAPIHumanOutput(t *testing.T) {
	p := apiFixture(t)
	apiEnv(t, p)
	var out, errOut bytes.Buffer
	if code := runAPIVerb(&out, &errOut, strings.NewReader(""), "get", []string{"repo-sync"}); code != 0 || out.String() != apiScriptTOML {
		t.Errorf("get: %d %q", code, out.String())
	}
	out.Reset()
	if code := runAPIVerb(&out, &errOut, strings.NewReader(""), "disable", []string{"repo-sync"}); code != 0 || out.String() != "disabled repo-sync\n" {
		t.Errorf("disable: %d %q", code, out.String())
	}
	out.Reset()
	if code := runAPIVerb(&out, &errOut, strings.NewReader(""), "get", []string{"nope"}); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "no action named") {
		t.Errorf("get nope: %d %q %q", code, out.String(), errOut.String())
	}
}

func TestNameErrorMatchesLoader(t *testing.T) {
	for _, n := range []string{"ok-name", "a.b", "x_1"} {
		if err := nameError(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"", "a b", "a/b", `a\b`, ".x", "a\tb"} {
		if err := nameError(n); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestAPIErrorShape(t *testing.T) {
	var out bytes.Buffer
	err := error(fieldErr("name", "bad"))
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatal("not an apiError")
	}
	writeJSON(&out, map[string]any{"error": ae.Error(), "fields": ae.fields})
	if strings.TrimSpace(out.String()) != `{"error":"bad","fields":{"name":"bad"}}` {
		t.Errorf("shape = %s", out.String())
	}
}
