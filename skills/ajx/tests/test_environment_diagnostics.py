"""Offline diagnostics tests: container creation failures, the doctor bind-mount probe, and the
one-shot re-inspection of a starting directory that changes while being inspected.

Everything runs against the synthetic FakeDocker boundary from test_environments.py; no Docker,
agent, or model process is started.

    python3 -I skills/ajx/tests/test_environment_diagnostics.py
"""

import contextlib
import importlib.util
import io
import json
import os
import stat
import sys
import tempfile
import types
import unittest
from importlib.machinery import SourceFileLoader
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))
sys.path.insert(0, str(HERE))

from ajx import environments as envmod, plugins, spec as specmod  # noqa: E402
from ajx.builtin.codex import Codex  # noqa: E402
from ajx.environments import EnvironmentError, EnvironmentRunner, normalize_profiles  # noqa: E402
from test_environments import IMAGE, FakeDocker, FakeRunner, context, persist, profile  # noqa: E402

# What a VM-based engine (Colima, Docker Desktop) prints for a workspace root it does not share.
BIND_ERROR = ('docker: Error response from daemon: invalid mount config for type "bind": '
              "bind source path does not exist: /private/tmp/ajx-example/workspace.\n\n"
              "Run 'docker run --help' for more information\n")
STAT_FIELDS = ("st_mode", "st_ino", "st_dev", "st_nlink", "st_size", "st_mtime_ns", "st_ctime_ns")


class DiagnosticDocker(FakeDocker):
    """FakeDocker plus engine-style stderr for a refused creation and the doctor's throwaway probe run."""

    def __init__(self):
        super().__init__()
        self.create_stderr = BIND_ERROR
        self.probe_stderr = None      # None: the probe exits 0; text: it exits 125 with this stderr
        self.probe_error = None       # text: the probe's docker client itself fails (transport/timeout)
        self.probe_runs = []

    def call(self, args, **kwargs):
        args = list(args)
        if args[0] == "run" and "--detach" not in args:
            self.calls.append(args)
            if self.probe_error:
                raise EnvironmentError(self.probe_error)
            mount = dict(v.split("=", 1) if "=" in v else (v, True) for v in self._option(args, "--mount").split(","))
            source = Path(mount["source"])
            self.probe_runs.append({"args": args, "mount": mount, "source": source, "existed": source.is_dir(),
                                    "mode": stat.S_IMODE(source.lstat().st_mode) if source.exists() else None})
            if self.probe_stderr is not None:
                return {"code": 125, "stdout": "", "stderr": self.probe_stderr}
            return {"code": 0, "stdout": "", "stderr": ""}
        result = super().call(args, **kwargs)
        if args[0] == "run" and self.fail_create:
            # The engine refused the run: nothing was created and stderr says why.
            self.containers.pop(self._option(args, "--name"), None)
            return {"code": 125, "stdout": "", "stderr": self.create_stderr}
        return result


class DiagnosticsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ajx-env-diagnostics-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.base = self.root / "trial"
        self.base.mkdir()
        self.engine = DiagnosticDocker()

    def runner(self, **options):
        return FakeRunner(profile(self.base, **options), self.engine)

    @staticmethod
    def doctor_ctx(workspace_root):
        return {"spec": {"trial": {"workspace_root": str(workspace_root)}}, "cell": {"id": "c"}}


