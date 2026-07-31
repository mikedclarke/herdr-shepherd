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

// boardRow is one line of the board: a loaded action, a broken TOML file
// (which the loader already quarantines to its own file), or the trailing
// "+ new action" button row.
type boardRow struct {
	action  *Action
	errFile string
	errText string
	isNew   bool
}

func (r boardRow) sourceFile(actionsDir string) string {
	if r.action != nil {
		return r.action.SourceFile
	}
	return actionsDir + "/" + r.errFile
}

type tickMsg time.Time

type scriptDoneMsg struct {
	name   string
	err    error
	tail   string
	logErr error // the run-log append failed; the TUI owns the screen, so it reports here
}

type editorDoneMsg struct{ err error }

type agentStartedMsg struct {
	name   string
	ws     string
	err    error
	logErr error // the started record failed to append; reported here, not stderr
}

type boardModel struct {
	paths       paths
	rows        []boardRow
	st          *daemonState
	now         time.Time
	cursor      int
	top         int // first visible row when the list is taller than the terminal
	width       int
	height      int
	detail      string // action name shown in the detail view; "" = board
	history     []runRecord
	histName    string // action the cached history belongs to
	histSize    int64  // run-log size and mtime the cache was built from
	histMod     time.Time
	fingerprint string // actions-dir stat summary; unchanged means no re-parse
	status      string
	statusAt    time.Time
	running     map[string]bool   // manual runs this board started
	locked      map[string]bool   // run locks held by anyone, refreshed each reload
	locks       map[string]func() // release funcs for the runs above
	form        *formModel        // non-nil while the new-action form is open
}

// Board layout rows (see viewBoard): title 0, actions dir 1, blank 2, column
// header 3, action rows from 4; then a blank row, an optional status line, and
// the footer. footerY computes the footer's y to match viewBoard exactly.
const (
	boardRowsTop     = 4
	boardFooterLines = 2
)

func (m *boardModel) footerY() int {
	y := boardRowsTop + m.visibleRows() + 1
	if m.status != "" {
		y++
	}
	return y
}

// visibleRows is how many rows fit under the column header. A zero height —
// before the first WindowSizeMsg — means no limit.
func (m *boardModel) visibleRows() int {
	if m.height <= 0 {
		return len(m.rows)
	}
	avail := m.height - boardRowsTop - boardFooterLines
	if m.status != "" {
		avail--
	}
	if avail < 1 {
		avail = 1
	}
	if avail > len(m.rows) {
		return len(m.rows)
	}
	return avail
}

// scrollTop slides the window so the cursor stays in view; every click-target
// calculation goes through it, so the view and the mouse agree on which row is
// on which line.
func (m *boardModel) scrollTop() int {
	n := m.visibleRows()
	top := m.top
	if m.cursor < top {
		top = m.cursor
	}
	if m.cursor >= top+n {
		top = m.cursor - n + 1
	}
	if last := len(m.rows) - n; top > last {
		top = last
	}
	if top < 0 {
		top = 0
	}
	m.top = top
	return top
}

// footerSegments are the footer hints; each is also a click target that
// presses its key.
var footerSegments = []struct{ label, key string }{
	{"↑↓ move", ""},
	{"space pause/resume", " "},
	{"r run", "r"},
	{"enter details", "enter"},
	{"e edit", "e"},
	{"E toml", "E"},
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
	m := &boardModel{
		paths: p, st: readState(p.StateFile()), now: time.Now(),
		running: map[string]bool{}, locked: map[string]bool{}, locks: map[string]func(){},
	}
	m.reload()
	return m
}

func (m *boardModel) Init() tea.Cmd { return tickCmd() }

