//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunScriptOnceCapturesOutput(t *testing.T) {
	out := &tailBuffer{max: outputTailMax}
	a := scriptAction("build-sync", "echo out; echo err >&2")
	if err := runScriptOnce(a, out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "out") || !strings.Contains(got, "err") {
		t.Errorf("both streams should be captured, got %q", got)
	}
}

func TestRunScriptOnceReturnsExitError(t *testing.T) {
	out := &tailBuffer{max: outputTailMax}
	err := runScriptOnce(scriptAction("build-sync", "exit 3"), out)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("got %v", err)
	}
}

func TestRunScriptOnceRunsInTheActionDirectory(t *testing.T) {
	dir := t.TempDir()
	out := &tailBuffer{max: outputTailMax}
	a := scriptAction("build-sync", "pwd")
	a.Directory = dir
	if err := runScriptOnce(a, out); err != nil {
		t.Fatal(err)
	}
	// macOS resolves the temp dir through /private; compare the leaf.
	if !strings.HasSuffix(out.String(), filepath.Base(dir)) {
		t.Errorf("got %q, want a path ending in %q", out.String(), filepath.Base(dir))
	}
}

func TestRunScriptOnceTimeoutKillsTheGroup(t *testing.T) {
	// A background child shares the process group, so the timeout kill
	// reaches it too — and releasing the output pipe is what lets Wait
	// return. Zero minutes makes the timeout immediate.
	dir := t.TempDir()
	marker := filepath.Join(dir, "survived")
	a := &Action{
		Name: "build-sync", Kind: KindScript, Directory: dir,
		Command: "(sleep 2; touch " + marker + ") & sleep 60",
	}
	out := &tailBuffer{max: outputTailMax}
	start := time.Now()
	err := runScriptOnce(a, out)
	elapsed := time.Since(start)
	if !errors.Is(err, errScriptTimeout) {
		t.Fatalf("got %v", err)
	}
	if elapsed > waitDelay {
		t.Errorf("a killed script should return promptly, took %s", elapsed)
	}
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the whole process group should have been killed")
	}
}

func TestRunScriptOnceStartFailureIsReported(t *testing.T) {
	a := scriptAction("build-sync", "true")
	a.Directory = filepath.Join(t.TempDir(), "does-not-exist")
	if err := runScriptOnce(a, &tailBuffer{max: outputTailMax}); err == nil {
		t.Fatal("a script that cannot start must report an error")
	}
}
