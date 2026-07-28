package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// The new-action form: a guided, field-by-field editor so creating an action
// never requires knowing the TOML by heart. Enum fields cycle, ints and text
// edit inline, and the result goes through the same validate() as the daemon
// before anything is written. New actions are created paused — review on the
// board, then resume.

type fieldType int

const (
	ftText fieldType = iota
	ftInt
	ftBool
	ftEnum
)

type formField struct {
	key     string
	label   string
	ftype   fieldType
	options []string // ftEnum
	help    string
}

// Layout rows in the form view (see View): title 0, error/blank 1, fields
// from 2; then a blank row, then Save / Cancel.
const formFieldsTop = 2

type formModel struct {
	dir      string // actions dir the saved file lands in
	editPath string // non-empty when editing an existing action's file
	origName string // the edited action's name at load time (rename detection)
	values   map[string]string
	fields   []formField
	cursor   int // 0..len(fields)-1, then len = Save, len+1 = Cancel
	editing  bool
	editBuf  string
	err      string
}

func newFormModel(actionsDir string) *formModel {
	f := &formModel{
		dir: actionsDir,
		values: map[string]string{
			"name": "", "kind": "routine", "directory": "~", "enabled": "false",
			"prompt": "",
			"cli":    "claude", "model": "", "permission_mode": "default",
			"auto_close": "false", "watch_minutes": "240",
			"command": "", "timeout_minutes": "30",
			"interval_minutes": "30", "wh_days": "", "start_hour": "", "end_hour": "",
			"preset": "weekdays", "hours": "9", "minute": "0",
			"days": "1,2,3,4,5", "month_day": "1", "cron": "0 9 * * *",
		},
	}
	f.rebuild()
	return f
}

// newFormModelForAction opens the form pre-filled with an existing action for
// editing. Saving rewrites the file cleanly (hand-written comments do not
// survive a form edit; the raw-TOML editor remains for that).
func newFormModelForAction(a *Action, actionsDir string) *formModel {
	f := newFormModel(actionsDir)
	f.editPath = a.SourceFile
	f.origName = a.Name
	v := f.values
	v["name"] = a.Name
	v["kind"] = string(a.Kind)
	v["directory"] = a.Directory
	v["enabled"] = fmt.Sprintf("%t", a.IsEnabled())
	v["prompt"] = a.Prompt
	v["cli"] = a.CLI
	v["model"] = a.Model
	v["permission_mode"] = a.PermissionMode
	v["auto_close"] = fmt.Sprintf("%t", a.AutoClose)
	v["watch_minutes"] = strconv.Itoa(a.WatchMinutes)
	v["command"] = a.Command
	v["timeout_minutes"] = strconv.Itoa(a.TimeoutMinutes)
	v["interval_minutes"] = strconv.Itoa(a.Heartbeat.IntervalMinutes)
	if wh := a.Heartbeat.WorkingHours; wh != nil {
		v["wh_days"] = csvInts(wh.Days)
		v["start_hour"] = strconv.Itoa(wh.StartHour)
		v["end_hour"] = strconv.Itoa(wh.EndHour)
	}
	r := a.Routine
	if r.Preset != "" {
		v["preset"] = r.Preset
	}
	if len(r.Hours) > 0 {
		v["hours"] = csvInts(r.Hours)
	}
	v["minute"] = strconv.Itoa(r.Minute)
	if len(r.Days) > 0 {
		v["days"] = csvInts(r.Days)
	}
	if r.MonthDay > 0 {
		v["month_day"] = strconv.Itoa(r.MonthDay)
	}
	if r.Cron != "" {
		v["cron"] = r.Cron
	}
	f.rebuild()
	return f
}

func csvInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// rebuild recomputes the visible field list from the current kind/preset.
func (f *formModel) rebuild() {
	kind := f.values["kind"]
	fields := []formField{
		{key: "name", label: "name", ftype: ftText, help: "unique, no spaces; also the file name"},
		{key: "kind", label: "kind", ftype: ftEnum, options: []string{"routine", "heartbeat", "script"}, help: "routine: agent on a schedule · heartbeat: agent every N minutes · script: command on a schedule"},
		{key: "directory", label: "directory", ftype: ftText, help: "where the session or command runs"},
		{key: "enabled", label: "enabled", ftype: ftBool, help: "paused actions never fire"},
	}
	if kind == "script" {
		fields = append(fields,
			formField{key: "command", label: "command", ftype: ftText, help: "shell command to run"},
			formField{key: "timeout_minutes", label: "timeout (min)", ftype: ftInt},
		)
	} else {
		fields = append(fields,
			formField{key: "prompt", label: "prompt", ftype: ftText, help: "what the agent session should do"},
			formField{key: "cli", label: "cli", ftype: ftEnum, options: []string{"claude", "codex"}},
			formField{key: "model", label: "model", ftype: ftText, help: "optional; blank = the CLI's default"},
			formField{key: "permission_mode", label: "permissions", ftype: ftEnum, options: []string{"default", "auto", "skip"}, help: "skip = no permission prompts, unattended — use with care"},
			formField{key: "auto_close", label: "auto close", ftype: ftBool, help: "close the workspace when the run completes"},
			formField{key: "watch_minutes", label: "watch (min)", ftype: ftInt},
		)
	}
	switch kind {
	case "heartbeat":
		fields = append(fields,
			formField{key: "interval_minutes", label: "every (min)", ftype: ftInt},
			formField{key: "wh_days", label: "only on days", ftype: ftText, help: "0=Sun .. 6=Sat, comma-separated; blank = every day"},
			formField{key: "start_hour", label: "from hour", ftype: ftText, help: "0-23; blank with 'to hour' = any time of day"},
			formField{key: "end_hour", label: "to hour", ftype: ftText, help: "1-24; must differ from 'from hour'"},
		)
	default:
		fields = append(fields,
			formField{key: "preset", label: "schedule", ftype: ftEnum, options: []string{"daily", "weekdays", "days", "monthly", "cron"}},
		)
		switch f.values["preset"] {
		case "cron":
			fields = append(fields, formField{key: "cron", label: "cron expr", ftype: ftText, help: "min hour day month weekday"})
		case "days":
			fields = append(fields,
				formField{key: "days", label: "days", ftype: ftText, help: "0=Sun .. 6=Sat, comma-separated"},
				formField{key: "hours", label: "at hours", ftype: ftText, help: "comma-separated, 0-23"},
				formField{key: "minute", label: "at minute", ftype: ftInt},
			)
		case "monthly":
			fields = append(fields,
				formField{key: "month_day", label: "day of month", ftype: ftInt, help: "1-28"},
				formField{key: "hours", label: "at hours", ftype: ftText},
				formField{key: "minute", label: "at minute", ftype: ftInt},
			)
		default:
			fields = append(fields,
				formField{key: "hours", label: "at hours", ftype: ftText, help: "comma-separated, 0-23"},
				formField{key: "minute", label: "at minute", ftype: ftInt},
			)
		}
	}
	f.fields = fields
	if f.cursor > len(fields)+1 {
		f.cursor = len(fields) + 1
	}
}

func (f *formModel) saveRow() int   { return len(f.fields) }
func (f *formModel) cancelRow() int { return len(f.fields) + 1 }

// update returns (done, saved). done=true closes the form.
func (f *formModel) update(msg tea.Msg) (bool, bool) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return f.press(msg)
	case tea.MouseMsg:
		if msg.Action != tea.MouseActionPress {
			return false, false
		}
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			f.move(-1)
		case tea.MouseButtonWheelDown:
			f.move(1)
		case tea.MouseButtonLeft:
			row := msg.Y - formFieldsTop
			gap := 1 // blank row between fields and Save/Cancel
			switch {
			case row >= 0 && row < len(f.fields):
				if f.editing {
					f.commitEdit()
				}
				f.cursor = row
				f.activate()
			case row == len(f.fields)+gap:
				f.cursor = f.saveRow()
				return f.trySave()
			case row == len(f.fields)+gap+1:
				return true, false
			}
		}
	}
	return false, false
}

