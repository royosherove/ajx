"""Offline tests for the Claude Code stream adapter: sessions that emit several `result` events
(self-resumed / scheduled continuations), reporter result folding, and the live progress hook.
Every stream and transcript here is synthetic; nothing launches a real `claude`."""

import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))

from ajx import plugins  # noqa: E402
from ajx.base import Harness  # noqa: E402
from ajx.builtin import claude_code  # noqa: E402

FIX = HERE / "fixtures"
SID = "00000000-0000-4000-8000-000000000011"
MODEL = "claude-haiku-4-5"
MULTI_RESULT_NOTE = "result events in one session"


def _ctx(tmp, stage_state, **extra):
    run_dir = Path(tmp) / "run"
    run_dir.mkdir(exist_ok=True)
    cell = {"id": "c", "harness": "claude-code", "config": "clean", "model": "haiku"}
    return {"spec": {"trial": {"max_budget_usd": None}}, "cell": cell, "run_dir": run_dir, "workspace": Path(tmp),
            "config_dir": tmp, "state": stage_state, "env": {}, "unset_env": [], "timeout": 10,
            "prompt_text": "x", "runner": plugins.get("runner", "local")(), "auth": None, **extra}


def _execute_ctx(tmp):
    return _ctx(tmp, {"execute": {"session_id": SID, "config_isolated": True}})


def _write_raw(path, events, trailing=None):
    """A run_streaming capture: one {"at", "line"} record per stream-json line (plus an optional
    unterminated tail, as seen while the worker is still writing)."""
    lines = [json.dumps({"at": f"2026-01-01T00:00:{i:02d}.000Z", "line": json.dumps(ev)})
             for i, ev in enumerate(events)]
    Path(path).write_text("\n".join(lines) + "\n" + (trailing or ""), encoding="utf-8")


def _write_transcript(tmp, rows, sid=SID):
    proj = Path(tmp) / "projects" / "-synthetic-workspace"
    proj.mkdir(parents=True, exist_ok=True)
    (proj / f"{sid}.jsonl").write_text("\n".join(json.dumps(r) for r in rows) + "\n", encoding="utf-8")


def _init():
    return {"type": "system", "subtype": "init", "session_id": SID, "cwd": "/tmp/synthetic", "model": MODEL,
            "claude_code_version": "0.0.0-fixture", "permissionMode": "bypassPermissions", "apiKeySource": "none",
            "slash_commands": [], "mcp_servers": []}


def _assistant(mid, block, ts, output_tokens, parent=None):
    ev = {"type": "assistant", "session_id": SID, "timestamp": ts,
          "message": {"id": mid, "model": MODEL, "role": "assistant", "usage": {"output_tokens": output_tokens},
                      "content": [block]}}
    if parent:
        ev["parent_tool_use_id"] = parent
    return ev


def _tool_use(tid, command):
    return {"type": "tool_use", "id": tid, "name": "Bash", "input": {"command": command}}


def _text(text):
    return {"type": "text", "text": text}


def _tool_result(tid, ts, content="ok"):
    return {"type": "user", "session_id": SID, "timestamp": ts,
            "message": {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tid,
                                                     "content": content, "is_error": False}]}}


def _result(num_turns, duration_ms, cost, output_tokens, model_cost=None, text="segment done", spawned=0,
            subtype="success", model=MODEL):
    return {"type": "result", "subtype": subtype, "session_id": SID, "result": text, "is_error": False,
            "num_turns": num_turns, "duration_ms": duration_ms, "total_cost_usd": cost,
            "usage": {"output_tokens": output_tokens},
            "modelUsage": {model: {"inputTokens": 100, "outputTokens": output_tokens, "cacheReadInputTokens": 50,
                                   "costUSD": cost if model_cost is None else model_cost, "contextWindow": 200000}},
            "terminal_reason": "completed", "subagent_stats": {"spawned": spawned}}


