package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	boardRefresh   = 2 * time.Second
	statusLinger   = 5 * time.Second
	historyDepth   = 8
	staleHeartbeat = 3 * tickInterval
)

var (
	styleHeader   = lipgloss.NewStyle().Bold(true)
	styleDim      = lipgloss.NewStyle().Faint(true)
	styleSelected = lipgloss.NewStyle().Reverse(true)
	styleEnabled  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleDisabled = lipgloss.NewStyle().Faint(true)
	styleError    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleAttn     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// boardRow is one line of the board: a loaded action, or a broken TOML file
// (which the loader already quarantines to its own file).
type boardRow struct {
	action  *Action
	errFile string
	errText string
}

func (r boardRow) sourceFile(actionsDir string) string {
	if r.action != nil {
		return r.action.SourceFile
	}
	return actionsDir + "/" + r.errFile
}

type tickMsg time.Time

type scriptDoneMsg struct {
	name string
	err  error
	tail string
}

type editorDoneMsg struct{ err error }

type agentStartedMsg struct {
	name string
	ws   string
	err  error
}

type boardModel struct {
	paths    paths
	rows     []boardRow
	st       *daemonState
	now      time.Time
	cursor   int
	width    int
	height   int
	detail   string // action name shown in the detail view; "" = board
	history  []runRecord
	status   string
	statusAt time.Time
	running  map[string]bool // manual script runs in flight
	form     *formModel      // non-nil while the new-action form is open
}

// Board layout rows (see viewBoard): title 0, actions dir 1, blank 2, column
// header 3, action rows from 4. The footer's y depends on row count and the
// status line; footerY computes it to match viewBoard exactly.
const boardRowsTop = 4

func (m *boardModel) footerY() int {
	y := boardRowsTop + len(m.rows) + 1
	if len(m.rows) == 0 {
		y++ // the "no actions" placeholder line
	}
	if m.status != "" {
		y++
	}
	return y
}

// footerSegments are the footer hints; each is also a click target that
// presses its key.
var footerSegments = []struct{ label, key string }{
	{"↑↓ move", ""},
	{"space pause/resume", " "},
	{"r run", "r"},
	{"enter details", "enter"},
	{"e edit", "e"},
	{"n new", "n"},
	{"q quit", "q"},
}

func footerText() string {
	labels := make([]string, len(footerSegments))
	for i, s := range footerSegments {
		labels[i] = s.label
	}
	return strings.Join(labels, " · ")
}

// footerKeyAt maps a click x-position on the footer line to its segment's key.
func footerKeyAt(x int) string {
	start := 0
	for _, s := range footerSegments {
		end := start + len([]rune(s.label))
		if x >= start && x < end {
			return s.key
		}
		start = end + 3 // " · "
	}
	return ""
}

func newBoardModel(p paths) *boardModel {
	m := &boardModel{paths: p, st: loadState(p.StateFile()), now: time.Now(), running: map[string]bool{}}
	m.reload()
	return m
}

func (m *boardModel) Init() tea.Cmd { return tickCmd() }

func tickCmd() tea.Cmd {
	return tea.Tick(boardRefresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *boardModel) reload() {
	selected := ""
	if m.cursor < len(m.rows) {
		if a := m.rows[m.cursor].action; a != nil {
			selected = a.Name
		} else {
			selected = m.rows[m.cursor].errFile
		}
	}
	actions, fileErrs, err := LoadActions(m.paths.ActionsDir())
	if err != nil {
		m.rows = []boardRow{{errFile: "", errText: err.Error()}}
		return
	}
	rows := make([]boardRow, 0, len(actions)+len(fileErrs))
	for _, a := range actions {
		rows = append(rows, boardRow{action: a})
	}
	for _, ferr := range fileErrs {
		file, text, _ := strings.Cut(ferr.Error(), ":")
		rows = append(rows, boardRow{errFile: strings.TrimSpace(file), errText: strings.TrimSpace(text)})
	}
	m.rows = rows
	m.st = loadState(m.paths.StateFile())
	m.now = time.Now()
	m.cursor = 0
	for i, r := range rows {
		if (r.action != nil && r.action.Name == selected) || (r.action == nil && r.errFile == selected) {
			m.cursor = i
			break
		}
	}
	if m.detail != "" {
		m.history = actionHistory(m.paths.RunLogFile(), m.detail, historyDepth)
	}
}

func (m *boardModel) selectedRow() *boardRow {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return nil
	}
	return &m.rows[m.cursor]
}

func (m *boardModel) note(s string) {
	m.status = s
	m.statusAt = time.Now()
}

func (m *boardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		if m.form == nil {
			m.reload()
		}
		if m.status != "" && time.Since(m.statusAt) > statusLinger {
			m.status = ""
		}
		return m, tickCmd()
	case scriptDoneMsg:
		delete(m.running, msg.name)
		if msg.err != nil {
			m.note(styleError.Render(fmt.Sprintf("%s failed: %v — %s", msg.name, msg.err, firstLine(msg.tail))))
		} else {
			m.note(fmt.Sprintf("%s completed", msg.name))
		}
		return m, nil
	case editorDoneMsg:
		if msg.err != nil {
			m.note(styleError.Render(fmt.Sprintf("editor: %v", msg.err)))
		}
		m.reload()
		return m, nil
	case agentStartedMsg:
		if msg.err != nil {
			m.note(styleError.Render(fmt.Sprintf("%s: %v", msg.name, msg.err)))
		} else {
			m.note(fmt.Sprintf("%s started in workspace %s", msg.name, msg.ws))
		}
		return m, nil
	}

	if m.form != nil {
		if done, saved := m.form.update(msg); done {
			m.form = nil
			m.reload()
			if saved {
				m.note("action created (paused) — space resumes it")
			}
		}
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.press(msg.String())
	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}

func (m *boardModel) press(key string) (tea.Model, tea.Cmd) {
	if m.detail != "" {
		switch key {
		case "q", "esc", "enter":
			m.detail = ""
			m.history = nil
		case "ctrl+c":
			return m, tea.Quit
		}
		return m, nil
	}
	switch key {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case " ":
		return m, m.toggleSelected()
	case "r":
		return m, m.runSelected()
	case "enter":
		if r := m.selectedRow(); r != nil && r.action != nil {
			m.detail = r.action.Name
			m.history = actionHistory(m.paths.RunLogFile(), m.detail, historyDepth)
		}
	case "e":
		return m, m.editSelected()
	case "n":
		m.form = newFormModel(m.paths.ActionsDir())
	}
	return m, nil
}

// handleMouse: wheel scrolls the selection; left click selects a row (a click
// on the already-selected row opens details, as does right click); the footer
// hints double as buttons; any click leaves the detail view.
func (m *boardModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}
	if m.detail != "" {
		if msg.Button == tea.MouseButtonLeft || msg.Button == tea.MouseButtonRight {
			return m.press("esc")
		}
		return m, nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		return m.press("up")
	case tea.MouseButtonWheelDown:
		return m.press("down")
	case tea.MouseButtonLeft, tea.MouseButtonRight:
		row := msg.Y - boardRowsTop
		if row >= 0 && row < len(m.rows) {
			already := m.cursor == row
			m.cursor = row
			if msg.Button == tea.MouseButtonRight || already {
				return m.press("enter")
			}
			return m, nil
		}
		if msg.Button == tea.MouseButtonLeft && msg.Y == m.footerY() {
			if key := footerKeyAt(msg.X); key != "" {
				return m.press(key)
			}
		}
	}
	return m, nil
}

