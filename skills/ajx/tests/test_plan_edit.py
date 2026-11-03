"""The console's structured trial editor: reading the model, writing it back section by section, and
the preview/save flow with its output-folder locks. Synthetic copies of the bundled examples only."""

import copy
import shutil
import sys
import tempfile
import tomllib
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))

from ajx import console, plan_edit, plugins, runner, spec as specmod  # noqa: E402
from ajx.util import sha256_bytes, write_json  # noqa: E402

EXAMPLES = HERE.parent / "examples"
plan_edit._DOCKER.update(at=float("inf"), info={"images": [], "host": ""})  # never ask Docker in tests


class PlanEditCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def copy(self, name):
        dst = self.tmp / name
        shutil.copytree(EXAMPLES / name, dst, ignore=shutil.ignore_patterns("ajx-reports", ".ajx-console", "__pycache__"))
        plugins.load(dst)
        return dst / "trial.toml"

    def write(self, trial, model):
        text, prompt, added = plan_edit.render(trial, model)
        out = trial.with_name("edited.toml")
        out.write_text(text)
        return text, specmod.load(out, readiness=False), added

    def edit_text(self, trial, old, new):
        text = trial.read_text()
        self.assertEqual(text.count(old), 1, old)
        trial.write_text(text.replace(old, new))


class RoundTripTest(PlanEditCase):
    def test_an_unchanged_model_writes_back_the_same_bytes(self):
        for name in ("console-demo", "environment-profiles", "aws-cloud", "jq-smoke"):
            trial = self.copy(name)
            text, _, added = plan_edit.render(trial, plan_edit.read_model(trial))
            self.assertEqual(text, trial.read_text(), name)
            self.assertEqual(added, [])

    def test_the_model_reads_layers_from_the_file(self):
        model = plan_edit.read_model(self.copy("environment-profiles"))
        self.assertEqual([(e["id"], e["kind"]) for e in model["envs"]], [("developer", "profile")])
        self.assertEqual({w["id"]: (w["env"], w["skills"]) for w in model["workers"]},
                         {"plain": ("developer", "none"), "guided": ("developer", "folders")})
        self.assertEqual(model["workers"][1]["skillPaths"], ["skills/csv-guide"])

    def test_a_profile_named_local_does_not_collide_with_the_folder_on_your_machine(self):
        trial = self.copy("console-demo")
        trial.write_text(trial.read_text() + '\n[environments.local]\nbackend = "local"\n')
        model = plan_edit.read_model(trial)
        self.assertEqual(sorted(e["id"] for e in model["envs"]), sorted([plan_edit.FOLDER_ENV, "local"]))
        model["run"]["attempts"] = 2
        self.assertEqual(tomllib.loads(plan_edit.render(trial, model)[0])["trial"]["repetitions"], 2)

    def test_toml_dates_keep_the_page_working_without_the_editor(self):
        trial = self.copy("console-demo")
        trial.write_text(trial.read_text().replace("[task]", "[task]\nwritten = 2024-05-01", 1))
        with self.assertRaisesRegex(plan_edit.PlanError, "date"):
            plan_edit.read_model(trial)
        self.assertIsNone(console.snapshot(trial)["plan_model"])


