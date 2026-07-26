package main

import (
	"fmt"
	"os"
	"os/exec"
)

const version = "0.1.0"

func execCommand(a *Action) *exec.Cmd {
	cmd := exec.Command("sh", "-c", a.Command)
	cmd.Dir = a.Dir()
	return cmd
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: herdr-shepherd <command>

  daemon         Run the scheduler (herdr starts this via the plugin manifest)
  list           List actions with their schedules and next runs
  run <name>     Fire an action now
  status         Show daemon liveness and the next scheduled run
    --notify     Also show the status as a herdr notification
  version        Print the version`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "daemon":
		err = runDaemon()
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
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "herdr-shepherd:", err)
		os.Exit(1)
	}
}
