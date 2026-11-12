package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode"
)

// The machine-readable verbs: list, get, fields, apply, put-toml, enable,
// disable, runs and run, for front ends that should never read the shepherd's
// files themselves. With --json each prints exactly one JSON object on stdout;
// a failure prints {"error", "fields"} and exits 1. apply and put-toml read
// stdin and always answer in JSON.

type apiError struct {
	msg    string
	fields map[string]string
}

func (e *apiError) Error() string { return e.msg }

func fieldErr(key, msg string) *apiError {
	return &apiError{msg: msg, fields: map[string]string{key: msg}}
}

type apiLastRun struct {
	At           string   `json:"at"`
	Status       string   `json:"status"`
	Detail       string   `json:"detail"`
	DurationSecs *float64 `json:"duration_secs"`
	Trigger      string   `json:"trigger"`
}

type apiAction struct {
	Name      string      `json:"name"`
	Kind      *Kind       `json:"kind"`
	Enabled   *bool       `json:"enabled"`
	Schedule  *string     `json:"schedule"`
	Directory *string     `json:"directory"`
	LastRun   *apiLastRun `json:"last_run"`
	NextRun   *string     `json:"next_run"`
	Running   *bool       `json:"running"`
	File      string      `json:"file"`
	Error     *string     `json:"error"`
}

type apiDaemon struct {
	Running bool `json:"running"`
	Pid     *int `json:"pid"`
}

type apiList struct {
	Version   string      `json:"version"`
	Backend   string      `json:"backend"`
	Daemon    apiDaemon   `json:"daemon"`
	ConfigDir string      `json:"config_dir"`
	StateDir  string      `json:"state_dir"`
	Actions   []apiAction `json:"actions"`
}

type apiRun struct {
	At           string   `json:"at"`
	Action       string   `json:"action"`
	Kind         Kind     `json:"kind"`
	Status       string   `json:"status"`
	Detail       string   `json:"detail"`
	Trigger      string   `json:"trigger"`
	DurationSecs *float64 `json:"duration_secs"`
}

type apiOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// apiField is one field of the guided form as a client draws it. A choice
// with multiple set takes several options at once; its value is the chosen
// option values joined by commas, the way the file's day lists are typed.
type apiField struct {
	Key      string              `json:"key"`
	Label    string              `json:"label"`
	Help     string              `json:"help"`
	Type     string              `json:"type"`
	Multiple bool                `json:"multiple,omitempty"`
	Options  []apiOption         `json:"options,omitempty"`
	Kinds    []Kind              `json:"kinds"`
	Required bool                `json:"required"`
	Default  any                 `json:"default"`
	ShowWhen map[string][]string `json:"show_when"`
}

type apiFields struct {
	Kinds  []Kind     `json:"kinds"`
	Fields []apiField `json:"fields"`
}

// fieldMeta is what the board's form leaves to the screen: help worded for a
// client rather than for the TUI's keys, whether a value is required, and a
// type where the form's own does not say it (a prompt is long text; the
// working-hours bounds are numbers the form keeps as text so they can be
// blank; the model is a free id whose presets differ by cli).
type fieldMeta struct {
	typ      string
	required bool
	help     string
}

var apiFieldMeta = map[string]fieldMeta{
	"name":                {"", true, "Unique, with no spaces, path separators or leading dot; also the file name."},
	"kind":                {"", true, "routine: an agent session on a schedule. heartbeat: an agent session every N minutes. script: a shell command on a schedule."},
	"directory":           {"", true, "Where the session or command runs; ~ and $VARS expand."},
	"prompt":              {"multiline", true, "What the agent session should do."},
	"cli":                 {"", true, "The agent CLI that runs the prompt."},
	"model":               {"text", false, "Passed as --model; blank uses the CLI's default."},
	"thinking":            {"", false, "pi's --thinking level; default leaves the model's own."},
	"permission_mode":     {"", true, "skip runs with no permission prompts, unattended. pi accepts only default."},
	"auto_close":          {"", false, "Close the run's tab when the run completes."},
	"watch_minutes":       {"", true, "How long the daemon watches a session before flagging it, up to 1440."},
	"command":             {"", true, "The shell command to run."},
	"timeout_minutes":     {"", true, "The command is stopped after this many minutes, up to 1440."},
	"defer_retry_minutes": {"", true, "A command that exits 75 is retried for this many minutes; 0 records the deferral and stops."},
	"interval_minutes":    {"", true, "Minutes between runs, measured from the last run."},
	"wh_days":             {"", false, "Run only on these days; none means every day. Needs from hour and to hour."},
	"start_hour":          {"number", false, "Working hours start, 0-23. Blank with to hour blank means any time of day."},
	"end_hour":            {"number", false, "Working hours end, 1-24; must differ from from hour. Lower than from hour spans midnight."},
	"preset":              {"", true, "daily, weekdays, chosen days, monthly, or a cron expression."},
	"hours":               {"", true, "Comma-separated hours, 0-23."},
	"minute":              {"", true, "Minute past the hour, 0-59."},
	"days":                {"", true, "The days of the week it runs."},
	"month_day":           {"", true, "Day of the month, 1-28."},
	"cron":                {"", true, "Five fields: minute hour day-of-month month day-of-week."},
}

