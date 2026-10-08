"""Offline integration of environment profiles with real synthetic processes."""

import json
import os
import shlex
import shutil
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))

from ajx import plugins, runner, spec as specmod  # noqa: E402
from ajx.base import Auth, Harness, empty_telemetry  # noqa: E402
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


@plugins.register("auth", "environment-copy")
class EnvironmentCopyAuth(Auth):
    """Copy only the synthetic credential created by a lifecycle test."""

    def prepare(self, ctx):
        shutil.copyfile(self.conf["source"], Path(ctx["config_dir"]) / "auth.json")

    def cleanup(self, ctx):
        (Path(ctx["config_dir"]) / "auth.json").unlink(missing_ok=True)


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
    def copied_auth_run(self, root):
        spec = trial_at(root)
        source = Path(root) / "synthetic-login.json"
        source.write_text('{"token": "synthetic-offline-credential"}')
        spec["auth"]["test"] = {"type": "environment-copy", "source": str(source)}
        run = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
        return spec, run, source

    def interrupt_with_evidence(self, run, source, interruption):
        def interrupt(*args, **kwargs):
            paths = run.state["paths"]
            self.assertEqual((Path(paths["config_dir"]) / "auth.json").read_text(), source.read_text())
            (Path(paths["workspace"]) / "partial-evidence.txt").write_text("keep the interrupted attempt")
            (Path(paths["config_dir"]) / "transcript.jsonl").write_text('{"event": "synthetic partial evidence"}\n')
            cached = Path(paths["home_dir"]) / ".synthetic"
            cached.mkdir(exist_ok=True)
            (cached / "credentials.json").write_text("synthetic cached credential")
            raise interruption
        return interrupt

    def assert_purged_evidence(self, paths, source):
        self.assertEqual(source.read_text(), '{"token": "synthetic-offline-credential"}')
        self.assertFalse((Path(paths["config_dir"]) / "auth.json").exists())
        self.assertFalse((Path(paths["home_dir"]) / ".synthetic" / "credentials.json").exists())
        self.assertEqual((Path(paths["workspace"]) / "partial-evidence.txt").read_text(),
                         "keep the interrupted attempt")
        self.assertEqual((Path(paths["config_dir"]) / "transcript.jsonl").read_text(),
                         '{"event": "synthetic partial evidence"}\n')

    def interrupted_prepare(self, root):
        spec, run, source = self.copied_auth_run(root)
        run.mark("prepare", "running")
        # Bypass go's exception handler to leave the same durable state as coordinator death.
        with mock.patch.object(run, "_command", side_effect=KeyboardInterrupt("synthetic coordinator crash")):
            with self.assertRaises(KeyboardInterrupt):
                run.stage_prepare()
        self.assertEqual(run.status("prepare"), "running")
        self.assertEqual((Path(run.state["paths"]["config_dir"]) / "auth.json").read_text(), source.read_text())
        (Path(run.state["paths"]["workspace"]) / "partial-evidence.txt").write_text("keep preparation evidence")
        (Path(run.state["paths"]["home_dir"]) / "credentials.json").write_text("synthetic cached credential")
        return spec, run, source

    def test_handled_interruption_archives_evidence_and_purges_auth_without_replay(self):
        for stage in ("prepare", "execute", "verify", "narrate"):
            with self.subTest(stage=stage), tempfile.TemporaryDirectory() as tmp:
                spec, run, source = self.copied_auth_run(tmp)
                interruption = KeyboardInterrupt("synthetic user interruption")
                target, method = {
                    "prepare": (run, "_command"),
                    "execute": (run.harness, "execute"),
                    "verify": (run, "stage_verify"),
                    "narrate": (run.harness, "narrate"),
                }[stage]
                with mock.patch.object(target, method,
                                       side_effect=self.interrupt_with_evidence(run, source, interruption)), \
                        mock.patch.object(run.runner, "abort", side_effect=AssertionError("adapter owns cancellation")) as abort:
                    with self.assertRaises(KeyboardInterrupt) as raised:
                        run.go()
                self.assertIs(raised.exception, interruption)
                abort.assert_not_called()
                self.assertEqual(run.status(stage), "interrupted", run.state)
                self.assertEqual(read_json(run.state_path)["stages"][stage]["status"], "interrupted")
                self.assertTrue(run.done("archive"), run.state)
                self.assertTrue(run.state["environment_cleanup"]["confirmed"])
                self.assert_purged_evidence(run.state["archived"], source)
                paths, owned, environment = (dict(run.state["paths"]), list(run.state["owned_paths"]),
                                             dict(run.state["environment"]))
                self.assertTrue(all(not Path(path).exists() for path in paths.values()))
                if stage == "prepare":
                    self.assertIsNone(run.status("execute"))
                    self.assertIsNone(run.status("teardown"))
                else:
                    self.assertTrue(run.done("teardown"), run.state)
                    self.assertEqual(run.state["teardown"]["status"], "ok")
                    self.assertTrue((run.run_dir / "agent-home" / "teardown-ran").exists())
                if stage == "execute":
                    self.assertEqual(run.state["execute"]["stop_reason"], "interrupted")
                    self.assertTrue(run.state["execute"]["interrupted"])
                    self.assertIsNotNone(run.state["execute"]["stopped_at"])
                resumed = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
                with mock.patch.object(resumed.runner, "plan") as plan, \
                        mock.patch.object(resumed.runner, "prepare") as prepare, \
                        mock.patch.object(resumed.harness, "execute") as execute:
                    resumed.go(["execute"])
                    resumed.go(["archive"])
                plan.assert_not_called()
                prepare.assert_not_called()
                execute.assert_not_called()
                self.assertEqual(resumed.state["paths"], paths)
                self.assertEqual(resumed.state["owned_paths"], owned)
                self.assertEqual(resumed.state["environment"], environment)
                self.assert_purged_evidence(resumed.state["archived"], source)

    def test_handled_interruption_keeps_requested_workspace_but_purges_credentials(self):
        for stage in ("prepare", "execute"):
            with self.subTest(stage=stage), tempfile.TemporaryDirectory() as tmp:
                spec, run, source = self.copied_auth_run(tmp)
                interruption = KeyboardInterrupt("synthetic user interruption")
                target, method = (run, "_command") if stage == "prepare" else (run.harness, "execute")
                with mock.patch.object(target, method,
                                       side_effect=self.interrupt_with_evidence(run, source, interruption)):
                    with self.assertRaises(KeyboardInterrupt) as raised:
                        run.go(keep_workspace=True)
                self.assertIs(raised.exception, interruption)
                self.assertEqual(run.status(stage), "interrupted")
                self.assertIsNone(run.status("archive"))
                self.assertNotIn("archived", run.state)
                self.assertTrue(run.state["environment_cleanup"]["confirmed"])
                if stage == "execute":
                    self.assertTrue(run.done("teardown"), run.state)
                self.assertEqual(run.state["interruption_cleanup"]["steps"]["credentials"]["status"], "done")
                self.assertTrue(all(Path(path).is_dir() for path in run.state["paths"].values()))
                self.assert_purged_evidence(run.state["paths"], source)
                resumed = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
                with mock.patch.object(resumed.runner, "plan") as plan, \
                        mock.patch.object(resumed.runner, "prepare") as prepare, \
                        mock.patch.object(resumed.harness, "execute") as execute:
                    resumed.go([stage], keep_workspace=True)
                plan.assert_not_called()
                prepare.assert_not_called()
                execute.assert_not_called()
                self.assertNotIn("archived", resumed.state)
                self.assertTrue(all(Path(path).is_dir() for path in resumed.state["paths"].values()))
                self.assert_purged_evidence(resumed.state["paths"], source)

    def test_handled_interruption_retains_failed_release_paths_and_purges_auth_before_retry(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec, run, source = self.copied_auth_run(tmp)
            interruption = KeyboardInterrupt("synthetic user interruption")
            with mock.patch.object(run.harness, "execute",
                                   side_effect=self.interrupt_with_evidence(run, source, interruption)), \
                    mock.patch.object(run.runner, "release", return_value={
                        "status": "failed", "confirmed": False, "error": "synthetic backend outage"}) as release:
                with self.assertRaises(KeyboardInterrupt) as raised:
                    run.go()
            self.assertIs(raised.exception, interruption)
            release.assert_called_once()
            self.assertEqual(run.status("release"), "error")
            self.assertEqual(run.status("archive"), "error")
            self.assertFalse(run.state["environment_cleanup"]["confirmed"])
            self.assertNotIn("archived", run.state)
            paths, owned, environment = (dict(run.state["paths"]), list(run.state["owned_paths"]),
                                         dict(run.state["environment"]))
            self.assertTrue(all(Path(path).is_dir() for path in paths.values()))
            self.assert_purged_evidence(paths, source)
            resumed = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            with mock.patch.object(resumed.runner, "release", wraps=resumed.runner.release) as release, \
                    mock.patch.object(resumed.runner, "plan") as plan, \
                    mock.patch.object(resumed.runner, "prepare") as prepare, \
                    mock.patch.object(resumed.harness, "execute") as execute:
                resumed.go(["release", "archive"])
                resumed.go(["execute"])
            release.assert_called_once()
            self.assertEqual(release.call_args.args[0]["state"]["paths"], paths)
            plan.assert_not_called()
            prepare.assert_not_called()
            execute.assert_not_called()
            self.assertTrue(resumed.done("archive"), resumed.state)
            self.assertTrue(resumed.state["environment_cleanup"]["confirmed"])
            self.assertEqual(resumed.state["paths"], paths)
            self.assertEqual(resumed.state["owned_paths"], owned)
            self.assertEqual(resumed.state["environment"], environment)
            self.assertTrue(all(not Path(path).exists() for path in paths.values()))
            self.assert_purged_evidence(resumed.state["archived"], source)

    def test_cleanup_interruption_preserves_original_exception_and_purges_known_credentials(self):
        for failed_cleanup in ("teardown", "release", "auth"):
            with self.subTest(cleanup=failed_cleanup), tempfile.TemporaryDirectory() as tmp:
                _, run, source = self.copied_auth_run(tmp)
                interruption = KeyboardInterrupt("original synthetic interruption")
                target, method = {
                    "teardown": (run, "stage_teardown"),
                    "release": (run.runner, "release"),
                    "auth": (run.auth, "cleanup"),
                }[failed_cleanup]
                with mock.patch.object(run.harness, "execute",
                                       side_effect=self.interrupt_with_evidence(run, source, interruption)), \
                        mock.patch.object(target, method, side_effect=SystemExit("synthetic cleanup interruption")):
                    with self.assertRaises(KeyboardInterrupt) as raised:
                        run.go()
                self.assertIs(raised.exception, interruption)
                self.assertEqual(run.status("execute"), "interrupted")
                if failed_cleanup == "teardown":
                    self.assertEqual(run.status("teardown"), "interrupted")
                    self.assertTrue(run.done("archive"), run.state)
                elif failed_cleanup == "release":
                    self.assertEqual(run.status("release"), "interrupted")
                    self.assertEqual(run.status("archive"), "error")
                    self.assertNotIn("archived", run.state)
                    self.assertIsNot((run.state.get("environment_cleanup") or {}).get("confirmed"), True)
                else:
                    self.assertEqual(run.status("archive"), "interrupted")
                    self.assertEqual(run.state["interruption_cleanup"]["steps"]["credentials"]["status"], "interrupted")
                self.assert_purged_evidence(run.state.get("archived") or run.state["paths"], source)

    def test_handled_interruption_archives_after_already_confirmed_release_without_backend(self):
        with tempfile.TemporaryDirectory() as tmp:
            _, run, source = self.copied_auth_run(tmp)
            run.go(["prepare", "execute", "verify", "teardown", "normalize", "narrate", "release"])
            confirmed = dict(run.state["environment_cleanup"])
            self.assertTrue(confirmed["confirmed"])
            interruption = KeyboardInterrupt("synthetic reporting interruption")
            with mock.patch.object(run, "stage_measure",
                                   side_effect=self.interrupt_with_evidence(run, source, interruption)), \
                    mock.patch.object(run.runner, "release", side_effect=AssertionError("backend unavailable")) as release:
                with self.assertRaises(KeyboardInterrupt) as raised:
                    run.go(["measure"])
            self.assertIs(raised.exception, interruption)
            release.assert_not_called()
            self.assertEqual(run.status("measure"), "interrupted")
            self.assertTrue(run.done("archive"), run.state)
            self.assertEqual(run.state["environment_cleanup"], confirmed)
            self.assert_purged_evidence(run.state["archived"], source)

    def test_interrupted_prepare_cleans_original_credentials_without_replaying(self):
        for stages in (None, ["prepare"]):
            with self.subTest(stages=stages), tempfile.TemporaryDirectory() as tmp:
                spec, interrupted, source = self.interrupted_prepare(tmp)
                paths = dict(interrupted.state["paths"])
                owned = list(interrupted.state["owned_paths"])
                handle = dict(interrupted.state["environment"])
                resumed = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
                with mock.patch.object(resumed.runner, "plan", wraps=resumed.runner.plan) as plan, \
                        mock.patch.object(resumed.runner, "prepare", wraps=resumed.runner.prepare) as prepare, \
                        mock.patch.object(resumed.auth, "cleanup", wraps=resumed.auth.cleanup) as cleanup, \
                        mock.patch.object(EnvironmentProbe, "execute", side_effect=AssertionError("must not execute")) as execute:
                    resumed.go(stages)
                    self.assertEqual(resumed.status("prepare"), "interrupted", resumed.state)
                    self.assertIsNone(resumed.status("execute"))
                    self.assertTrue(resumed.done("archive"), resumed.state)
                    self.assertTrue(resumed.state["environment_cleanup"]["confirmed"])
                    self.assertEqual(resumed.state["paths"], paths)
                    self.assertEqual(resumed.state["owned_paths"], owned)
                    self.assertEqual(resumed.state["environment"], handle)
                    self.assertEqual(cleanup.call_args.args[0]["config_dir"], paths["config_dir"])
                    self.assertTrue(all(not Path(path).exists() for path in paths.values()))
                    self.assertFalse((resumed.run_dir / "harness-config" / "auth.json").exists())
                    self.assertFalse((resumed.run_dir / "agent-home" / "credentials.json").exists())
                    self.assertEqual((resumed.run_dir / "workspace" / "partial-evidence.txt").read_text(),
                                     "keep preparation evidence")
                    self.assertTrue(source.exists(), "cleanup must preserve the original login")
                    resumed.go()
                    plan.assert_not_called()
                    prepare.assert_not_called()
                    execute.assert_not_called()
                    cleanup.assert_called_once()

    def test_interrupted_prepare_retries_unconfirmed_cleanup_with_original_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec, interrupted, source = self.interrupted_prepare(tmp)
            paths = dict(interrupted.state["paths"])
            resumed = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            with mock.patch.object(resumed.runner, "release", return_value={"status": "failed", "confirmed": False}), \
                    mock.patch.object(resumed.auth, "cleanup", wraps=resumed.auth.cleanup) as cleanup:
                resumed.go()
                self.assertEqual(resumed.status("prepare"), "interrupted", resumed.state)
                self.assertEqual(resumed.status("archive"), "error")
                self.assertEqual(resumed.state["paths"], paths)
                self.assertNotIn("archived", resumed.state)
                self.assertTrue(all(Path(path).exists() for path in paths.values()))
                self.assertFalse((Path(paths["config_dir"]) / "auth.json").exists())
                self.assertFalse((Path(paths["home_dir"]) / "credentials.json").exists())
                self.assertEqual(source.read_text(), '{"token": "synthetic-offline-credential"}')
                cleanup.assert_not_called()
            recovered = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            with mock.patch.object(EnvironmentProbe, "execute", side_effect=AssertionError("must not execute")) as execute:
                recovered.go(["prepare"])
            execute.assert_not_called()
            self.assertEqual(recovered.state["paths"], paths)
            self.assertTrue(recovered.done("archive"), recovered.state)
            self.assertTrue(recovered.state["environment_cleanup"]["confirmed"])
            self.assertTrue(all(not Path(path).exists() for path in paths.values()))
            self.assertFalse((recovered.run_dir / "harness-config" / "auth.json").exists())

    def test_archive_failed_release_purges_known_credentials_without_following_symlinks(self):
        with tempfile.TemporaryDirectory() as tmp:
            _, run, source = self.copied_auth_run(tmp)
            run.go(["prepare", "teardown"])
            paths = dict(run.state["paths"])
            config = Path(paths["config_dir"])
            outside = Path(tmp) / "outside"
            outside.mkdir()
            original = outside / "auth.json"
            original.write_text("synthetic original outside owned roots")
            nested = config / "nested"
            nested.mkdir()
            (nested / "tokens.json").write_text("synthetic staged token")
            (nested / "transcript.jsonl").write_text("retain evidence")
            (config / "redirected-directory").symlink_to(outside, target_is_directory=True)
            (config / "credentials.json").symlink_to(original)
            with mock.patch.object(run.runner, "release", return_value={"status": "failed", "confirmed": False}) as release, \
                    mock.patch.object(run.auth, "cleanup") as cleanup:
                run.go(["archive"])
            release.assert_called_once()
            cleanup.assert_not_called()
            self.assertEqual(run.status("archive"), "error")
            self.assertFalse(run.state["environment_cleanup"]["confirmed"])
            self.assertEqual(run.state["paths"], paths)
            self.assertNotIn("archived", run.state)
            self.assertFalse((config / "auth.json").exists())
            self.assertFalse((nested / "tokens.json").exists())
            self.assertFalse((config / "credentials.json").is_symlink())
            self.assertTrue((config / "redirected-directory").is_symlink())
            self.assertEqual((nested / "transcript.jsonl").read_text(), "retain evidence")
            self.assertEqual(original.read_text(), "synthetic original outside owned roots")
            self.assertEqual(source.read_text(), '{"token": "synthetic-offline-credential"}')

    def test_archive_failed_release_purge_stays_anchored_during_directory_replacement(self):
        for replacement in ("root", "ancestor", "descendant"):
            with self.subTest(replacement=replacement), tempfile.TemporaryDirectory() as tmp:
                _, run, source = self.copied_auth_run(tmp)
                run.go(["prepare", "teardown"])
                paths, owned = dict(run.state["paths"]), list(run.state["owned_paths"])
                config = Path(paths["config_dir"])
                outside = Path(tmp) / "outside"
                outside.mkdir()
                if replacement == "root":
                    watched, relative = config, Path()
                elif replacement == "ancestor":
                    watched, relative = config.parent, Path(config.name)
                else:
                    watched, relative = config / "nested", Path()
                    watched.mkdir()
                    (watched / "auth.json").write_text("synthetic staged credential")
                (watched / relative / "transcript.jsonl").write_text("retain evidence")
                target = outside / relative
                target.mkdir(parents=True, exist_ok=True)
                original = target / "auth.json"
                original.write_text("synthetic original outside owned roots")
                (target / "transcript.jsonl").write_text("outside evidence must remain untouched")
                retained = watched.with_name(watched.name + "-retained")
                identity = watched.stat()
                original_open = os.open
                swapped = False

                def replace_after_open(path, flags, *args, **kwargs):
                    nonlocal swapped
                    fd = original_open(path, flags, *args, **kwargs)
                    opened = os.fstat(fd)
                    if not swapped and (opened.st_dev, opened.st_ino) == (identity.st_dev, identity.st_ino):
                        watched.rename(retained)
                        watched.symlink_to(outside, target_is_directory=True)
                        swapped = True
                    return fd

                with mock.patch.object(run.runner, "release", return_value={"status": "failed", "confirmed": False}), \
                        mock.patch.object(run.auth, "cleanup") as cleanup, \
                        mock.patch.object(os, "open", side_effect=replace_after_open):
                    run.go(["archive"])
                self.assertTrue(swapped, "the race must occur after opening the intended directory")
                cleanup.assert_not_called()
                self.assertEqual(run.status("archive"), "error")
                self.assertFalse(run.state["environment_cleanup"]["confirmed"])
                self.assertEqual(run.state["paths"], paths)
                self.assertEqual(run.state["owned_paths"], owned)
                self.assertNotIn("archived", run.state)
                self.assertFalse((retained / relative / "auth.json").exists())
                self.assertEqual((retained / relative / "transcript.jsonl").read_text(), "retain evidence")
                self.assertEqual(original.read_text(), "synthetic original outside owned roots")
                self.assertEqual((target / "transcript.jsonl").read_text(), "outside evidence must remain untouched")
                self.assertEqual(source.read_text(), '{"token": "synthetic-offline-credential"}')

    def test_local_task_timeout_still_runs_default_environment_teardown(self):
        def timeout_worker(harness, ctx):
            code = "from pathlib import Path; import time; Path('task-started').write_text('once'); time.sleep(60)"
            return harness.run(ctx, [sys.executable, "-I", "-c", code], "execute", timeout=1)

        with tempfile.TemporaryDirectory() as tmp:
            spec = trial_at(tmp)
            self.assertEqual(spec["teardown"][0]["location"], "environment")
            run = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            stages = ["prepare", "execute", "teardown", "release", "archive"]
            with mock.patch.object(EnvironmentProbe, "execute", timeout_worker):
                run.go(stages)
            self.assertTrue(all(run.done(stage) for stage in stages), run.state)
            self.assertEqual(run.state["execute"]["stop_reason"], "timeout")
            self.assertTrue(run.state["execute"]["timed_out"])
            self.assertTrue((run.run_dir / "agent-home" / "teardown-ran").exists())
            self.assertEqual((run.run_dir / "workspace" / "task-started").read_text(), "once")
            self.assertEqual(run.state["teardown"]["status"], "ok")
            self.assertEqual(read_json(run.run_dir / "teardown.json")[0]["location"], "environment")
            self.assertTrue(run.state["environment_cleanup"]["confirmed"])
            self.assertTrue(all(not Path(path).exists() for path in run.state["paths"].values()))
            with mock.patch.object(EnvironmentProbe, "execute", side_effect=AssertionError("must not execute twice")) as execute:
                run.go(["execute"])
            execute.assert_not_called()

    def test_archive_keeps_confirmed_release_when_backend_becomes_unavailable(self):
        with tempfile.TemporaryDirectory() as tmp:
            spec = trial_at(tmp)
            source = Path(tmp) / "synthetic-login.json"
            source.write_text('{"token": "synthetic-offline-credential"}')
            spec["auth"]["test"] = {"type": "environment-copy", "source": str(source)}
            run = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            run.go(["prepare", "execute", "verify", "teardown", "normalize", "narrate", "release"])
            self.assertTrue(run.done("release"), run.state)
            confirmed = dict(run.state["environment_cleanup"])
            self.assertTrue(confirmed["confirmed"])
            self.assertTrue((Path(run.state["paths"]["config_dir"]) / "auth.json").exists())
            # Recover in a new coordinator, with the backend unavailable after its confirmed release.
            resumed = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
            with mock.patch.object(resumed.runner, "release", return_value={
                    "status": "failed", "confirmed": False, "error": "synthetic backend outage"}) as release, \
                    mock.patch.object(resumed.auth, "cleanup", wraps=resumed.auth.cleanup) as cleanup:
                resumed.go(["archive"])
            release.assert_not_called()
            cleanup.assert_called_once()
            self.assertTrue(resumed.done("archive"), resumed.state)
            self.assertEqual(resumed.state["environment_cleanup"], confirmed)
            self.assertFalse((resumed.run_dir / "harness-config" / "auth.json").exists())
            self.assertTrue(source.exists(), "the original login must remain intact")
            self.assertTrue(all(not Path(path).exists() for path in resumed.state["paths"].values()))

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
