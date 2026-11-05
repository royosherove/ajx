"""Offline tests for `ajx export` (a shareable copy of one run) and `ajx status --tail` (recent worker
events while a cell executes). Synthetic run directories and fake harnesses only; no agent, model or
cloud call is made."""

import contextlib
import io
import os
import re
import stat
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))

from ajx import export, plugins  # noqa: E402
from ajx.base import Harness  # noqa: E402
from ajx.util import write_json  # noqa: E402

# Unnamed attempt directories look like secrets.token_hex(5); these are invented.
HEX_WS, HEX_HOME, HEX_CFG, HEX_CACHE, HEX_SIBLING = "8b41708955", "1f2e3d4c5b", "a0b1c2d3e4", "f9e8d7c6b5", "9c1e0f2a3b"


def _cli():
    import importlib.util
    from importlib.machinery import SourceFileLoader
    path = str(HERE.parent / "bin" / "ajx")
    spec_ = importlib.util.spec_from_file_location("ajx_cli", path, loader=SourceFileLoader("ajx_cli", path))
    cli = importlib.util.module_from_spec(spec_)
    spec_.loader.exec_module(cli)
    return cli


def _run_cli(argv):
    out, err = io.StringIO(), io.StringIO()
    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
        code = _cli().main(argv)
    return code, out.getvalue(), err.getvalue()


def _layout(tmp):
    """A coordinator home with the trial's workspace_root inside it; config and cache nested in the
    agent home so the longest-first ordering is exercised."""
    home = Path(tmp) / "home" / "me"
    root = home / "ajx" / "work"
    agent_home = root / HEX_HOME
    return {"home": str(home), "root": str(root),
            "paths": {"workspace": str(root / HEX_WS), "home_dir": str(agent_home),
                      "config_dir": str(agent_home / HEX_CFG), "cache_dir": str(agent_home / HEX_CACHE)}}


def _report_html(lay):
    p, home, root = lay["paths"], lay["home"], lay["root"]
    enc_ws = p["workspace"].replace("/", "-")
    return f'''<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>prod: product asks</title></head><body>
<aside class="rail"><a class="brand" href="../../index.html">AJX<small>Agent Journey Experience</small></a>
<nav aria-label="Report views"><a href="report.html" aria-current="page">Product asks</a><a href="pretty-journey.html">Agent journey</a><a href="../../index.html">Compare runs</a><a href="asks.md">Markdown asks</a><a href="run.json">Run data</a></nav></aside>
<main id="content">
<p>Workspace {p["workspace"]} with transcript {home}/.claude/projects/{enc_ws}/sid.jsonl, settings in {p["config_dir"]}/settings.json, caches under {p["cache_dir"]}/npm, agent home {p["home_dir"]}.</p>
<p>A stale sibling attempt {root}/{HEX_SIBLING} was visible; the trial lives in {home}/ajx/trials/demo.</p>
<div class="links"><a class="mono" href="pretty-journey.html#E-003">E-003</a><a href="#ASK-001-detail">Inspect evidence and verification</a></div>
<ul><li>Harness: fake (1.0)</li><li><a href="verify.json">Inspect verification checks</a> after the run</li><li>Stop: completed</li></ul>
<div class="links"><a href="environment.json">Environment evidence</a> <a href="agent-configuration.json#top">Agent configuration manifest</a><a href="run.json">Complete run evidence</a></div>
<a href="pretty-journey.html">Open journey map and event table</a>
</main></body></html>
'''


def _journey_html(lay):
    p = lay["paths"]
    return f'''<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>prod: agent journey</title></head><body>
<aside class="rail"><a class="brand" href="../../index.html">AJX<small>Agent Journey Experience</small></a>
<nav aria-label="Report views"><a href="report.html">Product asks</a><a href="pretty-journey.html" aria-current="page">Agent journey</a><a href="../../index.html">Compare runs</a><a href="asks.md">Markdown asks</a><a href="run.json">Run data</a></nav></aside>
<main id="content"><table><tr id="E-003"><td><a class="mono" href="#E-003">E-003</a></td><td>cd {p["workspace"]} && ls {p["config_dir"]}</td><td><a class="mono" href="report.html#ASK-001">ASK-001</a></td></tr></table>
<p>Journey written from {p["workspace"]}.</p></main></body></html>
'''


