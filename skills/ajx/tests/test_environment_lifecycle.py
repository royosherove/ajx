"""Offline integration of environment profiles with real synthetic processes."""

import json
import os
import shlex
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))

from ajx import plugins, runner, spec as specmod  # noqa: E402
from ajx.base import Harness, empty_telemetry  # noqa: E402
from ajx.util import context_shell, read_json, write_json  # noqa: E402


@plugins.register("harness", "environment-probe")
class EnvironmentProbe(Harness):
    binary = sys.executable
    version_argv = [sys.executable, "--version"]
    default_auth = "env"
    clean_supported = True
    can_resume = True

    def execute(self, ctx):
        code = """
import json, os
from pathlib import Path
home = Path(os.environ["HOME"])
assert (home / "installed-tool").read_text() == "ready"
assert "AJX_TEST_AMBIENT_SECRET" not in os.environ
out = {"home": str(home), "workspace": str(Path.cwd()), "tool": "ready"}
Path("result.json").write_text(json.dumps(out))
Path("executions.txt").write_text("one execution")
print("synthetic task completed")
"""
        result = self.run(ctx, [sys.executable, "-I", "-c", code], "execute")
        return {**result, "session_id": "synthetic-environment-session"}

    def narrate(self, ctx, prompt):
        code = """
import os
from pathlib import Path
assert (Path(os.environ["HOME"]) / "installed-tool").read_text() == "ready"
assert (Path(os.environ["HOME"]) / "teardown-ran").exists()
print("same environment available for narration")
"""
        result = self.run(ctx, [sys.executable, "-I", "-c", code], "narrate")
        return {**result, "tools_disabled": True}

    def normalize(self, ctx, stage, exclude_message_ids=()):
        return empty_telemetry(
            session_id="synthetic-environment-session",
            final_text=("This is a synthetic narration used only by an offline lifecycle test. "
                        "The worker used a tool prepared in its isolated home, wrote its result "
                        "in the workspace, and reused the same environment for narration after "
                        "verification and teardown. No model provider was called.")
            if stage == "narrate" else "synthetic task completed",
            stop_reason="completed",
        )


def trial_at(root, *, setup='printf ready > "$HOME/installed-tool"', extra=""):
    root = Path(root)
    (root / "task.md").write_text("Create the synthetic result file.")
    (root / "fixture").mkdir(exist_ok=True)
    text = f"""
[trial]
id = "environment-lifecycle"
product = "synthetic"
workspace_root = {json.dumps(str(root / "workspaces"))}
output_dir = "results"
timeout_seconds = 10
environment = "local"

[task]
prompt_file = "task.md"
fixture_dir = "fixture"

[environments.local]
backend = "local"

[auth.test]
type = "env"
isolates_config = true

[reporter]
harness = "codex"

[[setup]]
run = {json.dumps(setup)}

[[verify]]
type = "file_exists"
name = "result"
path = "result.json"
contains = "ready"

[[teardown]]
run = 'touch "$HOME/teardown-ran"'

[[cells]]
id = "local"
harness = "environment-probe"
auth = "test"
{extra}
"""
    path = root / "trial.toml"
    path.write_text(text)
    return specmod.load(path)


def no_model_extract(run):
    write_json(run.run_dir / "asks.json", {
        "asks": [], "strengths": [], "gates": [], "journey_errata": [],
        "no_obstacles_observed": True, "extraction_status": "ok", "extractor": {},
    })