func (m *boardModel) toggleSelected() tea.Cmd {
	r := m.selectedRow()
	if r == nil || r.action == nil {
		m.note("fix the file first (e to edit)")
		return nil
	}
	target := !r.action.IsEnabled()
	if err := setActionEnabled(r.action.SourceFile, target); err != nil {
		m.note(styleError.Render(err.Error()))
		return nil
	}
	verb := "paused"
	if target {
		verb = "resumed"
	}
	m.note(fmt.Sprintf("%s %s", verb, r.action.Name))
	m.reload()
	return nil
}

func (m *boardModel) runSelected() tea.Cmd {
	r := m.selectedRow()
	if r == nil || r.action == nil {
		m.note("fix the file first (e to edit)")
		return nil
	}
	a := r.action
	if m.running[a.Name] {
		m.note(fmt.Sprintf("%s is already running", a.Name))
		return nil
	}
	if a.Kind == KindScript {
		m.running[a.Name] = true
		m.note(fmt.Sprintf("running %s…", a.Name))
		return func() tea.Msg {
			out := &tailBuffer{max: outputTailMax}
			cmd := exec.Command("sh", "-c", a.Command)
			cmd.Dir = a.Dir()
			cmd.Stdout = out
			cmd.Stderr = out
			err := cmd.Run()
			return scriptDoneMsg{name: a.Name, err: err, tail: out.String()}
		}
	}
	m.note(fmt.Sprintf("starting %s…", a.Name))
	return func() tea.Msg {
		ws, err := startAgentRun(a)
		return agentStartedMsg{name: a.Name, ws: ws, err: err}
	}
}

func (m *boardModel) editSelected() tea.Cmd {
	r := m.selectedRow()
	if r == nil {
		return nil
	}
	return openEditor(r.sourceFile(m.paths.ActionsDir()))
}

func openEditor(path string) tea.Cmd {
	c := exec.Command("sh", "-c", editorCommand()+" "+shellQuote(path))
	return tea.ExecProcess(c, func(err error) tea.Msg { return editorDoneMsg{err: err} })
}

// editorCommand honours $VISUAL/$EDITOR; with neither set it prefers nano,
// which keeps its save/exit keys on screen — never strand a user in vi they
// didn't ask for.
func editorCommand() string {
	if e := os.Getenv("VISUAL"); e != "" {
		return e
	}
	if e := os.Getenv("EDITOR"); e != "" {
		return e
	}
	if _, err := exec.LookPath("nano"); err == nil {
		return "nano"
	}
	return "vi"
}

