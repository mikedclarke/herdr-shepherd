package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// runFunc runs one program and returns what it printed and its exit code. err
// is set only when the program could not be run or overran its deadline; a
// nonzero exit is not an error here.
type runFunc func(ctx context.Context, name string, args ...string) (stdout []byte, exitCode int, err error)

// coreClient drives the new Herdr: herdr-core (hc) owns the terminals and the
// desktop app (herdr-app) shows them as spaces and tabs. A pane id here is an
// hc terminal address, which is opaque (t19 is followed by t1a).
type coreClient struct {
	hc       string
	app      string
	stateDir string
	run      runFunc
	// ids makes the random hex in tab ids and terminal names.
	ids func(n int) string

	// Timing knobs; newCoreClient sets the real values, tests shrink them.
	poll    time.Duration
	tabWait time.Duration
	now     func() time.Time
}

const (
	corePollInterval = 500 * time.Millisecond
	coreTabWait      = 8 * time.Second
	notifyMaxAge     = 24 * time.Hour
	defaultShell     = "/bin/zsh"
	daemonTerminal   = "shepherd"
	sendDeadline     = 90 * time.Second
)

func newCoreClient(stateDir string) (*coreClient, error) {
	hc := findProgram("hc")
	if hc == "" {
		return nil, errors.New("hc (herdr-core) not found on PATH or in ~/.local/bin")
	}
	app := findProgram("herdr-app")
	if app == "" {
		return nil, errors.New("herdr-app not found on PATH or in ~/.local/bin")
	}
	return &coreClient{
		hc: hc, app: app, stateDir: stateDir, run: runProgram, ids: randomHex,
		poll: corePollInterval, tabWait: coreTabWait, now: time.Now,
	}, nil
}

// findProgram looks on PATH, then in ~/.local/bin, where both programs
// install their links; a login job's PATH often lacks that folder.
func findProgram(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	p := filepath.Join(home, ".local", "bin", name)
	if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return p
	}
	return ""
}

// runProgram is the real runner. HC_TERMINAL_ID and HC_SCOPE are dropped: with
// them hc send wraps the text in <hc from> tags and a started terminal becomes
// the caller's child. hc prints its errors on stderr, so a failed call's
// stderr stands in for an empty stdout.
func runProgram(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "HC_TERMINAL_ID=") || strings.HasPrefix(kv, "HC_SCOPE=")
	})
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, -1, fmt.Errorf("%s %s: %w", filepath.Base(name), strings.Join(args, " "), ctx.Err())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		out := stdout.Bytes()
		if len(bytes.TrimSpace(out)) == 0 {
			out = stderr.Bytes()
		}
		return out, exitErr.ExitCode(), nil
	}
	if err != nil {
		return nil, -1, err
	}
	return stdout.Bytes(), 0, nil
}

// call runs one program call under callDeadline and turns a failed exit into
// a herdrError: hc's is {"error":{"kind","message"}}, herdr-app's
// {"error":"message"}.
func (c *coreClient) call(name string, args ...string) ([]byte, error) {
	return c.callWithin(callDeadline, name, args...)
}

func (c *coreClient) callWithin(deadline time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	out, code, err := c.run(ctx, name, args...)
	if err != nil {
		return nil, err
	}
	if code == 0 {
		return out, nil
	}
	var reply struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(out, &reply) == nil && len(reply.Error) > 0 {
		var kinded struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		}
		if json.Unmarshal(reply.Error, &kinded) == nil && kinded.Kind != "" {
			return nil, &herdrError{Code: kinded.Kind, Message: kinded.Message}
		}
		var msg string
		if json.Unmarshal(reply.Error, &msg) == nil {
			return nil, &herdrError{Code: "app_error", Message: msg}
		}
	}
	return nil, fmt.Errorf("%s %s: exit %d: %s", filepath.Base(name), strings.Join(args, " "), code,
		tailString(strings.TrimSpace(string(out)), 200))
}