class EnvironmentLifecycleTests(unittest.TestCase):
    def test_local_lifecycle_isolated_home_and_environment_survives_to_narration(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = trial_at(tmp)
            run = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            caller_home = os.environ.get("HOME")
            with mock.patch.dict(os.environ, {"AJX_TEST_AMBIENT_SECRET": "synthetic-do-not-forward"}), \
                    mock.patch.object(runner.Run, "stage_extract", no_model_extract):
                run.go()
            self.assertTrue(all(run.done(stage) for stage in runner.STAGES), run.state)
            self.assertEqual(read_json(run.run_dir / "verify.json")["outcome"], "succeeded")
            result = read_json(run.run_dir / "workspace" / "result.json")
            self.assertNotEqual(result["home"], caller_home)
            self.assertEqual(result["home"], run.state["paths"]["home_dir"])
            self.assertTrue((run.run_dir / "agent-home" / "teardown-ran").exists())
            self.assertEqual(os.environ.get("HOME"), caller_home)
            self.assertIn("environment_cleanup", read_json(run.run_dir / "run.json"))
            self.assertTrue(run.state["environment_contract_sha256"])
            self.assertTrue(all(not Path(p).exists() for p in run.state["paths"].values()))
            again = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            with mock.patch.object(EnvironmentProbe, "execute", side_effect=AssertionError("must not execute twice")):
                again.go()
                again.go(["release", "render", "archive"])
            self.assertTrue(again.done("release"), again.state)
            self.assertTrue(again.done("archive"), again.state)
            self.assertEqual(again.state["environment_cleanup"]["confirmed"], True)
            self.assertEqual((run.run_dir / "workspace" / "executions.txt").read_text(), "one execution")

    def test_changed_profile_cannot_resume_an_existing_attempt(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = trial_at(tmp)
            run = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            try:
                run.go(["prepare"])
                self.assertTrue(run.done("prepare"), run.state)
                spec["cells"][0]["args"] = ["changed-configuration"]
                with self.assertRaisesRegex(specmod.SpecError, "changed"):
                    runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            finally:
                run.stage_archive()

    def test_host_check_does_not_use_worker_installed_executable_or_home(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = trial_at(tmp)
            run = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            try:
                run.go(["prepare"])
                self.assertTrue(run.done("prepare"), run.state)
                ctx = run.ctx()
                runtime = Path(sys.executable).name
                planted = Path(ctx["home_dir"]) / ".local" / "bin" / runtime
                planted.write_text("#!/bin/sh\nprintf 'worker replacement\\n'\n")
                planted.chmod(0o700)
                self.assertEqual(run.runner.shell(shlex.quote(runtime), ctx)["stdout"], "worker replacement\n")
                code = 'import json,os;print(json.dumps({"runtime":"coordinator","home":os.environ["HOME"]}))'
                host_path = str(Path(sys.executable).parent) + os.pathsep + os.defpath
                with mock.patch.dict(os.environ, {"PATH": host_path}):
                    result = context_shell(shlex.quote(runtime) + " -I -c " + shlex.quote(code), ctx,
                                           location="host")
                self.assertEqual(result["exit_code"], 0, result)
                observed = json.loads(result["stdout"])
                self.assertEqual(observed["runtime"], "coordinator")
                self.assertNotEqual(observed["home"], ctx["home_dir"])
                self.assertTrue(Path(observed["home"]).is_relative_to(run.run_dir))
                self.assertEqual(result["location"], "host")
            finally:
                run.stage_archive()
            self.assertFalse((run.run_dir / "host-environment").exists())

    def test_prepare_failure_releases_owned_state_without_starting_worker(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = trial_at(tmp, setup="exit 9")
            run = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            with mock.patch.object(EnvironmentProbe, "execute", side_effect=AssertionError("worker must not start")):
                run.go()
            self.assertEqual(run.status("prepare"), "error")
            self.assertIsNone(run.status("execute"))
            self.assertTrue(run.done("archive"), run.state)
            self.assertTrue(all(not Path(p).exists() for p in run.state["paths"].values()))
            self.assertIn("environment_cleanup", run.state)
            original_paths = dict(run.state["paths"])
            sentinel = run.run_dir / "workspace" / "failure-evidence.txt"
            sentinel.write_text("preserve the failed attempt")
            again = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            with mock.patch.object(EnvironmentProbe, "execute", side_effect=AssertionError("failed prepare cannot execute")):
                again.go()
            self.assertIsNone(again.status("execute"), again.state)
            self.assertEqual(again.state["paths"], original_paths)
            self.assertEqual(sentinel.read_text(), "preserve the failed attempt")

    def test_fixture_symlink_is_rejected_without_reading_target(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = trial_at(tmp)
            secret = Path(tmp) / "unrelated.txt"
            secret.write_text("synthetic outside fixture")
            (Path(tmp) / "fixture" / "linked").symlink_to(secret)
            run = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            run.go()
            self.assertEqual(run.status("prepare"), "error")
            self.assertIn("symlinks", run.state["stages"]["prepare"]["error"])
            self.assertFalse((run.run_dir / "workspace" / "linked").exists())
            self.assertEqual(secret.read_text(), "synthetic outside fixture")

    def test_check_locations_are_explicit_and_invalid_combination_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = trial_at(tmp)
            self.assertEqual(spec["setup"][0]["location"], "environment")
            self.assertEqual(spec["verify"][0]["location"], "host")
            self.assertEqual(spec["teardown"][0]["location"], "environment")
            path = Path(spec["path"])
            path.write_text(path.read_text().replace('type = "file_exists"', 'type = "file_exists"\nlocation = "environment"'))
            with self.assertRaisesRegex(specmod.SpecError, "only shell"):
                specmod.load(path)

    def test_profiles_cannot_be_combined_with_legacy_runner(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(specmod.SpecError, "legacy runner"):
                trial_at(tmp, extra='runner = "docker"')


if __name__ == "__main__":
    unittest.main()
