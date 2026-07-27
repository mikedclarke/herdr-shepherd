package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
	isatty "github.com/mattn/go-isatty"
)

// cmdBoard opens the status board. From a shell (stdin is a terminal) it runs
// the TUI right here; invoked as the manifest action (server-side, no
// terminal) it asks herdr to host the `board` pane entrypoint instead — the
// herdr-plus pattern, so herdr creates and tears down the pane.
func cmdBoard() error {
	if isatty.IsTerminal(os.Stdin.Fd()) {
		return runBoardUI()
	}
	herdr := os.Getenv("HERDR_BIN_PATH")
	if herdr == "" {
		herdr = "herdr"
	}
	cmd := exec.Command(herdr, "plugin", "pane", "open",
		"--plugin", pluginID,
		"--entrypoint", "board",
		"--placement", "zoomed",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("open board pane: %w", err)
	}
	return nil
}

func runBoardUI() error {
	p := resolvePaths()
	m := newBoardModel(p)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

// actionHistory returns the newest-first run records for one action, at most n
// of them. Corrupt lines are skipped — a half-appended record must not blank
// the whole history view.
func actionHistory(path, name string, n int) []runRecord {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var recs []runRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var r runRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if r.Action == name {
			recs = append(recs, r)
		}
	}
	if len(recs) > n {
		recs = recs[len(recs)-n:]
	}
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	return recs
}
