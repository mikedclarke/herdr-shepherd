//go:build unix

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"syscall"
	"time"
)

// errScriptTimeout marks the timeout kill, so callers can report it apart from
// a script that failed on its own.
var errScriptTimeout = errors.New("timed out")

// deferExitCode is EX_TEMPFAIL: the script is saying "not now, retry later"
// (e.g. a model slot is busy), which is a deferral, not a failure.
const deferExitCode = 75

// isDeferExit reports whether the script exited with the deferral code.
func isDeferExit(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == deferExitCode
}

// waitDelay bounds Wait once the shell has gone: a grandchild that daemonized
// still holds the output pipe, and Wait would otherwise block on it forever.
const waitDelay = 5 * time.Second

// runScriptOnce runs the action's command to completion, writing combined
// output to out. The command gets its own process group so a timeout kill
// reaches grandchildren, not just the shell.
func runScriptOnce(a *Action, out io.Writer) error {
	return runScriptTracked(a, out, nil)
}

// runScriptTracked is runScriptOnce with the child's pid handed to started as
// soon as there is one, for a caller that has to record which process the run
// belongs to.
func runScriptTracked(a *Action, out io.Writer, started func(pid int)) error {
	cmd := exec.Command("sh", "-c", a.Command)
	cmd.Dir = a.Dir()
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		return err
	}
	if started != nil {
		started(cmd.Process.Pid)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Duration(a.TimeoutMinutes) * time.Minute):
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			log.Printf("%s: kill process group: %v", a.Name, err)
		}
		<-done
		return fmt.Errorf("%w after %dm", errScriptTimeout, a.TimeoutMinutes)
	}
}
