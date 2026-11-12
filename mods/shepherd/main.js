// Shepherd: the herdr-shepherd schedule in a window. It lists the actions and
// enables, pauses, wakes, edits and creates them only through shepherd's own
// command line (list, get, fields, runs, enable, disable, apply, put-toml,
// wake), so the validation, the file format and the locks stay in shepherd.
// It writes no files itself.
//
// props: `bin` runs another program in place of herdr-shepherd (a wrapper
// around a test copy, say); `action` opens on that action's details, and
// `view` ("edit", "text" or "new") on its form, its text, or a new action.
// Key commands (keys.json "mod:shepherd", or run("shepherd.<command>")):
// refresh, new, back, edit, text, toggle, run, confirm, save.

import { View, div } from "gpui-kit";
import { v_flex, h_flex, InputState, Input, TextareaState, Textarea, EditorState, Editor, ScrollHandle } from "gpui-base";
import { exec, home, props, theme, settings, closeOverlay, nextCommand } from "herdr";

const MIN_VERSION = [0, 11, 0];
const FAILED = new Set(["error", "deferred-expired", "interrupted"]);
const WARNED = new Set(["attention", "cancelled"]);
const DAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const NARROW = 620;
const pad = (n) => String(n).padStart(2, "0");
const message = (e) => String(e?.message ?? e);

function atLeast(version) {
  const m = /(\d+)\.(\d+)\.(\d+)/.exec(version ?? "");
  if (!m) return false;
  for (let i = 0; i < 3; i++) {
    const n = Number(m[i + 1]);
    if (n !== MIN_VERSION[i]) return n > MIN_VERSION[i];
  }
  return true;
}

function time(s) {
  const ms = s ? Date.parse(s) : NaN;
  return Number.isNaN(ms) ? null : ms;
}

function ago(ms, now) {
  const m = Math.round((now - ms) / 60000);
  if (m < 1) return "just now";
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  return h < 48 ? `${h}h ago` : `${Math.floor(h / 24)}d ago`;
}

function until(ms, now) {
  const m = Math.round((ms - now) / 60000);
  if (m <= 0) return "due now";
  if (m < 60) return `in ${m}m`;
  const h = Math.floor(m / 60);
  return h < 48 ? `in ${h}h` : `in ${Math.floor(h / 24)}d`;
}

function clock(ms, now) {
  const d = new Date(ms);
  const n = new Date(now);
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (d.getFullYear() === n.getFullYear() && d.getMonth() === n.getMonth() && d.getDate() === n.getDate()) return `today ${hm}`;
  if (Math.abs(ms - now) < 6 * 86400000) return `${DAYS[d.getDay()]} ${hm}`;
  return `${d.getDate()} ${MONTHS[d.getMonth()]} ${hm}`;
}

function duration(s) {
  if (s === null || s === undefined) return "";
  if (s < 60) return `${s < 10 ? s.toFixed(1) : Math.round(s)}s`;
  const m = Math.round(s / 60);
  return m < 60 ? `${m}m` : `${Math.floor(m / 60)}h ${pad(m % 60)}m`;
}

// A failed verb: its JSON error object, else the last line it wrote to stderr.
function failure(r) {
  try {
    const j = JSON.parse(r.stdout);
    if (j && j.error) return j;
  } catch {}
  const line = (r.stderr ?? "").split("\n").map((l) => l.trim()).filter((l) => l && !l.startsWith("warning:")).pop();
  return { error: (line ?? `exited with code ${r.code}`).replace(/^herdr-shepherd:\s*/, "") };
}

function state(a) {
  if (a.error) return "file error";
  if (a.running) return "running";
  if (!a.enabled) return "paused";
  if (FAILED.has(a.last_run?.status)) return "last run failed";
  return "enabled";
}

export default class Shepherd extends View {
  init(_props, cx) {
    this.bin = null;
    this.missing = null;
    this.data = null;
    this.fields = null;
    this.listError = null;
    this.note = null;
    this.view = "list";
    this.name = null;
    this.detail = null;
    this.runs = null;
    this.detailError = null;
    this.confirm = null;
    this.busy = new Set();
    this.filter = "";
    this.width = 900;
    this.form = null;
    this.inputs = null;
    this.code = null;
    this.textError = null;
    this.saving = false;
    this.handle = cx.focus_handle();
    this.scroll = ScrollHandle.new();
    this.search = InputState.new({ placeholder: "Filter by name, kind, schedule or state" });
    this.search.on("change", (_e, cx) => { this.filter = this.search.value(); cx.notify(); });
    this.search.on("submit", (_e, cx) => { const a = this.shown()[0]; if (a) this.openDetail(a.name, cx); });
    this.search.focus();
    cx.timer.after(600, () => { if (this.view === "list") this.search.focus(); });
    cx.timer.every(30000, (cx) => this.refresh(cx));
    cx.spawn(async (cx) => {
      await this.start();
      this.opening(cx);
      cx.notify();
    });
    cx.spawn(async (cx) => {
      for (;;) {
        const c = await nextCommand();
        try {
          this.command(c, cx);
        } catch (e) {
          this.note = { level: "error", text: message(e) };
        }
        cx.notify();
      }
    });
  }

