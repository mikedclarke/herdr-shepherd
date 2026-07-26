# herdr-shepherd

Scheduled agent sessions for [herdr](https://herdr.dev): heartbeats, cron routines, and scripts — launched on time, in the right directory, visible in the herd.

herdr shows you what your agents are doing. Shepherd decides when they run.

## What it does

- **Heartbeats** — run an agent with a prompt every N minutes, optionally only within working hours.
- **Routines** — run an agent on a cron schedule (weekdays at 9, monthly on the 1st, or any cron expression).
- **Scripts** — run plain commands on the same schedules, with timeouts.

Every scheduled run opens as a real pane in your herdr workspace, so herdr's native agent-state detection (working / blocked / done / idle) applies to scheduled sessions too. A heartbeat stuck on a permission prompt is a visible `blocked` pane, not a silent background failure.

## Status

Early development. Not yet functional.

## How it will work

Shepherd is a herdr plugin. Its manifest registers a startup command — a small daemon herdr launches alongside its server — that reads action definitions from TOML files in the plugin config directory, computes next runs, and fires them through the herdr CLI: create a pane in the target directory, run the agent with its prompt, watch for completion, optionally close the pane.

## License

MIT