class EditTest(PlanEditCase):
    def test_a_model_change_rewrites_only_its_sections_and_keeps_other_comments(self):
        trial = self.copy("console-demo")
        model = plan_edit.read_model(trial)
        model["workers"][0]["model"] = "opus"
        model["checks"].append({"name": "out.csv exists", "type": "file", "path": "out.csv", "where": "host"})
        model["run"]["attempts"] = 3
        text, spec, _ = self.write(trial, model)
        self.assertEqual(spec["cells"][0]["model"], "opus")
        self.assertEqual([c["name"] for c in spec["verify"]][-1], "out.csv exists")
        self.assertEqual(spec["trial"]["repetitions"], 3)
        self.assertIn("# AJX Console demo.", text)  # the file's own comments outside the edits survive
        changes = [c["text"] for c in plan_edit.changes(plan_edit.read_model(trial), model)]
        self.assertIn("worker demo \u00b7 model: default -> opus", changes)
        self.assertIn("run settings \u00b7 attempts: 1 -> 3", changes)

    def test_multi_line_commands_stay_readable_and_exact(self):
        trial = self.copy("console-demo")
        model = plan_edit.read_model(trial)
        script = 'set -eu\n[ -f out.csv ] && echo "one"\nprintf %s \\\\\nrm -rf "$AJX_RUN_TOKEN"'
        model["account"]["teardown"] = [{"run": script}]
        text, spec, _ = self.write(trial, model)
        self.assertIn("'''\n" + script + "'''", text)
        self.assertEqual(spec["teardown"][0]["run"], script)
        trial.write_text(text)  # a header-like line inside the command is not a section
        again = plan_edit.read_model(trial)
        again["run"]["minutes"] = 4
        self.assertEqual(self.write(trial, again)[1]["teardown"][0]["run"], script)

    def test_multi_line_values_and_subtables_are_edited_whole(self):
        trial = self.copy("console-demo")
        self.edit_text(trial, 'materials = ["orders-cli --help"]', 'materials = [\n  "orders-cli --help",\n  "README.md",\n]')
        trial.write_text(trial.read_text() + '\n[[setup]]\nrun = "true"\n\n[setup.env]\nA = "1"\n')
        model = plan_edit.read_model(trial)
        model["task"]["materials"] = ["orders-cli --help"]
        model["task"]["setup"].append({"run": "echo two", "name": "", "extra": {}})
        text, spec, _ = self.write(trial, model)
        self.assertEqual(spec["task"]["materials"], ["orders-cli --help"])
        self.assertEqual(tomllib.loads(text)["setup"], [{"run": "true", "env": {"A": "1"}}, {"run": "echo two"}])

    def test_a_single_model_list_keeps_its_form_so_cell_ids_do_not_change(self):
        trial = self.copy("console-demo")
        self.edit_text(trial, 'id = "demo"\nharness = "console-demo"', 'id = "demo"\nharness = "console-demo"\nmodels = ["demo-model"]')
        model = plan_edit.read_model(trial)
        model["workers"][0]["effort"] = "high"
        _, spec, _ = self.write(trial, model)
        self.assertEqual([c["id"] for c in spec["cells"]], ["demo-demo-model"])

    def test_a_container_worker_gets_an_environment_and_a_sign_in_route_but_no_needless_profile(self):
        trial = self.copy("console-demo")
        model = plan_edit.read_model(trial)
        model["envs"].append({"id": "sealed", "kind": "container", "image": "sha256:" + "a" * 64, "network": "none",
                              "required": ["filesystem_isolation"], "limits": dict(plan_edit.CONTAINER_LIMITS),
                              "tools": [{"name": "python3", "required": True}]})
        worker = copy.deepcopy(model["workers"][0])
        worker.update(id="sealed-claude", cli="claude-code", env="sealed", auth="bedrock-keys", model="sonnet")
        model["workers"].append(worker)
        text, _, added = plan_edit.render(trial, model)
        self.assertEqual(added, ["bedrock-keys"])
        raw = tomllib.loads(text)
        self.assertEqual(raw["environments"]["sealed"]["backend"], "container")
        self.assertNotIn("limits", raw["environments"]["sealed"])  # the defaults stay implicit
        cell = raw["cells"][-1]
        self.assertEqual((cell["environment"], cell["config"]), ("sealed", "clean"))
        self.assertNotIn("agent_configuration", cell)
        self.assertNotIn("agent_configurations", raw)
        self.assertEqual(raw["auth"]["bedrock-keys"]["type"], "claude-bedrock")
        self.assertNotIn("AKIA", text)  # credentials are references to the terminal, never values

    def test_skills_get_their_own_profile_without_replacing_another_workers(self):
        trial = self.copy("environment-profiles")
        model = plan_edit.read_model(trial)
        plain = model["workers"][0]
        plain.update(skills="folders", skillPaths=["skills/csv-guide", "skills/other"])
        text, _, _ = plan_edit.render(trial, model)
        raw = tomllib.loads(text)
        self.assertEqual(raw["agent_configurations"]["plain"]["skills"]["paths"], ["skills/csv-guide", "skills/other"])
        self.assertEqual(raw["agent_configurations"]["guided"]["skills"]["paths"], ["skills/csv-guide"])

    def test_removing_a_worker_keeps_its_profile_so_recorded_attempts_stay_comparable(self):
        trial = self.copy("environment-profiles")
        before = sorted(tomllib.loads(trial.read_text())["agent_configurations"])
        model = plan_edit.read_model(trial)
        model["workers"] = [w for w in model["workers"] if w["id"] == "plain"]
        _, spec, _ = self.write(trial, model)
        self.assertEqual([c["id"] for c in spec["cells"]], ["plain"])
        self.assertEqual(sorted(spec["agent_configurations"]), before)

    def test_edits_that_cannot_work_are_refused_with_a_reason(self):
        trial = self.copy("console-demo")
        cases = [
            (lambda m: m["workers"][0].update(model="sonnet 5.5"), "not a model name"),
            (lambda m: m["workers"][0].update(skills="folders", skillPaths=["skills/x"]), "fresh-profile or container"),
            (lambda m: m["workers"].append(dict(m["workers"][0])), "unique"),
            (lambda m: m["run"].update(attempts=1.5), "whole number"),
            (lambda m: m["run"].update(minutes="x"), "must be a number"),
            (lambda m: m["checks"][0].update(expect_exit="x"), "must be a number"),
            (lambda m: m["workers"][0].pop("env"), "cannot write"),
            (lambda m: m.update(workers=[]), "at least one worker"),
        ]
        for edit, reason in cases:
            model = plan_edit.read_model(trial)
            edit(model)
            with self.assertRaisesRegex(plan_edit.PlanError, reason):
                plan_edit.render(trial, model)

    def test_an_odd_value_the_user_did_not_touch_never_blocks_an_edit(self):
        trial = self.copy("console-demo")
        self.edit_text(trial, "timeout_seconds = 300", "timeout_seconds = 5")
        model = plan_edit.read_model(trial)
        model["run"]["attempts"] = 2
        self.assertIn("timeout_seconds = 5", plan_edit.render(trial, model)[0])

    def test_values_spec_accepts_but_the_editor_would_refuse_block_only_their_own_edit(self):
        trial = self.copy("console-demo")
        self.edit_text(trial, 'id = "demo"\nharness = "console-demo"', 'id = "demo"\nharness = "console-demo"\nmodel = "my model"')
        model = plan_edit.read_model(trial)
        model["run"]["attempts"] = 2
        text = plan_edit.render(trial, model)[0]
        self.assertIn('model = "my model"', text)
        self.assertIn("repetitions = 2", text)
        model["workers"][0]["effort"] = "high"  # touching that worker validates it again
        with self.assertRaisesRegex(plan_edit.PlanError, "not a model name"):
            plan_edit.render(trial, model)

    def test_an_untouched_check_value_spec_accepts_never_blocks_an_edit(self):
        trial = self.copy("console-demo")
        self.edit_text(trial, 'run = "cmp -s out.csv', 'expect_exit = true\nrun = "cmp -s out.csv')
        model = plan_edit.read_model(trial)
        model["run"]["minutes"] = 3
        text = plan_edit.render(trial, model)[0]
        self.assertIn("expect_exit = true", text)
        self.assertIn("timeout_seconds = 180", text)

    def test_unchanged_workers_keep_their_cell_tables_exactly(self):
        trial = self.copy("console-demo")
        self.edit_text(trial, 'id = "demo"\nharness = "console-demo"', 'id = "demo"\nharness = "console-demo"\neffort = ""')
        model = plan_edit.read_model(trial)
        extra = copy.deepcopy(model["workers"][0])
        extra["id"] = "demo-two"
        model["workers"].append(extra)
        cells = tomllib.loads(plan_edit.render(trial, model)[0])["cells"]
        self.assertEqual(cells[0], {"id": "demo", "harness": "console-demo", "effort": ""})
        self.assertEqual(cells[1]["id"], "demo-two")

    def test_a_reporter_left_automatic_stays_automatic(self):
        trial = self.copy("console-demo")
        self.edit_text(trial, '[reporter]\nharness = "console-demo"\n', "")
        model = plan_edit.read_model(trial)
        self.assertEqual(model["reporter"]["cli"], "")
        model["reporter"]["model"] = "demo-model"
        raw = tomllib.loads(plan_edit.render(trial, model)[0])
        self.assertEqual(raw["reporter"], {"model": "demo-model"})