// apiFieldOmitted are form fields a client never edits through apply.
var apiFieldOmitted = map[string]string{
	"enabled":  "set with enable and disable; a new action starts disabled",
	"provider": "a filter for the board's pi model picker, never written to the file",
}

var apiKinds = []Kind{KindHeartbeat, KindRoutine, KindScript}

// apiForm is the board's form with pi's model catalog marked as fetched, so
// building a field list never runs pi.
func apiForm(dir string) *formModel {
	f := newFormModel(dir)
	f.piDiscovered = true
	f.piModels = nil
	return f
}

var (
	fieldsOnce sync.Once
	fieldsList []apiField
	fieldsErr  error
)

func apiFieldList() ([]apiField, error) {
	fieldsOnce.Do(func() { fieldsList, fieldsErr = buildAPIFields() })
	return fieldsList, fieldsErr
}

// buildAPIFields walks the board's form through every kind, schedule preset
// and cli, and describes each field it shows: which kinds show it, and which
// other values it depends on. The description is checked against the walk, so
// a form change that show_when cannot express fails here rather than drawing
// a different form in a client.
func buildAPIFields() ([]apiField, error) {
	kinds := []string{"routine", "heartbeat", "script"}
	presets := []string{"daily", "weekdays", "days", "monthly", "cron"}
	clis := []string{"claude", "codex", "pi"}
	type combo struct{ kind, preset, cli string }
	var combos []combo
	visible := map[combo]map[string]bool{}
	var order []string
	seen := map[string]formField{}
	ftypes := map[string]map[fieldType]bool{}
	var optionOrder = map[string][]apiOption{}

	defaults := apiForm("").values
	for _, k := range kinds {
		for _, p := range presets {
			for _, c := range clis {
				f := apiForm("")
				f.values["kind"], f.values["preset"], f.values["cli"] = k, p, c
				f.rebuild()
				cb := combo{k, p, c}
				combos = append(combos, cb)
				visible[cb] = map[string]bool{}
				for i, fd := range f.fields {
					visible[cb][fd.key] = true
					if _, ok := seen[fd.key]; !ok {
						seen[fd.key] = fd
						at := 0
						if i > 0 {
							at = indexOf(order, f.fields[i-1].key) + 1
						}
						order = append(order[:at], append([]string{fd.key}, order[at:]...)...)
					}
					if ftypes[fd.key] == nil {
						ftypes[fd.key] = map[fieldType]bool{}
					}
					ftypes[fd.key][fd.ftype] = true
					for _, o := range formOptions(fd) {
						if !hasOption(optionOrder[fd.key], o.Value) {
							optionOrder[fd.key] = append(optionOrder[fd.key], o)
						}
					}
				}
			}
		}
	}

	var out []apiField
	for _, key := range order {
		if _, skip := apiFieldOmitted[key]; skip {
			continue
		}
		meta, ok := apiFieldMeta[key]
		if !ok {
			return nil, fmt.Errorf("form field %q has no entry in apiFieldMeta", key)
		}
		fd := seen[key]
		typ, multiple := meta.typ, false
		if typ == "" {
			if len(ftypes[key]) > 1 {
				return nil, fmt.Errorf("form field %q changes type with other values; give it a type in apiFieldMeta", key)
			}
			typ, multiple = apiType(fd.ftype)
		}
		af := apiField{
			Key: key, Label: fd.label, Help: meta.help, Type: typ, Multiple: multiple,
			Required: meta.required,
		}
		if typ == "choice" {
			af.Options = optionOrder[key]
		}
		kindSet := map[string]bool{}
		presetSet := map[string]bool{}
		cliSet := map[string]bool{}
		for _, cb := range combos {
			if visible[cb][key] {
				kindSet[cb.kind], presetSet[cb.preset], cliSet[cb.cli] = true, true, true
			}
		}
		for _, k := range apiKinds {
			if kindSet[string(k)] {
				af.Kinds = append(af.Kinds, k)
			}
		}
		// A dimension is a condition only where it matters: the preset for the
		// scheduled kinds, the cli for the agent kinds.
		cond := map[string][]string{}
		if len(presetSet) < len(presets) {
			cond["preset"] = inOrder(presets, presetSet)
		}
		if len(cliSet) < len(clis) {
			cond["cli"] = inOrder(clis, cliSet)
		}
		if len(cond) > 0 {
			af.ShowWhen = cond
		}
		for _, cb := range combos {
			want := visible[cb][key]
			got := kindSet[cb.kind] &&
				(cond["preset"] == nil || indexOf(cond["preset"], cb.preset) >= 0) &&
				(cond["cli"] == nil || indexOf(cond["cli"], cb.cli) >= 0)
			if want != got {
				return nil, fmt.Errorf("form field %q: visibility for kind %s, preset %s, cli %s is not expressible as kinds and show_when", key, cb.kind, cb.preset, cb.cli)
			}
		}
		af.Default = typedValue(af, defaults[key])
		out = append(out, af)
	}
	for key := range apiFieldMeta {
		if _, ok := seen[key]; !ok {
			return nil, fmt.Errorf("apiFieldMeta names %q, which the form does not show", key)
		}
	}
	return out, nil
}