def _asks_md(lay):
    p = lay["paths"]
    return f'''# Asks: prod (f-r1)

Harness fake, model m. Task outcome declared by the agent is recorded in run.json; verified outcome: **succeeded**.

- **How observed:** ran `ls {p["workspace"]}` and read {p["config_dir"]}/settings.json
- [Environment evidence](environment.json)
- [Agent configuration manifest](agent-configuration.json)

See [E-003](pretty-journey.html#E-003) and [Complete run evidence](run.json#top).
'''


def _run_dir(out, lay, run_id="f-r1", execute="done", harness="fake-tail", with_paths=True, run_json=True):
    d = Path(out) / "runs" / run_id
    d.mkdir(parents=True, exist_ok=True)
    state = {"stages": {"prepare": {"status": "done"}, "execute": {"status": execute}}}
    if with_paths:
        state["paths"] = lay["paths"]
    write_json(d / "state.json", state)
    if run_json:
        write_json(d / "run.json", {"run_id": run_id, "agent": {"harness_name": harness}})
    (d / "report.html").write_text(_report_html(lay), encoding="utf-8")
    (d / "pretty-journey.html").write_text(_journey_html(lay), encoding="utf-8")
    (d / "asks.md").write_text(_asks_md(lay), encoding="utf-8")
    return d


def _texts(dest):
    return {name: (Path(dest) / name).read_text(encoding="utf-8") for name in ("report.html", "journey.html", "asks.md")}


