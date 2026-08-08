package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

const version = "0.6.1"

func usage(w *os.File) {
	fmt.Fprintln(w, `usage: herdr-shepherd <command>

  daemon         Run the scheduler in the foreground
    --detach     Spawn the scheduler detached and exit (the manifest startup
                 hook uses this; logs go to shepherd.log in the state dir)
  board          Open the status board (a live TUI: pause/resume, run now,
                 details; opens as a herdr pane when invoked without a TTY)
  list           List actions with their schedules and next runs
  run <name>     Fire an action now
  status         Show daemon liveness and the next scheduled run
    --notify     Also show the status as a herdr notification
  version        Print the version`)
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "daemon":
		if len(os.Args) > 2 && os.Args[2] == "--detach" {
			err = spawnDetached()
		} else {
			err = runDaemon()
		}
	case "board":
		err = cmdBoard()
	case "board-ui":
		// Internal: the manifest's pane entrypoint (herdr hosts the TUI).
		err = runBoardUI()
	case "list":
		err = cmdList()
	case "run":
		if len(os.Args) < 3 {
			err = fmt.Errorf("run: action name required")
		} else {
			err = cmdRun(os.Args[2])
		}
	case "status":
		notify := len(os.Args) > 2 && os.Args[2] == "--notify"
		err = cmdStatus(notify)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "herdr-shepherd: unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "herdr-shepherd:", err)
		os.Exit(1)
	}
}

// spawnDetached starts the scheduler in its own session and returns, so the
// manifest's startup hook conforms to herdr's one-shot contract (and gets a
// clean completion record in the plugin log).
func spawnDetached() error {
	p := resolvePaths()
	if err := os.MkdirAll(p.StateDir, 0o755); err != nil {
		return err
	}
	// The redirect only has to catch output from a crash; the daemon opens
	// the same file itself and rotates it as it runs.
	logf, err := os.OpenFile(p.LogFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Env = append(os.Environ(), logPathEnv+"="+p.LogFile())
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Printf("shepherd daemon spawned (pid %d), log: %s\n", cmd.Process.Pid, p.LogFile())
	return nil
}
