"""Synthetic regressions for inventory semantics and reporter evidence."""

import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))

from ajx import evidence, plugins
from ajx.base import empty_telemetry, finish_tool, tool_event
from test_claude_code_stream import _execute_ctx, _init, _write_raw, _write_transcript, _assistant, _tool_use, _tool_result


class EvidenceIntegrityTest(unittest.TestCase):
    def test_skill_and_slash_command_inventories_are_distinct(self):
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _execute_ctx(tmp)
            init = {**_init(), "skills": ["review", "ajx"],
                    "slash_commands": ["help", "review", "ajx", "compact"]}
            _write_raw(ctx["run_dir"] / "execute.raw.jsonl", [init])
            tel = plugins.get("harness", "claude-code")().normalize(ctx, "execute")
            hr = tel["harness_reported"]
            self.assertEqual(hr["skills_visible"], 2)
            self.assertEqual(hr["slash_commands_visible"], 4)
            self.assertEqual(hr["ajx_skills_visible"], ["ajx"])
            self.assertEqual(hr["ajx_slash_commands_visible"], ["ajx"])
            self.assertFalse(any("user config" in note or "is evaluation-aware" in note for note in tel["limitations"]))
            del init["skills"]
            _write_raw(ctx["run_dir"] / "execute.raw.jsonl", [init])
            hr = plugins.get("harness", "claude-code")().normalize(ctx, "execute")["harness_reported"]
            self.assertIsNone(hr["skills_visible"])
            self.assertIsNone(hr["ajx_skills_visible"])
            init["skills"] = []
            _write_raw(ctx["run_dir"] / "execute.raw.jsonl", [init])
            hr = plugins.get("harness", "claude-code")().normalize(ctx, "execute")["harness_reported"]
            self.assertEqual(hr["skills_visible"], 0)

    def test_missing_tool_error_flag_stays_unknown(self):
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _execute_ctx(tmp)
            _write_raw(ctx["run_dir"] / "execute.raw.jsonl", [_init()])
            result = _tool_result("t1", "2026-01-01T00:00:02Z")
            del result["message"]["content"][0]["is_error"]
            _write_transcript(tmp, [_assistant("m1", _tool_use("t1", "echo ok"),
                                              "2026-01-01T00:00:01Z", 1), result])
            tel = plugins.get("harness", "claude-code")().normalize(ctx, "execute")
            self.assertIsNone(tel["events"][0]["is_error"])

    def test_ajx_product_mentions_still_check_outer_paths(self):
        spec = {"trial": {"product": "AJX", "output_dir": "/outer/results", "workspace_root": "/work"},
                "path": "/outer/trial.toml"}
        tel = evidence.assign_ids({"events": [
            tool_event("a", "Bash", {"command": "ajx --help"}, None),
            tool_event("b", "Read", {"file_path": "/outer/trial.toml"}, None),
            tool_event("c", "Read", {"file_path": "/opt/.claude/skills/ajx/SKILL.md"}, None),
        ]})
        flags = evidence.isolation_scan(tel, spec, {"paths": {"workspace": "/work/0123456789"}})
        self.assertEqual(len(flags), 2)
        self.assertIn("trial dir", flags[0]["reasons"])
        self.assertIn("skills dir", flags[1]["reasons"])
        spec["trial"]["product"] = "other-cli"
        self.assertEqual(len(evidence.isolation_scan(tel, spec, {})), 3)

    def test_complete_input_reaches_digest_and_oversized_input_is_labeled(self):
        command = "jq '" + ".items | " * 40 + "sort_by(.total)' orders.json > summary.json"
        event = tool_event("a", "Bash", {"command": command}, None)
        tel = evidence.assign_ids(empty_telemetry(events=[event]))
        digest = evidence.build_digest({"trial": {}}, {"harness": "fake", "config": "clean"}, {}, tel, {}, {})
        self.assertIn(command, digest)
        self.assertIn("input_raw (complete)", digest)
        event["input_raw"] = {"command": "x" * (evidence.TOOL_INPUT_MAX + 1)}
        appendix = "\n".join(evidence.tool_input_evidence(tel["events"]))
        self.assertIn("input omitted from this digest", appendix)
        self.assertNotIn("input_raw (complete)", appendix)
        self.assertEqual(len(event["input_raw"]["command"]), evidence.TOOL_INPUT_MAX + 1)

    def test_failure_scope_and_unknown_coverage(self):
        ok = tool_event("a", "Bash", {"command": "broken-command; true"}, None)
        finish_tool(ok, None, False, "error: first command failed")
        unknown = tool_event("b", "Bash", {"command": "still running"}, None)
        tel = evidence.assign_ids(empty_telemetry(events=[ok, unknown], tool_status="complete"))
        summary = evidence.task_summary({}, tel)
        self.assertEqual(summary["failed_tool_calls"], 0)
        self.assertEqual(summary["tool_results_without_error_status"], 1)
        self.assertEqual(summary["tool_error_status"], "partial")
        self.assertIn("is_error", summary["failure_metric_basis"])
        self.assertEqual(evidence.task_summary({}, empty_telemetry())["tool_error_status"], "unavailable")

    def test_only_bare_product_mentions_are_exempted(self):
        for command in ("ajx --help", "curl https://ajx.dev/agents/", "/opt/bin/ajx version"):
            self.assertFalse(evidence._unexpected_ajx_mention(command, "ajx"), command)
        for command in ("cat ajx-review.md", "cat ajx_context.json", "curl https://ajx.dev.invalid"):
            self.assertTrue(evidence._unexpected_ajx_mention(command, "ajx"), command)


if __name__ == "__main__":
    unittest.main()
