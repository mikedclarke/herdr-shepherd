package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Kind string

const (
	KindHeartbeat Kind = "heartbeat"
	KindRoutine   Kind = "routine"
	KindScript    Kind = "script"
)

type HeartbeatSpec struct {
	IntervalMinutes int           `toml:"interval_minutes"`
	WorkingHours    *WorkingHours `toml:"working_hours"`
}

type RoutineSpec struct {
	Preset   string `toml:"preset"`
	Hours    []int  `toml:"hours"`
	Minute   int    `toml:"minute"`
	Days     []int  `toml:"days"`
	MonthDay int    `toml:"month_day"`
	Cron     string `toml:"cron"`
}

type Action struct {
	Name      string `toml:"name"`
	Kind      Kind   `toml:"kind"`
	Directory string `toml:"directory"`
	Enabled   *bool  `toml:"enabled"`
	Prompt    string `toml:"prompt"`
	// AppendSystemPrompt is passed verbatim to the CLI's --append-system-prompt
	// flag (claude and pi accept it; pi also treats an existing file path as a
	// file to inline). It lets a large stable instruction block live in the
	// system prompt, where an inference server's prefix cache can re-serve it.
	AppendSystemPrompt string `toml:"append_system_prompt"`
	CLI                string `toml:"cli"`
	Model              string `toml:"model"`
	PermissionMode     string `toml:"permission_mode"`
	AutoClose          bool   `toml:"auto_close"`
	WatchMinutes       int    `toml:"watch_minutes"`
	Command            string `toml:"command"`
	TimeoutMinutes     int    `toml:"timeout_minutes"`
	// DeferRetryMinutes is how long a script that exits 75 (deferred) keeps
	// being retried on subsequent ticks; 0 means record the deferral and stop.
	DeferRetryMinutes int `toml:"defer_retry_minutes"`
	// Gate is a command run before an agent action's workspace opens. Exit 0
	// runs the agent, exit 75 skips the occurrence, anything else runs the
	// agent anyway (a broken gate must not silence a schedule).
	Gate               string `toml:"gate"`
	GateTimeoutMinutes int    `toml:"gate_timeout_minutes"`

	Heartbeat HeartbeatSpec `toml:"heartbeat"`
	Schedule  *RoutineSpec  `toml:"schedule"`
	// Deprecated alias for [schedule], kept for existing configs.
	Routine RoutineSpec `toml:"routine"`

	// SourceFile is the TOML file this action was loaded from; the board edits
	// enabled-state in place through it.
	SourceFile string `toml:"-"`
}

func (a *Action) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

func (a *Action) applyDefaults() {
	if a.Schedule != nil {
		a.Routine = *a.Schedule
	}
	if a.CLI == "" {
		a.CLI = "claude"
	}
	if a.PermissionMode == "" {
		a.PermissionMode = "default"
	}
	a.Gate = strings.TrimSpace(a.Gate)
	if a.Gate != "" && a.GateTimeoutMinutes == 0 {
		a.GateTimeoutMinutes = defaultGateTimeoutMinutes
	}
	if a.WatchMinutes <= 0 {
		a.WatchMinutes = 240
	}
	if a.TimeoutMinutes <= 0 {
		a.TimeoutMinutes = 30
	}
	if a.Heartbeat.IntervalMinutes == 0 {
		a.Heartbeat.IntervalMinutes = 30
	}
	if a.Routine.Preset == "" {
		a.Routine.Preset = "daily"
	}
	if len(a.Routine.Hours) == 0 {
		a.Routine.Hours = []int{9}
	}
	if a.Routine.MonthDay == 0 {
		a.Routine.MonthDay = 1
	}
}

// maxRunMinutes caps watch and timeout windows at a day: a longer one is a
// typo, and it would pin a run for the daemon's lifetime.
const maxRunMinutes = 1440