class CreationFailureTest(DiagnosticsTest):
    def test_creation_failure_names_exit_code_stderr_tail_and_vm_hint(self):
        runner = self.runner()
        ctx = context(self.root)
        persist(runner, ctx)
        self.engine.fail_create = True
        with self.assertRaisesRegex(EnvironmentError, "Docker creation failed") as raised:
            runner.prepare(ctx)
        message = str(raised.exception)
        self.assertIn("Docker creation failed (exit code 125): docker: Error response from daemon: "
                      'invalid mount config for type "bind": bind source path does not exist: '
                      "/private/tmp/ajx-example/workspace.", message)
        self.assertIn("; no fallback or task retry was attempted", message)
        self.assertIn("; hint: " + envmod._VM_MOUNT_HINT, message)
        self.assertNotIn("\n", message)
        cleanup = json.loads("\n".join(raised.exception.__notes__).split("Environment preparation cleanup: ", 1)[1])
        self.assertEqual(cleanup["status"], "already_absent")
        self.assertEqual(cleanup["reason"], "not present; container creation failed (see creation_failure)")
        self.assertEqual(cleanup["creation_failure"]["exit_code"], 125)
        self.assertIn("bind source path does not exist: /private/tmp/ajx-example/workspace",
                      cleanup["creation_failure"]["stderr_tail"])
        self.assertEqual(cleanup["creation_failure"]["hint"], envmod._VM_MOUNT_HINT)
        self.assertEqual(self.engine.containers, {})
        # The run's cleanup stage persists release() into environment.json; the record keeps its known cause.
        released = runner.release(ctx)
        self.assertEqual(released["status"], "already_absent")
        self.assertEqual(released["creation_failure"], cleanup["creation_failure"])
        with self.assertRaises(EnvironmentError):
            runner.prepare(ctx)
        self.assertEqual(self.engine.count(["run"]), 1)

    def test_creation_failure_without_a_known_engine_limit_has_no_hint(self):
        runner = self.runner()
        ctx = context(self.root)
        persist(runner, ctx)
        self.engine.fail_create = True
        self.engine.create_stderr = "docker: Error response from daemon: OCI runtime create failed: synthetic\n"
        with self.assertRaises(EnvironmentError) as raised:
            runner.prepare(ctx)
        self.assertEqual(str(raised.exception),
                         "Docker creation failed (exit code 125): docker: Error response from daemon: "
                         "OCI runtime create failed: synthetic; no fallback or task retry was attempted")
        self.assertNotIn("hint", runner.release(ctx)["creation_failure"])

    def test_successful_lifecycle_records_no_creation_failure(self):
        runner = self.runner()
        ctx = context(self.root)
        persist(runner, ctx)
        self.engine.fail_create = True
        with self.assertRaises(EnvironmentError):
            runner.prepare(ctx)
        fresh = self.runner()
        fresh_ctx = context(self.root, "second")
        persist(fresh, fresh_ctx)
        self.engine.fail_create = False
        released = fresh.release(fresh_ctx)
        self.assertEqual(released["status"], "already_absent")
        self.assertNotIn("creation_failure", released)
        self.assertEqual(released["reason"], "not present; expiry/startup failure cannot be distinguished")

    def test_stderr_tail_is_one_bounded_printable_line(self):
        noisy = "\n".join(f"line {i}" for i in range(20)) + "\r\n\n\tlast\tline\n"
        self.assertEqual(envmod._stderr_tail(noisy), "line 15 line 16 line 17 line 18 line 19 last line")
        bounded = envmod._stderr_tail("x" * 1000)
        self.assertEqual(len(bounded), 600)
        self.assertTrue(bounded.endswith("..."))
        self.assertEqual(envmod._stderr_tail(None), "")
        self.assertEqual(envmod._stderr_tail("\x1b[31mred\x1b[0m"), "[31mred[0m")
        self.assertEqual(envmod._docker_error("Docker image inspect failed", {"exit_code": 1, "stderr_tail": ""},
                                              consequence="check the daemon"),
                         "Docker image inspect failed (exit code 1): (no stderr); check the daemon")
        for text in ("Error: mounts denied: the path /x is not shared from the host",
                     "bind source path does not exist: /private/tmp/x"):
            self.assertEqual(envmod._docker_failure({"code": 125, "stderr": text})["hint"], envmod._VM_MOUNT_HINT)
        self.assertNotIn("hint", envmod._docker_failure({"code": 125, "stderr": "no space left on device"}))

    def test_image_inspect_failure_includes_the_engine_stderr(self):
        runner = self.runner()
        original = self.engine.call

        def call(args, **kwargs):
            if list(args)[:2] == ["image", "inspect"]:
                return {"code": 1, "stdout": "[]\n", "stderr": f"Error response from daemon: No such image: {IMAGE}\n"}
            return original(args, **kwargs)

        self.engine.call = call
        report = runner.doctor(self.doctor_ctx(self.root))
        self.assertEqual(report["status"], "unavailable")
        self.assertEqual(report["error"], f"Docker image inspect failed (exit code 1): Error response from daemon: "
                                          f"No such image: {IMAGE}; check the daemon and preloaded pinned image")
        self.assertEqual(report["mount_probe"], {"status": "skipped", "reason": "image/daemon checks failed"})
        self.assertEqual(self.engine.probe_runs, [])
        with self.assertRaisesRegex(EnvironmentError, "No such image"):
            runner.plan(context(self.root))