func tickCmd() tea.Cmd {
	return tea.Tick(boardRefresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *boardModel) reload() {
	if fp := actionsFingerprint(m.paths.ActionsDir()); fp == "" || fp != m.fingerprint {
		m.fingerprint = fp
		m.reloadActions()
	}
	m.st = readState(m.paths.StateFile())
	m.now = time.Now()
	m.locked = map[string]bool{}
	for _, r := range m.rows {
		if r.action != nil && runLockHeld(m.paths.StateDir, r.action.Name) {
			m.locked[r.action.Name] = true
		}
	}
	if m.detail != "" {
		m.history = m.historyFor(m.detail)
	}
}

func (m *boardModel) reloadActions() {
	selected := ""
	selectedNew := false
	if m.cursor < len(m.rows) {
		if a := m.rows[m.cursor].action; a != nil {
			selected = a.Name
		} else {
			selected = m.rows[m.cursor].errFile
		}
		selectedNew = m.rows[m.cursor].isNew
	}
	actions, fileErrs, err := LoadActions(m.paths.ActionsDir())
	if err != nil {
		m.rows = []boardRow{{errFile: "", errText: err.Error()}}
		return
	}
	rows := make([]boardRow, 0, len(actions)+len(fileErrs)+1)
	for _, a := range actions {
		rows = append(rows, boardRow{action: a})
	}
	for _, ferr := range fileErrs {
		file, text, _ := strings.Cut(ferr.Error(), ":")
		rows = append(rows, boardRow{errFile: strings.TrimSpace(file), errText: strings.TrimSpace(text)})
	}
	rows = append(rows, boardRow{isNew: true})
	m.rows = rows
	m.cursor = 0
	if selectedNew {
		m.cursor = len(rows) - 1
	} else if selected != "" {
		for i, r := range rows {
			if (r.action != nil && r.action.Name == selected) || (r.action == nil && r.errFile == selected) {
				m.cursor = i
				break
			}
		}
	}
}

// actionsFingerprint summarises the action files by name, size and mtime. An
// unchanged fingerprint means reload can keep the parsed rows; an unreadable
// directory returns "" so the parse is never skipped on a guess.
func actionsFingerprint(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return ""
		}
		fmt.Fprintf(&b, "%s %d %d\n", e.Name(), info.Size(), info.ModTime().UnixNano())
	}
	return b.String()
}

// historyFor caches the detail view's history against the run log's size and
// mtime: the 2s tick would otherwise re-read and re-parse an unchanged file.
func (m *boardModel) historyFor(name string) []runRecord {
	path := m.paths.RunLogFile()
	var size int64
	var mod time.Time
	if info, err := os.Stat(path); err == nil {
		size, mod = info.Size(), info.ModTime()
	}
	if m.histName == name && m.histSize == size && m.histMod.Equal(mod) {
		return m.history
	}
	m.histName, m.histSize, m.histMod = name, size, mod
	return actionHistory(path, name, historyDepth)
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
		m.releaseRun(msg.name)
		switch {
		case msg.err != nil:
			m.note(styleError.Render(fmt.Sprintf("%s failed: %v — %s", msg.name, msg.err, firstLine(msg.tail))))
		case msg.logErr != nil:
			m.note(styleError.Render(fmt.Sprintf("%s completed; run log: %v", msg.name, msg.logErr)))
		default:
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
		// The run itself is unwatched by design, so the lock only covers the
		// launch: it stops a double press opening two workspaces.
		delete(m.running, msg.name)
		m.releaseRun(msg.name)
		switch {
		case msg.err != nil:
			m.note(styleError.Render(fmt.Sprintf("%s: %v", msg.name, msg.err)))
		case msg.logErr != nil:
			m.note(styleError.Render(fmt.Sprintf("%s started in workspace %s; run log: %v", msg.name, msg.ws, msg.logErr)))
		default:
			m.note(fmt.Sprintf("%s started in workspace %s", msg.name, msg.ws))
		}
		return m, nil
	}

	if m.form != nil {
		// ctrl+c quits from anywhere, including a half-typed field.
		if key, ok := msg.(tea.KeyMsg); ok && key.String() == "ctrl+c" {
			return m, tea.Quit
		}
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
		case "e":
			if a := m.detailAction(); a != nil {
				m.form = newFormModelForAction(a, m.paths.ActionsDir())
			}
		case "E":
			if a := m.detailAction(); a != nil {
				return m, openEditor(a.SourceFile)
			}
		case "r":
			if m.selectDetailRow() {
				return m, m.runSelected()
			}
		case " ":
			if m.selectDetailRow() {
				return m, m.toggleSelected()
			}
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
		r := m.selectedRow()
		switch {
		case r == nil:
		case r.isNew:
			m.form = newFormModel(m.paths.ActionsDir())
		case r.action != nil:
			m.detail = r.action.Name
			m.history = m.historyFor(m.detail)
		}
	case "e":
		return m, m.editSelected()
	case "E":
		if r := m.selectedRow(); r != nil && !r.isNew {
			return m, openEditor(r.sourceFile(m.paths.ActionsDir()))
		}
	case "n":
		m.form = newFormModel(m.paths.ActionsDir())
	}
	return m, nil
}

// detailAction resolves the action the detail view is showing.
func (m *boardModel) detailAction() *Action {
	for _, r := range m.rows {
		if r.action != nil && r.action.Name == m.detail {
			return r.action
		}
	}
	return nil
}