# Three segments of one session: the agent scheduled its own continuation twice. Per-segment usage
# is 10, 20 and 5 output tokens; total_cost_usd is a running total (0.10 -> 0.25 -> 0.30).
T = [f"2026-01-01T00:00:{i:02d}.000Z" for i in range(20)]
MULTI_STREAM = [
    _init(),
    _assistant("msg-a1", _tool_use("tool-a1", "echo one"), T[1], 6), _tool_result("tool-a1", T[2], "one"),
    _assistant("msg-a2", _text("First segment."), T[3], 4),
    _result(2, 1000, 0.10, 10, text="First segment."),
    _assistant("msg-b1", _tool_use("tool-b1", "echo two"), T[5], 20), _tool_result("tool-b1", T[6], "two"),
    _result(3, 2000, 0.25, 20, text="Second segment."),
    _assistant("msg-c1", _text("All done."), T[8], 5),
    _result(1, 500, 0.30, 5, text="All done."),
]
MULTI_TRANSCRIPT = [
    {"type": "user", "sessionId": SID, "timestamp": T[0], "message": {"role": "user", "content": "Run the task."}},
    _assistant("msg-a1", _tool_use("tool-a1", "echo one"), T[1], 6), _tool_result("tool-a1", T[2], "one"),
    _assistant("msg-a2", _text("First segment."), T[3], 4),
    _assistant("msg-b1", _tool_use("tool-b1", "echo two"), T[5], 20), _tool_result("tool-b1", T[6], "two"),
    _assistant("msg-c1", _text("All done."), T[8], 5),
]


class MultiResultNormalizeTest(unittest.TestCase):
    def _normalize(self, tmp, stream, transcript=None):
        ctx = _execute_ctx(tmp)
        _write_raw(ctx["run_dir"] / "execute.raw.jsonl", stream)
        if transcript is not None:
            _write_transcript(tmp, transcript)
        return plugins.get("harness", "claude-code")().normalize(ctx, "execute")

    def test_segments_are_summed_and_cost_is_the_running_total(self):
        with tempfile.TemporaryDirectory() as tmp:
            tel = self._normalize(tmp, MULTI_STREAM, MULTI_TRANSCRIPT)
        hr = tel["harness_reported"]
        self.assertEqual(hr["result_events"], 3)
        self.assertEqual(hr["num_turns"], 6)
        self.assertEqual(hr["duration_ms"], 3500)
        self.assertEqual(hr["cost_estimate_usd"], 0.30)
        self.assertEqual(hr["subagents_spawned"], 0)
        self.assertEqual(hr["model_usage"], {MODEL: {"inputTokens": 300, "outputTokens": 35, "cacheReadInputTokens": 150,
                                                     "costUSD": 0.30, "contextWindow": 200000}})
        self.assertEqual(tel["final_text"], "All done.")
        self.assertEqual(tel["stop_reason"], "success")
        self.assertEqual(hr["result_subtype"], "success")
        # Reconciliation: the transcript's 35 output tokens equal the per-segment usage summed.
        self.assertEqual(tel["usage_status"], "complete")
        self.assertEqual(tel["expected_output_tokens"], 35)
        self.assertEqual(sum(u["output_tokens"] for u in tel["usage"]), 35)
        self.assertEqual(tel["tool_status"], "complete")
        self.assertEqual(tel["expected_tool_calls"], 2)
        notes = [l for l in tel["limitations"] if MULTI_RESULT_NOTE in l]
        self.assertEqual(len(notes), 1)
        self.assertIn("execute: 3 result events in one session (self-resumed/scheduled continuation)", notes[0])
        self.assertIn("turns, duration and usage summed across segments", notes[0])
        self.assertIn("cost taken as the cumulative session total", notes[0])
        self.assertFalse([l for l in tel["limitations"] if "!= stream result" in l])

    def test_non_cumulative_cost_falls_back_to_the_sum_with_a_limitation(self):
        stream = [_init(), _result(2, 1000, 0.20, 10), _result(1, 500, 0.15, 5, text="done")]
        with tempfile.TemporaryDirectory() as tmp:
            tel = self._normalize(tmp, stream)   # no transcript needed: harness_reported is stream-only
        hr = tel["harness_reported"]
        self.assertAlmostEqual(hr["cost_estimate_usd"], 0.35)
        self.assertAlmostEqual(hr["model_usage"][MODEL]["costUSD"], 0.35)
        self.assertEqual(hr["result_events"], 2)
        notes = [l for l in tel["limitations"] if MULTI_RESULT_NOTE in l]
        self.assertEqual(len(notes), 1)
        self.assertIn("summed from non-cumulative segments", notes[0])
        self.assertIn("total_cost_usd and modelUsage costUSD decreased between segments", notes[0])
        self.assertNotIn("cumulative session total", notes[0])

    def test_single_result_session_is_unchanged(self):
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _ctx(tmp, {"execute": {"session_id": "00000000-0000-4000-8000-000000000001",
                                         "config_isolated": True}})
            lines = (FIX / "claude-stream.jsonl").read_text().splitlines()
            (ctx["run_dir"] / "execute.raw.jsonl").write_text(
                "\n".join(json.dumps({"at": T[i], "line": l}) for i, l in enumerate(lines)) + "\n")
            _write_transcript(tmp, [json.loads(l) for l in (FIX / "claude-transcript.jsonl").read_text().splitlines()],
                              sid="00000000-0000-4000-8000-000000000001")
            tel = plugins.get("harness", "claude-code")().normalize(ctx, "execute")
        hr = tel["harness_reported"]
        self.assertEqual(hr["result_events"], 1)
        self.assertEqual((hr["num_turns"], hr["duration_ms"], hr["cost_estimate_usd"]), (2, 4000, 0.025))
        self.assertEqual(hr["model_usage"], {"claude-haiku-4-5": {"outputTokens": 131}})   # kept verbatim, not merged
        self.assertEqual(hr["subagents_spawned"], 0)
        self.assertEqual(tel["usage_status"], "complete")
        self.assertEqual(tel["expected_output_tokens"], 131)
        self.assertEqual(tel["final_text"], "It printed `hi`.")
        self.assertFalse([l for l in tel["limitations"] if MULTI_RESULT_NOTE in l])

    def test_last_result_decides_text_subtype_and_error_state(self):
        stream = [_init(), _result(2, 1000, 0.10, 10, text="fine"),
                  {**_result(1, 500, 0.12, 3, text="", subtype="error_during_execution"), "is_error": True}]
        with tempfile.TemporaryDirectory() as tmp:
            tel = self._normalize(tmp, stream)
        self.assertEqual(tel["stop_reason"], "error_during_execution")
        self.assertEqual(tel["final_text"], "")
        self.assertTrue(tel["harness_reported"]["is_error"])
        self.assertEqual(tel["harness_reported"]["num_turns"], 3)

    def test_subagents_spawned_sums_across_segments(self):
        stream = [_init(), _result(2, 1000, 0.10, 10, spawned=1), _result(1, 500, 0.12, 3, spawned=2)]
        with tempfile.TemporaryDirectory() as tmp:
            tel = self._normalize(tmp, stream)
        self.assertEqual(tel["harness_reported"]["subagents_spawned"], 3)