class DoctorMountProbeTest(DiagnosticsTest):
    def test_ok_probe_uses_a_private_directory_under_workspace_root_in_a_hardened_container(self):
        runner = self.runner(platform="linux/amd64")
        workspace_root = self.root / "work"
        workspace_root.mkdir()
        report = runner.doctor(self.doctor_ctx(workspace_root))
        self.assertEqual(report["status"], "available")
        self.assertNotIn("error", report)
        self.assertEqual(report["mount_probe"], {"status": "ok", "workspace_root": str(workspace_root),
                                                 "stderr_tail": "", "hint": None})
        [run] = self.engine.probe_runs
        self.assertTrue(run["existed"])
        self.assertEqual(run["mode"], 0o700)
        self.assertEqual(run["source"].parent, workspace_root)
        self.assertTrue(run["source"].name.startswith("ajx-doctor-probe-"), run["source"])
        self.assertFalse(run["source"].exists())
        self.assertEqual(list(workspace_root.iterdir()), [])
        args = run["args"]
        padded = " " + " ".join(args) + " "
        for flags in ("--rm", "--pull never", "--network none", "--read-only", "--cap-drop ALL",
                      "--security-opt no-new-privileges=true", "--ipc none", "--log-driver none",
                      "--platform linux/amd64", "--entrypoint /usr/bin/env"):
            self.assertIn(" " + flags + " ", padded)
        for flag in ("--detach", "--privileged", "--cap-add", "--volume", "-v", "--device", "--name"):
            self.assertNotIn(flag, args)
        self.assertRegex(args[args.index("--user") + 1], r"^[1-9][0-9]*:[1-9][0-9]*$")
        self.assertEqual(run["mount"], {"type": "bind", "source": str(run["source"]), "target": "/ajx-probe",
                                        "bind-propagation": "rprivate", "bind-recursive": "disabled", "readonly": True})
        self.assertEqual(args[args.index(IMAGE) + 1:],
                         ["-i", "PATH=" + envmod._PATH, "/bin/sh", "-c", "test -d /ajx-probe"])
        self.assertIn("IMAGE_SECRET=", args)  # image ENV is cleared, as for the real worker container

    def test_failed_probe_marks_the_profile_unavailable_with_stderr_and_hint(self):
        runner = self.runner()
        self.engine.probe_stderr = BIND_ERROR
        report = runner.doctor(self.doctor_ctx(self.root))
        self.assertEqual(report["status"], "unavailable")
        probe = report["mount_probe"]
        self.assertEqual(probe["status"], "failed")
        self.assertEqual(probe["workspace_root"], str(self.root))
        self.assertEqual(probe["exit_code"], 125)
        self.assertIn("bind source path does not exist: /private/tmp/ajx-example/workspace", probe["stderr_tail"])
        self.assertEqual(probe["hint"], envmod._VM_MOUNT_HINT)
        self.assertEqual(report["error"], probe["error"])
        self.assertTrue(report["error"].startswith(
            f"Workspace bind-mount probe failed for trial.workspace_root {self.root} (exit code 125): "
            "docker: Error response from daemon: invalid mount config"), report["error"])
        self.assertTrue(report["error"].endswith("; hint: " + envmod._VM_MOUNT_HINT), report["error"])
        self.assertNotIn("\n", report["error"])
        self.assertFalse(self.engine.probe_runs[0]["source"].exists())
        self.assertEqual(report["image"]["id"], IMAGE)  # image/daemon checks still ran and are reported

    def test_failed_probe_without_a_known_engine_limit_has_no_hint(self):
        runner = self.runner()
        self.engine.probe_stderr = "docker: Error response from daemon: synthetic runtime failure\n"
        report = runner.doctor(self.doctor_ctx(self.root))
        self.assertEqual(report["status"], "unavailable")
        self.assertIsNone(report["mount_probe"]["hint"])
        self.assertEqual(report["error"], f"Workspace bind-mount probe failed for trial.workspace_root {self.root} "
                                          "(exit code 125): docker: Error response from daemon: synthetic runtime failure")

    def test_probe_client_failure_is_reported_and_the_directory_is_still_removed(self):
        runner = self.runner()
        self.engine.probe_error = "Docker run unavailable (TimeoutExpired); check the local daemon"
        before = set(self.root.iterdir())
        report = runner.doctor(self.doctor_ctx(self.root))
        self.assertEqual(report["status"], "unavailable")
        self.assertEqual(report["mount_probe"]["status"], "failed")
        self.assertIn("Workspace bind-mount probe could not run", report["error"])
        self.assertIn("TimeoutExpired", report["error"])
        self.assertEqual(set(self.root.iterdir()), before)

    def test_probe_is_skipped_without_a_workspace_root_for_a_missing_root_and_for_local(self):
        runner = self.runner()
        for ctx in (None, {}, {"spec": {}}, {"spec": {"trial": {}}}):
            report = runner.doctor(ctx)
            self.assertEqual(report["status"], "available")
            self.assertEqual(report["mount_probe"],
                             {"status": "skipped", "reason": "no trial.workspace_root in the doctor context"})
        report = runner.doctor(self.doctor_ctx(self.root / "missing"))
        self.assertEqual(report["status"], "available")
        self.assertEqual(report["mount_probe"]["status"], "skipped")
        self.assertIn("does not exist yet", report["mount_probe"]["reason"])
        self.assertEqual(self.engine.probe_runs, [])
        local = EnvironmentRunner(normalize_profiles({"ajx-local": {"backend": "local"}}, self.base)["ajx-local"])
        report = local.doctor(self.doctor_ctx(self.root))
        self.assertEqual(report["status"], "available")
        self.assertEqual(report["mount_probe"],
                         {"status": "skipped", "reason": "the local backend has no container bind mounts"})


