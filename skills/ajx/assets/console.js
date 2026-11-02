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
    openState: {}, inflight: false,
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
  function toast(msg, bad) {
    const el = $("#toast");
    el.textContent = msg;
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
    await refresh(false);
    const active = ui.snap && ACTIVE.includes(ui.snap.process.state);
    setTimeout(poll, active ? 1500 : 3000);
  }

  const SIG = {
    plan: (s) => JSON.stringify([s.trial, s.plan, s.readiness, s.process.state, s.process.by, s.process.since,
                                 s.output, s.locks, s.pending_start]),
    live: (s) => JSON.stringify([s.runs, s.process, s.output]),
    results: (s) => JSON.stringify([s.runs.map((r) => [r.id, r.started, r.asks, r.verify, r.links, r.review_status,
                                                       r.complete, r.declared, r.teardown]), s.output.index,
                                    s.process.state, ui.exports]),
    activity: (s) => JSON.stringify([s.journal, s.agent]),
  };
  const RENDER = { plan: renderPlan, live: renderLive, results: renderResults, activity: renderActivity };

  function render(force) {
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
    el.replaceChildren(...RENDER[name](s));
    for (const [key, top, atEnd] of scrollers) {
      const n = el.querySelector(`[data-scroll="${key}"]`);
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
    const el = root.querySelector(`[data-key="${f.key}"]`);
    if (!el) return;
    el.focus();
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

  function autoSwitch(s) {
    const prev = ui.lastState, cur = s.process.state;
    ui.lastState = cur;
    if (!prev || prev === cur) return;
    const editing = anyDirty(s) || (document.activeElement && document.activeElement.dataset && document.activeElement.dataset.key);
    if (cur === "running" && ui.tab === "plan" && !editing) setTab("live");
    else if (cur === "running" && ui.tab === "plan") toast("The run started. Open the Live tab to follow it.");
    else if ((prev === "running" || prev === "stopping") && cur === "idle" && ui.tab === "live"
             && s.runs.some((r) => r.asks.status === "ready")) setTab("results");
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
    $("#chips").replaceChildren(...chips);
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
    return promptDirty || textDirty || Object.keys(settingsChanges(s)).length > 0;
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
    if (out.blocker) return { disabled: true, label: out.blocker_kind === "complete" ? "Finished" : "Check and start", reason: out.blocker };
    if (!s.plan.complete) return { disabled: true, label: "Check and start", reason: "The starting files or trial plugins are too large to fingerprint for approval. Start this trial with ajx run." };
    const ready = r.current && r.status === "ready";
    if (out.resumable) {
      return { disabled: false, label: "Resume", reason: "Finishes the remaining stages; a finished or stopped attempt is never run again. Readiness is checked first if the plan changed." };
    }
    return {
      disabled: false, label: ready ? "Start" : "Check and start",
      reason: ready ? `Ready for this plan (checked at ${clock(r.checked_at)}).`
        : r.current && r.status === "not_ready" ? "The last check failed. This checks again and starts only if it passes."
          : "Runs ajx validate and ajx doctor first, and starts only if both pass.",
    };
  }

  function renderPlan(s) {
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
      const model = c.model_editable
        ? h("input", { type: "text", "data-key": `cell-${c.raw_index}`, placeholder: "agent default", disabled: lock ? true : null,
                       value: (ui.drafts.settings.cell_models || {})[c.raw_index] ?? (c.model || ""),
                       oninput: (e) => { ui.drafts.settings.cell_models = { ...(ui.drafts.settings.cell_models || {}), [c.raw_index]: e.target.value }; updateDirty(); } })
        : h("span", { class: "mono", text: c.model || "agent default" });
      return h("li", {},
        h("div", { class: "row spread" }, h("div", { class: "row" }, h("b", { text: c.harness }), h("span", { class: "tiny muted", text: c.id }),
            c.installed ? null : h("span", { class: "badge bad", text: "not installed" }),
            c.verified_live ? null : h("span", { class: "badge warn", text: "adapter not yet verified against a live run", title: "AJX's adapter for this agent follows its documented output format but has not been confirmed with a live run." }))),
        h("label", { class: "field" }, h("span", { text: "Model" }), model));
    });
    const rep = t.reporter || {};
    const repModel = h("input", { type: "text", "data-key": "reporter-model", placeholder: "agent default", disabled: lock ? true : null,
                                  value: draftValue(s, "reporter_model"),
                                  oninput: (e) => { ui.drafts.settings.reporter_model = e.target.value; updateDirty(); } });
    return h("div", { class: "card" },
      h("h2", { text: "Worker and reporter" }),
      h("ul", { class: "list" }, rows,
        h("li", {}, h("div", { class: "row" }, h("b", { text: rep.harness || "reporter" }), h("span", { class: "tiny muted", text: "reporter: writes the analysis after the task, kept the same across runs" })),
          h("label", { class: "field" }, h("span", { text: "Reporter model" }), repModel))),
      h("p", { class: "note", text: "Save model changes with \u201cSave changes\u201d under Your decisions." }));
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
    const remedies = [];
    if (out.started) {
      remedies.push(h("button", { onclick: () => { if (confirm("Point trial.output_dir at a new, empty folder? The recorded attempts stay where they are. The plan changes, so starting needs a new approval.")) act("/api/fresh-output", { base: t.sha256 }, (r) => `Fresh output folder: ${r.output_dir}. Start needs a new approval.`); }, disabled: lock ? true : null }, "Use a fresh output folder"));
    }
    if (out.blocker_kind === "complete") {
      remedies.push(h("button", { onclick: () => { if (confirm("Add one more attempt of the same task in this folder? The plan changes, so starting needs a new approval.")) act("/api/add-attempt", { base: t.sha256 }, "Added an attempt. Start runs only the new one, after a new approval."); }, disabled: lock ? true : null }, "Add another attempt"));
    }
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
      remedies.length ? h("div", { class: "row" }, remedies) : null,
      h("div", { class: "hr" }),
      h("h3", { text: "Readiness" }),
      readinessView(s),
      h("button", { id: "start-btn", class: "primary start-btn", disabled: st.disabled ? true : null, "aria-describedby": "start-reason",
                    onclick: (e) => { e.currentTarget.disabled = true; act("/api/start", { plan: s.plan.sha }, "Checking readiness, then starting\u2026"); } }, st.label),
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
    return h("div", {}, head, steps,
      h("div", { class: "row", style: "margin-top:8px" },
        h("button", { onclick: () => act("/api/check", {}, "Checking readiness\u2026"), disabled: active || s.trial.error ? true : null }, "Check now")));
  }

  // ------------------------------------------------------------------ live tab
  function renderLive(s) {
    const out = s.output;
    const running = s.runs.filter((r) => r.current).length;
    const ready = s.runs.filter((r) => r.complete && r.links.report).length;
    const counts = `${running} running \u00b7 ${out.complete} of ${out.planned} planned attempt(s) finished \u00b7 ${ready} report(s) ready`;
    const runs = s.runs.filter((r) => r.started || r.planned);
    const items = [h("h1", { text: "Live" }), h("p", { class: "sub", text: counts }), processCard(s)];
    if (!s.runs.some((r) => r.started) && !ACTIVE.includes(s.process.state)) {
      items.push(h("div", { class: "card empty" }, "Nothing has run yet. Review the plan and press ", h("b", { text: "Check and start" }), "."));
    }
    return items.concat(runs.map((r) => runCard(s, r)));
  }

  function processCard(s) {
    const p = s.process;
    const active = ACTIVE.includes(p.state);
    const text = { idle: "Nothing is running.", running: "AJX is running the trial.", stopping: "Stopping. AJX stops the worker, cleans up and keeps the evidence.",
                   checking: "Checking readiness.", starting: "Checking readiness, then starting.", busy: "Rebuilding the reports." }[p.state] || p.state;
    const lines = p.log_tail || [];
    return h("div", { class: "card" + (p.state === "stopping" ? " attn" : "") },
      h("div", { class: "row spread" },
        h("div", {}, h("h2", { text: text }),
          p.command && active ? h("p", { class: "cmd", text: `${p.command} \u00b7 started ${byText(p.by)} at ${clock(p.since)} \u00b7 pid ${p.pid}` }) : null),
        (p.state === "running" || p.state === "stopping")
          ? h("button", { class: "danger", "data-key": "stop", disabled: p.state === "stopping" || (p.parallel > 1 && p.job === "run") ? true : null,
                          onclick: () => { if (confirm("Stop the run?\n\nAJX stops the worker, runs cleanup and keeps the evidence captured so far. Resume later to write the report.")) act("/api/stop", {}, "Stop requested. Cleanup runs before AJX exits."); } },
              p.state === "stopping" ? "Stopping\u2026" : "Stop")
          : null),
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
    const recovery = r.recovery.map((x) => h("div", { class: "warnbox" }, x.why, " ",
      h("button", { disabled: ACTIVE.includes(s.process.state) ? true : null,
                    onclick: () => { if (x.stages !== "extract,measure,render" || confirm("This calls the reporter model again. Continue?")) act("/api/stages", { run: r.id, stages: x.stages }, `${x.label}\u2026`); } }, x.label),
      h("div", { class: "cmd", text: `runs: ajx run <trial> --cells ${r.id.replace(/-r\d+$/, "")} --stages ${x.stages}` })));
    return h("div", { class: "card" },
      h("div", { class: "row spread" }, h("div", { class: "row" }, h("h2", { text: r.id, style: "margin:0" }), runStatus(r)),
        r.links.report ? link(r.links.report, "Open report") : null),
      h("div", { class: "pills" }, pills),
      timer, feed,
      errors.map((st) => h("div", { class: "errbox", text: `${st.name}: ${st.error}` })),
      r.started ? h("div", { class: "minis" },
        h("div", { class: "mini" }, h("h3", { text: "Verification" }), verifyText(r.verify)),
        h("div", { class: "mini" }, h("h3", { text: "Cleanup" }), cleanupText(r)),
        h("div", { class: "mini" }, h("h3", { text: "Reporting" }), asksText(r.asks, r))) : null,
      recovery);
  }

  // ------------------------------------------------------------------ results tab
  function renderResults(s) {
    const runs = s.runs.filter((r) => r.started);
    const items = [h("h1", { text: "Results" }),
      h("p", { class: "sub", text: "Start with the verified outcome, then the asks and the evidence behind them. Asks come from one recorded attempt; they are not a product score." })];
    if (!runs.length) return items.concat(h("div", { class: "card empty", text: "Results appear here as each attempt finishes reporting." }));
    for (const r of runs) items.push(resultCard(s, r));
    items.push(h("div", { class: "card" },
      h("div", { class: "row" },
        s.output.index ? link(s.output.index, "Open the matrix overview (index.html)") : h("span", { class: "muted", text: "The matrix overview appears after the first report." }),
        h("button", { disabled: ACTIVE.includes(s.process.state) ? true : null, onclick: () => act("/api/report", {}, "Rebuilding the reports (no model calls)\u2026") }, "Rebuild reports")),
      h("p", { class: "cmd", text: "Rebuild runs: ajx report <trial> --no-synthesis" })));
    return items;
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
    const body = [];
    if (r.declared) body.push(h("p", { class: "small" }, h("b", { text: "The worker said: " }), r.declared));
    if (r.journey_provenance && r.journey_provenance.startsWith("EDITORIAL")) body.push(h("p", { class: "note", text: "The journey is a labeled reconstruction, not the worker's own account." }));
    if (a.status === "ready") {
      if (!a.count) body.push(h("p", { class: "muted", text: a.no_obstacles_observed ? "No obstacles observed in this attempt." : "No asks were extracted. Check the verification and the journey before reading this as an easy experience." }));
      for (const ask of a.top) {
        body.push(h("div", { class: "ask" },
          h("div", {},
            h("div", { class: "row" }, h("span", { class: "badge dark", text: ask.id || "" }), h("b", { text: ask.title }),
              (ask.labels || []).map((l) => h("span", { class: "badge", text: l }))),
            h("p", { text: ask.requested_behavior }),
            ask.consequence ? h("p", { class: "small", text: ask.consequence }) : null,
            h("div", { class: "refs", text: "Evidence: " + ((ask.event_refs || []).join(", ") || "journey anchors") })),
          h("button", { "aria-label": `Ask my agent about ${ask.id}`, onclick: () => askAgent(`About ${ask.id} \u201c${ask.title}\u201d in ${r.id}: `) }, "Ask my agent")));
      }
      if (a.count > a.top.length) body.push(h("p", { class: "note" }, `${a.count - a.top.length} more in `, r.links.report ? link(r.links.report, "the report") : "the report", "."));
      if (a.strengths || a.gates) body.push(h("p", { class: "note", text: `${a.strengths} helpful behavior(s) and ${a.gates} gate(s) are in the report.` }));
    } else {
      body.push(h("div", { class: "row" }, asksText(a, r)));
    }
    const exp = ui.exports[r.id];
    body.push(h("div", { class: "hr" }), h("div", { class: "row" },
      h("button", { disabled: !r.complete || ACTIVE.includes(s.process.state) ? true : null,
                    onclick: async () => { const res = await act("/api/export", { run: r.id }, (x) => `Exported to ${x.dest}`); if (res) { ui.exports[r.id] = res; renderTab("results", true); } } }, "Export for a team"),
      h("span", { class: "tiny muted", text: "report.html, journey.html and asks.md with your local paths relabeled. Review them before sharing." })),
      exp ? h("div", { class: "okbox" }, `Exported to ${exp.dest}: `, exp.files.map((f, i) => [i ? ", " : "", link(f, f.split("/").pop())])) : null);
    return h("div", { class: "card" }, head, ...body);
  }

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
        h("span", {}, e.text, e.reply_to_text ? h("span", { class: "tiny muted", style: "display:block", text: `Reply to: ${e.reply_to_text}` }) : null)))) : h("p", { class: "muted", text: "Nothing yet." }));
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
