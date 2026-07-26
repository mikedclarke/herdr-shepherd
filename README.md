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

The plugin's startup hook spawns the scheduler daemon when the herdr server starts, and registers a `Shepherd: Status` action in herdr's plugin action menu. After a fresh `plugin link`, run `./bin/herdr-shepherd daemon --detach` once to start it immediately — a kernel lock keeps any two daemons from double-firing, however they were started.

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

[schedule]
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

[schedule]
preset = "cron"
cron = "0 6 * * 1-5"
```

Agent actions also accept `cli = "claude" | "codex"`, `model = "..."`, `enabled = false`, `auto_close = true` (close the run's workspace once the agent finishes), and `watch_minutes` (how long the daemon watches a session before flagging it; default 240). Treat `permission_mode = "skip"` with respect: it maps to the CLI's skip-all-permissions flag, on an unattended schedule.

### Semantics worth knowing

- **No backfill.** A routine occurrence more than ten minutes in the past is dropped, not run late — whether the daemon was down or the machine was asleep. Waking your laptop at 08:55 does not fire the 06:00 jobs. Overdue heartbeats fire at the next opportunity inside working hours.
- **Startup failures retry; runs don't.** If a run fails before its agent ever started (herdr restarting, socket briefly down), the occurrence is retried on the next ticks, up to three attempts. Once a session is running, shepherd never re-fires it.
- **No overlap.** An action never fires while its previous run is still going.
- **Completed runs stay visible.** By default a finished session stays in the herd for review, herdr's normal workflow. Set `auto_close = true` to close its workspace on completion instead. A `blocked` session triggers one notification and stays open either way.
- **Manual runs are yours.** `herdr-shepherd run <name>` starts the session and hands it to you — no watcher, no auto-close, and no coordination with the schedule (running an action a minute before its scheduled time will not stop the scheduled run).
- **DST:** on fall-back days the repeated hour runs once, not twice. On spring-forward days a job scheduled inside the skipped hour does not run that day.

## CLI

```
herdr-shepherd daemon         # the scheduler (the manifest startup hook runs daemon --detach)
herdr-shepherd list           # actions, schedules, last/next runs
herdr-shepherd run <name>     # fire an action now
herdr-shepherd status         # daemon liveness + next run (--notify for a herdr toast)
herdr-shepherd version        # print the version
```

The CLI resolves the same managed config directory the daemon uses by asking herdr (`herdr plugin config-dir`), so `list` and `status` agree with the scheduler from any shell. Only when herdr is unreachable does it fall back to `~/.config/herdr-shepherd` and `~/.local/state/herdr-shepherd` (`XDG_CONFIG_HOME`/`XDG_STATE_HOME` respected).

## Troubleshooting

- **Daemon logs:** `shepherd.log` in the plugin state directory (`~/.local/state/herdr/plugins/mikedclarke.herdr-shepherd/`), alongside `runs.jsonl` (one JSON line per run) and `state.json` (last-run times). Both logs rotate at 5 MB.
- **Is it alive?** `herdr-shepherd status`, or the `Shepherd: Status` action from herdr's plugin menu. "Daemon not running" with a running herdr means the startup hook hasn't fired since the plugin was installed — start it with `daemon --detach` or restart herdr.
- **Run statuses:** `completed` is a clean finish; `attention` means look at the session (it needed input, exceeded its watch window, or its agent exited unexpectedly); `cancelled` means its pane was closed mid-run; `error` carries the failure detail.

## License

MIT
