/* Synthetic component tests for the actual console script, without a browser or packages. */
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

// Only the DOM operations used by these renderers. This does not emulate layout,
// browser events, polling, or focus behavior.
class Node {
  constructor(tag, text = "") {
    this.tag = tag;
    this.text = text;
    this.children = [];
    this.attributes = {};
    this.style = {};
  }
  append(child) { this.children.push(child); }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener() {}
  set textContent(value) {
    this.text = String(value);
    this.children = [];
  }
  get textContent() {
    return this.text + this.children.map((child) => child.textContent).join("");
  }
}

function components() {
  const file = path.resolve(__dirname, "../../skills/ajx/assets/console.js");
  const source = fs.readFileSync(file, "utf8");
  const boot = source.indexOf("  // ------------------------------------------------------------------ boot");
  assert.ok(boot > 0, "console boot boundary must remain explicit");
  let script = source.slice(0, boot);
  // Expose private renderers only in this test copy. Production keeps its IIFE
  // boundary and boot behavior; no test exports are shipped to the browser.
  const moduleReturn = "return { supported, render };";
  assert.equal(script.split(moduleReturn).length, 3, "expected Live and Results modules");
  script = script.replace(moduleReturn,
    "return { supported, render, verify(s, r) { L.s = s; return verifySection(s, r); } };");
  script = script.replace(moduleReturn, "return { supported, render, outcome };");
  script += "\nglobalThis.components = { runStage, stageStatus, verificationBadge, verificationWarning, verifyText, Live, Results };\n})();";
  const context = {
    Node, URLSearchParams, location: { hash: "" },
    sessionStorage: { getItem: () => null },
    document: { createElement: (tag) => new Node(tag), createTextNode: (text) => new Node("#text", text) },
  };
  vm.runInNewContext(script, context, { filename: file, timeout: 1000 });
  return context.components;
}

const ui = components();
const verify = (outcome = "succeeded") => ({
  outcome, passed: 1, total: 2, auth_errors: [],
  checks: [{ name: "synthetic check", passed: true, location: "environment", detail: "" }],
});
const snapshot = () => ({ process: { state: "idle" }, plan_model: { checks: [] } });
const run = (verification) => ({
  verify: verification, stages: [], execute: { status: "done" }, result: {}, current: null,
});
function descendants(node) {
  return [node, ...node.children.flatMap(descendants)];
}

test("shared stage lookup preserves absent and first matching stage behavior", () => {
  assert.equal(ui.stageStatus({}, "verify"), null);
  const first = { name: "verify", status: "error", at: "synthetic time" };
  const r = { stages: [first, { name: "verify", status: "done" }] };
  assert.equal(ui.runStage(r, "verify"), first);
  assert.equal(ui.stageStatus(r, "verify"), "error");
  assert.equal(ui.stageStatus(r, "narrate"), null);
});

test("shared verification badges retain every label, tone, and count", () => {
  for (const [outcome, label, tone] of [
    ["succeeded", "Verified", "good"], ["partial", "Some checks failed", "warn"],
    ["failed", "Checks failed", "bad"], ["not_run", "No checks", ""],
    ["unrecognized", "unrecognized", ""],
  ]) {
    const badge = ui.verificationBadge(verify(outcome));
    assert.equal(badge.className, "badge " + tone);
    assert.equal(badge.textContent, `${label} (1/2)`);
  }
});

test("classic and live views render the same badge and credential warning", () => {
  const v = verify("partial");
  v.auth_errors = ["synthetic check"];
  const classic = ui.verifyText(v);
  const live = descendants(ui.Live.verify(snapshot(), run(v)));
  assert.equal(live.find((node) => node.className === "badge warn").textContent, classic[0].textContent);
  assert.equal(live.find((node) => node.className === "errbox").textContent, classic[1].textContent);
  assert.match(classic[1].textContent, /measured AJX's access, not the worker's result/);
  assert.match(classic[2].textContent, /synthetic check/);
  assert.match(live[0].textContent, /ran inside its environment/);
  assert.equal(ui.verificationWarning(verify()), null);
});

test("verification remains pending when no result exists", () => {
  assert.match(ui.verifyText(null)[0].textContent, /Runs after the worker stops/);
  const r = run(null);
  r.execute.status = null;
  assert.match(ui.Live.verify(snapshot(), r).textContent, /Runs after the worker stops/);
});

test("Results keeps stricter failed-start and credential-error outcomes", () => {
  const s = snapshot(), v = verify(), r = run(v);
  r.result.could_not_start = true;
  assert.equal(ui.Results.outcome(s, r)[0], "warn");
  assert.match(ui.Results.outcome(s, r)[2], /worker could not start/);
  r.result = {};
  v.auth_errors = ["synthetic check"];
  assert.equal(ui.Results.outcome(s, r)[0], "warn");
  assert.match(ui.Results.outcome(s, r)[2], /credential errors/);
  v.auth_errors = [];
  assert.equal(ui.Results.outcome(s, r)[0], "good");
  r.verify = null;
  r.stages = [{ name: "verify", status: "error" }];
  assert.match(ui.Results.outcome(s, r)[2], /checks failed to run/);
});