class ExportRunTest(unittest.TestCase):
    def _export(self, tmp, **kwargs):
        lay = _layout(tmp)
        run_dir = _run_dir(Path(tmp) / "out", lay, **kwargs)
        dest = Path(tmp) / "share" / "f-r1"
        return lay, run_dir, dest, export.export_run(run_dir, dest, home=lay["home"])

    def test_bundle_is_exactly_three_files_with_the_journey_renamed(self):
        with tempfile.TemporaryDirectory() as tmp:
            _, _, dest, summary = self._export(tmp)
            self.assertEqual(sorted(p.name for p in dest.iterdir()), ["asks.md", "journey.html", "report.html"])
            self.assertFalse((dest / "pretty-journey.html").exists())
            self.assertEqual(summary["files"], ["report.html", "journey.html", "asks.md"])
            self.assertEqual(summary["dest"], str(dest))
            self.assertEqual(summary["warnings"], [])
            for name in summary["files"]:
                self.assertEqual(stat.S_IMODE(os.stat(dest / name).st_mode), 0o644, name)

    def test_worker_directories_become_labels_longest_first(self):
        with tempfile.TemporaryDirectory() as tmp:
            lay, _, dest, _ = self._export(tmp)
            texts = _texts(dest)
            for name, text in texts.items():
                for raw in lay["paths"].values():
                    self.assertNotIn(raw, text, name)
                    self.assertNotIn(raw.replace("/", "-"), text, name)
                self.assertNotIn(lay["home"], text, name)
                self.assertNotIn(HEX_SIBLING, text, name)
                self.assertNotIn(HEX_CFG, text, name)  # nested config dir got its own label, not /agent-home/<hex>
                self.assertIsNone(re.search(re.escape(lay["root"]) + r"/[0-9a-f]{6,}", text), name)
            report = texts["report.html"]
            self.assertIn("Workspace /workspace with transcript ~/.claude/projects/-workspace/sid.jsonl", report)
            self.assertIn("settings in /agent-home/.config/harness/settings.json", report)
            self.assertIn("caches under /agent-home/.cache/npm, agent home /agent-home.", report)
            self.assertIn("A stale sibling attempt /agent-home/attempt was visible; the trial lives in ~/ajx/trials/demo.", report)
            self.assertIn("cd /workspace && ls /agent-home/.config/harness", texts["journey.html"])
            self.assertIn("ran `ls /workspace` and read /agent-home/.config/harness/settings.json", texts["asks.md"])

    def test_links_outside_the_bundle_become_text_and_journey_links_follow_the_rename(self):
        with tempfile.TemporaryDirectory() as tmp:
            _, _, dest, _ = self._export(tmp)
            texts = _texts(dest)
            for name in ("report.html", "journey.html"):
                text = texts[name]
                for outside in export.OUTSIDE:
                    self.assertNotIn(f'href="{outside}', text, name)
                self.assertNotIn('href="pretty-journey.html', text, name)
                self.assertIn("Run data", text, name)          # link text survives the unwrap
                self.assertIn("Compare runs", text, name)
                self.assertNotIn("Traceback", text, name)
            report = texts["report.html"]
            self.assertIn('<a class="mono" href="journey.html#E-003">E-003</a>', report)
            self.assertIn('<a href="journey.html">Agent journey</a>', report)
            self.assertIn('<a href="journey.html">Open journey map and event table</a>', report)
            self.assertIn('<a href="report.html" aria-current="page">Product asks</a>', report)
            self.assertIn('<a href="#ASK-001-detail">Inspect evidence and verification</a>', report)
            self.assertIn('<div class="links">Environment evidence Agent configuration manifestComplete run evidence</div>', report)
            # the list item that only existed to carry a verify.json link is gone; its neighbours stay
            self.assertNotIn("Inspect verification checks", report)
            self.assertNotIn("after the run", report)
            self.assertIn("<ul><li>Harness: fake (1.0)</li><li>Stop: completed</li></ul>", report)
            journey = texts["journey.html"]
            self.assertIn('<a href="journey.html" aria-current="page">Agent journey</a>', journey)
            self.assertIn('<a class="mono" href="report.html#ASK-001">ASK-001</a>', journey)
            self.assertIn('<a class="mono" href="#E-003">E-003</a>', journey)

    def test_brand_link_keeps_its_class_and_loses_its_href(self):
        with tempfile.TemporaryDirectory() as tmp:
            _, _, dest, _ = self._export(tmp)
            for name in ("report.html", "journey.html"):
                text = (dest / name).read_text()
                self.assertIn('<a class="brand">AJX<small>Agent Journey Experience</small></a>', text, name)
                self.assertNotIn('<a class="brand" href=', text, name)

    def test_markdown_links_follow_the_same_rules(self):
        with tempfile.TemporaryDirectory() as tmp:
            _, _, dest, _ = self._export(tmp)
            asks = (dest / "asks.md").read_text()
            self.assertIn("See [E-003](journey.html#E-003) and Complete run evidence.", asks)
            self.assertNotIn("](run.json", asks)
            self.assertNotIn("pretty-journey", asks)
            self.assertNotIn("Environment evidence", asks)            # bullet that only carried a link
            self.assertNotIn("Agent configuration manifest", asks)
            self.assertIn("recorded in run.json;", asks)              # prose mentions are not links

    def test_summary_counts_are_sane(self):
        with tempfile.TemporaryDirectory() as tmp:
            _, _, _, summary = self._export(tmp)
            counts = summary["replacements"]
            self.assertEqual(set(counts), {"/workspace", "/agent-home", "/agent-home/.config/harness",
                                           "/agent-home/.cache", "/agent-home/attempt", "/agent-home/attempts", "~"})
            self.assertTrue(all(isinstance(n, int) and n >= 0 for n in counts.values()), counts)
            self.assertEqual(counts["/workspace"], 5)                 # report: raw + encoded; journey: 2; asks: 1
            self.assertEqual(counts["/agent-home/.config/harness"], 3)
            self.assertEqual(counts["/agent-home/.cache"], 1)
            self.assertEqual(counts["/agent-home"], 1)
            self.assertEqual(counts["/agent-home/attempt"], 1)
            self.assertEqual(counts["~"], 2)

    def test_missing_source_fails_before_anything_is_written(self):
        with tempfile.TemporaryDirectory() as tmp:
            lay = _layout(tmp)
            run_dir = _run_dir(Path(tmp) / "out", lay)
            (run_dir / "asks.md").unlink()
            dest = Path(tmp) / "share"
            with self.assertRaises(export.ExportError) as caught:
                export.export_run(run_dir, dest, home=lay["home"])
            self.assertIn("asks.md", str(caught.exception))
            self.assertFalse(dest.exists())
            with self.assertRaises(export.ExportError):
                export.export_run(Path(tmp) / "nope", dest, home=lay["home"])

    def test_run_without_recorded_paths_exports_with_a_warning(self):
        with tempfile.TemporaryDirectory() as tmp:
            lay, _, dest, summary = self._export(tmp, with_paths=False)
            self.assertTrue(summary["warnings"] and "paths" in summary["warnings"][0], summary)
            report = (dest / "report.html").read_text()
            self.assertNotIn(lay["home"], report)                     # the home rule still applies
            self.assertIn("~/ajx/work/" + HEX_WS, report)
            self.assertEqual(set(summary["replacements"]), {"~"})

    def test_home_defaults_to_the_coordinators_home(self):
        with tempfile.TemporaryDirectory() as tmp:
            lay = _layout(tmp)
            run_dir = _run_dir(Path(tmp) / "out", lay)
            real_home = str(Path.home())
            (run_dir / "asks.md").write_text(f"trial dir {real_home}/ajx/trials/demo and {lay['paths']['workspace']}\n")
            summary = export.export_run(run_dir, Path(tmp) / "share")
            asks = (Path(tmp) / "share" / "asks.md").read_text()
            self.assertEqual(asks, "trial dir ~/ajx/trials/demo and /workspace\n")
            self.assertGreaterEqual(summary["replacements"]["~"], 1)

    def test_boundaries_keep_longer_names_intact(self):
        rules = export.replacement_rules({"workspace": "/w/root/8b41708955"}, "/srv/people/me")
        text = ("/srv/people/me/x /srv/people/meredith/x /srv/people/me.bak /srv/people/me. -srv-people-me-y -srv-people-meredith "
                "/w/root/8b41708955/sub /w/root/9c1e0f2a3b/f /w/root/cafe /w/root/8b41708955xyz")
        out = export.rewrite_paths(text, rules)
        # The bare root is relabeled too, so a non-attempt child or a truncated attempt name keeps no layout.
        self.assertEqual(out, "~/x /srv/people/meredith/x /srv/people/me.bak ~. ~-y -srv-people-meredith "
                              "/workspace/sub /agent-home/attempt/f /agent-home/attempts/cafe "
                              "/agent-home/attempts/8b41708955xyz")