// selectDetailRow points the cursor at the detail view's action so the shared
// run/toggle helpers act on it.
func (m *boardModel) selectDetailRow() bool {
	for i, r := range m.rows {
		if r.action != nil && r.action.Name == m.detail {
			m.cursor = i
			return true
		}
	}
	return false
}

// handleMouse: wheel scrolls the selection; click selects a row, click again
// opens details; the "+ new action" row and the footer hints act as buttons;
// the detail view has its own button bar. (Right-click never reaches the TUI —
// herdr keeps it for its pane context menu.)
func (m *boardModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}
	if m.detail != "" {
		if msg.Button != tea.MouseButtonLeft {
			return m, nil
		}
		if msg.Y == detailButtonsY {
			if key := detailButtonKeyAt(msg.X, m.detailPaused()); key != "" {
				return m.press(key)
			}
			return m, nil
		}
		return m.press("esc")
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		return m.press("up")
	case tea.MouseButtonWheelDown:
		return m.press("down")
	case tea.MouseButtonLeft:
		line := msg.Y - boardRowsTop
		row := line + m.scrollTop()
		if line >= 0 && line < m.visibleRows() && row < len(m.rows) {
			already := m.cursor == row
			m.cursor = row
			if m.rows[row].isNew || already {
				return m.press("enter")
			}
			return m, nil
		}
		if msg.Y == m.footerY() {
			if key := footerKeyAt(msg.X); key != "" {
				return m.press(key)
			}
		}
	}
	return m, nil
}

// The detail view's button bar sits directly under the title (row 1).
const detailButtonsY = 1

func detailButtons(paused bool) []struct{ label, key string } {
	pause := struct{ label, key string }{"[ pause ]", " "}
	if paused {
		pause = struct{ label, key string }{"[ resume ]", " "}
	}
	return []struct{ label, key string }{
		{"[ edit ]", "e"},
		{"[ run now ]", "r"},
		pause,
		{"[ back ]", "esc"},
	}
}

func detailButtonsText(paused bool) string {
	var labels []string
	for _, b := range detailButtons(paused) {
		labels = append(labels, b.label)
	}
	return strings.Join(labels, "  ")
}

func detailButtonKeyAt(x int, paused bool) string {
	start := 0
	for _, b := range detailButtons(paused) {
		end := start + len([]rune(b.label))
		if x >= start && x < end {
			return b.key
		}
		start = end + 2
	}
	return ""
}

func (m *boardModel) detailPaused() bool {
	if a := m.detailAction(); a != nil {
		return !a.IsEnabled()
	}
	return false
}

