# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.7.2] - 2026-08-26

### Added

- **`herdr-shepherd wake <name> --at <when>`: ask for a wake at an instant, not
  just on the next tick.** The instant is RFC3339 or `YYYY-MM-DD HH:MM[:SS]` read
  as local time, and the request is a file beside the ordinary wake; the daemon
  promotes it to a normal wake on the first tick at or after the instant, which
  then fires under the same run lock, debounce and `trigger: "wake"` stamping as
  any other wake. This is for a producer that already knows when it will want a
  run, such as a calendar alarm asking for a session 25 minutes before a call.
  At most one schedule per action: the latest one replaces the last, so a plan
  that moves is one more request. A schedule left more than an hour behind its
  instant, say because the daemon was down, is dropped rather than fired late; an
  instant already past by less than that is simply queued now. A disabled or
  unknown action refuses as it always did, and a schedule for a deleted action is
  pruned with its wake file.

## [0.7.1] - 2026-08-25

### Changed

- **A scheduled run absorbs a wake queued for the same action.** A request that
  arrived just before an occurrence used to wait for the scheduled run to finish
  and then buy a second full run of its own. Now the due occurrence claims the
  wake file under its run lock, records `trigger: "wake"` and hands
  `SHEPHERD_TRIGGER=wake` to the pane, so the session knows it is serving the
  request, while the schedule stamp advances exactly as it would for any
  scheduled occurrence. If the wake was already claimed elsewhere, the scheduled
  run still runs, as a scheduled one.

## [0.7.0] - 2026-08-25

### Added

- **`herdr-shepherd wake <name>`: fire an action outside its schedule, safely.**
  The command writes a request into the state directory and returns; the daemon
  fires it on its next tick under the action's run lock, so a wake never overlaps
  a running run (it waits behind it) and never launches from the CLI's own process
  the way `run` does. Requests are debounced (a second one inside 20 seconds is
  `already queued`), at most one is ever pending per action, a request older than
  an hour is dropped rather than fired late, and a disabled action refuses. A
  woken heartbeat restamps its schedule exactly like a scheduled one. Run history
  records `trigger: "wake"` on the `started` and terminal records; the board shows
  `wake` beside an action with a request pending.
- **`SHEPHERD_TRIGGER` in the pane.** Agent sessions now receive `SHEPHERD_TRIGGER`
  (`schedule`, `wake`, or `manual`) beside `SHEPHERD_ACTION`.

## [0.6.5] - 2026-08-18

### Added

- **`pi` is a supported agent CLI** for heartbeat and routine actions, alongside
  `claude` and `codex`. The prompt is passed as a bare argument, which opens an
  interactive pi session in the run's workspace (never `-p`, pi's headless print
  mode), and `model` maps to pi's `--model`. pi has no permission flags, so
  `permission_mode` must stay `default` — `auto`/`skip` are rejected with a clear
  error rather than silently dropped, and the form pins the field. herdr detects
  pi panes natively, so blocked-detection and the watch window work as they do
  for the other CLIs.
- **Exit 75 is a first-class deferral for scripts.** A script exiting 75
  (`EX_TEMPFAIL`, "not now, retry later") records `deferred` instead of `error`
  and raises no notification. A new `defer_retry_minutes` key (default 0 = no
  retries) makes the daemon retry the script every tick until it runs or the
  window closes; the window closing records `deferred-expired` and notifies,
  since the run never happened. Only the first deferral and the final outcome
  reach the run history. The board shows the new statuses (`…` / `!`) and the
  detail view shows the retry window.

## [0.6.1] - 2026-08-08

### Added

- The new-action form is friendlier to fill in without knowing the TOML:
  - **Model picker.** For `cli = "claude"` the `model` field is a `‹ ›` list of
    common ids (opus 4.8 1M, opus 5, sonnet 5, haiku 4.5, fable 5), with a
    `custom…` step that drops to free text so any id still works. No allowlist is
    added: a typed id is never rejected. `codex` keeps the free-text field.
  - **Live next-run preview.** A `next run:` line under the fields shows the next
    three fire times for the schedule as typed (heartbeats and scripts too),
    recomputed as you edit and blank while a field is half-typed.
  - **Weekday chips.** The `days` and working-hours day pickers are Mon–Sun chips
    (`←/→` to move, space to toggle) instead of a `0=Sun..6=Sat` CSV. The value on
    disk is unchanged.
  - **Skip caution.** `permission_mode = "skip"` shows in amber with a one-line
    reminder that the run has no permission prompts and is unattended.
