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

// waitDelay bounds Wait once the shell has gone: a grandchild that daemonized
// still holds the output pipe, and Wait would otherwise block on it forever.
const waitDelay = 5 * time.Second

// runScriptOnce runs the action's command to completion, writing combined
// output to out. The command gets its own process group so a timeout kill
// reaches grandchildren, not just the shell.
func runScriptOnce(a *Action, out io.Writer) error {
	cmd := exec.Command("sh", "-c", a.Command)
	cmd.Dir = a.Dir()
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		return err
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