func apiType(t fieldType) (string, bool) {
	switch t {
	case ftInt:
		return "number", false
	case ftBool:
		return "bool", false
	case ftEnum, ftChoice:
		return "choice", false
	case ftDays:
		return "choice", true
	default:
		return "text", false
	}
}

func formOptions(fd formField) []apiOption {
	var out []apiOption
	switch fd.ftype {
	case ftEnum:
		for _, o := range fd.options {
			out = append(out, apiOption{o, o})
		}
	case ftChoice:
		for _, c := range fd.choices {
			out = append(out, apiOption{c.value, c.label})
		}
	case ftDays:
		for _, d := range dayOrder {
			out = append(out, apiOption{strconv.Itoa(d), dayLabels[d]})
		}
	}
	return out
}

func hasOption(opts []apiOption, v string) bool {
	for _, o := range opts {
		if o.Value == v {
			return true
		}
	}
	return false
}

func indexOf(xs []string, x string) int {
	for i, s := range xs {
		if s == x {
			return i
		}
	}
	return -1
}

func inOrder(all []string, set map[string]bool) []string {
	var out []string
	for _, s := range all {
		if set[s] {
			out = append(out, s)
		}
	}
	return out
}

// typedValue turns a form value (always a string in the form) into the JSON
// value for its field type: a number or null when blank, a bool, or a string.
func typedValue(f apiField, s string) any {
	switch f.Type {
	case "bool":
		return s == "true"
	case "number":
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return nil
		}
		return n
	default:
		return s
	}
}

// formString is typedValue's inverse for apply's input: the string the form
// would hold for a JSON value.
func formString(f apiField, raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("not a JSON value")
	}
	if v == nil {
		return "", nil
	}
	switch f.Type {
	case "bool":
		switch b := v.(type) {
		case bool:
			return strconv.FormatBool(b), nil
		case string:
			if b == "true" || b == "false" {
				return b, nil
			}
		}
		return "", fmt.Errorf("must be true or false")
	case "number":
		switch n := v.(type) {
		case json.Number:
			if _, err := strconv.Atoi(n.String()); err != nil {
				return "", fmt.Errorf("must be a whole number")
			}
			return n.String(), nil
		case string:
			return strings.TrimSpace(n), nil
		}
		return "", fmt.Errorf("must be a number")
	case "choice":
		if f.Multiple {
			if list, ok := v.([]any); ok {
				parts := make([]string, 0, len(list))
				for _, item := range list {
					s := fmt.Sprint(item)
					if !hasOption(f.Options, s) {
						return "", fmt.Errorf("%q is not one of the options", s)
					}
					parts = append(parts, s)
				}
				return strings.Join(parts, ","), nil
			}
			s, ok := v.(string)
			if !ok {
				return "", fmt.Errorf("must be a list of option values or a comma-separated string")
			}
			for _, part := range strings.Split(s, ",") {
				if p := strings.TrimSpace(part); p != "" && !hasOption(f.Options, p) {
					return "", fmt.Errorf("%q is not one of the options", p)
				}
			}
			return s, nil
		}
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("must be a string")
		}
		if !hasOption(f.Options, s) {
			return "", fmt.Errorf("%q is not one of the options", s)
		}
		return s, nil
	default:
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("must be a string")
		}
		// The file is written with Go string quoting, whose \x and \a escapes
		// TOML does not read: a control character would leave a broken file.
		for _, r := range s {
			if unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' {
				return "", fmt.Errorf("must not contain control characters")
			}
		}
		return s, nil
	}
}