func (c *coreClient) hcJSON(out any, args ...string) error {
	data, err := c.call(c.hc, args...)
	if err != nil || out == nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode hc %s reply: %w", args[0], err)
	}
	return nil
}

// appDo runs one herdr-app command with its arguments as a JSON object.
func (c *coreClient) appDo(out any, command string, params any) error {
	arg, err := json.Marshal(params)
	if err != nil {
		return err
	}
	data, err := c.call(c.app, "do", command, string(arg))
	if err != nil || out == nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode herdr-app %s reply: %w", command, err)
	}
	return nil
}

type appListing struct {
	Spaces []appSpace `json:"spaces"`
}

type appSpace struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Active bool     `json:"active"`
	Tabs   []appTab `json:"tabs"`
}

type appTab struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Active    bool          `json:"active"`
	Terminals []appTerminal `json:"terminals"`
}

type appTerminal struct {
	Terminal string `json:"terminal"`
	Pane     string `json:"pane"`
}

func (l appListing) findTab(tabID string) *appTab {
	for i := range l.Spaces {
		for j := range l.Spaces[i].Tabs {
			if l.Spaces[i].Tabs[j].ID == tabID {
				return &l.Spaces[i].Tabs[j]
			}
		}
	}
	return nil
}

// hcTerminal is the part of `hc get` and `hc run --bg` replies the client
// reads. Status is null while no agent reports.
type hcTerminal struct {
	Address string `json:"address"`
	State   string `json:"state"`
	Status  *struct {
		State string `json:"state"`
	} `json:"status"`
}

func (c *coreClient) list() (appListing, error) {
	var l appListing
	data, err := c.call(c.app, "ls")
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(data, &l); err != nil {
		return l, fmt.Errorf("decode herdr-app ls: %w", err)
	}
	return l, nil
}

func (c *coreClient) workspaceList() ([]workspaceInfo, error) {
	l, err := c.list()
	if err != nil {
		return nil, err
	}
	out := make([]workspaceInfo, 0, len(l.Spaces))
	for i, s := range l.Spaces {
		out = append(out, workspaceInfo{WorkspaceID: s.ID, Label: s.Name, Number: i + 1})
	}
	return out, nil
}

type spaceNewArgs struct {
	Name   string `json:"name"`
	Folder string `json:"folder"`
	Tab    string `json:"tab"`
}

type idArg struct {
	ID string `json:"id"`
}

// workspaceCreate makes the space, puts the person's screen back where it
// was (space.new shows the new space), and opens the run's tab in it with env.
// space.new's own first tab cannot carry env, so it is closed again.
func (c *coreClient) workspaceCreate(cwd, label string, env map[string]string) (workspaceID, tabID, paneID string, err error) {
	var prevSpace, prevTab string
	if l, lerr := c.list(); lerr == nil {
		for _, s := range l.Spaces {
			if !s.Active {
				continue
			}
			prevSpace = s.ID
			for _, t := range s.Tabs {
				if t.Active {
					prevTab = t.ID
				}
			}
		}
	}
	var created struct {
		Space    string `json:"space"`
		Tab      string `json:"tab"`
		Terminal string `json:"terminal"`
	}
	if err := c.appDo(&created, "space.new", spaceNewArgs{Name: label, Folder: cwd, Tab: "1"}); err != nil {
		return "", "", "", err
	}
	if created.Space == "" {
		return "", "", "", errors.New("herdr-app space.new returned no space id")
	}
	if prevSpace != "" {
		_ = c.appDo(nil, "space.select", idArg{ID: prevSpace})
	}
	if prevTab != "" {
		_ = c.appDo(nil, "tab.select", idArg{ID: prevTab})
	}
	tabID, paneID, err = c.tabCreate(created.Space, cwd, label, env)
	if err != nil {
		if created.Tab != "" {
			if cerr := c.tabClose(created.Tab); cerr != nil {
				log.Printf("close space %s first tab after failed tab open: %v", created.Space, cerr)
			}
		}
		return "", "", "", err
	}
	if created.Tab != "" {
		if cerr := c.tabClose(created.Tab); cerr != nil {
			log.Printf("close space %s first tab: %v", created.Space, cerr)
		}
	}
	return created.Space, tabID, paneID, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// tabCreate starts the tab's shell in hc with labels that place it in the
// space, so the tab appears without taking the screen (tab.new would). hc does
// not forward the caller's environment, so env rides in an /usr/bin/env prefix.
func (c *coreClient) tabCreate(workspaceID, cwd, label string, env map[string]string) (tabID, paneID string, err error) {
	tabID = "tb" + c.ids(16)
	args := []string{"run", "--bg", "--keep", "--no-parent", "--json",
		"--name", "sh-" + c.ids(4), "--cwd", cwd,
		"--label", "herdr.space=" + workspaceID, "--label", "herdr.tab=" + tabID, "--"}
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		args = append(args, "/usr/bin/env")
		for _, k := range keys {
			args = append(args, k+"="+env[k])
		}
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = defaultShell
	}
	args = append(args, shell, "-l")
	var started hcTerminal
	if err := c.hcJSON(&started, args...); err != nil {
		return "", "", err
	}
	if started.Address == "" {
		return "", "", errors.New("hc run returned no terminal address")
	}
	if !c.waitTabListed(tabID) {
		c.removeTerminal(started.Address)
		return "", "", fmt.Errorf("tab %s for terminal %s never appeared in herdr-app", tabID, started.Address)
	}
	if rerr := c.tabRename(tabID, label); rerr != nil {
		log.Printf("rename tab %s to %q: %v", tabID, label, rerr)
	}
	return tabID, started.Address, nil
}