- Run records carry the run's wall-clock duration (`duration_secs`), written on
  the record that ends a run: scheduled scripts and agent sessions, and manual
  script runs from the CLI or the board. Started records, interrupted markers,
  and manual agent runs (unwatched by design) record none. The board's Recent
  runs list shows the duration next to each run.

## [0.6.0] - 2026-07-31

### Added

- Per-action run locks, held as kernel file locks in the state directory, so the
  no-overlap guarantee holds across processes: the board and `run <name>` refuse an
  action the daemon is already running, and the schedule skips an action you are
  running by hand. Board rows show `running…` for scheduled runs too.
- Run history records a `trigger` field, so manual runs are distinguishable from
  scheduled ones, and manual runs (which the daemon never watched) now appear in
  the history at all.
- Two run statuses: `started`, written when an agent session is launched, and
  `interrupted`, written at daemon start for a run that was still `started` when the
  daemon or the machine went down. A crash mid-run leaves a record instead of a gap.
- The heartbeat form round-trips working hours: days, start hour, and end hour are
  editable fields, and an existing `[heartbeat.working_hours]` block survives an
  unrelated edit and save.
- Rejects more unrunnable configurations up front: names containing `/`, `\`, or a
  leading `.`; `timeout_minutes` or `watch_minutes` over 1440; working hours whose
  start hour equals its end hour.
- `scripts/install.sh` downloads a checksum-verified prebuilt binary for the host OS
  and architecture, and `scripts/build.sh` falls back to it when no Go toolchain is
  present, so installing the plugin no longer requires Go.
- `scripts/check.sh` runs the release gate: `gofmt -l`, `go vet`, `go test -race`.

### Changed

- Schedules render in plain language everywhere they are shown. A heartbeat that can
  only fire once per working-hours window reads `weekdays ~06:00` instead of
  `every 1200m (06-16h)`; hour lists collapse to ranges (`weekdays hourly
  08:40-16:40` instead of `days 8,9,10,11,12,13,14,15,16:40`); day sets read
  `weekdays`, `weekends`, or `Mon,Wed,Fri`. The board's detail view also states the
  firing semantics: heartbeats note that a missed run fires late rather than being
  skipped, routines and scripts note the missed-run grace after which an occurrence
  is dropped.
- Paths under the home directory render as `~/…` in the board's header and detail
  view.
- `go.mod` no longer pins a patch release of Go.
- Both manifest actions declare `contexts = ["global"]`.
- README: corrected the plugin-menu, log-rotation, `auto_close`, and `$PATH` claims;
  added a first-run walkthrough, a defaults table, the board's key list, where config
  and state live, how errors surface, and the platform and filesystem caveats.

### Fixed

- A brief `idle` no longer ends a run. Agents report `idle` between turns, so the
  watcher could log a still-working session `completed` and stop watching, silencing
  every later notification. Idle is now confirmed before it is believed terminal.
- Missed occurrences are clamped to the catch-up grace instead of walked forward one
  tick at a time. An action added to a long-running daemon fires its next occurrence
  promptly rather than after a walk, and a machine waking from sleep still never
  backfills.
- A transient TOML parse error no longer prunes that action's saved state, so a typo
  saved and fixed within a tick can't make a heartbeat fire early.
- Scripts run through one runner shared by the daemon, the CLI, and the board:
  process-group kill on timeout, and a bounded wait so a daemonized grandchild
  holding the output pipe cannot hang the run forever. The board's manual script runs
  gain the timeout they were missing.
- Agent launches go through one shared path. A workspace whose command could not be
  submitted is closed again instead of being left empty, a pane that disappears
  before the agent starts reports `cancelled` rather than an error, transport hiccups
  during startup no longer tear down a healthy run, and a command that produced no
  agent within 20 seconds is re-sent once.
- The run log is safe for concurrent writers: appends, size checks, and rotation all
  happen under one file lock, and append failures are logged instead of dropped.
- Log rotation happens inside the running daemon, not only when a detached daemon is
  spawned, so a long-lived daemon keeps `shepherd.log` bounded without a restart.
- `SIGTERM` and `SIGINT` shut the daemon down cleanly and release its lock.
- Config-error notifications are rebuilt each tick, so the set can't grow without
  bound and a recurring error notifies again after it was fixed.
- The board and the CLI no longer quarantine a corrupt `state.json`; only the daemon
  does. Reading the board can't cost you your schedule state.
- The board reads only the tail of the run log, falls back to the rotated file when
  the current one is short, and caches on size and modification time, so the two-second
  refresh stops re-parsing history.
- Rendering and input fixes: truncation and padding respect display width instead of
  bytes, rows clamp to the terminal width and scroll to keep the selection visible,
  `ctrl+c` quits from anywhere including mid-edit, and a field error clears when you
  edit that field.
- Resolving the herdr config directory can no longer hang: the lookup has a deadline,
  and when it falls back to the XDG path shepherd says so on stderr instead of quietly
  reading a different directory than the daemon.
- The board's next-run column is computed by the same code the daemon schedules with.
- The form's temporary files are suffixed `.tmp`, so one left behind by a killed
  board session no longer shows up as a broken action row.

## [0.5.1] - 2026-07-28

### Fixed

- Startup detection required `working` or `blocked` before treating a run as started.
  A freshly launched agent reports `idle` for a few seconds before its first turn
  registers, and accepting `idle` or `done` there ended the watch seconds into a real
  run, masking stalls and skipping every later notification. A short `done`-only
  probe keeps runs that finish inside one wait slice from being misreported, and a
  pane that never starts working reports `attention` instead of success.
- The daemon's herdr client became an interface, with scripted-fake regression tests
  covering the launch and watch paths.

## [0.5.0] - 2026-07-27

### Added

- Edit any action in the guided form: `e`, or the detail view's `[ edit ]` button,
  opens it pre-filled. Renames move the file, enabled state is preserved, and other
  actions are never clobbered. `E` still opens the raw TOML, as do broken files.
- A button bar in the detail view: `[ edit ] [ run now ] [ pause/resume ] [ back ]`.
- A trailing `+ new action…` row, so creating an action is discoverable by click alone.
- `enabled` is a form field.

### Removed

- Right-click gestures: herdr owns the pane context menu, so those clicks never
  reached the TUI.

## [0.4.0] - 2026-07-27

### Added

- Full mouse and touch control on the board: the wheel scrolls, a click selects, a
  second click or a right-click opens details, and the footer hints are buttons.
- `n` opens a guided form instead of a file template: the kind and schedule presets
  cycle, integers and text edit inline, and the result passes the daemon's own
  validation before anything is written. New actions are created paused for review,
  and existing files are never overwritten.

### Changed

- `e` falls back to nano when `$VISUAL` and `$EDITOR` are unset, with on-screen exit
  keys, instead of stranding you in vi.

## [0.3.0] - 2026-07-27

### Added

- The status board: a live TUI listing every action with its schedule, last run, next
  run, and daemon liveness. `board` opens it as a zoomed herdr plugin pane from an
  action, or inline when run from a shell.
- Pause and resume as a line-preserving atomic edit of the action's TOML, so comments
  and formatting survive.
- Run now, with manual-run semantics, and a detail view with recent run history.
- `e` edits the action's TOML in `$EDITOR`; `n` scaffolds a new action from a template.
- Broken TOML files surface as rows carrying their parse error.

### Removed

- The CI workflow; the repository is checked locally.

## [0.2.0] - 2026-07-26

### Added

- Per-file config loading, so one broken TOML disables only itself. Unknown keys and
  out-of-range or unsatisfiable schedules are rejected rather than clamped.
- A one-shot startup hook (`daemon --detach`) that spawns the scheduler and exits,
  with logs in the plugin state directory.
- Config-directory resolution via `herdr plugin config-dir`, so the CLI reads the same
  actions from a plain shell that the daemon runs.
- Socket deadlines, DST fall-back deduplication, a backwards-clock guard, and log
  rotation.
- State, daemon, and socket-client tests.

### Fixed

- Shared state is guarded by a mutex and per-save temporary files, and the pid lock
  became a kernel flock, so a killed daemon leaves no stale lock.
- Panics in the tick loop and in individual fires are recovered.
- `agent_not_found` around agent detection is handled, and `blocked` is no longer
  re-armed in the wait set (it returned instantly and spun the socket).
- Routine occurrences older than a ten-minute grace are dropped, so waking from sleep
  never backfills. Routines are stamped with their occurrence time and heartbeats at
  completion, and occurrences that fail before their agent starts are retried.
- Script process groups are killed on timeout, and captured output is bounded.

### Changed

- `[routine]` became `[schedule]`, with the old name kept as an alias.
- `auto_close` defaults to `false`, matching herdr's review-the-finished-session flow.

## [0.1.0] - 2026-07-26

### Added

- Heartbeat, routine, and script actions from per-action TOML files.
- Agent sessions run as herdr workspaces over the socket API, with agent-state
  watching, notifications when a session is blocked, and optional auto-close.
- Scripts run headless with timeouts.
- A five-field cron engine, working-hours windows, no-backfill scheduling, a lock
  against double daemons, and a JSONL run log.
- The plugin manifest, the CLI, and the MIT license.
