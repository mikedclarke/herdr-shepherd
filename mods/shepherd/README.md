# Shepherd mod for the Herdr desktop app

A window in the Herdr desktop app for herdr-shepherd's scheduled actions: see
them, pause or enable them, run one now, read its recent runs, edit it with a
form or as text, and create a new one.

It runs the `herdr-shepherd` command line and nothing else (`list`, `get`,
`fields`, `runs`, `enable`, `disable`, `apply`, `put-toml` and `wake`, with
`--json`), so shepherd does all the checking, writing and locking. The mod reads
and writes no files of its own and makes no network calls.

## Needs

- herdr-shepherd 0.11.0 or later, on the app's `PATH` or in `~/.local/bin`.
- The Herdr desktop app (`herdr-app`).

## Install

From a checkout of herdr-shepherd, copy this folder into the app's mods
folder. The same commands update it later:

```sh
mkdir -p ~/.config/herdr-app/mods/shepherd
cp -R mods/shepherd/. ~/.config/herdr-app/mods/shepherd/
herdr-app check ~/.config/herdr-app/mods/shepherd
```

## Open it

As a window:

```sh
herdr-app do mod.open --name shepherd
```

As a tab:

```sh
herdr-app do tab.new --name Shepherd --mod shepherd
```

The app has no menu entry for a mod yet, so bind a key to open the window: add
a line to the `global` section of `~/.config/herdr-app/keys.json` (any free key
works):

```json
"cmd-shift-s": "mod.open:shepherd"
```

The same key closes it again; so does Esc on the list.

## What you see

- **The list**: one row per action with a dot in the theme's colours (green:
  enabled; hollow: paused; pulsing: running; red: the last run failed; the
  attention colour: the file does not load),
  its name, kind, schedule, last run and next run. A filter box (Enter opens the
  first match). The header says whether the daemon is running and which backend
  shepherd uses.
- **Row buttons**: Pause or Enable (one click), Run (asks first), and a click on
  the row opens its details. A file that does not load offers only Edit as text.
- **Details**: the action's settings, its last 20 runs, and Edit, Edit as text,
  Run now, Pause or Enable.
- **Edit and New action**: a form drawn from `herdr-shepherd fields --json`,
  saved with `apply`. Shepherd's validation messages show under their fields. A
  new action starts paused. Saving the form rewrites the file the way the
  board's form does, so comments go; Edit as text keeps them.
- **Run now** is `herdr-shepherd wake <name>`: the daemon starts the run within
  30 seconds, exactly as a scheduled one, and records it. Shepherd refuses to
  wake a paused action, and the window shows its message. After a wake the list
  refreshes a few times over the next minute.

The list refreshes when it opens, after every change and every 30 seconds.

## Key commands

While the mod has the keyboard, a `mod:shepherd` section in `keys.json` can bind
its commands: `shepherd.refresh`, `shepherd.new`, `shepherd.back`,
`shepherd.edit`, `shepherd.text`, `shepherd.toggle`, `shepherd.run`,
`shepherd.confirm` (answers yes to Run now) and `shepherd.save` (the form or the
text editor). For example:

```json
"mod:shepherd": { "cmd-s": "shepherd.save", "cmd-r": "shepherd.refresh" }
```

## Props

When opened as a tab or by another mod, `props` can choose what it shows:

| Prop | Effect |
|---|---|
| `action` | Opens on that action's details |
| `view` | With `action`: `"edit"` (the form) or `"text"` (the text editor). Alone: `"new"` (a new action) |
| `bin` | The program to run in place of `herdr-shepherd`, such as a wrapper that points shepherd at a test copy of its folders |

```sh
herdr-app do tab.new --name Shepherd --mod shepherd --props '{"action":"<action-name>"}'
```