  // Where props() asks the window to open.
  opening(cx) {
    const p = props() ?? {};
    if (!this.bin) return;
    if (p.view === "new") return this.startForm("create", null, cx);
    if (!p.action) return;
    this.openDetail(p.action, cx, p.view);
  }

  command(c, cx) {
    const a = this.detail?.action;
    if (c === "refresh") this.refresh(cx);
    else if (c === "back") this.back(cx);
    else if (c === "new" && this.view !== "edit" && this.view !== "text") this.startForm("create", null, cx);
    else if (c === "save" && this.view === "edit" && this.form) this.save(cx);
    else if (c === "save" && this.view === "text") this.saveText(cx);
    else if (c === "confirm" && this.confirm) this.wake(this.confirm, cx);
    else if (this.view !== "details" || !a) return;
    else if (c === "edit" && this.detail.values) this.startForm("edit", this.detail.values, cx);
    else if (c === "text") this.openText(cx);
    else if (c === "toggle" && !a.error) this.toggle(a, cx);
    else if (c === "run" && !a.error) this.confirm = a.name;
  }

  async start() {
    try {
      const wanted = props()?.bin;
      const tries = wanted ? [wanted] : ["herdr-shepherd", `${home()}/.local/bin/herdr-shepherd`];
      for (const bin of tries) {
        let r;
        try {
          r = await exec(bin, ["version"], { timeout: 10000 });
        } catch {
          continue;
        }
        if (r.code !== 0) continue;
        const v = r.stdout.trim();
        if (!atLeast(v)) {
          this.missing = `This window needs herdr-shepherd ${MIN_VERSION.join(".")} or later, and ${bin} is ${v || "older"}. Update it (see Update in the shepherd README), then open the window again.`;
          return;
        }
        this.bin = bin;
        break;
      }
      if (!this.bin) {
        this.missing = wanted
          ? `Could not run ${wanted}.`
          : "herdr-shepherd was not found on the PATH or in ~/.local/bin. Install it (see Install in the shepherd README) and open the window again.";
        return;
      }
      await Promise.all([this.loadList(), this.loadFields()]);
    } catch (e) {
      this.listError = message(e);
    }
  }

  // One verb. `input` goes to its stdin, through sh, since exec takes none.
  async call(args, input) {
    try {
      const r = input === undefined
        ? await exec(this.bin, args, { timeout: 60000 })
        : await exec("/bin/sh", ["-c", 'input=$1; shift; printf "%s" "$input" | "$@"', "sh", input, this.bin, ...args], { timeout: 60000 });
      if (r.code !== 0) return { fail: failure(r) };
      try {
        return { ok: JSON.parse(r.stdout) };
      } catch {
        return { ok: r.stdout.trim() };
      }
    } catch (e) {
      return { fail: { error: message(e) } };
    }
  }

  async loadList() {
    const r = await this.call(["list", "--json"]);
    if (r.fail) this.listError = `Could not list the actions: ${r.fail.error}`;
    else {
      this.data = r.ok;
      this.listError = null;
    }
  }

  async loadFields() {
    const r = await this.call(["fields", "--json"]);
    if (r.fail) this.listError = `Could not read the form's fields: ${r.fail.error}`;
    else this.fields = r.ok.fields ?? [];
  }

  async loadDetail(name) {
    const [got, runs] = await Promise.all([
      this.call(["get", name, "--json"]),
      this.call(["runs", "--json", "--action", name, "--limit", "20"]),
    ]);
    if (this.name !== name) return;
    if (got.fail) {
      this.detailError = got.fail.error;
      return;
    }
    this.detail = got.ok;
    this.detailError = null;
    this.runs = runs.fail ? { error: runs.fail.error } : runs.ok.runs ?? [];
  }

  refresh(cx) {
    if (!this.bin || this.refreshing) return;
    this.refreshing = true;
    cx.spawn(async (cx) => {
      try {
        await this.loadList();
        if (this.view === "details" && this.name) await this.loadDetail(this.name);
      } catch (e) {
        this.listError = message(e);
      } finally {
        this.refreshing = false;
        cx.notify();
      }
    });
  }