class DoctorCommandTest(DiagnosticsTest):
    """`ajx doctor` prints the probe on one indented line and keeps the backend prerequisites line."""

    def setUp(self):
        super().setUp()
        login = self.root / "login"
        login.mkdir()
        (login / "auth.json").write_text('{"synthetic": "not-a-real-credential"}\n', encoding="utf-8")
        (login / "auth.json").chmod(0o600)
        (self.base / "task.md").write_text("Create the synthetic result file.\n", encoding="utf-8")
        self.workspace_root = self.root / "workspaces"
        self.workspace_root.mkdir()
        self.trial = self.base / "trial.toml"
        self.trial.write_text(f"""
[trial]
id = "diagnostics"
product = "synthetic"
workspace_root = {json.dumps(str(self.workspace_root))}
output_dir = "results"

[task]
prompt_file = "task.md"

[auth.test]
type = "codex-chatgpt"
codex_home = {json.dumps(str(login))}

[reporter]
harness = "codex"
auth = "test"

[environments.container]
backend = "container"
image = {json.dumps(IMAGE)}

[[cells]]
id = "boxed"
harness = "codex"
auth = "test"
environment = "container"
""", encoding="utf-8")
        patches = [
            mock.patch.object(plugins, "plugin_dirs", return_value=[]),
            mock.patch.object(Codex, "available", lambda self: True),
            mock.patch.object(Codex, "version", lambda self: "synthetic"),
            mock.patch.object(specmod, "runner_for", side_effect=lambda spec, cell: FakeRunner(
                specmod.environment_for(spec, cell), self.engine)),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def doctor(self):
        path = str(HERE.parent / "bin" / "ajx")
        loader = SourceFileLoader("ajx_cli_diagnostics", path)
        module = importlib.util.module_from_spec(
            importlib.util.spec_from_file_location("ajx_cli_diagnostics", path, loader=loader))
        loader.exec_module(module)
        out = io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()):
            code = module.main(["doctor", str(self.trial)])
        return code, out.getvalue()

    def test_doctor_prints_the_probe_result_and_fails_the_trial_on_a_failed_probe(self):
        code, out = self.doctor()
        self.assertIn("    backend prerequisites: available\n", out)
        self.assertIn(f"    workspace bind-mount probe: ok ({self.workspace_root})\n", out)
        self.assertEqual(list(self.workspace_root.iterdir()), [])
        self.assertNotIn("NOT READY", out)
        self.assertEqual(code, 0, out)
        self.engine.probe_stderr = BIND_ERROR
        code, out = self.doctor()
        self.assertEqual(code, 1, out)
        self.assertIn("    backend prerequisites: FAILED\n", out)
        self.assertIn(f"    workspace bind-mount probe: FAILED ({self.workspace_root})\n", out)
        self.assertIn(f"    Workspace bind-mount probe failed for trial.workspace_root {self.workspace_root} "
                      "(exit code 125): docker: Error response from daemon", out)
        self.assertIn("; hint: " + envmod._VM_MOUNT_HINT, out)
        self.assertIn("NOT READY", out)
        self.assertEqual(list(self.workspace_root.iterdir()), [])