// validate rejects rather than clamps: a silently adjusted schedule runs at a
// time the user never asked for.
func (a *Action) validate() error {
	if a.Name == "" {
		return fmt.Errorf("name is required")
	}
	if strings.ContainsAny(a.Name, " \t\n") {
		return fmt.Errorf("name %q must not contain whitespace", a.Name)
	}
	// The name is used as a file name for the action's run lock.
	if strings.ContainsAny(a.Name, `/\`) {
		return fmt.Errorf("name %q must not contain a path separator", a.Name)
	}
	if strings.HasPrefix(a.Name, ".") {
		return fmt.Errorf("name %q must not start with a dot", a.Name)
	}
	switch a.Kind {
	case KindHeartbeat, KindRoutine:
		if strings.TrimSpace(a.Prompt) == "" {
			return fmt.Errorf("%s: prompt is required", a.Name)
		}
		if a.CLI != "claude" && a.CLI != "codex" && a.CLI != "pi" {
			return fmt.Errorf("%s: cli must be claude, codex, or pi, got %q", a.Name, a.CLI)
		}
		switch a.PermissionMode {
		case "default", "auto", "skip":
		default:
			return fmt.Errorf("%s: permission_mode must be default, auto, or skip, got %q", a.Name, a.PermissionMode)
		}
		if a.CLI == "pi" && a.PermissionMode != "default" {
			return fmt.Errorf("%s: pi has no permission flags; permission_mode must be default, got %q", a.Name, a.PermissionMode)
		}
		if a.AppendSystemPrompt != "" && a.CLI == "codex" {
			return fmt.Errorf("%s: codex has no --append-system-prompt flag; append_system_prompt needs claude or pi", a.Name)
		}
		if a.DeferRetryMinutes != 0 {
			return fmt.Errorf("%s: defer_retry_minutes only applies to script actions", a.Name)
		}
		if a.Gate == "" && a.GateTimeoutMinutes != 0 {
			return fmt.Errorf("%s: gate_timeout_minutes needs a gate", a.Name)
		}
		if a.Gate != "" && (a.GateTimeoutMinutes < 1 || a.GateTimeoutMinutes > maxRunMinutes) {
			return fmt.Errorf("%s: gate_timeout_minutes must be 1-%d, got %d", a.Name, maxRunMinutes, a.GateTimeoutMinutes)
		}
	case KindScript:
		if strings.TrimSpace(a.Command) == "" {
			return fmt.Errorf("%s: command is required", a.Name)
		}
		if a.Gate != "" || a.GateTimeoutMinutes != 0 {
			return fmt.Errorf("%s: gate only applies to agent actions", a.Name)
		}
		if a.AppendSystemPrompt != "" {
			return fmt.Errorf("%s: append_system_prompt only applies to agent actions", a.Name)
		}
		if a.DeferRetryMinutes < 0 || a.DeferRetryMinutes > maxRunMinutes {
			return fmt.Errorf("%s: defer_retry_minutes must be 0-%d, got %d", a.Name, maxRunMinutes, a.DeferRetryMinutes)
		}
	default:
		return fmt.Errorf("%s: kind must be heartbeat, routine, or script, got %q", a.Name, a.Kind)
	}
	if a.Directory == "" {
		return fmt.Errorf("%s: directory is required", a.Name)
	}
	if a.TimeoutMinutes > maxRunMinutes {
		return fmt.Errorf("%s: timeout_minutes must be <= %d, got %d", a.Name, maxRunMinutes, a.TimeoutMinutes)
	}
	if a.WatchMinutes > maxRunMinutes {
		return fmt.Errorf("%s: watch_minutes must be <= %d, got %d", a.Name, maxRunMinutes, a.WatchMinutes)
	}
	if a.Kind == KindHeartbeat {
		if a.Heartbeat.IntervalMinutes < 1 {
			return fmt.Errorf("%s: heartbeat interval_minutes must be >= 1", a.Name)
		}
		if wh := a.Heartbeat.WorkingHours; wh != nil {
			if err := wh.validate(); err != nil {
				return fmt.Errorf("%s: %w", a.Name, err)
			}
		}
	}
	if a.Kind == KindRoutine || a.Kind == KindScript {
		r := a.Routine
		if r.Minute < 0 || r.Minute > 59 {
			return fmt.Errorf("%s: schedule minute must be 0-59, got %d", a.Name, r.Minute)
		}
		if r.MonthDay < 1 || r.MonthDay > 28 {
			return fmt.Errorf("%s: schedule month_day must be 1-28, got %d", a.Name, r.MonthDay)
		}
		for _, h := range r.Hours {
			if h < 0 || h > 23 {
				return fmt.Errorf("%s: schedule hour %d out of range 0-23", a.Name, h)
			}
		}
		for _, d := range r.Days {
			if d < 0 || d > 6 {
				return fmt.Errorf("%s: schedule day %d out of range 0-6", a.Name, d)
			}
		}
		// A parseable expression can still be unsatisfiable (e.g. Feb 30);
		// catch it here instead of never running. The scan runs from an
		// explicit now over scanYears, so Feb 29 validates in any year.
		if _, err := r.NextRoutine(time.Now()); err != nil {
			return fmt.Errorf("%s: %w", a.Name, err)
		}
	}
	return nil
}

// Dir returns the action's directory with ~ and $VARS expanded.
func (a *Action) Dir() string {
	return expandPath(a.Directory)
}

func expandPath(p string) string {
	p = os.ExpandEnv(p)
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// contractPath is the display-side inverse of expandPath: a path under the
// home directory renders as ~/... so views stay short and machine-agnostic.
func contractPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

// AgentCommand builds the shell command that starts the agent session with the
// action's prompt. Flags match what each CLI accepts for its permission modes.
func (a *Action) AgentCommand() (string, error) {
	prompt := strings.TrimSpace(a.Prompt)
	if prompt == "" {
		return "", fmt.Errorf("prompt is required")
	}
	parts := []string{a.CLI}
	switch a.CLI {
	case "claude":
		switch a.PermissionMode {
		case "auto":
			parts = append(parts, "--permission-mode", "auto")
		case "skip":
			parts = append(parts, "--dangerously-skip-permissions")
		}
	case "codex":
		switch a.PermissionMode {
		case "auto":
			parts = append(parts, "--ask-for-approval", "on-request", "--sandbox", "workspace-write")
		case "skip":
			parts = append(parts, "--dangerously-bypass-approvals-and-sandbox")
		}
	case "pi":
		// A bare prompt argument opens an interactive session (pi -p is the
		// headless print mode, which a watched pane must not use).
		if a.PermissionMode != "default" {
			return "", fmt.Errorf("pi has no permission flags; permission_mode must be default, got %q", a.PermissionMode)
		}
	default:
		return "", fmt.Errorf("unsupported cli %q", a.CLI)
	}
	if s := strings.TrimSpace(a.AppendSystemPrompt); s != "" {
		parts = append(parts, "--append-system-prompt", shellQuote(s))
	}
	if m := strings.TrimSpace(a.Model); m != "" {
		parts = append(parts, "--model", shellQuote(m))
	}
	parts = append(parts, shellQuote(prompt))
	return strings.Join(parts, " "), nil
}

// shellQuote is POSIX single-quote quoting; it assumes an sh-compatible shell
// in the target pane.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// LoadActions reads every *.toml in dir. A missing dir is an empty list, not
// an error, so the daemon runs cleanly before the user configures anything.
// A broken file disables only itself: it is returned in fileErrs while the
// remaining files load normally.
func LoadActions(dir string) (actions []*Action, fileErrs []error, err error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	seen := map[string]string{}
	for _, name := range names {
		path := filepath.Join(dir, name)
		var a Action
		md, decodeErr := toml.DecodeFile(path, &a)
		if decodeErr != nil {
			fileErrs = append(fileErrs, fmt.Errorf("%s: %w", name, decodeErr))
			continue
		}
		if u := md.Undecoded(); len(u) > 0 {
			fileErrs = append(fileErrs, fmt.Errorf("%s: unknown key %q", name, u[0].String()))
			continue
		}
		a.applyDefaults()
		a.SourceFile = path
		if err := a.validate(); err != nil {
			fileErrs = append(fileErrs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if prev, dup := seen[a.Name]; dup {
			fileErrs = append(fileErrs, fmt.Errorf("%s: duplicate action name %q (also in %s)", name, a.Name, prev))
			continue
		}
		seen[a.Name] = name
		actions = append(actions, &a)
	}
	return actions, fileErrs, nil
}