// nameError applies the loader's name rule, by validating an action that
// differs from a valid one only in its name.
func nameError(name string) error {
	a := &Action{Name: name, Kind: KindScript, Command: "true", Directory: "~"}
	a.applyDefaults()
	return a.validate()
}

func fmtRFC3339(t time.Time) string { return t.Format(time.RFC3339) }

func optSecs(s float64) *float64 {
	if s <= 0 {
		return nil
	}
	return &s
}

func triggerName(t string) string {
	if t == "" {
		return "schedule"
	}
	return t
}

// actionSet is the actions directory as the loader sees it: the actions that
// load, and the files that do not, keyed by file name.
type actionSet struct {
	actions []*Action
	broken  map[string]string
}

func loadActionSet(p paths) (actionSet, error) {
	actions, fileErrs, err := LoadActions(p.ActionsDir())
	if err != nil {
		return actionSet{}, err
	}
	set := actionSet{actions: actions, broken: map[string]string{}}
	for _, ferr := range fileErrs {
		file, text, _ := strings.Cut(ferr.Error(), ":")
		set.broken[strings.TrimSpace(file)] = strings.TrimSpace(text)
	}
	return set, nil
}

func (s actionSet) find(name string) *Action {
	for _, a := range s.actions {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// brokenFile names the file <name>.toml when it is one the loader refused.
func (s actionSet) brokenFile(name string) (string, bool) {
	msg, ok := s.broken[name+".toml"]
	return msg, ok
}

func notFound(s actionSet, name string) error {
	if msg, ok := s.brokenFile(name); ok {
		return fmt.Errorf("%s.toml does not load (%s); fix it with put-toml", name, msg)
	}
	return fmt.Errorf("no action named %q", name)
}

// runHistory gives the newest run record for each action, from the run log's
// tail and, for actions the tail does not reach, the rotated log's.
type runHistory struct {
	current, rotated []runRecord
	rotatedRead      bool
	path             string
}

func (h *runHistory) newest(name string) (runRecord, bool) {
	if r, ok := newestFor(h.current, name); ok {
		return r, true
	}
	if !h.rotatedRead {
		h.rotated, h.rotatedRead = readRunTail(h.path+".1"), true
	}
	return newestFor(h.rotated, name)
}

func newestFor(recs []runRecord, name string) (runRecord, bool) {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Action == name {
			return recs[i], true
		}
	}
	return runRecord{}, false
}

func describeAction(p paths, a *Action, st *daemonState, hist *runHistory, now time.Time) apiAction {
	kind := a.Kind
	enabled := a.IsEnabled()
	sched := scheduleSummary(a)
	dir := a.Directory
	running := runLockHeld(p.StateDir, a.Name, runPidMaxAge(a))
	out := apiAction{
		Name: a.Name, Kind: &kind, Enabled: &enabled, Schedule: &sched, Directory: &dir,
		Running: &running, File: a.SourceFile,
	}
	if rec, ok := hist.newest(a.Name); ok {
		out.LastRun = &apiLastRun{
			At: fmtRFC3339(rec.At), Status: rec.Status, Detail: rec.Detail,
			DurationSecs: optSecs(rec.DurationSecs), Trigger: triggerName(rec.Trigger),
		}
	} else if last := st.lastRun(a.Name); !last.IsZero() {
		out.LastRun = &apiLastRun{At: fmtRFC3339(last), Status: st.lastStatus(a.Name), Trigger: "schedule"}
	}
	if next := nextRun(a, st.lastRun(a.Name), now); !next.IsZero() {
		s := fmtRFC3339(next)
		out.NextRun = &s
	}
	return out
}

func describeBroken(p paths, file, msg string) apiAction {
	m := msg
	return apiAction{
		Name: strings.TrimSuffix(file, ".toml"), File: filepath.Join(p.ActionsDir(), file), Error: &m,
	}
}

func newRunHistory(p paths) *runHistory {
	return &runHistory{current: readRunTail(p.RunLogFile()), path: p.RunLogFile()}
}

func apiDescribe(p paths, name string) (apiAction, error) {
	set, err := loadActionSet(p)
	if err != nil {
		return apiAction{}, err
	}
	if a := set.find(name); a != nil {
		return describeAction(p, a, readState(p.StateFile()), newRunHistory(p), time.Now()), nil
	}
	if msg, ok := set.brokenFile(name); ok {
		return describeBroken(p, name+".toml", msg), nil
	}
	return apiAction{}, notFound(set, name)
}

func apiListActions(p paths) (apiList, error) {
	set, err := loadActionSet(p)
	if err != nil {
		return apiList{}, err
	}
	st := readState(p.StateFile())
	hist := newRunHistory(p)
	now := time.Now()
	out := apiList{
		Version: version, Backend: p.Backend, ConfigDir: p.ConfigDir, StateDir: p.StateDir,
		Daemon: daemonInfo(p, st), Actions: []apiAction{},
	}
	for _, a := range set.actions {
		out.Actions = append(out.Actions, describeAction(p, a, st, hist, now))
	}
	for file, msg := range set.broken {
		out.Actions = append(out.Actions, describeBroken(p, file, msg))
	}
	sort.SliceStable(out.Actions, func(i, j int) bool {
		if out.Actions[i].Name != out.Actions[j].Name {
			return out.Actions[i].Name < out.Actions[j].Name
		}
		return out.Actions[i].File < out.Actions[j].File
	})
	return out, nil
}

// daemonInfo uses status's liveness rule, a fresh heartbeat in the state
// file, and reads the pid the daemon wrote into its lock without taking the
// lock: a probe could make a daemon starting that instant refuse to start.
func daemonInfo(p paths, st *daemonState) apiDaemon {
	beat := st.heartbeatAt()
	d := apiDaemon{Running: !beat.IsZero() && time.Since(beat) < 3*tickInterval}
	if !d.Running {
		return d
	}
	if data, err := os.ReadFile(p.LockFile()); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pidAlive(pid) {
			d.Pid = &pid
		}
	}
	return d
}