class ExportCliTest(unittest.TestCase):
    def test_directory_mode_default_dest_and_overwrite_protection(self):
        with tempfile.TemporaryDirectory() as tmp:
            lay = _layout(tmp)
            out = Path(tmp) / "out"
            _run_dir(out, lay)
            code, stdout, stderr = _run_cli(["export", str(out), "--run", "f-r1"])
            self.assertEqual(code, 0, stderr)
            dest = out / "share" / "f-r1"
            self.assertTrue(dest.is_dir())
            self.assertEqual(sorted(p.name for p in dest.iterdir()), ["asks.md", "journey.html", "report.html"])
            self.assertIn(str(dest), stdout)
            for name in ("report.html", "journey.html", "asks.md"):
                self.assertIn(name, stdout)
            self.assertIn("/workspace", stdout)
            self.assertIn("Review before sharing: exported reports still contain the agent's commands and outputs, "
                          "account identifiers, and URLs.", stdout)
            code, _, stderr = _run_cli(["export", str(out), "--run", "f-r1"])
            self.assertEqual(code, 1)
            self.assertIn("--force", stderr)
            self.assertEqual(len(stderr.strip().splitlines()), 1)
            code, _, stderr = _run_cli(["export", str(out), "--run", "f-r1", "--force"])
            self.assertEqual(code, 0, stderr)

    def test_trial_file_mode_and_error_lines(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            (tmp / "task-prompt.md").write_text("x\n")
            (tmp / "trial.toml").write_text(f'[trial]\nid = "t"\nproduct = "p"\nworkspace_root = "{tmp}/ws"\n'
                                            '[task]\nprompt_file = "task-prompt.md"\n[[cells]]\nid = "f"\nharness = "fake-tail"\n')
            out = tmp / "ajx-reports" / "p" / "t"
            lay = _layout(tmp)
            _run_dir(out, lay)
            dest = tmp / "bundle"
            code, stdout, stderr = _run_cli(["export", str(tmp / "trial.toml"), "--run", "f-r1", "--dest", str(dest)])
            self.assertEqual(code, 0, stderr)
            self.assertTrue((dest / "journey.html").is_file())
            self.assertIn(str(dest), stdout)
            code, _, stderr = _run_cli(["export", str(tmp / "trial.toml"), "--run", "f-r9"])
            self.assertEqual(code, 1)
            self.assertIn("f-r9", stderr)
            self.assertIn("f-r1", stderr)                             # the known runs are listed
            self.assertEqual(len(stderr.strip().splitlines()), 1)
            code, _, stderr = _run_cli(["export", str(out), "--run", "../f-r1"])
            self.assertEqual(code, 1)
            code, _, stderr = _run_cli(["export", str(out), "--run", "f-r1", "--dest", str(out / "runs" / "f-r1"), "--force"])
            self.assertEqual(code, 1)                                 # never overwrite the run's own reports
            self.assertIn("run directory", stderr)
            (out / "runs" / "f-r1" / "pretty-journey.html").unlink()
            code, _, stderr = _run_cli(["export", str(out), "--run", "f-r1", "--dest", str(tmp / "b2")])
            self.assertEqual(code, 1)
            self.assertIn("pretty-journey.html", stderr)


class FakeTail(Harness):
    """Reports two recent worker events and records how it was asked."""
    plugin_name = "fake-tail"
    binary = "true"
    can_report = True
    calls = []

    def progress(self, run_dir, stage="execute", limit=10):
        FakeTail.calls.append((Path(run_dir).name, stage, limit))
        return [{"at": "2026-01-01T10:00:01Z", "kind": "tool", "text": "Bash: ls -la"},
                {"at": None, "kind": "text", "text": "Reading the README\nbefore the next step"}][-limit:]


class FakeBrokenTail(Harness):
    plugin_name = "fake-broken-tail"
    binary = "true"

    def progress(self, run_dir, stage="execute", limit=10):
        raise RuntimeError("stream unreadable")


class LegacyHarness:
    """An adapter written before the progress contract: no progress attribute at all. Duck-typed
    (not a Harness subclass, which now inherits progress) with the surface `plugins`/`doctor` read."""
    plugin_name = name = "fake-legacy"
    binary = "true"
    can_resume = can_report = clean_supported = verified_live = False
    default_auth = "inherit"
    telemetry = "wall clock only"
    strip_env = ()

    def available(self):
        return True

    def version(self):
        return None


plugins.register("harness", "fake-tail")(FakeTail)
plugins.register("harness", "fake-broken-tail")(FakeBrokenTail)
plugins.register("harness", "fake-legacy")(LegacyHarness)

EVENT_LINE = re.compile(r"^  (\d\d:\d\d:\d\d|--:--:--) (tool |text |resul) (.*)$", re.M)


def _trial(tmp, harness="fake-tail"):
    tmp = Path(tmp)
    (tmp / "task-prompt.md").write_text("x\n")
    (tmp / "trial.toml").write_text(f'[trial]\nid = "t"\nproduct = "p"\nworkspace_root = "{tmp}/ws"\n'
                                    f'[task]\nprompt_file = "task-prompt.md"\n[[cells]]\nid = "f"\nharness = "{harness}"\n')
    return tmp / "trial.toml", tmp / "ajx-reports" / "p" / "t"


class StatusTailTest(unittest.TestCase):
    def setUp(self):
        FakeTail.calls.clear()

    def test_tail_prints_recent_events_for_a_running_cell(self):
        with tempfile.TemporaryDirectory() as tmp:
            trial, out = _trial(tmp)
            _run_dir(out, _layout(tmp), execute="running", run_json=False)   # run.json does not exist yet while executing
            code, stdout, stderr = _run_cli(["status", str(trial), "--tail", "2"])
            self.assertEqual(code, 0, stderr)
            lines = stdout.splitlines()
            self.assertTrue(lines[0].startswith("run "), lines[0])
            self.assertTrue(lines[1].startswith("f-r1 ") and " run " in lines[1], lines[1])
            self.assertIn("f-r1: last 2 worker events (execute)", stdout)
            events = EVENT_LINE.findall(stdout)
            self.assertEqual([e[1:] for e in events], [("tool ", "Bash: ls -la"), ("text ", "Reading the README before the next step")])
            self.assertRegex(events[0][0], r"^\d\d:\d\d:\d\d$")
            self.assertEqual(events[1][0], "--:--:--")
            self.assertEqual(FakeTail.calls, [("f-r1", "execute", 2)])

    def test_tail_skips_finished_runs_while_another_executes(self):
        with tempfile.TemporaryDirectory() as tmp:
            trial, out = _trial(tmp)
            lay = _layout(tmp)
            _run_dir(out, lay, run_id="f-r1", execute="done")
            _run_dir(out, lay, run_id="f-r2", execute="running", run_json=False)
            code, stdout, _ = _run_cli(["status", str(trial), "--tail", "5"])
            self.assertEqual(code, 0)
            self.assertIn("f-r2: last 5 worker events", stdout)
            self.assertNotIn("f-r1: last", stdout)
            self.assertEqual(FakeTail.calls, [("f-r2", "execute", 5)])

    def test_tail_shows_every_run_when_none_is_executing(self):
        with tempfile.TemporaryDirectory() as tmp:
            trial, out = _trial(tmp)
            _run_dir(out, _layout(tmp), execute="done")
            code, stdout, _ = _run_cli(["status", str(trial), "--tail", "1"])
            self.assertEqual(code, 0)
            self.assertIn("f-r1: last 1 worker events", stdout)
            self.assertEqual([e[1:] for e in EVENT_LINE.findall(stdout)], [("text ", "Reading the README before the next step")])

    def test_status_without_tail_shows_stage_age_without_reading_events(self):
        with tempfile.TemporaryDirectory() as tmp:
            trial, out = _trial(tmp)
            _run_dir(out, _layout(tmp), execute="running", run_json=False)
            code, stdout, _ = _run_cli(["status", str(trial)])
            self.assertEqual(code, 0)
            self.assertIn("execute: started", stdout)
            self.assertIn("elapsed", stdout)
            self.assertEqual(FakeTail.calls, [])

    def test_directory_mode_finds_the_harness_in_run_json_or_environment_json(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "out"
            lay = _layout(tmp)
            _run_dir(out, lay, run_id="a-r1", execute="running")                     # run.json names the harness
            d = _run_dir(out, lay, run_id="b-r1", execute="running", run_json=False)
            write_json(d / "environment.json", {"harness": {"name": "fake-tail"}})   # written at prepare
            code, stdout, _ = _run_cli(["status", str(out), "--tail", "3"])
            self.assertEqual(code, 0)
            self.assertEqual(sorted(FakeTail.calls), [("a-r1", "execute", 3), ("b-r1", "execute", 3)])
            self.assertEqual(len(EVENT_LINE.findall(stdout)), 4)

    def test_harness_without_progress_prints_the_table_normally(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "out"
            _run_dir(out, _layout(tmp), execute="running", harness="fake-legacy")
            code, stdout, stderr = _run_cli(["status", str(out), "--tail", "2"])
            self.assertEqual(code, 0, stderr)
            lines = stdout.splitlines()
            self.assertTrue(lines[0].startswith("run ") and lines[1].startswith("f-r1 "), lines[:2])
            self.assertIn("  (fake-legacy does not report progress)", stdout)
            self.assertEqual(EVENT_LINE.findall(stdout), [])
            self.assertNotIn("Traceback", stdout + stderr)

    def test_progress_failures_and_unknown_harnesses_never_break_status(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "out"
            lay = _layout(tmp)
            _run_dir(out, lay, run_id="a-r1", execute="running", harness="fake-broken-tail")
            _run_dir(out, lay, run_id="b-r1", execute="running", harness="no-such-harness")
            _run_dir(out, lay, run_id="c-r1", execute="running", run_json=False)   # nothing records a harness
            code, stdout, stderr = _run_cli(["status", str(out), "--tail", "2"])
            self.assertEqual(code, 0, stderr)
            self.assertIn("a-r1: last 2 worker events", stdout)
            self.assertIn("  (progress unavailable: RuntimeError: stream unreadable)", stdout)
            self.assertIn("b-r1: last 2 worker events", stdout)
            self.assertIn("'no-such-harness'", stdout)
            self.assertIn("c-r1: last 2 worker events", stdout)
            self.assertIn("harness unknown", stdout)
            self.assertNotIn("Traceback", stdout + stderr)


if __name__ == "__main__":
    unittest.main()