  put(action) {
    if (!this.data || !action) return;
    const list = this.data.actions.filter((a) => a.name !== action.name);
    list.push(action);
    list.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    this.data.actions = list;
    if (this.detail?.action?.name === action.name) this.detail.action = action;
  }

  shown() {
    const all = this.data?.actions ?? [];
    const f = this.filter.trim().toLowerCase();
    if (!f) return all;
    return all.filter((a) => [a.name, a.kind, a.schedule, a.directory, state(a), a.last_run?.status].join(" ").toLowerCase().includes(f));
  }

  toggle(a, cx) {
    if (this.busy.has(a.name)) return;
    this.busy.add(a.name);
    cx.spawn(async (cx) => {
      try {
        const r = await this.call([a.enabled ? "disable" : "enable", a.name, "--json"]);
        if (r.fail) this.note = { level: "error", text: `${a.enabled ? "Pause" : "Enable"} ${a.name}: ${r.fail.error}` };
        else {
          this.put(r.ok.action);
          this.note = null;
        }
        await this.loadList();
      } catch (e) {
        this.note = { level: "error", text: message(e) };
      } finally {
        this.busy.delete(a.name);
        cx.notify();
      }
    });
  }

  wake(name, cx) {
    this.confirm = null;
    this.busy.add(name);
    cx.spawn(async (cx) => {
      try {
        const r = await this.call(["wake", name]);
        if (r.fail) this.note = { level: "error", text: `Run ${name} now: ${r.fail.error}` };
        else {
          const idle = this.data && !this.data.daemon?.running;
          this.note = {
            level: idle ? "warning" : "info",
            text: `${r.ok || `queued: ${name}`}${idle ? ". The daemon is not running, so it waits until the daemon starts (a wake lapses after an hour)." : ""}`,
          };
          for (const s of [5, 15, 30, 45, 60]) cx.timer.after(s * 1000, (cx) => this.refresh(cx));
        }
      } catch (e) {
        this.note = { level: "error", text: message(e) };
      } finally {
        this.busy.delete(name);
        cx.notify();
      }
    });
  }

  openDetail(name, cx, then) {
    this.view = "details";
    this.name = name;
    this.detail = null;
    this.runs = null;
    this.detailError = null;
    this.confirm = null;
    this.handle.focus();
    cx.spawn(async (cx) => {
      try {
        await this.loadDetail(name);
        if (this.name === name && this.detail) {
          if (then === "edit" && this.detail.values) this.startForm("edit", this.detail.values, cx);
          else if (then === "text") this.openText(cx);
        }
      } catch (e) {
        this.detailError = message(e);
      }
      cx.notify();
    });
  }

  back(cx) {
    if (this.confirm) {
      this.confirm = null;
      return cx.notify();
    }
    if (this.view === "edit" || this.view === "text") {
      const wasCreate = this.form?.mode === "create";
      this.form = null;
      this.textError = null;
      if (wasCreate || !this.name) {
        this.view = "list";
        this.search.focus();
      } else this.openDetail(this.name, cx);
    } else if (this.view === "details") {
      this.view = "list";
      this.name = null;
      this.search.focus();
    } else if (props()?.overlay) closeOverlay(props().overlay);
    cx.notify();
  }

  // The form's inputs, one per text, number and multiline field, made once.
  ensureInputs() {
    if (this.inputs) return;
    this.inputs = {};
    for (const f of this.fields ?? []) {
      if (f.type === "multiline") {
        const s = TextareaState.new({ rows: 5 });
        s.set_auto_grow(3, 12);
        this.inputs[f.key] = s;
      } else if (f.type === "text" || f.type === "number") {
        this.inputs[f.key] = InputState.new({ placeholder: f.type === "number" && f.default === null ? "blank" : "" });
      }
    }
  }

  startForm(mode, values, cx) {
    if (!this.fields) {
      this.note = { level: "error", text: "The form's fields have not loaded yet." };
      return cx.notify();
    }
    this.ensureInputs();
    const v = {};
    for (const f of this.fields) v[f.key] = values?.[f.key] ?? f.default;
    for (const [key, input] of Object.entries(this.inputs)) input.set_value(v[key] === null || v[key] === undefined ? "" : String(v[key]));
    this.form = { mode, prev: mode === "edit" ? this.name : null, values: v, errors: {}, general: null };
    this.view = "edit";
    this.confirm = null;
    this.saving = false;
    this.inputs.name?.focus();
    cx.notify();
  }

  visible(f, values) {
    if (!f.kinds.includes(values.kind)) return false;
    for (const [key, allowed] of Object.entries(f.show_when ?? {})) {
      if (!allowed.includes(String(values[key] ?? ""))) return false;
    }
    return true;
  }