class ResultFoldingHelpersTest(unittest.TestCase):
    def test_running_total_takes_last_when_monotonic_else_sums(self):
        self.assertEqual(claude_code._running_total([0.2596, 0.3177, 1.1042, 1.1259]), (1.1259, False))
        self.assertEqual(claude_code._running_total([0.1, 0.1]), (0.1, False))   # a free segment keeps the total
        self.assertEqual(claude_code._running_total([0.025]), (0.025, False))
        value, summed = claude_code._running_total([0.2, None, 0.15])
        self.assertAlmostEqual(value, 0.35)
        self.assertTrue(summed)
        self.assertEqual(claude_code._running_total([None, "n/a", True]), (None, False))

    def test_sum_or_none_ignores_missing_values(self):
        self.assertEqual(claude_code._sum_or_none([22, 5, 40, 1]), 68)
        self.assertEqual(claude_code._sum_or_none([None, 4]), 4)
        self.assertIsNone(claude_code._sum_or_none([None, "x", False]))

    def test_merge_model_usage_sums_counters_and_keeps_constants(self):
        merged, summed = claude_code._merge_model_usage([
            {"m1": {"inputTokens": 1, "outputTokens": 10, "costUSD": 0.1, "contextWindow": 200000, "tier": "a"}},
            {"m1": {"inputTokens": 2, "outputTokens": 20, "costUSD": 0.3, "contextWindow": 200000, "tier": "b"},
             "m2": {"outputTokens": 7, "webSearchRequests": 1, "costUSD": 0.05}},
            None,
            {"m1": {"outputTokens": 5, "costUSD": 0.35, "cacheCreationInputTokens": 9}},
        ])
        self.assertFalse(summed)
        self.assertEqual(merged["m1"], {"inputTokens": 3, "outputTokens": 35, "costUSD": 0.35, "contextWindow": 200000,
                                        "tier": "a", "cacheCreationInputTokens": 9})
        self.assertEqual(merged["m2"], {"outputTokens": 7, "webSearchRequests": 1, "costUSD": 0.05})

    def test_merge_model_usage_flags_non_cumulative_cost(self):
        merged, summed = claude_code._merge_model_usage([{"m1": {"costUSD": 0.2}}, {"m1": {"costUSD": 0.15}}])
        self.assertTrue(summed)
        self.assertAlmostEqual(merged["m1"]["costUSD"], 0.35)

    def test_aggregate_results_matches_the_observed_live_shape(self):
        turns, durations, costs, tokens = (22, 5, 40, 1), (277755, 19003, 412990, 4831), (0.2596, 0.3177, 1.1042, 1.1259), (9000, 2000, 10626, 207)
        results = [_result(n, d, c, t) for n, d, c, t in zip(turns, durations, costs, tokens)]
        agg = claude_code._aggregate_results(results)
        self.assertEqual((agg["count"], agg["num_turns"], agg["duration_ms"]), (4, 68, 714579))
        self.assertEqual(agg["cost_estimate_usd"], 1.1259)
        self.assertEqual(agg["output_tokens"], 21833)
        self.assertEqual(agg["model_usage"][MODEL]["outputTokens"], 21833)
        self.assertEqual(agg["model_ids"], [MODEL])
        self.assertIs(agg["last"], results[-1])
        self.assertFalse(agg["cost_summed"] or agg["model_cost_summed"])


