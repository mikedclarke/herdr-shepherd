//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Run locks are per-action flocks under <StateDir>/running. They coordinate
// the daemon, the CLI and the board across processes, and the kernel releases
// them however a holder dies — a killed run never leaves an action wedged.
// The files are not unlinked on release: removing one races a process that
// has already opened it.
func tryRunLock(stateDir, name string) (release func(), ok bool, err error) {
	dir := filepath.Join(stateDir, "running")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
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
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true, nil
}

// runLockHeld probes the lock without keeping it. A state dir that cannot be
// locked at all reports not-held: refusing every run is worse than allowing
// one the daemon may also be starting.
func runLockHeld(stateDir, name string) bool {
	release, ok, err := tryRunLock(stateDir, name)
	if err != nil {
		return false
	}
	if !ok {
		return true
	}
	release()
	return false
}