func (f *formModel) press(msg tea.KeyMsg) (bool, bool) {
	key := msg.String()
	if f.editing {
		switch key {
		case "enter":
			f.commitEdit()
		case "esc":
			f.editing = false
			f.editBuf = ""
		case "backspace":
			if len(f.editBuf) > 0 {
				r := []rune(f.editBuf)
				f.editBuf = string(r[:len(r)-1])
			}
		default:
			if key == " " {
				f.editBuf += " "
			} else if msg.Type == tea.KeyRunes {
				f.editBuf += string(msg.Runes)
			}
		}
		return false, false
	}
	switch key {
	case "esc", "ctrl+c":
		return true, false
	case "up", "k", "shift+tab":
		f.move(-1)
	case "down", "j", "tab":
		f.move(1)
	case "left":
		f.cycle(-1)
	case "right", " ":
		f.cycle(1)
	case "enter":
		switch f.cursor {
		case f.saveRow():
			return f.trySave()
		case f.cancelRow():
			return true, false
		default:
			f.activate()
		}
	}
	return false, false
}

func (f *formModel) move(delta int) {
	f.cursor += delta
	if f.cursor < 0 {
		f.cursor = 0
	}
	if f.cursor > f.cancelRow() {
		f.cursor = f.cancelRow()
	}
}

// activate starts the right interaction for the selected field: cycle for
// enums/bools, inline editing for text and ints.
func (f *formModel) activate() {
	if f.cursor >= len(f.fields) {
		return
	}
	fd := f.fields[f.cursor]
	switch fd.ftype {
	case ftEnum, ftBool:
		f.cycle(1)
	default:
		f.err = ""
		f.editing = true
		f.editBuf = f.values[fd.key]
	}
}

func (f *formModel) cycle(delta int) {
	if f.cursor >= len(f.fields) {
		return
	}
	fd := f.fields[f.cursor]
	options := fd.options
	if fd.ftype == ftBool {
		options = []string{"false", "true"}
	}
	if len(options) == 0 {
		return
	}
	current := 0
	for i, o := range options {
		if o == f.values[fd.key] {
			current = i
			break
		}
	}
	next := (current + delta + len(options)) % len(options)
	f.values[fd.key] = options[next]
	// A save error names a field; once that field is edited the message is
	// about a value the form no longer holds.
	f.err = ""
	f.rebuild()
}

func (f *formModel) commitEdit() {
	fd := f.fields[f.cursor]
	f.values[fd.key] = strings.TrimSpace(f.editBuf)
	f.err = ""
	f.editing = false
	f.editBuf = ""
}

func (f *formModel) trySave() (bool, bool) {
	a, err := f.buildAction()
	if err != nil {
		f.err = err.Error()
		return false, false
	}
	target := filepath.Join(f.dir, a.Name+".toml")
	if f.editPath == "" || (a.Name != f.origName) {
		// Creating, or renaming onto a new file: never clobber an existing one.
		if _, err := os.Stat(target); err == nil {
			f.err = fmt.Sprintf("%s already exists", filepath.Base(target))
			return false, false
		}
	}
	if f.editPath != "" && a.Name == f.origName {
		target = f.editPath
	}
	if err := writeActionFile(target, a); err != nil {
		f.err = err.Error()
		return false, false
	}
	if f.editPath != "" && target != f.editPath {
		os.Remove(f.editPath)
	}
	return true, true
}

