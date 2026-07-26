# herdr-shepherd

Scheduled agent sessions for [herdr](https://herdr.dev): heartbeats, cron routines, and scripts — launched on time, in the right directory, visible in the herd.

herdr shows you what your agents are doing. Shepherd decides when they run.

## What it does

- **Heartbeats** — run an agent with a prompt every N minutes, optionally only within working hours.
- **Routines** — run an agent on a schedule (weekdays at 9, monthly on the 1st, or any cron expression).
- **Scripts** — run plain commands on the same schedules, with timeouts.

Every scheduled agent run opens as a real workspace in herdr, so herdr's native agent-state detection (working / blocked / done / idle) applies to scheduled sessions too. A heartbeat stuck on a permission prompt is a visible `blocked` pane and a notification, not a silent background failure. Scripts run headlessly with their output captured in the run log.

## Install

Requires herdr ≥ 0.7.0 and a Go toolchain (a prebuilt-binary fallback will come with the first release).

```bash
herdr plugin install mikedclarke/herdr-shepherd
```

Or from a checkout:

```bash
sh scripts/build.sh
herdr plugin link /path/to/herdr-shepherd
```

The plugin registers a startup daemon (herdr launches it with the server) and a `Shepherd: Status` action in herdr's plugin action menu. After a fresh `plugin link`, the daemon starts on the next herdr server start; to run it immediately, start `./bin/herdr-shepherd daemon` yourself — the pid lock keeps the two from ever double-firing.

## Configuring actions

Actions are TOML files in the plugin config directory (`herdr plugin config-dir mikedclarke.herdr-shepherd`), one file per action, under `actions/`. The daemon re-reads them every tick, so edits apply within 30 seconds — no restart.

A heartbeat:

```toml
name = "inbox-triage"
kind = "heartbeat"
directory = "~/work/ops"
prompt = "Triage the support inbox per OPS.md. Exit when done."
permission_mode = "auto"        # default | auto | skip

[heartbeat]
interval_minutes = 60

[heartbeat.working_hours]       # optional; omit to run around the clock
days = [1, 2, 3, 4, 5]          # 0=Sunday .. 6=Saturday; empty = any day
start_hour = 9                  # start > end spans midnight
end_hour = 17
```

A routine:

```toml
name = "morning-digest"
kind = "routine"
directory = "~/work/pm"
prompt = "Prepare the morning digest per DIGEST.md."
auto_close = false              # keep the workspace open after the run

[routine]
preset = "weekdays"             # daily | weekdays | days | monthly | cron
hours = [6]
minute = 15
```

A script:

```toml
name = "repo-sync"
kind = "script"
directory = "~/work"
command = "./sync.sh"
timeout_minutes = 30

[routine]
preset = "cron"
cron = "0 6 * * 1-5"
```

Agent actions also accept `cli = "claude" | "codex"`, `model = "..."`, `enabled = false`, and `watch_minutes` (how long the daemon watches a session before flagging it; default 240).

### Semantics worth knowing

- **No backfill.** Routine occurrences missed while the daemon was down are dropped, not replayed on startup. Overdue heartbeats fire at the next opportunity inside working hours.
- **No overlap.** An action never fires while its previous run is still going.
- **Auto-close** (default on) closes the run's workspace once the agent reaches `done`. A `blocked` session triggers one notification and stays open.
- **Manual runs are yours.** `herdr-shepherd run <name>` starts the session and hands it to you — no watcher, no auto-close.

## CLI

```
herdr-shepherd daemon         # the scheduler (herdr starts this via the manifest)
herdr-shepherd list           # actions, schedules, last/next runs
herdr-shepherd run <name>     # fire an action now
herdr-shepherd status         # daemon liveness + next run (--notify for a herdr toast)
```

State (last runs, run log as JSONL) lives in the plugin state directory herdr provisions; outside herdr the CLI falls back to `~/.config/herdr-shepherd` and `~/.local/state/herdr-shepherd`.

## License

MIT
