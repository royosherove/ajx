"""Synthetic ownership regressions, plus an opt-in Docker/root lifecycle test.

Offline:
    python3 -I skills/ajx/tests/test_environment_ownership.py

Real root ownership (preloaded immutable Linux image; no pulls or model calls):
    sudo env AJX_TEST_CONTAINER_IMAGE="$AJX_TEST_CONTAINER_IMAGE" \
        "$(command -v python3)" -I skills/ajx/tests/test_environment_ownership.py
"""

import copy
import json
import os
import shlex
import stat
import sys
import types
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))
sys.path.insert(0, str(HERE))

from ajx import base, environments as envmod, runner as runmod, spec as specmod  # noqa: E402
from ajx.environments import EnvironmentError, EnvironmentRunner  # noqa: E402
import test_environments as fixtures  # noqa: E402


def identity(info):
    return info.st_dev, info.st_ino


class WorkerOwnershipTests(fixtures.TempTest):
    """Use real filesystem traversal, modeling only root identity and fchown."""

    def setUp(self):
        super().setUp()
        self.owners = {}
        self.changes = []
        self.real_fstat = os.fstat
        self.enterContext(mock.patch.object(envmod.os, "getuid", return_value=0))
        self.enterContext(mock.patch.object(envmod.os, "geteuid", return_value=0))
        self.enterContext(mock.patch.object(envmod.os, "getgid", return_value=0))
        self.enterContext(mock.patch.object(envmod.os, "fstat", side_effect=self.simulated_stat))
        self.chown = self.enterContext(mock.patch.object(envmod.os, "fchown", side_effect=self.record_chown))
        self.enterContext(mock.patch.object(
            envmod.os, "chown", side_effect=AssertionError("ownership changes must use pinned descriptors")))

    def simulated_stat(self, fd):
        info = self.real_fstat(fd)
        values = {name: getattr(info, name) for name in dir(info) if name.startswith("st_")}
        values["st_uid"], values["st_gid"] = self.owners.get(identity(info), (0, 0))
        return types.SimpleNamespace(**values)

    def record_chown(self, fd, uid, gid):
        key = identity(self.real_fstat(fd))
        self.changes.append((key, uid, gid))
        self.owners[key] = uid, gid

    def owner(self, path):
        return self.owners.get(identity(Path(path).lstat()), (0, 0))

    def prepared(self, name="attempt", **options):
        engine = fixtures.FakeDocker()
        runner = fixtures.FakeRunner(fixtures.profile(self.base, **options), engine)
        ctx = fixtures.context(self.root, name)
        ctx["state"]["paths"] = {key: str(ctx[key]) for key in envmod._ROOTS}
        ctx["state"]["owned_paths"] = list(ctx["state"]["paths"].values())
        handle = fixtures.persist(runner, ctx)
        # Inventory is unrelated to ownership; the real preparation/lifecycle code runs.
        with mock.patch.object(runner, "_report", return_value={"status": "prepared"}):
            runner.prepare(ctx)
        self.changes.clear()
        return runner, ctx, handle

    def test_preparation_and_repair_cover_only_the_four_owned_roots(self):
        runner, ctx, handle = self.prepared()
        worker = tuple(map(int, handle["worker_user"].split(":")))
        for key in envmod._ROOTS:
            self.assertEqual(self.owner(ctx[key]), worker)
        untouched = [ctx["run_dir"], Path(handle["docker_config_dir"]),
                     runner._marker_path(ctx, handle)]
        created = []
        for key in envmod._ROOTS:
            directory = Path(ctx[key]) / (".git" if key == "workspace" else "host-setup")
            directory.mkdir(mode=0o700)
            output = directory / "root-created"
            output.write_text("synthetic host setup\n")
            output.chmod(0o600)
            created.extend((directory, output))
        self.assertTrue(all(self.owner(path) == (0, 0) for path in created))
        original_plan = copy.deepcopy(handle)
        docker_calls = list(runner.engine.calls)
        self.assertIsNone(runner.restore_worker_ownership(ctx))
        self.assertTrue(all(self.owner(path) == worker for path in created))
        self.assertTrue(all(self.owner(path) == (0, 0) for path in untouched))
        self.assertEqual({key for key, _, _ in self.changes},
                         {identity(path.lstat()) for path in created})
        self.assertEqual(handle, original_plan)
        self.assertEqual(runner.engine.calls, docker_calls, "repair must not provision or replay an attempt")
        self.changes.clear()
        runner.restore_worker_ownership(ctx)
        self.assertEqual(self.changes, [], "already-correct ownership should be left alone")

    def test_uses_planned_identity_after_coordinator_identity_changes_and_recovery(self):
        with mock.patch.object(envmod.os, "getuid", return_value=23001), \
                mock.patch.object(envmod.os, "getgid", return_value=24002):
            runner, ctx, handle = self.prepared()
        output = ctx["config_dir"] / "staged-auth.json"
        output.write_text('{"token": "synthetic-only"}')
        output.chmod(0o600)
        recovered = fixtures.FakeRunner(runner.profile, runner.engine)
        recovered.restore_worker_ownership(ctx)
        self.assertEqual(handle["worker_user"], "23001:24002")
        self.assertEqual(self.owner(output), (23001, 24002))
        self.assertTrue(all((uid, gid) == (23001, 24002) for _, uid, gid in self.changes))

    def test_extra_sources_and_state_paths_do_not_expand_the_ownership_scope(self):
        read_source = self.fixture("read-source")
        write_source = self.fixture("write-source")
        runner, ctx, handle = self.prepared(mounts=[
            {"source": str(read_source), "target": "/input", "access": "read"},
            {"source": str(write_source), "target": "/output", "access": "write"},
        ])
        extra = self.root / "unrelated"
        extra.mkdir()
        sentinel = extra / "sentinel"
        sentinel.write_text("synthetic unrelated data")
        ctx["state"]["owned_paths"].append(str(extra))
        ctx["state"]["paths"]["unrelated"] = str(extra)
        ctx["state"]["archived"] = {"workspace": str(extra)}
        snapshot = Path(handle["mounts"][1]["host_source"])
        output = snapshot / "host-output"
        output.write_text("new output in the owned snapshot")
        runner.restore_worker_ownership(ctx)
        worker = tuple(map(int, handle["worker_user"].split(":")))
        self.assertEqual(self.owner(output), worker)
        self.assertEqual(self.owner(snapshot / "input.txt"), worker)
        for path in (extra, sentinel, read_source, read_source / "input.txt",
                     write_source, write_source / "input.txt"):
            self.assertEqual(self.owner(path), (0, 0), path)
        self.assertEqual((write_source / "input.txt").read_text(), "synthetic starting data\n")

    def test_symlinks_hardlinks_and_special_files_are_left_untouched(self):
        runner, ctx, _ = self.prepared()
        outside = self.fixture("outside")
        target = outside / "input.txt"
        workspace = ctx["workspace"]
        ordinary = workspace / "ordinary"
        ordinary.write_text("owned file")
        links = [workspace / name for name in ("external-file", "external-dir", "internal-file", "dangling")]
        links[0].symlink_to(target)
        links[1].symlink_to(outside, target_is_directory=True)
        links[2].symlink_to(ordinary.name)
        links[3].symlink_to("missing")
        shared = workspace / "hardlinked"
        os.link(target, shared)
        fifo = workspace / "fifo"
        os.mkfifo(fifo)
        runner.restore_worker_ownership(ctx)
        self.assertNotEqual(self.owner(ordinary), (0, 0))
        for path in [outside, target, shared, fifo, *links]:
            self.assertEqual(self.owner(path), (0, 0), path)
        self.assertEqual(target.read_text(), "synthetic starting data\n")
        self.assertTrue(all(path.is_symlink() for path in links))
        self.assertTrue(stat.S_ISFIFO(fifo.lstat().st_mode))

    def test_changed_root_identities_are_rejected_before_any_chown(self):
        for key in envmod._ROOTS:
            with self.subTest(root=key):
                runner, ctx, _ = self.prepared(name="replaced-" + key)
                original = ctx[key].with_name(key + "-original")
                ctx[key].rename(original)
                ctx[key].mkdir()
                (ctx[key] / "sentinel").write_text("replacement directory")
                with self.assertRaisesRegex(EnvironmentError, "identity changed"):
                    runner.restore_worker_ownership(ctx)
                self.assertEqual(self.changes, [])

    def test_symlinked_root_or_parent_is_rejected(self):
        runner, ctx, _ = self.prepared()
        outside = self.fixture("outside")
        ctx["workspace"].rename(ctx["workspace"].with_name("original"))
        ctx["workspace"].symlink_to(outside, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "symlink"):
            runner.restore_worker_ownership(ctx)
        self.assertEqual(self.changes, [])
        runner, ctx, _ = self.prepared(name="parent-link")
        parent = ctx["workspace"].parent
        parent.rename(parent.with_name("original-parent"))
        parent.symlink_to(outside, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "symlink"):
            runner.restore_worker_ownership(ctx)
        self.assertEqual(self.changes, [])
        self.assertEqual(self.owner(outside / "input.txt"), (0, 0))

    def test_missing_roots_and_retargeted_contexts_cannot_be_repaired(self):
        runner, ctx, _ = self.prepared()
        ctx["workspace"].rmdir()
        with self.assertRaisesRegex(EnvironmentError, "Cannot safely restore"):
            runner.restore_worker_ownership(ctx)
        self.assertFalse(ctx["workspace"].exists())
        self.assertEqual(self.changes, [])
        runner, ctx, _ = self.prepared(name="retargeted")
        ctx["workspace"] = self.fixture("unrelated")
        with self.assertRaisesRegex(EnvironmentError, "paths changed"):
            runner.restore_worker_ownership(ctx)
        self.assertEqual(self.changes, [])

    def test_owned_path_metadata_cannot_withdraw_an_original_root(self):
        runner, ctx, _ = self.prepared()
        ctx["state"]["owned_paths"].remove(str(ctx["home_dir"]))
        with self.assertRaisesRegex(EnvironmentError, "no longer recorded as owned"):
            runner.restore_worker_ownership(ctx)
        self.assertEqual(self.changes, [])

    def test_unconfirmed_released_and_invalid_identity_attempts_are_rejected(self):
        runner, ctx, handle = self.prepared()
        marker = runner._marker_path(ctx, handle)
        marker.unlink()
        with self.assertRaisesRegex(EnvironmentError, "not confirmed"):
            runner.restore_worker_ownership(ctx)
        self.assertEqual(self.changes, [])
        runner, ctx, handle = self.prepared(name="released")
        runner.release(ctx)
        with self.assertRaisesRegex(EnvironmentError, "released"):
            runner.restore_worker_ownership(ctx)
        self.assertEqual(self.changes, [])
        runner, ctx, handle = self.prepared(name="invalid-user")
        for user in ("0:1000", "1000:0", "-1:1000", "1000:-1", "root", "1000:1000:1000"):
            with self.subTest(user=user):
                handle["worker_user"] = user
                with self.assertRaisesRegex(EnvironmentError, "worker identity"):
                    runner.restore_worker_ownership(ctx)
                self.assertEqual(self.changes, [])

    def test_local_and_effectively_nonroot_coordinators_noop_without_a_plan(self):
        local = EnvironmentRunner({"backend": "local", "id": "local"})
        container = fixtures.FakeRunner(fixtures.profile(self.base), fixtures.FakeDocker())
        for runner, effective_uid in ((local, 0), (local, 1234), (container, 1234)):
            with self.subTest(backend=runner.backend, effective_uid=effective_uid), \
                    mock.patch.object(envmod.os, "geteuid", return_value=effective_uid), \
                    mock.patch.object(runner, "_handle", side_effect=AssertionError("must not inspect paths")):
                self.assertIsNone(runner.restore_worker_ownership({}))
        self.assertEqual(self.changes, [])

    def test_symlink_swap_between_stat_and_open_cannot_escape_or_leak_descriptors(self):
        for directory in (False, True):
            with self.subTest(directory=directory):
                runner, ctx, _ = self.prepared(name="swap-" + str(directory))
                outside = self.fixture("outside-" + str(directory))
                target = outside if directory else outside / "input.txt"
                victim = ctx["workspace"] / "victim"
                if directory:
                    victim.mkdir()
                    (victim / "owned").write_text("owned")
                else:
                    victim.write_text("owned")
                open_file, descriptors = os.open, []
                swapped = False

                def replace_then_open(path, flags, *args, **kwargs):
                    nonlocal swapped
                    if path == "victim" and kwargs.get("dir_fd") is not None and not swapped:
                        swapped = True
                        victim.rename(victim.with_name("original-victim"))
                        victim.symlink_to(target, target_is_directory=directory)
                    fd = open_file(path, flags, *args, **kwargs)
                    descriptors.append(fd)
                    return fd

                with mock.patch.object(envmod.os, "open", side_effect=replace_then_open):
                    with self.assertRaisesRegex(EnvironmentError, "Cannot safely restore"):
                        runner.restore_worker_ownership(ctx)
                self.assertTrue(swapped)
                self.assertEqual(self.owner(outside), (0, 0))
                self.assertEqual(self.owner(outside / "input.txt"), (0, 0))
                for fd in set(descriptors):
                    with self.assertRaises(OSError):
                        self.real_fstat(fd)

    def test_root_swap_after_validation_still_uses_original_open_directory(self):
        runner, ctx, _ = self.prepared()
        outside = self.fixture("outside")
        output = ctx["workspace"] / "output"
        output.write_text("host output")
        original = ctx["workspace"].with_name("original-workspace")
        chown_tree = envmod._chown_owned_tree
        swapped = False

        def swap_then_chown(fd, uid, gid):
            nonlocal swapped
            if not swapped:
                swapped = True
                ctx["workspace"].rename(original)
                ctx["workspace"].symlink_to(outside, target_is_directory=True)
            return chown_tree(fd, uid, gid)

        with mock.patch.object(envmod, "_chown_owned_tree", side_effect=swap_then_chown):
            runner.restore_worker_ownership(ctx)
        self.assertNotEqual(self.owner(original / "output"), (0, 0))
        self.assertEqual(self.owner(outside / "input.txt"), (0, 0))

    def test_same_device_nested_directory_and_file_mounts_are_not_chowned(self):
        runner, ctx, _ = self.prepared()
        mounted = ctx["workspace"] / "mounted"
        mounted.mkdir()
        sentinel = mounted / "sentinel"
        sentinel.write_text("unrelated mounted data")
        file_mount = ctx["workspace"] / "mounted-file"
        file_mount.write_text("unrelated bind-mounted file")
        ordinary = ctx["workspace"] / "owned"
        ordinary.write_text("host setup")
        mount_points = {identity(path.stat()) for path in (mounted, file_mount)}
        original_mount = envmod._ownership_mount

        def separate_mount(fd):
            value = original_mount(fd)
            return (value[0], "synthetic-other-mount") if identity(self.real_fstat(fd)) in mount_points else value

        with mock.patch.object(envmod, "_ownership_mount", side_effect=separate_mount):
            runner.restore_worker_ownership(ctx)
        self.assertNotEqual(self.owner(ordinary), (0, 0))
        for path in (mounted, sentinel, file_mount):
            self.assertEqual(self.owner(path), (0, 0), path)

    def test_chown_failure_is_reported_and_all_open_descriptors_are_closed(self):
        runner, ctx, _ = self.prepared()
        (ctx["workspace"] / "host-output").write_text("synthetic")
        open_file, descriptors = os.open, []

        def record_open(*args, **kwargs):
            fd = open_file(*args, **kwargs)
            descriptors.append(fd)
            return fd

        with mock.patch.object(envmod.os, "open", side_effect=record_open), \
                mock.patch.object(envmod.os, "fchown", side_effect=PermissionError("synthetic denial")):
            with self.assertRaisesRegex(EnvironmentError, "Cannot safely restore.*PermissionError"):
                runner.restore_worker_ownership(ctx)
        for fd in set(descriptors):
            with self.assertRaises(OSError):
                self.real_fstat(fd)


