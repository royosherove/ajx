"""Offline tests: adapters against synthetic fixtures, spec validation, evidence math, rendering,
and a full lifecycle with a fake harness (no LLM calls)."""

import contextlib
import io
import json
import os
import shutil
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))

from ajx import envcheck, evidence, mdlite, plugins, render, runner, spec as specmod  # noqa: E402
from ajx.base import Harness, empty_telemetry, tool_event, text_event, finish_tool  # noqa: E402
from ajx.util import now, read_json, write_json  # noqa: E402

FIX = HERE / "fixtures"

# The suite is offline: identity checks would otherwise run the caller's real `aws` (an inherit
# profile with CLAUDE_CODE_USE_BEDROCK in the caller's env does). Shadow it for every test.
_GUARD = Path(tempfile.mkdtemp(prefix="ajx-test-guard-"))
__import__("atexit").register(shutil.rmtree, _GUARD, True)
(_GUARD / "aws").write_text('#!/bin/sh\necho "$@" >> "$(dirname "$0")/aws.calls"\necho "ajx tests: real aws blocked" >&2\nexit 97\n')
(_GUARD / "aws").chmod(0o755)
os.environ["PATH"] = f"{_GUARD}{os.pathsep}{os.environ.get('PATH', '')}"
for _var in ("CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"):
    os.environ.pop(_var, None)


def _cli():
    import importlib.util
    from importlib.machinery import SourceFileLoader
    path = str(HERE.parent / "bin" / "ajx")
    spec_ = importlib.util.spec_from_file_location("ajx_cli", path, loader=SourceFileLoader("ajx_cli", path))
    cli = importlib.util.module_from_spec(spec_)
    spec_.loader.exec_module(cli)
    return cli


def _run_cli(argv):
    out = io.StringIO()
    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()):
        code = _cli().main(argv)
    return code, out.getvalue()


def _fake_shell(tmp, name="zsh", exports="export AJX_T_SHELLVAR=from-rc"):
    """A shell whose startup file sets a variable, like a ~/.zshenv export, then behaves as sh."""
    path = Path(tmp) / "bin" / name
    path.parent.mkdir(exist_ok=True)
    path.write_text(f'#!/bin/sh\n{exports}\nexec /bin/sh "$@"\n')
    path.chmod(0o755)
    return str(path)


def _ctx(tmp, cell, stage_state, config_dir=""):
    run_dir = Path(tmp) / "run"
    run_dir.mkdir(exist_ok=True)
    return {"spec": {"trial": {"max_budget_usd": None}}, "cell": cell, "run_dir": run_dir, "workspace": Path(tmp),
            "config_dir": config_dir, "state": stage_state, "env": {}, "unset_env": [], "timeout": 10,
            "prompt_text": "x", "runner": plugins.get("runner", "local")(), "auth": None}


class ClaudeAdapterTest(unittest.TestCase):
    def test_normalize_reconciles_tokens_and_tools(self):
        with tempfile.TemporaryDirectory() as tmp:
            cell = {"id": "c", "harness": "claude-code", "config": "clean", "model": "haiku"}
            ctx = _ctx(tmp, cell, {"execute": {"session_id": "00000000-0000-4000-8000-000000000001",
                                               "config_isolated": True}}, config_dir=tmp)
            shutil.copy(FIX / "claude-stream.jsonl", ctx["run_dir"] / "execute.raw.jsonl")
            proj = Path(tmp) / "projects" / "-private-tmp-x"
            proj.mkdir(parents=True)
            shutil.copy(FIX / "claude-transcript.jsonl", proj / "00000000-0000-4000-8000-000000000001.jsonl")
            # raw capture is a run_streaming file: wrap fixture lines
            lines = (FIX / "claude-stream.jsonl").read_text().splitlines()
            (ctx["run_dir"] / "execute.raw.jsonl").write_text(
                "\n".join(json.dumps({"at": now(), "line": l}) for l in lines) + "\n")
            tel = plugins.get("harness", "claude-code")().normalize(ctx, "execute")
        self.assertEqual(tel["usage_status"], "complete")
        self.assertEqual(tel["expected_output_tokens"], 131)
        self.assertEqual(sum(u["output_tokens"] for u in tel["usage"]), 131)
        self.assertEqual(tel["tool_status"], "complete")
        self.assertEqual(tel["expected_tool_calls"], 1)
        tools = [e for e in tel["events"] if e["kind"] == "tool"]
        self.assertEqual(tools[0]["name"], "Bash")
        self.assertIn("echo hi", tools[0]["input"])
        self.assertFalse(tools[0]["is_error"])
        self.assertIn("hi", tools[0]["output"])
        self.assertEqual(tel["final_text"], "It printed `hi`.")
        self.assertIn("haiku", tel["model_ids"][0])
        self.assertEqual(tel["harness_reported"]["cost_note"], "harness-computed estimate; not billed money")


class ClaudeNarrateReconciliationTest(unittest.TestCase):
    def test_forked_narrate_transcript_counts_only_new_messages(self):
        """A --fork-session transcript repeats the task's messages (same ids); narrate telemetry must
        exclude them so reporter tokens never leak into the task's count and vice versa."""
        with tempfile.TemporaryDirectory() as tmp:
            cell = {"id": "c", "harness": "claude-code", "config": "clean", "model": "haiku"}
            rows = [json.loads(l) for l in (FIX / "claude-transcript.jsonl").read_text().splitlines()]
            task_ids = [r["message"]["id"] for r in rows if r.get("type") == "assistant"]
            new = {"type": "assistant", "timestamp": "2026-01-01T00:05:00.000Z", "message": {
                "id": "msg_new_narration", "model": "claude-haiku", "usage": {"output_tokens": 1197},
                "content": [{"type": "text", "text": "# Journey: x\n" + "y" * 300}]}}
            ctx = _ctx(tmp, cell, {"execute": {"session_id": "orig", "config_isolated": True},
                                   "narrate": {"session_id": "forked"}}, config_dir=tmp)
            proj = Path(tmp) / "projects" / "-slug"
            proj.mkdir(parents=True)
            (proj / "forked.jsonl").write_text("\n".join(json.dumps(r) for r in rows + [new]) + "\n")
            result = {"type": "result", "subtype": "success", "result": new["message"]["content"][0]["text"],
                      "usage": {"output_tokens": 1197}, "modelUsage": {}}
            (ctx["run_dir"] / "narrate.raw.jsonl").write_text(json.dumps({"at": now(), "line": json.dumps(result)}) + "\n")
            tel = plugins.get("harness", "claude-code")().normalize(ctx, "narrate", exclude_message_ids=task_ids)
        self.assertEqual([u["id"] for u in tel["usage"]], ["msg_new_narration"])
        self.assertEqual(tel["usage_status"], "complete")
        self.assertEqual(tel["expected_output_tokens"], 1197)
        self.assertEqual([e["kind"] for e in tel["events"]], ["text"])  # the task's tool calls are not re-counted
        self.assertTrue(tel["final_text"].startswith("# Journey"))


class KiroAdapterTest(unittest.TestCase):
    def test_normalize_acp_stream(self):
        with tempfile.TemporaryDirectory() as tmp:
            cell = {"id": "k", "harness": "kiro-cli", "config": "clean", "model": "claude-haiku-4.5"}
            ctx = _ctx(tmp, cell, {"execute": {}})
            lines = (FIX / "kiro-stream.jsonl").read_text().splitlines()
            (ctx["run_dir"] / "execute.raw.jsonl").write_text(
                "\n".join(json.dumps({"at": now(), "line": l}) for l in lines) + "\n")
            tel = plugins.get("harness", "kiro-cli")().normalize(ctx, "execute")
        self.assertEqual(tel["session_id"], "00000000-0000-4000-8000-000000000002")
        self.assertEqual(tel["usage_status"], "unavailable")
        self.assertEqual(tel["tool_status"], "partial")
        tools = [e for e in tel["events"] if e["kind"] == "tool"]
        self.assertEqual(len(tools), 1)
        self.assertEqual(tools[0]["name"], "shell")
        self.assertFalse(tools[0]["is_error"])
        self.assertEqual(tel["final_text"], "It printed `hi` to stdout.")
        self.assertEqual(tel["harness_reported"]["final_text_concatenated"], "It printed `hi` to stdout.")
        self.assertGreater(tel["harness_reported"]["credits"], 0.04)
        self.assertEqual(tel["stop_reason"], "end_turn")


