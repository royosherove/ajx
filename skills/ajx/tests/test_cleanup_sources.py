"""Exercise CLI cleanup after the original profile inputs have disappeared."""

import contextlib
import io
import json
import os
import runpy
import shutil
import unittest
from pathlib import Path
from unittest import mock

import test_environments as container_fixtures
import test_profile_resume as profile_fixtures
from ajx import agent_configuration as ac, environments, runner, spec as specmod
from ajx.builtin.codex import Codex


class CleanupSourceTests(unittest.TestCase):
    def setUp(self):
        profile_fixtures.ProfileResumeTests.setUp(self)
        self.cli = runpy.run_path(str(Path(__file__).resolve().parents[1] / "bin" / "ajx"))["main"]
        self.fixture = self.root / "fixture"
        self.fixture.mkdir()
        (self.fixture / "input.txt").write_text("Synthetic starting material.\n")
        self.trial.write_text(self.trial.read_text().replace(
            'prompt_file = "task.md"', 'prompt_file = "task.md"\nfixture_dir = "fixture"'))

    def start_matrix(self, stages=("prepare", "execute")):
        spec = specmod.load(self.trial)
        runner.run_matrix(spec, stages=list(stages), synthesize=False, log=lambda _: None)
        attempt = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
        self.assertTrue(attempt.done("prepare"), attempt.state)
        return spec, attempt

    def cleanup(self, stages="teardown,release,render,archive"):
        with contextlib.redirect_stdout(io.StringIO()), \
                mock.patch.object(ac, "_read_tree", side_effect=AssertionError("cleanup must not inspect skills")), \
                mock.patch.object(Codex, "execute", side_effect=AssertionError("cleanup must not execute")) as execute, \
                mock.patch.object(Codex, "narrate", side_effect=AssertionError("cleanup must not narrate")) as narrate, \
                mock.patch.object(Codex, "report", side_effect=AssertionError("cleanup must not synthesize")) as report:
            self.assertEqual(self.cli(["run", str(self.trial), "--stages", stages]), 0)
            execute.assert_not_called()
            narrate.assert_not_called()
            report.assert_not_called()

    def assert_cleaned(self, spec):
        resumed = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
        profile_fixtures.ProfileResumeTests.assert_cleaned(self, resumed)

    def test_cli_cleanup_after_skill_prompt_and_fixture_are_deleted(self):
        spec, attempt = self.start_matrix()
        plan = attempt.out_dir / "matrix-plan.json"
        original_plan = plan.read_bytes()
        shutil.rmtree(self.source)
        shutil.rmtree(self.fixture)
        (self.root / "task.md").unlink()
        self.cleanup()
        self.assert_cleaned(spec)
        self.assertEqual(plan.read_bytes(), original_plan)
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(self.cli(["status", str(self.trial)]), 0)

    def test_changed_source_and_declaration_do_not_redirect_cleanup(self):
        spec, attempt = self.start_matrix()
        original_plan = (attempt.out_dir / "matrix-plan.json").read_bytes()
        (self.source / "guide.txt").write_text("Changed synthetic instructions.\n")
        self.trial.write_text(self.trial.read_text().replace("skills/product-guide", "missing/replacement"))
        with self.assertRaises(specmod.SpecError):
            specmod.load(self.trial)
        self.cleanup()
        self.assert_cleaned(spec)
        self.assertEqual((attempt.out_dir / "matrix-plan.json").read_bytes(), original_plan)

    def test_cleanup_skips_repetitions_without_state(self):
        spec, attempt = self.start_matrix()
        self.trial.write_text(self.trial.read_text().replace(
            'product = "synthetic"', 'product = "synthetic"\nrepetitions = 3'))
        shutil.rmtree(self.source)
        self.cleanup()
        self.assert_cleaned(spec)
        self.assertEqual(sorted(path.name for path in (attempt.out_dir / "runs").iterdir()), ["local-r1"])

    def test_saved_profiles_cannot_launch_or_prepare_an_attempt(self):
        _, attempt = self.start_matrix()
        shutil.rmtree(self.source)
        recovered = specmod.load(self.trial, cleanup_only=True)
        original_state = attempt.state_path.read_bytes()
        resumed = runner.Run(recovered, recovered["cells"][0], 1, lambda _: None)
        for stages in (None, ["prepare"], ["execute"], ["narrate"], ["extract"],
                       ["measure"], ["teardown", "execute"]):
            with self.subTest(stages=stages):
                with self.assertRaisesRegex(specmod.SpecError, "only teardown"):
                    runner.run_matrix(recovered, stages=stages, log=lambda _: None)
                with self.assertRaisesRegex(specmod.SpecError, "only teardown"):
                    resumed.go(stages)
        with self.assertRaisesRegex(specmod.SpecError, "cannot prepare"):
            resumed.stage_prepare()
        self.assertEqual(attempt.state_path.read_bytes(), original_state)
        self.cleanup()

    def test_fresh_execution_still_validates_original_sources(self):
        _, attempt = self.start_matrix()
        original_state = attempt.state_path.read_bytes()
        shutil.rmtree(self.source)
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(self.cli(["validate", str(self.trial)]), 2)
            self.assertEqual(self.cli(["run", str(self.trial)]), 2)
            self.assertEqual(self.cli(["run", str(self.trial), "--stages", "prepare,execute"]), 2)
        self.assertEqual(attempt.state_path.read_bytes(), original_state)
        self.cleanup()

    def test_another_cell_cannot_replace_the_saved_source_contract(self):
        spec, attempt = self.start_matrix()
        plan = attempt.out_dir / "matrix-plan.json"
        original_plan = plan.read_bytes()
        self.trial.write_text(self.trial.read_text() + """
[[cells]]
id = "later"
harness = "codex"
auth = "test"
""")
        (self.source / "guide.txt").write_text("Different synthetic instructions.\n")
        changed = specmod.load(self.trial)
        with self.assertRaisesRegex(specmod.SpecError, "new trial output directory"):
            runner.run_matrix(changed, only_cells=["later"], stages=["prepare"], log=lambda _: None)
        self.assertEqual(plan.read_bytes(), original_plan)
        self.assertFalse((attempt.out_dir / "runs" / "later-r1").exists())
        self.cleanup()
        self.assert_cleaned(spec)

    def test_source_metadata_changes_allow_normal_resume(self):
        spec, attempt = self.start_matrix()
        source = self.source / "SKILL.md"
        before = source.stat()
        os.utime(source, ns=(before.st_atime_ns, before.st_mtime_ns + 1_000_000_000))
        reloaded = specmod.load(self.trial)
        self.assertNotEqual(
            spec["agent_configurations"]["guided"]["skills"]["entries"][0]["_identity"],
            reloaded["agent_configurations"]["guided"]["skills"]["entries"][0]["_identity"])
        runner.run_matrix(reloaded, stages=["verify", "teardown", "normalize"],
                          synthesize=False, log=lambda _: None)
        resumed = runner.Run(reloaded, reloaded["cells"][0], 1, lambda _: None)
        self.assertTrue(resumed.done("verify"), resumed.state)
        self.assertEqual((Path(resumed.state["paths"]["workspace"]) / "executions.txt").read_text(),
                         "one execution")
        self.cleanup()
        self.assert_cleaned(spec)

    def test_deleted_mount_source_still_releases_the_original_container(self):
        self.engine = container_fixtures.FakeDocker()
        self.executions = []
        self.trial.write_text(self.trial.read_text().replace(
            'agent_configuration = "guided"', 'agent_configuration = "guided"\nenvironment = "container"') + f"""
[environments.container]
backend = "container"
image = {json.dumps(container_fixtures.IMAGE)}
mounts = [{{source = "fixture", target = "/inputs", access = "read"}}]
""")
        def backend(spec, cell):
            return container_fixtures.FakeRunner(specmod.environment_for(spec, cell), self.engine)

        def process(*args, **kwargs):
            return container_fixtures.ContainerEnvironmentTest.fake_process(self, *args, **kwargs)

        with mock.patch.object(specmod, "runner_for", side_effect=backend), \
                mock.patch.object(environments, "_tail_process", side_effect=process):
            spec, attempt = self.start_matrix(stages=["prepare"])
            identity = self.engine.containers[attempt.state["environment"]["name"]]["Id"]
            self.assertEqual(self.engine.count(["run"]), 1)
            shutil.rmtree(self.fixture)
            shutil.rmtree(self.source)
            self.cleanup()
            resumed = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            self.assertTrue(resumed.state["environment_cleanup"]["confirmed"], resumed.state)
            self.assertTrue(resumed.done("archive"), resumed.state)
            self.assertEqual(self.engine.count(["run"]), 1)
            self.assertIn(["container", "rm", "--force", identity], self.engine.calls)
            self.assertFalse(self.engine.containers)
