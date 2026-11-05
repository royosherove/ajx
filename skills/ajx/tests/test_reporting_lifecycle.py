"""Offline reporting regressions: final state, provenance, and progress."""

import contextlib
import io
import json
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest import mock

from test_ajx import _cli, render, runner, specmod, read_json, write_json


class ReportingLifecycleTest(unittest.TestCase):
    def make_run(self, tmp):
        root = Path(tmp)
        (root / "task-prompt.md").write_text("Create the requested file.\n")
        (root / "trial.toml").write_text(f"""
[trial]
id = "synthetic"
product = "sample"
workspace_root = {json.dumps(str(root / "workspaces"))}
[task]
prompt_file = "task-prompt.md"
[[cells]]
id = "fake"
harness = "fake"
""")
        spec = specmod.load(root / "trial.toml")
        return runner.Run(spec, spec["cells"][0], 1, lambda text: None)

    def test_final_stage_failures_are_recorded(self):
        for stage in ("render", "archive"):
            with self.subTest(stage=stage), tempfile.TemporaryDirectory() as tmp:
                run = self.make_run(tmp)
                run.stage_extract = lambda: write_json(run.run_dir / "asks.json", {
                    "extraction_status": "ok", "asks": [], "gates": [], "strengths": [], "journey_errata": []})
                original = run.stage_render

                def fail():
                    if stage == "render":
                        original()
                    raise RuntimeError("synthetic final-stage failure")

                setattr(run, f"stage_{stage}", fail)
                state = run.go()
                record = read_json(run.run_dir / "run.json")
                self.assertEqual(record["stages"][stage], "error")
                self.assertEqual(record["stages"], {name: item["status"] for name, item in state["stages"].items()})

    def test_narration_failure_is_preserved_through_reconstruction(self):
        cases = (
            ("safety_stop", True, True, None, {"final_text": "", "harness_reported": {"safety_stops": 1}}),
            ("resume_error", True, True, RuntimeError("synthetic failure"), {}),
            ("resume_error", True, True, RuntimeError("synthetic -->\nfailure"), {}),
            ("resume_error", True, True, RuntimeError("synthetic -->\n" + "λ detail " * 10000), {}),
            ("resume_unsupported", False, True, None, {}),
            ("session_unavailable", True, False, None, {}),
        )
        for category, can_resume, session, error, telemetry in cases:
            with self.subTest(category=category), tempfile.TemporaryDirectory() as tmp:
                run = self.make_run(tmp)
                run.harness.can_resume = can_resume
                run.state["execute"] = {"session_id": "synthetic-session"} if session else {}
                run.mark("execute", "done")
                write_json(run.run_dir / "telemetry.execute.json", {})
                (run.run_dir / "digest.md").write_text("Synthetic evidence.")
                with mock.patch.object(run.harness, "narrate", return_value={"exit_code": 0}, side_effect=error), \
                     mock.patch.object(run.harness, "normalize", return_value=telemetry), \
                     mock.patch.object(runner.reporter, "reconstruct", return_value={
                         "text": "# Journey: synthetic reconstruction", "proc": {"exit_code": 0}}):
                    run.stage_narrate()
                self.assertEqual(run.state["narration_failure"]["category"], category)
                journey = (run.run_dir / "journey.md").read_text()
                self.assertIn(category, journey)
                self.assertNotIn("harness cannot resume or resume failed", journey)
                header = journey.splitlines()[0]
                self.assertLessEqual(len(header), 600)
                self.assertEqual(header.count("-->"), 1)
                if error:
                    self.assertEqual(run.state["narration_failure"]["message"],
                                     f"{type(error).__name__}: {error}")
                    if "-->" in str(error):
                        self.assertIn("synthetic —>", header)
                    if len(str(error)) > 1000:
                        self.assertIn("[truncated; see narration_failure.message]", header)
                run.stage_render()
                failure = read_json(run.run_dir / "run.json")["narration_failure"]
                self.assertEqual(failure["category"], category)
                if error:
                    self.assertEqual(failure["message"], f"{type(error).__name__}: {error}")

    def test_final_record_failure_does_not_prevent_cleanup(self):
        with tempfile.TemporaryDirectory() as tmp:
            run = self.make_run(tmp)
            run.stage_extract = lambda: None
            run.stage_render = lambda: (run.run_dir / "run.json").mkdir()
            with self.assertRaises(IsADirectoryError):
                run.go()
            self.assertEqual(run.status("teardown"), "done")
            self.assertTrue((run.run_dir / "workspace" / "made.txt").exists())
            self.assertFalse(Path(run.state["paths"]["workspace"]).exists())

    def test_inventory_does_not_establish_instruction_visibility(self):
        with tempfile.TemporaryDirectory() as tmp:
            run = self.make_run(tmp)
            inventory = {"skills_visible": 2, "slash_commands_visible": 5,
                         "ajx_skills_visible": ["ajx"], "ajx_slash_commands_visible": ["ajx"],
                         "skills_inventory": [{"name": "ajx", "source": None, "source_status": "unknown"}]}
            write_json(run.run_dir / "telemetry.execute.json", {"harness_reported": inventory})
            record = render.write_run_json(run.spec, run.cell, 1, run.run_dir, run.state,
                                           run.harness, run.auth, run.runner)
            self.assertIsNone(record["report_instructions_visible_during_task"])
            self.assertEqual(record["reported_extension_inventory"], inventory)
            self.assertNotIn("evaluation-aware", " ".join(record["limitations"]))
            self.assertIn("listing does not establish", " ".join(record["limitations"]))

    def test_status_reports_stage_age_without_worker_events(self):
        cli = _cli()
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            run_dir = root / "runs" / "synthetic-r1"
            start = (datetime.now(timezone.utc) - timedelta(minutes=2)).isoformat()
            state = {"stages": {"execute": {"status": "done"}, "extract": {"status": "running", "at": start}}}
            write_json(run_dir / "state.json", state)
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                code = cli.main(["status", str(root)])
            self.assertEqual(code, 0)
            self.assertIn("extract: started " + start, output.getvalue())
            self.assertIn("elapsed", output.getvalue())
            active = cli._active_stages(state)
            self.assertEqual(active[0]["stage"], "extract")
            self.assertGreaterEqual(active[0]["elapsed_seconds"], 120)

    def test_comparability_values_remain_literal(self):
        self.assertEqual(render._comparability_md(["tool-1.2", "another-2"]), "tool-1.2, another-2")
        text = render._comparability_md(["<img src=x>", "[bad](javascript:bad)", "a_b"])
        self.assertNotIn("<img", text)
        self.assertNotIn("[bad](javascript:bad)", text)
        self.assertIn(r"a\_b", text)


if __name__ == "__main__":
    unittest.main()