func (m *boardModel) toggleSelected() tea.Cmd {
	r := m.selectedRow()
	if r == nil || r.action == nil {
		m.rowHint(r)
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

// rowHint explains why an action key did nothing on a non-action row.
func (m *boardModel) rowHint(r *boardRow) {
	if r != nil && r.isNew {
		m.note("press enter (or click) to create a new action")
	} else {
		m.note("fix the file first (e to edit)")
	}
}

// runSelected fires the selected action by hand. Manual runs keep their CLI
// semantics: scripts run to completion here, agent sessions are handed to the
// user once started, and neither consults or updates the daemon's schedule.
func (m *boardModel) runSelected() tea.Cmd {
	r := m.selectedRow()
	if r == nil || r.action == nil {
		m.rowHint(r)
		return nil
	}
	a := r.action
	if m.running[a.Name] {
		m.note(fmt.Sprintf("%s is already running", a.Name))
		return nil
	}
	release, ok, err := tryRunLock(m.paths.StateDir, a.Name)
	if err != nil {
		m.note(styleError.Render(err.Error()))
		return nil
	}
	if !ok {
		m.locked[a.Name] = true
		m.note(fmt.Sprintf("%s is already running (another process)", a.Name))
		return nil
	}
	m.running[a.Name] = true
	m.locks[a.Name] = release
	logFile := m.paths.RunLogFile()
	if a.Kind == KindScript {
		m.note(fmt.Sprintf("running %s…", a.Name))
		return func() tea.Msg {
			out := &tailBuffer{max: outputTailMax}
			runErr := runScriptOnce(a, out)
			rec := runRecord{
				At: time.Now(), Action: a.Name, Kind: a.Kind,
				Status: "completed", Detail: out.String(), Trigger: triggerManual,
			}
			if runErr != nil {
				rec.Status, rec.Detail = "error", runErr.Error()
			}
			return scriptDoneMsg{
				name: a.Name, err: runErr, tail: out.String(),
				logErr: appendRunLog(logFile, rec),
			}
		}
	}
	m.note(fmt.Sprintf("starting %s…", a.Name))
	return func() tea.Msg {
		ws, err := startAgentRun(a)
		msg := agentStartedMsg{name: a.Name, ws: ws, err: err}
		if err == nil {
			msg.logErr = recordManualStart(a, ws)
		}
		return msg
	}
}

// releaseRun drops the run lock a manual run held; runs the board did not
// start have none.
func (m *boardModel) releaseRun(name string) {
	if release := m.locks[name]; release != nil {
		release()
		delete(m.locks, name)
	}
}

// editSelected opens the guided form for a valid action; broken files fall
// back to the raw editor (the form cannot load them). E always goes raw.
func (m *boardModel) editSelected() tea.Cmd {
	r := m.selectedRow()
	switch {
	case r == nil:
		return nil
	case r.isNew:
		m.form = newFormModel(m.paths.ActionsDir())
		return nil
	case r.action != nil:
		m.form = newFormModelForAction(r.action, m.paths.ActionsDir())
		return nil
	default:
		return openEditor(r.sourceFile(m.paths.ActionsDir()))
	}
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
	b.WriteString(styleDim.Render(contractPath(m.paths.ActionsDir())) + "\n\n")

	b.WriteString(styleDim.Render(m.clamp(fmt.Sprintf("  %-22s %-9s %-28s %-20s %s", "NAME", "KIND", "SCHEDULE", "LAST RUN", "NEXT RUN"))) + "\n")

	top := m.scrollTop()
	for i, r := range m.rows[top : top+m.visibleRows()] {
		line := m.renderRow(r)
		if top+i == m.cursor {
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
	if r.isNew {
		return styleDim.Render("+ new action…")
	}
	if r.action == nil {
		return styleError.Render(m.clamp(fmt.Sprintf("✗ %s %s",
			pad(truncate(r.errFile, 22), 22), truncate(r.errText, 60))))
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
	// The lock is the only cross-process signal, so a scheduled run the daemon
	// started shows here too.
	if m.running[a.Name] || m.locked[a.Name] {
		last = "running…"
	}
	next := fmtTime(nextRun(a, m.st.lastRun(a.Name), m.now), "")
	if !a.IsEnabled() {
		next = "paused"
	}
	line := m.clamp(fmt.Sprintf("%s %s %s %s %s %s",
		dot, pad(truncate(a.Name, 22), 22), pad(string(a.Kind), 9),
		pad(truncate(scheduleSummary(a), 28), 28), pad(truncate(last, 20), 20), next))
	if !a.IsEnabled() {
		return styleDisabled.Render(line)
	}
	return line
}

// clamp cuts a row to the terminal width; a row that wraps pushes every line
// below it down and breaks the click-target arithmetic.
func (m *boardModel) clamp(line string) string {
	if m.width <= 0 {
		return line
	}
	return lipgloss.NewStyle().MaxWidth(m.width).Render(line)
}

func statusGlyph(status string) string {
	switch status {
	case "completed":
		return styleEnabled.Render("✓")
	case "error":
		return styleError.Render("✗")
	case "started":
		return styleDim.Render("▸")
	case "attention", "cancelled", "interrupted":
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
	b.WriteString(styleHeader.Render(a.Name) + styleDim.Render("  ·  "+string(a.Kind)) + "\n")
	b.WriteString(detailButtonsText(!a.IsEnabled()) + "\n\n")
	field := func(k, v string) {
		if v != "" {
			b.WriteString(styleDim.Render(fmt.Sprintf("  %-12s", k)) + v + "\n")
		}
	}
	field("file", contractPath(a.SourceFile))
	field("directory", a.Directory)
	field("schedule", scheduleDetail(a))
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
	b.WriteString("\n" + styleDim.Render("e edit · E toml · r run · space pause/resume · esc back"))
	return b.String()
}

// truncate and pad work in terminal cells, not bytes: %-Ns and s[:n] on a name
// with multi-byte or wide runes mis-column every field after it, and can cut a
// rune in half.
func truncate(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	if n <= 1 {
		return string(r[:n])
	}
	var b strings.Builder
	w := 0
	for _, c := range r {
		cw := lipgloss.Width(string(c))
		if w+cw > n-1 {
			break
		}
		b.WriteRune(c)
		w += cw
	}
	return b.String() + "…"
}

func pad(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