// buildAction assembles an Action from the form values and runs it through
// the same defaults + validation as the daemon's loader.
func (f *formModel) buildAction() (*Action, error) {
	v := f.values
	enabled := v["enabled"] == "true"
	a := &Action{
		Name:      v["name"],
		Kind:      Kind(v["kind"]),
		Directory: v["directory"],
		Enabled:   &enabled,
	}
	intVal := func(key, label string) (int, error) {
		n, err := strconv.Atoi(strings.TrimSpace(v[key]))
		if err != nil {
			return 0, fmt.Errorf("%s: %q is not a number", label, v[key])
		}
		return n, nil
	}
	var err error
	if a.Kind == KindScript {
		a.Command = v["command"]
		if a.TimeoutMinutes, err = intVal("timeout_minutes", "timeout"); err != nil {
			return nil, err
		}
	} else {
		a.Prompt = v["prompt"]
		a.CLI = v["cli"]
		a.Model = v["model"]
		a.PermissionMode = v["permission_mode"]
		a.AutoClose = v["auto_close"] == "true"
		if a.WatchMinutes, err = intVal("watch_minutes", "watch"); err != nil {
			return nil, err
		}
	}
	if a.Kind == KindHeartbeat {
		if a.Heartbeat.IntervalMinutes, err = intVal("interval_minutes", "every"); err != nil {
			return nil, err
		}
		if a.Heartbeat.WorkingHours, err = f.workingHours(); err != nil {
			return nil, err
		}
	} else {
		r := RoutineSpec{Preset: v["preset"]}
		switch r.Preset {
		case "cron":
			r.Cron = v["cron"]
		default:
			if r.Hours, err = intCSV(v["hours"], "hours"); err != nil {
				return nil, err
			}
			if r.Minute, err = intVal("minute", "minute"); err != nil {
				return nil, err
			}
			if r.Preset == "days" {
				if r.Days, err = intCSV(v["days"], "days"); err != nil {
					return nil, err
				}
			}
			if r.Preset == "monthly" {
				if r.MonthDay, err = intVal("month_day", "day of month"); err != nil {
					return nil, err
				}
			}
		}
		a.Schedule = &r
	}
	a.applyDefaults()
	if err := a.validate(); err != nil {
		return nil, err
	}
	return a, nil
}

// workingHours builds the optional [heartbeat.working_hours] table. Both hours
// blank means no table at all: two zeroes would be written as a window that
// starts and ends at midnight, which validate() rejects — and days alone
// cannot be expressed without them.
func (f *formModel) workingHours() (*WorkingHours, error) {
	days := strings.TrimSpace(f.values["wh_days"])
	start := strings.TrimSpace(f.values["start_hour"])
	end := strings.TrimSpace(f.values["end_hour"])
	if start == "" && end == "" {
		if days != "" {
			return nil, fmt.Errorf("only on days: set from/to hours as well (0 and 24 for all day)")
		}
		return nil, nil
	}
	if start == "" || end == "" {
		return nil, fmt.Errorf("working hours: set both from hour and to hour")
	}
	wh := &WorkingHours{}
	var err error
	if wh.StartHour, err = strconv.Atoi(start); err != nil {
		return nil, fmt.Errorf("from hour: %q is not a number", start)
	}
	if wh.EndHour, err = strconv.Atoi(end); err != nil {
		return nil, fmt.Errorf("to hour: %q is not a number", end)
	}
	if days != "" {
		if wh.Days, err = intCSV(days, "only on days"); err != nil {
			return nil, err
		}
	}
	return wh, nil
}

func intCSV(s, label string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a number", label, part)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: at least one value required", label)
	}
	return out, nil
}