type apiGetResult struct {
	Action apiAction      `json:"action"`
	Values map[string]any `json:"values"`
	TOML   string         `json:"toml"`
}

func apiGet(p paths, name string) (apiGetResult, error) {
	if err := nameError(name); err != nil {
		return apiGetResult{}, err
	}
	set, err := loadActionSet(p)
	if err != nil {
		return apiGetResult{}, err
	}
	if a := set.find(name); a != nil {
		text, err := os.ReadFile(a.SourceFile)
		if err != nil {
			return apiGetResult{}, err
		}
		values, err := formValues(a)
		if err != nil {
			return apiGetResult{}, err
		}
		return apiGetResult{
			Action: describeAction(p, a, readState(p.StateFile()), newRunHistory(p), time.Now()),
			Values: values, TOML: string(text),
		}, nil
	}
	if msg, ok := set.brokenFile(name); ok {
		file := name + ".toml"
		text, err := os.ReadFile(filepath.Join(p.ActionsDir(), file))
		if err != nil {
			return apiGetResult{}, err
		}
		return apiGetResult{Action: describeBroken(p, file, msg), TOML: string(text)}, nil
	}
	return apiGetResult{}, notFound(set, name)
}

// formValues is what the board's form holds for an action, for every field of
// the action's kind.
func formValues(a *Action) (map[string]any, error) {
	fields, err := apiFieldList()
	if err != nil {
		return nil, err
	}
	f := apiForm("")
	f.loadAction(a)
	out := map[string]any{}
	for _, fd := range fields {
		if indexOfKind(fd.Kinds, a.Kind) >= 0 {
			out[fd.Key] = typedValue(fd, f.values[fd.Key])
		}
	}
	return out, nil
}

func indexOfKind(ks []Kind, k Kind) int {
	for i, x := range ks {
		if x == k {
			return i
		}
	}
	return -1
}

type applyRequest struct {
	Name         string                     `json:"name"`
	PreviousName string                     `json:"previous_name"`
	Values       map[string]json.RawMessage `json:"values"`
}

type actionResult struct {
	Action apiAction `json:"action"`
}

