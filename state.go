package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	home, homeErr := os.UserHomeDir()
	managed := p.ConfigDir != ""
	if p.ConfigDir == "" {
		if dir := herdrConfigDir(); dir != "" {
			p.ConfigDir = dir
			managed = true
		} else if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			p.ConfigDir = filepath.Join(x, "herdr-shepherd")
			warnConfigFallback(p.ConfigDir)
		} else {
			p.ConfigDir = filepath.Join(mustHome(home, homeErr), ".config", "herdr-shepherd")
			warnConfigFallback(p.ConfigDir)
		}
	}
	if p.StateDir == "" {
		if managed {
			// herdr 0.7.x keeps plugin state here; the injected env var is
			// authoritative when present.
			p.StateDir = filepath.Join(mustHome(home, homeErr), ".local", "state", "herdr", "plugins", pluginID)
		} else if x := os.Getenv("XDG_STATE_HOME"); x != "" {
			p.StateDir = filepath.Join(x, "herdr-shepherd")
		} else {
			p.StateDir = filepath.Join(mustHome(home, homeErr), ".local", "state", "herdr-shepherd")
		}
	}
	return p
}

// mustHome refuses to continue without a home directory: joining an empty one
// resolves config and state to relative paths under whatever the working
// directory happens to be.
func mustHome(home string, err error) string {
	if err != nil || home == "" {
		fmt.Fprintln(os.Stderr, "herdr-shepherd: cannot determine the home directory;"+
			" set HERDR_PLUGIN_CONFIG_DIR and HERDR_PLUGIN_STATE_DIR")
		os.Exit(1)
	}
	return home
}

// warnConfigFallback names the directory in use when herdr could not be asked
// for its managed one — the daemon inside herdr may be reading a different one.
func warnConfigFallback(dir string) {
	fmt.Fprintf(os.Stderr, "warning: herdr unreachable, using %s (the daemon may be using herdr's managed plugin directory)\n", dir)
}

// herdrCallTimeout bounds the config-dir lookup; a wedged herdr must not hang
// every CLI invocation.
const herdrCallTimeout = 5 * time.Second

func herdrConfigDir() string {
	bin := os.Getenv("HERDR_BIN_PATH")
	if bin == "" {
		found, err := exec.LookPath("herdr")
		if err != nil {
			return ""
		}
		bin = found
	}
	ctx, cancel := context.WithTimeout(context.Background(), herdrCallTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "plugin", "config-dir", pluginID).Output()
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

func emptyState() *daemonState {
	return &daemonState{Actions: map[string]*actionState{}}
}

// loadState is the daemon's loader: it owns the file, so a corrupt one is
// quarantined rather than silently resetting every schedule.
func loadState(path string) *daemonState {
	st, err := parseStateFile(path)
	if err != nil {
		if rerr := os.Rename(path, path+".corrupt"); rerr != nil {
			log.Printf("quarantine corrupt state %s: %v", path, rerr)
		}
		return emptyState()
	}
	return st
}

// readState is the read-only loader for the CLI and the board. Renaming a file
// the daemon is writing is the daemon's call, not a reader's.
func readState(path string) *daemonState {
	st, err := parseStateFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: ignoring unreadable state file %s: %v\n", path, err)
		return emptyState()
	}
	return st
}

// parseStateFile treats a missing file as empty state: a daemon that has never
// run is not an error.
func parseStateFile(path string) (*daemonState, error) {
	st := emptyState()
	data, err := os.ReadFile(path)
	if err != nil {
		return st, nil
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, err
	}
	if st.Actions == nil {
		st.Actions = map[string]*actionState{}
	}
	return st, nil
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
	// Empty for a scheduled run; "manual" for one a person asked for.
	Trigger string `json:"trigger,omitempty"`
}

const (
	runLogMaxBytes  = 5 << 20
	runDetailMaxLen = 512
	triggerManual   = "manual"
)

// workspaceID returns the workspace a record's detail names, if any. Agent
// records carry it so a started run can be paired with the record that ended
// it.
func (r runRecord) workspaceID() string {
	i := strings.Index(r.Detail, "workspace ")
	if i < 0 {
		return ""
	}
	rest := r.Detail[i+len("workspace "):]
	if j := strings.IndexAny(rest, " ;"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// appendRunLog appends one record, rotating the log first when it has grown
// past runLogMaxBytes. The daemon, the CLI and the board all append here, so
// stat, rotate and write happen under one flock — taken on a separate file,
// because a lock on the log's inode stops guarding the path once it is
// renamed. On a network filesystem without working flock, concurrent appends
// may interleave.
func appendRunLog(path string, rec runRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if info, serr := os.Stat(path); serr == nil && info.Size() > runLogMaxBytes {
		if rerr := os.Rename(path, path+".1"); rerr != nil {
			return rerr
		}
	}
	rec.Detail = tailString(rec.Detail, runDetailMaxLen)
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, werr := f.Write(append(data, '\n')); werr != nil {
		f.Close()
		return werr
	}
	return f.Close()
}

// markInterrupted records runs that were still open when the daemon last
// stopped: a started record with no later record for its workspace. Manual
// runs are unwatched by design and never get a closing record, so they are
// skipped. The appended records close the runs out, so a later start does not
// report them twice.
func markInterrupted(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	open := map[string]runRecord{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var r runRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		ws := r.workspaceID()
		if ws == "" {
			continue
		}
		if r.Status == "started" {
			if r.Trigger == triggerManual {
				continue
			}
			if _, dup := open[ws]; !dup {
				order = append(order, ws)
			}
			open[ws] = r
			continue
		}
		delete(open, ws)
	}
	scanErr := sc.Err()
	f.Close()
	if scanErr != nil {
		return scanErr
	}
	for _, ws := range order {
		r, ok := open[ws]
		if !ok {
			continue
		}
		if err := appendRunLog(path, runRecord{
			At: time.Now(), Action: r.Action, Kind: r.Kind,
			Status: "interrupted", Detail: "workspace " + ws,
		}); err != nil {
			return err
		}
	}
	return nil
}