class ReinspectionTest(unittest.TestCase):
    """A starting directory that changes under inspection is named precisely and re-inspected once."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ajx-reinspect-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.source = self.root / "trial" / "fixtures"
        (self.source / "sub").mkdir(parents=True)
        (self.source / "input.txt").write_bytes(b"synthetic starting data\n")
        (self.source / "sub" / "nested.txt").write_bytes(b"nested synthetic data\n")
        self.field = "environment.mounts[0].source"
        self.destination = self.root / "attempt" / "workspace" / ".ajx-mounts" / "0"

    @staticmethod
    def altered_fstat(target, attribute, failing_passes, *, per_pass=2, position=1):
        """Patch os.fstat so one fstat of `target` per inspection pass reports a changed attribute.

        A regular file is fstat'ed twice per pass (after open and after the read); a directory once.
        Only the chosen position of the listed passes (1-based) is altered; everything else is real.
        """
        ino = target.stat().st_ino
        real, seen = os.fstat, []

        def fake(fd):
            info = real(fd)
            if info.st_ino != ino:
                return info
            seen.append(fd)
            pass_number, index = divmod(len(seen) - 1, per_pass)
            if index == position and pass_number + 1 in failing_passes:
                values = {name: getattr(info, name) for name in STAT_FIELDS}
                values[attribute] += 1
                return types.SimpleNamespace(**values)
            return info

        return mock.patch.object(os, "fstat", side_effect=fake)

    def copied(self):
        return sorted(str(p.relative_to(self.destination)) for p in self.destination.rglob("*"))

    def test_change_names_entry_and_attribute_then_a_fresh_pass_succeeds(self):
        clean = envmod._tree_manifest(self.source, self.field)
        discarded = []
        real_discard = envmod._discard_partial_copy

        def discard(destination):
            partial = sorted(str(p.relative_to(destination)) for p in Path(destination).rglob("*"))
            removed = real_discard(destination)
            discarded.append((partial, removed, Path(destination).exists()))
            return removed

        with self.altered_fstat(self.source / "sub" / "nested.txt", "st_ctime_ns", {1}), \
                mock.patch.object(envmod.time, "sleep") as sleep, \
                mock.patch.object(envmod, "_discard_partial_copy", side_effect=discard):
            result = envmod._tree_manifest(self.source, "environment.mount.snapshot", destination=self.destination)
        self.assertEqual(result, clean)
        sleep.assert_called_once_with(envmod._REINSPECT_DELAY)
        # Pass one had copied everything before nested.txt's post-read check failed; the retry started over.
        self.assertEqual(discarded, [(["input.txt", "sub", "sub/nested.txt"], True, False)])
        self.assertEqual(self.copied(), ["input.txt", "sub", "sub/nested.txt"])
        for name in ("input.txt", "sub/nested.txt"):
            self.assertEqual((self.destination / name).read_bytes(), (self.source / name).read_bytes())

    def test_reproduced_change_fails_with_the_entry_attribute_and_reinspection_marker(self):
        with self.altered_fstat(self.source / "input.txt", "st_mtime_ns", {1, 2}), \
                mock.patch.object(envmod.time, "sleep") as sleep:
            with self.assertRaises(envmod._InspectionChanged) as raised:
                envmod._tree_manifest(self.source, self.field, destination=self.destination)
        self.assertIsInstance(raised.exception, ValueError)
        self.assertEqual(str(raised.exception), f"{self.field}: starting directory changed while being inspected: "
                                                "input.txt (mtime) (reproduced on re-inspection)")
        sleep.assert_called_once_with(envmod._REINSPECT_DELAY)

    def test_directory_identity_change_names_the_directory(self):
        with self.altered_fstat(self.source / "sub", "st_ino", {1, 2}, per_pass=1, position=0), \
                mock.patch.object(envmod.time, "sleep"):
            with self.assertRaises(ValueError) as raised:
                envmod._tree_manifest(self.source, self.field)
        self.assertEqual(str(raised.exception), f"{self.field}: starting directory changed while being inspected: "
                                                "sub/ (dev/ino) (reproduced on re-inspection)")

    def test_several_changed_attributes_are_all_named_and_a_clean_second_pass_passes_validation(self):
        with self.altered_fstat(self.source / "input.txt", "st_ctime_ns", {1}), \
                mock.patch.object(envmod.time, "sleep"):
            conf = normalize_profiles({"test": {"backend": "container", "image": IMAGE, "mounts": [
                {"source": "fixtures", "target": "/fixtures", "access": "read"}]}}, self.root / "trial")
        self.assertEqual(conf["test"]["mounts"][0]["source"], str(self.source))
        with self.altered_fstat(self.source / "input.txt", "st_ctime_ns", {1, 2}), \
                mock.patch.object(envmod.time, "sleep"):
            with self.assertRaisesRegex(ValueError, r"environments\.test\.mounts\[0\]\.source: starting directory "
                                        r"changed while being inspected: input\.txt \(ctime\) \(reproduced on re-inspection\)"):
                normalize_profiles({"test": {"backend": "container", "image": IMAGE, "mounts": [
                    {"source": "fixtures", "target": "/fixtures", "access": "read"}]}}, self.root / "trial")
        before = os.stat(self.source / "input.txt")
        after = types.SimpleNamespace(**{name: getattr(before, name) for name in STAT_FIELDS})
        after.st_size += 1
        after.st_mtime_ns += 1
        self.assertEqual(envmod._stat_changes(before, after, ("size", "mtime", "ctime", "nlink")), ["size", "mtime"])
        self.assertEqual(envmod._stat_changes(before, before, ("dev/ino", "type", "size", "nlink")), [])


if __name__ == "__main__":
    unittest.main()