  // Every field of the chosen kind, as apply takes them: text as strings,
  // numbers as numbers (or the typed text, for apply to refuse), blank as null.
  collect() {
    const values = {};
    const kind = this.form.values.kind;
    for (const f of this.fields) {
      if (f.key === "name" || !f.kinds.includes(kind)) continue;
      if (f.type === "text" || f.type === "multiline") values[f.key] = this.inputs[f.key].value();
      else if (f.type === "number") {
        const s = this.inputs[f.key].value().trim();
        values[f.key] = s === "" ? null : /^-?\d+$/.test(s) ? Number(s) : s;
      } else values[f.key] = this.form.values[f.key];
    }
    return { name: this.inputs.name.value().trim(), values };
  }

  save(cx) {
    if (this.saving) return;
    const form = this.form;
    const body = this.collect();
    if (form.mode === "edit") body.previous_name = form.prev;
    this.saving = true;
    cx.spawn(async (cx) => {
      try {
        const r = await this.call(["apply", "--json"], JSON.stringify(body));
        if (r.fail) {
          form.errors = r.fail.fields ?? {};
          const shown = new Set(this.fields.filter((f) => this.visible(f, form.values)).map((f) => f.key));
          const other = Object.entries(form.errors).filter(([k]) => !shown.has(k));
          form.general = !r.fail.fields ? r.fail.error
            : other.length ? other.map(([k, m]) => `${this.fields.find((f) => f.key === k)?.label ?? k}: ${m}`).join("\n") : null;
        } else {
          this.form = null;
          this.note = { level: "info", text: form.mode === "create" ? `Created ${r.ok.action.name}. It starts paused: enable it when it is ready.` : `Saved ${r.ok.action.name}.` };
          await this.loadList();
          this.openDetail(r.ok.action.name, cx);
        }
      } catch (e) {
        form.general = message(e);
      } finally {
        this.saving = false;
        cx.notify();
      }
    });
  }

  openText(cx) {
    if (!this.detail) return;
    const t = theme();
    this.code?.release();
    this.code = EditorState.new({
      language: "toml",
      value: this.detail.toml ?? "",
      font_family: settings().font_family,
      font_size: 12.5,
      syntax: { keyword: t.ansi5, string: t.ansi2, comment: t.dim, number: t.ansi3, boolean: t.ansi6, constant: t.ansi6, punctuation: t.muted },
    });
    this.textError = null;
    this.view = "text";
    this.confirm = null;
    cx.notify();
  }

  saveText(cx) {
    if (this.saving || !this.code) return;
    const name = this.name;
    const text = this.code.value();
    this.saving = true;
    cx.spawn(async (cx) => {
      try {
        const r = await this.call(["put-toml", name, "--json"], text);
        if (r.fail) this.textError = r.fail.error;
        else {
          this.note = { level: "info", text: `Saved ${name}.` };
          this.code?.release();
          this.code = null;
          await this.loadList();
          this.openDetail(name, cx);
        }
      } catch (e) {
        this.textError = message(e);
      } finally {
        this.saving = false;
        cx.notify();
      }
    });
  }

  // ---- drawing ----

  dot(a, t) {
    const s = state(a);
    const d = div().w(9).h(9).flex_none().rounded_full();
    if (s === "file error") return d.bg(t.blocked);
    if (s === "running") return d.bg(t.working).loop("opacity", { from: 0.35, to: 1, period: 1200 });
    if (s === "paused") return d.border_1().border_color(t.dim);
    if (s === "last run failed") return d.bg(t.ansi1);
    return d.bg(t.ansi2);
  }

  statusColor(status, t) {
    if (FAILED.has(status)) return t.ansi1;
    if (WARNED.has(status)) return t.blocked;
    if (status === "started") return t.working;
    return t.muted;
  }

  button(t, id, label, action, kind) {
    const primary = kind === "primary";
    const off = kind === "off";
    return h_flex().id(id).h(28).px(11).flex_none().items_center().justify_center().rounded(7).border_1()
      .border_color(primary ? t.solid : t.input).bg(primary ? t.solid : t.raised)
      .text_color(primary ? t.solid_foreground : off ? t.dim : t.foreground).text_size(12).font_medium()
      .when(!off, (el) => el.cursor_pointer().hover((s) => s.opacity(0.85)).on_click(action))
      .on_mouse_down("left", (_e, cx) => cx.stop_propagation())
      .child(label);
  }

  noteBar(t) {
    const n = this.note;
    if (!n) return null;
    const color = n.level === "error" ? t.ansi1 : n.level === "warning" ? t.blocked : t.muted;
    return h_flex().id("note").flex_none().gap(8).items_start().px(10).py(7).rounded(7).bg(t.raised).border_1().border_color(n.level === "info" ? t.border : color)
      .child(div().flex_1().min_w(0).text_size(12).text_color(color).child(n.text))
      .child(div().id("note-x").flex_none().px(4).text_size(12).text_color(t.dim).cursor_pointer().hover((s) => s.text_color(t.foreground))
        .on_mouse_down("left", (_e, cx) => cx.stop_propagation())
        .on_click((_e, cx) => { this.note = null; cx.notify(); }).child("×"));
  }

