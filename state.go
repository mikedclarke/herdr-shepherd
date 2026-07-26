package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const pluginID = "mikedclarke.herdr-shepherd"

// paths resolves the config and state directories. Inside herdr these are the
// injected plugin directories. Outside plugin context (an ordinary pane shell)
// the env vars are absent, so the CLI asks herdr for the managed config dir —
// otherwise `list` and `status` would silently read different directories than
// the daemon. XDG-style fallbacks apply only when herdr itself is unreachable.
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
	managed := p.ConfigDir != ""
	if p.ConfigDir == "" {
		if dir := herdrConfigDir(); dir != "" {
			p.ConfigDir = dir
			managed = true
		} else if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			p.ConfigDir = filepath.Join(x, "herdr-shepherd")
		} else {
			p.ConfigDir = filepath.Join(home, ".config", "herdr-shepherd")
		}
	}
	if p.StateDir == "" {
		if managed {
			// herdr 0.7.x keeps plugin state here; the injected env var is
			// authoritative when present.
			p.StateDir = filepath.Join(home, ".local", "state", "herdr", "plugins", pluginID)
		} else if x := os.Getenv("XDG_STATE_HOME"); x != "" {
			p.StateDir = filepath.Join(x, "herdr-shepherd")
		} else {
			p.StateDir = filepath.Join(home, ".local", "state", "herdr-shepherd")
		}
	}
	return p
}

func herdrConfigDir() string {
	bin := os.Getenv("HERDR_BIN_PATH")
	if bin == "" {
		found, err := exec.LookPath("herdr")
		if err != nil {
			return ""
		}
		bin = found
	}
	out, err := exec.Command(bin, "plugin", "config-dir", pluginID).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (p paths) ActionsDir() string { return filepath.Join(p.ConfigDir, "actions") }
func (p paths) StateFile() string  { return filepath.Join(p.StateDir, "state.json") }
func (p paths) RunLogFile() string { return filepath.Join(p.StateDir, "runs.jsonl") }
func (p paths) LockFile() string   { return filepath.Join(p.StateDir, "daemon.lock") }
func (p paths) LogFile() string    { return filepath.Join(p.StateDir, "shepherd.log") }

type actionState struct {
	LastRunAt  time.Time `json:"last_run_at"`
	LastStatus string    `json:"last_status"`
}

// daemonState is shared between the tick loop and every fire goroutine; all
// access goes through the mutexed methods (an unguarded map write during
// save's marshal is a fatal, unrecoverable runtime error).
type daemonState struct {
	mu          sync.Mutex
	HeartbeatAt time.Time               `json:"heartbeat_at"`
	Actions     map[string]*actionState `json:"actions"`
}

func loadState(path string) *daemonState {
	st := &daemonState{Actions: map[string]*actionState{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	if err := json.Unmarshal(data, st); err != nil {
		// Keep the evidence rather than silently resetting every schedule.
		os.Rename(path, path+".corrupt")
		return &daemonState{Actions: map[string]*actionState{}}
	}
	if st.Actions == nil {
		st.Actions = map[string]*actionState{}
	}
	return st
}

func (st *daemonState) lastRun(name string) time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	if a := st.Actions[name]; a != nil {
		return a.LastRunAt
	}
	return time.Time{}
}

func (st *daemonState) lastStatus(name string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if a := st.Actions[name]; a != nil {
		return a.LastStatus
	}
	return ""
}

func (st *daemonState) setLastRun(name string, t time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.action(name).LastRunAt = t
}

func (st *daemonState) setStatus(name, status string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.action(name).LastStatus = status
}

func (st *daemonState) beat(t time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.HeartbeatAt = t
}

func (st *daemonState) heartbeatAt() time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.HeartbeatAt
}

// action assumes st.mu is held.
func (st *daemonState) action(name string) *actionState {
	if st.Actions[name] == nil {
		st.Actions[name] = &actionState{}
	}
	return st.Actions[name]
}

// prune drops state for actions that no longer exist in the config.
func (st *daemonState) prune(names map[string]bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for name := range st.Actions {
		if !names[name] {
			delete(st.Actions, name)
		}
	}
}

func (st *daemonState) save(path string) error {
	st.mu.Lock()
	data, err := json.MarshalIndent(st, "", " ")
	st.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// A per-save temp file: a shared name lets two writers rename each
	// other's half-written file into place.
	tmp, err := os.CreateTemp(filepath.Dir(path), "state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

type runRecord struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Kind   Kind      `json:"kind"`
	Status string    `json:"status"`
	Detail string    `json:"detail,omitempty"`
}

const runLogMaxBytes = 5 << 20

func appendRunLog(path string, rec runRecord) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if info, err := os.Stat(path); err == nil && info.Size() > runLogMaxBytes {
		os.Rename(path, path+".1")
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