// apiApply is the board's form save without the screen: the same values, the
// same validation and the same write, with a new action written disabled and
// no existing file ever overwritten by a create or a rename.
func apiApply(p paths, in io.Reader) (actionResult, error) {
	var req applyRequest
	dec := json.NewDecoder(in)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return actionResult{}, fmt.Errorf("stdin: %v", err)
	}
	fields, err := apiFieldList()
	if err != nil {
		return actionResult{}, err
	}
	byKey := map[string]apiField{}
	for _, fd := range fields {
		byKey[fd.Key] = fd
	}
	name := req.Name
	if raw, ok := req.Values["name"]; ok {
		var vn string
		if json.Unmarshal(raw, &vn) != nil {
			return actionResult{}, fieldErr("name", "name must be a string")
		}
		if name == "" {
			name = vn
		} else if vn != name {
			return actionResult{}, fieldErr("name", fmt.Sprintf("values.name %q differs from name %q", vn, name))
		}
	}
	if err := nameError(name); err != nil {
		return actionResult{}, fieldErr("name", err.Error())
	}
	if req.PreviousName != "" {
		if err := nameError(req.PreviousName); err != nil {
			return actionResult{}, fmt.Errorf("previous_name: %v", err)
		}
	}

	set, err := loadActionSet(p)
	if err != nil {
		return actionResult{}, err
	}
	f := apiForm(p.ActionsDir())
	if req.PreviousName != "" {
		a := set.find(req.PreviousName)
		if a == nil {
			return actionResult{}, notFound(set, req.PreviousName)
		}
		f.loadAction(a)
	}
	if name != req.PreviousName {
		if set.find(name) != nil {
			return actionResult{}, fieldErr("name", fmt.Sprintf("an action named %q already exists", name))
		}
	}

	bad := map[string]string{}
	for key, raw := range req.Values {
		if key == "name" {
			continue
		}
		if why, omitted := apiFieldOmitted[key]; omitted {
			bad[key] = fmt.Sprintf("%s is not set by apply: %s", key, why)
			continue
		}
		fd, ok := byKey[key]
		if !ok {
			bad[key] = fmt.Sprintf("unknown field %q", key)
			continue
		}
		s, err := formString(fd, raw)
		if err != nil {
			bad[key] = fmt.Sprintf("%s: %v", fd.Label, err)
			continue
		}
		f.values[key] = s
	}
	if len(bad) > 0 {
		keys := make([]string, 0, len(bad))
		for k := range bad {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return actionResult{}, &apiError{msg: bad[keys[0]], fields: bad}
	}
	f.values["name"] = name

	if _, err := f.buildAction(); err != nil {
		msg := strings.TrimPrefix(err.Error(), name+": ")
		if key := fieldForError(msg, f.values); key != "" {
			return actionResult{}, &apiError{msg: err.Error(), fields: map[string]string{key: msg}}
		}
		return actionResult{}, err
	}
	if err := os.MkdirAll(p.ActionsDir(), 0o755); err != nil {
		return actionResult{}, err
	}
	if _, saved := f.trySave(); !saved {
		if strings.HasSuffix(f.err, "already exists") {
			return actionResult{}, fieldErr("name", f.err)
		}
		return actionResult{}, errors.New(f.err)
	}
	a, err := apiDescribe(p, name)
	if err != nil {
		return actionResult{}, err
	}
	return actionResult{Action: a}, nil
}

// errorFields maps the form's and the loader's validation messages to the
// field a person would change. Order matters: the first match wins.
var errorFields = []struct{ match, key string }{
	{"name is required", "name"},
	{"must not contain", "name"},
	{"must not start with a dot", "name"},
	{"env key", ""},
	{"gate", ""},
	{"append_system_prompt", "cli"},
	{"prompt is required", "prompt"},
	{"cli must be", "cli"},
	{"permission", "permission_mode"},
	{"thinking", "thinking"},
	{"defer", "defer_retry_minutes"},
	{"command is required", "command"},
	{"kind must be", "kind"},
	{"directory is required", "directory"},
	{"timeout", "timeout_minutes"},
	{"watch", "watch_minutes"},
	{"interval_minutes", "interval_minutes"},
	{"every:", "interval_minutes"},
	{"set from/to hours", "start_hour"},
	{"only on days", "wh_days"},
	{"working_hours day", "wh_days"},
	{"from hour", "start_hour"},
	{"to hour", "end_hour"},
	{"must differ", "end_hour"},
	{"preset", "preset"},
	{"cron", "cron"},
	{"month_day", "month_day"},
	{"day of month", "month_day"},
	{"schedule minute", "minute"},
	{"minute:", "minute"},
	{"schedule hour", "hours"},
	{"hours:", "hours"},
	{"schedule day", "days"},
	{"days:", "days"},
}

func fieldForError(msg string, values map[string]string) string {
	if strings.Contains(msg, "working hours: set both") || strings.Contains(msg, "working_hours hours out of range") {
		start := strings.TrimSpace(values["start_hour"])
		if n, err := strconv.Atoi(start); start == "" || err != nil || n < 0 || n > 23 {
			return "start_hour"
		}
		return "end_hour"
	}
	for _, e := range errorFields {
		if strings.Contains(msg, e.match) {
			return e.key
		}
	}
	return ""
}