  confirmBar(t) {
    const name = this.confirm;
    if (!name) return null;
    const a = this.data?.actions.find((x) => x.name === name);
    return v_flex().id("confirm").flex_none().gap(8).p(12).rounded(8).bg(t.raised).border_1().border_color(t.blocked)
      .child(div().text_size(13).font_semibold().text_color(t.foreground).child(`Run ${name} now?`))
      .child(div().text_size(12).text_color(t.muted).child(
        a && !a.enabled
          ? "It is paused, so shepherd will refuse: enable it first."
          : "The daemon starts it within 30 seconds, exactly as a scheduled run, and records it. An action can touch backups and other live systems."))
      .child(h_flex().gap(8).justify_end()
        .child(this.button(t, "confirm-no", "Cancel", (_e, cx) => { this.confirm = null; cx.notify(); }))
        .child(this.button(t, "confirm-yes", "Run now", (_e, cx) => this.wake(name, cx), "primary")));
  }

  titleBar(t, title, right, sub) {
    const overlay = props()?.overlay;
    return v_flex().flex_none().gap(3)
      .child(h_flex().gap(8).items_center()
        .child(div().flex_1().min_w(0).truncate().text_size(15).font_semibold().text_color(t.foreground).child(title))
        .children(right.filter(Boolean))
        .when(Boolean(overlay), (el) => el.child(this.button(t, "close", "Close", () => closeOverlay(overlay)))))
      .when(Boolean(sub), (el) => el.child(sub));
  }

  daemonLine(t) {
    const d = this.data;
    if (!d) return null;
    const on = d.daemon?.running;
    const actions = d.actions ?? [];
    const enabled = actions.filter((a) => a.enabled).length;
    return h_flex().gap(6).flex_wrap().text_size(11.5).text_color(t.dim)
      .child(div().text_color(on ? t.muted : t.blocked).font_medium().child(on ? `daemon running${d.daemon.pid ? ` (pid ${d.daemon.pid})` : ""}` : "daemon not running"))
      .child(`· ${d.backend} backend · shepherd ${d.version} · ${actions.length} actions, ${enabled} enabled`);
  }

  renderMissing(t) {
    return v_flex().gap(12).child(this.titleBar(t, "Shepherd", []))
      .child(div().text_size(13).text_color(t.foreground).child(this.missing));
  }

