//go:build unix

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Run locks are per-action flocks under <StateDir>/running. They coordinate
// the daemon, the CLI and the board across processes, and the kernel releases
// them however a holder dies — a killed run never leaves an action wedged.
// The files are not unlinked on release: removing one races a process that
// has already opened it.
//
// The file also carries the pid of the process doing the run, once there is
// one. The flock goes with the daemon that took it, but a run that daemon
// started can outlive it, and the recorded pid is what still answers for the
// action.
type runLock struct {
	f *os.File
}

func runLockPath(stateDir, name string) string {
	return filepath.Join(stateDir, "running", name+".lock")
}

// openRunLock takes the action's lock and returns the handle, so the caller can
// record the run's pid in it.
func openRunLock(stateDir, name string) (lock *runLock, ok bool, err error) {
	if err := os.MkdirAll(filepath.Join(stateDir, "running"), 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(runLockPath(stateDir, name), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &runLock{f: f}, true, nil
}

func tryRunLock(stateDir, name string) (release func(), ok bool, err error) {
	lock, ok, err := openRunLock(stateDir, name)
	if lock == nil {
		return nil, ok, err
	}
	return lock.release, ok, nil
}

func (l *runLock) release() {
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}

// setPid records the pid of the process doing the run, replacing whatever the
// file held. A pid of 0 clears the record, which is what the end of a run
// writes: a finished run must not keep its action locked once its number is
// handed to some unrelated process.
func (l *runLock) setPid(pid int) error {
	// Write first, then cut the file to the new length: a reader that lands
	// between the two sees the old pid or the new one, never an empty file.
	text := ""
	if pid > 0 {
		text = strconv.Itoa(pid)
	}
	if _, err := l.f.WriteAt([]byte(text), 0); err != nil {
		return err
	}
	return l.f.Truncate(int64(len(text)))
}

// recordedPid reads the pid the lock file carries. An empty file, or one that
// does not parse, reports 0: no pid was recorded, so the flock is the only
// guard, exactly as it was before the file carried one.
func (l *runLock) recordedPid() int {
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	data, err := io.ReadAll(l.f)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// pidAlive probes a process without signalling it. Permission denied means the
// process exists under another user; only "no such process" means it is gone.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// runLockHeld probes the lock without keeping it: the flock, and failing that
// the pid of a run that outlived the daemon which started it. A state dir that
// cannot be locked at all reports not-held: refusing every run is worse than
// allowing one the daemon may also be starting.
//
// maxAge bounds how long a recorded pid is believed: a run cannot outlive its
// action's timeout, so a lock file older than that holds a number that has
// been handed to some other process (a daemon killed outright never clears
// it), and believing it would keep the action from ever running again.
func runLockHeld(stateDir, name string, maxAge time.Duration) bool {
	lock, ok, err := openRunLock(stateDir, name)
	if err != nil {
		return false
	}
	if !ok {
		return true
	}
	pid := lock.recordedPid()
	stale := false
	if info, err := lock.f.Stat(); err == nil && maxAge > 0 {
		stale = time.Since(info.ModTime()) > maxAge
	}
	lock.release()
	return !stale && pidAlive(pid)
}