// apiPutTOML is the board's raw edit: the text replaces the action's file
// whole, comments and all, once it loads exactly as the daemon would load it.
func apiPutTOML(p paths, name string, in io.Reader) (actionResult, error) {
	if err := nameError(name); err != nil {
		return actionResult{}, err
	}
	text, err := io.ReadAll(in)
	if err != nil {
		return actionResult{}, fmt.Errorf("stdin: %v", err)
	}
	set, err := loadActionSet(p)
	if err != nil {
		return actionResult{}, err
	}
	target := ""
	if a := set.find(name); a != nil {
		target = a.SourceFile
	} else if _, ok := set.brokenFile(name); ok {
		target = filepath.Join(p.ActionsDir(), name+".toml")
	} else {
		return actionResult{}, fmt.Errorf("no action named %q", name)
	}

	scratch, err := os.MkdirTemp("", "shepherd-put-*")
	if err != nil {
		return actionResult{}, err
	}
	defer os.RemoveAll(scratch)
	if err := os.WriteFile(filepath.Join(scratch, filepath.Base(target)), text, 0o600); err != nil {
		return actionResult{}, err
	}
	parsed, fileErrs, err := LoadActions(scratch)
	if err != nil {
		return actionResult{}, err
	}
	if len(fileErrs) > 0 {
		_, msg, _ := strings.Cut(fileErrs[0].Error(), ": ")
		return actionResult{}, errors.New(msg)
	}
	if len(parsed) != 1 || parsed[0].Name != name {
		got := ""
		if len(parsed) == 1 {
			got = parsed[0].Name
		}
		return actionResult{}, fmt.Errorf("the text names the action %q, not %q; rename with apply", got, name)
	}

	mode := os.FileMode(0o644)
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode()
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".put-*.tmp")
	if err != nil {
		return actionResult{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(text); err != nil {
		tmp.Close()
		return actionResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return actionResult{}, err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return actionResult{}, err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return actionResult{}, err
	}
	a, err := apiDescribe(p, name)
	if err != nil {
		return actionResult{}, err
	}
	return actionResult{Action: a}, nil
}

// apiSetEnabled is the board's pause and resume.
func apiSetEnabled(p paths, name string, v bool) (actionResult, error) {
	if err := nameError(name); err != nil {
		return actionResult{}, err
	}
	set, err := loadActionSet(p)
	if err != nil {
		return actionResult{}, err
	}
	a := set.find(name)
	if a == nil {
		return actionResult{}, notFound(set, name)
	}
	if err := setActionEnabled(a.SourceFile, v); err != nil {
		return actionResult{}, err
	}
	desc, err := apiDescribe(p, name)
	if err != nil {
		return actionResult{}, err
	}
	return actionResult{Action: desc}, nil
}

const defaultRunsLimit = 50

type runsResult struct {
	Runs []apiRun `json:"runs"`
}

// apiRuns reads the whole run log, the rotated one first, and returns the
// newest records first.
func apiRuns(p paths, action string, limit int) (runsResult, error) {
	if action != "" {
		if err := nameError(action); err != nil {
			return runsResult{}, err
		}
	}
	var recs []runRecord
	for _, path := range []string{p.RunLogFile() + ".1", p.RunLogFile()} {
		got, err := readRunLogAll(path)
		if err != nil {
			return runsResult{}, err
		}
		recs = append(recs, got...)
	}
	out := runsResult{Runs: []apiRun{}}
	for i := len(recs) - 1; i >= 0 && len(out.Runs) < limit; i-- {
		r := recs[i]
		if action != "" && r.Action != action {
			continue
		}
		out.Runs = append(out.Runs, apiRun{
			At: fmtRFC3339(r.At), Action: r.Action, Kind: r.Kind, Status: r.Status, Detail: r.Detail,
			Trigger: triggerName(r.Trigger), DurationSecs: optSecs(r.DurationSecs),
		})
	}
	return out, nil
}

// readRunLogAll parses a whole run log, oldest first; a missing log is empty
// and a line that does not parse (a half-appended record) is skipped.
func readRunLogAll(path string) ([]runRecord, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []runRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var r runRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			recs = append(recs, r)
		}
	}
	return recs, sc.Err()
}

type verbArgs struct {
	json   bool
	action string
	limit  int
	pos    []string
}

func parseVerbArgs(verb string, args []string, withRunFlags bool) (verbArgs, error) {
	va := verbArgs{limit: defaultRunsLimit}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		key, val, hasVal := strings.Cut(arg, "=")
		switch {
		case arg == "--json":
			va.json = true
		case withRunFlags && (key == "--action" || key == "--limit"):
			if !hasVal {
				if i+1 >= len(args) {
					return va, fmt.Errorf("%s: %s needs a value", verb, key)
				}
				i++
				val = args[i]
			}
			if key == "--action" {
				va.action = val
				continue
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return va, fmt.Errorf("%s: --limit must be a whole number of at least 1, got %q", verb, val)
			}
			va.limit = n
		case strings.HasPrefix(arg, "-"):
			return va, fmt.Errorf("%s: unknown flag %q", verb, arg)
		default:
			va.pos = append(va.pos, arg)
		}
	}
	return va, nil
}