func (m *boardModel) View() string {
	if m.form != nil {
		return m.form.View()
	}
	if m.detail != "" {
		return m.viewDetail()
	}
	return m.viewBoard()
}

func (m *boardModel) viewBoard() string {
	var b strings.Builder

	beat := m.st.heartbeatAt()
	daemon := styleError.Render("daemon not running")
	if !beat.IsZero() && time.Since(beat) < staleHeartbeat {
		daemon = styleEnabled.Render("daemon running")
	}
	enabled, total := 0, 0
	for _, r := range m.rows {
		if r.action == nil {
			continue
		}
		total++
		if r.action.IsEnabled() {
			enabled++
		}
	}
	b.WriteString(styleHeader.Render("Shepherd "+version) +
		styleDim.Render("  ·  ") + daemon +
		styleDim.Render(fmt.Sprintf("  ·  %d/%d enabled", enabled, total)) + "\n")
	b.WriteString(styleDim.Render(m.paths.ActionsDir()) + "\n\n")

	b.WriteString(styleDim.Render(fmt.Sprintf("  %-22s %-9s %-22s %-20s %s", "NAME", "KIND", "SCHEDULE", "LAST RUN", "NEXT RUN")) + "\n")

	if len(m.rows) == 0 {
		b.WriteString(styleDim.Render("  no actions — press n to create one") + "\n")
	}
	for i, r := range m.rows {
		line := m.renderRow(r)
		if i == m.cursor {
			line = styleSelected.Render(line)
		}
		b.WriteString(line + "\n")
	}

	b.WriteString("\n")
	if m.status != "" {
		b.WriteString(m.status + "\n")
	}
	b.WriteString(styleDim.Render(footerText()))
	return b.String()
}

func (m *boardModel) renderRow(r boardRow) string {
	if r.action == nil {
		return styleError.Render(fmt.Sprintf("✗ %-22s %s", truncate(r.errFile, 22), truncate(r.errText, 60)))
	}
	a := r.action
	dot := styleEnabled.Render("●")
	if !a.IsEnabled() {
		dot = styleDisabled.Render("○")
	}
	last := fmtTime(m.st.lastRun(a.Name), "")
	if s := m.st.lastStatus(a.Name); s != "" && last != "-" {
		last += " " + statusGlyph(s)
	}
	if m.running[a.Name] {
		last = "running…"
	}
	next := fmtTime(nextRun(a, m.st.lastRun(a.Name), m.now), "")
	if !a.IsEnabled() {
		next = "paused"
	}
	line := fmt.Sprintf("%s %-22s %-9s %-22s %-20s %s",
		dot, truncate(a.Name, 22), a.Kind, truncate(scheduleSummary(a), 22), truncate(last, 20), next)
	if !a.IsEnabled() {
		return styleDisabled.Render(line)
	}
	return line
}

func statusGlyph(status string) string {
	switch status {
	case "completed":
		return styleEnabled.Render("✓")
	case "error":
		return styleError.Render("✗")
	case "attention", "cancelled":
		return styleAttn.Render("!")
	}
	return status
}

func (m *boardModel) viewDetail() string {
	var a *Action
	for _, r := range m.rows {
		if r.action != nil && r.action.Name == m.detail {
			a = r.action
			break
		}
	}
	if a == nil {
		return styleDim.Render("action removed — press esc")
	}
	var b strings.Builder
	b.WriteString(styleHeader.Render(a.Name) + styleDim.Render("  ·  "+string(a.Kind)) + "\n\n")
	field := func(k, v string) {
		if v != "" {
			b.WriteString(styleDim.Render(fmt.Sprintf("  %-12s", k)) + v + "\n")
		}
	}
	field("file", a.SourceFile)
	field("directory", a.Directory)
	field("schedule", scheduleSummary(a))
	enabled := "yes"
	if !a.IsEnabled() {
		enabled = "no (paused)"
	}
	field("enabled", enabled)
	if a.Kind == KindScript {
		field("command", truncate(a.Command, 80))
		field("timeout", fmt.Sprintf("%dm", a.TimeoutMinutes))
	} else {
		field("cli", a.CLI)
		field("model", a.Model)
		field("permissions", a.PermissionMode)
		field("watch", fmt.Sprintf("%dm", a.WatchMinutes))
		field("auto close", fmt.Sprintf("%t", a.AutoClose))
		field("prompt", truncate(strings.ReplaceAll(a.Prompt, "\n", " "), 100))
	}

	b.WriteString("\n" + styleHeader.Render("Recent runs") + "\n")
	if len(m.history) == 0 {
		b.WriteString(styleDim.Render("  none recorded") + "\n")
	}
	for _, rec := range m.history {
		b.WriteString(fmt.Sprintf("  %s  %s %-10s %s\n",
			styleDim.Render(rec.At.Format("Mon 02 Jan 15:04")),
			statusGlyph(rec.Status), rec.Status,
			styleDim.Render(truncate(firstLine(rec.Detail), 60))))
	}
	b.WriteString("\n" + styleDim.Render("esc back · q close"))
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