class SavePlanTest(PlanEditCase):
    def setUp(self):
        super().setUp()
        self.trial = self.copy("console-demo")

    def base(self):
        return sha256_bytes(self.trial.read_bytes())

    def record_attempt(self, out=None):
        spec = console.load(self.trial)[0]
        out = out or console.output_dir(self.trial, spec)
        write_json(out / "runs" / "demo-r1" / "state.json", {"stages": {"prepare": {"status": "done"}}})
        write_json(out / "matrix-plan.json", {
            "task_prompt_sha256": spec["task"]["prompt_sha256"], "cells_contract": runner.cells_contract(spec),
            "normalized_profiles": {"schema_version": 1, "environments": spec.get("environments", {}),
                                    "agent_configurations": spec.get("agent_configurations", {})},
            "task_inputs": runner.task_inputs(spec)})
        return out

    def test_preview_reports_the_diff_changes_and_problems_without_writing(self):
        before = self.trial.read_text()
        model = plan_edit.read_model(self.trial)
        model["run"]["minutes"] = 3
        view = console.save_plan(self.trial, model, dry_run=True)
        self.assertIn("+timeout_seconds = 180", view["diff"])
        self.assertEqual(view["problems"], [])
        self.assertIn("timeout_seconds = 180", view["text"])
        self.assertEqual(self.trial.read_text(), before)
        model["envs"].append({"id": "box", "kind": "container", "image": "sha256:" + "b" * 64, "network": "none"})
        model["workers"][0]["env"] = "box"
        problems = console.save_plan(self.trial, model, dry_run=True)["problems"]
        self.assertTrue(problems and "fresh HOME" in problems[0], problems)
        model["run"]["minutes"] = "soon"
        self.assertIn("must be a number", console.save_plan(self.trial, model, dry_run=True)["problems"][0])

    def test_save_writes_the_file_and_the_prompt_and_records_the_change(self):
        model = plan_edit.read_model(self.trial)
        model["task"]["prompt"] = "Convert orders.json to out.csv with orders-cli.\n"
        model["workers"][0]["model"] = "demo-model"
        base = self.base()
        prompt_base = sha256_bytes((self.trial.parent / "task-prompt.md").read_bytes())
        console.save_plan(self.trial, model, base, prompt_base)
        self.assertEqual((self.trial.parent / "task-prompt.md").read_text(), "Convert orders.json to out.csv with orders-cli.\n")
        self.assertEqual(specmod.load(self.trial)["cells"][0]["model"], "demo-model")
        entry = [e for e in console.read_entries(self.trial)[0] if e["kind"] == "plan_changed"][-1]
        self.assertIn("task prompt edited", entry["changes"])
        with self.assertRaises(console.Conflict):
            console.save_plan(self.trial, model, base)
        with self.assertRaisesRegex(console.ConsoleError, "which version"):
            console.save_plan(self.trial, model)

    def test_an_output_folder_with_attempts_takes_only_more_attempts_new_workers_or_another_folder(self):
        self.record_attempt()
        for edit in (lambda m: m["workers"][0].update(model="other"),
                     lambda m: m["checks"][0].update(expect_exit=3),
                     lambda m: m["task"].update(prompt="A different task.\n"),
                     lambda m: m["reporter"].update(model="demo-model")):
            model = plan_edit.read_model(self.trial)
            edit(model)
            with self.assertRaisesRegex(console.ConsoleError, "Locked for this output folder"):
                console.save_plan(self.trial, model, self.base(), sha256_bytes((self.trial.parent / "task-prompt.md").read_bytes()))
        model = plan_edit.read_model(self.trial)
        model["run"]["attempts"] = 2
        extra = copy.deepcopy(model["workers"][0])
        extra["id"] = "demo-two"
        model["workers"].append(extra)
        console.save_plan(self.trial, model, self.base())
        self.assertEqual(specmod.load(self.trial)["trial"]["repetitions"], 2)
        model = plan_edit.read_model(self.trial)
        model["workers"][0]["model"] = "other"
        model["run"]["output_dir"] = "ajx-reports/orders-cli/fresh"
        console.save_plan(self.trial, model, self.base())
        self.assertEqual(specmod.load(self.trial)["cells"][0]["model"], "other")

    def test_switching_back_to_a_folder_checks_what_its_attempts_recorded(self):
        first = self.record_attempt()
        model = plan_edit.read_model(self.trial)
        model["run"]["output_dir"] = "ajx-reports/orders-cli/second"
        console.save_plan(self.trial, model, self.base())
        model = plan_edit.read_model(self.trial)
        model["workers"][0]["model"] = "other"
        console.save_plan(self.trial, model, self.base())
        model = plan_edit.read_model(self.trial)
        model["run"]["output_dir"] = str(first.resolve().relative_to(self.trial.parent.resolve()))
        problems = console.save_plan(self.trial, model, dry_run=True)["problems"]
        self.assertTrue(problems and "Locked for this output folder" in problems[0], problems)

    def test_switching_back_to_a_folder_with_other_checks_is_locked(self):
        first = self.record_attempt()
        model = plan_edit.read_model(self.trial)
        model["run"]["output_dir"] = "ajx-reports/orders-cli/second"
        model["checks"][0]["run"] = "test -s out.csv"
        console.save_plan(self.trial, model, self.base())
        model = plan_edit.read_model(self.trial)
        model["run"]["output_dir"] = str(first.resolve().relative_to(self.trial.parent.resolve()))
        problems = console.save_plan(self.trial, model, dry_run=True)["problems"]
        self.assertTrue(problems and "the success checks" in problems[0], problems)

    def test_a_prompt_edit_waits_until_the_trial_file_loads(self):
        self.record_attempt()
        shutil.rmtree(self.trial.parent / "fixture")  # trial.toml no longer loads
        model = plan_edit.read_model(self.trial)
        model["task"]["prompt"] = "A different task.\n"
        problems = console.save_plan(self.trial, model, dry_run=True)["problems"]
        self.assertTrue(problems and "fixture_dir" in problems[0], problems)

    def test_the_editor_can_repair_an_empty_prompt(self):
        (self.trial.parent / "task-prompt.md").write_text("")
        model = plan_edit.read_model(self.trial)
        model["task"]["prompt"] = "Convert orders.json to out.csv with orders-cli."
        console.save_plan(self.trial, model, self.base(), sha256_bytes(b""))
        self.assertEqual((self.trial.parent / "task-prompt.md").read_text(),
                         "Convert orders.json to out.csv with orders-cli.\n")

    def test_a_crlf_trial_saves_the_prompt_and_the_file_together(self):
        self.trial.write_bytes(self.trial.read_bytes().replace(b"\n", b"\r\n"))
        base = console.snapshot(self.trial)["trial"]["sha256"]
        self.assertEqual(base, self.base())
        model = plan_edit.read_model(self.trial)
        model["task"]["prompt"] = "A different task.\n"
        model["run"]["minutes"] = 3
        console.save_plan(self.trial, model, base, sha256_bytes((self.trial.parent / "task-prompt.md").read_bytes()))
        self.assertEqual((self.trial.parent / "task-prompt.md").read_text(), "A different task.\n")
        self.assertEqual(specmod.load(self.trial)["trial"]["timeout_seconds"], 180)

    def test_a_failed_trial_write_puts_the_old_prompt_back(self):
        before = (self.trial.parent / "task-prompt.md").read_bytes()
        model = plan_edit.read_model(self.trial)
        model["task"]["prompt"] = "A different task.\n"
        model["run"]["minutes"] = 3
        real = console._write_trial
        def failing(*a, **k):
            raise console.Conflict("trial.toml changed on disk")
        console._write_trial = failing
        try:
            with self.assertRaises(console.Conflict):
                console.save_plan(self.trial, model, self.base(), sha256_bytes(before))
        finally:
            console._write_trial = real
        self.assertEqual((self.trial.parent / "task-prompt.md").read_bytes(), before)

    def test_task_inputs_survive_moving_the_trial_folder_and_hold_only_json(self):
        spec = specmod.load(self.trial)
        inputs = runner.task_inputs(spec)
        self.assertEqual(inputs["fixture_dir"], "fixture")
        moved = self.tmp / "moved"
        shutil.copytree(self.trial.parent, moved)
        self.assertEqual(runner.task_inputs(specmod.load(moved / "trial.toml")), inputs)
        self.edit_text(self.trial, "[[verify]]", '[[setup]]\nrun = "true"\nwhen = 2026-01-01\n\n[[verify]]')
        self.assertEqual(runner.task_inputs(specmod.load(self.trial))["setup"][0]["when"], "2026-01-01")

    def test_saving_nothing_writes_no_journal_entry(self):
        before = len(console.read_entries(self.trial)[0])
        console.save_plan(self.trial, plan_edit.read_model(self.trial), self.base())
        self.assertEqual(len(console.read_entries(self.trial)[0]), before)

    def test_the_journey_view_says_why_it_is_a_reconstruction(self):
        out = console.output_dir(self.trial, console.load(self.trial)[0])
        run = out / "runs" / "demo-r1"
        write_json(run / "state.json", {"stages": {}, "journey_provenance": "EDITORIAL RECONSTRUCTION ...",
                                        "narrate": {"reconstruction": True, "self_narration_rejected": "safety_stop"}})
        write_json(run / "telemetry.narrate.json", {"harness_reported": {"safety_stops": 1}})
        (run / "journey.md").write_text("# Journey (editorial reconstruction): x\n")
        view = console._journey_view(run, console.read_json(run / "state.json", {}))
        self.assertEqual((view["reconstruction"], view["self_narration_rejected"], view["safety_stops"], view["total_steps"]),
                         (True, "safety_stop", 1, 0))
        write_json(run / "state.json", {"stages": {}, "journey_provenance": "self-narrated: ...", "narrate": {}})
        (run / "telemetry.narrate.json").unlink()
        view = console._journey_view(run, console.read_json(run / "state.json", {}))
        self.assertEqual((view["reconstruction"], view["self_narration_rejected"], view["safety_stops"]),
                         (False, None, 0))

    def test_the_run_view_names_its_cell_cleanup_commands_and_check_locations(self):
        out = console.output_dir(self.trial, console.load(self.trial)[0])
        run = out / "runs" / "demo-r1"
        write_json(run / "state.json", {"stages": {"teardown": {"status": "done"}},
                                        "environment_cleanup": {"status": "failed", "confirmed": False,
                                                                "reason": "container still running", "error": "x" * 500}})
        write_json(run / "teardown.json", [{"cmd": "aws s3 rb s3://demo", "exit_code": 1, "timed_out": False,
                                            "location": "environment", "auth_errors": ["AccessDenied"]}])
        write_json(run / "verify.json", {"outcome": "succeeded", "passed": 1, "total": 1,
                                         "checks": [{"name": "csv", "passed": True, "location": "host"}]})
        view = next(r for r in console.snapshot(self.trial)["runs"] if r["id"] == "demo-r1")
        self.assertEqual((view["cell"], view["group"], view["attempt"]), ("demo", None, 1))
        self.assertEqual(view["teardown_commands"], [{"cmd": "aws s3 rb s3://demo", "exit_code": 1, "timed_out": False,
                                                      "location": "environment", "auth_errors": True}])
        self.assertEqual(view["environment_cleanup"]["reason"], "container still running")
        self.assertLessEqual(len(view["environment_cleanup"]["error"]), 300)
        self.assertEqual(view["verify"]["checks"][0]["location"], "host")
        self.assertIn("outputs_truncated", view)

    def test_the_snapshot_carries_the_model_options_and_journey_highlights(self):
        snap = console.snapshot(self.trial)
        self.assertEqual(snap["plan_model"]["workers"][0]["id"], "demo")
        names = {h["name"] for h in snap["plan_options"]["harnesses"]}
        self.assertIn("claude-code", names)
        self.assertIn("console-demo", names)
        self.assertEqual(snap["plan_options"]["auto_reporter"], "console-demo")
        self.assertTrue(snap["plan_options"]["prompt_lint"] and snap["plan_options"]["bash_only"])
        self.assertIn(snap["locks"]["kind"], (None, "attempts", "job"))
        steps = console.journey_highlights("## Chronological account\n1. \U0001f6a7 Roadblock. It failed [E-003].\n"
                                           "2. I read the help [E-004].\n\n## What helped\nnothing\n")
        self.assertEqual([(s["label"], s["refs"]) for s in steps], [("Roadblock", ["E-003"]), (None, ["E-004"])])
        self.assertEqual(steps[0]["text"], "It failed [E-003].")


if __name__ == "__main__":
    unittest.main()