// writeActionFile generates clean TOML for a form-built action and writes it
// atomically to path. Overwrite policy is the caller's (trySave never clobbers
// a file it isn't editing).
func writeActionFile(path string, a *Action) error {
	var b strings.Builder
	fmt.Fprintf(&b, "name = %q\n", a.Name)
	fmt.Fprintf(&b, "kind = %q\n", a.Kind)
	fmt.Fprintf(&b, "directory = %q\n", a.Directory)
	fmt.Fprintf(&b, "enabled = %t\n", a.IsEnabled())
	if a.Kind == KindScript {
		fmt.Fprintf(&b, "command = %q\n", a.Command)
		fmt.Fprintf(&b, "timeout_minutes = %d\n", a.TimeoutMinutes)
	} else {
		fmt.Fprintf(&b, "prompt = %q\n", a.Prompt)
		fmt.Fprintf(&b, "cli = %q\n", a.CLI)
		if a.Model != "" {
			fmt.Fprintf(&b, "model = %q\n", a.Model)
		}
		fmt.Fprintf(&b, "permission_mode = %q\n", a.PermissionMode)
		fmt.Fprintf(&b, "auto_close = %t\n", a.AutoClose)
		fmt.Fprintf(&b, "watch_minutes = %d\n", a.WatchMinutes)
	}
	if a.Kind == KindHeartbeat {
		fmt.Fprintf(&b, "\n[heartbeat]\ninterval_minutes = %d\n", a.Heartbeat.IntervalMinutes)
		if wh := a.Heartbeat.WorkingHours; wh != nil {
			b.WriteString("\n[heartbeat.working_hours]\n")
			if len(wh.Days) > 0 {
				fmt.Fprintf(&b, "days = %s\n", intListTOML(wh.Days))
			}
			fmt.Fprintf(&b, "start_hour = %d\nend_hour = %d\n", wh.StartHour, wh.EndHour)
		}
	} else {
		r := a.Routine
		fmt.Fprintf(&b, "\n[schedule]\npreset = %q\n", r.Preset)
		if r.Preset == "cron" {
			fmt.Fprintf(&b, "cron = %q\n", r.Cron)
		} else {
			if r.Preset == "days" {
				fmt.Fprintf(&b, "days = %s\n", intListTOML(r.Days))
			}
			if r.Preset == "monthly" {
				fmt.Fprintf(&b, "month_day = %d\n", r.MonthDay)
			}
			fmt.Fprintf(&b, "hours = %s\n", intListTOML(r.Hours))
			fmt.Fprintf(&b, "minute = %d\n", r.Minute)
		}
	}
	// The suffix must not be .toml: a temp file left behind by a killed board
	// would otherwise load as an action of its own.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".form-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func intListTOML(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func (f *formModel) View() string {
	var b strings.Builder
	title, note := "New action", "starts paused until enabled"
	if f.editPath != "" {
		title, note = "Edit action", filepath.Base(f.editPath)
	}
	b.WriteString(styleHeader.Render(title) + styleDim.Render("  ·  "+note) + "\n")
	if f.err != "" {
		b.WriteString(styleError.Render(f.err) + "\n")
	} else {
		b.WriteString("\n")
	}
	help := ""
	for i, fd := range f.fields {
		value := f.values[fd.key]
		if f.editing && i == f.cursor {
			value = f.editBuf + "▌"
		} else if fd.ftype == ftEnum || fd.ftype == ftBool {
			if fd.ftype == ftBool {
				if value == "true" {
					value = "yes"
				} else {
					value = "no"
				}
			}
			value = "‹ " + value + " ›"
		}
		line := fmt.Sprintf("  %-14s %s", fd.label, value)
		if i == f.cursor {
			line = styleSelected.Render(line)
			help = fd.help
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	save, cancel := "  [ save ]", "  [ cancel ]"
	if f.cursor == f.saveRow() {
		save = styleSelected.Render(save)
	}
	if f.cursor == f.cancelRow() {
		cancel = styleSelected.Render(cancel)
	}
	b.WriteString(save + "\n" + cancel + "\n\n")
	if help != "" {
		b.WriteString(styleDim.Render(help) + "\n")
	}
	b.WriteString(styleDim.Render("↑↓/click fields · enter or ‹›/space change · esc cancel"))
	return b.String()
}
