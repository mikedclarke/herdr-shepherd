//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunLockExcludesSecondHolder(t *testing.T) {
	dir := t.TempDir()
	release, ok, err := tryRunLock(dir, "build-sync")
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	if _, ok, err := tryRunLock(dir, "build-sync"); ok || err != nil {
		t.Fatalf("second acquire should be refused cleanly: ok=%v err=%v", ok, err)
	}
	if !runLockHeld(dir, "build-sync") {
		t.Error("a held lock should report held")
	}
	if runLockHeld(dir, "nightly-report") {
		t.Error("another action's lock is unrelated")
	}
	release()
	if runLockHeld(dir, "build-sync") {
		t.Error("the lock should be free after release")
	}
	release2, ok, err := tryRunLock(dir, "build-sync")
	if err != nil || !ok {
		t.Fatalf("reacquire: ok=%v err=%v", ok, err)
	}
	release2()
}

func TestRunLockLivesUnderTheStateDir(t *testing.T) {
	dir := t.TempDir()
	release, _, err := tryRunLock(dir, "build-sync")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := os.Stat(filepath.Join(dir, "running", "build-sync.lock")); err != nil {
		t.Fatalf("the lock file should live under <state>/running: %v", err)
	}
}