  renderList(t, now) {
    const narrow = this.width < NARROW;
    const list = this.shown();
    const row = (a) => {
      const last = a.last_run;
      const lastAt = time(last?.at);
      const next = time(a.next_run);
      const busy = this.busy.has(a.name);
      const lastText = lastAt ? `${ago(lastAt, now)} · ${last.status}` : "never run";
      const nextText = a.error ? "" : !a.enabled ? "paused" : next ? until(next, now) : "";
      const lastCell = div().truncate().text_color(last && (FAILED.has(last.status) || WARNED.has(last.status)) ? this.statusColor(last.status, t) : t.muted).child(lastText);
      const open = h_flex().id(`open-${a.name}`).flex_1().min_w(0).gap(10).items_center().cursor_pointer()
        .on_click((_e, cx) => this.openDetail(a.name, cx))
        .child(this.dot(a, t));
      const main = narrow
        ? open.child(v_flex().flex_1().min_w(0).gap(1)
          .child(h_flex().gap(6).items_center()
            .child(div().flex_none().text_color(t.foreground).font_medium().child(a.name))
            .child(div().flex_1().min_w(0).truncate().text_size(11).text_color(t.dim).child(a.error ? "file error" : a.kind)))
          .child(div().truncate().text_size(11.5).text_color(t.muted).child(a.error ? a.error : `${a.schedule} · ${lastText}${nextText ? ` · next ${nextText}` : ""}`)))
        : open
          .child(div().flex_1().min_w(0).truncate().text_color(t.foreground).font_medium().child(a.name))
          .child(div().w(70).flex_none().truncate().text_color(t.dim).child(a.error ? "" : a.kind))
          .child(div().w(160).flex_none().truncate().text_color(a.error ? t.blocked : t.muted).child(a.error ? a.error : a.schedule))
          .child(div().w(150).flex_none().child(a.error ? "" : lastCell))
          .child(div().w(80).flex_none().truncate().text_color(t.dim).child(nextText));
      return h_flex().id(`row-${a.name}`).flex_none().min_h(narrow ? 46 : 36).px(10).gap(8).items_center().rounded(7)
        .hover((s) => s.bg(t.hover))
        .child(main)
        .child(a.error
          ? this.button(t, `text-${a.name}`, "Edit as text", (_e, cx) => this.openDetail(a.name, cx, "text"))
          : h_flex().flex_none().gap(6)
            .child(this.button(t, `toggle-${a.name}`, busy ? "…" : a.enabled ? "Pause" : "Enable", (_e, cx) => this.toggle(a, cx), busy ? "off" : undefined).w(66))
            .child(this.button(t, `run-${a.name}`, "Run", (_e, cx) => { this.confirm = a.name; cx.notify(); }, busy ? "off" : undefined)));
    };
    const head = narrow ? null
      : h_flex().flex_none().px(10).gap(10).text_size(11).text_color(t.dim)
        .child(div().w(9).flex_none())
        .child(div().flex_1().child("NAME"))
        .child(div().w(70).flex_none().child("KIND"))
        .child(div().w(160).flex_none().child("SCHEDULE"))
        .child(div().w(150).flex_none().child("LAST RUN"))
        .child(div().w(80).flex_none().child("NEXT"))
        .child(div().w(66 + 6 + 48 + 8).flex_none());
    return v_flex().flex_1().min_h(0).gap(10)
      .child(this.titleBar(t, "Shepherd", [
        this.button(t, "refresh", this.refreshing ? "…" : "Refresh", (_e, cx) => this.refresh(cx)),
        this.button(t, "new", "New action", (_e, cx) => this.startForm("create", null, cx), "primary"),
      ], this.daemonLine(t)))
      .child(h_flex().flex_none().h(34).px(10).items_center().rounded(8).border_1().border_color(t.input).bg(t.background)
        .on_mouse_down("left", (_e, cx) => cx.stop_propagation())
        .child(Input.new(this.search).flex_1().h_full().text_size(12.8).text_color(t.foreground)))
      .when(Boolean(this.listError), (el) => el.child(div().flex_none().text_size(12).text_color(t.ansi1).child(this.listError)))
      .when(Boolean(this.note), (el) => el.child(this.noteBar(t)))
      .when(Boolean(this.confirm), (el) => el.child(this.confirmBar(t)))
      .when(Boolean(head), (el) => el.child(head))
      .child(v_flex().id("rows").flex_1().min_h(0).overflow_y_scrollbar().track_scroll(this.scroll).text_size(12.5)
        .on_mouse_down("left", (_e, cx) => cx.stop_propagation())
        .when(!this.data && !this.listError, (el) => el.child(div().p(10).text_color(t.dim).child("Loading…")))
        .when(Boolean(this.data) && list.length === 0, (el) => el.child(div().p(10).text_color(t.dim).child(this.filter ? "No action matches the filter." : "No actions yet. New action makes one.")))
        .children(list.map(row)));
  }

  fact(t, label, value, color) {
    return h_flex().gap(10).items_start().text_size(12.5)
      .child(div().w(120).flex_none().text_color(t.dim).child(label))
      .child(div().flex_1().min_w(0).text_color(color ?? t.foreground).child(value === null || value === undefined || value === "" ? "–" : String(value)));
  }