class SpecTest(unittest.TestCase):
    def _write(self, tmp, extra=""):
        (Path(tmp) / "task-prompt.md").write_text("do the thing\n")
        (Path(tmp) / "trial.toml").write_text(f'''
[trial]
id = "t1"
product = "p"
[task]
prompt_file = "task-prompt.md"
[[cells]]
id = "a"
harness = "claude-code"
{extra}
''')
        return Path(tmp) / "trial.toml"

    def test_load_and_plan(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = specmod.load(self._write(tmp, '[[cells]]\nid = "b"\nharness = "kiro-cli"\n[trial.x]\n'))
            self.assertEqual(len(spec["cells"]), 2)
            self.assertEqual(len(spec["task"]["prompt_sha256"]), 64)
            spec["trial"]["repetitions"] = 2
            plan = specmod.run_plan(spec)
            self.assertEqual([r for _, r in plan], [1, 1, 2, 2])

    def test_rejects_unknown_harness_and_bad_auth(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(specmod.SpecError):
                specmod.load(self._write(tmp, '[[cells]]\nid = "b"\nharness = "nope"\n'))
            with self.assertRaises(specmod.SpecError):
                specmod.load(self._write(tmp, '[[cells]]\nid = "b"\nharness = "claude-code"\nauth = "openai-api"\n'))

    def test_declarative_adapter_and_auth_profile(self):
        with tempfile.TemporaryDirectory() as tmp:
            os.environ["AJX_TEST_KEY"] = "sekret"
            spec = specmod.load(self._write(tmp, '''
[adapters.mytool]
execute = ["mytool", "run", "{prompt}"]
[auth.gw]
type = "env"
env = { MY_KEY = "${AJX_TEST_KEY}" }
[[cells]]
id = "d"
harness = "mytool"
auth = "gw"
'''))
            cell = spec["cells"][1]
            auth = specmod.auth_for(spec, cell)
            self.assertEqual(auth.env(), {"MY_KEY": "sekret"})
            self.assertEqual(auth.describe()["env_names"], ["MY_KEY"])
            h = specmod.harness_for(spec, "mytool")
            self.assertEqual(h.name, "mytool")
            self.assertFalse(h.can_resume)


class EvidenceTest(unittest.TestCase):
    def _tel(self):
        tel = empty_telemetry(usage_status="complete", expected_output_tokens=30, tool_status="complete",
                              expected_tool_calls=2)
        t1 = tool_event("a", "Bash", {"command": "ls"}, "2026-01-01T00:00:01Z", "m1")
        finish_tool(t1, "2026-01-01T00:00:03Z", True, "boom")
        t2 = tool_event("b", "Bash", {"command": "ls -a"}, "2026-01-01T00:00:10Z", "m2")
        finish_tool(t2, "2026-01-01T00:00:11Z", False, "ok")
        tel["events"] = [text_event("hello", "2026-01-01T00:00:00Z", "m0"), t1, t2]
        tel["usage"] = [{"id": "m0", "at": "2026-01-01T00:00:00Z", "output_tokens": 10},
                        {"id": "m1", "at": "2026-01-01T00:00:01Z", "output_tokens": 5},
                        {"id": "m2", "at": "2026-01-01T00:00:10Z", "output_tokens": 15}]
        tel["message_ids"] = ["m0", "m1", "m2"]
        return evidence.assign_ids(tel)

    def test_ids_costs_and_retries(self):
        tel = self._tel()
        self.assertEqual([e["eid"] for e in tel["events"]], ["E-001", "E-002", "E-003"])
        asks = [{"id": "ASK-001", "event_refs": ["E-002", "E-003", "E-999"]}, {"id": "ASK-002", "event_refs": ["E-003"]}]
        evidence.ask_costs(asks, tel)
        m = asks[0]["measured"]
        self.assertEqual(m["tool_calls"], 2)
        self.assertEqual(m["failed_tool_calls"], 1)
        self.assertEqual(m["output_tokens"], 20)
        self.assertEqual(m["span_seconds"], 10.0)
        self.assertEqual(m["shared_events"], ["E-003"])
        self.assertEqual(asks[0]["invalid_refs"], ["E-999"])
        state = {"execute": {"started_at": "2026-01-01T00:00:00Z", "stopped_at": "2026-01-01T00:00:20Z", "stop_reason": "exit"}}
        s = evidence.task_summary(state, tel)
        self.assertEqual(s["retries_after_error"], 1)
        self.assertEqual(s["elapsed_seconds"], 20.0)
        self.assertEqual(s["output_tokens"], 30)

    def test_isolation_scan(self):
        tel = self._tel()
        tel["events"].append(evidence.assign_ids({"events": [tool_event("c", "Read", {"file_path": "/tmp/example-home/.claude/skills/ajx/SKILL.md"}, "2026-01-01T00:00:12Z")]})["events"][0])
        spec = {"trial": {"output_dir": "/out", "workspace_root": "/tmp"}, "path": "/trial/trial.toml"}
        flags = evidence.isolation_scan(tel, spec, {"paths": {"workspace": "/tmp/abc123def"}})
        self.assertEqual(len(flags), 1)
        self.assertIn("skills dir", flags[0]["reasons"])

    def test_isolation_scan_ignores_own_paths_containing_ajx(self):
        tel = evidence.assign_ids({"events": [tool_event("c", "shell", {"command": "ls", "cwd": "/tmp/ajx-ws/abc123def"}, "2026-01-01T00:00:12Z")]})
        spec = {"trial": {"output_dir": "/out", "workspace_root": "/tmp/ajx-ws"}, "path": "/trial/trial.toml"}
        self.assertEqual(evidence.isolation_scan(tel, spec, {"paths": {"workspace": "/tmp/ajx-ws/abc123def"}}), [])

    def test_measure_run_reconciles_phases(self):
        with tempfile.TemporaryDirectory() as tmp:
            tel = self._tel()
            write_json(Path(tmp) / "telemetry.execute.json", tel)
            state = {"execute": {"started_at": "2026-01-01T00:00:00Z", "stopped_at": "2026-01-01T00:00:20Z", "stop_reason": "exit"},
                     "narrate": {"started_at": "2026-01-01T00:01:00Z", "stopped_at": "2026-01-01T00:02:00Z",
                                 "reconstruction": True, "output_tokens": 500}}
            write_json(Path(tmp) / "verify.json", {"window": {"start": "2026-01-01T00:00:30Z", "end": "2026-01-01T00:00:40Z"}})
            out = evidence.measure_run({"trial": {}}, {}, tmp, state)
            self.assertIsNone(out["measurement_error"], out["measurement_error"])
            self.assertEqual(out["schema_version"], 1)
            sessions = {s["id"]: s for s in out["phase_measurements"]["sessions"]}
            self.assertEqual(sessions["execute"]["output_tokens"], 30)
            self.assertEqual(sessions["execute"]["tool_calls"], 2)
            self.assertEqual(sessions["execute"]["phases"][0]["scope"], "task")
            self.assertEqual(sessions["narrate"]["usage_status"], "partial")
            self.assertEqual(sessions["verify"]["phases"][0]["scope"], "verification")


class MdliteTest(unittest.TestCase):
    def test_render_escapes_and_tables(self):
        html = mdlite.render("# T\n\n| a | b |\n|---|---|\n| 1 | <x> |\n\n- `c` **b** [l](http://x)\n\n```sh\necho <hi>\n```")
        self.assertIn("<table>", html)
        self.assertIn("&lt;x&gt;", html)
        self.assertIn("<code>c</code>", html)
        self.assertIn('<a href="http://x">l</a>', html)
        self.assertIn("echo &lt;hi&gt;", html)


class FakeHarness(Harness):
    """Deterministic harness: writes a file, emits two events, supports narrate."""
    plugin_name = "fake"
    binary = "true"
    can_resume = True
    clean_supported = True
    verified_live = True

    def plan(self, ctx):
        return {"session_id": "fake-sid"}

    def execute(self, ctx):
        (ctx["workspace"] / "made.txt").write_text("hello")
        proc = self.run(ctx, ["sh", "-c", "echo fake-run; sleep 0.2; echo done"], "execute")
        proc.update(session_id="fake-sid")
        return proc

    def narrate(self, ctx, prompt):
        text = ("# Journey: fake\n\n## Outcome I declared\nI made the file.\n\n## What I asked for first\n- nothing\n\n"
                "## Chronological account\n1. I ran the command [E-001]. It printed done [E-002].\n" + "x" * 200)
        proc = self.run(ctx, ["sh", "-c", f"printf %s {json.dumps(text)}"], "narrate", timeout=10)
        proc.update(session_id="fake-sid", tools_disabled=True)
        return proc

    def normalize(self, ctx, stage, exclude_message_ids=()):
        from ajx.util import read_jsonl
        rows = read_jsonl(ctx["run_dir"] / f"{stage}.raw.jsonl")
        tel = empty_telemetry(session_id="fake-sid", model_ids=["fake-1"], usage_status="complete",
                              expected_output_tokens=7, tool_status="complete", expected_tool_calls=1,
                              timestamp_source="arrival")
        if stage == "execute":
            t = tool_event("t1", "shell", {"command": "echo fake-run"}, rows[0]["at"], "m1")
            finish_tool(t, rows[-1]["at"], False, "done")
            tel["events"] = [t, text_event("I made the file.", rows[-1]["at"], "m2")]
            tel["usage"] = [{"id": "m1", "at": rows[0]["at"], "output_tokens": 3}, {"id": "m2", "at": rows[-1]["at"], "output_tokens": 4}]
            tel["message_ids"] = ["m1", "m2"]
            tel["final_text"] = "I made the file."
            tel["stop_reason"] = "success"
        else:
            tel["final_text"] = "\n".join(r["line"] for r in rows)
            tel["usage_status"], tel["tool_status"] = "unavailable", "unavailable"
        return tel


plugins.register("harness", "fake")(FakeHarness)


class LifecycleTest(unittest.TestCase):
    def test_full_run_without_llm(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            (tmp / "task-prompt.md").write_text("make a file\n")
            (tmp / "trial.toml").write_text(f'''
[trial]
id = "fake-trial"
product = "fakeprod"
workspace_root = "{tmp}/ws"
product_version_cmd = "echo v1.2.3"
[task]
prompt_file = "task-prompt.md"
[[setup]]
run = "echo setup > setup.marker"
[[verify]]
name = "file made"
type = "file_exists"
path = "made.txt"
contains = "hello"
[[verify]]
name = "shell check fails"
run = "test -f nope.txt"
[[teardown]]
run = "echo torn-$AJX_RUN_TOKEN"
[[cells]]
id = "f"
harness = "fake"
''')
            spec = specmod.load(tmp / "trial.toml")
            # stub the extract stage (reporter LLM) with a deterministic register
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)

            def fake_extract():
                tel = read_json(run.run_dir / "telemetry.execute.json")
                asks = [{"id": "ASK-001", "title": "Say more", "requested_behavior": "print progress", "evidence_status": "observed_friction",
                         "how_observed": "I ran it", "consequence": "waited", "event_refs": ["E-001"], "journey_anchors": [],
                         "labels": ["Wait"], "priority_rationale": "one slow call", "modeled_saving": None, "verification": "rerun", "workaround": None}]
                evidence.ask_costs(asks, tel)
                write_json(run.run_dir / "asks.json", {"asks": asks, "journey_errata": [], "strengths": [], "gates": [], "no_obstacles_observed": False, "extraction_status": "ok", "extractor": {}})
                run.state["extract"] = {"started_at": now(), "stopped_at": now(), "output_tokens": 50}
            run.stage_extract = fake_extract
            state = run.go()
            statuses = {k: v["status"] for k, v in state["stages"].items()}
            self.assertTrue(all(v == "done" for v in statuses.values()), statuses)
            rd = run.run_dir
            verify = read_json(rd / "verify.json")
            self.assertEqual((verify["passed"], verify["total"], verify["outcome"]), (1, 2, "partial"))
            self.assertIn("torn-" + state["run_token"], read_json(rd / "teardown.json")[0]["stdout"])
            env = read_json(rd / "environment.json")
            self.assertEqual(env["product_version_before"], "v1.2.3")
            runj = read_json(rd / "run.json")
            self.assertEqual(runj["task_outcome_verified"], "partial")
            self.assertEqual(runj["task_outcome_declared"], "I made the file.")
            self.assertFalse(runj["report_instructions_visible_during_task"])
            self.assertTrue(runj["journey_provenance"].startswith("self-narrated"))
            self.assertEqual(runj["review_status"], "ready")
            self.assertIn("ASK-001", (rd / "asks.md").read_text())
            # the recorded argv never contains the prompt or auth values
            self.assertNotIn("make a file", json.dumps(state["execute"]["argv"]))
            self.assertTrue((rd / "report.html").exists())
            meas = read_json(rd / "measurements.json")
            self.assertIsNone(meas["measurement_error"], meas["measurement_error"])
            self.assertEqual(meas["task_summary"]["output_tokens"], 7)
            # archive moved the workspace under the run dir and nothing is left in workspace_root
            self.assertTrue((rd / "workspace" / "made.txt").exists())
            self.assertEqual([p for p in (tmp / "ws").iterdir()], [])
            # workspace path was unnamed (no cell/trial identifiers)
            self.assertNotIn("fake", Path(state["paths"]["workspace"]).name)
            # matrix report
            render.matrix_report(spec, synthesize=False, log=lambda m: None)
            out = Path(spec["trial"]["output_dir"])
            self.assertTrue((out / "index.html").exists())
            mx = read_json(out / "matrix.json")
            self.assertEqual(mx["runs"][0]["verified"], "partial")
            self.assertEqual(mx["runs"][0]["asks"], 1)

    def test_resume_skips_done_and_runs_pending_teardown(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            (tmp / "task-prompt.md").write_text("x\n")
            (tmp / "trial.toml").write_text(f'''
[trial]
id = "t"
product = "p"
workspace_root = "{tmp}/ws"
[task]
prompt_file = "task-prompt.md"
[[teardown]]
run = "echo td"
[[cells]]
id = "f"
harness = "fake"
''')
            spec = specmod.load(tmp / "trial.toml")
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            run.go(stages=["prepare", "execute"])
            self.assertEqual(run.state["stages"]["execute"]["status"], "done")
            self.assertNotIn("teardown", run.state["stages"])
            run2 = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            run2.go(stages=["verify"])
            self.assertEqual(run2.state["stages"]["teardown"]["status"], "done")
            self.assertEqual(run2.state["stages"]["verify"]["status"], "done")
            first = run2.state["stages"]["verify"]["at"]
            run3 = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            run3.go(stages=["verify"])  # explicit post-processing stage is redone, not skipped
            self.assertNotEqual(run3.state["stages"]["verify"]["at"], first)
            exec_at = run3.state["stages"]["execute"]["at"]
            run4 = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            run4.go(stages=["execute"])  # a task is never re-run in place
            self.assertEqual(run4.state["stages"]["execute"]["at"], exec_at)


class FailurePathTest(unittest.TestCase):
    def _trial(self, tmp, cells_extra="", setup=""):
        (tmp / "task-prompt.md").write_text("x\n")
        (tmp / "trial.toml").write_text(f"""
[trial]
id = "t"
product = "p"
workspace_root = "{tmp}/ws"
[task]
prompt_file = "task-prompt.md"
{setup}
[[verify]]
name = "file made"
type = "file_exists"
path = "made.txt"
[[cells]]
id = "f"
harness = "fake"
{cells_extra}
""")
        return specmod.load(tmp / "trial.toml")

    def test_interrupted_execute_keeps_evidence_and_produces_artifacts_without_llm(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            spec = self._trial(tmp)
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            run.go(stages=["prepare"])
            # simulate ajx dying mid-execute: planned session recorded, stage left "running"
            run.state["execute"] = {"session_id": "fake-sid", "started_at": now(), "stopped_at": None, "stop_reason": "running"}
            run.mark("execute", "running")
            (run.run_dir / "execute.raw.jsonl").write_text(json.dumps({"at": now(), "line": "partial"}) + "\n")
            run2 = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            run2.stage_extract = lambda: write_json(run2.run_dir / "asks.json", {"asks": [], "journey_errata": [], "strengths": [], "gates": [], "no_obstacles_observed": True, "extraction_status": "ok", "extractor": {}})
            state = run2.go()
            self.assertEqual(state["stages"]["execute"]["status"], "interrupted")
            self.assertEqual(state["execute"]["stop_reason"], "interrupted")
            self.assertEqual(state["execute"]["session_id"], "fake-sid")
            for st in ("verify", "teardown", "normalize", "narrate", "measure", "render", "archive"):
                self.assertEqual(state["stages"][st]["status"], "done", st)
            runj = read_json(run2.run_dir / "run.json")
            self.assertEqual(runj["task_stop_reason"], "interrupted")
            self.assertEqual(runj["task_outcome_verified"], "failed")  # made.txt never created
            self.assertTrue((run2.run_dir / "journey.md").exists())
            self.assertIn("interrupted", " ".join(runj["limitations"]))

    def test_execute_error_yields_stub_artifacts_and_incomplete_status(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            spec = self._trial(tmp)
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            def boom(ctx):
                raise RuntimeError("binary exploded")
            run.harness.execute = boom
            state = run.go()
            self.assertEqual(state["stages"]["execute"]["status"], "error")
            self.assertEqual(state["stages"]["teardown"]["status"], "done")
            self.assertEqual(state["journey_provenance"], "unavailable")
            asks = read_json(run.run_dir / "asks.json")
            self.assertTrue(asks["extraction_status"].startswith("skipped"))
            runj = read_json(run.run_dir / "run.json")
            self.assertEqual(runj["review_status"], "incomplete")
            self.assertIn("Extraction did not complete", (run.run_dir / "asks.md").read_text())
            self.assertEqual([p for p in (tmp / "ws").iterdir()], [])  # archived despite the failure

    def test_prepare_failure_archives_workspace(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            spec = self._trial(tmp, setup='[[setup]]\nrun = "exit 3"\n')
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            state = run.go()
            self.assertEqual(state["stages"]["prepare"]["status"], "error")
            self.assertEqual([p for p in (tmp / "ws").iterdir()], [])
            self.assertTrue((run.run_dir / "workspace").exists())
            self.assertEqual(read_json(run.run_dir / "setup.json")[0]["exit_code"], 3)


class SafetyTest(unittest.TestCase):
    def test_safe_argv_redacts_auth_values_and_prompt(self):
        from ajx.base import Auth
        auth = Auth({"env": {"MY_KEY": "supersecretvalue"}, "args": ["--token", "tok-abcdefgh"]})
        h = FakeHarness()
        argv = ["tool", "--token", "tok-abcdefgh", "--key", "supersecretvalue", "the prompt text", "-v"]
        out = h.safe_argv({"auth": auth}, argv, hide=("the prompt text",))
        self.assertEqual(out, ["tool", "--token", "<redacted>", "--key", "<redacted>", "<prompt>", "-v"])

    def test_fill_leaves_literal_braces(self):
        from ajx.util import fill
        self.assertEqual(fill('{"json": true} {model} {nope}', {"model": "m1"}), '{"json": true} m1 {nope}')

    def test_auth_flags_unresolved_env_reference(self):
        from ajx.base import Auth
        os.environ.pop("AJX_DEFINITELY_UNSET", None)
        auth = Auth({"env": {"K": "${AJX_DEFINITELY_UNSET}"}})
        self.assertTrue(any("AJX_DEFINITELY_UNSET" in p for p in auth.problems()))

    def test_isolation_scan_scrubs_own_paths_before_needles(self):
        trial_dir = "/trials/t1"
        tel = evidence.assign_ids({"events": [tool_event("c", "shell", {"command": "ls", "cwd": f"{trial_dir}/ws/abc123def"}, "2026-01-01T00:00:12Z")]})
        spec = {"trial": {"output_dir": f"{trial_dir}/ajx-reports", "workspace_root": f"{trial_dir}/ws"}, "path": f"{trial_dir}/trial.toml"}
        self.assertEqual(evidence.isolation_scan(tel, spec, {"paths": {"workspace": f"{trial_dir}/ws/abc123def"}}), [])


class RenderTest(unittest.TestCase):
    def test_asks_table_escapes_pipes_and_reports_extraction_failure(self):
        run = {"product": {"name": "p", "version": "1"}, "run_id": "r1", "agent": {"harness_name": "h", "harness_version": "1", "model_id": "m", "model_requested": "m"},
               "task_outcome_verified": "failed", "verification_scope": "0/1", "journey_provenance": "self", "evidence": {"usage_coverage": "x", "tool_call_coverage": "y", "timestamp_coverage": "z"}, "limitations": []}
        asks = {"asks": [{"id": "ASK-001", "title": "a | b", "evidence_status": "verified_defect", "labels": [], "event_refs": ["E-001"], "journey_anchors": [],
                          "priority_rationale": "x|y", "how_observed": "o", "requested_behavior": "r", "consequence": "c", "verification": "v",
                          "measured": {"tool_calls": 1, "failed_tool_calls": 0, "span_seconds": 1.0, "output_tokens": None, "tool_seconds": 1.0, "shared_events": [], "basis": "b"}}],
                "extraction_status": "ok", "strengths": [], "gates": [], "journey_errata": [], "extractor": {}}
        md = render.asks_markdown(run, asks, {"events": []})
        self.assertIn("a \\| b", md)
        self.assertIn("x\\|y", md)
        md2 = render.asks_markdown(run, {**asks, "asks": [], "extraction_status": "failed: timeout"}, {"events": []})
        self.assertIn("Extraction did not complete", md2)
        self.assertNotIn("No obstacles observed", md2)

    def test_digest_is_bounded_for_long_tasks(self):
        events = []
        for i in range(500):
            e = tool_event(f"t{i}", "Bash", {"command": f"cmd {i}"}, f"2026-01-01T00:{i // 60:02d}:{i % 60:02d}Z", f"m{i}")
            finish_tool(e, f"2026-01-01T00:{i // 60:02d}:{i % 60:02d}Z", i == 250, "out")
            events.append(e)
        tel = evidence.assign_ids(empty_telemetry(events=events))
        shown = evidence.digest_events(tel["events"])
        self.assertLess(len(shown), 300)
        self.assertIn("E-251", [e.get("eid") for e in shown])  # the failed call in the middle survives
        self.assertEqual(sum(1 for e in shown if e.get("kind") == "omitted"), 1)
        digest = evidence.build_digest({"trial": {}}, {"harness": "fake", "config": "clean"}, {"execute": {"started_at": "2026-01-01T00:00:00Z", "stopped_at": "2026-01-01T00:09:00Z"}}, tel, {}, {})
        self.assertIn("routine events omitted", digest)


class ReviewRegressionTest(unittest.TestCase):
    """Cases from the independent review of the first build."""

    def test_gap_measured_from_end_of_running_tool(self):
        t = tool_event("a", "Bash", {"command": "npm install"}, "2026-01-01T00:00:00Z", "m1")
        finish_tool(t, "2026-01-01T00:01:30Z", False, "ok")  # ran 90 s
        tel = evidence.assign_ids(empty_telemetry(events=[t, text_event("done", "2026-01-01T00:01:31Z", "m2")]))
        s = evidence.task_summary({"execute": {}}, tel)
        self.assertEqual(s["gaps_over_60s"], [])  # the agent was not idle while the tool ran

    def test_narrator_digest_omits_verification(self):
        tel = evidence.assign_ids(empty_telemetry(events=[], final_text="```sh\necho hi\n```"))
        verify = {"checks": [{"passed": False, "name": "x", "type": "shell", "detail": "exit=1"}], "passed": 0, "total": 1, "outcome": "failed"}
        full = evidence.build_digest({"trial": {}}, {"harness": "h", "config": "clean"}, {"execute": {}}, tel, verify, {})
        narr = evidence.build_digest({"trial": {}}, {"harness": "h", "config": "clean"}, {"execute": {}}, tel, verify, {}, include_verification=False)
        self.assertIn("FAIL: x", full)
        self.assertNotIn("FAIL", narr)
        self.assertNotIn("verification", narr.lower())
        self.assertIn("````text", full)  # fence longer than the agent's own ``` block

    def test_errata_block_is_replaced_not_appended(self):
        from ajx import reporter
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / "journey.md").write_text("# Journey\n\nbody\n")
            reporter.write_errata(tmp, [{"journey_claim": "a", "evidence": "b", "event_refs": ["E-001"]}])
            reporter.write_errata(tmp, [{"journey_claim": "c", "evidence": "d", "event_refs": []}])
            text = (Path(tmp) / "journey.md").read_text()
            self.assertEqual(text.count("Corrections added"), 1)
            self.assertIn("c -> evidence: d", text)
            self.assertNotIn("a -> evidence: b", text)
            self.assertTrue(text.startswith("# Journey\n\nbody"))

    def test_isolation_scan_ignores_tempfile_dirs_under_root(self):
        tel = evidence.assign_ids({"events": [tool_event("c", "Bash", {"command": "ls /tmp/tmpk3j2h1ab /tmp/0123456789"}, "2026-01-01T00:00:12Z")]})
        spec = {"trial": {"output_dir": "/out", "workspace_root": "/tmp"}, "path": "/trial/trial.toml"}
        flags = evidence.isolation_scan(tel, spec, {"paths": {"workspace": "/tmp/abcdef0123"}})
        self.assertEqual(len(flags), 1)
        self.assertEqual(flags[0]["reasons"], ["another run's workspace root entry"])  # only the 10-hex sibling

    def test_mdlite_neutralizes_javascript_links(self):
        html = mdlite.render("[x](javascript:alert(1)) [y](data:text/html,zz)")
        self.assertNotIn("javascript:", html)
        self.assertNotIn("data:", html)
        self.assertIn('href="#"', html)

    def test_lock_blocks_second_runner_and_tolerates_stale_lock(self):
        with tempfile.TemporaryDirectory() as tmp:
            lock = runner.acquire_lock(tmp)
            with self.assertRaises(runner.LockedError):
                runner.acquire_lock(tmp)
            runner.release_lock(lock)
            write_json(Path(tmp) / ".lock", {"pid": 2 ** 22 + 12345, "host": __import__("socket").gethostname(), "at": "x"})
            runner.release_lock(runner.acquire_lock(tmp))  # dead pid: stale lock is replaced

    def test_cli_main_dry_run_and_unknown_stage(self):
        import importlib.util
        from importlib.machinery import SourceFileLoader
        path = str(HERE.parent / "bin" / "ajx")
        spec_ = importlib.util.spec_from_file_location("ajx_cli", path, loader=SourceFileLoader("ajx_cli", path))
        cli = importlib.util.module_from_spec(spec_)
        spec_.loader.exec_module(cli)
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            (tmp / "task-prompt.md").write_text("x\n")
            (tmp / "trial.toml").write_text(f'[trial]\nid = "t"\nproduct = "p"\nworkspace_root = "{tmp}/ws"\n[task]\nprompt_file = "task-prompt.md"\n[[cells]]\nid = "f"\nharness = "fake"\n')
            import contextlib, io
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                self.assertEqual(cli.main(["run", str(tmp / "trial.toml"), "--dry-run"]), 0)
            self.assertIn('"f-r1"', buf.getvalue())
            self.assertTrue((tmp / "ajx-reports" / "p" / "t" / "matrix-plan.json").exists())
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(cli.main(["run", str(tmp / "trial.toml"), "--stages", "rendr"]), 2)
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(cli.main(["validate", str(tmp / "trial.toml")]), 0)
                self.assertEqual(cli.main(["plugins"]), 0)

    def test_timeout_kills_worker_and_records_stop_reason(self):
        from ajx.util import run_streaming
        with tempfile.TemporaryDirectory() as tmp:
            res = run_streaming(["sh", "-c", "echo start; sleep 30; echo never"], tmp, Path(tmp) / "o.jsonl", Path(tmp) / "e.txt", timeout=1)
            self.assertTrue(res["timed_out"])
            self.assertNotEqual(res["exit_code"], 0)
            lines = [json.loads(l)["line"] for l in (Path(tmp) / "o.jsonl").read_text().splitlines()]
            self.assertEqual(lines, ["start"])

    def test_archive_purges_copied_credentials_and_repoints_transcript(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            (tmp / "task-prompt.md").write_text("x\n")
            (tmp / "trial.toml").write_text(f'[trial]\nid = "t"\nproduct = "p"\nworkspace_root = "{tmp}/ws"\n[task]\nprompt_file = "task-prompt.md"\n[[cells]]\nid = "f"\nharness = "fake"\n')
            spec = specmod.load(tmp / "trial.toml")
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            run.go(stages=["prepare", "execute", "verify", "teardown"])
            cfg = Path(run.state["paths"]["config_dir"])
            (cfg / "auth.json").write_text("{\"token\": \"secret\"}")
            (cfg / "projects").mkdir()
            (cfg / "projects" / "s.jsonl").write_text("{}\n")
            write_json(run.run_dir / "telemetry.execute.json", {"events": [], "transcript_path": str(cfg / "projects" / "s.jsonl")})
            run.go(stages=["archive"])
            archived = run.run_dir / "harness-config"
            self.assertFalse((archived / "auth.json").exists())
            self.assertTrue((archived / "projects" / "s.jsonl").exists())
            self.assertEqual(read_json(run.run_dir / "telemetry.execute.json")["transcript_path"], str(archived / "projects" / "s.jsonl"))

    def test_token_variance_excludes_partial_runs(self):
        rows = [{"output_tokens": 100, "usage_status": "complete"}, {"output_tokens": 5, "usage_status": "partial"}]
        self.assertEqual(render._variance(rows, "output_tokens")["n"], 1)
        self.assertEqual(render._variance(rows, "tool_calls"), None)

    def test_parallel_matrix_runs_both_cells(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            (tmp / "task-prompt.md").write_text("x\n")
            (tmp / "trial.toml").write_text(f'[trial]\nid = "t"\nproduct = "p"\nparallel = 2\nworkspace_root = "{tmp}/ws"\n[task]\nprompt_file = "task-prompt.md"\n[[cells]]\nid = "a"\nharness = "fake"\n[[cells]]\nid = "b"\nharness = "fake"\n')
            spec = specmod.load(tmp / "trial.toml")
            orig = runner.Run.stage_extract
            runner.Run.stage_extract = lambda self: write_json(self.run_dir / "asks.json", {"asks": [], "journey_errata": [], "strengths": [], "gates": [], "no_obstacles_observed": True, "extraction_status": "ok", "extractor": {}})
            try:
                runner.run_matrix(spec, log=lambda m: None, synthesize=False)
            finally:
                runner.Run.stage_extract = orig
            mx = read_json(tmp / "ajx-reports" / "p" / "t" / "matrix.json")
            self.assertEqual(sorted(r["run_id"] for r in mx["runs"]), ["a-r1", "b-r1"])
            self.assertFalse(mx["plan"]["wall_clock_comparable"])
            self.assertFalse((tmp / "ajx-reports" / "p" / "t" / ".lock").exists())


def _trial(tmp, body, cells='[[cells]]\nid = "f"\nharness = "fake"\n'):
    tmp = Path(tmp)
    (tmp / "task-prompt.md").write_text("x\n")
    (tmp / "trial.toml").write_text(f'[trial]\nid = "t"\nproduct = "p"\nworkspace_root = "{tmp}/ws"\n'
                                    f'[task]\nprompt_file = "task-prompt.md"\n{body}\n{cells}')
    return tmp / "trial.toml"


def _stub_extract(run):
    run.stage_extract = lambda: write_json(run.run_dir / "asks.json", {
        "asks": [], "journey_errata": [], "strengths": [], "gates": [], "no_obstacles_observed": True,
        "extraction_status": "ok", "extractor": {}})


class ModelMatrixTest(unittest.TestCase):
    """'claude code with sonnet and opus, codex with example-model-a and example-model-b' -> one cell (and run) per model."""

    def test_models_expand_and_reach_each_harness_argv(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = specmod.load(_trial(tmp, "", '[[cells]]\nid = "cc"\nharness = "claude-code"\nmodels = ["sonnet", "opus"]\n'
                                                '[[cells]]\nid = "codex"\nharness = "codex"\nmodels = ["example-model-a", "example-model-b"]\n'))
        self.assertEqual([(c["id"], c["model"], c["group"]) for c in spec["cells"]],
                         [("cc-sonnet", "sonnet", "cc"), ("cc-opus", "opus", "cc"),
                          ("codex-example-model-a", "example-model-a", "codex"), ("codex-example-model-b", "example-model-b", "codex")])
        self.assertEqual(sorted(c["id"] for c, _ in specmod.run_plan(spec, ["cc"])), ["cc-opus", "cc-sonnet"])
        self.assertEqual([c["id"] for c, _ in specmod.run_plan(spec, ["codex-example-model-b"])], ["codex-example-model-b"])
        for cell in spec["cells"]:
            h = specmod.harness_for(spec, cell["harness"])
            argv = h._common({"cell": cell, "auth": None})
            self.assertEqual(argv[argv.index("--model") + 1], cell["model"])

    def test_models_rejects_ambiguous_or_bad_cells(self):
        bad = ['id = "cc"\nharness = "claude-code"\nmodel = "opus"\nmodels = ["sonnet"]',
               'id = "cc"\nharness = "claude-code"\nmodels = []',
               'id = "cc"\nharness = "claude-code"\nmodels = ["opus", "opus"]',
               'id = "cc"\nharness = "claude-code"\nmodels = ["' + "x" * 60 + '"]']
        for cell in bad:
            with tempfile.TemporaryDirectory() as tmp, self.assertRaises(specmod.SpecError, msg=cell):
                specmod.load(_trial(tmp, "", f"[[cells]]\n{cell}\n"))
        with tempfile.TemporaryDirectory() as tmp, self.assertRaises(specmod.SpecError):
            specmod.load(_trial(tmp, "", '[[cells]]\nid = "cc-opus"\nharness = "claude-code"\n'
                                         '[[cells]]\nid = "cc"\nharness = "claude-code"\nmodels = ["opus"]\n'))

    def test_task_identity_cmd_and_preflight_parse(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = specmod.load(_trial(tmp, 'identity_cmd = ["aws", "sts", "get-caller-identity", "--profile", "my sb"]\n'
                                            '[[preflight]]\nrun = "true"\n'))
            self.assertEqual(spec["task"]["identity_cmd"], "aws sts get-caller-identity --profile 'my sb'")
            self.assertEqual(spec["preflight"][0]["run"], "true")
            with self.assertRaises(specmod.SpecError):
                specmod.load(_trial(tmp, "identity_cmd = 3\n"))


class EnvCheckTest(unittest.TestCase):
    """The three environments a worker's credentials pass through, compared before a run."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.cfg = self.tmp / "claude-config"
        self.cfg.mkdir()
        (self.cfg / "settings.json").write_text(json.dumps({"env": {"AJX_T_SETTINGSVAR": "from-settings",
                                                                    "AJX_T_PROFILE": "from-settings-profile"}}))
        self.shell = _fake_shell(self.tmp)
        cc = plugins.get("harness", "claude-code")
        patches = [mock.patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": str(self.cfg), "CLAUDE_CODE_SHELL": self.shell}),
                   mock.patch.object(cc, "managed_settings", self.tmp / "no-managed-settings.json")]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(shutil.rmtree, self.tmp, True)

    def _inspect(self, env_block, cell, identities=False):
        spec = specmod.load(_trial(self.tmp, env_block, cell))
        cell = spec["cells"][0]
        auth = specmod.auth_for(spec, cell)
        rn = specmod.runner_for(spec, cell)
        checks, view = envcheck.inspect_cell(spec, cell, specmod.harness_for(spec, cell["harness"]), auth, rn,
                                             envcheck.scratch_workspace(spec, self.tmp),
                                             envcheck.placeholder_ctx(spec, cell, auth, rn, self.tmp), identities=identities)
        return checks, view, auth

    def test_overrides_by_harness_settings_and_shell_startup(self):
        checks, view, auth = self._inspect('[env]\nAJX_T_SETTINGSVAR = "trial"\nAJX_T_SHELLVAR = "trial"\nAJX_T_PLAIN = "trial"\n'
                                           'AJX_T_PROFILE = "trial"\n',
                                           '[[cells]]\nid = "u"\nharness = "claude-code"\nconfig = "user"\n')
        found = {o["name"]: o for o in checks["overrides"]}
        self.assertEqual(sorted(found), ["AJX_T_PROFILE", "AJX_T_SETTINGSVAR", "AJX_T_SHELLVAR"])
        self.assertTrue(found["AJX_T_SETTINGSVAR"]["model_affected"])
        self.assertTrue(found["AJX_T_SETTINGSVAR"]["settings_source"].endswith("settings.json"))
        self.assertEqual(found["AJX_T_SETTINGSVAR"]["tool_shell"], "from-settings")
        self.assertFalse(found["AJX_T_SHELLVAR"]["model_affected"])
        self.assertTrue(found["AJX_T_SHELLVAR"]["shell_startup"])
        self.assertEqual(found["AJX_T_SHELLVAR"]["tool_shell"], "from-rc")
        self.assertEqual(checks["tool_shell"], f"{self.shell} -c")
        text = " ".join(envcheck.warnings(checks, auth, view))
        self.assertIn('[env] sets AJX_T_PROFILE="trial", but the harness applies "from-settings-profile" from harness settings', text)
        self.assertIn("[env] sets AJX_T_SETTINGSVAR=<value hidden>", text)  # values only for allowlisted names
        self.assertNotIn('"from-settings"', text)
        # environment.json form carries names and sources, never values
        self.assertNotIn("from-settings", json.dumps(envcheck.record(checks)))

    def test_isolated_config_dir_ignores_user_settings(self):
        checks, _, _ = self._inspect('[env]\nAJX_T_SETTINGSVAR = "trial"\n[auth.bed]\ntype = "claude-bedrock"\n',
                                     '[[cells]]\nid = "c"\nharness = "claude-code"\nauth = "bed"\n')
        self.assertEqual(checks["harness_settings"], [])
        self.assertEqual(checks["overrides"], [])

    def test_task_env_colliding_with_model_credentials(self):
        env = '[env]\nAWS_PROFILE = "default"\nAWS_REGION = "us-east-1"\nMY_FLAG = "1"\n'
        cases = [('[auth.bed]\ntype = "claude-bedrock"\n', "claude-code", 'auth = "bed"', {}, ["AWS_PROFILE", "AWS_REGION"]),
                 ("", "claude-code", "", {"CLAUDE_CODE_USE_BEDROCK": "1"}, ["AWS_PROFILE", "AWS_REGION"]),
                 ("", "claude-code", "", {"CLAUDE_CODE_USE_BEDROCK": ""}, []),
                 ('[auth.key]\ntype = "anthropic-api"\n', "claude-code", 'auth = "key"', {}, []),
                 ("", "kiro-cli", 'auth = "inherit"', {"CLAUDE_CODE_USE_BEDROCK": "1"}, [])]
        for extra, harness, auth, caller, want in cases:
            with mock.patch.dict(os.environ, caller):
                checks, view, auth_obj = self._inspect(env + extra, f'[[cells]]\nid = "c"\nharness = "{harness}"\n{auth}\n')
            self.assertEqual(checks["collisions"], want, (harness, auth, caller))
            if want:
                self.assertIn("--profile", envcheck.collision_text(want, auth_obj, view))

    def test_task_identity_in_tool_shell_and_check_shell(self):
        checks, _, _ = self._inspect('identity_cmd = "echo acct-$AJX_T_SHELLVAR"\n[env]\nAJX_T_SHELLVAR = "trial"\n',
                                     '[[cells]]\nid = "u"\nharness = "claude-code"\nconfig = "user"\n', identities=True)
        self.assertTrue(checks["task_identity"]["ok"])
        self.assertEqual(checks["task_identity"]["output"], "acct-from-rc")   # what the agent's commands see
        spec = specmod.load(self.tmp / "trial.toml")
        cell = spec["cells"][0]
        auth = specmod.auth_for(spec, cell)
        check_env = envcheck.command_env(specmod.worker_env(spec, cell, auth), auth.unset(), "t")
        self.assertEqual(envcheck.identity_result(spec["task"]["identity_cmd"], self.tmp, check_env)["output"], "acct-trial")
        failing = envcheck.identity_result('echo "An error occurred (NoCredentials): Unable to locate credentials" >&2; exit 253',
                                           self.tmp, check_env)
        self.assertEqual((failing["ok"], failing["exit_code"]), (False, 253))
        self.assertIn("NoCredentials", failing["output"])
        hidden = envcheck.identity_result('echo "Unable to locate credentials" >&2; true', self.tmp, check_env)
        self.assertFalse(hidden["ok"])  # exit 0 is not enough

    def test_auth_error_scanner(self):
        for line in ['aws: [ERROR]: An error occurred (NoCredentials): Unable to locate credentials. You can configure credentials by running "aws login".',
                     "An error occurred (ExpiredToken) when calling the DescribeInstances operation",
                     "The config profile (sandbox) could not be found", "Please run 'az login' to setup account.",
                     "error: You must be logged in to the server (Unauthorized)", "HTTP 401: Bad credentials"]:
            self.assertTrue(envcheck.auth_error_lines(line), line)
        for line in ["running=0", "sg=0 kp=0", "run the login flow later", "Authentication succeeded", "Token refreshed"]:
            self.assertFalse(envcheck.auth_error_lines(line), line)


class DoctorValidateTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)
        cc = plugins.get("harness", "claude-code")  # the reporter harness is always checked; keep it offline
        for p in (mock.patch.object(cc, "available", lambda self: True), mock.patch.object(cc, "version", lambda self: "x"),
                  mock.patch.dict(os.environ, {"SHELL": _fake_shell(self.tmp, "sh")})):
            p.start()
            self.addCleanup(p.stop)

    def _write(self, task_extra):
        return _trial(self.tmp, task_extra + '[auth.m]\ntype = "env"\nidentity_cmd = ["echo", "model-acct"]\n',
                      '[[cells]]\nid = "f"\nharness = "fake"\nauth = "m"\n')

    def test_doctor_separates_model_and_task_identity_and_fails_loudly(self):
        trial = self._write("identity_cmd = 'echo \"An error occurred (NoCredentials)\" >&2; exit 253'\n"
                            '[[preflight]]\nname = "task creds"\nrun = "echo Unable to locate credentials >&2; exit 253"\n')
        code, out = _run_cli(["doctor", str(trial)])
        self.assertEqual(code, 1, out)
        self.assertIn("harness auth identity (model credentials): model-acct", out)
        self.assertIn("task identity (worker tool shell", out)
        self.assertIn("FAILED (exit 253)", out)
        self.assertIn("preflight task creds: FAILED", out)
        self.assertIn("NOT READY", out)

    def test_doctor_ok_shows_task_identity_and_flags_a_different_teardown_identity(self):
        trial = self._write('identity_cmd = "echo acct-$AJX_T_SHELLVAR"\n[[preflight]]\nrun = "true"\n'
                            '[env]\nAJX_T_SHELLVAR = "trial"\n')
        code, out = _run_cli(["doctor", str(trial)])
        self.assertEqual(code, 0, out)
        self.assertIn("task identity (worker tool shell", out)
        self.assertIn("acct-from-rc", out)
        self.assertIn("task identity: acct-trial", out)
        self.assertIn("differs from the worker's tool shell identity", out)
        self.assertIn("preflight true: ok", out)

    def test_validate_warns_without_running_identity(self):
        marker = self.tmp / "identity-ran"
        trial = self._write(f'identity_cmd = "touch {marker}"\n[env]\nAJX_T_SHELLVAR = "trial"\n')
        code, out = _run_cli(["validate", str(trial)])
        self.assertEqual(code, 0, out)
        self.assertIn("WARNING f: [env] sets AJX_T_SHELLVAR=<value hidden>", out)
        self.assertIn("-c (assumed)", out)  # the fake harness's tool shell is not an observed one
        self.assertIn("preflight=0 task identity_cmd=set", out)
        self.assertFalse(marker.exists())


class CredentialLifecycleTest(unittest.TestCase):
    def test_preflight_failure_stops_the_run_before_the_worker(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = specmod.load(_trial(tmp, '[[preflight]]\nname = "creds"\nrun = "echo Unable to locate credentials >&2; exit 253"\n'))
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            state = run.go()
            self.assertEqual(state["stages"]["prepare"]["status"], "error")
            self.assertIn("preflight 'creds' failed (exit 253", state["stages"]["prepare"]["error"])
            self.assertNotIn("execute", state["stages"])
            self.assertFalse((run.run_dir / "execute.raw.jsonl").exists())
            self.assertFalse(read_json(run.run_dir / "preflight.json")[0]["passed"])
            self.assertEqual([p for p in (Path(tmp) / "ws").iterdir()], [])

    def test_rerun_after_failed_preflight_uses_a_fresh_workspace(self):
        with tempfile.TemporaryDirectory() as tmp:
            marker = Path(tmp) / "creds-fixed"
            spec = specmod.load(_trial(tmp, f'[[preflight]]\nrun = "test -f {marker}"\n'))
            first = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            self.assertEqual(first.go()["stages"]["prepare"]["status"], "error")
            self.assertTrue(first.state.get("archived"))
            marker.write_text("ok")  # the user fixes the credentials and reruns
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            _stub_extract(run)
            seen = []
            orig = run.harness.execute
            run.harness.execute = lambda ctx: seen.append(ctx["workspace"]) or orig(ctx)
            state = run.go()
            self.assertEqual(state["stages"]["execute"]["status"], "done")
            self.assertEqual(seen[0], Path(state["paths"]["workspace"]))
            self.assertNotIn(str(run.run_dir), str(seen[0]))  # never inside the report dir
            self.assertEqual(state["stages"]["archive"]["status"], "done")
            self.assertEqual([p for p in (Path(tmp) / "ws").iterdir()], [])  # the retry was archived too
            self.assertTrue(state["execute"]["reaped"])

    def test_teardown_and_verify_credential_errors_behind_exit_zero_are_surfaced(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = specmod.load(_trial(tmp, '[[preflight]]\nrun = "true"\n'
                                            '[[verify]]\nname = "nothing left"\n'
                                            "run = 'echo \"An error occurred (ExpiredToken)\" >&2; true'\n"
                                            "[[teardown]]\nrun = 'set +e; echo \"aws: [ERROR]: An error occurred (NoCredentials): "
                                            "Unable to locate credentials.\" >&2; true'\n"))
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            _stub_extract(run)
            state = run.go()
            self.assertTrue(all(v["status"] == "done" for v in state["stages"].values()), state["stages"])
            self.assertEqual(state["teardown"]["status"], "auth_errors")
            self.assertEqual(read_json(run.run_dir / "teardown.json")[0]["exit_code"], 0)
            self.assertTrue(read_json(run.run_dir / "preflight.json")[0]["passed"])
            verify = read_json(run.run_dir / "verify.json")
            self.assertEqual((verify["outcome"], verify["checks_with_auth_errors"]), ("succeeded", ["nothing left"]))
            runj = read_json(run.run_dir / "run.json")
            self.assertEqual(runj["teardown"]["status"], "auth_errors")
            limits = " ".join(runj["limitations"])
            self.assertIn("cleanup: teardown printed credential or permission errors", limits)
            self.assertIn("verify: 1 check(s) printed credential or permission errors", limits)
            self.assertIn("Cleanup not confirmed", (run.run_dir / "report.html").read_text())
            self.assertIn("credential errors: the result reflects ajx's", (run.run_dir / "digest.md").read_text())
            render.matrix_report(spec, synthesize=False, log=lambda m: None)
            matrix = (Path(spec["trial"]["output_dir"]) / "matrix.md").read_text()
            self.assertIn("Cleanup not confirmed", matrix)
            self.assertIn("Verify checks that hit credential errors", matrix)
            code, out = _run_cli(["status", str(Path(tmp) / "trial.toml")])
            self.assertEqual(code, 1)
            self.assertIn("AUTH", out)
            self.assertIn("CLEANUP NOT CONFIRMED", out)
            self.assertIn("1 check(s) hit credential errors", out)

    def test_failing_teardown_command_is_not_ok(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = specmod.load(_trial(tmp, '[[teardown]]\nrun = "exit 4"\n'))
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            _stub_extract(run)
            state = run.go()
            self.assertEqual(state["stages"]["teardown"]["status"], "done")
            self.assertEqual((state["teardown"]["status"], state["teardown"]["failed"]), ("failed", [0]))
            code, out = _run_cli(["status", str(Path(tmp) / "trial.toml")])
            self.assertEqual(code, 1)
            self.assertIn("FAIL", out)

    def test_teardown_redone_after_archive_clears_the_problem(self):
        with tempfile.TemporaryDirectory() as tmp:
            marker = Path(tmp) / "creds-fixed"
            spec = specmod.load(_trial(tmp, f'[[teardown]]\nrun = "test -f {marker} || {{ echo Unable to locate credentials >&2; exit 253; }}"\n'))
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            _stub_extract(run)
            run.go()
            self.assertEqual((run.state["teardown"]["status"], bool(run.state.get("archived"))), ("auth_errors", True))
            self.assertIn("Cleanup not confirmed", (run.run_dir / "report.html").read_text())
            marker.write_text("ok")
            again = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            again.go(stages=["teardown", "render"])
            self.assertEqual(again.state["teardown"]["status"], "ok")
            self.assertEqual(read_json(again.run_dir / "run.json")["teardown"]["status"], "ok")
            self.assertNotIn("Cleanup not confirmed", (again.run_dir / "report.html").read_text())
            self.assertEqual(_run_cli(["status", str(Path(tmp) / "trial.toml")])[0], 0)

    def test_environment_records_task_identity_and_overrides(self):
        with tempfile.TemporaryDirectory() as tmp, mock.patch.dict(os.environ, {"SHELL": _fake_shell(tmp, "sh")}):
            spec = specmod.load(_trial(tmp, 'identity_cmd = "echo acct-$AJX_T_SHELLVAR"\n[env]\nAJX_T_SHELLVAR = "trial"\n'))
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            _stub_extract(run)
            run.go()
            env = read_json(run.run_dir / "environment.json")
            self.assertEqual((env["task_identity"]["ok"], env["task_identity"]["output"]), (True, "acct-from-rc"))
            self.assertEqual([(o["name"], o["shell_startup"]) for o in env["env_checks"]["overrides"]], [("AJX_T_SHELLVAR", True)])
            runj = read_json(run.run_dir / "run.json")
            self.assertTrue(runj["task_identity"]["ok"])
            self.assertIn("env: [env] AJX_T_SHELLVAR was replaced by the tool shell's startup files", " ".join(runj["limitations"]))
            self.assertIn("Environment notes", (run.run_dir / "digest.md").read_text())
            self.assertNotIn("Environment notes", (run.run_dir / "digest.narrator.md").read_text())


class ExamplesTest(unittest.TestCase):
    def test_example_trials_load_and_cloud_example_keeps_credentials_apart(self):
        for path in sorted((HERE.parent / "examples").rglob("trial.toml")):
            self.assertTrue(specmod.load(path)["cells"], path)
        aws = specmod.load(HERE.parent / "examples" / "aws-cloud" / "trial.toml")
        self.assertEqual([c["id"] for c in aws["cells"]], ["cc-sonnet", "cc-opus"])
        self.assertTrue(aws["preflight"] and aws["teardown"])
        self.assertIn("--profile", aws["task"]["identity_cmd"])
        self.assertFalse([k for k in aws["env"] if k.startswith("AWS_")])
        for cell in aws["cells"]:
            auth = specmod.auth_for(aws, cell)
            view = envcheck.worker_view(aws, specmod.harness_for(aws, "claude-code"), auth,
                                        envcheck.placeholder_ctx(aws, cell, auth, specmod.runner_for(aws, cell), "/nonexistent"))
            self.assertEqual(envcheck.collision_findings(aws, cell, auth, view), [])


class ReviewFindingsTest(unittest.TestCase):
    """Independent review of the credential checks (2026-10-07)."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)
        cc = plugins.get("harness", "claude-code")
        for p in (mock.patch.object(cc, "available", lambda self: True), mock.patch.object(cc, "version", lambda self: "x"),
                  mock.patch.object(cc, "managed_settings", self.tmp / "no-managed-settings.json"),
                  mock.patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": str(self.tmp / "empty-config"),
                                               "CLAUDE_CODE_SHELL": "/bin/sh", "SHELL": "/bin/sh"})):
            p.start()
            self.addCleanup(p.stop)

    def test_secret_values_are_never_printed(self):
        shell = _fake_shell(self.tmp, "sh", 'export ANTHROPIC_CUSTOM_HEADERS="x-api-key: from-rc-SECRET"')
        trial = _trial(self.tmp, '[env]\nANTHROPIC_CUSTOM_HEADERS = "x-api-key: sk-live-SECRET"\n')
        with mock.patch.dict(os.environ, {"SHELL": shell}):
            code, out = _run_cli(["validate", str(trial)])
        self.assertIn("ANTHROPIC_CUSTOM_HEADERS=<value hidden>", out)
        self.assertNotIn("SECRET", out)

    def test_old_style_plugins_keep_working(self):
        plugdir = self.tmp / "plugins"
        plugdir.mkdir()
        (plugdir / "oldstyle.py").write_text(
            "from ajx.plugins import register\nfrom ajx.base import Auth, Harness\n"
            "@register('auth', 'old-style')\nclass OldStyle(Auth):\n    def identity(self):\n        return 'old-style-identity'\n")
        ws_harness = type("WsHarness", (FakeHarness,), {"clean_env": lambda self, ctx: {"AJX_T_WS": str(ctx["workspace"])}})
        plugins.register("harness", "fake-ws")(ws_harness)
        trial = _trial(self.tmp, "", '[[cells]]\nid = "f"\nharness = "fake-ws"\nauth = "old-style"\n')
        code, out = _run_cli(["doctor", str(trial)])
        self.assertEqual(code, 0, out)
        self.assertIn("harness auth identity (model credentials): old-style-identity", out)
        spec = specmod.load(trial)
        run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
        self.assertEqual(run.go(stages=["prepare"])["stages"]["prepare"]["status"], "done")
        self.assertEqual(read_json(run.run_dir / "environment.json")["auth"]["identity"], "old-style-identity")
        broken = type("Broken", (FakeHarness,), {"clean_env": lambda self, ctx: ctx["no-such-key"]})
        plugins.register("harness", "fake-broken")(broken)
        code, out = _run_cli(["validate", str(_trial(self.tmp, "", '[[cells]]\nid = "b"\nharness = "fake-broken"\n'))])
        self.assertEqual(code, 0, out)
        self.assertIn("could not model the worker's env: fake-broken.clean_env failed: KeyError", out)

    def test_bedrock_without_sigv4_is_not_a_failed_identity(self):
        spec = specmod.load(_trial(self.tmp, "", '[[cells]]\nid = "c"\nharness = "claude-code"\n'))
        auth = specmod.auth_for(spec, spec["cells"][0])
        for var in ("AWS_BEARER_TOKEN_BEDROCK", "CLAUDE_CODE_SKIP_BEDROCK_AUTH"):
            note = auth.identity({**os.environ, "CLAUDE_CODE_USE_BEDROCK": "1", var: "1"})
            self.assertTrue(note.startswith("not checked"), note)
            self.assertFalse(envcheck.auth_error_lines(note))

    def test_adapter_launch_env_is_modeled(self):
        trial = _trial(self.tmp, '[env]\nAJX_T_PROFILE = "trial-value"\n[adapters.mytool]\nexecute = ["true"]\n'
                                 'env = { AJX_T_PROFILE = "adapter-value" }\n', '[[cells]]\nid = "d"\nharness = "mytool"\n')
        spec = specmod.load(trial)
        cell, auth, rn = spec["cells"][0], specmod.auth_for(spec, spec["cells"][0]), specmod.runner_for(spec, spec["cells"][0])
        checks, view = envcheck.inspect_cell(spec, cell, specmod.harness_for(spec, "mytool"), auth, rn,
                                             envcheck.scratch_workspace(spec, self.tmp),
                                             envcheck.placeholder_ctx(spec, cell, auth, rn, self.tmp))
        self.assertEqual(view["effective"]["AJX_T_PROFILE"], "adapter-value")
        self.assertIn('[env] sets AJX_T_PROFILE="trial-value", but the harness applies "adapter-value" from the '
                      "harness's launch env", " ".join(envcheck.warnings(checks, auth, view)))

    def test_default_aws_identity_shows_what_a_command_without_profile_acts_as(self):
        fake = self.tmp / "bin" / "aws"
        fake.parent.mkdir()
        fake.write_text('#!/bin/sh\np="$AWS_PROFILE"\nwhile [ $# -gt 0 ]; do [ "$1" = "--profile" ] && p="$2"; shift; done\n'
                        'echo "acct-$p"\n')
        fake.chmod(0o755)
        trial = _trial(self.tmp, 'identity_cmd = "aws sts get-caller-identity --profile task-prof"\n'
                                 '[auth.model]\ntype = "claude-bedrock"\nenv = { AWS_PROFILE = "model-prof", AWS_REGION = "us-east-1" }\n'
                                 'identity_cmd = ["aws", "sts", "get-caller-identity", "--profile", "model-prof"]\n',
                       '[[cells]]\nid = "c"\nharness = "claude-code"\nauth = "model"\n')
        with mock.patch.dict(os.environ, {"PATH": f"{fake.parent}{os.pathsep}{os.environ['PATH']}"}):
            code, out = _run_cli(["doctor", str(trial)])
        self.assertEqual(code, 0, out)
        self.assertIn("task identity (worker tool shell, /bin/sh -c): acct-task-prof", out)
        self.assertIn("default AWS identity in the agent's tool shell (any aws/SDK call without --profile): acct-model-prof", out)
        self.assertIn("omits the task's --profile acts as acct-model-prof (the model's account)", out)

    def test_auth_error_scopes_and_opt_out(self):
        for line in ["npm ERR! code E401", "Error: No valid credential sources found",
                     "CredentialsProviderError: Could not load credentials from any providers",
                     "Unable to resolve AWS account to use. It must be either configured when you define your CDK Stack",
                     "failed to refresh cached credentials, no EC2 IMDS role found",
                     "botocore.exceptions.NoCredentialsError: Unable to locate credentials"]:
            self.assertTrue(envcheck.auth_error_lines(line), line)
        self.assertFalse(envcheck.auth_error_lines("app.py:1:1: E401 multiple imports on one line"))
        spec = specmod.load(_trial(self.tmp, '[[preflight]]\nname = "denied as expected"\nallow_auth_errors = true\n'
                                             "run = 'echo \"An error occurred (AccessDenied)\" >&2; true'\n"
                                             '[[verify]]\nname = "cloudtrail"\nrun = \'echo \"{\\\"errorCode\\\": \\\"AccessDenied\\\"}\"\'\n'
                                             '[[verify]]\nname = "creds"\nrun = \'echo \"Unable to locate credentials\" >&2; true\'\n'))
        run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
        _stub_extract(run)
        state = run.go()
        self.assertEqual(state["stages"]["prepare"]["status"], "done")
        verify = read_json(run.run_dir / "verify.json")
        self.assertEqual(verify["checks_with_auth_errors"], ["creds"])  # stdout content is the product's, not ours

    def test_verify_env_matches_teardown_env(self):
        with mock.patch.dict(os.environ, {"AJX_T_UNSETME": "leak"}):
            spec = specmod.load(_trial(self.tmp, '[[verify]]\nname = "unset applied"\nrun = \'test -z "$AJX_T_UNSETME"\'\n'
                                                 '[auth.u]\ntype = "env"\nunset = ["AJX_T_UNSETME"]\n',
                                       '[[cells]]\nid = "f"\nharness = "fake"\nauth = "u"\n'))
            run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
            run.go(stages=["prepare", "execute", "verify"])
        self.assertEqual(read_json(run.run_dir / "verify.json")["outcome"], "succeeded")

    def test_probe_ignores_locale_coercion_and_flags_path_snapshot(self):
        spec = specmod.load(_trial(self.tmp, '[env]\nLC_CTYPE = "C"\nPATH = "/opt/x/bin:/usr/bin:/bin"\n',
                                   '[[cells]]\nid = "c"\nharness = "claude-code"\n'))
        cell, auth, rn = spec["cells"][0], specmod.auth_for(spec, spec["cells"][0]), specmod.runner_for(spec, spec["cells"][0])
        checks, view = envcheck.inspect_cell(spec, cell, specmod.harness_for(spec, "claude-code"), auth, rn,
                                             envcheck.scratch_workspace(spec, self.tmp),
                                             envcheck.placeholder_ctx(spec, cell, auth, rn, self.tmp))
        self.assertEqual([o["name"] for o in checks["overrides"]], [])
        self.assertEqual(checks["snapshot_restored"], ["PATH"])
        self.assertIn("PATH is reset before every tool command", " ".join(envcheck.warnings(checks, auth, view)))

    def test_spec_rejects_non_shell_preflight_and_reads_model_env_string(self):
        with self.assertRaises(specmod.SpecError):
            specmod.load(_trial(self.tmp, '[[preflight]]\ntype = "http"\nurl = "http://x"\n'))
        from ajx.base import Auth
        self.assertEqual(Auth({"model_env": "MY_*"}).model_vars()[-1], "MY_*")

    def test_offline_guard_shadows_real_aws(self):
        calls = _GUARD / "aws.calls"
        before = calls.read_text() if calls.exists() else ""
        spec = specmod.load(_trial(self.tmp, "", '[[cells]]\nid = "c"\nharness = "claude-code"\n'))
        out = specmod.auth_for(spec, spec["cells"][0]).identity({**os.environ, "CLAUDE_CODE_USE_BEDROCK": "1"})
        self.assertIn("real aws blocked", out)
        self.assertIn("sts get-caller-identity", calls.read_text()[len(before):])


class PortableReporterTest(unittest.TestCase):
    @staticmethod
    def register():
        return {"asks": [], "journey_errata": [], "strengths": [], "gates": [], "no_obstacles_observed": True}

    def test_init_selects_each_common_harness_for_worker_and_reporter(self):
        for harness in ("claude-code", "codex", "kiro-cli"):
            with self.subTest(harness=harness), tempfile.TemporaryDirectory() as tmp:
                code, _ = _run_cli(["init", tmp, "--product", "example", "--harness", harness])
                self.assertEqual(code, 0)
                spec = specmod.load(Path(tmp) / "trial.toml")
                self.assertEqual(spec["cells"][0]["harness"], harness)
                self.assertEqual(spec["reporter"]["harness"], harness)

    def test_default_reporter_follows_first_capable_cell_and_auth(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = specmod.load(_trial(tmp, '[auth.model]\ntype = "openai-api"\n',
                                      '[[cells]]\nid = "a"\nharness = "codex"\nauth = "model"\n'))
            self.assertEqual(spec["reporter"]["harness"], "codex")
            self.assertEqual(spec["reporter"]["auth"], "model")

    def test_reporter_adapters_use_restricted_commands_and_parse_responses(self):
        from ajx import reporter
        schema = json.loads((HERE.parent / "schemas" / "asks.schema.json").read_text())
        data = self.register()
        for name in ("claude-code", "codex", "kiro-cli"):
            with self.subTest(harness=name), tempfile.TemporaryDirectory() as tmp:
                tmp = Path(tmp)
                spec = specmod.load(_trial(tmp, f'[reporter]\nharness = "{name}"\nauth = "inherit"\n'))
                commands = []

                def fake_run(ctx, argv, stage, **kwargs):
                    commands.append(argv)
                    text = json.dumps(data)
                    if name == "claude-code":
                        events = [{"type": "result", "result": text, "structured_output": data, "is_error": False,
                                   "subtype": "success", "usage": {"output_tokens": 12}, "modelUsage": {"fake": {}}}]
                    elif name == "codex":
                        events = [{"type": "item.completed", "item": {"id": "fake", "type": "agent_message", "text": text}},
                                  {"type": "turn.completed", "usage": {"output_tokens": 12}}]
                        Path(argv[argv.index("--output-last-message") + 1]).write_text(text)
                    else:
                        events = [{"type": "runFinished", "data": {"finalText": text, "stopReason": "end_turn"}}]
                    (ctx["run_dir"] / f"{stage}.raw.jsonl").write_text(
                        "".join(json.dumps({"at": "2026-01-01T00:00:00Z", "line": json.dumps(event)}) + "\n" for event in events))
                    return {"exit_code": 0, "timed_out": False}

                with mock.patch.object(plugins.get("harness", name), "run", side_effect=fake_run):
                    result = reporter._reporter_call(spec, tmp, "extract", "Synthetic evidence only.", schema)
                self.assertEqual(result["structured"], data)
                self.assertFalse(result["proc"]["is_error"])
                argv = commands[0]
                self.assertNotIn("--trust-all-tools", argv)
                self.assertNotIn("--dangerously-bypass-approvals-and-sandbox", argv)
                if name == "claude-code":
                    self.assertEqual(argv[argv.index("--tools") + 1], "")
                elif name == "codex":
                    self.assertEqual(argv[argv.index("--sandbox") + 1], "read-only")
                    self.assertIn("--output-schema", argv)
                else:
                    self.assertIn("--trust-tools=", argv)
                    self.assertNotIn("Synthetic evidence only.", str(result["proc"].get("argv")))

    def test_reporter_cleans_up_on_exception_and_rejects_bad_schema(self):
        from ajx import reporter
        schema = json.loads((HERE.parent / "schemas" / "asks.schema.json").read_text())
        with tempfile.TemporaryDirectory() as tmp:
            spec = specmod.load(_trial(tmp, ""))
            auth = specmod.auth_for(spec, {"id": "reporter", "harness": "claude-code"})
            with mock.patch.object(specmod, "auth_for", return_value=auth), \
                 mock.patch.object(auth, "prepare") as prepare, mock.patch.object(auth, "cleanup") as cleanup, \
                 mock.patch.object(plugins.get("harness", "claude-code"), "report", side_effect=RuntimeError("fake failure")):
                with self.assertRaises(RuntimeError):
                    reporter._reporter_call(spec, tmp, "extract", "x", schema)
                prepare.assert_called_once()
                cleanup.assert_called_once()
            bad = {**self.register(), "gates": [{"trigger": "fake", "resolved": "yes"}]}
            with mock.patch.object(plugins.get("harness", "claude-code"), "report", return_value={
                "proc": {"exit_code": 0}, "structured": bad, "text": json.dumps(bad)}):
                result = reporter._reporter_call(spec, tmp, "extract", "x", schema)
                self.assertTrue(result["proc"]["is_error"])
                self.assertIsNone(result["structured"])

    def test_failed_reextraction_invalidates_previous_register(self):
        from ajx import reporter
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            spec = specmod.load(_trial(tmp, ""))
            (tmp / "journey.md").write_text("# Synthetic journey")
            (tmp / "digest.md").write_text("Synthetic evidence")
            write_json(tmp / "asks.json", {**self.register(), "extraction_status": "ok"})
            with mock.patch.object(reporter, "_reporter_call", side_effect=RuntimeError("fake failure")):
                with self.assertRaises(RuntimeError):
                    reporter.extract(spec, tmp)
            self.assertIn("failed:", read_json(tmp / "asks.json")["extraction_status"])
            self.assertEqual(render.review_status(tmp, {"journey_provenance": "self"}), "incomplete")


class PublicReportViewTest(unittest.TestCase):
    def test_example_generates_complete_linked_views_without_an_agent(self):
        from ajx import demo, report_ui
        with tempfile.TemporaryDirectory() as tmp, \
             mock.patch.object(Harness, "run", side_effect=AssertionError("No agent may run")):
            out = demo.generate(Path(tmp) / "example")
            run = out / "runs" / "baseline-codex-r1"
            report = (run / "report.html").read_text()
            journey = (run / "pretty-journey.html").read_text()
            matrix = (out / "index.html").read_text()
            self.assertLess(report.index('id="asks"'), report.index('id="verification"'))
            self.assertIn('id="ASK-001"', report)
            self.assertIn('href="pretty-journey.html#E-003"', report)
            self.assertIn('id="E-003"', journey)
            self.assertIn("<svg", journey)
            self.assertIn("Table of happenings", journey)
            for name, (icon, _) in report_ui.LABELS.items():
                self.assertIn(icon + " " + name, journey)
            for row in read_json(out / "matrix.json")["runs"]:
                self.assertIn(row["run_id"], matrix)
                self.assertEqual(row["asks"], len(row["ask_register"]))
                for ask in row["ask_register"]:
                    self.assertIn(ask["title"], matrix)
            self.assertIn("Fictional example.", report)
            self.assertNotRegex(report + journey + matrix, r'<(?:script|link)[^>]+(?:src|href)="https?://')
            self.assertEqual(demo.generate(out), out)  # refresh only its own example

    def test_example_refuses_an_existing_unrelated_directory(self):
        from ajx.demo import generate
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "keep.txt"
            path.write_text("existing work")
            with self.assertRaises(ValueError):
                generate(tmp)
            self.assertEqual(path.read_text(), "existing work")

    def test_annotations_are_cited_and_narrative_takes_precedence(self):
        from ajx import report_ui
        events = [{"eid": "E-001"}, {"eid": "E-002"}, {"eid": "E-003"}]
        markdown = "# J\n\n## Chronological account\n\n1. 🔀 Fork. I chose a path [E-001].\n\n2. I continued [E-002].\n"
        data = {"asks": [{"id": "ASK-001", "event_refs": ["E-001", "E-002", "E-999"], "labels": ["Wait"]}],
                "strengths": [{"event_refs": ["E-003"]}]}
        marks = report_ui.annotations(markdown, data, events)
        self.assertEqual(marks["E-001"]["labels"], ["Fork"])
        self.assertEqual(marks["E-002"]["labels"], ["Wait"])
        self.assertEqual(marks["E-003"]["labels"], ["Delight"])
        self.assertNotIn("E-999", marks)

    def test_untrusted_content_and_missing_evidence_stay_safe_and_explicit(self):
        from ajx import report_ui
        attack = '<script>alert("fake")</script>'
        ask = {"id": "ASK-001", "title": attack, "event_refs": ['E-001" onclick="x'], "measured": {"span_seconds": None}}
        rendered = report_ui.ask_card(ask, 0)
        self.assertNotIn("<script>", rendered)
        self.assertNotIn('onclick="x"', rendered)
        self.assertIn("&lt;script&gt;", rendered)
        self.assertIn("Unavailable", rendered)
        self.assertEqual(report_ui.number(0.003), "0.003")
        self.assertIn("incomplete", report_ui.empty_register({"extraction_status": "failed"}))
        for url in ("javascript:alert", "data:text/html,x", "vbscript:fake", "http://[bad"):
            self.assertIn('href="#"', mdlite.inline(f"[unsafe]({url})"))


if __name__ == "__main__":
    unittest.main()