func (c *coreClient) waitTabListed(tabID string) bool {
	deadline := c.now().Add(c.tabWait)
	for {
		if l, err := c.list(); err == nil && l.findTab(tabID) != nil {
			return true
		}
		if c.now().After(deadline) {
			return false
		}
		time.Sleep(c.poll)
	}
}

// removeTerminal ends and forgets one terminal; either step may fail on a
// terminal that is already finished or gone, which is the point of calling it.
func (c *coreClient) removeTerminal(address string) {
	_ = c.hcJSON(nil, "kill", address, "--json")
	_ = c.hcJSON(nil, "rm", address, "--json")
}

type tabRenameArgs struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *coreClient) tabRename(tabID, label string) error {
	return c.appDo(nil, "tab.rename", tabRenameArgs{ID: tabID, Name: label})
}

type tabCloseArgs struct {
	ID        string `json:"id"`
	Confirmed bool   `json:"confirmed"`
}

// tabClose closes the tab, then kills and removes its terminals: a closed tab
// leaves them listed as finished.
func (c *coreClient) tabClose(tabID string) error {
	var addrs []string
	if l, err := c.list(); err == nil {
		if t := l.findTab(tabID); t != nil {
			for _, term := range t.Terminals {
				if term.Terminal != "" {
					addrs = append(addrs, term.Terminal)
				}
			}
		}
	}
	if err := c.appDo(nil, "tab.close", tabCloseArgs{ID: tabID, Confirmed: true}); err != nil {
		return err
	}
	for _, a := range addrs {
		c.removeTerminal(a)
	}
	return nil
}

// runCommand pastes the command and presses Enter. The text follows "--" so a
// command that starts with a dash is not read as an option. hc holds a send
// for up to a minute while a person has an unfinished draft in that terminal,
// so this call gets sendDeadline rather than callDeadline.
func (c *coreClient) runCommand(paneID, command string) error {
	_, err := c.callWithin(sendDeadline, c.hc, "send", "--json", "--", paneID, command)
	return err
}