class ReportResultFoldingTest(unittest.TestCase):
    def _report(self, events):
        harness = plugins.get("harness", "claude-code")()
        argvs = []

        def fake_run(ctx, argv, stage, **kwargs):
            argvs.append(argv)
            _write_raw(ctx["run_dir"] / f"{stage}.raw.jsonl", events)
            return {"exit_code": 0, "timed_out": False}

        with tempfile.TemporaryDirectory() as tmp:
            ctx = _ctx(tmp, {}, stage_name="extract")
            with mock.patch.object(claude_code.ClaudeCode, "run", side_effect=fake_run):
                res = harness.report(ctx, "Synthetic evidence only.", schema={"type": "object"})
        self.assertNotIn("Synthetic evidence only.", json.dumps(argvs))
        return res

    def test_single_result_behavior_is_unchanged(self):
        data = {"asks": []}
        res = self._report([{"type": "result", "subtype": "success", "result": json.dumps(data), "structured_output": data,
                             "is_error": False, "usage": {"output_tokens": 12}, "total_cost_usd": 0.02,
                             "modelUsage": {"fake": {}}}])
        self.assertEqual(res["structured"], data)
        self.assertEqual(res["text"], json.dumps(data))
        proc = res["proc"]
        self.assertEqual((proc["output_tokens"], proc["model_ids"], proc["cost_estimate_usd"]), (12, "fake", 0.02))
        self.assertFalse(proc["is_error"])
        self.assertEqual(proc["result_subtype"], "success")
        self.assertNotIn("result_events", proc)

    def test_multiple_results_use_the_last_text_and_fold_usage(self):
        data = {"asks": [{"id": "a1"}]}
        res = self._report([
            {"type": "result", "subtype": "success", "result": "thinking out loud", "is_error": False,
             "usage": {"output_tokens": 12}, "total_cost_usd": 0.10, "modelUsage": {"m1": {"outputTokens": 12}}},
            {"type": "result", "subtype": "success", "result": json.dumps(data), "structured_output": data,
             "is_error": False, "usage": {"output_tokens": 8}, "total_cost_usd": 0.30, "modelUsage": {"m2": {"outputTokens": 8}}},
        ])
        self.assertEqual(res["structured"], data)
        self.assertEqual(res["text"], json.dumps(data))
        proc = res["proc"]
        self.assertEqual((proc["output_tokens"], proc["model_ids"], proc["cost_estimate_usd"]), (20, "m1,m2", 0.30))
        self.assertEqual(proc["result_events"], 2)
        self.assertFalse(proc["is_error"])

    def test_missing_result_is_still_an_error(self):
        res = self._report([_init()])
        self.assertTrue(res["proc"]["is_error"])
        self.assertEqual(res["proc"]["result_subtype"], "missing result")
        self.assertEqual((res["text"], res["structured"]), ("", None))