func hasJSONFlag(args []string) bool { return indexOf(args, "--json") >= 0 }

// machineVerbs answer in JSON whether or not --json is given: they read
// their input from stdin and have no human form.
var machineVerbs = map[string]bool{"apply": true, "put-toml": true}

// runAPIVerb runs one of the machine-readable verbs and returns the exit
// code. In JSON mode every failure is the error object on stdout; otherwise
// it is one line on stderr, as for the other commands.
func runAPIVerb(stdout, stderr io.Writer, stdin io.Reader, verb string, args []string) int {
	asJSON := hasJSONFlag(args) || machineVerbs[verb]
	result, human, err := apiVerb(stdin, verb, args)
	if err != nil {
		if asJSON {
			body := map[string]any{"error": err.Error()}
			var ae *apiError
			if errors.As(err, &ae) && len(ae.fields) > 0 {
				body["fields"] = ae.fields
			}
			writeJSON(stdout, body)
		} else {
			fmt.Fprintln(stderr, "herdr-shepherd:", err)
		}
		return 1
	}
	if asJSON {
		writeJSON(stdout, result)
	} else {
		io.WriteString(stdout, human())
	}
	return 0
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(w, "{\"error\": %q}\n", err.Error())
	}
}

func apiVerb(stdin io.Reader, verb string, args []string) (any, func() string, error) {
	va, err := parseVerbArgs(verb, args, verb == "runs")
	if err != nil {
		return nil, nil, err
	}
	wantPos := map[string]int{
		"list": 0, "fields": 0, "apply": 0, "runs": 0, "run": 1,
		"get": 1, "put-toml": 1, "enable": 1, "disable": 1,
	}
	if n, ok := wantPos[verb]; !ok {
		return nil, nil, fmt.Errorf("unknown command %q", verb)
	} else if len(va.pos) != n {
		if n == 1 && len(va.pos) == 0 {
			return nil, nil, fmt.Errorf("%s: action name required", verb)
		}
		return nil, nil, fmt.Errorf("%s: unexpected argument %q", verb, va.pos[n])
	}
	if verb == "fields" {
		fields, err := apiFieldList()
		if err != nil {
			return nil, nil, err
		}
		return apiFields{Kinds: apiKinds, Fields: fields}, func() string { return fieldsText(fields) }, nil
	}
	if verb == "run" {
		res, err := runNowJSON(va.pos[0])
		return res, nil, err
	}
	p := resolvePaths()
	switch verb {
	case "list":
		res, err := apiListActions(p)
		return res, nil, err
	case "get":
		res, err := apiGet(p, va.pos[0])
		return res, func() string { return res.TOML }, err
	case "apply":
		res, err := apiApply(p, stdin)
		return res, nil, err
	case "put-toml":
		res, err := apiPutTOML(p, va.pos[0], stdin)
		return res, nil, err
	case "enable", "disable":
		res, err := apiSetEnabled(p, va.pos[0], verb == "enable")
		return res, func() string { return verb + "d " + va.pos[0] + "\n" }, err
	default: // runs
		res, err := apiRuns(p, va.action, va.limit)
		return res, func() string { return runsText(res.Runs) }, err
	}
}

func fieldsText(fields []apiField) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tTYPE\tKINDS\tSHOWN WHEN\tLABEL")
	for _, f := range fields {
		typ := f.Type
		if f.Multiple {
			typ += " (several)"
		}
		kinds := make([]string, len(f.Kinds))
		for i, k := range f.Kinds {
			kinds[i] = string(k)
		}
		var when []string
		for _, k := range []string{"preset", "cli"} {
			if vs := f.ShowWhen[k]; vs != nil {
				when = append(when, k+"="+strings.Join(vs, "|"))
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", f.Key, typ, strings.Join(kinds, ","), strings.Join(when, " "), f.Label)
	}
	w.Flush()
	return b.String()
}

func runsText(runs []apiRun) string {
	if len(runs) == 0 {
		return "No runs recorded.\n"
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "AT\tACTION\tSTATUS\tTRIGGER\tDURATION\tDETAIL")
	for _, r := range runs {
		dur := "-"
		if r.DurationSecs != nil {
			dur = fmtRunDuration(*r.DurationSecs)
		}
		at, _ := time.Parse(time.RFC3339, r.At)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", fmtTime(at, ""), r.Action, r.Status, r.Trigger, dur, truncate(firstLine(r.Detail), 60))
	}
	w.Flush()
	return b.String()
}
