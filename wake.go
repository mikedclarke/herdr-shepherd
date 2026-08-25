package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A wake asks the daemon to fire an action on its next tick, outside the
// schedule. It is a file, <StateDir>/wake/<action>.wake, whose content names the
// requester; one file per action means at most one wake is ever queued. The
// daemon owns execution: the CLI only writes the file, so a wake behind a
// running run waits for the lock like any scheduled occurrence would.
const (
	// A second request inside this window is the same wake.
	wakeDebounce = 20 * time.Second
	// A wake older than this is dropped, not fired: a request made while the
	// daemon was down must not fire hours later (compare catchUpGrace).
	wakeMaxAge  = 1 * time.Hour
	triggerWake = "wake"
	// The value of SHEPHERD_TRIGGER for a scheduled occurrence.
	triggerSchedule = "schedule"
)

var errWakeDebounced = errors.New("a wake is already queued")

func wakeDir(stateDir string) string { return filepath.Join(stateDir, "wake") }

func wakePath(stateDir, name string) string { return filepath.Join(wakeDir(stateDir), name+".wake") }

// requestWake queues a wake for name. It returns errWakeDebounced when a wake
// younger than wakeDebounce already exists; an older leftover is replaced.
func requestWake(stateDir, name, source string) error {
	dir := wakeDir(stateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := wakePath(stateDir, name)
	if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < wakeDebounce {
		return errWakeDebounced
	}
	tmp, err := os.CreateTemp(dir, "."+name+"-*.wake")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.TrimSpace(source) + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// wakePending reports whether a live wake is queued for name. A wake past
// wakeMaxAge is removed and reported as absent.
func wakePending(stateDir, name string) bool {
	path := wakePath(stateDir, name)
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if time.Since(info.ModTime()) > wakeMaxAge {
		os.Remove(path)
		return false
	}
	return true
}

// consumeWake removes the wake file for name and returns its requester. The
// removal is the claim: of two callers racing for one wake, only the one whose
// remove succeeds gets a nil error.
func consumeWake(stateDir, name string) (string, error) {
	path := wakePath(stateDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// pruneWakes drops wake files for actions that no longer exist.
func pruneWakes(stateDir string, names map[string]bool) {
	entries, err := os.ReadDir(wakeDir(stateDir))
	if err != nil {
		return
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".wake")
		if !ok || strings.HasPrefix(name, ".") {
			continue
		}
		if !names[name] {
			os.Remove(filepath.Join(wakeDir(stateDir), e.Name()))
		}
	}
}