  renderDetail(t, now) {
    const d = this.detail;
    const a = d?.action ?? this.data?.actions.find((x) => x.name === this.name);
    const busy = a && this.busy.has(a.name);
    const buttons = !a ? [] : a.error
      ? [this.button(t, "d-text", "Edit as text", (_e, cx) => this.openText(cx), d ? "primary" : "off")]
      : [
        this.button(t, "d-edit", "Edit", (_e, cx) => this.startForm("edit", d?.values, cx), d?.values ? undefined : "off"),
        this.button(t, "d-text", "Edit as text", (_e, cx) => this.openText(cx), d ? undefined : "off"),
        this.button(t, "d-run", "Run now", (_e, cx) => { this.confirm = a.name; cx.notify(); }, busy ? "off" : undefined),
        this.button(t, "d-toggle", busy ? "…" : a.enabled ? "Pause" : "Enable", (_e, cx) => this.toggle(a, cx), busy ? "off" : "primary"),
      ];
    const facts = [];
    if (a) {
      const st = state(a);
      facts.push(this.fact(t, "state", st, st === "file error" || st === "last run failed" ? t.ansi1 : st === "running" ? t.working : undefined));
      if (a.error) facts.push(this.fact(t, "error", a.error, t.ansi1));
      else {
        const next = time(a.next_run);
        const lastAt = time(a.last_run?.at);
        facts.push(this.fact(t, "kind", a.kind));
        facts.push(this.fact(t, "schedule", a.schedule));
        facts.push(this.fact(t, "next run", next ? `${clock(next, now)} (${until(next, now)})` : a.enabled ? "" : "paused"));
        facts.push(this.fact(t, "last run", lastAt ? `${clock(lastAt, now)} (${ago(lastAt, now)}) · ${a.last_run.status}${a.last_run.detail ? ` · ${a.last_run.detail}` : ""}` : "never run",
          a.last_run ? this.statusColor(a.last_run.status, t) : undefined));
        if (d?.values && this.fields) {
          for (const f of this.fields) {
            if (f.key === "name" || f.key === "kind" || !this.visible(f, d.values)) continue;
            let v = d.values[f.key];
            if (f.type === "bool") v = v ? "yes" : "no";
            else if (f.type === "choice" && f.options) {
              const labels = String(v ?? "").split(",").filter((x) => x !== "" || !f.multiple).map((x) => f.options.find((o) => o.value === x)?.label ?? x);
              v = labels.join(", ");
            }
            facts.push(this.fact(t, f.label, v));
          }
        }
      }
      facts.push(this.fact(t, "file", a.file, t.dim));
    }
    const runs = this.runs;
    const runRow = (r, i) => {
      const at = time(r.at);
      return h_flex().id(`run-${i}`).flex_none().gap(10).items_center().min_h(24).text_size(12)
        .child(div().w(this.width < NARROW ? 90 : 150).flex_none().truncate().text_color(t.muted).child(at ? (this.width < NARROW ? clock(at, now) : `${clock(at, now)} · ${ago(at, now)}`) : r.at))
        .child(div().w(80).flex_none().truncate().text_color(this.statusColor(r.status, t)).child(r.status))
        .when(this.width >= NARROW, (el) => el.child(div().w(64).flex_none().truncate().text_color(t.dim).child(r.trigger ?? "")))
        .child(div().w(54).flex_none().text_color(t.dim).child(duration(r.duration_secs)))
        .child(div().flex_1().min_w(0).truncate().text_color(t.muted).child(r.detail ?? ""));
    };
    return v_flex().flex_1().min_h(0).gap(10)
      .child(this.titleBar(t, this.name ?? "", [this.button(t, "back", "‹ Back", (_e, cx) => this.back(cx))], this.daemonLine(t)))
      .child(h_flex().flex_none().gap(8).flex_wrap().children(buttons))
      .when(Boolean(this.note), (el) => el.child(this.noteBar(t)))
      .when(Boolean(this.confirm), (el) => el.child(this.confirmBar(t)))
      .when(Boolean(this.detailError), (el) => el.child(div().flex_none().text_size(12).text_color(t.ansi1).child(this.detailError)))
      .child(v_flex().id("detail-body").flex_1().min_h(0).overflow_y_scrollbar().gap(14)
        .on_mouse_down("left", (_e, cx) => cx.stop_propagation())
        .child(v_flex().flex_none().gap(5).children(facts))
        .when(!a?.error, (el) => el.child(v_flex().flex_none().gap(4)
          .child(div().text_size(11).font_semibold().text_color(t.dim).child("RECENT RUNS"))
          .when(runs === null, (el) => el.child(div().text_size(12).text_color(t.dim).child("Loading…")))
          .when(Boolean(runs?.error), (el) => el.child(div().text_size(12).text_color(t.ansi1).child(runs.error)))
          .when(Array.isArray(runs) && runs.length === 0, (el) => el.child(div().text_size(12).text_color(t.dim).child("No runs recorded yet.")))
          .when(Array.isArray(runs), (el) => el.children(runs.map(runRow))))));
  }

  control(t, f, values, err) {
    const border = err ? t.ansi1 : t.input;
    if (f.type === "choice" || f.type === "bool") {
      const options = f.type === "bool" ? [{ value: false, label: "no" }, { value: true, label: "yes" }] : f.options ?? [];
      const picked = f.multiple ? new Set(String(values[f.key] ?? "").split(",").filter(Boolean)) : null;
      return h_flex().gap(6).flex_wrap().children(options.map((o) => {
        const on = f.multiple ? picked.has(o.value) : values[f.key] === o.value;
        return h_flex().id(`opt-${f.key}-${o.value}`).h(26).px(10).flex_none().items_center().rounded(6).border_1()
          .border_color(on ? t.accent : border).bg(on ? t.selected : t.background)
          .text_size(12).text_color(on ? t.foreground : t.muted).when(on, (el) => el.font_medium())
          .cursor_pointer().hover((s) => s.bg(t.hover))
          .on_click((_e, cx) => {
            if (f.multiple) {
              if (picked.has(o.value)) picked.delete(o.value);
              else picked.add(o.value);
              values[f.key] = f.options.map((x) => x.value).filter((x) => picked.has(x)).join(",");
            } else values[f.key] = o.value;
            cx.notify();
          })
          .child(o.label);
      }));
    }
    const input = this.inputs[f.key];
    if (f.type === "multiline") return Textarea.new(input).w_full().px(10).py(6).rounded(7).border_1().border_color(border).bg(t.background).text_size(12.8).text_color(t.foreground);
    return Input.new(input).h(32).px(10).rounded(7).border_1().border_color(border).bg(t.background).text_size(12.8).text_color(t.foreground)
      .map((el) => (f.type === "number" ? el.w(140) : el.w_full()));
  }

