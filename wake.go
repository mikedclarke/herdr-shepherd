package main

import (
	"errors"
	"fmt"
	"log"
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

var (
	errWakeDebounced = errors.New("a wake is already queued")
	errWakeInPast    = errors.New("the instant is further in the past than a wake survives")
)

func wakeDir(stateDir string) string { return filepath.Join(stateDir, "wake") }

func wakePath(stateDir, name string) string { return filepath.Join(wakeDir(stateDir), name+".wake") }

func wakeAtPath(stateDir, name string) string {
	return filepath.Join(wakeDir(stateDir), name+".wake.at")
}

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

// A scheduled wake is a second file, <StateDir>/wake/<action>.wake.at, holding the
// instant on its first line and the requester on its second. The daemon promotes it
// to a normal wake on the first tick that reaches the instant, so a producer that
// already knows when it will want a run (a calendar prep, 25 minutes before the
// call) can ask now and stop watching the clock. One file per action: the latest
// schedule replaces the last.

// scheduleWake asks for a wake at an instant instead of on the next tick. It
// returns errWakeInPast for an instant so old the daemon would drop it on sight.
func scheduleWake(stateDir, name string, at time.Time, source string) error {
	if time.Since(at) > wakeMaxAge {
		return errWakeInPast
	}
	dir := wakeDir(stateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+"-*.wake.at")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(at.Format(time.RFC3339) + "\n" + strings.TrimSpace(source) + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), wakeAtPath(stateDir, name))
}

// readScheduledWake reads one .wake.at file: its instant and its requester.
func readScheduledWake(path string) (time.Time, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, "", err
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(lines[0]))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("unreadable instant: %w", err)
	}
	source := ""
	if len(lines) > 1 {
		source = strings.TrimSpace(lines[1])
	}
	return at, source, nil
}

// promoteScheduledWakes turns every scheduled wake whose instant has arrived into an
// ordinary wake, so the tick's action loop fires it under exactly the rules a chat
// wake gets. A file left further than wakeMaxAge behind its instant is dropped rather
// than fired late: the daemon was down, and the reason for the wake went with the hour.
func promoteScheduledWakes(stateDir string, now time.Time) {
	entries, err := os.ReadDir(wakeDir(stateDir))
	if err != nil {
		return
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".wake.at")
		if !ok || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(wakeDir(stateDir), e.Name())
		at, source, err := readScheduledWake(path)
		if err != nil {
			log.Printf("%s: scheduled wake dropped, %v", name, err)
			os.Remove(path)
			continue
		}
		if at.After(now) {
			continue
		}
		if now.Sub(at) > wakeMaxAge {
			log.Printf("%s: scheduled wake dropped, %d min past its instant", name, int(now.Sub(at).Minutes()))
			os.Remove(path)
			continue
		}
		// A debounced result is fine: a wake is queued either way, which is all
		// the schedule was ever asking for.
		if err := requestWake(stateDir, name, source); err != nil && !errors.Is(err, errWakeDebounced) {
			log.Printf("%s: scheduled wake not queued: %v", name, err)
			continue
		}
		os.Remove(path)
	}
}

// pruneWakes drops wake files, queued and scheduled, for actions that no longer exist.
func pruneWakes(stateDir string, names map[string]bool) {
	entries, err := os.ReadDir(wakeDir(stateDir))
	if err != nil {
		return
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".wake.at")
		if !ok {
			name, ok = strings.CutSuffix(e.Name(), ".wake")
		}
		if !ok || strings.HasPrefix(name, ".") {
			continue
		}
		if !names[name] {
			os.Remove(filepath.Join(wakeDir(stateDir), e.Name()))
		}
	}
}
