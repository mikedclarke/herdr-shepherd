package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
	isatty "github.com/mattn/go-isatty"
)

// cmdBoard opens the status board. From a shell (both stdin and stdout are a
// terminal) it runs the TUI right here; invoked as the manifest action
// (server-side, no terminal) or with its output piped it asks herdr to host
// the `board` pane entrypoint instead — the herdr-plus pattern, so herdr
// creates and tears down the pane.
func cmdBoard() error {
	if isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd()) {
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
	// Mouse cell motion: herdr forwards clicks and wheel events to the pane
	// once we ask for them, so the board works by touch as well as keys.
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

// historyTailBytes bounds a history read. The board re-reads on every tick and
// the run log grows to megabytes between rotations, so only the tail is
// parsed; n records never span more than this.
const historyTailBytes = 64 << 10

// actionHistory returns the newest-first run records for one action, at most n
// of them, reaching back into the rotated log when the current one holds
// fewer. Corrupt lines are skipped — a half-appended record must not blank the
// whole history view.
func actionHistory(path, name string, n int) []runRecord {
	recs := runsFor(readRunTail(path), name)
	if len(recs) < n {
		recs = append(runsFor(readRunTail(path+".1"), name), recs...)
	}
	if len(recs) > n {
		recs = recs[len(recs)-n:]
	}
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	return recs
}

func runsFor(recs []runRecord, name string) []runRecord {
	var out []runRecord
	for _, r := range recs {
		if r.Action == name {
			out = append(out, r)
		}
	}
	return out
}

// readRunTail parses the last historyTailBytes of a run log, oldest first. The
// first line after a seek is a fragment of the record that straddles the
// boundary, so it is dropped.
func readRunTail(path string) []runRecord {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	partial := false
	if info.Size() > historyTailBytes {
		if _, err := f.Seek(-historyTailBytes, io.SeekEnd); err != nil {
			return nil
		}
		partial = true
	}
	var recs []runRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if partial {
			partial = false
			continue
		}
		var r runRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		recs = append(recs, r)
	}
	return recs
}
