"use strict";
/* AJX Console page. Everything shown comes from GET /api/state; every button is an `ajx` command or
   an edit of the trial files, made through the local console server. No external requests. */
(() => {
  const KEY = "ajx-console-token";
  const hash = new URLSearchParams(location.hash.slice(1));
  if (hash.get("token")) {
    sessionStorage.setItem(KEY, hash.get("token"));
    history.replaceState(null, "", location.pathname + location.search);
  }
  const TOKEN = sessionStorage.getItem(KEY) || "";
  const ACTIVE = ["running", "stopping", "checking", "starting", "busy"];
  const TABS = ["plan", "live", "results", "activity"];
  if (TABS.includes(hash.get("tab"))) sessionStorage.setItem("ajx-console-tab", hash.get("tab"));
  const ui = {
    tab: TABS.includes(sessionStorage.getItem("ajx-console-tab")) ? sessionStorage.getItem("ajx-console-tab") : "plan",
    snap: null, sigs: {}, offline: null, lastState: null,
    drafts: { settings: {} }, trialText: null, advancedOpen: false, composer: "", exports: {}, seenEntries: 0,
    openState: {}, inflight: false, acceptWarnings: false, acceptPlan: null, restartOpen: false, stopped: null,
  };

  // ------------------------------------------------------------------ helpers
  const $ = (sel) => document.querySelector(sel);

  function h(tag, attrs, ...kids) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === null || v === undefined || v === false) continue;
      if (k === "class") el.className = v;
      else if (k === "text") el.textContent = v;
      else if (k === "style") el.style.cssText = v;  // CSSOM: the page's CSP blocks inline style attributes
      else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v === true ? "" : String(v));
    }
    for (const kid of kids.flat(Infinity)) {
      if (kid === null || kid === undefined || kid === false) continue;
      el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
    }
    if (tag === "details" && attrs && attrs["data-key"]) {
      const key = attrs["data-key"];
      if (key in ui.openState) el.open = ui.openState[key];
      el.addEventListener("toggle", () => { ui.openState[key] = el.open; });
    }
    return el;
  }

  function clock(at) {
    if (!at) return "--:--";
    const d = new Date(at);
    return isNaN(d) ? "--:--" : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  }

  function dur(s) {
    if (s === null || s === undefined) return "";
    s = Math.max(0, Math.round(s));
    const m = Math.floor(s / 60);
    return m ? `${m}m ${String(s % 60).padStart(2, "0")}s` : `${s}s`;
  }

  function fileUrl(rel) {
    return "/files/" + String(rel).split("/").map(encodeURIComponent).join("/");
  }

  function link(rel, text) {
    return h("a", { href: fileUrl(rel), target: "_blank", rel: "noopener", text });
  }

  function byText(actor) {
    return { user: "by you", agent: "by your agent", outside: "outside the console", console: "by the console" }[actor] || "";
  }

  let toastTimer;
  function toast(msg, bad, action) {
    const el = $("#toast");
    el.replaceChildren(msg);
    if (action) el.append(h("button", { type: "button", class: "toast-act", onclick: () => { el.hidden = true; action.fn(); } }, action.label));
    el.className = "toast" + (bad ? " bad" : "");
    el.setAttribute("role", bad ? "alert" : "status");
    el.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { el.hidden = true; }, bad ? 9000 : 4500);
  }

  async function api(path, body) {
    const init = { method: body === undefined ? "GET" : "POST", cache: "no-store", credentials: "same-origin",
                   headers: { "X-AJX-Token": TOKEN } };
    if (body !== undefined) {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
    const res = await fetch(path, init);
    let data = {};
    try { data = await res.json(); } catch (e) { data = {}; }
    if (!res.ok) {
      const err = new Error(data.error || res.statusText || "request failed");
      err.status = res.status;
      throw err;
    }
    return data;
  }

  async function act(path, body, okMsg) {
    if (ui.inflight) return null;
    ui.inflight = true;
    document.body.classList.add("inflight");
    try {
      const res = await api(path, body || {});
      if (okMsg) toast(typeof okMsg === "function" ? okMsg(res) : okMsg);
      await refresh(true);
      return res;
    } catch (e) {
      toast(e.message, true);
      if (e.status === 409) await refresh(true);
      return null;
    } finally {
      ui.inflight = false;
      document.body.classList.remove("inflight");
    }
  }

  // ------------------------------------------------------------------ polling and rendering
  async function refresh(force) {
    try {
      ui.snap = await api("/api/state");
      ui.offline = null;
    } catch (e) {
      ui.offline = e.status === 403 ? "auth" : "offline";
    }
    render(force);
  }

  async function poll() {
    if (ui.stopped) return;
    try {
      await refresh(false);
    } finally {  // an error while drawing one update must not stop the ones after it
      const active = ui.snap && ui.snap.process && ACTIVE.includes(ui.snap.process.state);
      setTimeout(poll, active ? 1500 : 3000);
    }
  }

  const SIG = {
    plan: (s) => JSON.stringify([s.trial, s.plan, s.readiness, s.process.state, s.process.by, s.process.since,
                                 s.output, s.locks, s.pending_start, s.plan_model, s.plan_options]),
    live: (s) => JSON.stringify([s.runs, s.process, s.output, s.plan_model, s.plan_options && s.plan_options.harnesses]),
    results: (s) => JSON.stringify([s.runs.map((r) => [r.id, r.started, r.asks, r.verify, r.links, r.review_status,
                                                       r.complete, r.declared, r.teardown, r.result, r.strengths, r.gates,
                                                       r.journey, r.journey_provenance, r.cell, r.group, r.model, r.never_ran,
                                                       r.current, r.stages, r.execute && [r.execute.status, r.execute.interrupted]]),
                                    s.output.index, s.process.state, s.process.since, ui.exports, s.vocabulary]),
    activity: (s) => JSON.stringify([s.journal, s.agent]),
  };
  const RENDER = { plan: renderPlan, live: renderLive, results: renderResults, activity: renderActivity };

  function render(force) {
    if (ui.stopped) return renderStopped();
    renderHeader();
    document.body.classList.toggle("offline", Boolean(ui.offline));
    const banner = $("#offline");
    if (banner) {
      banner.hidden = !ui.offline;
      banner.textContent = ui.offline === "auth"
        ? "This link no longer works (the console restarted). Run `ajx console open <trial>` and use the new link."
        : `The console is not responding. Showing the last update from ${clock(ui.snap && ui.snap.generated_at)}; actions are off. A run keeps going without it; reopen it with \`ajx console open <trial>\`.`;
    }
    const s = ui.snap;
    if (!s) return;
    if (ui.acceptPlan !== s.plan.sha) { ui.acceptWarnings = false; ui.acceptPlan = s.plan.sha; }
    if (ui.tab === "activity") ui.seenEntries = s.journal.cursor;
    autoSwitch(s);
    renderTabLabels(s);
    renderTab(ui.tab, force);
  }

  function renderTab(name, force) {
    const s = ui.snap;
    if (!s) return;
    const sig = SIG[name](s);
    if (!force && ui.sigs[name] === sig) return;
    ui.sigs[name] = sig;
    const el = document.getElementById("tab-" + name);
    const focus = captureFocus(el);
    const scrollers = [...el.querySelectorAll("[data-scroll]")].map((n) => [n.dataset.scroll, n.scrollTop, n.scrollHeight - n.scrollTop - n.clientHeight < 8]);
    const nodes = RENDER[name](s);
    if (!(nodes.length === 1 && el.childNodes.length === 1 && el.firstChild === nodes[0])) el.replaceChildren(...nodes);
    for (const [key, top, atEnd] of scrollers) {
      const n = el.querySelector(`[data-scroll="${CSS.escape(key)}"]`);
      if (n) n.scrollTop = atEnd ? n.scrollHeight : top;
    }
    restoreFocus(el, focus);
    if (name === "plan") updateDirty();
  }

  function captureFocus(root) {
    const a = document.activeElement;
    if (!a || !root.contains(a) || !a.dataset.key) return null;
    return { key: a.dataset.key, start: a.selectionStart, end: a.selectionEnd };
  }

  function restoreFocus(root, f) {
    if (!f) return;
    const el = root.querySelector(`[data-key="${CSS.escape(f.key)}"]`);
    if (!el) return;
    el.focus({ preventScroll: true });
    try { if (f.start !== null && f.start !== undefined) el.setSelectionRange(f.start, f.end); } catch (e) { /* number inputs */ }
  }

  function setTab(name) {
    ui.tab = name;
    sessionStorage.setItem("ajx-console-tab", name);
    document.querySelectorAll("#tabs button").forEach((b) => b.setAttribute("aria-selected", String(b.dataset.tab === name)));
    document.querySelectorAll(".tab").forEach((sec) => { sec.hidden = sec.id !== "tab-" + name; });
    if (name === "activity" && ui.snap) ui.seenEntries = ui.snap.journal.cursor;
    if (ui.snap) renderTabLabels(ui.snap);
    renderTab(name, true);
  }

  // Reports by output folder, attempt and render time, so a fresh folder's first report counts as new.
  function reportKeys(s) {
    return s.runs.filter((r) => r.links && r.links.report)
      .map((r) => `${s.output.dir}|${r.id}|${((r.stages || []).find((x) => x.name === "render") || {}).at || ""}`);
  }

  function stoppedLastJob(s) {  // the user asked the latest job to stop (or restart)
    const entries = (s.journal && s.journal.entries) || [];
    const start = [...entries].reverse().find((e) => e.kind === "job_started");
    return Boolean(start) && entries.some((e) => e.n > start.n && (e.kind === "stop_requested" || e.kind === "restart_requested"));
  }

  function autoSwitch(s) {
    const prev = ui.lastState, cur = s.process.state;
    ui.lastState = cur;
    if (!ui.reportsBefore) ui.reportsBefore = new Set(reportKeys(s));
    const before = ui.reportsBefore;  // the reports that existed before this run started
    if (cur !== "running" && cur !== "stopping") ui.reportsBefore = new Set(reportKeys(s));
    if (!prev || prev === cur) return;
    const editing = anyDirty(s) || (document.activeElement && document.activeElement.dataset && document.activeElement.dataset.key);
    if (cur === "running" && ui.tab === "plan" && !editing) setTab("live");
    else if (cur === "running" && ui.tab === "plan") toast("The run started. Open the Live tab to follow it.");
    else if (prev === "running" && cur === "idle" && ui.tab === "live" && !stoppedLastJob(s)
             && reportKeys(s).some((k) => !before.has(k))) setTab("results");  // a new report; after a Stop, Live shows the cleanup
  }

  function renderTabLabels(s) {
    const ready = s.runs.filter((r) => r.complete && r.links.report).length;
    const unseen = s.journal.entries.filter((e) => e.n > ui.seenEntries && e.actor === "agent").length;
    const extra = { live: ACTIVE.includes(s.process.state) ? "\u25cf" : "", results: ready ? String(ready) : "",
                    activity: ui.tab !== "activity" && unseen ? String(unseen) : "" };
    document.querySelectorAll("#tabs button").forEach((b) => {
      const name = b.dataset.tab;
      const label = { live: "running", results: "report(s) ready", activity: "new note(s) from your agent" }[name];
      b.replaceChildren(name[0].toUpperCase() + name.slice(1),
                        extra[name] ? h("span", { class: "count", text: extra[name], "aria-label": extra[name] === "\u25cf" ? label : `${extra[name]} ${label}` }) : "");
    });
  }

  function chip(text, cls, title) {
    return h("span", { class: "chip " + (cls || ""), title: title || null }, h("span", { class: "dot" }), text);
  }

  function renderHeader() {
    const s = ui.snap;
    if (s) {
      $("#trialname").replaceChildren(h("b", { text: s.trial.product || "trial" }), " \u00b7 ", s.trial.id || "",
                                      " \u00b7 ", h("span", { class: "mono", text: s.trial.short }));
    }
    const chips = [];
    if (s) {
      const p = s.process;
      const label = { idle: "Idle", running: p.job === "stages" ? "Recovery running" : "Run in progress",
                      stopping: "Stopping: cleaning up", checking: "Checking readiness",
                      starting: "Checking, then starting", busy: "Rebuilding reports" }[p.state] || p.state;
      chips.push(chip(label, p.state === "stopping" ? "warn" : ACTIVE.includes(p.state) ? "live" : ""));
      const a = s.agent;
      chips.push(a.state === "watching"
        ? chip("Your agent is watching", "live", "It sees changes here within seconds.")
        : a.state === "seen"
          ? chip(`Your agent last checked ${clock(a.last_seen)}`, "", "It catches up from this page the next time you message it in chat.")
          : chip("Your agent hasn't checked yet", "", "Tell your agent to run `ajx console since` or paste a message into your chat."));
    }
    if (ui.offline) {
      chips.push(chip(ui.offline === "auth" ? "Link expired: reopen it with ajx console" : "Console not responding", "bad",
                      "Run `ajx console open <trial>` again; a run keeps going without the console."));
    }
    if (s && !ui.offline) chips.push(h("button", { class: "chipbtn", onclick: stopConsole, title: "Stops this page's server. Runs keep going." }, "Stop console"));
    $("#chips").replaceChildren(...chips);
  }

  async function stopConsole() {
    const where = ui.snap ? ui.snap.trial.short.replace(/\/trial\.toml$/, "") : "<trial>";
    if (!confirm(`Stop this console's server?\n\nRuns keep going. Reopen it any time with:\najx console open ${where}`)) return;
    try {
      const res = await api("/api/shutdown", {});
      ui.stopped = res.reopen || `ajx console open ${where}`;
    } catch (e) {
      toast(e.message, true);
      return;
    }
    renderStopped();
  }

  function renderStopped() {
    $("#chips").replaceChildren(chip("Console stopped", ""));
    document.body.classList.remove("offline");
    const banner = $("#offline");
    if (banner) banner.hidden = true;
    for (const name of TABS) {
      const el = document.getElementById("tab-" + name);
      el.hidden = name !== "plan";
      el.replaceChildren();
    }
    document.getElementById("tab-plan").replaceChildren(h("div", { class: "card empty" },
      h("h2", { text: "The console is stopped" }),
      h("p", { text: "Runs keep going without it. Reopen it with:" }),
      h("pre", { class: "logtail", text: ui.stopped })));
  }

  // ------------------------------------------------------------------ plan tab
  function settingsBase(s) {
    const t = s.trial;
    const cells = {};
    for (const c of t.cells || []) if (c.model_editable) cells[c.raw_index] = c.model || "";
    return { timeout_minutes: t.timeout_seconds ? +(t.timeout_seconds / 60).toFixed(2) : "", repetitions: t.repetitions || 1,
             max_budget_usd: t.max_budget_usd ?? "", reporter_model: (t.reporter || {}).model || "", cell_models: cells };
  }

  function settingsChanges(s) {
    const base = settingsBase(s), d = ui.drafts.settings, out = {};
    const same = (a, b) => (String(a).trim() === "" && String(b).trim() === "") || String(a).trim() === String(b).trim()
      || (String(a).trim() !== "" && String(b).trim() !== "" && Number(a) === Number(b));
    for (const k of ["timeout_minutes", "repetitions", "max_budget_usd"]) if (k in d && !same(d[k], base[k])) out[k] = d[k];
    if ("reporter_model" in d && String(d.reporter_model).trim() !== String(base.reporter_model).trim()) out.reporter_model = d.reporter_model;
    const cells = {};
    for (const [i, m] of Object.entries(d.cell_models || {})) if (String(m) !== String(base.cell_models[i] ?? "")) cells[i] = m;
    if (Object.keys(cells).length) out.cell_models = cells;
    return out;
  }

  function draftValue(s, key) {
    return key in ui.drafts.settings ? ui.drafts.settings[key] : settingsBase(s)[key];
  }

  function anyDirty(s) {
    const promptDirty = ui.drafts.prompt !== undefined && ui.drafts.prompt !== s.trial.prompt.text;
    const textDirty = ui.trialText && ui.trialText.draft !== undefined && ui.trialText.draft !== ui.trialText.text;
    return promptDirty || textDirty || Object.keys(settingsChanges(s)).length > 0 || Coach.dirty();
  }

  function updateDirty() {
    const s = ui.snap;
    if (!s) return;
    const promptDirty = ui.drafts.prompt !== undefined && ui.drafts.prompt !== s.trial.prompt.text;
    const settingsDirty = Object.keys(settingsChanges(s)).length > 0;
    const textDirty = ui.trialText && ui.trialText.draft !== undefined && ui.trialText.draft !== ui.trialText.text;
    for (const [id, on] of [["prompt-actions", promptDirty], ["settings-actions", settingsDirty], ["text-actions", textDirty]]) {
      const el = document.getElementById(id);
      if (el) el.hidden = !on;
    }
    Coach.touch();
    const edits = anyDirty(s);
    for (const b of document.querySelectorAll("[data-clean]")) b.disabled = b.dataset.baseOff === "1" || edits;
    for (const n of document.querySelectorAll("[data-clean-note]")) n.hidden = !edits;
    const st = startState(s), btn = $("#start-btn"), reason = $("#start-reason");
    if (btn) {
      btn.textContent = st.label;
      btn.disabled = st.disabled;
    }
    if (reason) {
      reason.textContent = st.reason;
      reason.className = "reason" + (st.disabled ? " block" : "");
    }
  }

  function startState(s) {
    const p = s.process, out = s.output, r = s.readiness;
    if (s.trial.error) return { disabled: true, label: "Check and start", reason: "Fix the trial file error first." };
    if (p.state === "running" || p.state === "stopping") {
      return { disabled: true, label: "Running", reason: `A run is in progress (started ${byText(p.by)} at ${clock(p.since)}). Watch it in Live.` };
    }
    if (p.state === "checking") return { disabled: true, label: "Checking\u2026", reason: "Checking readiness: ajx validate, then ajx doctor." };
    if (p.state === "starting") return { disabled: true, label: "Checking, then starting\u2026", reason: "It starts by itself if both checks pass." };
    if (p.state === "busy") return { disabled: true, label: "Busy", reason: "Rebuilding the reports." };
    if (anyDirty(s)) return { disabled: true, label: "Check and start", reason: "Save or discard your changes first." };
    if (out.blocker) {
      return { disabled: true, label: out.blocker_kind === "complete" ? "Finished" : "Check and start",
               reason: out.blocker_kind === "complete" ? "Every planned attempt has finished. Use Run again to start another." : out.blocker };
    }
    if (!s.plan.complete) return { disabled: true, label: "Check and start", reason: "The starting files or trial plugins are too large to fingerprint for approval. Start this trial with ajx run." };
    const ready = r.current && r.status === "ready";
    if (r.current && r.needs_ack && (r.warnings || []).length && !ui.acceptWarnings) {
      return { disabled: true, label: "Check and start", reason: "This task acts in an account: accept the readiness warnings above first." };
    }
    if (out.resumable) {
      if ((out.unstarted || []).length) {
        return { disabled: false, label: `Resume: ${out.unstarted.length} attempt(s) left`,
                 reason: "Runs the attempts that have not started, and finishes any unfinished report. Finished or stopped attempts are never run again." };
      }
      if ((out.pending_reports || []).length) {
        return { disabled: false, label: `Finish the report for ${out.pending_reports.join(", ")}`,
                 reason: "The worker already stopped. This writes the missing report; it does not run the task again (Run again does)." };
      }
      return { disabled: false, label: "Resume", reason: "Finishes the remaining stages; a finished or stopped attempt is never run again." };
    }
    return {
      disabled: false, label: ready ? "Start" : "Check and start",
      reason: ready ? `Ready for this plan (checked at ${clock(r.checked_at)}).`
        : r.current && r.status === "not_ready" ? "The last check failed. This checks again and starts only if it passes."
          : "Runs ajx validate and ajx doctor first, and starts only if both pass.",
    };
  }

  function renderPlan(s) {
    if (s.plan_model && s.plan_options) return Coach.render(s);  // older servers and the Go server: the classic cards
    const main = [h("h1", { text: "Review the plan, then start" }),
                  h("p", { class: "sub" }, "Everything here comes from ", h("span", { class: "mono", text: s.trial.short }),
                    ". Your agent and this page edit the same files.")];
    if (s.trial.error) {
      main.push(h("div", { class: "card err" }, h("h2", { text: "AJX cannot load this trial file" }), h("pre", { text: s.trial.error }),
                  h("p", { class: "note", text: "Fix it under Advanced, or ask your agent. Nothing can start until the trial loads." })));
    }
    main.push(promptCard(s), checksCard(s), placeCard(s), agentsCard(s), advancedCard(s));
    return [h("div", { class: "grid" }, h("div", {}, ...main), h("aside", { class: "side" }, startCard(s)))];
  }

  function promptCard(s) {
    const p = s.trial.prompt, lock = s.locks.prompt;
    const dirty = ui.drafts.prompt !== undefined && ui.drafts.prompt !== p.text;
    const ta = h("textarea", { rows: Math.min(14, Math.max(5, (p.text.match(/\n/g) || []).length + 3)), "data-key": "prompt",
                               readonly: lock ? true : null, "aria-label": "Task prompt",
                               oninput: (e) => { ui.drafts.prompt = e.target.value; updateDirty(); } });
    ta.value = dirty ? ui.drafts.prompt : p.text;
    const lint = (p.lint || []).map((l) => h("li", {}, h("b", { text: `\u201c${l.match}\u201d` }), ` ${l.why}.`));
    return h("div", { class: "card" },
      h("div", { class: "row spread" }, h("h2", { text: "The task the worker receives" }),
        h("span", { class: "badge blue", text: "the task the worker is given" })),
      ta,
      lock ? h("p", { class: "lock", text: lock }) : null,
      lint.length ? h("div", { class: "warnbox" }, "Check the wording. It can turn the task into a guided or observed one:", h("ul", { class: "list" }, lint)) : null,
      h("div", { class: "row", id: "prompt-actions", hidden: !dirty },
        h("button", { class: "primary", onclick: () => savePrompt(s) }, "Save task"),
        h("button", { onclick: () => { delete ui.drafts.prompt; renderTab("plan", true); } }, "Discard")),
      h("div", { class: "row", style: "margin-top:8px" },
        h("button", { class: "link", onclick: () => askAgent("About the task prompt: ") }, "Ask my agent to change it"),
        h("span", { class: "tiny muted", text: `${p.short || ""}` })),
      h("p", { class: "note", text: "The worker is told only this. It can still read its starting files" + ((s.trial.cells || []).some((c) => !c.place.sandbox) ? " and anything else on your machine that you can read." : ".") + " Keep it neutral: the user's goal and materials, with no hints about expected problems and no mention of testing or AJX." }));
  }

  async function savePrompt(s) {
    const res = await act("/api/prompt", { text: ui.drafts.prompt, base: s.trial.prompt.sha256 }, "Task saved. The plan changed, so it is checked again before it starts.");
    if (res) delete ui.drafts.prompt;
    renderTab("plan", true);
  }

  function checksCard(s) {
    const checks = s.trial.checks || [];
    return h("div", { class: "card" },
      h("h2", { text: "How success is checked" }),
      checks.length
        ? h("ul", { class: "list" }, checks.map((c) => h("li", {},
            h("div", { class: "row" }, h("b", { text: c.name }), h("span", { class: "badge", text: c.type }),
              c.location === "environment" ? h("span", { class: "badge warn", text: "runs inside the worker's environment" }) : null),
            h("div", { class: "cmd", text: c.detail }))))
        : h("div", { class: "warnbox", text: "No success check yet. Without one, AJX can only report what the worker claimed. Ask your agent to add a check of the actual result." }),
      h("p", { class: "note", text: "AJX runs these after the worker stops. The worker never sees them." }),
      h("button", { class: "link", onclick: () => askAgent("About the success check: ") }, "Ask my agent to change it"));
  }

  function placeCard(s) {
    const cells = s.trial.cells || [];
    if (!cells.length) return h("div", { class: "card" }, h("h2", { text: "Where it runs" }), h("p", { class: "muted", text: "Unknown until the trial file loads." }));
    const seen = new Map();
    for (const c of cells) {
      const key = c.place.label + "|" + c.signin.profile;
      if (!seen.has(key)) seen.set(key, { c, ids: [] });
      seen.get(key).ids.push(c.id);
    }
    const blocks = [...seen.values()].map(({ c, ids }) => h("div", { class: "stack", style: "margin-top:8px" },
      h("div", { class: "row" }, h("h3", { text: c.place.label, style: "margin:0" }),
        c.place.sandbox ? h("span", { class: "badge blue", text: "runs in a container" }) : h("span", { class: "badge warn", text: "not a sandbox" }),
        cells.length > 1 ? h("span", { class: "tiny muted", text: ids.join(", ") }) : null),
      h("p", { class: "small", style: "margin:0", text: c.place.summary }),
      c.place.facts.length ? h("ul", { class: "list small" }, c.place.facts.map((f) => h("li", { text: f }))) : null,
      c.place.sandbox ? h("p", { class: "small", style: "margin:0", text: "Separate from your machine's files and login. It still uses its network access and the credentials you give it." }) : null,
      h("p", { class: "small", style: "margin:0" }, h("b", { text: "Sign-in: " }), `${c.signin.profile || "default"} \u2014 ${c.signin.description || ""}`),
      h("p", { class: "tiny muted", style: "margin:0" }, `Credentials come from the terminal that opened this console${s.console.started_at ? ` at ${clock(s.console.started_at)}` : ""}. If you signed in or exported keys since, run ajx console stop and open it again.`,
        s.readiness.current && s.readiness.status === "ready" ? ` Checked by ajx doctor at ${clock(s.readiness.checked_at)}.` : "")));
    const sandboxed = cells.some((c) => c.place.sandbox);
    const inside = (s.trial.checks || []).some((c) => c.location === "environment");
    const outside = (s.trial.checks || []).some((c) => c.location !== "environment");
    const diagram = sandboxed
      ? h("div", { class: "diagram" },
          h("div", { class: "zone" }, h("h4", { text: "Your machine" }), ["you", "your agent", "this console", outside ? "success checks" : null, "reporter"].filter(Boolean).map((n) => h("span", { class: "node", text: n }))),
          h("div", { class: "zone" }, h("h4", { text: "The container" }), h("span", { class: "node hot", text: "the worker" }), ["setup and cleanup commands", inside ? "checks marked as inside" : null, "narration"].filter(Boolean).map((n) => h("span", { class: "node", text: n }))))
      : h("div", { class: "diagram one" },
          h("div", { class: "zone" }, h("h4", { text: "Your machine: everything runs here, as you" }),
            ["you", "your agent", "this console"].map((n) => h("span", { class: "node", text: n })),
            h("span", { class: "node hot", text: "the worker, in a fresh folder" }),
            ["success checks", "reporter"].map((n) => h("span", { class: "node", text: n }))));
    return h("div", { class: "card" }, h("h2", { text: "Where it runs" }), ...blocks, diagram,
      h("p", { class: "note", text: "Changing where it runs is a trial-file change. Ask your agent; a container needs a prepared image and its own credentials (an API key or cloud keys, not a desktop login)." }));
  }

  function agentsCard(s) {
    const t = s.trial, lock = s.locks.settings;
    const rows = (t.cells || []).map((c) => {
      const listId = `models-${c.raw_index}`;
      const model = c.model_editable
        ? [h("input", { type: "text", list: listId, "data-key": `cell-${c.raw_index}`, placeholder: "agent default", disabled: lock ? true : null,
                        spellcheck: "false", value: (ui.drafts.settings.cell_models || {})[c.raw_index] ?? (c.model || ""),
                        oninput: (e) => { ui.drafts.settings.cell_models = { ...(ui.drafts.settings.cell_models || {}), [c.raw_index]: e.target.value }; updateDirty(); } }),
           h("datalist", { id: listId }, (c.model_suggestions || []).map((m) => h("option", { value: m }))),
           h("small", { text: modelHint(c.harness, c.model_suggestions) })]
        : h("span", { class: "mono", text: c.model || "agent default" });
      return h("li", {},
        h("div", { class: "row spread" }, h("div", { class: "row" }, h("b", { text: c.harness }), h("span", { class: "tiny muted", text: c.id }),
            c.installed ? null : h("span", { class: "badge bad", text: "not installed" }),
            c.verified_live ? null : h("span", { class: "badge warn", text: "adapter not yet verified against a live run", title: "AJX's adapter for this agent follows its documented output format but has not been confirmed with a live run." }))),
        h("label", { class: "field" }, h("span", { text: "Model" }), model));
    });
    const rep = t.reporter || {};
    const repModel = [h("input", { type: "text", list: "models-reporter", "data-key": "reporter-model", placeholder: "agent default",
                                   disabled: lock ? true : null, spellcheck: "false", value: draftValue(s, "reporter_model"),
                                   oninput: (e) => { ui.drafts.settings.reporter_model = e.target.value; updateDirty(); } }),
                      h("datalist", { id: "models-reporter" }, (rep.model_suggestions || []).map((m) => h("option", { value: m }))),
                      h("small", { text: modelHint(rep.harness, rep.model_suggestions) })];
    return h("div", { class: "card" },
      h("h2", { text: "Worker and reporter" }),
      h("ul", { class: "list" }, rows,
        h("li", {}, h("div", { class: "row" }, h("b", { text: rep.harness || "reporter" }), h("span", { class: "tiny muted", text: "reporter: writes the analysis after the task, kept the same across runs" })),
          h("label", { class: "field" }, h("span", { text: "Reporter model" }), repModel))),
      h("p", { class: "note", text: "Save model changes with \u201cSave changes\u201d under Your decisions." }));
  }

  function modelHint(harness, suggestions) {
    const known = (suggestions || []).slice(0, 5).join(", ");
    return "Leave empty for the agent's default." + (known ? ` Suggestions: ${known}.` : "")
      + " Other names must be ones this agent accepts, without spaces; an unknown name makes the worker exit at once.";
  }

  function advancedCard(s) {
    const t = s.trial;
    const details = h("details", { id: "advanced", open: ui.advancedOpen ? true : null,
                                   ontoggle: (e) => { ui.advancedOpen = e.target.open; if (e.target.open) loadTrialText(s); } });
    details.append(h("summary", { text: "Advanced: details and trial.toml" }));
    const kv = h("dl", { class: "kv" },
      h("dt", { text: "Trial file" }), h("dd", { class: "mono", text: t.short }),
      h("dt", { text: "Output folder" }), h("dd", { class: "mono", text: s.output.short }),
      h("dt", { text: "Workspaces" }), h("dd", { class: "mono", text: t.workspace_root || "" }),
      h("dt", { text: "Starting files" }), h("dd", { class: "mono", text: t.fixture_dir || "none" }),
      h("dt", { text: "Materials" }), h("dd", { text: (t.materials || []).join(", ") || "none listed" }),
      h("dt", { text: "Setup / cleanup" }), h("dd", { text: `${(t.setup || []).length} setup, ${(t.teardown || []).length} cleanup, ${(t.preflight || []).length} preflight command(s)` }),
      h("dt", { text: "Task account" }), h("dd", { class: "mono", text: t.identity_cmd || "none (no cloud or SaaS account declared)" }),
      h("dt", { text: "Parallel" }), h("dd", { text: String(t.parallel || 1) + ((t.parallel || 1) > 1 ? " (Stop waits for running attempts to finish)" : "") }));
    const editor = [];
    if (ui.trialText) {
      const tt = ui.trialText;
      const stale = tt.sha256 !== t.sha256;
      const ta = h("textarea", { rows: 22, class: "mono", "data-key": "trialtext", "aria-label": "trial.toml", readonly: s.locks.settings ? true : null,
                                 oninput: (e) => { tt.draft = e.target.value; updateDirty(); } });
      ta.value = tt.draft !== undefined ? tt.draft : tt.text;
      editor.push(stale ? h("div", { class: "warnbox" }, "trial.toml changed on disk since you opened it. ",
        h("button", { class: "link", onclick: () => { if (tt.draft === undefined || confirm("Discard your unsaved edits and load the file from disk?")) { ui.trialText = null; loadTrialText(s, true); } } }, "Reload")) : null,
        ta,
        h("div", { class: "row", id: "text-actions", hidden: !(tt.draft !== undefined && tt.draft !== tt.text) },
          h("button", { class: "primary", onclick: () => saveTrialText() }, "Save trial.toml"),
          h("button", { onclick: () => { delete tt.draft; renderTab("plan", true); } }, "Discard")),
        h("p", { class: "note", text: "Saved only if AJX can load the result; otherwise the file stays as it was." }));
    } else if (ui.advancedOpen) {
      editor.push(h("p", { class: "muted", text: "Loading trial.toml\u2026" }));
    }
    details.append(kv, h("div", { class: "hr" }), ...editor.filter(Boolean));
    return h("div", { class: "card" }, details);
  }

  async function loadTrialText(s, force) {
    if ((ui.trialText && !force) || ui.trialTextLoading) return;
    ui.trialTextLoading = true;
    try {
      const res = await api("/api/trial-text");
      ui.trialText = { text: res.text, sha256: res.sha256 };
    } catch (e) {
      toast(e.message, true);
      ui.advancedOpen = false;
    } finally {
      ui.trialTextLoading = false;
    }
    renderTab("plan", true);
  }

  async function saveTrialText() {
    const tt = ui.trialText;
    const res = await act("/api/trial-text", { text: tt.draft, base: tt.sha256 }, "trial.toml saved. The plan changed, so it is checked again before it starts.");
    if (res) ui.trialText = null;
    if (ui.advancedOpen) loadTrialText(ui.snap, true);
  }

  function startCard(s) {
    const t = s.trial, out = s.output, lock = s.locks.settings;
    const num = (key, label, hint, attrs) => h("label", { class: "field" }, h("span", { text: label }),
      h("input", { type: "number", "data-key": key, disabled: lock ? true : null, ...attrs, value: draftValue(s, key),
                   oninput: (e) => { ui.drafts.settings[key] = e.target.value; updateDirty(); } }),
      hint ? h("small", { text: hint }) : null);
    const budgetHint = (t.cells || []).some((c) => c.budget_enforced)
      ? "Optional. Claude Code enforces it; other agents ignore it." : "Optional. None of these agents enforces a budget.";
    const outStatus = !out.started ? "Not started yet."
      : `${out.complete} of ${out.planned} planned attempt(s) finished` + (out.started > out.complete ? `, ${out.started - out.complete} in progress or stopped.` : ".");
    const st = startState(s);
    return h("div", { class: "card" },
      h("h2", { text: "Your decisions" }),
      h("div", { class: "inline" },
        num("timeout_minutes", "Time limit (minutes)", null, { min: 1, step: 1 }),
        num("repetitions", "Attempts", null, { min: 1, step: 1 })),
      h("p", { class: "note", text: "AJX stops the worker at the time limit. Each attempt is a separate fresh run of the same task. Reporting adds time afterwards." }),
      num("max_budget_usd", "Budget per attempt (USD)", budgetHint, { min: 0, step: 0.5, placeholder: "none" }),
      lock ? h("p", { class: "lock", text: lock }) : null,
      h("div", { class: "row", id: "settings-actions", hidden: true },
        h("button", { class: "primary", onclick: () => saveSettings(s) }, "Save changes"),
        h("button", { onclick: () => { ui.drafts.settings = {}; renderTab("plan", true); } }, "Discard")),
      h("div", { class: "hr" }),
      h("h3", { text: "Output folder" }),
      h("p", { class: "mono small", style: "margin:0;word-break:break-all", text: out.short }),
      h("p", { class: "small", style: "margin:4px 0", text: outStatus }),
      runAgainBox(s),
      h("div", { class: "hr" }),
      h("h3", { text: "Readiness" }),
      readinessView(s),
      h("button", { id: "start-btn", class: "primary start-btn", disabled: st.disabled ? true : null, "aria-describedby": "start-reason",
                    onclick: (e) => { e.currentTarget.disabled = true; act("/api/start", { plan: s.plan.sha, accept_warnings: ui.acceptWarnings }, "Checking readiness, then starting\u2026"); } }, st.label),
      h("p", { id: "start-reason", class: "reason" + (st.disabled ? " block" : ""), text: st.reason }),
      h("p", { class: "note" }, "Starting approves plan ",
        h("span", { class: "mono", title: "A fingerprint of " + (s.plan.covers || []).join(", ") + ". Any change to them gives a new plan id.", text: s.plan.short.slice(0, 7) }),
        `: ${(s.plan.covers || []).join(", ")}. Any later change needs a new approval.`),
      s.plan.approval.state !== "none" ? h("p", { class: "tiny muted", text: `Last approval: ${s.plan.approval.state === "valid" ? "this plan" : "an earlier plan"}, ${s.plan.approval.via === "chat" ? "in chat" : "in the console"} at ${clock(s.plan.approval.at)}.` }) : null,
      h("p", { class: "note", text: (t.cells || []).some((c) => !c.place.sandbox)
        ? "This worker runs as you on this machine, without asking before each command. It can read and change files you can, and use your network and any accounts you are signed in to."
        : "The worker runs commands without asking you first, inside its container." }),
      h("p", { class: "note", text: "Model calls by the worker and the reporter can cost money." }),
      h("p", { class: "cmd", text: `runs: ajx run ${t.short}` }));
  }

  function runAgainBox(s) {
    const out = s.output, t = s.trial;
    if (!out.attempts_exist || ACTIVE.includes(s.process.state)) return null;
    const changed = out.blocker_kind === "changed";
    const blocked = !s.plan.complete || (s.readiness.current && s.readiness.needs_ack && (s.readiness.warnings || []).length && !ui.acceptWarnings);
    const edits = anyDirty(s), why = edits ? "Save or discard your changes first." : null;
    const where = t.short.replace(/\/trial\.toml$/, "");
    const go = (mode, msg) => act("/api/again", { mode, plan: s.plan.sha, accept_warnings: ui.acceptWarnings }, msg);
    return h("div", { class: "againbox" },
      h("h3", { text: "Run again" }),
      h("p", { class: "small", style: "margin:0 0 8px", text: "Starts a new attempt of the same task after the readiness check. Earlier attempts stay exactly as they are; nothing runs twice." }),
      h("div", { class: "row" },
        h("button", { class: "primary", disabled: changed || blocked || edits ? true : null, "data-clean": "", "data-base-off": changed || blocked ? "1" : null,
                      title: why || (changed ? "The settings changed since these attempts; run again in a fresh folder." : null),
                      onclick: () => { if (confirm(`Run attempt ${out.planned + 1} in this output folder, with the same settings? It sits next to the earlier attempts, so their results compare directly.`)) go("same-folder", "Run again: checking readiness, then starting\u2026"); } },
          "Run again here"),
        h("button", { disabled: blocked || edits ? true : null, "data-clean": "", "data-base-off": blocked ? "1" : null, title: why,
                      onclick: () => { if (confirm("Run again in a new, empty output folder? Use this after changing the task or settings. The current folder and its attempts stay where they are.")) go("fresh-folder", "Run again in a fresh folder: checking readiness, then starting\u2026"); } },
          "Run again in a fresh folder")),
      h("p", { class: "tiny muted", style: "margin:8px 0 0" }, "To change settings before starting, ",
        h("button", { class: "link", disabled: s.locks.prompt === "Locked while a run or command is in progress." || edits ? true : null, "data-clean": "",
                      "data-base-off": s.locks.prompt === "Locked while a run or command is in progress." ? "1" : null, title: why,
                      onclick: () => { if (confirm("Point trial.output_dir at a new, empty folder without starting? The recorded attempts stay where they are; the task and settings unlock.")) act("/api/fresh-output", { base: t.sha256 }, (r) => `Fresh output folder: ${r.output_dir}. Settings are editable again.`); } },
          "switch to a fresh output folder"), " first."),
      h("p", { class: "reason block", "data-clean-note": "", hidden: !edits || null, text: "Save or discard your changes first." }),
      h("p", { class: "cmd", text: `agents: ajx console again ${where} --same-folder | --fresh-folder` }));
  }

  async function saveSettings(s) {
    const changes = settingsChanges(s);
    const res = await act("/api/settings", { changes, base: s.trial.sha256 }, "Saved. The plan changed, so it is checked again before it starts.");
    if (res) {
      ui.drafts.settings = {};
      if (ui.trialText && ui.trialText.draft === undefined) { ui.trialText = null; if (ui.advancedOpen) loadTrialText(ui.snap, true); }
    }
    renderTab("plan", true);
  }

  function readinessView(s) {
    const r = s.readiness, active = ACTIVE.includes(s.process.state);
    let head;
    if (r.status === "checking") head = h("div", { class: "okbox", text: "Checking: ajx validate, then ajx doctor\u2026" });
    else if (!r.current) head = h("p", { class: "small", text: r.status === "unknown" ? "Not checked for this plan yet." : "Last checked for an earlier version of the plan." });
    else if (r.status === "ready") head = h("div", { class: "okbox", text: `Ready: validate and doctor passed at ${clock(r.checked_at)}.` });
    else head = h("div", { class: "errbox" }, h("b", { text: "Not ready." }), " Fix these, then check again:",
      h("ul", { class: "list" }, (r.problems || []).map((p) => h("li", { class: "mono", text: p }))));
    const steps = (r.steps || []).map((st) => h("details", { "data-key": `ready-${st.name}` }, h("summary", { text: `${st.name}: ${st.exit_code === 0 ? "passed" : st.timed_out ? "timed out" : "exit " + st.exit_code}` }),
      h("pre", { class: "logtail", text: st.output })));
    const warns = r.current ? (r.warnings || []) : [];
    const warnBox = !warns.length ? null : r.needs_ack
      ? h("div", { class: "warnbox" }, h("b", { text: "Passed with warnings." }), " This task acts in an account, so read them before starting:",
          h("ul", { class: "list" }, warns.map((w) => h("li", { text: w }))),
          h("label", { class: "row", style: "margin-top:6px" },
            h("input", { type: "checkbox", "data-key": "accept-warnings", checked: ui.acceptWarnings ? true : null,
                         onchange: (e) => { ui.acceptWarnings = e.target.checked; updateDirty(); renderTab("plan", true); } }),
            h("span", { text: "I accept these warnings for this run" })))
      : h("div", { class: "notebox" }, h("b", { text: "Notes from doctor:" }), h("ul", { class: "list" }, warns.map((w) => h("li", { text: w }))));
    return h("div", {}, head, warnBox, steps,
      h("div", { class: "row", style: "margin-top:8px" },
        h("button", { onclick: () => act("/api/check", {}, "Checking readiness\u2026"), disabled: active || s.trial.error ? true : null }, "Check now")));
  }

  // ------------------------------------------------------------------ plan tab: the coach
  // A map of the trial (your side, the execution environments, the workers inside them), popover editors
  // anchored to its boxes, a "Best result" checklist with recipes, and one save path through
  // /api/plan/save. Shown only when the server sends plan_model and plan_options; other servers keep
  // the classic cards above.
  const Coach = (() => {
    const FOLDER = "@local";
    const SLUG = /^[a-z0-9][a-z0-9._-]{0,47}$/;
    const TOOL = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/;
    const IMAGE = /^(?:sha256:[0-9a-f]{64}|[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64})$/;
    const LIMITS = { cpus: 1, memory_mb: 1024, pids: 256, tmpfs_mb: 64, lifetime_seconds: 3600 };
    const REQUIRED = ["filesystem_isolation", "non_root", "resource_limits", "lifetime"];
    const PROTECT = [["filesystem_isolation", "Sees only its own folders"], ["non_root", "Runs as a non-root user"],
                     ["resource_limits", "Resource limits"], ["lifetime", "Lifetime limit"],
                     ["network_isolation", "No network (only with network none)"]];
    const KIND = {
      folder: ["On your machine: a fresh folder", "Runs as you in a new folder. Not a sandbox: it can read and change your files and use your network and the accounts you are signed in to."],
      profile: ["On your machine: a fresh profile", "Runs as you with a fresh home and config folder, so your skills and settings are not loaded. Still not a sandbox: it can read your files and use your network."],
      container: ["In a container on your machine", ""],  // described from its protections: containerSays()
    };
    const FIELD = {
      minutes: "time limit (min)", attempts: "attempts", budget: "budget (USD)", parallel: "attempts at the same time",
      report_minutes: "reporting time limit (min)", workspace: "workspace folder", shell: "shell startup files",
      output_dir: "output folder", order_seed: "order seed", prompt: "prompt", materials: "materials", fixture: "starting files",
      human: "human help", setup: "setup commands", cli: "agent", model: "model", models: "models compared",
      effort: "effort", config: "starting setup", auth: "sign-in", env: "runs in", skills: "skills", skillPaths: "skill folders",
      on: "acts in an account", identity: "account check", preflight: "preflight", teardown: "cleanup", kind: "kind",
      image: "image", network: "network", required: "protections", limits: "limits", tools: "tools", docker_host: "Docker socket",
      extra: "other settings", name: "name", type: "type", path: "path", contains: "contains", run: "command",
      expect_exit: "expected exit code", expect_stdout: "output pattern", url: "URL", expect_status: "expected status",
      expect_body: "body pattern", where: "runs on",
    };
    const SKIP = new Set(["id", "profile", "models_form", "label", "prompt_file"]);
    // Not "only what crosses it is shared": on your machine the worker can read more than AJX hands it.
    const BOUNDARY = "boundary: what AJX hands the worker and collects from it";
    const P = {
      snap: null, saved: null, cur: null, savedSig: "", base: { sha: "", prompt: "" }, serverSig: null, stale: false,
      adoptNext: false, open: null, tab: "check", recipe: null, flash: new Set(), changed: new Set(), pv: new Set(),
      prev: false, full: false, undo: [], typing: null, note: "", preview: null, saving: false, fileText: null,
      timer: null, flashTimer: null, drag: null, lastLock: undefined, opened: false, el: null, seq: 0,
    };

    // ---------------------------------------------------------------- small helpers
    function stable(v) {  // key-order-insensitive JSON: the Go server sorts keys, Python keeps file order
      if (Array.isArray(v)) return "[" + v.map(stable).join(",") + "]";
      if (v && typeof v === "object") return "{" + Object.keys(v).sort().map((k) => JSON.stringify(k) + ":" + stable(v[k])).join(",") + "}";
      return JSON.stringify(v === undefined ? null : v);
    }
    const clone = (v) => JSON.parse(JSON.stringify(v));
    const getP = (o, path) => path.split(".").reduce((a, k) => (a === null || a === undefined ? a : a[k]), o);
    function setP(o, path, v) {
      const keys = path.split("."), last = keys.pop();
      keys.reduce((a, k) => a[k], o)[last] = v;
    }
    const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;
    const O = () => (P.snap && P.snap.plan_options) || {};
    const harness = (name) => (O().harnesses || []).find((x) => x.name === name)
      || { name, label: name || "agent", installed: false, verified: false, reporter: false, effort: false, models: [] };
    const routes = (cli) => (O().auth || {})[cli] || [{ value: "", label: "Default", fresh_home: false, kind: "default", detail: "" }];
    const route = (cli, value) => routes(cli).find((r) => r.value === (value || "")) || null;
    const envOf = (m, id) => m.envs.find((e) => e.id === id);
    const kindOf = (m, w) => (envOf(m, w.env) || { kind: w.env === FOLDER ? "folder" : "container" }).kind;
    const envName = (e) => (!e ? "?" : e.kind === "folder" ? "your machine" : e.id);
    const repCli = (m) => m.reporter.cli || (m.workers.find((w) => harness(w.cli).reporter) || { cli: "claude-code" }).cli;
    const modelsOf = (w) => (w.models || []).map((x) => String(x).trim()).filter(Boolean);
    const effW = (m) => m.workers.reduce((n, w) => n + Math.max(1, modelsOf(w).length), 0);
    const cmd = (run) => ({ run, name: "", extra: {} });
    const acctName = (m) => (/--profile[ =](\S+)/.exec(m.account.identity || "") || [])[1] || "";
    const shortImg = (s) => { const x = /sha256:([0-9a-f]+)/.exec(s || ""); return x ? `${(s.split("@")[0] !== s ? s.split("@")[0] + " " : "")}sha256:${x[1].slice(0, 6)}…` : (s || "no image yet"); };
    const dirty = () => Boolean(P.cur) && stable(P.cur) !== P.savedSig;
    const anchor = (id) => (P.el && id ? P.el.querySelector(`[data-n="${CSS.escape(id)}"]`) : null);
    const minutes = (v) => (v === "" || v === null || v === undefined ? "?" : `${+v} min`);
    function containerSays(e) {
      const req = e.required || [], mounts = ((e.extra || {}).mounts || []).length;
      const fs = req.includes("filesystem_isolation")
        ? `It sees only its own folders${mounts ? `, plus ${plural(mounts, "folder")} mounted into it` : ""}.`
        : `AJX does not check that it sees only its own folders, because that protection is not required here${mounts ? `; ${plural(mounts, "folder")} are mounted into it` : ""}.`;
      const net = e.network === "none" ? "It has no network, but it has the credentials you give it." : "It still has the network and the credentials you give it.";
      return `${fs} ${net}`;
    }
    function newCheck(type) {
      return { name: "", type, path: "", contains: "", run: "", expect_exit: 0, expect_stdout: "", url: "", expect_status: 200,
               expect_body: "", where: "host", extra: {} };
    }
    function lintOf(text) {
      const out = [];
      for (const [pattern, why] of O().prompt_lint || []) {
        try {
          const m = new RegExp(pattern, "i").exec(text || "");
          if (m) out.push({ match: m[0], why });
        } catch (e) { /* a pattern JavaScript cannot read: skip it */ }
      }
      return out;
    }
    function bashOnly(text) {
      const out = [];
      for (const [pattern, label] of O().bash_only || []) {
        try { if (new RegExp(pattern).test(text || "")) out.push(label); } catch (e) { /* skip */ }
      }
      return out;
    }
    function allCommands(m) {
      return [...m.task.setup, ...(m.account.on ? m.account.preflight : []), ...m.account.teardown,
              ...m.checks.filter((c) => c.type === "cmd").map((c) => ({ run: c.run }))].map((c) => String(c.run || ""));
    }
    function swap(box, kids) {  // replace a part of the page, keeping keyboard focus and scroll positions
      const a = document.activeElement, inside = a && box.contains(a);
      const attr = !inside ? null : a.dataset.key ? "data-key" : a.dataset.n ? "data-n" : a.dataset.fk ? "data-fk" : null;
      const keep = attr ? a.getAttribute(attr) : null;
      const scrolls = [...box.querySelectorAll("[data-scroll]")].map((n) => [n.dataset.scroll, n.scrollTop]);
      box.replaceChildren(...kids.flat(Infinity).filter((k) => k !== null && k !== undefined && k !== false));
      for (const [key, top] of scrolls) { const n = box.querySelector(`[data-scroll="${CSS.escape(key)}"]`); if (n) n.scrollTop = top; }
      if (keep) { const n = box.querySelector(`[${attr}="${CSS.escape(keep)}"]`); if (n && !n.disabled) n.focus({ preventScroll: true }); }
    }

    // ---------------------------------------------------------------- locks
    function lockKind() {
      const l = (P.snap && P.snap.locks) || {};
      if ("kind" in l) return l.kind || null;
      return l.settings ? (/in progress/i.test(l.settings) ? "job" : "attempts") : null;
    }
    // An output folder with attempts takes only more attempts, another output folder, and new workers in
    // the environments it already has (ajx run refuses anything else there). A running job takes nothing.
    function can(path) {
      const k = lockKind();
      if (!k) return true;
      if (k === "job") return false;
      if (path === "run.attempts" || path === "run.output_dir" || path === "+worker") return true;
      const m = /^workers\.(\d+)\.(\w+)/.exec(path || "");
      if (!m) return false;
      const w = P.cur.workers[+m[1]];
      return Boolean(w) && !P.saved.workers.some((x) => x.id === w.id) && !["skills", "skillPaths"].includes(m[2]);
    }

    // ---------------------------------------------------------------- the model: normalize, problems, diff
    function norm(m, before, quiet) {
      const was = Object.fromEntries((before ? before.workers : []).map((w) => [w.id, w]));
      const wasKind = Object.fromEntries((before ? before.envs : []).map((e) => [e.id, e.kind]));
      const switched = [], unskilled = [];
      for (const c of m.checks) if (c.type !== "cmd") c.where = "host";
      for (const e of m.envs) {
        if (e.kind === "container" && e.network !== "none") e.required = (e.required || []).filter((r) => r !== "network_isolation");
      }
      for (const w of m.workers) {
        if (!envOf(m, w.env) && m.envs.length) w.env = m.envs[0].id;
        const env = envOf(m, w.env) || { kind: "folder" }, old = was[w.id];
        if ((!old || old.cli !== w.cli) && !harness(w.cli).effort) w.effort = "";  // only for a new agent: never rewrite an untouched value
        if (env.kind !== "folder") w.config = "clean";
        const r = route(w.cli, w.auth);
        const moved = !old || old.cli !== w.cli || old.env !== w.env || wasKind[w.env] !== env.kind;
        const arrived = !old || old.env !== w.env || wasKind[w.env] !== env.kind;
        if (env.kind === "folder" && w.skills === "folders" && arrived) { w.skills = "none"; unskilled.push(w.id); }
        if (env.kind !== "folder" && !(r && r.fresh_home)) {
          const best = bestRoute(w.cli);
          if (best) {
            switched.push(`“${w.id}” to “${routeLabel(w.cli, best)}”`);
            w.auth = best.value;
          }
        } else if (moved && !r) {
          w.auth = "";
        }
      }
      if (before && before.reporter.cli !== m.reporter.cli && !route(repCli(m), m.reporter.auth)) m.reporter.auth = "";
      if (!quiet) {
        const notes = [P.note];
        if (switched.length) notes.push(`Sign-in changed for ${switched.join(", ")}: the earlier sign-in needs your own login folder, which a fresh profile or a container does not have.`);
        if (unskilled.length) notes.push(`Skills removed from “${unskilled.join("”, “")}”: skills need a fresh-profile or container environment, not a fresh folder on your machine. Undo brings them back.`);
        P.note = notes.filter(Boolean).join(" ");
      }
      return m;
    }

    const AS_YOURS = "the sign-in this agent CLI already has on your machine";
    function routeLabel(cli, r) {
      if (!r) return "";
      const d = harness(cli).default_auth;
      if (r.kind === "default") return !d || d === "inherit" ? "Default: as on your machine" : `Default (${d})`;
      if (r.value === "inherit") return "As on your machine (inherit)";
      return r.label;
    }
    function routeDetail(cli, r) {
      if (!r) return null;
      const d = harness(cli).default_auth;
      if (r.value === "inherit" || (r.kind === "default" && (!d || d === "inherit"))) return `Uses ${AS_YOURS}.`;
      if (r.kind === "default") { const t = routes(cli).find((x) => x.value === d); return t && t.detail ? `Uses ${d}: ${t.detail}.` : "Uses this agent's default sign-in."; }
      return r.detail || null;
    }
    const authText = (cli, v) => routeLabel(cli, route(cli, v)) || v || "default";
    const bestRoute = (cli) => ["profile", "template", "type", "default"].map((k) => routes(cli).find((x) => x.kind === k && x.fresh_home)).find(Boolean);
    function clientProblems(m) {
      const out = [], add = (node, msg, more) => out.push({ node, msg, ...more });
      const ids = m.workers.map((w) => w.id);
      if (!m.workers.length) add("map", "Add at least one worker.");
      for (const w of m.workers) {
        const n = "worker-" + w.id, hh = harness(w.cli), k = kindOf(m, w);
        if (!w.id || ids.filter((x) => x === w.id).length > 1) add(n, "Worker names must be unique and not empty.");
        if (!hh.installed) add(n, `${hh.label} is not installed on this machine, so the readiness check would fail for worker “${w.id}”.`);
        for (const x of [w.model, ...(w.models || [])]) if (x && /\s/.test(x)) add(n, `Model “${x}” has a space; model names cannot.`);
        if (w.skills === "folders" && k === "folder") add(n, `Worker “${w.id}”: giving skills needs a fresh-profile or container environment.`);
        if (w.skills === "folders" && !(w.skillPaths || []).some((x) => String(x).trim())) add(n, `Worker “${w.id}”: choose at least one skill folder, or select None.`);
        const r = route(w.cli, w.auth), best = bestRoute(w.cli), wi = m.workers.indexOf(w);
        if (k !== "folder" && !(r && r.fresh_home)) {
          if (best) add(n, `Worker “${w.id}”: the sign-in “${authText(w.cli, w.auth)}” needs your own login folder, which a fresh profile or a container does not have.`,
                        { fix: `Sign in with “${routeLabel(w.cli, best)}”`, fn: (x) => { x.workers[wi].auth = best.value; } });
          else add(n, `Worker “${w.id}”: no sign-in for ${hh.label} works with a fresh home folder, so it can run only in a fresh folder on your machine.`);
        }
      }
      for (const e of m.envs) {
        if (e.kind === "container" && !IMAGE.test(String(e.image || "").trim())) add("env-" + e.id, `Environment “${e.id}”: choose an image pinned by digest (sha256:… or repo@sha256:…).`);
      }
      m.checks.forEach((c, i) => {
        const need = { file: ["path", "a path"], cmd: ["run", "a command"], http: ["url", "a URL"] }[c.type];
        if (need && !String(c[need[0]] || "").trim()) add("checks", `Check ${i + 1} needs ${need[1]}.`);
      });
      const r = m.run, whole = (v, low) => Number.isInteger(Number(v)) && String(v).trim() !== "" && Number(v) >= low;
      if (!(Number(r.minutes) > 0) || String(r.minutes).trim() === "") add("run", "The time limit must be more than 0 minutes.");
      if (!whole(r.attempts, 1)) add("run", "Attempts must be a whole number, at least 1.");
      if (!whole(r.parallel, 1)) add("run", "Attempts at the same time must be a whole number, at least 1.");
      if (String(r.budget ?? "").trim() !== "" && !(Number(r.budget) > 0)) add("run", "The budget must be more than 0, or empty for none.");
      if (!String(m.task.prompt || "").trim()) add("task", "The task prompt cannot be empty.");
      const ac = m.account;
      if (ac.on && !String(ac.identity || "").trim() && !(ac.preflight || []).some((c) => String(c.run || "").trim())) {
        add("account", "The task account needs an account check command or a preflight command. Without one, saving turns the account off again.", { block: true });
      }
      if (m.reporter.model && /\s/.test(m.reporter.model)) add("reporter", `Reporter model “${m.reporter.model}” has a space; model names cannot.`);
      return out;
    }

    function nodeFor(msg) {  // where a server problem points on the map, best effort
      const t = String(msg || ""), m = P.cur;
      const w = m.workers.find((x) => x.id && (t.includes(`worker ${x.id}`) || t.includes(`cell ${x.id}`) || t.includes(`“${x.id}”`)));
      if (w) return "worker-" + w.id;
      const cell = /cells\[(\d+)\]/.exec(t);
      if (cell && m.workers[+cell[1]]) return "worker-" + m.workers[+cell[1]].id;
      const env = m.envs.find((e) => e.kind !== "folder" && (t.includes(`environments.${e.id}`) || t.includes(`environment ${e.id}`) || t.includes(`${e.id} `)));
      if (env) return "env-" + env.id;
      if (/output folder/i.test(t)) return "advanced";
      if (/prompt/i.test(t)) return "task";
      if (/verify|check/i.test(t)) return "checks";
      if (/reporter/i.test(t)) return "reporter";
      if (/identity|preflight|teardown|cleanup/i.test(t)) return "account";
      if (/timeout|time limit|repetitions|attempts|budget|parallel/i.test(t)) return "run";
      return null;
    }

    function allProblems(m) {
      const out = clientProblems(m), seen = new Set(out.map((p) => p.msg));
      const push = (msg) => { if (msg && !seen.has(msg)) { seen.add(msg); out.push({ node: nodeFor(msg), msg }); } };
      const s = P.snap;
      if (dirty()) {
        if (P.preview) for (const msg of P.preview.problems || []) push(msg);
      } else {
        if (s.trial.error) push(`AJX cannot load trial.toml: ${s.trial.error}`);
        if (s.readiness.current && s.readiness.status === "not_ready") {
          const first = (s.readiness.problems || [])[0];
          push(`The last readiness check failed${first ? ": " + first : "."}`);
        }
      }
      return out;
    }

    function flat(m) {
      const out = new Map(), put = (node, label, field, v, cli) => out.set(`${node}\u0000${label}\u0000${field}`, { node, label, field, v, cli });
      const part = { task: ["task", "Task"], run: ["run", "Run settings"], reporter: ["reporter", "Reporter"], account: ["account", "Task account"] };
      for (const [key, [node, label]] of Object.entries(part)) {
        for (const [k, v] of Object.entries(m[key] || {})) {
          if (!SKIP.has(k)) put(key === "run" && k === "output_dir" ? "advanced" : node, key === "run" && k === "output_dir" ? "Advanced" : label, k, v, key === "reporter" ? repCli(m) : null);
        }
      }
      m.checks.forEach((c, i) => { for (const [k, v] of Object.entries(c)) put("checks", `Check ${i + 1}`, k, v); });
      for (const e of m.envs) for (const [k, v] of Object.entries(e)) if (!SKIP.has(k)) put("env-" + e.id, `Environment “${envName(e)}”`, k, v);
      for (const w of m.workers) for (const [k, v] of Object.entries(w)) if (!SKIP.has(k)) put("worker-" + w.id, `Worker “${w.id}”`, k, v, w.cli);
      return out;
    }

    function changeList(a, b) {
      const out = [], ids = (m, k) => new Set(m[k].map((x) => x.id));
      const aw = ids(a, "workers"), bw = ids(b, "workers"), ae = ids(a, "envs"), be = ids(b, "envs");
      for (const w of b.workers) {
        if (aw.has(w.id)) continue;
        const mods = modelsOf(w);
        out.push({ node: "worker-" + w.id, text: `Add worker “${w.id}”: ${harness(w.cli).label} · ${mods.length ? mods.join(" + ") : w.model || "agent default"} · sign-in “${authText(w.cli, w.auth)}” · in ${envName(envOf(b, w.env))}${w.skills === "folders" ? " · with skills" : ""}` });
      }
      for (const id of aw) if (!bw.has(id)) out.push({ node: "map", text: `Remove worker “${id}”` });
      for (const e of b.envs) if (!ae.has(e.id)) out.push({ node: "env-" + e.id, text: `Add environment “${envName(e)}” (${KIND[e.kind] ? KIND[e.kind][0].toLowerCase() : e.kind})` });
      for (const e of a.envs) if (!be.has(e.id)) out.push({ node: "map", text: `Remove environment “${envName(e)}”` });
      const counts = a.checks.length !== b.checks.length;
      if (counts) out.push({ node: "checks", text: `Success checks: ${a.checks.length} → ${b.checks.length}` });
      const fa = flat(a), fb = flat(b);
      for (const key of new Set([...fa.keys(), ...fb.keys()])) {
        const x = fa.get(key), y = fb.get(key), it = y || x;
        if (it.node.startsWith("worker-") && !(aw.has(it.node.slice(7)) && bw.has(it.node.slice(7)))) continue;
        if (it.node.startsWith("env-") && !(ae.has(it.node.slice(4)) && be.has(it.node.slice(4)))) continue;
        if (it.node === "checks" && counts) continue;
        if (stable(x ? x.v : null) === stable(y ? y.v : null)) continue;
        const val = (e) => (!e ? undefined : e.field === "auth" && e.cli ? authText(e.cli, e.v) : e.v);
        if (it.field === "prompt") out.push({ node: it.node, text: "Task · prompt edited" });
        else out.push({ node: it.node, text: `${it.label} · ${FIELD[it.field] || it.field}`, from: val(x), to: val(y), kv: true });
      }
      const tables = (m) => {
        const t = new Map(), use = (cli, v, node) => { const r = route(cli, v); if (r && r.kind === "template" && !t.has(r.value)) t.set(r.value, [routeLabel(cli, r), node]); };
        for (const w of m.workers) use(w.cli, w.auth, "worker-" + w.id);
        use(repCli(m), m.reporter.auth, "reporter");
        return t;
      };
      const ta = tables(a);
      for (const [v, [label, node]] of tables(b)) {
        if (!ta.has(v)) out.push({ node, text: `Adds sign-in [auth.${v}] to trial.toml (${label}): it reads the keys from the terminal that started the console; no secret is written` });
      }
      return out;
    }

    function shown(v) {
      if (v === "" || v === null || v === undefined) return "(empty)";
      if (v === true) return "on";
      if (v === false) return "off";
      if (Array.isArray(v)) return v.map((x) => (x && typeof x === "object" ? x.run || x.name || "…" : String(x))).join(", ") || "none";
      if (typeof v === "object") return Object.keys(v).length ? Object.entries(v).map(([k, x]) => `${k} ${x && typeof x === "object" ? "…" : x}`).join(", ") : "none";
      const t = String(v);
      return t.length > 60 ? t.slice(0, 57) + "…" : t;
    }

    function nodeWord(n) {
      if (n.startsWith("worker-")) return `worker ${n.slice(7)}`;
      if (n.startsWith("env-")) return n === "env-" + FOLDER ? "your machine" : `environment ${n.slice(4)}`;
      return { task: "the task", checks: "success checks", run: "run settings", reporter: "reporter", account: "task account",
               advanced: "output folder", map: "workers or environments" }[n] || n;
    }

    // ---------------------------------------------------------------- the "Best result" checklist
    const ASPECTS = [
      ["cli", "agent", (w) => w.cli], ["model", "model", (w) => modelsOf(w).join("+") || w.model || ""],
      ["effort", "effort", (w) => w.effort || ""], ["config", "starting setup", (w) => w.config],
      ["skills", "skills", (w) => (w.skills === "folders" ? (w.skillPaths || []).join("|") || "folders" : "none")],
      ["env", "environment", (w) => w.env], ["auth", "sign-in", (w) => w.auth || ""], ["extra", "other settings", (w) => stable(w.extra || {})],
    ];
    const ASPECT = Object.fromEntries(ASPECTS.map((a) => [a[0], a[1]]));
    // An environment difference brings its own sign-in and starting setup, and another agent its own
    // models, sign-in and effort: those follow from the one thing compared.
    const ABSORBS = { env: ["auth", "config"], cli: ["model", "auth", "effort"] };
    function wdiffs(m) {
      const f = m.workers[0], d = new Set();
      for (const w of m.workers.slice(1)) for (const [k, , get] of ASPECTS) if (get(f) !== get(w)) d.add(k);
      for (const [k, gone] of Object.entries(ABSORBS)) if (d.has(k)) for (const g of gone) d.delete(g);
      return [...d];
    }
    function copyAspect(f, w, k) {
      if (k === "model") { w.model = f.model; w.models = [...(f.models || [])]; w.models_form = f.models_form; }
      else if (k === "skills") { w.skills = f.skills; w.skillPaths = [...(f.skillPaths || [])]; }
      else if (k === "extra") w.extra = clone(f.extra || {});
      else w[k] = f[k];
    }
    function ensureEnv(m, kind) {
      const have = m.envs.find((e) => e.kind === kind);
      if (have) return have.id;
      const id = uniqueEnv(m, kind === "container" ? "container" : kind === "profile" ? "fresh-profile" : FOLDER);
      m.envs.push(kind === "folder" ? { id: FOLDER, kind, label: "your machine" } : freshEnv(id, kind));
      return kind === "folder" ? FOLDER : id;
    }
    function uniqueEnv(m, stem) {
      if (stem === FOLDER) return FOLDER;
      let id = stem, n = 2;
      while (m.envs.some((e) => e.id === id)) id = `${stem}-${n++}`;
      return id;
    }
    function freshEnv(id, kind, from) {
      const c = kind === "container";
      return { id, kind, image: c && from && from.kind === "container" ? from.image || "" : "", network: c ? "bridge" : "inherit",
               required: c ? [...REQUIRED] : [], limits: c ? { ...LIMITS } : {}, tools: from && from.tools ? clone(from.tools) : [],
               docker_host: c ? O().docker_host || "" : "", extra: from && from.extra ? clone(from.extra) : {} };
    }
    function strongRep(m) {
      const rm = m.reporter.model;
      if ((!rm || /haiku/i.test(rm)) && harness(repCli(m)).models.includes("sonnet")) m.reporter.model = "sonnet";
    }

    function items(m) {
      const I = [], E = effW(m), cmp = E > 1;
      const probs = allProblems(m);
      I.push(probs.length
        ? { id: "valid", st: "bad", t: "The setup can run", w: `${plural(probs.length, "problem")}: ${probs[0].msg}`, show: probs[0].node, fix: probs[0].fix, fn: probs[0].fn }
        : { id: "valid", st: "ok", t: "The setup can run", w: dirty() && !P.preview ? "No errors found so far; AJX checks the edit as you go." : "No errors found in any box." });
      const nc = m.checks.length, inside = nc && m.checks.every((c) => c.type === "cmd" && c.where === "worker");
      I.push(!nc
        ? { id: "checks", st: "bad", t: "Check the actual outcome", w: "No success check: AJX can only report what the worker said it did.", fix: "Add a file check", fn: (x) => { x.checks.push(newCheck("file")); }, open: "checks", focus: `cp:checks.${nc}.path`, show: "checks" }
        : inside
          ? { id: "checks", st: "warn", t: "Check the actual outcome", w: "Every check runs inside the worker's environment, where the worker could change what the check sees.", fix: "Run the checks on your machine", fn: (x) => { for (const c of x.checks) c.where = "host"; }, show: "checks" }
          : { id: "checks", st: "ok", t: "Check the actual outcome", w: `${plural(nc, "check")}, run after the worker stops. The worker never sees them.` });
      const lint = lintOf(m.task.prompt);
      I.push(lint.length
        ? { id: "neutral", st: "warn", t: "The task prompt is neutral", w: `${lint.map((l) => `“${l.match}” ${l.why}`).join("; ")}. A worker that knows it is observed may act differently.`, show: "task" }
        : { id: "neutral", st: "ok", t: "The task prompt is neutral", w: "No AJX vocabulary or hints found." });
      const own = m.workers.filter((w) => w.config === "user" && kindOf(m, w) === "folder");
      I.push(own.length
        ? { id: "clean", st: "warn", t: "A baseline worker starts clean", w: `“${own.map((w) => w.id).join("”, “")}” uses your normal setup: it tests your own skills and settings, not a first-time user's.`, fix: "Start clean", fn: (x) => { for (const w of x.workers) if (w.config === "user") w.config = "clean"; }, show: "worker-" + own[0].id }
        : { id: "clean", st: "ok", t: "A baseline worker starts clean", w: "No worker loads your personal skills, settings or MCP servers." });
      const att = Number(m.run.attempts) || 1;
      I.push(cmp && att < 3
        ? { id: "att", st: "warn", t: "Enough attempts to see variance", w: `Comparing ${E} workers with ${plural(att, "attempt")} each: one run can be luck; 3 shows variance.`, fix: "Set attempts to 3", fn: (x) => { x.run.attempts = 3; }, free: true, show: "run" }
        : { id: "att", st: "ok", t: "Enough attempts to see variance", w: `${plural(att, "attempt")} per worker.` + (cmp ? "" : " One is fine for a single walkthrough; 3 shows variance.") });
      const rc = repCli(m), rh = harness(rc), rm = m.reporter.model;
      I.push(/haiku/i.test(rm || "") || (!rm && rh.models.length)
        ? { id: "rep", st: "warn", t: "The reporter is strong and fixed", w: `The reporter model is ${rm || "the agent's default"}: a stronger model writes a better analysis.`, ...(rh.models.includes("sonnet") ? { fix: "Use sonnet", fn: (x) => { x.reporter.model = "sonnet"; } } : {}), show: "reporter" }
        : { id: "rep", st: "ok", t: "The reporter is strong and fixed", w: `One reporter (${rh.label} · ${rm || "agent default"}) for every worker. Keep it the same between runs you compare.` });
      if (m.workers.length < 2) {
        I.push({ id: "one", st: "ok", t: "Comparisons change one thing at a time", w: E > 1 ? "One worker compared across models: only the model differs." : "One worker: nothing to compare. Add a worker or use a recipe to compare." });
      } else {
        const D = wdiffs(m), second = "worker-" + m.workers[1].id;
        if (D.length === 1) I.push({ id: "one", st: "ok", t: "Comparisons change one thing at a time", w: `The workers differ only in ${ASPECT[D[0]]}.` });
        else if (!D.length) I.push({ id: "one", st: "warn", t: "Comparisons change one thing at a time", w: "The workers are identical, so there is nothing to compare. Change one thing.", show: second });
        else {
          const keep = D.includes("skills") ? "skills" : D.includes("env") ? "env" : D.includes("cli") ? "cli" : D[0];
          I.push({ id: "one", st: "warn", t: "Comparisons change one thing at a time", w: `“${m.workers[0].id}” and the others differ in ${D.map((d) => ASPECT[d]).join(", ")}: you could not tell which one made the difference.`,
                   fix: `Keep only the ${ASPECT[keep]} different`, show: second,
                   fn: (x) => {
                     const f = x.workers[0];
                     if (keep === "skills") {  // skills need a fresh profile or a container: everyone shares the skilled worker's
                       const at = x.workers.find((w) => w.skills === "folders" && kindOf(x, w) !== "folder") || x.workers.find((w) => kindOf(x, w) !== "folder");
                       if (at) f.env = at.env;
                     }
                     for (const w of x.workers.slice(1)) for (const [k] of ASPECTS) if (k !== keep && !(ABSORBS[keep] || []).includes(k)) copyAspect(f, w, k);
                   } });
        }
      }
      const a = m.account, bad = allCommands(m).map(bashOnly).filter((x) => x.length);
      const acct = { id: "acct", t: "Account, preflight and cleanup", show: "account" };
      if (bad.length) I.push({ ...acct, st: "warn", w: `A setup, preflight, cleanup or check command uses ${[...new Set(bad.flat())].join(", ")}, which /bin/sh may not support. AJX runs these with /bin/sh -c.` });
      else if (!a.on) I.push({ ...acct, st: "ok", w: "No task account. Turn it on if the task acts in a cloud or SaaS account." });
      else if (!a.identity && !a.preflight.length) I.push({ ...acct, st: "warn", w: "No account check: nothing shows which account the task acts in." });
      else if (!a.preflight.length) I.push({ ...acct, st: "warn", w: "No preflight check: the worker could start in the wrong account.", fix: "Use the account check as preflight", fn: (x) => { x.account.preflight = [cmd(x.account.identity)]; } });
      else if (!a.teardown.length) I.push({ ...acct, st: "warn", w: "No cleanup: what the task creates stays in the account. Tag it with $AJX_RUN_TOKEN and delete by that tag." });
      else I.push({ ...acct, st: "ok", w: `Preflight ×${a.preflight.length}, cleanup ×${a.teardown.length}, POSIX shell syntax.` });
      const mine = m.workers.filter((w) => kindOf(m, w) !== "container");
      I.push(mine.length && a.on
        ? { id: "box", st: "warn", t: "“On your machine” is not a sandbox", w: `“${mine.map((w) => w.id).join("”, “")}” runs as you while the task acts in an account: it can read your files and use the accounts you are signed in to. Run it in a container instead.`,
            fix: "Run in a container", show: "worker-" + mine[0].id,
            fn: (x) => { const c = ensureEnv(x, "container"); for (const w of x.workers) if (kindOf(x, w) !== "container") w.env = c; return "env-" + c; } }
        : { id: "box", st: "ok", t: "“On your machine” is not a sandbox", w: mine.length ? "Workers on your machine run as you. Fine for a task that works only in its folder; use a container if it could touch your files or accounts." : "Every worker runs in a container. It still has the credentials you give it, and the network unless that is set to none." });
      const par = Number(m.run.parallel) || 1;
      I.push(par > 1
        ? { id: "par", st: "warn", t: "Attempts run one at a time", w: `${par} attempts run at once: their timings are not comparable, and Stop waits for every running attempt.`, fix: "Run one at a time", fn: (x) => { x.run.parallel = 1; }, show: "run" }
        : { id: "par", st: "ok", t: "Attempts run one at a time", w: "One at a time, so timings are comparable." });
      return I;
    }

    const RECIPES = [
      { id: "base", n: "First-time user baseline", d: "Clean workers without skills, 3 attempts one at a time, a strong reporter, no human help.",
        f: (m) => { for (const w of m.workers) { w.config = "clean"; w.skills = "none"; } m.run.attempts = Math.max(3, Number(m.run.attempts) || 1); m.run.parallel = 1; strongRep(m); m.task.human = "unavailable"; } },
      { id: "models", n: "Compare two models", d: "Keeps only the first worker and runs it with two models; everything else about it stays the same. Other workers are removed.",
        ok: (m) => m.workers.length > 0 && harness(m.workers[0].cli).models.length >= 2,
        why: (m) => `Needs an agent with two suggested models; ${m.workers.length ? harness(m.workers[0].cli).label : "this trial"} has ${m.workers.length ? harness(m.workers[0].cli).models.length : 0}.`,
        f: (m) => { const w = m.workers[0]; w.models = harness(w.cli).models.slice(0, 2); w.model = ""; m.workers = [w]; m.run.attempts = Math.max(3, Number(m.run.attempts) || 1); m.run.parallel = 1; strongRep(m); },
        open: (x) => "worker-" + x.workers[0].id },
      { id: "skill", n: "With vs without a skill", d: "Two identical workers in a fresh profile; only the skill differs. You choose the skill folder.",
        ok: (m) => m.workers.length > 0,
        why: () => "Needs a worker to start from.",
        f: (m) => {
          const e = ensureEnv(m, "profile"), a = clone(m.workers[0]);
          Object.assign(a, { id: "without-skill", env: e, config: "clean", skills: "none" });
          const b = Object.assign(clone(a), { id: "with-skill", skills: "folders", skillPaths: [], profile: "" });
          m.workers = [a, b]; m.run.attempts = Math.max(3, Number(m.run.attempts) || 1); m.run.parallel = 1; strongRep(m);
        },
        open: () => "worker-with-skill", focus: "cp-add:workers.1.skillPaths" },
      { id: "cloud", n: "Cloud task, safely", d: "Task account on with a preflight check; workers in a container; your shell startup files skipped; one at a time; a $2 budget.",
        f: (m) => {
          const a = m.account; a.on = true;
          if (!a.preflight.length && a.identity) a.preflight = [cmd(a.identity)];
          const c = ensureEnv(m, "container"); for (const w of m.workers) if (kindOf(m, w) !== "container") w.env = c;
          m.run.shell = "skip"; m.run.parallel = 1;
          if (String(m.run.budget ?? "").trim() === "") m.run.budget = 2;
        },
        open: (x) => { const c = x.envs.find((e) => e.kind === "container"); return !x.account.identity ? "account" : c && !IMAGE.test(c.image || "") ? "env-" + c.id : null; } },
    ];
    function recipeResult(r) {
      const x = clone(P.cur);
      if (r.ok && !r.ok(x)) return { x, list: [], blocked: r.why(x) };
      r.f(x);
      norm(x, P.cur, true);
      return { x, list: changeList(P.cur, x) };
    }

    // ---------------------------------------------------------------- editing
    function pushUndo(m) {
      P.undo.push({ m: m || clone(P.cur), id: ++P.seq });
      if (P.undo.length > 60) P.undo.shift();
      return P.seq;
    }
    // Edits that leave nothing unsaved while trial.toml changed on disk: show the file as it is now.
    function settle() {
      if (!P.stale || dirty() || !P.snap || !P.snap.plan_model) return false;
      adopt(P.snap);
      return true;
    }
    function flashNodes(nodes) {
      P.flash = new Set(nodes);
      clearTimeout(P.flashTimer);
      P.flashTimer = setTimeout(() => { P.flash = new Set(); if (P.el && P.el.isConnected) paintMap(); }, 2300);
    }
    function commit(label, fn, o = {}) {
      if (P.saving) { toast("Saving… make that change again in a moment.", true); return; }
      if (!can(o.path || "*")) { toast(lockKind() === "job" ? "Locked while a run or command is in progress." : "Locked for this output folder: its attempts used these settings.", true); return; }
      const before = clone(P.cur), x = clone(P.cur);
      P.note = "";
      const ret = fn(x);
      norm(x, before);
      const target = typeof o.open === "function" ? o.open(x) : o.open || (typeof ret === "string" ? ret : null);
      if (stable(x) === stable(P.cur)) {
        if (target) openNode(target, { force: true, focus: o.focus });
        else if (P.note) paintPop();
        return;
      }
      const id = pushUndo(before);
      P.typing = null;
      P.cur = x;
      flashNodes(changeList(before, x).map((c) => c.node));
      if (target) P.open = target;
      settle();
      if (P.open && !popDef(P.open)) P.open = null;
      const note = P.note;
      paintAll(o.focus);
      if (target && !o.focus && P.popEl) P.popEl.focus({ preventScroll: true });
      if (label) toast(label + (note && !P.open ? ` ${note}` : ""), false, { label: "Undo", fn: () => undo(id) });
      schedulePreview();
    }
    function undo(id) {
      if (lockKind() === "job" || P.saving) return;
      const top = P.undo[P.undo.length - 1];
      if (!top) return;
      if (id && top.id !== id) { toast("That change is no longer the latest one. Use Undo in the bar to step back one change at a time.", true); return; }
      P.undo.pop();
      const before = P.cur;
      P.cur = top.m;
      P.typing = null;
      flashNodes(changeList(before, P.cur).map((c) => c.node));
      settle();
      if (P.open && !popDef(P.open)) P.open = null;
      paintAll();
      schedulePreview();
    }
    function discard() {
      if (!dirty() || P.saving) return;
      const id = pushUndo();
      const before = P.cur;
      P.cur = clone(P.saved);
      P.typing = null;
      P.preview = null;
      flashNodes(changeList(before, P.cur).map((c) => c.node));
      settle();
      if (P.open && !popDef(P.open)) P.open = null;
      paintAll();
      toast("Discarded the unsaved changes.", false, { label: "Undo", fn: () => undo(id) });
    }
    function onText(el, path, o) {
      if (!can(path) || P.saving) return;
      if (P.typing !== path) { pushUndo(); P.typing = path; }
      let v = el.value;
      if (o.num) v = v.trim() === "" ? "" : Number.isFinite(Number(v)) ? Number(v) : v;
      setP(P.cur, path, v);
      if (o.ns) el.classList.toggle("cp-bad", /\s/.test(el.value));
      if (o.live) o.live(el.value);
      if (settle()) paintAll(); else paintLive();
      schedulePreview();
    }

    function schedulePreview() {
      clearTimeout(P.timer);
      if (!dirty()) { P.preview = null; paintBar(); paintPrev(); return; }
      P.timer = setTimeout(runPreview, 300);
    }
    async function runPreview() {
      const sig = stable(P.cur), model = clone(P.cur);
      let res;
      try {
        res = await api("/api/plan/preview", { model });
      } catch (e) {
        res = { problems: [e.message], diff: [], changes: [], added_auth: [], text: "", failed: true };
      }
      if (!P.cur || stable(P.cur) !== sig) return;  // a newer edit has its own preview on the way
      P.preview = { sig, ...res };
      paintCoach();
      paintBar();
      paintPrev();
    }

    async function save() {
      if (P.saving || !dirty() || P.stale || ui.inflight) return;
      const a = document.activeElement;
      const keep = a && P.popRoot.contains(a) && a.dataset.key ? { key: a.dataset.key, s: a.selectionStart, e: a.selectionEnd } : null;
      P.saving = true;
      ui.inflight = true;
      document.body.classList.add("inflight");
      for (const el of P.el.querySelectorAll("input.cp-in, textarea.cp-in")) el.readOnly = true;  // keeps focus and caret
      paintBar();
      try {
        const res = await api("/api/plan/save", { model: P.cur, base: P.base.sha, prompt_base: P.base.prompt });
        P.adoptNext = true;
        const n = (res.changes || []).length;
        toast(`Saved${n ? " " + plural(n, "change") : ""}. The plan changed, so readiness is checked again before it starts.`);
        await refresh(true);
        if (P.adoptNext && P.snap && P.snap.plan_model) adopt(P.snap);
      } catch (e) {
        if (e.status === 409) {
          await refresh(true);  // the edits stay; they are stale unless the change on disk does not touch them
          toast(P.stale ? e.message : "trial.toml changed on disk, but not in anything you edited. Press Save again.", P.stale);
        } else toast(e.message, true);
      } finally {
        P.saving = false;
        ui.inflight = false;
        document.body.classList.remove("inflight");
        paintAll(keep && keep.key);
        const el = keep && P.popEl && P.popEl.querySelector(`[data-key="${CSS.escape(keep.key)}"]`);
        if (el) { try { el.setSelectionRange(keep.s, keep.e); } catch (err) { /* not a text field */ } }
      }
    }

    function freshOutput() {
      if (dirty() || ui.inflight) return;
      if (!confirm("Point trial.output_dir at a new, empty folder? The recorded attempts stay where they are, and the task and settings unlock.")) return;
      act("/api/fresh-output", { base: P.snap.trial.sha256 }, (r) => `Fresh output folder: ${r.output_dir}. Settings are editable again.`);
    }

    function addWorker(envId) {
      commit("Added a worker to compare", (x) => {
        const base = x.workers.find((w) => w.env === envId) || x.workers[0];
        const w = base ? clone(base) : { id: "worker", cli: defaultCli(), model: "", models: [], models_form: false, effort: "", config: "clean",
                                         auth: "", env: envId, skills: "none", skillPaths: [], profile: "", extra: {} };
        const stem = (base ? base.id : "worker").replace(/-\d+$/, "").slice(0, 44);
        let id = base ? `${stem}-2` : "worker", n = 2;
        while (x.workers.some((y) => y.id === id)) id = `${stem}-${++n}`;
        Object.assign(w, { id, env: envId });
        x.workers.push(w);
        return "worker-" + id;
      }, { path: "+worker" });
    }
    function defaultCli() {
      const hs = O().harnesses || [];
      return (hs.find((x) => x.name === "claude-code" && x.installed) || hs.find((x) => x.installed && x.verified) || hs.find((x) => x.installed) || { name: "claude-code" }).name;
    }
    function addEnv(kind) {
      commit(`Added ${KIND[kind][0].toLowerCase()}`, (x) => "env-" + ensureEnvNew(x, kind));
    }
    function ensureEnvNew(m, kind) {
      if (kind === "folder") return ensureEnv(m, "folder");
      const id = uniqueEnv(m, kind === "container" ? "container" : "fresh-profile");
      m.envs.push(freshEnv(id, kind));
      return id;
    }
    function changeKind(m, i, kind) {
      const e = m.envs[i];
      if (!e || e.kind === kind) return null;
      if (kind === "folder" && m.envs.some((o, j) => j !== i && o.kind === "folder")) {
        P.note = "There is already a fresh folder on your machine. Move the workers there instead.";
        return null;
      }
      const old = e.id;
      const id = kind === "folder" ? FOLDER : old === FOLDER ? uniqueEnv(m, kind === "container" ? "container" : "fresh-profile") : old;
      m.envs[i] = kind === "folder" ? { id, kind, label: "your machine" } : freshEnv(id, kind, e);
      for (const w of m.workers) if (w.env === old) w.env = id;
      return "env-" + id;
    }

    // ---------------------------------------------------------------- form controls
    function F(label, control, hint, o = {}) {
      return h(o.div ? "div" : "label", { class: "cp-f" }, label ? h("span", { class: "cp-lb", text: label }) : null, control,
        hint ? h("div", { class: "cp-h" }, hint) : null);
    }
    function callout(kind, ...kids) { return h("div", { class: "cp-callout cp-" + kind }, ...kids); }
    function inp(path, o = {}) {
      const v = o.value !== undefined ? o.value : getP(P.cur, path);
      const el = h(o.area ? "textarea" : "input", {
        type: o.area ? null : o.num ? "number" : "text", class: "cp-in" + (o.mono ? " cp-mono" : ""), "data-key": "cp:" + path,
        placeholder: o.ph || null, list: o.list || null, rows: o.area ? o.rows || 3 : null, step: o.num ? o.step || "any" : null,
        min: o.min ?? null, spellcheck: o.mono || o.ns ? "false" : null, autocomplete: "off", disabled: !can(o.lock || path) || null, readonly: P.saving || null,
        "aria-label": o.aria || null, oninput: (e) => onText(e.target, path, o),
      });
      el.value = v === null || v === undefined ? "" : String(v);
      if (o.ns) el.classList.toggle("cp-bad", /\s/.test(el.value));
      return el;
    }
    function sel(path, options, o = {}) {
      const v = o.value !== undefined ? o.value : getP(P.cur, path);
      return h("select", { class: "cp-in", "data-key": "cp:" + path, disabled: !can(o.lock || path) || null, "aria-label": o.aria || null,
                           onchange: (e) => { const nv = e.target.value; commit(o.label || "Changed a setting", (x) => (o.set ? o.set(x, nv) : setP(x, path, nv)), { path: o.lock || path, focus: "cp:" + path }); } },
        options.map(([value, text, off]) => h("option", { value, selected: String(value) === String(v ?? "") || null, disabled: off || null }, text)));
    }
    function tog(path, text, o = {}) {
      const v = o.value !== undefined ? o.value : Boolean(getP(P.cur, path));
      return h("label", { class: "cp-tg" + (o.light ? " cp-light" : "") },
        h("input", { type: "checkbox", "data-key": "cp:" + path, checked: v || null, disabled: o.off || !can(o.lock || path) || null,
                     onchange: (e) => { const c = e.target.checked; commit(o.label || text, (x) => (o.set ? o.set(x, c) : setP(x, path, c)), { path: o.lock || path, focus: "cp:" + path }); } }),
        h("span", { text }));
    }
    function addRow(path, ph, push, o = {}) {
      const lock = o.lock || path, ok = can(lock);
      const input = h("input", { type: "text", class: "cp-in" + (o.mono ? " cp-mono" : ""), "data-key": "cp-add:" + path, placeholder: ph,
                                 list: o.list || null, spellcheck: "false", autocomplete: "off", disabled: !ok || null, "aria-label": o.aria || ph,
                                 onkeydown: (e) => { if (e.key === "Enter") { e.preventDefault(); go(); } } });
      const go = () => {
        const v = input.value.trim();
        if (!v) return;
        const why = o.check ? o.check(v) : null;
        if (why) { toast(why, true); return; }
        commit(o.label || "Added it", (x) => push(x, v), { path: lock, focus: "cp-add:" + path });
      };
      return h("div", { class: "cp-lr" }, input, h("button", { type: "button", class: "cp-mini", disabled: !ok || null, onclick: go }, "Add"));
    }
    function chips(path, ph, o = {}) {
      const arr = getP(P.cur, path) || [], lock = o.lock || path, ok = can(lock);
      return h("div", {},
        arr.length ? h("div", { class: "cp-chips" }, arr.map((x, i) => h("span", { class: "cp-chip" }, String(x),
          h("button", { type: "button", "aria-label": `Remove ${x}`, disabled: !ok || null,
                        onclick: () => commit(`Removed ${x}`, (y) => { getP(y, path).splice(i, 1); }, { path: lock }) }, "×")))) : null,
        addRow(path, ph, (y, v) => { getP(y, path).push(v); }, o));
    }
    function bashHint(text) {
      const found = bashOnly(text);
      return found.length ? [h("div", { class: "cp-nh", text: `⚠ Uses ${found.join(", ")}, which /bin/sh may not support; AJX runs it with /bin/sh -c. Use POSIX syntax or wrap it in bash -c '…'.` })] : [];
    }
    function cmdList(path, ph) {
      const arr = getP(P.cur, path) || [], ok = can(path);
      return h("div", {},
        arr.map((c, i) => {
          const hint = h("div", {}, bashHint(c.run));
          return h("div", {},
            h("div", { class: "cp-lr" },
              inp(`${path}.${i}.run`, { mono: 1, area: String(c.run).includes("\n"), rows: 3, aria: `Command ${i + 1}`, lock: path, live: (v) => hint.replaceChildren(...bashHint(v)) }),
              h("button", { type: "button", class: "cp-x", "aria-label": `Remove command ${i + 1}`, disabled: !ok || null,
                            onclick: () => commit("Removed a command", (x) => { getP(x, path).splice(i, 1); }, { path }) }, "✕")),
            hint);
        }),
        addRow(path, ph, (x, v) => { getP(x, path).push(cmd(v)); }, { mono: 1, label: "Added a command" }));
    }
    function strList(path, ph, o = {}) {
      const arr = getP(P.cur, path) || [], lock = o.lock || path, ok = can(lock);
      return h("div", {},
        arr.map((x, i) => h("div", { class: "cp-lr" },
          inp(`${path}.${i}`, { mono: 1, lock, aria: `${o.aria || "Item"} ${i + 1}` }),
          h("button", { type: "button", class: "cp-x", "aria-label": "Remove", disabled: !ok || null,
                        onclick: () => commit("Removed it", (y) => { getP(y, path).splice(i, 1); }, { path: lock }) }, "✕"))),
        addRow(path, ph, (y, v) => { getP(y, path).push(v); }, { mono: 1, lock }));
    }
    function renameInput(path, current, taken, apply, what) {
      return h("input", {
        type: "text", class: "cp-in cp-mono", "data-key": "cp-rn:" + path, spellcheck: "false", autocomplete: "off",
        "aria-label": `${what} name`, disabled: !can(path) || null, value: current,
        onkeydown: (e) => { if (e.key === "Enter") { e.preventDefault(); e.target.blur(); } },
        onchange: (e) => {
          const v = e.target.value.trim().toLowerCase().replace(/[^a-z0-9._-]+/g, "-").replace(/^[^a-z0-9]+/, "");
          if (v === current) { e.target.value = current; return; }
          if (!SLUG.test(v)) { toast("Use a short lowercase name: letters, digits, dots, dashes and underscores.", true); e.target.value = current; return; }
          if (taken(v)) { toast(`“${v}” is already used.`, true); e.target.value = current; return; }
          commit(`Renamed “${current}” to “${v}”`, (x) => apply(x, v), { path, focus: "cp-rn:" + path });
        },
      });
    }
    function lockNote() {
      const k = lockKind();
      if (!k) return null;
      if (k === "job") return callout("lock", "🔒 Locked while a run or command is in progress.");
      return callout("lock", "🔒 Locked for this output folder: its attempts used these settings. You can still change the attempts and the output folder, and add workers. ",
        h("button", { type: "button", class: "link", disabled: dirty() || null, title: dirty() ? "Save or discard your changes first." : null, onclick: freshOutput },
          "Switch to a fresh output folder to edit everything"));
    }
    function kvs(rows) {
      return h("div", { class: "cp-kvs" }, rows.filter(Boolean).map(([k, ...v]) => h("div", {}, h("b", { text: k }), ...v)));
    }

    // ---------------------------------------------------------------- popover editors
    function popDef(id) {
      if (!id || !P.cur) return null;
      if (id === "task") return popTask();
      if (id === "checks") return popChecks();
      if (id === "run") return popRun();
      if (id === "reporter") return popReporter();
      if (id === "account") return popAccount();
      if (id === "advanced") return popAdvanced();
      if (id === "x-in" || id === "x-out") return popCross(id);
      if (id.startsWith("env-")) return popEnv(id);
      if (id.startsWith("worker-")) return popWorker(id);
      return null;
    }

    function lintHints(text) {
      const found = lintOf(text);
      return found.length ? found.map((l) => h("div", { class: "cp-nh", text: `⚠ “${l.match}” ${l.why}. Consider removing it.` }))
        : [h("div", { class: "cp-nh cp-okh", text: "✓ Neutral: no AJX vocabulary or hints found." })];
    }

    function popTask() {
      const t = P.cur.task, hints = h("div", {}, lintHints(t.prompt));
      return { layer: "1 · Your side · what goes in", title: "The task", wide: true, body: [
        lockNote(),
        F("Task prompt", inp("task.prompt", { area: 1, rows: 7, aria: "Task prompt", live: (v) => hints.replaceChildren(...lintHints(v)) }), null, { div: 1 }),
        hints,
        h("div", { class: "cp-h" }, `Saved as ${t.prompt_file || "task-prompt.md"}. The worker is told only this: say what the user wants done, not how, with no hints about expected problems. `,
          h("button", { type: "button", class: "link", onclick: () => askAgent("About the task prompt: ") }, "Ask my agent to change it")),
        F("Materials the worker may use", chips("task.materials", "URL or command, e.g. mytool --help", { label: "Added a material" }),
          "Recorded with the trial. The worker learns of them only through the prompt, so mention them there.", { div: 1 }),
        F("Starting files", inp("task.fixture", { mono: 1, ph: "no starting files" }), "A folder next to trial.toml, copied into each fresh workspace."),
        F("Human help", sel("task.human", [["unavailable", "Unavailable: a step that needs a person is recorded as a gate"], ["available", "Available"]], { label: "Changed human help" })),
        F("Setup before the clock starts", cmdList("task.setup", "command, e.g. git init"),
          "Runs in the worker's environment before the time limit starts, with /bin/sh.", { div: 1 }),
      ] };
    }

    function popChecks() {
      const m = P.cur, types = O().check_types || [["file", "File exists"], ["cmd", "Command succeeds"], ["http", "HTTP returns"]];
      const ok = can("checks");
      const move = (i, d) => commit("Moved a check", (x) => { const a = x.checks; [a[i], a[i + d]] = [a[i + d], a[i]]; }, { path: "checks" });
      const box = (c, i) => {
        const p = (f) => `checks.${i}.${f}`, known = types.some((t) => t[0] === c.type);
        const hint = h("div", {}, bashHint(c.run));
        return h("div", { class: "cp-ck" },
          h("div", { class: "cp-ckh" }, h("b", { text: String(i + 1) }), inp(p("name"), { ph: "Name this check", aria: `Check ${i + 1} name` }),
            h("button", { type: "button", class: "cp-x", title: "Move up", "aria-label": `Move check ${i + 1} up`, disabled: !i || !ok || null, onclick: () => move(i, -1) }, "↑"),
            h("button", { type: "button", class: "cp-x", title: "Move down", "aria-label": `Move check ${i + 1} down`, disabled: i === m.checks.length - 1 || !ok || null, onclick: () => move(i, 1) }, "↓"),
            h("button", { type: "button", class: "cp-x", title: "Remove", "aria-label": `Remove check ${i + 1}`, disabled: !ok || null,
                          onclick: () => commit("Removed a check", (x) => { x.checks.splice(i, 1); }, { path: "checks" }) }, "✕")),
          known ? F("Type", sel(p("type"), types, { label: "Changed the check type" })) : h("div", { class: "cp-h", text: `A “${c.type}” check from a plugin. Edit it in trial.toml.` }),
          c.type === "file" ? [F("Path in the workspace (glob)", inp(p("path"), { mono: 1, ph: "out.csv" })),
                               F("Contains (regex, optional)", inp(p("contains"), { mono: 1 }))] : null,
          c.type === "cmd" ? [F("Shell command", inp(p("run"), { mono: 1, area: String(c.run).includes("\n"), rows: 3, live: (v) => hint.replaceChildren(...bashHint(v)) })), hint,
                              h("div", { class: "cp-two" }, F("Expected exit code", inp(p("expect_exit"), { num: 1, step: 1 })), F("Output regex (optional)", inp(p("expect_stdout"), { mono: 1 }))),
                              F("Runs on", sel(p("where"), [["host", "Your machine, after the worker stops"], ["worker", "Inside the worker's environment"]], { label: "Changed where the check runs" })),
                              c.where === "worker" ? callout("warn", "This check shares the worker's files and tools, so the worker could influence it.") : null] : null,
          c.type === "http" ? [F("URL", inp(p("url"), { mono: 1, ph: "http://127.0.0.1:8080/health" })),
                               h("div", { class: "cp-two" }, F("Expected status", inp(p("expect_status"), { num: 1, step: 1 })), F("Body regex (optional)", inp(p("expect_body"), { mono: 1 })))] : null);
      };
      return { layer: "1 · Your side · after the worker stops", title: "Success checks", wide: true, body: [
        lockNote(),
        h("p", { class: "cp-lead", text: "Check the actual outcome (a file, an exit code), not what the worker says it did. AJX runs these after the worker stops; the worker never sees them. At least one is needed for a verified result." }),
        m.checks.map(box),
        m.checks.length ? null : callout("bad", "No checks yet: results will be unverified."),
        h("div", { class: "cp-lr cp-foot" },
          h("button", { type: "button", class: "cp-mini", disabled: !ok || null,
                        onclick: () => commit("Added a check", (x) => { x.checks.push(newCheck("file")); }, { path: "checks", focus: `cp:checks.${m.checks.length}.path` }) }, "+ Add check"),
          h("button", { type: "button", class: "link", onclick: () => askAgent("About the success check: ") }, "Ask my agent to write one")),
      ] };
    }

    function popRun() {
      const m = P.cur;
      const budget = m.workers.some((w) => w.cli === "claude-code") ? "Optional. Claude Code enforces it; other agents ignore it." : "Optional. None of these agents enforces a budget.";
      return { layer: "1 · Your side", title: "Run settings", body: [
        lockNote(),
        h("div", { class: "cp-two" },
          F("Time limit per attempt (minutes)", inp("run.minutes", { num: 1, min: 0.1 })),
          F("Attempts per worker", inp("run.attempts", { num: 1, min: 1, step: 1 }), "3 shows variance.")),
        F("Budget per attempt (USD)", inp("run.budget", { num: 1, min: 0, ph: "no budget" }), budget),
        F("Attempts at the same time", inp("run.parallel", { num: 1, min: 1, step: 1 }), "1 by default. More makes timings incomparable, and Stop then waits for every running attempt."),
        h("details", { class: "cp-adv", "data-key": "cp-adv-run" }, h("summary", { text: "Advanced" }),
          F("Reporting time limit (minutes)", inp("run.report_minutes", { num: 1, min: 0.1 }), "Time the reporter has after each attempt. It is not part of the time limit."),
          F("Workspace location", inp("run.workspace", { mono: 1, ph: "/tmp" }), "Each attempt gets a fresh folder here."),
          F("Shell startup files", sel("run.shell", [["inherit", "inherit: your ~/.zshenv runs in the worker's tool shell"], ["skip", "skip: the worker's tool shell starts without them"]], { label: "Changed the shell startup files" }),
            "For workers on your machine.")),
      ] };
    }

    function popReporter() {
      const m = P.cur, rc = repCli(m), hh = harness(rc), auto = (m.workers.find((w) => harness(w.cli).reporter) || { cli: "claude-code" }).cli;
      const list = "cp-models-reporter", r = route(rc, m.reporter.auth);
      const agents = [["", `Automatic: the first worker's agent that can report (${harness(auto).label})`],
                      ...(O().harnesses || []).filter((x) => x.reporter).map((x) => [x.name, `${x.label}${x.installed ? "" : " (not installed)"}`])];
      return { layer: "1 · Your side · after the worker stops", title: "Reporter", body: [
        lockNote(),
        h("p", { class: "cp-lead", text: "Writes the journey and the asks after each attempt, on your machine after the worker stops. Keep it the same for every worker you compare." }),
        F("Agent", sel("reporter.cli", agents, { label: "Changed the reporter agent",
          set: (x, v) => { if (x.reporter.cli !== v) { x.reporter.cli = v; if (!harness(repCli(x)).models.includes(x.reporter.model)) x.reporter.model = ""; } } })),
        F("Model", [inp("reporter.model", { mono: 1, ns: 1, ph: "agent default", list }), h("datalist", { id: list }, hh.models.map((x) => h("option", { value: x })))],
          `${hh.models.length ? `Suggestions: ${hh.models.join(", ")}. ` : ""}A stronger model writes a better analysis. No spaces.`),
        F("Sign-in", sel("reporter.auth", routes(rc).map((x) => [x.value, routeLabel(rc, x)]), { label: "Changed the reporter sign-in" }), routeDetail(rc, r)),
      ] };
    }

    function popAccount() {
      const a = P.cur.account;
      return { layer: "1 · Your side", title: "Task account", body: [
        lockNote(),
        tog("account.on", "This task acts in an account", { label: "Changed the task account",
          set: (x, c) => { x.account.on = c; if (!c && (x.account.identity || x.account.preflight.length)) P.note = "Saving removes the account check and the preflight commands; Undo brings them back."; } }),
        h("div", { class: "cp-h", text: "A cloud or SaaS account the task changes, not the account that pays for model calls." }),
        a.on ? [
          F("Account check command", inp("account.identity", { mono: 1, ph: "aws sts get-caller-identity --profile my-sandbox" }),
            acctName(P.cur) ? `Task account: ${acctName(P.cur)}. AJX records which account it is.` : "Shows which account the task acts in; AJX records its output."),
          F("Must pass before the worker starts (preflight)", cmdList("account.preflight", "command, e.g. the account check"),
            "Runs in the worker's environment before each attempt. The attempt does not start if one fails.", { div: 1 }),
        ] : callout("info", "The task works only in its own folder. Turn this on to add an account check and preflight commands."),
        F("Cleanup commands", cmdList("account.teardown", "command"),
          "Run in the worker's environment after the checks, with /bin/sh. Tag what the task creates with $AJX_RUN_TOKEN and delete by that tag.", { div: 1 }),
      ] };
    }

    function popAdvanced() {
      const s = P.snap, out = s.output, d = dirty();
      return { layer: "Advanced", title: "Output folder", body: [
        lockKind() === "job" ? lockNote() : null,
        F("Output folder", inp("run.output_dir", { mono: 1, ph: out.short }), "Reports and attempts are written here. Leave it empty for the default."),
        kvs([["Now writing to", h("span", { class: "cp-mono", text: out.short })],
             ["Attempts", out.started ? `${out.complete} of ${out.planned} planned finished` : "none yet"]]),
        out.attempts_exist
          ? callout("lock", "Settings are locked for this folder: its attempts used them. A fresh folder lets you change them. ",
              h("button", { type: "button", class: "link", disabled: d || lockKind() === "job" || null, title: d ? "Save or discard your changes first." : null, onclick: freshOutput }, "Switch to a fresh output folder"))
          : callout("ok", "No attempts here yet: every setting is editable."),
        h("div", { class: "cp-lr cp-foot" }, h("button", { type: "button", class: "link", onclick: openRaw }, "Edit trial.toml directly")),
      ] };
    }

    function openRaw() {
      closePop();
      ui.advancedOpen = true;
      loadTrialText(P.snap);
      renderTab("plan", true);
      const el = document.getElementById("advanced");
      if (el) el.scrollIntoView({ block: "start", behavior: "smooth" });
    }

    function popCross(id) {
      const m = P.cur, t = m.task, mine = m.workers.some((w) => kindOf(m, w) !== "container");
      if (id === "x-in") {
        return { layer: "The boundary", title: "What goes in", body: [
          kvs([["Prompt", `the task prompt (${String(t.prompt || "").length} characters)`],
               ["Starting files", t.fixture ? h("code", { text: t.fixture }) : "none"],
               ["Materials", t.materials.length ? t.materials.map((x) => [h("code", { text: x }), " "]) : "none (the worker learns of them only through the prompt)"],
               ["Human help", t.human === "unavailable" ? "unavailable" : "available"],
               ["Setup", t.setup.length ? `${plural(t.setup.length, "command")}, before the clock starts` : "none"],
               ["Never seen", "the success checks, the reporter's prompts, AJX's own files, your messages to your agent"]]),
          mine ? callout("warn", "On your machine the worker can also read anything you can. It is not a sandbox.") : null,
          h("div", { class: "cp-h", text: "Edit these on the task box." }),
        ] };
      }
      const inside = m.checks.filter((c) => c.where === "worker").length;
      return { layer: "The boundary", title: "What comes out", body: [
        kvs([["Comes out", "the workspace files and the transcript"],
             ["Then, on your machine", `success checks (${m.checks.length}) → reporter (${harness(repCli(m)).label} · ${m.reporter.model || "agent default"}) → results`],
             ["When", "after the worker stops"],
             inside ? ["Inside the environment", `${plural(inside, "check")} marked to run there, before the worker's environment is removed`] : null]),
      ] };
    }

    function popEnv(id) {
      const m = P.cur, i = m.envs.findIndex((e) => "env-" + e.id === id);
      if (i < 0) return null;
      const e = m.envs[i], k = e.kind, p = (f) => `envs.${i}.${f}`, n = m.workers.filter((w) => w.env === e.id).length, ok = can(p("kind"));
      const imgs = O().images || [], folderTaken = m.envs.some((o, j) => j !== i && o.kind === "folder");
      const kinds = [["folder", KIND.folder[0], folderTaken], ["profile", KIND.profile[0]], ["container", KIND.container[0]]];
      const container = k !== "container" ? [] : [
        F("Image", inp(p("image"), { mono: 1, ph: "repo@sha256:… or sha256:…" }), "Pinned by digest, so every attempt uses the same image. It must already be on this machine."),
        imgs.length ? F("Images on this machine", sel(p("image") + "#pick", [["", "Choose a local image…"], ...imgs.map((x) => [x.ref, x.label])],
                                                      { value: "", lock: p("image"), label: "Picked an image", set: (x, v) => { if (v) x.envs[i].image = v; } }))
                    : h("div", { class: "cp-h", text: "No local Docker images found. Pull or build one, then pin it by digest." }),
        F("Network", sel(p("network"), [["none", "none: no network"], ["bridge", "bridge: reaches your network and the internet"]], { label: "Changed the network" }),
          "An agent needs network access to reach its model API."),
        F("Protections that must be enforced", PROTECT.map(([key, label]) => tog(`${p("required")}.${key}`, label, {
          light: 1, value: (e.required || []).includes(key), off: key === "network_isolation" && e.network !== "none", lock: p("required"), label: `Changed: ${label.toLowerCase()}`,
          set: (x, c) => { const r = x.envs[i].required || (x.envs[i].required = []); const at = r.indexOf(key); if (c && at < 0) r.push(key); if (!c && at >= 0) r.splice(at, 1); } })),
          "If the container cannot enforce one, the run stops instead of continuing.", { div: 1 }),
        F("Limits", h("div", { class: "cp-three" }, [["cpus", "CPUs"], ["memory_mb", "Memory (MB)"], ["pids", "Processes"], ["tmpfs_mb", "Temp space (MB)"], ["lifetime_seconds", "Lifetime (s)"]]
          .map(([key, label]) => h("label", {}, h("span", { class: "cp-h", text: label }), inp(`${p("limits")}.${key}`, { num: 1, min: 0, value: (e.limits || {})[key] ?? "", aria: label, lock: p("limits") })))), null, { div: 1 }),
        F("Docker socket", inp(p("docker_host"), { mono: 1, ph: O().docker_host || "unix:///var/run/docker.sock" }),
          O().docker_host ? "Detected from the active Docker context." : "A local unix:// socket; never a remote daemon."),
      ];
      const tools = k === "folder" ? null : F("Tools the worker should start with", h("div", {},
        (e.tools || []).length ? h("div", { class: "cp-chips" }, e.tools.map((t, j) => h("span", { class: "cp-chip" }, t.name,
          h("button", { type: "button", class: "cp-rq", disabled: !ok || null, title: "Required stops the run when the tool is missing",
                        onclick: () => commit(`${t.name}: ${t.required === false ? "required" : "optional"}`, (x) => { const y = x.envs[i].tools[j]; y.required = y.required === false; }, { path: p("tools") }) },
            t.required === false ? "optional" : "required"),
          h("button", { type: "button", "aria-label": `Remove ${t.name}`, disabled: !ok || null, onclick: () => commit(`Removed ${t.name}`, (x) => { x.envs[i].tools.splice(j, 1); }, { path: p("tools") }) }, "×")))) : null,
        addRow(p("tools"), "python3, node, jq…", (x, v) => { x.envs[i].tools.push({ name: v, argv: [v, "--version"], required: true }); },
          { label: "Added a tool", check: (v) => (!TOOL.test(v) ? "A tool name: letters, digits, dots, dashes and underscores." : (e.tools || []).some((t) => t.name === v) ? "Already listed." : null) })),
        "AJX checks each one with --version before the attempt. Required stops the run when it is missing.", { div: 1 });
      const extra = Object.keys(e.extra || {});
      return { layer: "2 · Execution environment", title: envName(e), wide: k === "container", body: [
        lockNote(),
        callout(k === "container" ? "ok" : "warn", h("b", { text: k === "container" ? "Runs in a Docker container. " : "Not a sandbox. " }), k === "container" ? containerSays(e) : KIND[k][1]),
        k !== "folder" ? F("Name", renameInput(p("id"), e.id, (v) => m.envs.some((o) => o.id === v),
          (x, v) => { const old = x.envs[i].id; x.envs[i].id = v; for (const w of x.workers) if (w.env === old) w.env = v; return "env-" + v; }, "Environment")) : null,
        F("Kind", sel(p("kind"), kinds, { label: "Changed the kind of environment", set: (x, v) => changeKind(x, i, v) }),
          k === "folder" ? "Your machine has one fresh-folder box; every worker in it gets its own new folder." : null),
        ...container,
        F("Workspace location", inp("run.workspace", { mono: 1, ph: "/tmp", lock: p("kind") }),
          k === "container" ? "Shared by every environment. Docker on a Mac can only use folders its VM shares, usually your home folder." : "Shared by every environment. Each attempt gets a fresh folder here."),
        k !== "container" ? F("Shell startup files", sel("run.shell", [["inherit", "inherit: your ~/.zshenv runs in the worker's tool shell"], ["skip", "skip: the worker's tool shell starts without them"]],
                                                        { label: "Changed the shell startup files", lock: p("kind") })) : null,
        tools,
        extra.length ? F("Also set in trial.toml", h("div", { class: "cp-static cp-mono", text: extra.join(", ") }), "Kept as written. Change them in trial.toml.", { div: 1 }) : null,
        h("div", { class: "cp-lr cp-foot" },
          h("button", { type: "button", class: "cp-mini", disabled: !can("+worker") || null, onclick: () => addWorker(e.id) }, "+ Add worker here"),
          h("button", { type: "button", class: "cp-x", disabled: n || m.envs.length < 2 || !ok || null, title: n ? "Move its workers first." : null,
                        onclick: () => { commit(`Removed ${envName(e)}`, (x) => { x.envs.splice(i, 1); }, { path: p("kind") }); closePop(); } }, "Remove environment")),
      ] };
    }

    function popWorker(id) {
      const m = P.cur, i = m.workers.findIndex((w) => "worker-" + w.id === id);
      if (i < 0) return null;
      const w = m.workers[i], hh = harness(w.cli), e = envOf(m, w.env), k = kindOf(m, w), p = (f) => `workers.${i}.${f}`;
      const list = "cp-models-" + i, mods = modelsOf(w), r = route(w.cli, w.auth), extra = Object.keys(w.extra || {});
      const agents = (O().harnesses || []).map((x) => [x.name, `${x.label}: ${x.installed ? (x.verified ? "installed, verified live" : "installed") : "not installed"}`]);
      if (!agents.some((x) => x[0] === w.cli)) agents.unshift([w.cli, w.cli]);
      const signins = routes(w.cli).map((x) => [x.value, routeLabel(w.cli, x) + (k !== "folder" && !x.fresh_home ? " (only in a fresh folder on your machine)" : ""), k !== "folder" && !x.fresh_home]);
      if (!signins.some((x) => x[0] === (w.auth || ""))) signins.push([w.auth, w.auth]);
      return { layer: `3 · Test harness · inside ${envName(e)}`, title: `Worker “${w.id}”`, wide: true, body: [
        lockNote(),
        F("Name", renameInput(p("id"), w.id, (v) => m.workers.some((o) => o.id === v), (x, v) => { x.workers[i].id = v; return "worker-" + v; }, "Worker")),
        F("Agent CLI", sel(p("cli"), agents, { label: "Changed the agent; its model and sign-in start from their defaults",
          set: (x, v) => { const y = x.workers[i]; if (y.cli !== v) Object.assign(y, { cli: v, model: "", models: [], auth: "" }); } })),
        !hh.installed ? callout("bad", `${hh.label} is not installed here, so the readiness check would fail.`)
          : !hh.verified ? callout("info", "AJX's adapter for this agent follows its documented output format but has not been confirmed with a live run.") : null,
        F("Model", [inp(p("model"), { mono: 1, ns: 1, ph: "agent default", list }), h("datalist", { id: list }, hh.models.map((x) => h("option", { value: x })))],
          `Leave it empty for the agent's default.${hh.models.length ? ` Suggestions: ${hh.models.join(", ")}.` : ""} An unknown name makes the worker exit at once.`, { div: 1 }),
        F("Compare models (one run per model)", chips(p("models"), "add a model to compare", { list, label: "Added a model to compare", check: (v) => (/\s/.test(v) ? "Model names cannot contain spaces." : null) }),
          mods.length ? `${plural(mods.length, "run")} per attempt, one per model; “Model” above is not used.` : "Each model becomes its own result, with everything else the same.", { div: 1 }),
        hh.effort ? F("Effort", sel(p("effort"), (O().efforts || ["", "low", "medium", "high"]).map((x) => [x, x || "Agent default"]), { label: "Changed the effort" })) : null,
        k === "folder"
          ? F("Starting setup", sel(p("config"), [["clean", "Clean: without your skills, settings and MCP servers (a first-time user)"], ["user", "Your normal setup: tests the experience with your own tools"]], { label: "Changed the starting setup" }))
          : F("Starting setup", h("div", { class: "cp-static", text: "Clean: a fresh profile or a container never loads your own skills and settings." }), null, { div: 1 }),
        F("Skills given to the worker", [sel(p("skills"), [["none", "None"], ["folders", "Selected folders"]], { label: "Changed the skills" }),
          w.skills === "folders" ? strList(p("skillPaths"), "skill folder, e.g. skills/my-guide", { aria: "Skill folder" }) : null,
          w.skills === "folders" && k === "folder" ? callout("bad", "Skills need a fresh-profile or container environment. Change “Runs in” below.") : null],
          "Plugins and hooks stay off.", { div: 1 }),
        F("Sign-in", sel(p("auth"), signins, { label: "Changed the sign-in" }), routeDetail(w.cli, r)),
        F("Runs in", sel(p("env"), m.envs.map((x) => [x.id, `${envName(x)}: ${KIND[x.kind] ? KIND[x.kind][0] : x.kind}`]), { label: "Moved the worker" }),
          "You can also drag this card into another environment box on the map."),
        k !== "container" ? h("div", { class: "cp-nh", text: "On your machine the worker can read anything you can. Not a sandbox." }) : null,
        extra.length ? F("Also set in trial.toml", h("div", { class: "cp-static cp-mono", text: extra.join(", ") }), "Kept as written. Change them in trial.toml.", { div: 1 }) : null,
        h("div", { class: "cp-lr cp-foot" },
          h("button", { type: "button", class: "cp-x", disabled: m.workers.length < 2 || !can(p("remove")) || null,
                        onclick: () => { commit(`Removed worker “${w.id}”`, (x) => { x.workers.splice(i, 1); }, { path: p("remove") }); closePop(); } }, "Remove worker")),
      ] };
    }

    // ---------------------------------------------------------------- painting
    function derive() {
      P.changed = new Set(dirty() ? changeList(P.saved, P.cur).map((c) => c.node) : []);
      P.pv = new Set();
      const r = P.tab === "rec" && RECIPES.find((x) => x.id === P.recipe);
      if (r) for (const c of recipeResult(r).list) P.pv.add(c.node);
    }
    function cls(id, base) {
      return base + (P.open === id ? " cp-sel" : "") + (P.changed.has(id) ? " cp-chg" : "") + (P.flash.has(id) ? " cp-flash" : "") + (P.pv.has(id) ? " cp-pv" : "");
    }
    function node(id, title, sub, tag, tone, extra) {
      return h("button", { type: "button", class: cls(id, "cp-node" + (extra ? " " + extra : "")), "data-n": id,
                           "aria-expanded": String(P.open === id), "aria-haspopup": "dialog", onclick: () => openNode(id) },
        P.changed.has(id) ? h("i", { class: "cp-dot", title: "Edited, not saved" }, h("span", { class: "cp-sr", text: "Edited, not saved." })) : null,
        tag ? h("span", { class: "cp-tag cp-" + tone, text: tag }) : null,
        h("b", { text: title }), h("small", { text: sub }));
    }

    function paintMap() {
      const m = P.cur, s = P.snap, t = m.task, r = m.run, ac = m.account, nc = m.checks.length, lint = lintOf(t.prompt).length, E = effW(m);
      const budget = String(r.budget ?? "").trim() === "" ? "no budget" : `$${r.budget}`, par = Number(r.parallel) || 1;
      const att = Number(r.attempts) || r.attempts;
      const z1 = h("div", { class: "cp-zone cp-z1" },
        h("div", { class: "cp-zh" }, h("i", {}, h("em", { text: "1" })), "Your side"),
        h("div", { class: "cp-zs", text: "Your machine. You set the task; AJX checks and reports after the worker stops." }),
        h("div", { class: "cp-ng" },
          node("task", "The task", `prompt · ${plural(t.materials.length, "material")} · ${t.fixture ? "starting files" : "no starting files"}`, lint ? "check wording" : "neutral", lint ? "a" : "b"),
          node("checks", "Success checks", nc ? `${plural(nc, "check")} · the worker never sees them` : "none yet: results unverified", nc ? "after" : "none", nc ? "g" : "r"),
          node("run", "Run settings", `${minutes(r.minutes)} · ${att} attempt${att === 1 ? "" : "s"} · ${budget}`, par > 1 ? `${par} at once` : "one by one", par > 1 ? "a" : "b"),
          node("reporter", "Reporter", `${harness(repCli(m)).label}${m.reporter.cli ? "" : " (automatic)"} · ${m.reporter.model || "agent default"} · same for all`, "after", "g"),
          node("account", "Task account", ac.on ? `${acctName(m) || "an account"} · preflight ${ac.preflight.length} · cleanup ${ac.teardown.length}` : `off · the task works only in its folder${ac.teardown.length ? ` · cleanup ${ac.teardown.length}` : ""}`,
               ac.on ? "on" : "off", ac.on ? "a" : "m", ac.on ? "" : "cp-dim"),
          node("advanced", "Advanced", lockKind() === "attempts" ? `output folder · ${plural(s.output.started, "attempt")} recorded` : `output folder · ${s.output.attempts_exist ? plural(s.output.started, "attempt") : "no attempts yet"}`,
               lockKind() === "attempts" ? "locked" : "", "m", "cp-dim")),
        h("div", { class: "cp-chain", "aria-hidden": "true" }, h("span", { text: "worker output" }), h("span", { class: "cp-ln" }), h("span", { text: "checks" }),
          h("span", { class: "cp-ln" }), h("span", { text: "reporter" }), h("span", { class: "cp-ln" }), h("b", { text: "results" })));
      const arrow = (id, cl, arrowText, text) => h("button", { type: "button", class: cls(id, "cp-arrow" + cl), "data-n": id, "aria-expanded": String(P.open === id), onclick: () => openNode(id) },
        h("b", { text: arrowText }), text);
      const cross = h("div", { class: "cp-cross" }, h("span", { class: "cp-lab", text: "BOUNDARY" }),
        arrow("x-in", "", "→", "in: prompt + starting files"), arrow("x-out", " cp-out", "←", "out: files + transcript → checks → reporter"));
      const anyLock = Boolean(lockKind());
      const z2 = h("div", { class: "cp-zone cp-z2" },
        h("div", { class: "cp-zh" }, h("i", {}, h("em", { text: "2" })), "Execution environments", h("span", { class: "cp-zx", text: E > 1 ? ` · ${E} workers = a comparison` : " · where the worker runs" })),
        m.envs.map(envBox),
        h("div", { class: "cp-addrow" },
          m.envs.some((e) => e.kind === "folder") ? null : h("button", { type: "button", class: "cp-addb cp-light", disabled: anyLock || null, onclick: () => addEnv("folder") }, "+ Fresh folder on my machine"),
          h("button", { type: "button", class: "cp-addb cp-light", disabled: anyLock || null, onclick: () => addEnv("profile") }, "+ Fresh profile on my machine"),
          h("button", { type: "button", class: "cp-addb cp-light", disabled: anyLock || null, onclick: () => addEnv("container") }, "+ Container")));
      swap(P.mapEl, [z1, cross, z2]);
    }

    function envFacts(m, e) {  // one line under an environment's name; the Live map shows the same
      const lim = { ...LIMITS, ...(e.limits || {}) };
      return e.kind === "container" ? `Docker · ${shortImg(e.image)} · ${lim.cpus} CPU, ${lim.memory_mb} MB · network ${e.network}`
        : e.kind === "folder" ? `on your machine · a fresh folder in ${m.run.workspace || "/tmp"} · runs as you` : "on your machine · a fresh home and config · runs as you";
    }

    function envBox(e) {
      const m = P.cur, k = e.kind === "container", ws = m.workers.filter((w) => w.env === e.id), id = "env-" + e.id;
      const facts = envFacts(m, e);
      const box = h("div", { class: "cp-env" + (k ? " cp-sealed" : "") + (P.changed.has(id) ? " cp-chg" : "") + (P.pv.has(id) ? " cp-pv" : "") + (P.flash.has(id) ? " cp-flash" : ""), "data-drop": e.id,
        ondragover: (ev) => { if (!P.drag || P.drag.env === e.id || !can(P.drag.path)) return; ev.preventDefault(); ev.dataTransfer.dropEffect = "move"; box.classList.add("cp-drop"); },
        ondragleave: () => box.classList.remove("cp-drop"),
        ondrop: (ev) => {
          ev.preventDefault();
          box.classList.remove("cp-drop");
          const d = P.drag;
          P.drag = null;
          if (d && d.env !== e.id) commit(`Moved “${d.id}” to ${envName(e)}`, (x) => { const w = x.workers.find((y) => y.id === d.id); if (w) w.env = e.id; }, { path: d.path });
        } },
        h("button", { type: "button", class: "cp-envbtn" + (P.open === id ? " cp-sel" : ""), "data-n": id, "aria-expanded": String(P.open === id), "aria-haspopup": "dialog", onclick: () => openNode(id) },
          h("div", { class: "cp-eh" }, h("b", { text: envName(e) }), h("span", { class: "cp-spc" }), h("span", { class: "cp-kind " + (k ? "cp-g" : "cp-a"), text: k ? "Runs in a container" : "Not a sandbox" })),
          h("div", { class: "cp-ef", text: facts })),
        h("div", { class: "cp-harness" }, h("div", { class: "cp-hh", text: "3 · Test harness" }),
          ws.length ? ws.map(workerCard) : h("div", { class: "cp-empty", text: "No worker here. Drag one in, or add one." }),
          h("button", { type: "button", class: "cp-addb", disabled: !can("+worker") || null, onclick: () => addWorker(e.id) }, "+ Add worker to compare")));
      return box;
    }

    function workerCard(w) {
      const m = P.cur, hh = harness(w.cli), mods = modelsOf(w), i = m.workers.indexOf(w), path = `workers.${i}.env`;
      const model = mods.length ? `${mods.join(" + ")} (${plural(mods.length, "run")})` : w.model || "agent default";
      const signin = authText(w.cli, w.auth);
      const tag = !hh.installed ? ["not installed", "r"] : w.config === "user" ? ["your setup", "a"] : ["clean", "g"];
      const free = can(path);
      return h("div", { class: "cp-wrap", draggable: free ? "true" : "false", "data-w": w.id,
                        ondragstart: (ev) => { P.drag = { id: w.id, env: w.env, path }; ev.dataTransfer.setData("text/plain", w.id); ev.dataTransfer.effectAllowed = "move"; },
                        ondragend: () => { P.drag = null; } },
        node("worker-" + w.id, `Worker “${w.id}”`, `${hh.label} · ${model} · ${signin}`, tag[0], tag[1]));
    }

    function paintCoach() {
      const I = items(P.cur), ok = I.filter((i) => i.st === "ok").length, need = I.length - ok, order = { bad: 0, warn: 1, ok: 2 };
      const sorted = [...I].sort((a, b) => order[a.st] - order[b.st]), lk = lockKind();
      const fixable = (i) => i.fn && (!lk || (lk === "attempts" && i.free));
      const row = (i) => h("div", { class: "cp-ci cp-" + i.st },
        h("span", { class: "cp-ic", "aria-hidden": "true", text: { ok: "✓", warn: "!", bad: "✕" }[i.st] }),
        h("div", {}, h("span", { class: "cp-sr", text: { ok: "In order: ", warn: "Worth a look: ", bad: "Needs a fix: " }[i.st] }), h("b", { text: i.t }), h("div", { class: "cp-w", text: i.w }),
          i.st !== "ok" && (fixable(i) || i.show) ? h("div", { class: "cp-ba" },
            fixable(i) ? h("button", { type: "button", class: "cp-fx", onclick: () => commit(i.fix, i.fn, { path: i.free ? "run.attempts" : "*", open: i.open, focus: i.focus }) }, "Fix: " + i.fix) : null,
            i.show ? h("button", { type: "button", class: "cp-sh", onclick: () => openNode(i.show, { force: true }) }, "Show me") : null) : null));
      let body;
      if (P.tab === "check") {
        body = [need ? h("div", { class: "cp-gh", text: `Needs attention · ${need}` }) : callout("ok", "Everything on the list is in order."),
                sorted.filter((i) => i.st !== "ok").map(row), h("div", { class: "cp-gh", text: `In order · ${ok}` }), sorted.filter((i) => i.st === "ok").map(row)];
      } else {
        body = [h("p", { class: "cp-small", text: "A recipe sets many things at once. Open one to see exactly what it changes: nothing changes until you press Apply, and Apply can be undone." }),
          lk ? callout("lock", lk === "job" ? "Recipes are off while a run or command is in progress." : "Recipes are off for an output folder with attempts. Switch to a fresh output folder first.") : null,
          RECIPES.map((r) => {
            const open = P.recipe === r.id, res = open ? recipeResult(r) : null, boxes = res ? new Set(res.list.map((c) => c.node)).size : 0;
            return h("div", { class: "cp-rc" + (open ? " cp-open" : "") },
              h("button", { type: "button", class: "cp-rh", "data-fk": "rc:" + r.id, "aria-expanded": String(open), onclick: () => { P.recipe = open ? null : r.id; paintAll(); const c = P.coachEl.querySelector(".cp-rc.cp-open"); if (c) c.scrollIntoView({ block: "nearest" }); } }, h("b", { text: r.n }), h("small", { text: r.d })),
              open ? h("div", { class: "cp-rb" },
                res.blocked ? h("div", { class: "cp-small", text: res.blocked })
                  : res.list.length ? [h("div", { class: "cp-small", text: `${plural(res.list.length, "change")} on ${plural(boxes, "box").replace("boxs", "boxes")} (outlined on the map):` }),
                                       h("ul", {}, res.list.map((c) => h("li", {}, c.kv ? [`${c.text}: `, h("s", { text: shown(c.from) }), " → ", h("em", { text: shown(c.to) })] : c.text)))]
                    : h("div", { class: "cp-small", text: "Already applied: nothing would change." }),
                h("div", { class: "cp-lr" },
                  h("button", { type: "button", class: "cp-fx", disabled: !res.list.length || lk || null,
                                onclick: () => { P.recipe = null; P.tab = "check"; commit(`Applied “${r.n}”`, r.f, { open: r.open, focus: r.focus || null }); } }, "Apply recipe"),
                  h("button", { type: "button", class: "cp-sh", onclick: () => { P.recipe = null; paintAll(); } }, "Cancel"))) : null);
          })];
      }
      const tabBtn = (t, label, n) => h("button", { type: "button", role: "tab", "data-fk": "tab:" + t, "aria-selected": String(P.tab === t), onclick: () => { P.tab = t; P.recipe = null; paintAll(); } }, label, h("i", { text: String(n) }));
      swap(P.coachEl, [
        h("div", { class: "cp-ch" },
          h("h3", {}, "Best result", h("small", { text: "checks your setup for trustworthy results" })),
          h("div", { class: "cp-meter", "aria-hidden": "true" }, I.map((i) => h("i", { class: "cp-" + i.st }))),
          h("div", { class: "cp-score" }, h("b", { text: `${ok} of ${I.length}` }), ` in order${need ? ` · ${need} to look at` : ""}. Advice on setup quality, not a guarantee.`)),
        h("div", { class: "cp-ctabs", role: "tablist", "aria-label": "Best result" }, tabBtn("check", "Checklist", need), tabBtn("rec", "Recipes", RECIPES.length)),
        h("div", { class: "cp-clist", "data-scroll": "cp-clist", role: "tabpanel" }, body)]);
    }

    function paintStart() {
      const s = P.snap, r = s.readiness, st = P.stale
        ? { disabled: true, label: startState(s).label, reason: "trial.toml changed on disk while you were editing. Press Reload first, so you approve the plan you can see." }
        : startState(s), warns = r.current ? r.warnings || [] : [], active = ACTIVE.includes(s.process.state);
      let head;
      if (r.status === "checking") head = h("div", { class: "okbox", text: "Checking: ajx validate, then ajx doctor…" });
      else if (!r.current) head = h("div", { class: "cp-small", text: r.status === "unknown" ? "Not checked for this plan yet." : "Last checked for an earlier version of the plan." });
      else if (r.status === "ready") head = h("div", { class: "okbox", text: `✓ validate and doctor passed at ${clock(r.checked_at)}${warns.length ? ` with ${plural(warns.length, "warning")}` : ""}.` });
      else head = h("div", { class: "errbox" }, h("b", { text: "Not ready. " }), "Fix these, then check again:", h("ul", { class: "list" }, (r.problems || []).slice(0, 6).map((p) => h("li", { class: "mono", text: p }))));
      const warnBox = !warns.length ? null : r.needs_ack
        ? h("div", { class: "warnbox" }, h("b", { text: "This task acts in an account. " }), "Read the warnings before starting:", h("ul", { class: "list" }, warns.map((w) => h("li", { text: w }))),
            h("label", {}, h("input", { type: "checkbox", "data-key": "cp-accept", checked: ui.acceptWarnings || null, onchange: (e) => { ui.acceptWarnings = e.target.checked; paintStart(); } }),
              h("span", { text: "I accept these warnings for this run" })))
        : h("div", { class: "notebox" }, h("b", { text: "Notes from doctor: " }), h("ul", { class: "list" }, warns.map((w) => h("li", { text: w }))));
      const mine = (P.saved.workers || []).some((w) => kindOf(P.saved, w) !== "container");
      swap(P.startEl, [
        h("div", { class: "cp-srow" }, h("h3", { text: "Start" }),
          h("span", { class: "cp-small" }, "Plan ", h("code", { title: "A fingerprint of " + (s.plan.covers || []).join(", ") + ". Any change gives a new plan id.", text: s.plan.short.slice(0, 7) }))),
        head,
        h("div", { class: "cp-small cp-checkrow" },
          h("button", { type: "button", class: "link", disabled: active || s.trial.error || P.stale || null, onclick: () => act("/api/check", {}, "Checking readiness…") }, "Check now"),
          (r.steps || []).length ? h("details", { "data-key": "cp-ready-steps" }, h("summary", { text: "What the checks printed" }),
            (r.steps || []).map((x) => h("div", {}, h("b", { class: "tiny", text: `${x.name}: ${x.exit_code === 0 ? "passed" : x.timed_out ? "timed out" : "exit " + x.exit_code}` }), h("pre", { class: "logtail", text: x.output })))) : null),
        warnBox,
        h("button", { type: "button", class: "cp-btn", "data-fk": "start", disabled: st.disabled || null, "aria-describedby": "cp-why",
                      onclick: (e) => { e.currentTarget.disabled = true; act("/api/start", { plan: P.snap.plan.sha, accept_warnings: ui.acceptWarnings }, "Checking readiness, then starting…"); } }, st.label),
        h("div", { id: "cp-why", class: "cp-why" + (st.disabled ? " cp-block" : ""), text: st.reason }),
        h("div", { class: "cp-small", text: `Starting approves ${(s.plan.covers || []).join(", ")}; any later change needs a new approval. `
          + (mine ? "Workers on your machine run as you, without asking before each command. Model calls can cost money."
                  : "The worker runs commands without asking you first, inside its container. Model calls can cost money.") }),
        runAgainBox(s)]);
    }

    function paintBar() {
      const d = dirty(), lk = lockKind(), sig = d ? stable(P.cur) : "";
      const ch = d ? changeList(P.saved, P.cur) : [], nodes = [...new Set(ch.map((c) => c.node))];
      const pv = P.preview, fresh = pv && pv.sig === sig, blockers = d ? clientProblems(P.cur).filter((x) => x.block).map((x) => x.msg) : [];
      const probs = [...blockers, ...(d && pv ? pv.problems || [] : [])];
      const prevBtn = h("button", { type: "button", "aria-expanded": String(P.prev), onclick: () => { P.prev = !P.prev; paintBar(); paintPrev(); } },
        d ? `What this writes ${P.prev ? "▾" : "▴"}` : P.prev ? "Hide trial.toml" : "View trial.toml");
      let kids;
      if (P.stale) {
        P.barEl.className = "cp-bar cp-stale";
        kids = [h("span", { class: "cp-dot2" }), h("div", { class: "cp-grow" }, h("b", { text: "trial.toml changed on disk while you were editing" }),
                  h("small", { text: `By your agent or an editor. Reload shows the new version and drops your ${plural(ch.length, "unsaved change")}.` })),
                h("button", { type: "button", onclick: () => { if (!d || confirm(`Drop your ${plural(ch.length, "unsaved change")} and load the file from disk?`)) { adopt(P.snap); paintAll(); } } }, "Reload")];
      } else if (d) {
        P.barEl.className = "cp-bar";
        const why = probs.length ? h("small", { class: "cp-bad2", text: `Cannot save yet: ${probs[0]}` })
          : h("small", { text: `${nodes.slice(0, 4).map(nodeWord).join(" · ")}${nodes.length > 4 ? " …" : ""} · Start waits until you save or discard` });
        kids = [h("span", { class: "cp-dot2" }), h("div", { class: "cp-grow" }, h("b", { text: plural(ch.length, "unsaved change") }), why),
                h("button", { type: "button", disabled: !P.undo.length || lk === "job" || P.saving || null, onclick: () => undo() }, "Undo"), prevBtn,
                h("button", { type: "button", disabled: P.saving || null, onclick: discard }, "Discard"),
                h("button", { type: "button", class: "cp-pri", disabled: P.saving || blockers.length || (fresh && probs.length) || lk === "job" || null, onclick: save }, P.saving ? "Saving…" : "Save")];
      } else {
        P.barEl.className = "cp-bar cp-clean";
        kids = [h("span", { class: "cp-dot2" }), h("div", { class: "cp-grow" }, lk === "job" ? "Read only while a run or command is in progress · " : "All changes saved · ",
                  "trial.toml matches plan ", h("code", { text: P.snap.plan.short.slice(0, 7) })),
                P.undo.length && lk !== "job" ? h("button", { type: "button", onclick: () => undo() }, "Undo") : null, prevBtn];
      }
      swap(P.barEl, kids);
    }

    function paintPrev() {
      const el = P.prevEl;
      el.hidden = !P.prev;
      if (!P.prev) { el.replaceChildren(); return; }
      const d = dirty(), pv = P.preview, sig = d ? stable(P.cur) : "", blockers = d ? clientProblems(P.cur).filter((x) => x.block).map((x) => x.msg) : [];
      let lines;
      if (d && !pv) lines = [h("span", { class: "cp-l-c", text: "Working out what this writes…" })];
      else if (d && P.full) lines = String(pv.text || "").split("\n").map((l) => h("span", { class: "cp-l-c", text: l || " " }));
      else if (d) {
        lines = (pv.diff || []).length ? pv.diff.map((l) => h("span", { class: /^(\+\+\+|---)/.test(l) || l.startsWith("@@") ? "cp-l-h" : l.startsWith("+") ? "cp-l-a" : l.startsWith("-") ? "cp-l-r" : "cp-l-c", text: l || " " }))
          : [h("span", { class: (pv.problems || []).length ? "cp-l-r" : "cp-l-c", text: (pv.problems || []).length ? "AJX cannot write this edit yet. Fix the problem above first." : "No change to the files: the edit writes the same text." })];
      } else if (P.full) {
        const ft = P.fileText;
        if (!ft || ft.sha !== P.snap.trial.sha256) { lines = [h("span", { class: "cp-l-c", text: "Loading trial.toml…" })]; loadFile(); }
        else lines = ft.error ? [h("span", { class: "cp-l-r", text: ft.error })] : ft.text.split("\n").map((l) => h("span", { class: "cp-l-c", text: l || " " }));
      } else lines = [h("span", { class: "cp-l-c", text: "No changes: trial.toml is exactly as saved." })];
      const seg = (full, text) => h("button", { type: "button", "aria-pressed": String(P.full === full), onclick: () => { P.full = full; paintPrev(); } }, text);
      swap(el, [
        h("div", { class: "cp-pt" }, "What this writes", h("span", { class: "cp-small", text: d ? (P.full ? "trial.toml after saving" : "changed lines, as a diff") : "trial.toml" }),
          h("span", { class: "cp-spc" }), h("span", { class: "cp-seg" }, seg(false, "Changed lines"), seg(true, "Whole file")),
          h("button", { type: "button", class: "cp-close", "aria-label": "Close", onclick: () => { P.prev = false; paintBar(); paintPrev(); } }, "✕")),
        d && pv && pv.sig !== sig ? h("div", { class: "notebox", text: "Updating for your latest edit…" }) : null,
        d && pv && (pv.added_auth || []).length ? h("div", { class: "notebox", text: `Adds sign-in ${pv.added_auth.map((x) => `[auth.${x}]`).join(", ")}: it reads keys from the terminal that started the console; no secret is written.` }) : null,
        d && (blockers.length || (pv && (pv.problems || []).length)) ? h("div", { class: "errbox" }, h("b", { text: "Cannot save yet. " }), [...blockers, ...((pv && pv.problems) || [])].join(" ")) : null,
        h("pre", { "data-scroll": "cp-prev" }, lines)]);
    }

    async function loadFile() {
      const sha = P.snap.trial.sha256;
      if (P.fileLoading === sha) return;
      P.fileLoading = sha;
      try {
        const r = await api("/api/trial-text");
        P.fileText = { sha: r.sha256, text: r.text };
      } catch (e) {
        P.fileText = { sha, text: "", error: e.message };
      } finally {
        P.fileLoading = null;
      }
      if (P.fileText.sha !== P.snap.trial.sha256) P.fileText.sha = P.snap.trial.sha256;
      paintPrev();
    }

    function paintPop(focusKey) {
      const root = P.popRoot, d = P.open ? popDef(P.open) : null;
      if (!d) { root.replaceChildren(); P.popEl = null; return; }
      const a = document.activeElement, hadFocus = Boolean(a && root.contains(a));
      const keep = hadFocus && a.dataset.key ? { key: a.dataset.key, s: a.selectionStart, e: a.selectionEnd } : null;
      const same = P.popEl && P.popEl.dataset.node === P.open, body = P.popEl && P.popEl.querySelector(".cp-pb"), top = same && body ? body.scrollTop : 0;
      const note = P.note;
      P.note = "";
      const pop = h("div", { class: "cp-pop" + (d.wide ? " cp-wide" : ""), role: "dialog", "aria-label": d.title, tabindex: "-1", "data-node": P.open },
        h("div", { class: "cp-ph" }, h("div", {}, h("div", { class: "cp-layer", text: d.layer }), h("h2", { text: d.title })),
          h("button", { type: "button", class: "cp-close", "aria-label": "Close editor", onclick: () => closePop() }, "✕")),
        h("div", { class: "cp-pb" }, note ? callout("info", note) : null, d.body),
        h("div", { class: "cp-pf", text: lockKind() === "job" ? "Read only while a run or command is in progress." : "Edits show on the map right away and stay unsaved until you press Save." }));
      root.replaceChildren(pop);
      P.popEl = pop;
      placePop();
      if (same) pop.querySelector(".cp-pb").scrollTop = top;
      const want = focusKey || (keep && keep.key);
      if (want) {
        const el = pop.querySelector(`[data-key="${CSS.escape(want)}"]`);
        if (el && !el.disabled) {
          el.focus({ preventScroll: true });
          try { if (keep && keep.key === want && keep.s !== null && keep.s !== undefined) el.setSelectionRange(keep.s, keep.e); } catch (e) { /* not a text field */ }
        } else pop.focus({ preventScroll: true });
      } else if (hadFocus) pop.focus({ preventScroll: true });
    }

    function placePop() {
      const pop = P.popEl;
      if (!pop || !P.el || !P.el.isConnected) return;
      const a = anchor(P.open);
      if (!a || !a.getClientRects().length) { pop.hidden = true; return; }
      pop.hidden = false;
      const page = P.el.getBoundingClientRect(), r = a.getBoundingClientRect(), W = pop.offsetWidth, H = pop.offsetHeight;
      const right = r.left + r.width / 2 < innerWidth * 0.45, TOP = 112, BOTTOM = 84;
      let left = right ? r.right + 14 : r.left - W - 14;
      left = Math.max(8, Math.min(left, innerWidth - W - 8));
      const top = Math.max(TOP, Math.min(r.top, innerHeight - H - BOTTOM));
      pop.classList.toggle("cp-pr", right);
      pop.classList.toggle("cp-pl", !right);
      pop.style.left = `${left - page.left}px`;
      pop.style.top = `${top - page.top}px`;
      pop.style.setProperty("--cy", `${Math.max(14, Math.min(H - 30, r.top + r.height / 2 - top - 6))}px`);
    }

    function openNode(id, o = {}) {
      if (P.open === id && !o.force) { closePop(); return; }
      if (!popDef(id)) return;
      P.open = id;
      derive();
      paintMap();
      paintPop(o.focus);
      if (!o.focus && P.popEl) P.popEl.focus({ preventScroll: true });
      const a = anchor(id);
      if (a) {
        const r = a.getBoundingClientRect();
        if (r.top < 112 || r.bottom > innerHeight - 84) { a.scrollIntoView({ block: "center", behavior: "smooth" }); setTimeout(placePop, 400); }
      }
    }
    function closePop() {
      const id = P.open;
      if (!id) return;
      P.open = null;
      paintMap();
      paintPop();
      const a = anchor(id);
      if (a) a.focus({ preventScroll: true });
    }

    function paintAll(focus) {
      derive();
      paintMap();
      paintCoach();
      paintStart();
      paintBar();
      paintPrev();
      paintPop(focus);
    }
    function paintLive() {  // while typing: everything but the popover, so the field keeps its focus
      derive();
      paintMap();
      paintCoach();
      paintStart();
      paintBar();
      if (P.prev) paintPrev();
      placePop();
    }

    function paintHead() {
      const s = P.snap;
      swap(P.headEl, [h("h1", { text: "Configure the trial, then start" }),
        h("p", { class: "cp-lede" }, "Click any box to edit it; the map updates as you type. The ", h("b", { text: "Best result" }),
          " coach checks the setup and can fix many things at once. Everything here comes from ", h("span", { class: "mono", text: s.trial.short }),
          "; your agent and this page edit the same files."),
        s.trial.error ? h("div", { class: "cp-card cp-errcard" }, h("b", { text: "AJX cannot load this trial file. " }), "Nothing can start until it loads. Fix it in a box, under Advanced, or ask your agent.",
                          h("pre", { text: s.trial.error })) : null]);
    }

    // ---------------------------------------------------------------- server sync and the entry point
    function adopt(s) {
      P.saved = clone(s.plan_model);
      P.cur = clone(s.plan_model);
      P.savedSig = stable(P.saved);
      P.base = { sha: s.trial.sha256, prompt: (s.trial.prompt || {}).sha256 || "" };
      P.serverSig = stable([s.plan_model, s.trial.sha256, (s.trial.prompt || {}).sha256]);
      P.undo = [];
      P.typing = null;
      P.preview = null;
      P.stale = false;
      P.adoptNext = false;
      if (P.open && !popDef(P.open)) P.open = null;
    }
    // Adopt the server's model when there are no local edits, or right after our own save; otherwise
    // keep the edits and say that trial.toml changed on disk.
    function sync(s) {
      const sig = stable([s.plan_model, s.trial.sha256, (s.trial.prompt || {}).sha256]);
      if (sig === P.serverSig) {
        if (P.stale && !dirty()) { adopt(s); return true; }
        return false;
      }
      const first = P.serverSig === null, ours = P.adoptNext;
      if (first || !dirty() || ours) {
        const before = P.saved;
        adopt(s);
        if (!first && before) {
          const nodes = changeList(before, P.saved).map((c) => c.node);
          flashNodes(nodes);
          if (!ours && !ui.inflight && nodes.length) toast("trial.toml changed on disk (by your agent or an editor). The map shows the new version.");
        }
        return true;
      }
      P.serverSig = sig;
      if (stable(s.plan_model) === P.savedSig) {  // e.g. a comment or a setting the editor does not show
        P.base = { sha: s.trial.sha256, prompt: (s.trial.prompt || {}).sha256 || "" };
        P.stale = false;
        schedulePreview();
        return true;
      }
      P.stale = true;
      return true;
    }

    function build() {
      P.headEl = h("div", {});
      P.mapEl = h("div", { class: "cp-map" });
      P.advEl = h("div", {});
      P.coachEl = h("section", { class: "cp-card cp-coach", "aria-label": "Best result coach" });
      P.startEl = h("section", { class: "cp-card cp-start", "aria-label": "Start" });
      P.popRoot = h("div", {});
      P.prevEl = h("div", { class: "cp-prev", role: "region", "aria-label": "What this writes", hidden: true });
      P.barEl = h("div", { class: "cp-bar", role: "region", "aria-label": "Unsaved changes" });
      P.el = h("div", { class: "cp-page" },
        h("div", { class: "cp-main" }, P.headEl,
          h("section", { class: "cp-card", "aria-label": "Trial map" }, P.mapEl,
            h("div", { class: "cp-legend" },
              h("span", {}, h("i", { class: "cp-sw cp-d" }), "runs as you, not a sandbox"), h("span", {}, h("i", { class: "cp-sw cp-s" }), "runs in a container"),
              h("span", {}, h("i", { class: "cp-sw cp-x2" }), BOUNDARY),
              h("span", {}, h("i", { class: "cp-sw cp-c" }), "would change if you apply the open recipe"),
              h("span", {}, h("span", { class: "cp-bl", text: "●" }), " edited, not saved"))),
          P.advEl),
        h("aside", { class: "cp-side" }, P.coachEl, P.startEl),
        P.popRoot, P.prevEl, P.barEl);
      document.addEventListener("keydown", (e) => {
        if (e.key !== "Escape" || !P.el.isConnected || document.getElementById("tab-plan").hidden) return;
        if (P.open) { e.preventDefault(); closePop(); } else if (P.prev) { P.prev = false; paintBar(); paintPrev(); }
      });
      document.addEventListener("mousedown", (e) => {
        if (P.open && P.mapEl.contains(e.target) && !e.target.closest("[data-n]") && !e.target.closest("button")) closePop();
      });
      addEventListener("resize", () => placePop());
    }

    function render(s) {
      P.snap = s;
      if (!P.el) build();
      const lockWas = P.lastLock;
      const changed = sync(s);
      P.lastLock = lockKind();
      if (P.open && !popDef(P.open)) P.open = null;
      if (!P.opened) {
        P.opened = true;
        const want = hash.get("open");
        if (want && popDef(want)) {
          P.open = want;
          requestAnimationFrame(() => { const a = anchor(want); if (a) a.scrollIntoView({ block: "center" }); placePop(); });
        }
      }
      derive();
      paintHead();
      paintMap();
      paintCoach();
      paintStart();
      paintBar();
      paintPrev();
      P.advEl.replaceChildren(advancedCard(s));
      if (changed || lockWas !== P.lastLock || !P.popEl || (P.popEl && P.popEl.dataset.node !== P.open)) paintPop();
      requestAnimationFrame(placePop);
      return [P.el];
    }

    // Repaint what depends on edits made outside the coach (the trial.toml editor under Advanced).
    function touch() { if (P.el && P.el.isConnected && P.snap) { paintStart(); paintBar(); } }
    return { render, touch, dirty: () => Boolean(P.el && P.el.isConnected) && (dirty() || P.stale),
             shared: { envName, envFacts, modelsOf, plural, stable, FOLDER, BOUNDARY } };  // pure helpers the Live map reuses
  })();

  // ------------------------------------------------------------------ live tab
  // The map view needs the plan model and each run's workspace files; older servers get the classic cards.
  // ------------------------------------------------------------------ journey vocabulary (Live and Results)
  const TONE = { bad: "bad", warn: "warn", good: "good", info: "blue" };
  // The vocabulary's own order (references/vocabulary.md). The Go server sorts JSON keys, so the key order
  // of s.vocabulary.labels is not this order.
  const LABEL_ORDER = ["Wall", "Roadblock", "Detour", "Wait", "Dead End", "Fork", "Wrong Turn", "Misdirection", "Blind Spot",
                       "Gate", "Trust Decision", "Cost", "Delight"];
  function byVocabulary(names) {
    const at = (n) => { const i = LABEL_ORDER.indexOf(n); return i < 0 ? LABEL_ORDER.length : i; };
    return [...names].sort((a, b) => at(a) - at(b) || (a < b ? -1 : a > b ? 1 : 0));
  }
  // Why a journey is a labeled reconstruction rather than the worker's own account
  // (state.json narrate.self_narration_rejected, sent as runs[].journey.self_narration_rejected).
  const JOURNEY_WHY = {
    safety_stop: "the model's safeguards stopped the worker's own account",
    no_journey_heading: "the worker's reply did not contain the requested # Journey account",
    empty_or_short: "the worker's reply was empty or too short",
  };

  // Who wrote an attempt's journey: {kind: own | reconstruction | unavailable | other | unknown, ...}, or null
  // before there is one. A reconstruction is always said to be one, with the reason AJX recorded; an older
  // server without the reason fields still says so from the provenance text.
  function journeyProvenance(r) {
    const j = r.journey, p = String((j && j.provenance) || r.journey_provenance || "");
    if (!j && !p) return null;
    if (p === "unavailable") return { kind: "unavailable" };
    const stops = Math.max(0, Number(j && j.safety_stops) || 0);
    if (/^EDITORIAL/.test(p) || (j && j.reconstruction)) {
      const code = j && j.self_narration_rejected ? String(j.self_narration_rejected) : "";
      const why = code ? JOURNEY_WHY[code] || `its own account was not usable (${code})`
        : j && j.reconstruction ? "the worker could not be resumed to give its own account" : "the worker could not give its own account";
      return { kind: "reconstruction", code, why, stops };
    }
    if (/^self-narrated/.test(p)) return { kind: "own", detail: p.includes(";") ? p.slice(p.indexOf(";") + 1).trim() : "", stops };
    return p ? { kind: "other", text: p, stops } : { kind: "unknown", stops };
  }
  function renderLive(s) {
    return Live.supported(s) ? Live.render(s) : renderLiveClassic(s);
  }

  // The stage an attempt is running now. A stage is live only while a job runs and only if that job
  // marked it: a mark an earlier, crashed run left behind is not progress, even during a later job.
  function liveStage(s, r) {
    if (!r.current || !["running", "stopping"].includes(s.process.state)) return null;
    const at = Date.parse((r.stages.find((x) => x.name === r.current) || {}).at || ""), since = Date.parse(s.process.since || "");
    return Number.isNaN(at) || Number.isNaN(since) || at >= since - 2000 ? r.current : null;
  }

  function liveCounts(s) {
    const out = s.output;
    const running = s.runs.filter((r) => liveStage(s, r)).length;
    const ready = s.runs.filter((r) => r.complete && r.links.report).length;
    return `${running} running \u00b7 ${out.complete} of ${out.planned} planned attempt(s) finished \u00b7 ${ready} report(s) ready`;
  }

  function nothingYet(s) {
    return !s.runs.some((r) => r.started) && !ACTIVE.includes(s.process.state)
      ? h("div", { class: "card empty" }, "Nothing has run yet. Review the plan and press ", h("b", { text: "Check and start" }), ".") : null;
  }

  function recoveryBox(s, r, x) {
    return h("div", { class: "warnbox" }, x.why, " ",
      h("button", { disabled: ACTIVE.includes(s.process.state) ? true : null,
                    onclick: () => { if (x.stages !== "extract,measure,render" || confirm("This calls the reporter model again. Continue?")) act("/api/stages", { run: r.id, stages: x.stages }, `${x.label}\u2026`); } }, x.label),
      h("div", { class: "cmd", text: `runs: ajx run <trial> --cells ${r.id.replace(/-r\d+$/, "")} --stages ${x.stages}` }));
  }

  function workspaceUrl(run, path) {  // the worker chose these names: percent-encode every segment
    return "/workspace/" + [run, ...String(path).split("/")].map(encodeURIComponent).join("/");
  }

  const Live = (() => {
    const C = Coach.shared;
    const PULSE = 1600, FLASH = 2200, PREVIEW_CHARS = 200000, FEW_FILES = 12;
    const L = { s: null, preview: null, seq: 0, all: {}, bound: false, moving: false, root: null, parts: new Map(), gen: 0 };
    const REPORTING = ["normalize", "narrate", "extract", "measure", "render"];
    const DOING = {
      prepare: "preparing its environment", execute: "the worker is working", verify: "running the success checks",
      teardown: "running task cleanup", normalize: "reading the transcript", narrate: "writing the journey",
      release: "releasing the environment", extract: "extracting asks", measure: "measuring", render: "writing the report",
      archive: "archiving the evidence",
    };
    const ICON = { ok: "✓", bad: "✕", warn: "!", now: "●", none: "·" };
    const SAY = { ok: "Done: ", bad: "Problem: ", warn: "Worth a look: ", now: "Running now: ", none: "Not yet: " };
    const TONE_OF = { done: "ok", running: "now", error: "bad", interrupted: "warn" };
    const BADGE = { now: "blue", ok: "good", warn: "warn", bad: "bad", idle: "" };

    // Every pulse runs on one shared clock, so a part rebuilt on a poll never restarts it.
    const pulse = () => `animation-delay:-${Math.round(performance.now() % PULSE)}ms`;
    const cur = (r) => liveStage(L.s, r);  // a mark an earlier, crashed run left behind is not progress
    const stale = (r) => Boolean(r.current) && !cur(r);
    const shownStatus = (r, x) => (x.status === "running" && x.name !== cur(r) ? "interrupted" : x.status);
    const stageOf = (r, name) => (r.stages.find((x) => x.name === name) || {}).status || null;
    const ids = (list) => list.map((x) => x.id).join(", ");
    const bytes = (n) => (!(n >= 0) ? "" : n < 1024 ? `${n} B` : n < 1048576 ? `${(n / 1024).toFixed(1)} KB` : `${(n / 1048576).toFixed(1)} MB`);
    function hk(text) {  // a short key for data-key: worker-chosen file names never go into a selector
      let x = 5381;
      for (const ch of String(text)) x = (Math.imul(x, 33) + ch.codePointAt(0)) | 0;
      return (x >>> 0).toString(36);
    }
    const harnessOf = (s, cli) => ((s.plan_options || {}).harnesses || []).find((x) => x.name === cli) || null;
    const harnessLabel = (s, cli) => (harnessOf(s, cli) || {}).label || cli || "agent";
    const lean = (r) => { const { feed, feed_note, outputs, journey, strengths, gates, ...rest } = r; return rest; };
    const calm = (r) => ({ ...lean(r), execute: { ...r.execute, elapsed: null } });  // without the ticking clock

    function supported(s) {
      const m = s.plan_model;
      return Boolean(m && Array.isArray(m.envs) && Array.isArray(m.workers) && Array.isArray(m.checks)
        && Array.isArray(s.runs) && s.runs.every((r) => r && Array.isArray(r.stages) && Array.isArray(r.outputs)));
    }

    // Which worker card an attempt belongs to: the server says (cell, and group for a models
    // comparison). An older server does not, so fall back to the run id, <cell>-r<N>.
    function ownerOf(m, r) {
      const byId = (id) => m.workers.find((w) => String(w.id) === String(id)) || null;
      if ("cell" in r) return r.cell ? byId(r.group || r.cell) : null;
      const cell = String(r.id).replace(/-r\d+$/, "");
      return byId(cell) || m.workers.filter((w) => C.modelsOf(w).length && cell.startsWith(`${w.id}-`))
        .sort((a, b) => String(b.id).length - String(a.id).length)[0] || null;
    }

    function unconfirmed(r) {
      const env = r.environment_cleanup;
      return ["failed", "auth_errors", "skipped"].includes(r.teardown) || ["error", "interrupted"].includes(stageOf(r, "teardown"))
        || Boolean(env && env.confirmed !== true) || stageOf(r, "release") === "error"
        || (stale(r) && (stageOf(r, "teardown") !== "done" || stageOf(r, "release") !== "done"));  // AJX stopped before it could clean up
    }

    function staleText(r) {
      return L.moving ? `stopped during ${r.current} in an earlier run` : `stopped during ${r.current}; AJX is not running`;
    }

    function state(s, r) {
      if (stale(r)) return { tone: "warn", text: staleText(r) + (unconfirmed(r) ? " · cleanup not confirmed" : "") };
      const now = cur(r);
      if (now) {
        const t = now === "execute" && r.execute.elapsed !== null && r.execute.elapsed !== undefined ? ` · ${dur(r.execute.elapsed)}` : "";
        return { tone: "now", text: (s.process.state === "stopping" ? "stopping: " : "") + (DOING[now] || now) + t, live: true };
      }
      if (!r.started) {
        return { tone: "idle", text: r.planned && s.process.state === "running" && s.process.job === "run" ? "waiting its turn" : "not started" };
      }
      let out;
      const err = r.stages.find((x) => x.status === "error"), v = r.verify;
      const checks = !v || v.outcome === "not_run" ? "" : v.outcome === "succeeded" ? `verified ${v.passed}/${v.total}` : `checks ${v.passed}/${v.total}`;
      if (r.never_ran) out = { tone: "bad", text: "never ran" };
      else if (r.complete && !err) out = { tone: v && ["failed", "partial"].includes(v.outcome) ? "warn" : "ok", text: ["finished", checks].filter(Boolean).join(" · ") };
      else if (r.execute.interrupted) out = { tone: "warn", text: r.complete ? "stopped; report written" : "stopped; resume to write the report" };
      else if (err) out = { tone: "bad", text: `error in ${err.name}` };
      else out = { tone: "warn", text: "waiting to resume" };
      if (unconfirmed(r)) out = { tone: out.tone === "bad" ? "bad" : "warn", text: `${out.text} · cleanup not confirmed` };
      return out;
    }

    function track(r) {
      const now = cur(r), at = now ? r.stages.findIndex((x) => x.name === now) : -1;
      const words = at >= 0 ? `stage ${at + 1} of ${r.stages.length}: ${now}, running`
        : `${r.stages.filter((x) => x.status === "done").length} of ${r.stages.length} stages done`;
      return h("span", { class: "lv-trk", role: "img", "aria-label": words },
        r.stages.map((x) => h("i", { class: `lv-s lv-${shownStatus(r, x) || "none"}${x.name === "execute" ? " lv-x" : ""}${x.name === now ? " lv-pulse" : ""}`,
                                     style: x.name === now ? pulse() : null,
                                     title: `${x.name}: ${x.status ? `${x.status} at ${clock(x.at)}` : "not reached"}` })));
    }

    // ---------------------------------------------------------------- persistent parts
    // Each poll rebuilds only the parts whose data changed, so focus, a text selection and scroll
    // positions in the others survive (the page polls every 1.5 seconds during a run).
    function part(key, sig, tag, attrs, build) {
      let p = L.parts.get(key);
      if (!p) { p = { el: h(tag, attrs), sig: null }; L.parts.set(key, p); }
      p.gen = L.gen;
      if (p.sig !== sig) {
        p.sig = sig;
        p.el.replaceChildren(...[build()].flat(Infinity).filter((k) => k !== null && k !== undefined && k !== false));
      }
      return p.el;
    }
    function place(parent, kids) {  // the kids in order, moving only what is out of place
      kids.forEach((k, i) => { if (parent.childNodes[i] !== k) parent.insertBefore(k, parent.childNodes[i] || null); });
      while (parent.childNodes.length > kids.length) parent.lastChild.remove();
      return parent;
    }

    // ---------------------------------------------------------------- the map
    function zNode(title, sub, tag, tone, on, extra) {
      return h("div", { class: "cp-node lv-static" + (on ? " lv-on" : "") + (extra ? " " + extra : "") },
        tag ? h("span", { class: "cp-tag cp-" + tone, text: tag }) : null,
        on ? h("i", { class: "lv-dot lv-pulse lv-nd", style: pulse(), "aria-hidden": "true" }) : null,
        h("b", { text: title }), h("small", { text: sub }));
    }

    function yourSide(s, started) {
      const m = s.plan_model, t = m.task || {}, nc = m.checks.length, inside = m.checks.filter((c) => c.where === "worker").length;
      const preparing = started.filter((r) => cur(r) === "prepare");
      const checking = started.filter((r) => cur(r) === "verify");
      const verified = started.filter((r) => r.verify && r.verify.outcome !== "not_run");
      const passed = verified.filter((r) => r.verify.outcome === "succeeded").length, failed = verified.length - passed;
      const reporting = started.filter((r) => REPORTING.includes(cur(r)));
      const reporter = m.reporter || {}, rep = reporter.cli || (m.workers.find((w) => (harnessOf(s, w.cli) || {}).reporter) || { cli: "claude-code" }).cli;
      const analysisFailed = started.filter((r) => stageOf(r, "extract") === "error");
      const ready = s.runs.filter((r) => r.complete && r.links.report).length, out = s.output;
      const results = h("button", { type: "button", class: "cp-node" + (ready ? "" : " cp-dim"), "data-key": "lv-results", onclick: () => setTab("results") },
        h("span", { class: "cp-tag cp-" + (ready ? "g" : "m"), text: ready ? "open" : "later" }),
        h("b", { text: "Results" }), h("small", { text: `${out.complete} of ${out.planned} planned attempt${out.planned === 1 ? "" : "s"} finished · ${C.plural(ready, "report")} ready` }));
      return h("div", { class: "cp-zone cp-z1" },
        h("div", { class: "cp-zh" }, h("i", {}, h("em", { text: "1" })), "Your side"),
        h("div", { class: "cp-zs", text: "Your machine. AJX checks and reports after each worker stops." }),
        h("div", { class: "cp-ng" },
          zNode("The task", preparing.length ? `starting files and setup for ${ids(preparing)}`
            : `prompt · ${C.plural((t.materials || []).length, "material")} · ${t.fixture ? "starting files" : "no starting files"}`,
            preparing.length ? "sending" : "same for all", preparing.length ? "b" : "m", preparing.length > 0),
          zNode("Success checks", checking.length ? `running for ${ids(checking)}`
            : verified.length ? `${passed} of ${C.plural(verified.length, "attempt")} verified` + (inside ? ` · ${inside} inside its environment` : "")
              : nc ? `${C.plural(nc, "check")} · after the worker stops` + (inside ? ` · ${inside} inside its environment` : "") : "none: results unverified",
            checking.length ? "checking" : failed ? "failed" : verified.length ? "passed" : nc ? "after" : "none",
            checking.length ? "b" : failed ? "r" : verified.length ? "g" : nc ? "m" : "r", checking.length > 0),
          zNode("Reporter", reporting.length ? `${DOING[cur(reporting[0])]} · ${ids(reporting)}`
            : `${harnessLabel(s, rep)}${reporter.cli ? "" : " (automatic)"} · ${reporter.model || "agent default"}`,
            reporting.length ? "reporting" : analysisFailed.length ? "failed" : "after", reporting.length ? "b" : analysisFailed.length ? "r" : "m", reporting.length > 0),
          results),
        h("div", { class: "cp-chain", "aria-hidden": "true" }, h("span", { text: "worker output" }), h("span", { class: "cp-ln" }), h("span", { text: "checks" }),
          h("span", { class: "cp-ln" }), h("span", { text: "reporter" }), h("span", { class: "cp-ln" }), h("b", { text: "results" })));
    }

    function cleanupLine(e, atts) {
      const rs = atts.map((x) => x.r).filter((r) => r.started);
      const busy = rs.filter((r) => ["teardown", "release", "archive"].includes(cur(r)));
      const bad = rs.filter(unconfirmed);
      const done = rs.filter((r) => stageOf(r, "archive") === "done" && !unconfirmed(r));
      const after = e.kind === "folder" ? "after each attempt: task cleanup, then its folder moves into the evidence"
        : e.kind === "container" ? "after each attempt: task cleanup, then the container is removed"
          : "after each attempt: task cleanup, then the profile is released and its folders move into the evidence";
      const [tone, text] = busy.length ? ["now", `${DOING[cur(busy[0])]} · ${ids(busy)}`]
        : bad.length ? ["bad", `not confirmed for ${ids(bad)}: see its card`]
          : done.length ? ["ok", `done for ${C.plural(done.length, "attempt")}${done.length < rs.length ? `; ${after}` : ""}`] : ["none", after];
      return h("div", { class: "lv-envfoot lv-" + tone },
        tone === "now" ? h("i", { class: "lv-dot lv-pulse", style: pulse(), "aria-hidden": "true" }) : null,
        h("b", { text: "Cleanup: " }), text);
    }

    function envBox(s, e, atts) {
      const m = s.plan_model, k = e.kind === "container", ws = m.workers.filter((w) => w.env === e.id);
      const mine = ws.flatMap((w) => atts.get(w.id) || []);
      const inUse = mine.filter((x) => ["prepare", "execute"].includes(cur(x.r))).length;
      return h("div", { class: "cp-env" + (k ? " cp-sealed" : "") + (inUse ? " lv-busy" : "") },
        h("div", { class: "cp-eh" }, h("b", { text: C.envName(e) }),
          inUse ? h("span", { class: "lv-use", text: `${C.plural(inUse, "attempt")} here now` }) : null,
          h("span", { class: "cp-spc" }), h("span", { class: "cp-kind " + (k ? "cp-g" : "cp-a"), text: k ? "Runs in a container" : "Not a sandbox" })),
        h("div", { class: "cp-ef", text: C.envFacts(m, e) }),
        h("div", { class: "cp-harness" }, h("div", { class: "cp-hh", text: "3 · Test harness" }),
          ws.length ? ws.map((w) => workerCard(s, w, atts.get(w.id) || [])) : h("div", { class: "cp-empty", text: "No worker here." })),
        cleanupLine(e, mine));
    }

    function workerCard(s, w, atts) {
      const mods = C.modelsOf(w), states = atts.map((x) => state(s, x.r));
      const model = mods.length ? `${mods.join(" + ")} (${C.plural(mods.length, "run")})` : w.model || "agent default";
      const tag = states.some((x) => x.live) ? ["running", "b"] : atts.length && atts.every((x) => x.r.never_ran) ? ["never ran", "r"]
        : states.some((x) => x.tone === "bad") ? ["error", "r"]
        : atts.some((x) => x.r.started && unconfirmed(x.r)) ? ["unconfirmed", "a"]
          : atts.length && atts.every((x) => x.r.complete && !x.r.never_ran) ? ["finished", "g"] : atts.some((x) => x.r.started) ? ["unfinished", "a"] : ["waiting", "m"];
      return h("div", { class: "cp-node lv-static lv-wk" + (states.some((x) => x.live) ? " lv-busy" : "") },
        h("span", { class: "cp-tag cp-" + tag[1], text: tag[0], title: tag[0] === "unconfirmed" ? "Cleanup was not confirmed for an attempt" : null }),
        h("b", { text: `Worker “${w.id}”` }), h("small", { text: `${harnessLabel(s, w.cli)} · ${model}` }),
        atts.length ? h("div", { class: "lv-atts" }, atts.map((x, i) => attRow(s, x, states[i], mods.length > 1)))
          : h("small", { class: "lv-none", text: "No attempts of this worker in this output folder." }));
    }

    function attRow(s, x, stt, showModel) {
      const r = x.r;
      const kids = [h("span", { class: "lv-ar" },
        stt.live ? h("i", { class: "lv-dot lv-pulse", style: pulse(), "aria-hidden": "true" }) : null,
        h("b", { text: r.id }), showModel && x.model ? h("span", { class: "lv-mdl", text: x.model }) : null,
        h("span", { class: "lv-st lv-" + stt.tone, text: stt.text })), track(r)];
      if (!r.started) return h("div", { class: "lv-att lv-off" }, kids);
      return h("button", { type: "button", class: "lv-att", "data-key": "lv-att-" + r.id, "aria-label": `${r.id}: ${stt.text}. Show its outputs below.`,
                           onclick: () => show(r.id) }, kids);
    }

    function mapCard(s) {
      const m = s.plan_model, runs = s.runs.filter((r) => r.started || r.planned), started = runs.filter((r) => r.started);
      const atts = new Map(), other = [], placed = new Set(m.workers.filter((w) => m.envs.some((e) => e.id === w.env)).map((w) => w.id));
      for (const r of runs) {
        const w = ownerOf(m, r);
        if (!w || !placed.has(w.id)) { other.push(r); continue; }
        if (!atts.has(w.id)) atts.set(w.id, []);
        atts.get(w.id).push({ r, model: r.model || "" });
      }
      const inNow = started.some((r) => ["prepare", "execute"].includes(cur(r)));
      const outNow = started.some((r) => cur(r) && !["prepare", "execute"].includes(cur(r)));
      const cross = h("div", { class: "cp-cross" }, h("span", { class: "cp-lab", text: "BOUNDARY" }),
        h("div", { class: "cp-arrow lv-static" + (inNow ? " lv-on" : "") }, h("b", { text: "→" }), "in: prompt + starting files"),
        h("div", { class: "cp-arrow cp-out lv-static" + (outNow ? " lv-on" : "") }, h("b", { text: "←" }), "out: files + transcript → checks → reporter"));
      const z2 = h("div", { class: "cp-zone cp-z2" },
        h("div", { class: "cp-zh" }, h("i", {}, h("em", { text: "2" })), "Execution environments", h("span", { class: "cp-zx", text: " · where each attempt runs" })),
        m.envs.map((e) => envBox(s, e, atts)),
        other.length ? h("div", { class: "cp-env lv-other" },
          h("div", { class: "cp-eh" }, h("b", { text: "Not in the current plan" })),
          h("div", { class: "cp-ef", text: "Attempts in this output folder that no worker and environment in trial.toml match." }),
          h("div", { class: "cp-harness" }, h("div", { class: "cp-node lv-static lv-wk" },
            h("div", { class: "lv-atts" }, other.map((r) => attRow(s, { r, model: "" }, state(s, r), false)))))) : null);
      return h("section", { class: "cp-card lv-mapcard", "aria-label": "Where each attempt is" },
        h("div", { class: "lv-mh" }, h("h2", { text: "Where each attempt is" }),
          h("span", { class: "small muted", text: "The boxes of the Plan map. The pulse marks what each attempt is doing now; select an attempt for its outputs." })),
        s.output.blocker_kind === "changed" ? h("div", { class: "warnbox lv-mnote", text: "These attempts were recorded with other settings. The boxes show the current trial.toml." }) : null,
        h("div", { class: "cp-map" }, yourSide(s, started), cross, z2),
        h("div", { class: "cp-legend" },
          h("span", {}, h("i", { class: "lv-dot" }), " pulsing: what an attempt is doing now"),
          h("span", {}, h("i", { class: "cp-sw cp-d" }), "runs as you, not a sandbox"), h("span", {}, h("i", { class: "cp-sw cp-s" }), "runs in a container"),
          h("span", {}, h("i", { class: "cp-sw cp-x2" }), C.BOUNDARY)));
    }

    function show(id) {
      const el = document.getElementById("lv-run-" + id);
      if (!el) return;
      el.classList.remove("lv-flash");
      void el.offsetWidth;  // restart the highlight
      el.classList.add("lv-flash");
      clearTimeout(L.flashTimer);
      L.flashTimer = setTimeout(() => el.classList.remove("lv-flash"), FLASH);
      el.scrollIntoView({ block: "start", behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
      el.focus({ preventScroll: true });
    }

    // ---------------------------------------------------------------- one card per attempt: every output
    function step(tone, label, text, ...extra) {
      return h("li", { class: "lv-step lv-" + tone },
        h("span", { class: "lv-si" + (tone === "now" ? " lv-pulse" : ""), style: tone === "now" ? pulse() : null, "aria-hidden": "true", text: ICON[tone] }),
        h("div", {}, h("span", { class: "cp-sr", text: SAY[tone] }), h("b", { text: label }), " ", text, ...extra));
    }
    const stageTone = (status) => TONE_OF[status] || "none";

    function cardTop(s, r, w, e) {
      const ex = r.execute, now = cur(r), stt = state(s, r);
      const pills = r.stages.map((x) => h("span", { class: "pill " + (shownStatus(r, x) || "") + (x.name === now ? " lv-pulse" : ""), style: x.name === now ? pulse() : null,
                                                  title: x.status ? `${x.status === "running" && x.name !== now ? "marked running when AJX stopped" : x.status} at ${clock(x.at)}` : "not reached", text: x.name }));
      const timer = now === "execute" && ex.timeout_seconds
        ? [h("div", { class: "bar", role: "progressbar", "aria-label": "Time used", "aria-valuemin": "0", "aria-valuemax": String(ex.timeout_seconds), "aria-valuenow": String(ex.elapsed || 0) },
             h("i", { style: `width:${Math.min(100, 100 * (ex.elapsed || 0) / ex.timeout_seconds).toFixed(1)}%` })),
           h("div", { class: "tiny muted", text: `${dur(ex.elapsed)} of ${dur(ex.timeout_seconds)} time limit` })]
        : [];
      const where = w && e ? `worker “${w.id}” in ${C.envName(e)}` : "not in the current plan";
      return [h("div", { class: "row spread" }, h("div", { class: "row" }, h("h2", { text: r.id, style: "margin:0" }),
            h("span", { class: "badge " + BADGE[stt.tone], text: now ? `running: ${now}` : stt.text }),
            h("span", { class: "tiny muted", text: `${where} · model: ${r.model || "agent default"}` })),
          r.links.report ? link(r.links.report, "Open report") : null),
        h("div", { class: "pills" }, pills),
        timer, workerError(r),
        r.stages.filter((x) => x.status === "error" && x.error).map((x) => h("div", { class: "errbox", text: `${x.name}: ${x.error}` }))];
    }

    const feedLine = (e) => h("div", { class: e.error ? "bad" : null },
      h("span", { class: "t", text: clock(e.at) + " " }), h("span", { class: "k", text: (e.kind || "").padEnd(5) + " " }), e.text);

    // The feed is a window of the latest events. Lines already shown stay in place (with any text
    // selected in them): new events are appended, and the oldest leave the window.
    function feedList(k, r) {
      let f = L.parts.get(k + "|feedlist");
      if (!f) {
        f = { el: h("div", { class: "feed", "data-scroll": `feed-${r.id}`, "data-key": `lv-feed-${r.id}`, tabindex: "0", "aria-label": `Latest events from ${r.id}` }), items: [] };
        L.parts.set(k + "|feedlist", f);
      }
      f.gen = L.gen;
      const next = r.feed.map((e) => C.stable([e.at, e.kind, e.text, Boolean(e.error)])), old = f.items;
      let drop = 0;
      while (drop < old.length && !(old.length - drop <= next.length && old.slice(drop).every((x, i) => x === next[i]))) drop += 1;
      for (let i = 0; i < drop; i++) f.el.firstChild.remove();
      r.feed.slice(old.length - drop).forEach((e) => f.el.append(feedLine(e)));
      f.items = next;
      return f.el;
    }

    function feedSection(k, r) {
      const ex = r.execute, live = cur(r) === "execute";
      return place(part(k + "|feed", "feed", "section", { class: "lv-sec" }, () => []), [
        part(k + "|feedh", C.stable([live, r.feed.length > 0]), "div", {}, () => h("h3", {}, "Worker feed",
          live ? h("span", { class: "lv-hn lv-now" }, h("i", { class: "lv-dot lv-pulse", style: pulse(), "aria-hidden": "true" }), " live")
            : r.feed.length ? h("span", { class: "lv-hn", text: "latest events" }) : null)),
        r.feed.length ? feedList(k, r)
          : part(k + "|feedn", C.stable([r.feed_note, ex.status, live]), "div", {}, () => h("p", { class: "note", text: r.feed_note
            || (live ? "Waiting for the worker's first event…" : ex.status ? "No events were reported." : "Starts when the worker starts.") })),
        part(k + "|feedf", C.stable([r.feed.length > 0, r.links.report || null]), "div", {}, () =>
          r.feed.length && r.links.report ? h("p", { class: "tiny muted lv-foot" }, "The report lists every event: ", link(r.links.report, "open it"), ".") : null)]);
    }

    function fresh(r) {  // whether a file is new or changed: the server says; an older one leaves a guess by time
      const since = Date.parse(r.execute.started_at || "") / 1000;
      return (f) => ("starting_file" in f ? !f.starting_file : Number.isFinite(since) && Number(f.mtime) >= since - 1);
    }

    function listKids(r) {
      const files = r.outputs, isNew = fresh(r), all = Boolean(L.all[r.id]);
      const known = files.some((f) => "starting_file" in f), changed = files.filter(isNew).length;
      const shown = all ? files : files.slice(0, FEW_FILES);
      const truncated = "outputs_truncated" in r ? Boolean(r.outputs_truncated) : files.length >= 200;
      return [h("h3", {}, "Files in its workspace", files.length ? h("span", { class: "lv-hn", text: `${C.plural(files.length, "file")}${changed ? ` · ${changed} new or changed` : ""}` }) : null),
        files.length ? h("ul", { class: "lv-files", "data-scroll": `files-${r.id}` }, shown.map((f) => fileRow(r, f, isNew(f), known)))
          : h("p", { class: "note", text: r.execute.status ? "The workspace has no files." : "The workspace appears when the attempt is prepared." }),
        files.length > FEW_FILES ? h("button", { type: "button", class: "link tiny", "data-key": `lv-all-${r.id}`,
          onclick: () => { L.all[r.id] = !all; renderTab("live", true); } }, all ? "Show fewer" : `Show all ${files.length}`) : null,
        truncated ? h("p", { class: "note", text: `Showing the first ${C.plural(files.length, "file")}; the workspace has more.` }) : null];
    }

    function footKids(r) {
      if (!r.outputs.length) return [];
      const known = r.outputs.some((f) => "starting_file" in f), some = r.outputs.some((f) => f.starting_file);
      return [h("p", { class: "tiny muted lv-foot", text: known && !some ? "Files open as plain text and never run." : known
        ? "Starting file: an unchanged copy of the trial's starting files. Files open as plain text and never run."
        : "The starting files are listed too. New or changed: modified after the worker started. Files open as plain text and never run." })];
    }

    function fileRow(r, f, isNew, known) {
      const key = `${r.id}/${f.path}`, open = Boolean(L.preview && L.preview.key === key);
      if (!String(f.path).split("/").every((x) => x && x !== "." && x !== "..")) {  // never a workspace file: no link
        return h("li", {}, h("bdi", { class: "mono", text: f.path }), h("span", { class: "lv-fm", text: bytes(f.size) }));
      }
      return h("li", { class: open ? "lv-open" : null },
        h("button", { type: "button", class: "lv-fn", "data-key": "lv-f-" + hk(key), "aria-expanded": String(open), title: "Show it here as plain text",
                      onclick: () => preview(r.id, f) }, h("bdi", { text: f.path })),
        isNew ? h("span", { class: "badge blue", text: "new or changed" }) : known && f.starting_file ? h("span", { class: "badge", text: "starting file" }) : null,
        h("span", { class: "lv-fm", text: `${bytes(f.size)} · ${clock(Number(f.mtime) * 1000)}` }),
        h("a", { href: workspaceUrl(r.id, f.path), target: "_blank", rel: "noopener noreferrer", class: "tiny", "data-key": "lv-o-" + hk(key),
                 "aria-label": `Open ${f.path} in a new tab`, text: "open" }));
    }

    function viewer(r) {
      const p = L.preview;
      if (!p || p.run !== r.id) return null;
      const there = r.outputs.some((x) => x.path === p.path);
      return h("div", { class: "lv-view", role: "region", "aria-label": `Contents of ${p.path}` },
        h("div", { class: "lv-vh" }, h("b", { class: "mono" }, h("bdi", { text: p.path })), h("span", { class: "lv-grow" }),
          p.status === "ok" && p.truncated ? h("span", { class: "tiny muted", text: "beginning only" }) : null,
          h("a", { href: workspaceUrl(r.id, p.path), target: "_blank", rel: "noopener noreferrer", class: "tiny", "data-key": "lv-view-open", text: "Open in a new tab" }),
          h("button", { type: "button", class: "link tiny", "data-key": "lv-close", onclick: () => { L.preview = null; renderTab("live", true); } }, "Close")),
        p.status === "loading" ? h("p", { class: "note lv-pad", text: "Loading…" })
          : p.status === "binary" ? h("p", { class: "note lv-pad", text: "Not a text file. Open it to download a copy." })
            : p.status === "error" ? h("div", { class: "errbox lv-pad", text: p.error })
              : h("pre", { "data-scroll": "lv-view", "data-key": "lv-view-text", tabindex: "0", text: p.text || "(empty file)" }),
        there ? null : h("p", { class: "note lv-pad", text: "The workspace no longer lists this file." }));
    }

    async function preview(run, f, again) {
      const key = `${run}/${f.path}`;
      if (!again && L.preview && L.preview.key === key) { L.preview = null; renderTab("live", true); return; }
      const seq = ++L.seq, sig = `${f.size}:${f.mtime}`;
      if (!again) {
        L.preview = { key, run, dir: L.s.output.dir, path: f.path, sig, status: "loading", text: "", v: 1 };
        renderTab("live", true);
      } else {
        L.preview.sig = sig;
      }
      let next;
      try {
        const res = await fetch(workspaceUrl(run, f.path), { cache: "no-store", credentials: "same-origin", headers: { "X-AJX-Token": TOKEN } });
        if (!res.ok) throw new Error(res.status === 404 ? "The console could not open it: it is no longer in the workspace." : `The console could not open it (${res.status}).`);
        if (!/^text\/plain\b/i.test(res.headers.get("Content-Type") || "")) next = { status: "binary", text: "" };
        else {
          const text = await res.text();
          next = { status: "ok", text: text.slice(0, PREVIEW_CHARS), truncated: text.length > PREVIEW_CHARS || Number(f.size) > 2000000 };
        }
      } catch (e) {
        next = { status: "error", error: e.message || "The console could not open it.", text: "" };
      }
      if (seq !== L.seq || !L.preview || L.preview.key !== key) return;
      Object.assign(L.preview, next);
      L.preview.v += 1;
      renderTab("live", true);
    }

    function refreshPreview(s) {  // the open file changed on disk: show the new version
      const p = L.preview;
      if (!p || p.status === "loading") return;
      const r = s.runs.find((x) => x.id === p.run), f = r && r.outputs.find((x) => x.path === p.path);
      if (f && `${f.size}:${f.mtime}` !== p.sig) { p.sig = `${f.size}:${f.mtime}`; setTimeout(() => preview(p.run, f, true), 0); }
    }

    function verifySection(s, r) {
      const v = r.verify, now = cur(r) === "verify", status = stageOf(r, "verify");
      // Where each check ran: the run's own record; an older server leaves the current plan's settings.
      const recorded = v && (v.checks || []).some((c) => "location" in c);
      const inside = recorded ? v.checks.filter((c) => c.location === "environment").length : s.plan_model.checks.filter((c) => c.where === "worker").length;
      const all = recorded ? v.checks.length : s.plan_model.checks.length;
      const body = [];
      if (now) body.push(h("ul", { class: "lv-steps" }, step("now", "Running the success checks", "")));
      else if (v) {
        const cls = { succeeded: "good", partial: "warn", failed: "bad" }[v.outcome] || "";
        const label = { succeeded: "Verified", partial: "Some checks failed", failed: "Checks failed", not_run: "No checks" }[v.outcome] || v.outcome;
        body.push(h("span", { class: "badge " + cls, text: `${label} (${v.passed}/${v.total})` }),
          (v.auth_errors || []).length ? h("div", { class: "errbox", text: "A check hit credential errors: it measured AJX's access, not the worker's result." }) : null,
          h("ul", { class: "lv-steps" }, (v.checks || []).map((c) => step(c.passed ? "ok" : "bad", c.name || "check", "",
            c.detail ? h("div", { class: "tiny muted lv-detail", text: c.detail }) : null,
            recorded && inside && c.location === "environment" ? h("div", { class: "tiny muted", text: "ran inside its environment" }) : null))));
      } else if (status === "error" || status === "interrupted") {
        body.push(h("ul", { class: "lv-steps" }, step(stageTone(status), "Checks", status === "error" ? "failed to run; see the error above" : "were interrupted")));
      } else {
        body.push(h("span", { class: "muted", text: (r.execute.interrupted || stale(r)) && !now ? "Not run: the run was stopped before the checks."
          : r.execute.status && r.execute.status !== "running" ? "Waiting to run." : "Runs after the worker stops." }));
      }
      const host = all - inside, verb = now ? "Running" : v ? "Ran" : "Runs";
      const where = [host ? `${verb} on your machine` : "", inside ? `${C.plural(inside, "check")} inside its environment` : ""].filter(Boolean).join("; ");
      return h("div", { class: "mini" }, h("h3", { text: "Verification" }), body,
        where ? h("div", { class: "tiny muted lv-foot", text: `${where}, after the worker ${v || now ? "stopped" : "stops"}.` }) : null);
    }

    function failedCommands(r) {  // which cleanup commands failed, when the server says
      const bad = (r.teardown_commands || []).filter((c) => c.exit_code !== 0 || c.timed_out || c.auth_errors);
      if (!bad.length) return null;
      return h("ul", { class: "lv-cmds" }, bad.map((c) => h("li", {},
        h("code", { text: c.cmd || "(command)" }), " ",
        c.timed_out ? "timed out" : c.auth_errors && c.exit_code === 0 ? "printed credential errors" : `exit ${c.exit_code}${c.auth_errors ? ", credential errors" : ""}`,
        c.location ? ` · ran ${c.location === "environment" ? "inside the environment" : "on your machine"}` : "")));
    }

    function cleanupSection(s, r, e) {
      const td = r.teardown, tds = stageOf(r, "teardown"), rel = stageOf(r, "release"), arc = stageOf(r, "archive"), env = r.environment_cleanup;
      const n = "teardown_commands" in r && tds === "done" ? r.teardown_commands.length : ((s.plan_model.account || {}).teardown || []).length;
      const crashed = stale(r) ? `AJX stopped during ${r.current}` : "";
      const task = tds === "running" && cur(r) === "teardown" ? step("now", "Task cleanup", "running…")
        : td === "ok" ? step("ok", "Task cleanup", n ? `ran ${C.plural(n, "command")}: ok` : "nothing to clean up (no cleanup commands)")
          : td === "failed" ? step("bad", "Task cleanup", "failed: not confirmed. Resources the task created may still exist.", failedCommands(r))
            : td === "auth_errors" ? step("bad", "Task cleanup", "hit credential errors: not confirmed. Resources the task created may still exist.", failedCommands(r))
              : td === "skipped" ? step("warn", "Task cleanup", "did not run: not confirmed. Resources the task created may still exist.")
                : tds === "done" ? step("ok", "Task cleanup", "nothing to clean up")
                  : crashed ? step("warn", "Task cleanup", `did not run: ${crashed}. Not confirmed; Resume on the Plan tab runs it.`)
                    : tds === "error" || tds === "interrupted" ? step(tds === "error" ? "bad" : "warn", "Task cleanup", `${tds}: not confirmed`)
                      : step("none", "Task cleanup", "runs after verification");
      const local = e ? e.kind === "folder" : !env;  // no release record: it ran in a plain folder
      const released = { released: e && e.kind === "container" ? "container removed (confirmed)" : "released (confirmed)",
                         already_absent: "already gone (confirmed)", workers_stopped: "its processes were stopped (confirmed); kept for cleanup" };
      const unconfirmedWords = { caller_managed: "managed outside AJX: not confirmed removed", failed: "removal failed: not confirmed removed" };
      const why = env && (env.error || env.reason) ? h("div", { class: "tiny muted lv-detail", text: env.error || env.reason }) : null;
      const place = local ? step(arc === "done" ? "ok" : cur(r) === "archive" ? "now" : crashed ? "warn" : stageTone(arc),
        "Fresh folder", arc === "done" ? "moved into the run's evidence; any copied sign-in files removed" : cur(r) === "archive" ? "archiving…"
          : crashed ? `not moved into the evidence yet: ${crashed}` : "moves into the run's evidence at the end")
        : cur(r) === "release" ? step("now", "Environment", "releasing…")
          : env && env.confirmed === true ? step("ok", "Environment", released[env.status] || `${env.status || "released"} (confirmed)`)
            : env && env.status === "deferred_teardown" ? step("warn", "Environment", "kept, so task cleanup can be retried inside it: not confirmed removed", why)
              : env ? step("bad", "Environment", unconfirmedWords[env.status] || `not confirmed removed (${env.status || "unknown"})`, why)
                : rel === "error" ? step("bad", "Environment", "release failed: not confirmed removed")
                  : rel === "done" ? step("ok", "Environment", "nothing to release")
                    : crashed ? step("warn", "Environment", `not released: ${crashed}. Not confirmed removed.`)
                      : step("none", "Environment", `${e && e.kind === "container" ? "removed" : "released"} after the journey is written`);
      const archive = local ? null : step(arc === "done" ? "ok" : cur(r) === "archive" ? "now" : stageTone(arc), "Evidence", arc === "done" ? "archived; any copied sign-in files removed"
        : cur(r) === "archive" ? "archiving…" : arc === "error" ? "archiving failed" : "archived last");
      return h("div", { class: "mini" }, h("h3", { text: "Cleanup" }), h("ul", { class: "lv-steps" }, task, place, archive),
        r.recovery.filter((x) => x.stages !== "extract,measure,render").map((x) => recoveryBox(s, r, x)));
    }

    function journeySays(s, r) {
      const pv = journeyProvenance(r);
      if (!pv || pv.kind === "unknown") return null;
      if (pv.kind === "unavailable") return ["warn", "unavailable: there was no recorded experience to tell"];
      if (pv.kind === "reconstruction") {
        const stops = pv.stops ? ` (safeguards stopped it ${pv.stops === 1 ? "once" : `${pv.stops} times`})` : "";
        return ["warn", `a labeled reconstruction by the reporter from the recorded evidence, not the worker's own account: ${pv.why}${stops}`];
      }
      return ["ok", pv.kind === "own" ? "the worker's own account, given after the task" : pv.text];
    }

    function labelCounts(s, j) {
      const vocab = s.vocabulary || {}, counts = (j && j.counts) || {};
      // In the order they first happen in the journey: independent of how a server orders JSON keys.
      const names = [...new Set((j && j.steps || []).map((x) => x.label).filter((n) => n && counts[n]))];
      names.push(...byVocabulary(Object.keys(counts).filter((n) => counts[n] && !names.includes(n))));
      if (!names.length) return null;
      return h("div", { class: "lv-labels" }, names.map((n) => {
        const [icon, tone] = (vocab.labels || {})[n] || ["●", ""];
        return h("span", { class: "badge " + (TONE[tone] || ""), title: (vocab.meanings || {})[n] || n, text: `${icon} ${n} ${counts[n]}` });
      }));
    }

    function reportSection(s, r) {
      const status = (name) => stageOf(r, name), j = r.journey, says = journeySays(s, r), now = cur(r);
      const nar = status("narrate"), ext = status("extract"), ren = status("render");
      const stopped = ["done", "error", "interrupted"].includes(r.execute.status) || stale(r);  // the worker already stopped
      const transcript = step(now === "normalize" ? "now" : stageTone(status("normalize")), "Transcript", status("normalize") === "done" ? "read" : now === "normalize" ? "reading…"
        : status("normalize") || (stopped ? "not read yet" : "read after the worker stops"));
      const journey = now === "narrate" ? step("now", "Journey", "being written…")
        : says ? step(says[0] === "ok" && nar === "done" ? "ok" : says[0] === "ok" ? stageTone(nar) : "warn", "Journey", says[1],
          j && (j.steps || []).length ? h("div", { class: "tiny muted", text: `${C.plural(j.steps.length, "step")} in its chronological account` }) : null, labelCounts(s, j))
          : step(stageTone(nar), "Journey", nar || (stopped ? "not written yet" : "written after the transcript"));
      const asks = step(now === "extract" || r.asks.status === "generating" ? "now" : r.asks.status === "ready" ? "ok" : r.asks.status === "incomplete" || ext === "error" ? "bad" : stageTone(ext),
        "Asks", "", h("div", {}, asksText(r.asks, r)));
      const links = [r.links.report ? link(r.links.report, "report.html") : null, r.links.journey ? link(r.links.journey, "journey") : null, r.links.asks ? link(r.links.asks, "asks.md") : null].filter(Boolean);
      const report = step(ren === "done" && r.links.report ? "ok" : now === "render" ? "now" : stageTone(ren), "Report",
        now === "render" ? "being written…" : links.length ? (r.links.report ? "ready: " : "so far: ") : ren === "done" ? "written" : ren || (stopped ? "not written yet" : "written last"),
        links.length ? h("span", { class: "lv-links" }, links.map((a, i) => [i ? " · " : "", a])) : null);
      return h("div", { class: "mini" }, h("h3", { text: "Reporting" }), h("ul", { class: "lv-steps" }, transcript, journey, asks, report),
        r.recovery.filter((x) => x.stages === "extract,measure,render").map((x) => recoveryBox(s, r, x)),
        r.execute.interrupted && !r.complete && !now && !REPORTING.some((x) => stageOf(r, x))
          ? h("div", { class: "notebox" }, "The run was stopped before the report. ", h("b", { text: "Finish the report" }), " on the Plan tab writes it without running the task again.") : null,
        h("div", { class: "tiny muted lv-foot", text: "After the worker stops. The reporter runs on your machine." }));
    }

    function runCard(s, r) {
      const k = `${s.output.dir}|${r.id}`, w = ownerOf(s.plan_model, r), e = w ? s.plan_model.envs.find((x) => x.id === w.env) : null;
      const live = [cur(r), stale(r)];  // this attempt's own liveness, not the whole job's
      const card = part(k, "card", "div", { class: "card lv-run", id: "lv-run-" + r.id, "data-key": "lv-run-" + r.id, tabindex: "-1",
                                            role: "region", "aria-label": `Attempt ${r.id}` }, () => []);
      const top = part(k + "|top", C.stable([lean(r), live, stale(r) && L.moving, s.process.state === "stopping", w && w.id, e && e.id]), "div", {}, () => cardTop(s, r, w, e));
      const feed = feedSection(k, r);
      const p = L.preview && L.preview.run === r.id ? L.preview : null;
      const files = place(part(k + "|files", "files", "section", { class: "lv-sec" }, () => []), [
        part(k + "|list", C.stable([r.outputs, r.outputs_truncated, r.execute.started_at, r.execute.status, Boolean(L.all[r.id]), p && p.path]), "div", {}, () => listKids(r)),
        part(k + "|view", C.stable([p && [p.path, p.status, p.v], p && r.outputs.some((x) => x.path === p.path)]), "div", {}, () => viewer(r)),
        part(k + "|foot", C.stable(r.outputs.map((f) => "starting_file" in f).concat(r.outputs.length)), "div", {}, () => footKids(r))]);
      const two = place(part(k + "|two", "two", "div", { class: "lv-two" }, () => []), [feed, files]);
      const minis = part(k + "|minis", C.stable([calm(r), r.journey, live, ACTIVE.includes(s.process.state), s.plan_model.checks, (s.plan_model.account || {}).teardown, e && e.kind,
                                                (s.vocabulary || {}).labels]), "div", { class: "minis" },
        () => [verifySection(s, r), cleanupSection(s, r, e), reportSection(s, r)]);
      return place(card, [top, two, minis]);
    }

    function render(s) {
      if (!L.bound) {
        L.bound = true;
        document.addEventListener("keydown", (ev) => {
          if (ev.key === "Escape" && L.preview && !document.getElementById("tab-live").hidden) { L.preview = null; renderTab("live", true); }
        });
      }
      L.s = s;
      L.gen += 1;
      L.moving = ["running", "stopping"].includes(s.process.state);
      if (L.preview && (L.preview.dir !== s.output.dir || !s.runs.some((r) => r.id === L.preview.run))) L.preview = null;
      refreshPreview(s);
      const root = L.root || (L.root = h("div", { class: "lv-page" }));
      const head = part("head", C.stable([liveCounts(s), s.process, ui.restartOpen, ui.acceptWarnings, s.runs.map((r) => [r.current, r.started])]), "div", {},
        () => [h("h1", { text: "Live" }), h("p", { class: "sub", text: liveCounts(s) }), processCard(s), nothingYet(s)]);
      const map = part("map", C.stable([s.plan_model, (s.plan_options || {}).harnesses, s.output, s.process.state, s.process.since || null, s.process.job || null, s.runs.map(lean)]),
        "div", {}, () => mapCard(s));
      place(root, [head, map, ...s.runs.filter((r) => r.started).map((r) => runCard(s, r))]);
      for (const [key, p] of L.parts) if (p.gen !== L.gen) L.parts.delete(key);
      return [root];
    }

    return { supported, render };
  })();

  function renderLiveClassic(s) {
    const runs = s.runs.filter((r) => r.started || r.planned);
    const items = [h("h1", { text: "Live" }), h("p", { class: "sub", text: liveCounts(s) }), processCard(s), nothingYet(s)];
    return items.concat(runs.map((r) => runCard(s, r)));
  }

  function processCard(s) {
    const p = s.process;
    const active = ACTIVE.includes(p.state);
    const reporting = p.state === "running" && p.job === "run" && !s.runs.some((r) => ["prepare", "execute"].includes(r.current));
    const text = { idle: "Nothing is running.", running: "AJX is running the trial.", stopping: "Stopping. AJX stops the worker, cleans up and keeps the evidence.",
                   checking: "Checking readiness.", starting: "Checking readiness, then starting.", busy: "Rebuilding the reports." }[p.state] || p.state;
    const lines = p.log_tail || [];
    return h("div", { class: "card" + (p.state === "stopping" ? " attn" : "") },
      h("div", { class: "row spread" },
        h("div", {}, h("h2", { text: text }),
          p.command && active ? h("p", { class: "cmd", text: `${p.command} \u00b7 started ${byText(p.by)} at ${clock(p.since)} \u00b7 pid ${p.pid}` }) : null),
        (p.state === "running" || p.state === "stopping")
          ? h("div", { class: "row" },
              p.state === "running" && p.job === "run" && !(p.parallel > 1)
                ? h("button", { "data-key": "restart", onclick: () => { ui.restartOpen = !ui.restartOpen; renderTab("live", true); } }, "Restart\u2026") : null,
              h("button", { class: "danger", "data-key": "stop", disabled: p.state === "stopping" || (p.parallel > 1 && p.job === "run") ? true : null,
                            onclick: () => { if (confirm(reporting
                              ? "Stop reporting?\n\nThe worker already finished. Stopping now leaves the report unfinished; Finish the report writes it later."
                              : "Stop the run?\n\nAJX stops the worker, runs cleanup and keeps the evidence captured so far. Finish the report later from the Plan tab.")) act("/api/stop", {}, "Stop requested. Cleanup runs before AJX exits."); } },
                p.state === "stopping" ? "Stopping\u2026" : reporting ? "Stop reporting" : "Stop"))
          : null),
      ui.restartOpen && p.state === "running" ? h("div", { class: "againbox" },
        h("p", { class: "small", style: "margin:0 0 8px", text: "Restart stops this run (AJX cleans up first and keeps its evidence), then starts a new attempt of the same task." }),
        h("div", { class: "row" },
          h("button", { class: "primary", onclick: () => { ui.restartOpen = false; act("/api/restart", { mode: "same-folder", accept_warnings: ui.acceptWarnings }, "Stopping, then starting a new attempt here\u2026"); } }, "Restart here (adds an attempt)"),
          h("button", { onclick: () => { ui.restartOpen = false; act("/api/restart", { mode: "fresh-folder", accept_warnings: ui.acceptWarnings }, "Stopping, then starting in a fresh folder\u2026"); } }, "Restart in a fresh folder"),
          h("button", { class: "link", onclick: () => { ui.restartOpen = false; renderTab("live", true); } }, "Cancel"))) : null,
      p.restarting ? h("div", { class: "okbox", text: "Restarting: AJX is cleaning up this run; a new attempt starts as soon as it ends." }) : null,
      (p.state === "running" || p.state === "stopping") ? h("p", { class: "note", text: p.parallel > 1 && p.job === "run"
        ? "Stop is unavailable for parallel runs: AJX would stop only after every running attempt ends."
        : "Stop asks AJX to finish safely: it stops the worker, cleans up and keeps the evidence (an interrupt, like Ctrl-C). The page, the terminal and the chat can all close without stopping the run." }) : null,
      p.stale_lock && !active ? h("p", { class: "note", text: "A previous ajx run ended without releasing its lock. The next run replaces it." }) : null,
      lines.length ? h("details", { open: active ? true : null, "data-key": active ? "log-active" : "log-idle" }, h("summary", { text: "AJX output" }),
        h("pre", { class: "logtail", "data-scroll": "log", tabindex: "0", text: lines.join("\n") }),
        p.log ? h("a", { href: "/logs/" + encodeURIComponent(p.log), target: "_blank", rel: "noopener", class: "tiny", text: "full log" }) : null) : null);
  }

  function runStatus(r) {
    if (!r.started) return h("span", { class: "badge", text: "not started" });
    if (r.current) return h("span", { class: "badge blue", text: `running: ${r.current}` });
    const err = r.stages.find((x) => x.status === "error");
    if (r.complete && !err) return h("span", { class: "badge good", text: "finished" });
    if (r.execute.interrupted) return h("span", { class: "badge warn", text: r.complete ? "stopped; report written" : "stopped; resume to write the report" });
    if (err) return h("span", { class: "badge bad", text: `error in ${err.name}` });
    return h("span", { class: "badge warn", text: "waiting to resume" });
  }

  function verifyText(v) {
    if (!v) return [h("span", { class: "muted", text: "Runs after the worker stops." })];
    const cls = { succeeded: "good", partial: "warn", failed: "bad" }[v.outcome] || "";
    const label = { succeeded: "Verified", partial: "Some checks failed", failed: "Checks failed", not_run: "No checks" }[v.outcome] || v.outcome;
    return [h("span", { class: "badge " + cls, text: `${label} (${v.passed}/${v.total})` }),
      v.auth_errors.length ? h("div", { class: "errbox", text: "A check hit credential errors: it measured AJX's access, not the worker's result." }) : null,
      h("ul", { class: "list small" }, v.checks.map((c) => h("li", {}, c.passed ? "\u2713 " : "\u2717 ", c.name)))];
  }

  function cleanupText(r) {
    const td = r.teardown, env = r.environment_cleanup;
    const parts = [];
    if (td === "ok") parts.push(h("span", { class: "badge good", text: "task cleanup ok" }));
    else if (td === "failed") parts.push(h("span", { class: "badge bad", text: "task cleanup failed" }));
    else if (td === "auth_errors") parts.push(h("span", { class: "badge bad", text: "cleanup hit credential errors" }));
    else if (td === "skipped") parts.push(h("span", { class: "badge warn", text: "cleanup did not run" }));
    else if ((r.stages.find((x) => x.name === "teardown") || {}).status === "done") parts.push(h("span", { class: "badge good", text: "nothing to clean up" }));
    else parts.push(h("span", { class: "muted", text: "Runs after verification." }));
    if (env) parts.push(h("div", { class: "tiny", text: env.confirmed ? "Environment removed." : `Environment: ${env.status}` }));
    if (["failed", "auth_errors", "skipped"].includes(td)) parts.push(h("div", { class: "errbox", text: "Resources the task created may still exist." }));
    return parts;
  }

  function asksText(a, r) {
    if (r && r.never_ran) return [h("span", { class: "badge", text: "never ran" })];
    if (r && a.status === "pending" && !r.current && r.execute.interrupted && !r.complete) return [h("span", { class: "badge warn", text: "stopped: resume to write the report" })];
    if (a.status === "ready") return [h("span", { class: "badge good", text: `${a.count} ask(s)` }), a.count === 0 ? h("div", { class: "tiny muted", text: a.no_obstacles_observed ? "No obstacles observed." : "No asks extracted." }) : null];
    if (a.status === "generating") return [h("span", { class: "badge blue", text: "extracting asks\u2026" })];
    if (a.status === "incomplete") return [h("span", { class: "badge bad", text: "incomplete" }), h("div", { class: "tiny", text: a.reason })];
    return [h("span", { class: "badge", text: "asks pending" })];
  }

  function workerError(r) {
    const result = r.result || {};
    if (!result.error) return null;
    if (result.could_not_start) {
      return h("div", { class: "errbox" }, h("b", { text: "The worker could not start. " }), result.error,
        h("div", { class: "tiny", text: "Nothing was attempted, so the checks below say little about the product. Fix the cause (for example the model name) and use Run again." }));
    }
    return h("div", { class: "errbox" }, h("b", { text: result.timed_out ? "The worker reached the time limit. " : "The worker stopped with an error. " }), result.timed_out ? "" : result.error);
  }

  function runCard(s, r) {
    const ex = r.execute;
    const pills = r.stages.map((st) => h("span", { class: "pill " + (st.status || ""), title: st.status ? `${st.status} at ${clock(st.at)}` : "not reached", text: st.name }));
    const errors = r.stages.filter((st) => st.status === "error" && st.error);
    const timer = ex.status === "running" && ex.timeout_seconds
      ? [h("div", { class: "bar", role: "progressbar", "aria-label": "Time used", "aria-valuemin": "0", "aria-valuemax": String(ex.timeout_seconds), "aria-valuenow": String(ex.elapsed || 0) },
           h("i", { style: `width:${Math.min(100, 100 * (ex.elapsed || 0) / ex.timeout_seconds).toFixed(1)}%` })),
         h("div", { class: "tiny muted", text: `${dur(ex.elapsed)} of ${dur(ex.timeout_seconds)} time limit` })]
      : [];
    const feed = r.feed.length
      ? h("div", { class: "feed", "data-scroll": `feed-${r.id}` }, r.feed.map((e) => h("div", { class: e.error ? "bad" : null },
          h("span", { class: "t", text: clock(e.at) + " " }), h("span", { class: "k", text: (e.kind || "").padEnd(5) + " " }), e.text)))
      : r.feed_note ? h("p", { class: "note", text: r.feed_note })
        : ex.status === "running" ? h("p", { class: "note", text: "Waiting for the worker's first event\u2026" }) : null;
    const recovery = r.recovery.map((x) => recoveryBox(s, r, x));
    return h("div", { class: "card" },
      h("div", { class: "row spread" }, h("div", { class: "row" }, h("h2", { text: r.id, style: "margin:0" }), runStatus(r),
          h("span", { class: "tiny muted", text: `model: ${r.model || "agent default"}` })),
        r.links.report ? link(r.links.report, "Open report") : null),
      h("div", { class: "pills" }, pills),
      timer, workerError(r), feed,
      errors.map((st) => h("div", { class: "errbox", text: `${st.name}: ${st.error}` })),
      r.started ? h("div", { class: "minis" },
        h("div", { class: "mini" }, h("h3", { text: "Verification" }), verifyText(r.verify)),
        h("div", { class: "mini" }, h("h3", { text: "Cleanup" }), cleanupText(r)),
        h("div", { class: "mini" }, h("h3", { text: "Reporting" }), asksText(r.asks, r))) : null,
      recovery);
  }

  // ------------------------------------------------------------------ results tab
  // A server that sends each attempt's journey with its provenance (steps, reconstruction,
  // self_narration_rejected) gets the outcome-first view with the journey timeline. An older one
  // keeps the classic cards.
  function renderResults(s) {
    return Results.supported(s) ? Results.render(s) : renderResultsClassic(s);
  }

  function renderResultsClassic(s) {
    const runs = s.runs.filter((r) => r.started);
    const items = [h("h1", { text: "Results" }),
      h("p", { class: "sub", text: "Start with the verified outcome, then the asks and the evidence behind them. Asks come from one recorded attempt; they are not a product score." })];
    if (!runs.length) return items.concat(h("div", { class: "card empty", text: "Results appear here as each attempt finishes reporting." }));
    for (const r of runs) items.push(resultCard(s, r));
    return items.concat(resultsFooter(s));
  }

  function resultsFooter(s) {
    return [h("div", { class: "card" }, legend(s)),
      h("div", { class: "card" },
        h("div", { class: "row" },
          s.output.index ? link(s.output.index, "Open the matrix overview (index.html)") : h("span", { class: "muted", text: "The matrix overview appears after the first report." }),
          h("button", { disabled: ACTIVE.includes(s.process.state) ? true : null, onclick: () => act("/api/report", {}, "Rebuilding the reports (no model calls)…") }, "Rebuild reports")),
        h("p", { class: "cmd", text: "Rebuild runs: ajx report <trial> --no-synthesis" }))];
  }

  function labelBadge(s, name) {
    const vocab = s.vocabulary || {};
    const [icon, tone] = (vocab.labels || {})[name] || ["●", ""];
    return h("span", { class: "badge " + (TONE[tone] || ""), title: (vocab.meanings || {})[name] || name, text: `${icon} ${name}` });
  }

  function statusBadge(s, key) {
    const [label, tone] = ((s.vocabulary || {}).statuses || {})[key] || [key, ""];
    return key ? h("span", { class: "badge " + (TONE[tone] || ""), text: label }) : null;
  }

  function legend(s) {
    const vocab = s.vocabulary || {};
    return h("details", { "data-key": "legend", class: "legend" }, h("summary", { text: "What the icons mean" }),
      h("ul", { class: "list" }, byVocabulary(Object.keys(vocab.labels || {})).map((name) => h("li", {}, labelBadge(s, name), " ",
        h("span", { class: "small muted", text: (vocab.meanings || {})[name] || "" })))));
  }

  // How evidence shows under an ask, a strength or a gate: the classic view keeps the ids as text.
  const PLAIN_EVIDENCE = { refs: (refs, fallback) => "Evidence: " + ((refs || []).join(", ") || fallback), text: (text) => text };

  // Asks, then what helped (Delight) and human gates: reporter-model judgments, each linked to its evidence.
  function asksBody(s, r, ev) {
    const a = r.asks, body = [];
    if (a.status !== "ready") return [h("div", { class: "row" }, asksText(a, r))];
    if (!a.count) body.push(h("p", { class: "muted", text: a.no_obstacles_observed ? "No obstacles observed in this attempt." : "No asks were extracted. Check the verification and the journey before reading this as an easy experience." }));
    for (const ask of a.top) {
      body.push(h("div", { class: "ask" },
        h("div", {},
          h("div", { class: "row" }, h("span", { class: "badge dark", text: ask.id || "" }), h("b", { text: ask.title }),
            (ask.labels || []).map((l) => labelBadge(s, l)), statusBadge(s, ask.evidence_status)),
          h("p", { text: ask.requested_behavior }),
          ask.consequence ? h("p", { class: "small", text: ask.consequence }) : null,
          h("div", { class: "refs" }, ev.refs(ask.event_refs, "journey anchors"))),
        h("button", { "aria-label": `Ask my agent about ${ask.id}`, onclick: () => askAgent(`About ${ask.id} “${ask.title}” in ${r.id}: `) }, "Ask my agent")));
    }
    if (a.count > a.top.length) body.push(h("p", { class: "note" }, `${a.count - a.top.length} more in `, r.links.report ? link(r.links.report, "the report") : "the report", "."));
    for (const good of r.strengths || []) {
      body.push(h("div", { class: "ask" }, h("div", {},
        h("div", { class: "row" }, labelBadge(s, "Delight"), h("b", { text: good.title })),
        h("p", {}, ev.text(good.how_observed, good.event_refs)),
        h("div", { class: "refs" }, ev.refs(good.event_refs, "journey")))));
    }
    for (const gate of r.gates || []) {
      body.push(h("div", { class: "ask" }, h("div", {},
        h("div", { class: "row" }, labelBadge(s, "Gate"), h("b", { text: gate.trigger }),
          h("span", { class: "badge " + (gate.resolved ? "good" : "warn"), text: gate.resolved ? "resolved" : "unresolved" })),
        h("p", {}, ev.text(gate.agent_action, gate.event_refs)),
        h("div", { class: "refs" }, ev.refs(gate.event_refs, "journey")))));
    }
    return body;
  }

  function exportRow(s, r) {
    const exp = ui.exports[r.id];
    return [h("div", { class: "hr" }), h("div", { class: "row" },
      h("button", { "data-key": "export-" + r.id, disabled: !r.complete || ACTIVE.includes(s.process.state) ? true : null,
                    onclick: async () => { const res = await act("/api/export", { run: r.id }, (x) => `Exported to ${x.dest}`); if (res) { ui.exports[r.id] = res; renderTab("results", true); } } }, "Export for a team"),
      h("span", { class: "tiny muted", text: "report.html, journey.html and asks.md with your local paths relabeled. Review them before sharing." })),
      exp ? h("div", { class: "okbox" }, `Exported to ${exp.dest}: `, exp.files.map((f, i) => [i ? ", " : "", link(f, f.split("/").pop())])) : null];
  }

  function resultCard(s, r) {
    const a = r.asks, v = r.verify;
    const head = h("div", { class: "row spread" },
      h("div", { class: "row" }, h("h2", { text: r.id, style: "margin:0" }), v ? verifyText(v)[0] : h("span", { class: "badge", text: "no verification" }),
        r.review_status ? h("span", { class: "badge " + (r.review_status === "ready" ? "good" : "warn"), text: r.review_status === "ready" ? "report ready" : "report incomplete" }) : h("span", { class: "badge", text: "report pending" })),
      h("div", { class: "row" },
        r.links.report ? link(r.links.report, "Report") : null,
        r.links.journey ? link(r.links.journey, "Journey") : null,
        r.links.asks ? link(r.links.asks, "asks.md") : null));
    const body = [workerError(r)];
    if (r.declared && !(r.result || {}).could_not_start) body.push(h("p", { class: "small" }, h("b", { text: "The worker said: " }), r.declared));
    if (r.journey_provenance && r.journey_provenance.startsWith("EDITORIAL")) body.push(h("p", { class: "note", text: "The journey is a labeled reconstruction, not the worker's own account." }));
    body.push(...asksBody(s, r, PLAIN_EVIDENCE));
    body.push(...exportRow(s, r));
    return h("div", { class: "card" }, head, ...body);
  }

  const Results = (() => {
    const EVENT = /^E-\d{3,}$/;
    const SENT_STEPS = 30;  // the server sends the first 30 steps of a journey; its counts cover every step
    // How many steps the journey has in all (runs[].journey.total_steps); null from a server that does not say.
    const totalSteps = (j) => (j && Number.isInteger(j.total_steps) && j.total_steps >= 0 ? j.total_steps : null);
    const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;
    const FLASH = 2200;
    let flashTimer = null;

    function supported(s) {
      const runs = (s.runs || []).filter((r) => r && r.started);
      return runs.length > 0 && runs.every((r) => "journey" in r && (r.journey === null
        || (typeof r.journey === "object" && Array.isArray(r.journey.steps)
            && "reconstruction" in r.journey && "self_narration_rejected" in r.journey)));
    }

    // Which worker and model an attempt belongs to: the server says (cell, and group for a models
    // comparison); without it, the run id <cell>-r<N>.
    const cellOf = (r) => (r.cell ? String(r.cell) : String(r.id).replace(/-r\d+$/, ""));
    const workerOf = (r) => (r.group ? String(r.group) : cellOf(r));
    const outsidePlan = (r) => "cell" in r && !r.cell;
    const attemptNo = (r) => (Number.isInteger(r.attempt) ? r.attempt : Number((/-r(\d+)$/.exec(String(r.id)) || [0, 0])[1]));
    const modelOf = (r) => r.model || "agent default";
    function ordered(runs) {  // attempts of one worker, then of one model, side by side; the server's order otherwise
      const first = new Map(), rank = (k) => { if (!first.has(k)) first.set(k, first.size); return first.get(k); };
      runs.forEach((r) => { rank("w|" + workerOf(r)); rank("c|" + cellOf(r)); });
      return runs.map((r, i) => ({ r, i })).sort((a, b) => rank("w|" + workerOf(a.r)) - rank("w|" + workerOf(b.r))
        || rank("c|" + cellOf(a.r)) - rank("c|" + cellOf(b.r)) || attemptNo(a.r) - attemptNo(b.r) || a.i - b.i).map((x) => x.r);
    }

    // ---------------------------------------------------------------- labels
    function labelInfo(s, name) {
      const v = s.vocabulary || {}, [icon, tone] = (v.labels || {})[name] || ["●", ""];
      return { icon, tone: tone || "", meaning: (v.meanings || {})[name] || "" };
    }
    function countsOf(j) {  // label -> count: the server's counts, or the steps when it sent none
      const out = {}, c = j && j.counts;
      if (c && typeof c === "object" && !Array.isArray(c)) {
        for (const [k, n] of Object.entries(c)) if (Number(n) > 0) out[k] = Number(n);
      } else {
        for (const x of (j && j.steps) || []) if (x && x.label) out[x.label] = (out[x.label] || 0) + 1;
      }
      return out;
    }
    // In the order the labels first happen in the journeys, never the order of a counts object (the
    // Go server sorts JSON keys). Labels counted past the steps sent come last, in vocabulary order.
    function labelsInOrder(journeys) {
      const names = [], rest = new Set();
      for (const j of journeys) for (const x of (j && j.steps) || []) if (x && x.label && !names.includes(x.label)) names.push(String(x.label));
      for (const j of journeys) for (const n of Object.keys(countsOf(j))) if (!names.includes(n)) rest.add(n);
      return names.concat(byVocabulary([...rest]));
    }

    // ---------------------------------------------------------------- provenance
    const provenance = journeyProvenance;
    const timesText = (n) => (n === 1 ? "once" : `${n} times`);

    function provBox(r, pv) {
      if (pv.kind === "own") {
        return h("div", { class: "rs-prov rs-own" }, h("b", { text: "The worker's own account. " }),
          `AJX resumed the worker after the task and it described what it did${pv.detail ? ` (${pv.detail})` : ""}. The steps and labels are its own.`,
          pv.stops ? ` The model's safeguards stopped it ${timesText(pv.stops)} while it answered.` : "");
      }
      if (pv.kind === "reconstruction") {
        const why = pv.code === "safety_stop" && pv.stops ? `${pv.why} (${timesText(pv.stops)})` : pv.why;
        return h("div", { class: "rs-prov rs-recon", role: "note" }, h("b", { text: "A labeled reconstruction, not the worker's recollection. " }),
          `The reporter wrote it from the recorded evidence because ${why}.`,
          pv.code !== "safety_stop" && pv.stops ? ` The model's safeguards stopped the worker ${timesText(pv.stops)} while it answered.` : "",
          " The steps and labels are the reporter's.");
      }
      if (pv.kind === "unavailable") {
        return h("div", { class: "rs-prov rs-none" }, h("b", { text: "No journey. " }),
          `AJX could not write an account of this attempt${r.links.journey_md ? "; journey.md says why" : ""}.`);
      }
      if (pv.kind === "unknown") {
        return h("div", { class: "rs-prov rs-none" }, h("b", { text: "Who wrote this journey was not recorded, " }), "so it is not shown as the worker's own account.");
      }
      return h("div", { class: "rs-prov rs-none" }, h("b", { text: "Provenance: " }), pv.text);
    }

    function journeyWords(s, r) {  // the journey column of the comparison, and the card when there is none yet
      const pv = provenance(r), nar = (r.stages.find((x) => x.name === "narrate") || {}).status;
      if (pv) return { own: ["", "own account"], reconstruction: ["warn", "reconstruction"], unavailable: ["bad", "none"],
                       other: ["", "see its card"], unknown: ["warn", "not recorded"] }[pv.kind];
      if (r.never_ran) return ["", "never ran"];
      if (liveStage(s, r) === "narrate") return ["blue", "being written"];
      if (nar === "error") return ["bad", "failed"];
      return ["", "not yet"];
    }

    function pendingJourney(s, r) {
      const nar = (r.stages.find((x) => x.name === "narrate") || {}).status, now = liveStage(s, r);
      if (r.never_ran) return "No journey: the attempt never ran.";
      if (now === "narrate") return "The journey is being written…";
      if (nar === "error") return "Writing the journey failed. The Live tab shows the error and how to retry.";
      if (r.execute.interrupted && !r.complete && !now) return "Not written: the run was stopped first. Finish the report on the Plan tab to write it.";
      if (!r.execute.status || r.execute.status === "running") return "The journey is written after the worker stops.";
      // A stage error (verify, say) does not stop the attempt: while the job runs, the journey still follows.
      if (["running", "stopping"].includes(s.process.state)) return "The journey is written after the transcript is read.";
      const err = r.stages.find((x) => x.status === "error");
      return err ? `Not written: the run ended after an error in ${err.name}. The Live tab shows the error.`
        : "Not written yet, and nothing is running. Resume on the Plan tab to write it.";
    }

    // ---------------------------------------------------------------- evidence links
    // E-### ids open the run's pretty-journey.html at that event. Only ids the step itself cites, and
    // only in the E-### form, become links; anything else stays text.
    function refNode(r, ref) {
      const id = String(ref);
      return r.links.journey && EVENT.test(id)
        ? h("a", { href: fileUrl(r.links.journey) + "#" + encodeURIComponent(id), target: "_blank", rel: "noopener", class: "rs-ref",
                   title: `Open ${id} in the journey`, text: id })
        : h("span", { class: "rs-ref", text: id });
    }
    function linkedText(r, text, refs) {
      const cited = new Set((refs || []).map(String).filter((x) => EVENT.test(x))), used = new Set();
      const kids = String(text || "").split(/\b(E-\d{3,})\b/).map((part, i) => {
        if (i % 2 && cited.has(part)) { used.add(part); return refNode(r, part); }
        return part;
      });
      return { kids, rest: [...cited].filter((x) => !used.has(x)) };
    }
    const linkedEvidence = (r) => ({
      refs: (refs, fallback) => {
        const list = (refs || []).map(String);
        return ["Evidence: ", list.length ? list.map((x, i) => [i ? ", " : "", refNode(r, x)]) : fallback];
      },
      text: (text, refs) => linkedText(r, text, refs).kids,
    });

    // ---------------------------------------------------------------- the verified outcome
    function outcome(s, r) {  // [tone, short, sentence]
      const v = r.verify, res = r.result || {}, ver = (r.stages.find((x) => x.name === "verify") || {}).status;
      if (r.never_ran) return ["bad", "never ran", "Never ran: its preparation failed or was stopped, so the worker did not start."];
      if (v) {
        const n = Number(v.total) || 0, p = Number(v.passed) || 0, auth = (v.auth_errors || []).length;
        // As the report says: these results do not show what the worker achieved, pass or fail.
        if (res.could_not_start) return ["warn", `could not start · ${p}/${n}`, "Not verified: the worker could not start, so the checks say little about the product."];
        if (auth) return ["warn", `credential errors · ${p}/${n}`, `Not verified: ${auth === 1 ? "a check" : `${auth} checks`} hit credential errors, so ${p} of ${n} passing reflects AJX's access, not only the worker's work. Inspect the checks before comparing outcomes.`];
        if (v.outcome === "succeeded") return ["good", `verified ${p}/${n}`, n === 1 ? "Verified: the success check passed." : `Verified: all ${n} success checks passed.`];
        if (v.outcome === "partial") return ["warn", `${p}/${n} checks passed`, `Not verified: ${p} of ${n} success checks passed.`];
        if (v.outcome === "failed") return ["bad", `checks failed 0/${n}`, n === 1 ? "Not verified: the success check failed." : `Not verified: none of the ${n} success checks passed.`];
        if (v.outcome === "not_run") return ["warn", "no checks", "Not verified: the trial has no success checks, so nothing confirmed the result."];
        return ["", String(v.outcome || "unknown"), `Verification: ${v.outcome || "unknown"} (${p}/${n}).`];
      }
      if (res.could_not_start) return ["bad", "could not start", "Not verified: the worker could not start."];
      if (liveStage(s, r) === "verify") return ["blue", "checking", "Running the success checks…"];
      if (ver === "error") return ["bad", "checks failed to run", "Not verified: the success checks failed to run."];
      if (!r.execute.status || r.execute.status === "running") return liveStage(s, r) ? ["blue", "running", "Verification runs after the worker stops."] : ["", "pending", "Verification runs after the worker stops."];
      return ["warn", "not verified", r.complete || r.execute.interrupted ? "Not verified: no verification was recorded." : "Verification has not run yet."];
    }

    function outcomeSection(s, r) {
      const [tone, , sentence] = outcome(s, r), v = r.verify, checks = (v && v.checks) || [];
      const recorded = checks.some((c) => c && "location" in c), inside = checks.filter((c) => c && c.location === "environment").length;
      const where = !v || !checks.length ? "" : !recorded ? "The checks ran after the worker stopped."
        : inside === checks.length ? "The checks ran inside its environment, after the worker stopped."
          : inside ? `${plural(inside, "check")} ran inside its environment, the rest on your machine, after the worker stopped.` : "The checks ran on your machine after the worker stopped.";
      const icon = { good: "✓", bad: "✕", warn: "!", blue: "●" }[tone] || "·";
      return h("section", { class: "rs-outcome rs-" + (tone || "none"), "aria-label": "Verified outcome" },
        h("div", { class: "rs-ol" }, h("span", { class: "rs-oi", "aria-hidden": "true", text: icon }),
          h("div", {}, h("div", { class: "rs-big", text: sentence }), where ? h("div", { class: "tiny rs-where", text: where }) : null)),
        checks.length ? h("ul", { class: "rs-checks" }, checks.map((c) => h("li", { class: c.passed ? "rs-pass" : "rs-fail" },
          h("span", { class: "rs-ck", "aria-hidden": "true", text: c.passed ? "✓" : "✕" }), h("span", { class: "rs-sr", text: c.passed ? "Passed: " : "Failed: " }),
          h("span", { text: c.name || "check" }),
          !c.passed && c.detail ? h("span", { class: "tiny rs-cd", text: c.detail }) : null,
          v && (v.auth_errors || []).map(String).includes(String(c.name)) ? h("span", { class: "tiny rs-cd rs-cred", text: "hit credential errors" }) : null,
          recorded && inside && inside < checks.length && c.location === "environment" ? h("span", { class: "tiny rs-cd", text: "ran inside its environment" }) : null))) : null,
        null);
    }

    // ---------------------------------------------------------------- the journey
    function stepItem(s, r, x) {
      const lab = x && x.label ? String(x.label) : "", info = lab ? labelInfo(s, lab) : null;
      const { kids, rest } = linkedText(r, x && x.text, x && x.refs);
      return h("li", { class: "rs-step " + (lab ? "rs-t-" + (info.tone || "none") : "rs-plain") },
        h("span", { class: "rs-ico", "aria-hidden": "true", title: lab ? (info.meaning ? `${lab}: ${info.meaning}` : lab) : null, text: lab ? info.icon : "" }),
        h("div", { class: "rs-sb" },
          lab ? h("b", { class: "rs-lab", title: info.meaning || null, text: lab }) : null, lab ? " " : null, kids,
          rest.length ? h("span", { class: "rs-also" }, " ", rest.map((ref, i) => [i ? " " : "", refNode(r, ref)])) : null));
    }

    function countChips(s, j) {
      const counts = countsOf(j), names = labelsInOrder([j]).filter((n) => counts[n] > 0);
      if (!names.length) return null;
      return h("ul", { class: "rs-counts", "aria-label": "How often each label occurs in this journey" }, names.map((n) => {
        const info = labelInfo(s, n);
        return h("li", { class: "badge " + (TONE[info.tone] || ""), title: info.meaning || n }, `${info.icon} ${n} `, h("b", { text: String(counts[n]) }));
      }));
    }

    function journeySection(s, r) {
      const j = r.journey, pv = provenance(r), steps = j && Array.isArray(j.steps) ? j.steps : [], total = totalSteps(j);
      const labeled = steps.filter((x) => x && x.label).length, cut = total === null ? steps.length >= SENT_STEPS : total > steps.length;
      const head = h("h3", {}, "Journey", steps.length ? h("span", { class: "rs-hn",
        text: `${!cut ? plural(steps.length, "step") : total === null ? `the first ${plural(steps.length, "step")}` : `${steps.length} of ${total} steps`}${labeled ? `, ${labeled} labeled` : ""}` }) : null);
      if (!pv) return h("section", { class: "rs-sec", "aria-label": "Journey" }, head, h("p", { class: "note", text: pendingJourney(s, r) }));
      const recon = pv.kind === "reconstruction";
      const say = { own: "In the worker's words, in order", reconstruction: "Reconstructed from the recorded evidence, in order" }[pv.kind] || "In order";
      const links = [r.links.journey ? link(r.links.journey, "Open the journey") : null, r.links.journey_md ? link(r.links.journey_md, "journey.md") : null].filter(Boolean);
      return h("section", { class: "rs-sec", "aria-label": "Journey" }, head, provBox(r, pv),
        j ? countChips(s, j) : null,
        steps.length ? [h("p", { class: "rs-tlh", text: say + (r.links.journey ? ". Select an event id to see it in the journey." : ".") }),
          h("ol", { class: "rs-tl" + (recon ? " rs-recon" : "") }, steps.map((x) => stepItem(s, r, x)))]
          : j && pv.kind !== "unavailable" ? h("p", { class: "note", text: "The journey has no numbered chronological account to show here; open it to read it." }) : null,
        cut ? h("p", { class: "note", text: `Showing the first ${total === null ? plural(steps.length, "step") : `${steps.length} of ${total} steps`}; the counts cover every step. Open the journey for the rest.` }) : null,
        links.length ? h("p", { class: "tiny rs-jl" }, links.map((a, i) => [i ? " · " : "", a])) : null);
    }

    // ---------------------------------------------------------------- one card per attempt
    function reviewBadge(r) {
      return r.review_status ? h("span", { class: "badge " + (r.review_status === "ready" ? "good" : "warn"), text: r.review_status === "ready" ? "report ready" : "report incomplete" })
        : h("span", { class: "badge", text: "report pending" });
    }

    function card(s, r) {
      const a = r.asks, now = liveStage(s, r);
      const who = outsidePlan(r) ? `not in the current plan · model: ${modelOf(r)}` : `worker “${workerOf(r)}” · model: ${modelOf(r)}`;
      const tally = a.status === "ready" ? [plural(a.count, "ask"), a.strengths ? plural(a.strengths, "strength") : "", a.gates ? plural(a.gates, "human gate") : ""].filter(Boolean).join(" · ") : "";
      const asksHead = h("h3", {}, "Asks", tally ? h("span", { class: "rs-hn", text: tally }) : null);
      return h("div", { class: "card rs-run", id: "rs-run-" + r.id, tabindex: "-1", role: "region", "aria-label": `Attempt ${r.id}` },
        h("div", { class: "row spread" },
          h("div", { class: "row" }, h("h2", { class: "rs-h", text: r.id }), h("span", { class: "tiny muted", text: who }), reviewBadge(r),
            now ? h("span", { class: "badge blue", text: `running: ${now}` }) : null),
          h("div", { class: "row" },
            r.links.report ? link(r.links.report, "Report") : null,
            r.links.journey ? link(r.links.journey, "Journey") : null,
            r.links.asks ? link(r.links.asks, "asks.md") : null)),
        workerError(r),
        outcomeSection(s, r),
        r.declared && !(r.result || {}).could_not_start
          ? h("p", { class: "small rs-said" }, h("b", { text: "The worker said: " }), r.declared, h("span", { class: "tiny muted", text: " (its own claim; the checks are what AJX verified)" })) : null,
        journeySection(s, r),
        h("section", { class: "rs-sec", "aria-label": "Asks" }, asksHead,
          a.status === "ready" ? h("p", { class: "tiny muted rs-asknote", text: "Each ask is the reporter model's judgment of this attempt, linked to its evidence. Asks are not a score." }) : null,
          asksBody(s, r, linkedEvidence(r))),
        exportRow(s, r));
    }

    // ---------------------------------------------------------------- the comparison: one row per attempt
    function show(id) {
      const el = document.getElementById("rs-run-" + id);
      if (!el) return;
      el.classList.remove("rs-flash");
      void el.offsetWidth;  // restart the highlight
      el.classList.add("rs-flash");
      clearTimeout(flashTimer);
      flashTimer = setTimeout(() => el.classList.remove("rs-flash"), FLASH);
      el.scrollIntoView({ block: "start", behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
      el.focus({ preventScroll: true });
    }

    function asksCell(r) {
      const a = r.asks;
      if (r.never_ran) return h("span", { class: "muted", text: "—" });
      if (a.status === "ready") return h("b", { text: String(a.count) });
      if (a.status === "generating") return h("span", { class: "muted", text: "extracting…" });
      if (a.status === "incomplete") return h("span", { class: "badge bad", text: "incomplete", title: a.reason || null });
      return h("span", { class: "muted", text: "pending" });
    }

    function compareCard(s, runs) {
      const labels = labelsInOrder(runs.map((r) => r.journey));
      const verified = runs.filter((r) => outcome(s, r)[0] === "good").length;  // never one with credential errors or that could not start
      const recon = runs.filter((r) => (provenance(r) || {}).kind === "reconstruction").length;
      const head = h("tr", {},
        h("th", { scope: "col", text: "Attempt" }), h("th", { scope: "col", text: "Worker" }), h("th", { scope: "col", text: "Model" }),
        h("th", { scope: "col", text: "Verified outcome" }), h("th", { scope: "col", text: "Journey" }),
        labels.map((n) => { const info = labelInfo(s, n);
          return h("th", { scope: "col", class: "rs-lc", title: info.meaning ? `${n}: ${info.meaning}` : n },
            h("span", { class: "rs-li", "aria-hidden": "true", text: info.icon }), h("small", { text: n })); }),
        h("th", { scope: "col", class: "rs-num", text: "Asks" }), h("th", { scope: "col" }, h("span", { class: "rs-sr", text: "Report" })));
      let prev = null;
      const rows = runs.map((r) => {
        const [tone, short] = outcome(s, r), [jt, jw] = journeyWords(s, r), j = r.journey, counts = countsOf(j);
        const read = j && Array.isArray(j.steps) ? j.steps.length : 0, steps = totalSteps(j) === null ? read : Math.max(read, totalSteps(j)), pv = provenance(r);
        const group = prev !== null && cellOf(r) !== prev;  // a new worker, or a new model of one
        prev = cellOf(r);
        return h("tr", { class: group ? "rs-grp" : null },
          h("th", { scope: "row" }, h("button", { type: "button", class: "rs-go", "data-key": "rs-go-" + r.id, title: "Show this attempt's results", onclick: () => show(r.id) }, r.id)),
          h("td", { text: outsidePlan(r) ? "not in the plan" : workerOf(r) }),
          h("td", { class: r.model ? null : "muted", text: modelOf(r) }),
          h("td", {}, h("span", { class: "badge " + (TONE[tone] || tone || ""), text: short })),
          h("td", {}, jt ? h("span", { class: "badge " + (TONE[jt] || jt), text: jw, title: pv && pv.kind === "reconstruction" ? `Not the worker's recollection: ${pv.why}` : null }) : h("span", { text: jw }),
            steps ? h("span", { class: "tiny muted", text: ` · ${plural(steps, "step")}` })
              : j && pv && pv.kind !== "unavailable" ? h("span", { class: "tiny muted", text: " · no steps read" }) : null),
          // Counts mean something only for a journey whose steps were read; otherwise the cell is unknown.
          labels.map((n) => h("td", { class: "rs-lc" + (counts[n] ? "" : " rs-zero") }, read
            ? (counts[n] ? String(counts[n]) : [h("span", { "aria-hidden": "true", text: "·" }), h("span", { class: "rs-sr", text: "none" })])
            : h("span", { class: "rs-sr", text: "unknown" }))),
          h("td", { class: "rs-num" }, asksCell(r)),
          h("td", {}, r.links.report ? link(r.links.report, "report") : null));
      });
      return h("section", { class: "card rs-cmp", "aria-label": "Compare the attempts" },
        h("div", { class: "row spread" }, h("h2", { class: "rs-h", text: "Compare the attempts" }),
          s.output.index ? link(s.output.index, "Open the matrix overview (index.html)") : null),
        h("p", { class: "small muted rs-sum", text: `${plural(runs.length, "attempt")} · ${verified} verified${recon ? ` · ${recon} with a reconstructed journey` : ""}` }),
        h("div", { class: "rs-scroll" }, h("table", { class: "rs-tab" }, h("thead", {}, head), h("tbody", {}, rows))),
        h("p", { class: "note", text: "One row per attempt, grouped by worker and model. The verified outcome comes from the success checks. "
          + "Label counts come from each journey and asks from the reporter model's reading of it: evidence-linked observations, not scores. "
          + "A low count is not proof that an attempt went smoothly, and a reconstruction's labels are the reporter's, not the worker's." }));
    }

    function render(s) {
      const runs = ordered(s.runs.filter((r) => r.started));
      const kids = [h("h1", { text: "Results" }),
        h("p", { class: "sub", text: "Each attempt starts with its verified outcome, then the journey in order, then the asks and the evidence behind them. Asks are a reporter model's judgments of one attempt, not a product score." })];
      if (runs.length > 1) kids.push(compareCard(s, runs));
      kids.push(...runs.map((r) => card(s, r)), ...resultsFooter(s));
      return [h("div", { class: "rs-page" }, kids)];
    }

    return { supported, render };
  })();

  // ------------------------------------------------------------------ activity tab
  function askAgent(prefix) {
    ui.composer = !ui.composer.trim() ? prefix : ui.composer.includes(prefix) ? ui.composer : ui.composer.trimEnd() + "\n\n" + prefix;
    setTab("activity");
    const ta = document.querySelector('[data-key="composer"]');
    if (ta) {
      ta.value = ui.composer;
      ta.focus();
      ta.setSelectionRange(ta.value.length, ta.value.length);
    }
  }

  function presenceText(a) {
    if (a.state === "watching") return "Your agent is watching this console. It sees your message within seconds.";
    if (a.state === "seen") return `Your agent last checked this console at ${clock(a.last_seen)}. It reads your message the next time you message it in chat, because it checks this console first. You can also copy the message into the chat.`;
    return "Your agent hasn't checked this console yet. Copy the message into your chat, or ask your agent to run `ajx console since`.";
  }

  function renderActivity(s) {
    const ta = h("textarea", { rows: 4, "data-key": "composer", placeholder: "For example: why did the first convert fail?",
                               oninput: (e) => { ui.composer = e.target.value; } });
    ta.value = ui.composer;
    const composer = h("div", { class: "card" },
      h("h2", { text: "Message for your agent" }),
      h("p", { class: "small muted", text: presenceText(s.agent) }),
      ta,
      h("div", { class: "row", style: "margin-top:8px" },
        h("button", { class: "primary", onclick: async () => {
          const sent = ui.composer;
          if (!sent.trim()) { toast("Write a message first.", true); return; }
          const res = await act("/api/message", { text: sent }, s.agent.state === "watching" ? "Sent. Your agent sees it within seconds; replies appear below." : "Sent. Your agent reads it the next time it checks, and replies appear below. You can also paste it into the chat.");
          if (res && ui.composer === sent) { ui.composer = ""; renderTab("activity", true); }
        } }, "Send to my agent"),
        h("button", { onclick: async () => {
          try { await navigator.clipboard.writeText(ui.composer); toast("Copied. Paste it into your chat."); } catch (e) { toast("Copy failed; select the text and copy it.", true); }
        } }, "Copy for chat")),
      h("p", { class: "note", text: "Messages go to your agent only. They never reach the worker." }));
    const entries = [...s.journal.entries].reverse();
    const journal = h("div", { class: "card" }, h("h2", { text: "Activity" }),
      entries.length ? h("ul", { class: "list journal" }, entries.map((e) => h("li", {},
        h("time", { text: clock(e.at) }),
        h("span", { class: "who " + (e.actor || ""), text: { user: "you", agent: "agent", console: "console", runner: "run", outside: "outside" }[e.actor] || e.actor || "" }),
        h("span", {}, e.text, e.reply_to_text ? h("span", { class: "tiny muted", style: "display:block", text: `Reply to: ${e.reply_to_text}` }) : null,
          e.kind === "message" ? h("span", { class: "badge " + (e.answered ? "good" : "warn"), style: "margin-left:8px",
            text: e.answered ? "answered" : "waiting for your agent" }) : null)))) : h("p", { class: "muted", text: "Nothing yet." }));
    return [h("h1", { text: "Activity" }), h("p", { class: "sub", text: "Everything you, your agent and the console did with this trial. Your agent reads the same list with `ajx console since`." }), composer, journal];
  }

  // ------------------------------------------------------------------ boot
  function showNoKey() {
    document.getElementById("tab-plan").replaceChildren(h("div", { class: "card empty" },
      h("h2", { text: "Open the link printed by ajx console" }),
      h("p", { text: "This page needs the link that `ajx console open <trial>` prints (it carries a private key). Run it again to print the link." })));
  }

  document.querySelectorAll("#tabs button").forEach((b) => b.addEventListener("click", () => setTab(b.dataset.tab)));
  document.querySelectorAll(".tab").forEach((sec) => { sec.hidden = sec.id !== "tab-" + ui.tab; });
  document.querySelectorAll("#tabs button").forEach((b) => b.setAttribute("aria-selected", String(b.dataset.tab === ui.tab)));
  (async () => {
    if (!TOKEN) { showNoKey(); return; }
    try {
      await api("/api/session", {});
    } catch (e) {
      if (e.status === 403) { showNoKey(); return; }
    }
    poll();
  })();
})();