class ProgressTest(unittest.TestCase):
    STREAM = [
        _init(),
        _assistant("m1", _text("Let me look.\n\nSecond   line."), T[1], 3),
        _assistant("m2", _tool_use("t1", "echo hi && \\\n  echo there"), T[2], 3),
        _tool_result("t1", T[3], "hi\nthere"),
        _assistant("m3", {"type": "thinking", "thinking": "private reasoning is not progress"}, T[4], 3),
        _assistant("m4", {"type": "tool_use", "id": "t2", "name": "Read", "input": {"file_path": "/tmp/x.py", "limit": 5}},
                   T[5], 3, parent="t0"),
        _result(2, 4000, 0.01, 9, text="Done"),
    ]

    def _progress(self, events, trailing=None, **kwargs):
        with tempfile.TemporaryDirectory() as tmp:
            run_dir = Path(tmp) / "run"
            run_dir.mkdir()
            _write_raw(run_dir / "execute.raw.jsonl", events, trailing=trailing)
            return plugins.get("harness", "claude-code")().progress(run_dir, **kwargs)

    def test_kinds_order_and_one_line_text(self):
        items = self._progress(self.STREAM)
        self.assertEqual([i["kind"] for i in items], ["text", "tool", "tool", "result"])
        self.assertEqual(items[0], {"at": "2026-01-01T00:00:01.000Z", "kind": "text", "text": "Let me look. Second line."})
        self.assertEqual(items[1]["text"], "Bash: command: echo hi && \\ echo there")
        self.assertEqual(items[2]["text"], "Read: file_path: /tmp/x.py")   # sub-agent activity is shown too
        self.assertEqual(items[3], {"at": "2026-01-01T00:00:06.000Z", "kind": "result", "text": "result success turns=2"})
        self.assertTrue(all("\n" not in i["text"] and len(i["text"]) <= 200 for i in items))

    def test_limit_keeps_the_most_recent_items(self):
        events = [_init()] + [_assistant(f"m{i}", _text(f"step {i}"), T[i % 20], 1) for i in range(15)]
        items = self._progress(events)
        self.assertEqual(len(items), 10)
        self.assertEqual([i["text"] for i in items], [f"step {i}" for i in range(5, 15)])
        self.assertEqual([i["kind"] for i in self._progress(self.STREAM, limit=2)], ["tool", "result"])

    def test_long_inputs_are_truncated(self):
        long_cmd = "x" * 500
        items = self._progress([_init(), _assistant("m1", _tool_use("t1", long_cmd), T[1], 1),
                                _assistant("m2", _text("y" * 1000), T[2], 1)])
        self.assertTrue(items[0]["text"].startswith("Bash: command: xxx"))
        self.assertTrue(items[0]["text"].endswith("..."))
        self.assertLessEqual(len(items[0]["text"]), 160 + len("Bash: "))
        self.assertEqual(len(items[1]["text"]), 200)
        self.assertTrue(items[1]["text"].endswith("..."))

    def test_tolerates_a_partial_trailing_line_and_a_missing_file(self):
        half = '{"at": "2026-01-01T00:00:09.000Z", "line": "{\\"type\\":\\"assistant\\",\\"message\\":{\\"con'
        items = self._progress(self.STREAM, trailing=half)
        self.assertEqual([i["kind"] for i in items], ["text", "tool", "tool", "result"])
        with tempfile.TemporaryDirectory() as tmp:
            self.assertEqual(plugins.get("harness", "claude-code")().progress(Path(tmp) / "nope"), [])
            self.assertEqual(plugins.get("harness", "claude-code")().progress(Path(tmp), stage="narrate"), [])

    def test_base_harness_reports_nothing(self):
        with tempfile.TemporaryDirectory() as tmp:
            self.assertEqual(Harness().progress(Path(tmp)), [])


if __name__ == "__main__":
    unittest.main()
