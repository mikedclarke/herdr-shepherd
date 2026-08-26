//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
	if !runLockHeld(dir, "build-sync", time.Hour) {
		t.Error("a held lock should report held")
	}
	if runLockHeld(dir, "nightly-report", time.Hour) {
		t.Error("another action's lock is unrelated")
	}
	release()
	if runLockHeld(dir, "build-sync", time.Hour) {
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

func TestRunLockRecordsTheRunPid(t *testing.T) {
	dir := t.TempDir()
	lock, ok, err := openRunLock(dir, "build-sync")
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if err := lock.setPid(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(runLockPath(dir, "build-sync"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("the lock file should carry the run pid, got %q", got)
	}
	if got := lock.recordedPid(); got != os.Getpid() {
		t.Errorf("the pid should read back, got %d", got)
	}
	// The flock is gone with the release, as it would be with the daemon that
	// took it; the live pid still answers for the action.
	lock.release()
	if !runLockHeld(dir, "build-sync", time.Hour) {
		t.Error("a run whose process is still alive should report held")
	}
}

func TestRunLockPidIsClearedAtTheEndOfARun(t *testing.T) {
	dir := t.TempDir()
	lock, ok, err := openRunLock(dir, "build-sync")
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if err := lock.setPid(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := lock.setPid(0); err != nil {
		t.Fatal(err)
	}
	lock.release()
	if runLockHeld(dir, "build-sync", time.Hour) {
		t.Error("a finished run must leave its action free")
	}
}

func TestRunLockHeldIgnoresADeadPid(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	lock, ok, err := openRunLock(dir, "build-sync")
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if err := lock.setPid(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	lock.release()
	if runLockHeld(dir, "build-sync", time.Hour) {
		t.Error("a run whose process is gone must not keep its action locked")
	}
}

func TestRunLockHeldIgnoresAFileWithNoPid(t *testing.T) {
	dir := t.TempDir()
	release, ok, err := tryRunLock(dir, "build-sync")
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	release()
	for _, contents := range []string{"", "   ", "not a pid", "-1"} {
		if err := os.WriteFile(runLockPath(dir, "build-sync"), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		if runLockHeld(dir, "build-sync", time.Hour) {
			t.Errorf("a lock file holding %q records no pid and must not report held", contents)
		}
	}
}

func TestRunLockHeldIgnoresAPidOlderThanTheActionTimeout(t *testing.T) {
	// A daemon killed outright never clears the pid; once the number has been
	// handed to another process the file would lock the action for ever, so a
	// record older than any run could be is not believed.
	dir := t.TempDir()
	lock, ok, err := openRunLock(dir, "build-sync")
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if err := lock.setPid(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	lock.release()
	if !runLockHeld(dir, "build-sync", time.Hour) {
		t.Fatal("a fresh live pid should report held")
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(runLockPath(dir, "build-sync"), old, old); err != nil {
		t.Fatal(err)
	}
	if runLockHeld(dir, "build-sync", time.Hour) {
		t.Error("a pid recorded longer ago than the action could run must not report held")
	}
}

func TestSetPidNeverLeavesTheFileEmptyBetweenWrites(t *testing.T) {
	dir := t.TempDir()
	lock, ok, err := openRunLock(dir, "build-sync")
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer lock.release()
	if err := lock.setPid(123456); err != nil {
		t.Fatal(err)
	}
	if err := lock.setPid(7); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(runLockPath(dir, "build-sync"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "7" {
		t.Errorf("a shorter pid must replace the longer one whole, got %q", got)
	}
}