// paneRead prints plain text: visible is the screen alone, any other source
// the screen plus lines of history above it.
func (c *coreClient) paneRead(paneID, source string, lines int) (string, error) {
	n := lines
	if source == "visible" {
		n = 0
	}
	out, err := c.call(c.hc, "read", paneID, "--lines", strconv.Itoa(n))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (c *coreClient) get(target string) (hcTerminal, error) {
	var t hcTerminal
	err := c.hcJSON(&t, "get", target, "--json")
	return t, err
}

func agentNotFound(target, why string) error {
	return &herdrError{Code: "agent_not_found", Message: target + ": " + why}
}

func paneNotFound(target, why string) error {
	return &herdrError{Code: "pane_not_found", Message: target + ": " + why}
}

// agentWait polls the terminal's reported status, matching herdr's agent.wait
// codes: agent_not_found while a live terminal has no agent reporting (before
// it starts, after it exits), pane_not_found for a finished or missing
// terminal, timeout at the deadline. The core's error state counts as done.
func (c *coreClient) agentWait(target string, until []string, timeoutMS int) (string, error) {
	deadline := c.now().Add(time.Duration(timeoutMS) * time.Millisecond)
	for {
		t, err := c.get(target)
		switch {
		case hasCode(err, "not_found"):
			return "", paneNotFound(target, "no such terminal")
		case err != nil:
			return "", err
		case t.State != "live":
			return "", paneNotFound(target, "terminal is "+t.State)
		case t.Status == nil:
			return "", agentNotFound(target, "no agent reports status")
		}
		state := t.Status.State
		if state == "error" {
			state = "done"
		}
		if slices.Contains(until, state) {
			return state, nil
		}
		if !c.now().Before(deadline) {
			return "", &herdrError{Code: "timeout", Message: fmt.Sprintf("%s still %s after %dms", target, t.Status.State, timeoutMS)}
		}
		time.Sleep(c.poll)
	}
}

// paneExists reports a live terminal as present and a finished or missing one
// as gone; any other failure is no answer and is returned.
func (c *coreClient) paneExists(paneID string) (bool, error) {
	t, err := c.get(paneID)
	switch {
	case hasCode(err, "not_found"):
		return false, nil
	case err != nil:
		return false, err
	}
	return t.State == "live", nil
}

// notify leaves the notification as a JSON file for a mod to show: the new
// Herdr lets only a mod post one. Files older than a day are pruned.
func (c *coreClient) notify(title, body, sound string) error {
	dir := filepath.Join(c.stateDir, "notify")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	now := c.now()
	c.pruneNotifications(dir, now)
	data, err := json.Marshal(struct {
		Title string    `json:"title"`
		Body  string    `json:"body"`
		Sound string    `json:"sound"`
		At    time.Time `json:"at"`
	}{title, body, sound, now})
	if err != nil {
		return err
	}
	name := strconv.FormatInt(now.UnixNano(), 10)
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name+".json"))
}

func (c *coreClient) pruneNotifications(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= notifyMaxAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			log.Printf("prune notification %s: %v", e.Name(), err)
		}
	}
}

// startDaemon starts `<exe> daemon` in a kept herdr-core terminal named
// shepherd, after removing a terminal of that name the daemon no longer runs
// in (a dead daemon, or the bare shell keep left after one). The caller has
// already checked the daemon lock is free. hc gives a terminal the login
// environment, not the caller's, so the backend and the log path ride in an
// /usr/bin/env prefix. It returns the new terminal's address.
func (c *coreClient) startDaemon(exe string) (string, error) {
	old, err := c.get(daemonTerminal)
	switch {
	case hasCode(err, "not_found"):
	case err != nil:
		return "", fmt.Errorf("look up terminal %s: %w", daemonTerminal, err)
	default:
		addr := old.Address
		if addr == "" {
			addr = daemonTerminal
		}
		c.removeTerminal(addr)
	}
	var started hcTerminal
	if err := c.hcJSON(&started, "run", "--bg", "--keep", "--no-parent", "--json",
		"--name", daemonTerminal, "--", "/usr/bin/env", backendEnv+"="+backendCore,
		logPathEnv+"="+filepath.Join(c.stateDir, "shepherd.log"), exe, "daemon"); err != nil {
		return "", err
	}
	if started.Address == "" {
		return "", errors.New("hc run returned no terminal address")
	}
	return started.Address, nil
}