  renderForm(t) {
    const form = this.form;
    const values = form.values;
    let shown = this.fields.filter((f) => this.visible(f, values));
    // A new action starts from its kind: everything else depends on it.
    if (form.mode === "create") shown = [...shown.filter((f) => f.key === "kind"), ...shown.filter((f) => f.key !== "kind")];
    const field = (f) => {
      const err = form.errors[f.key];
      return v_flex().flex_none().gap(4)
        .child(h_flex().gap(4).text_size(12).font_medium().text_color(t.foreground).child(f.label).when(f.required && f.type !== "bool", (el) => el.child(div().text_color(t.dim).child("*"))))
        .child(this.control(t, f, values, err))
        .when(Boolean(f.help), (el) => el.child(div().text_size(11.5).text_color(t.dim).child(f.help)))
        .when(Boolean(err), (el) => el.child(div().text_size(12).text_color(t.ansi1).child(err)));
    };
    const title = form.mode === "create" ? "New action" : `Edit ${form.prev}`;
    const sub = div().text_size(11.5).text_color(t.dim).child(form.mode === "create"
      ? "Shepherd checks it the way the board's form does. A new action starts paused."
      : "Saving rewrites the file the way the board's form does: comments go (Edit as text keeps them). Enabled or paused stays as it is.");
    return v_flex().flex_1().min_h(0).gap(10)
      .child(this.titleBar(t, title, [], sub))
      .child(v_flex().id("form-body").flex_1().min_h(0).overflow_y_scrollbar().gap(14).pr(4)
        .on_mouse_down("left", (_e, cx) => cx.stop_propagation())
        .children(shown.map(field)))
      .when(Boolean(form.general), (el) => el.child(div().flex_none().text_size(12).text_color(t.ansi1).child(form.general)))
      .child(h_flex().flex_none().gap(8).justify_end()
        .child(this.button(t, "f-cancel", "Cancel", (_e, cx) => this.back(cx)))
        .child(this.button(t, "f-save", this.saving ? "Saving…" : form.mode === "create" ? "Create" : "Save", (_e, cx) => this.save(cx), this.saving ? "off" : "primary")));
  }

  renderText(t) {
    const sub = div().text_size(11.5).text_color(t.dim).child("The action's file. It must still load as this action to save; comments are kept. Renames go through Edit.");
    return v_flex().flex_1().min_h(0).gap(10)
      .child(this.titleBar(t, `Edit ${this.name} as text`, [], sub))
      .child(v_flex().flex_1().min_h(180).rounded(7).border_1().border_color(this.textError ? t.ansi1 : t.input).bg(t.background).overflow_hidden()
        .on_mouse_down("left", (_e, cx) => cx.stop_propagation())
        .child(this.code ? Editor.new(this.code).flex_1().min_h(0) : div().p(10).text_color(t.dim).child("Loading…")))
      .when(Boolean(this.textError), (el) => el.child(div().flex_none().text_size(12).text_color(t.ansi1).child(this.textError)))
      .child(h_flex().flex_none().gap(8).justify_end()
        .child(this.button(t, "t-cancel", "Cancel", (_e, cx) => this.back(cx)))
        .child(this.button(t, "t-save", this.saving ? "Saving…" : "Save", (_e, cx) => this.saveText(cx), this.saving ? "off" : "primary")));
  }

  render(cx) {
    const t = theme();
    const now = Date.now();
    const body = this.missing ? this.renderMissing(t)
      : this.view === "edit" && this.form ? this.renderForm(t)
        : this.view === "text" ? this.renderText(t)
          : this.view === "details" ? this.renderDetail(t, now)
            : this.renderList(t, now);
    return v_flex().id("shepherd").track_focus(this.handle).size_full().min_h(0).p(16).gap(10)
      .bg(props()?.overlay ? t.popover : t.surface).text_size(13).text_color(t.foreground)
      .on_size_change((size, cx) => { this.width = size.width; cx.notify(); })
      .on_key_down((e, cx) => {
        if (e.key !== "escape") return;
        cx.stop_propagation();
        this.back(cx);
      })
      .child(body);
  }
}