@unittest.skipUnless(os.environ.get("AJX_TEST_CONTAINER_IMAGE") and os.getuid() == 0,
                     "set AJX_TEST_CONTAINER_IMAGE and run as root to exercise real Docker ownership")
class RealDockerRootOwnershipTests(fixtures.TempTest):
    def test_root_host_setup_then_nonroot_container_setup_and_task(self):
        read_source = self.fixture("read-source")
        write_source = self.fixture("write-source")
        unrelated = self.fixture("unrelated")
        sentinel = unrelated / "input.txt"
        untouched = (read_source, read_source / "input.txt", write_source, write_source / "input.txt",
                     unrelated, sentinel)
        original_owners = {path: (path.stat().st_uid, path.stat().st_gid) for path in untouched}
        (self.base / "task.md").write_text("Modify synthetic files created by host setup.")
        trial = self.base / "trial.toml"
        host_setup = (
            'set -eu; test "$(id -u)" = 0; umask 077; mkdir .git; '
            'printf host > .git/config; printf host > host-owned.txt; '
            f'ln -s {shlex.quote(str(unrelated))} .git/external; '
            f'ln -s {shlex.quote(str(sentinel))} external-file; '
            f'ln {shlex.quote(str(sentinel))} external-hardlink'
        )
        trial.write_text(f"""
[trial]
id = "root-ownership"
product = "synthetic"
workspace_root = {json.dumps(str(self.root / "workspaces"))}
output_dir = "results"
timeout_seconds = 30
environment = "container"

[task]
prompt_file = "task.md"

[environments.container]
backend = "container"
image = {json.dumps(os.environ["AJX_TEST_CONTAINER_IMAGE"])}
mounts = [
    {{source = "read-source", target = "/input", access = "read"}},
    {{source = "write-source", target = "/output", access = "write"}}
]

[auth.test]
type = "env"
isolates_config = true

[reporter]
harness = "codex"

[[setup]]
location = "environment"
run = 'test "$(cat "$XDG_CONFIG_HOME/auth.json")" = synthetic-staged-auth'

[[setup]]
location = "host"
run = {json.dumps(host_setup)}

[[setup]]
location = "environment"
run = 'set -eu; printf "|setup" >> .git/config; printf "|setup" >> host-owned.txt; printf copied >> /output/input.txt'

[[setup]]
location = "host"
run = 'set -eu; test "$(id -u)" = 0; umask 077; printf later > .git/later'

[[verify]]
type = "file_exists"
name = "worker modified host file"
path = "host-owned.txt"
contains = "host|setup|worker"

[[teardown]]
run = 'test "$(cat .git/later)" = "later|worker"'

[[cells]]
id = "shell"
harness = "codex"
auth = "test"
""")
        spec = specmod.load(trial)
        run = runmod.Run(spec, spec["cells"][0], 1, lambda _: None)

        class ShellHarness(base.Harness):
            binary = "/bin/sh"

            def execute(self, ctx):
                script = (
                    'set -eu; test "$(id -u)" != 0; '
                    'printf "|worker" >> .git/config; printf "|worker" >> .git/later; '
                    'printf "|worker" >> host-owned.txt; printf created > .git/worker-created; '
                    'printf "|worker" >> "$HOME/host-created/output"; '
                    'printf "|worker" >> "$XDG_CONFIG_HOME/host-created/output"; '
                    'printf "|worker" >> "$XDG_CACHE_HOME/host-created/output"; '
                    'cat /input/input.txt; printf completed'
                )
                return self.run(ctx, ["/bin/sh", "-c", script], "execute")

        run.harness = ShellHarness()

        def release():
            if run.state.get("environment"):
                cleanup = run.runner.release(run.ctx())
                self.assertTrue(cleanup["confirmed"], cleanup)

        self.addCleanup(release)

        def stage_synthetic_auth(ctx):
            auth = Path(ctx["config_dir"]) / "auth.json"
            auth.write_text("synthetic-staged-auth")
            auth.chmod(0o600)
            self.assertEqual(auth.stat().st_uid, 0)
            for key in ("home_dir", "config_dir", "cache_dir"):
                directory = Path(ctx[key]) / "host-created"
                directory.mkdir(mode=0o700)
                output = directory / "output"
                output.write_text("host")
                output.chmod(0o600)

        with mock.patch.object(run.auth, "prepare", side_effect=stage_synthetic_auth):
            # Use the real Run/setup/harness/verification lifecycle without any reporting calls.
            run.go(["prepare", "execute", "verify"])
        for stage in ("prepare", "execute", "verify"):
            self.assertTrue(run.done(stage), run.state["stages"])
        self.assertEqual(run.state["execute"]["exit_code"], 0, run.state["execute"])
        self.assertEqual(json.loads((run.run_dir / "verify.json").read_text())["outcome"], "succeeded")
        handle = run.state["environment"]
        worker = tuple(map(int, handle["worker_user"].split(":")))
        self.assertNotEqual(worker[0], 0)
        paths = {key: Path(path) for key, path in handle["paths"].items()}
        workspace = paths["workspace"]
        self.assertEqual((workspace / ".git/config").read_text(), "host|setup|worker")
        self.assertEqual((workspace / ".git/later").read_text(), "later|worker")
        self.assertEqual((workspace / ".git/worker-created").read_text(), "created")
        for path in [workspace / ".git", workspace / ".git/config", workspace / "host-owned.txt",
                     paths["config_dir"] / "auth.json",
                     *[paths[key] / "host-created/output" for key in ("home_dir", "config_dir", "cache_dir")]]:
            self.assertEqual((path.stat().st_uid, path.stat().st_gid), worker, path)
        for path, owner in original_owners.items():
            self.assertEqual((path.stat().st_uid, path.stat().st_gid), owner, path)
        for source in (read_source, write_source, unrelated):
            self.assertEqual((source / "input.txt").read_text(), "synthetic starting data\n")
        for path in (workspace / ".git/external", workspace / "external-file"):
            self.assertTrue(path.is_symlink())
            self.assertEqual(path.lstat().st_uid, 0)
        self.assertEqual((workspace / "external-hardlink").stat().st_uid, original_owners[sentinel][0])
        self.assertEqual((Path(handle["mounts"][1]["host_source"]) / "input.txt").read_text(),
                         "synthetic starting data\ncopied")
        run.go(["teardown", "release", "archive"])
        self.assertTrue(run.done("archive"), run.state["stages"])
        self.assertTrue(run.state["environment_cleanup"]["confirmed"])
        self.assertEqual((run.run_dir / "workspace/host-owned.txt").read_text(), "host|setup|worker")
        self.assertFalse((Path(run.state["archived"]["config_dir"]) / "auth.json").exists())


if __name__ == "__main__":
    unittest.main()
