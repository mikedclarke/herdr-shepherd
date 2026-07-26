package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// paths resolves the config and state directories. Inside herdr these are the
// injected plugin directories; outside (bare CLI use) they fall back to the
// XDG-style equivalents so `shepherd list` works from any shell.
type paths struct {
	ConfigDir string
	StateDir  string
}

func resolvePaths() paths {
	p := paths{
		ConfigDir: os.Getenv("HERDR_PLUGIN_CONFIG_DIR"),
		StateDir:  os.Getenv("HERDR_PLUGIN_STATE_DIR"),
	}
	home, _ := os.UserHomeDir()
	if p.ConfigDir == "" {
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			p.ConfigDir = filepath.Join(x, "herdr-shepherd")
		} else {
			p.ConfigDir = filepath.Join(home, ".config", "herdr-shepherd")
		}
	}
	if p.StateDir == "" {
		if x := os.Getenv("XDG_STATE_HOME"); x != "" {
			p.StateDir = filepath.Join(x, "herdr-shepherd")
		} else {
			p.StateDir = filepath.Join(home, ".local", "state", "herdr-shepherd")
		}
	}
	return p
}

func (p paths) ActionsDir() string   { return filepath.Join(p.ConfigDir, "actions") }
func (p paths) StateFile() string    { return filepath.Join(p.StateDir, "state.json") }
func (p paths) RunLogFile() string   { return filepath.Join(p.StateDir, "runs.jsonl") }
func (p paths) LockFile() string     { return filepath.Join(p.StateDir, "daemon.pid") }

type actionState struct {
	LastRunAt  time.Time `json:"last_run_at"`
	LastStatus string    `json:"last_status"`
}

type daemonState struct {
	HeartbeatAt time.Time               `json:"heartbeat_at"`
	Actions     map[string]*actionState `json:"actions"`
}

func loadState(path string) *daemonState {
	st := &daemonState{Actions: map[string]*actionState{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	if json.Unmarshal(data, st) != nil || st.Actions == nil {
		st.Actions = map[string]*actionState{}
	}
	return st
}

func (st *daemonState) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (st *daemonState) forAction(name string) *actionState {
	if st.Actions[name] == nil {
		st.Actions[name] = &actionState{}
	}
	return st.Actions[name]
}

type runRecord struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Kind   Kind      `json:"kind"`
	Status string    `json:"status"`
	Detail string    `json:"detail,omitempty"`
}

func appendRunLog(path string, rec runRecord) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if data, err := json.Marshal(rec); err == nil {
		f.Write(append(data, '\n'))
	}
}
