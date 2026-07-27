package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// enabledLine matches a top-level `enabled = ...` assignment. Only lines before
// the first [section] header are considered top-level; a same-named key inside
// a table must never be touched.
var enabledLine = regexp.MustCompile(`^(\s*)enabled\s*=`)

var sectionLine = regexp.MustCompile(`^\s*\[`)

// setActionEnabled rewrites path so its top-level `enabled` key equals v,
// leaving every other line — comments, formatting, ordering — exactly as the
// user wrote them. The write is atomic (tmp + rename), so the daemon's 30s
// re-read never sees a half-written file.
func setActionEnabled(path string, v bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	value := fmt.Sprintf("enabled = %t", v)
	lines := strings.Split(string(data), "\n")

	replaced := false
	topLevelEnd := len(lines)
	nameAt := -1
	for i, line := range lines {
		if sectionLine.MatchString(line) {
			topLevelEnd = i
			break
		}
		if m := enabledLine.FindStringSubmatch(line); m != nil && !replaced {
			lines[i] = m[1] + value
			replaced = true
		}
		if nameAt < 0 && strings.HasPrefix(strings.TrimSpace(line), "name") {
			nameAt = i
		}
	}
	if !replaced {
		at := topLevelEnd
		if nameAt >= 0 {
			at = nameAt + 1
		}
		lines = append(lines[:at], append([]string{value}, lines[at:]...)...)
	}

	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".enabled-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(lines, "\n")); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

const newActionTemplate = `# New shepherd action. Set the fields, flip enabled to true, and the daemon
# picks it up within 30 seconds. One TOML file per action; kinds: heartbeat
# (prompt every N minutes), routine (prompt on a schedule), script (command on
# a schedule). See the README for every field.
name = "%s"
kind = "routine"
directory = "~"
enabled = false
prompt = "Describe the session's task here. Exit when done."
# cli = "claude"                # claude | codex
# permission_mode = "default"   # default | auto | skip
# auto_close = false

[schedule]
preset = "weekdays"             # daily | weekdays | days | monthly | cron
hours = [9]
minute = 0
`

// newActionFile writes a commented template into dir under an unused name and
// returns its path; the board opens it in $EDITOR.
func newActionFile(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for i := 0; i < 100; i++ {
		name := "new-action"
		if i > 0 {
			name = fmt.Sprintf("new-action-%d", i+1)
		}
		path := filepath.Join(dir, name+".toml")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := fmt.Fprintf(f, newActionTemplate, name)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(path)
			return "", werr
		}
		return path, nil
	}
	return "", fmt.Errorf("no free new-action name in %s", dir)
}
