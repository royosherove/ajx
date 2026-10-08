"""Offline environment tests, plus opt-in real Docker boundary tests.

Ordinary invocation is offline:
    python3 -I skills/ajx/tests/test_environments.py

Real Docker tests additionally run when AJX_TEST_CONTAINER_IMAGE is a preloaded
immutable Linux image ID/digest. The image needs Python 3, /bin/sh, /usr/bin/env,
/bin/sleep, id, and uname (for example, a CI-resolved python:3-alpine image).
The tests never pull images, use credentials, or call an agent/model provider.
"""

import copy
import json
import os
import shlex
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))

from ajx import base as basemod, environments as envmod  # noqa: E402
from ajx.environments import EnvironmentError, EnvironmentRunner, normalize_profiles, validate_for_cell  # noqa: E402


IMAGE = "sha256:" + "a" * 64
IMAGE_SECRET = "synthetic-image-env-value"


def context(root, name="attempt", *, create=True):
    attempt = root / name
    run_dir = root / "reports" / name
    run_dir.mkdir(parents=True, exist_ok=True)
    ctx = {"run_dir": run_dir, "state": {}, "env": {}, "unset_env": [], "timeout": 10,
           "cell": {"id": name}}
    for key in ("workspace", "home_dir", "config_dir", "cache_dir"):
        path = attempt / key
        if create:
            path.mkdir(parents=True, mode=0o700)
        ctx[key] = path
    return ctx


def profile(root, **options):
    return normalize_profiles({"test": {"backend": "container", "image": IMAGE, **options}}, root)["test"]


def persist(runner, ctx):
    ctx["state"]["environment"] = runner.plan(ctx)
    # Simulate the caller's durable JSON round-trip, not an alias to runner memory.
    ctx["state"]["environment"] = json.loads(json.dumps(ctx["state"]["environment"]))
    return ctx["state"]["environment"]


def result(stdout="", *, exit_code=0, timed_out=False):
    return {"exit_code": exit_code, "timed_out": timed_out, "stdout": stdout, "stderr": "",
            "started_at": "2026-01-01T00:00:00Z", "stopped_at": "2026-01-01T00:00:01Z"}


class FakeDocker:
    """An in-memory Docker command boundary; it never executes Docker or shell commands."""

    def __init__(self):
        self.calls = []
        self.containers = {}
        self.image = {"Id": IMAGE, "Os": "linux", "Architecture": "amd64", "RepoDigests": [],
                      "Config": {"Env": ["HOME=/root", "PATH=/image-only",
                                         "IMAGE_SECRET=" + IMAGE_SECRET, "BASH_ENV=/image-startup"], "Volumes": None}}
        self.info = {"OSType": "linux", "Architecture": "x86_64", "ServerVersion": "synthetic",
                     "MemoryLimit": True, "SwapLimit": True, "CpuCfsQuota": True, "PidsLimit": True,
                     "SecurityOptions": ["name=seccomp,profile=builtin", "name=cgroupns"]}
        self.fail_create = False
        self.fail_cancel = False
        self.fail_remove = False
        self.unreachable = False
        self.replace_at_remove = False
        self.sequence = 0

    @staticmethod
    def _option(args, flag):
        return args[args.index(flag) + 1]

    @staticmethod
    def _options(args, flag):
        return [args[i + 1] for i, a in enumerate(args) if a == flag]

    def call(self, args, **kwargs):
        args = list(args)
        self.calls.append(args)
        if self.unreachable:
            raise EnvironmentError("synthetic Docker transport failure")
        if args[:2] == ["image", "inspect"]:
            return {"code": 0, "stdout": json.dumps([self.image]), "stderr": ""}
        if args[:1] == ["info"]:
            return {"code": 0, "stdout": json.dumps(self.info), "stderr": ""}
        if args[0] == "run":
            self.sequence += 1
            identity = f"{self.sequence:064x}"
            name = self._option(args, "--name")
            labels = dict(v.split("=", 1) for v in self._options(args, "--label"))
            actual_mounts, configured_mounts = [], []
            for value in self._options(args, "--mount"):
                fields = dict(v.split("=", 1) if "=" in v else (v, True) for v in value.split(","))
                actual_mounts.append({"Type": "bind", "Source": fields["source"], "Destination": fields["target"],
                                      "RW": not fields.get("readonly", False), "Propagation": "rprivate"})
                configured_mounts.append({"Type": "bind", "Source": fields["source"], "Target": fields["target"],
                                          "ReadOnly": bool(fields.get("readonly")),
                                          "BindOptions": {"Propagation": fields["bind-propagation"],
                                                          "NonRecursive": fields["bind-recursive"] == "disabled"}})
            tmpfs_path, tmpfs_options = self._option(args, "--tmpfs").split(":", 1)
            # Docker can expose tmpfs here; it must not be mistaken for an unapproved volume.
            actual_mounts.append({"Type": "tmpfs", "Source": "", "Destination": tmpfs_path, "RW": True})
            host = {
                "ReadonlyRootfs": "--read-only" in args, "Privileged": False, "CapAdd": [],
                "CapDrop": self._options(args, "--cap-drop"), "SecurityOpt": self._options(args, "--security-opt"),
                "AutoRemove": "--rm" in args, "Init": "--init" in args, "RestartPolicy": {"Name": "no"},
                "NetworkMode": self._option(args, "--network"), "PortBindings": {}, "PublishAllPorts": False,
                "PidMode": "", "IpcMode": self._option(args, "--ipc"), "CgroupnsMode": self._option(args, "--cgroupns"),
                "Memory": int(self._option(args, "--memory")[:-1]) * 1024 * 1024,
                "MemorySwap": int(self._option(args, "--memory-swap")[:-1]) * 1024 * 1024,
                "PidsLimit": int(self._option(args, "--pids-limit")),
                "NanoCpus": round(float(self._option(args, "--cpus")) * 1000000000),
                "Binds": None, "VolumesFrom": None, "Devices": [], "DeviceRequests": [],
                "ExtraHosts": None, "Links": None, "GroupAdd": None,
                "Tmpfs": {tmpfs_path: tmpfs_options}, "Mounts": configured_mounts,
            }
            self.containers[name] = {
                "Id": identity, "Name": "/" + name, "Image": IMAGE, "HostConfig": host, "Mounts": actual_mounts,
                "Config": {"Labels": labels, "User": self._option(args, "--user"),
                           "Entrypoint": [self._option(args, "--entrypoint")],
                           "Cmd": args[args.index(IMAGE) + 1:]},
                "State": {"Running": True, "Paused": False, "Restarting": False},
            }
            return {"code": 1 if self.fail_create else 0, "stdout": identity + "\n", "stderr": "synthetic"}
        if args[:2] == ["container", "inspect"]:
            ident = args[2]
            data = next((c for key, c in self.containers.items() if key == ident or c["Id"] == ident), None)
            return {"code": 0 if data else 1, "stdout": json.dumps([data] if data else []),
                    "stderr": "" if data else "No such container"}
        if args[:2] == ["container", "ls"]:
            criterion = self._option(args, "--filter")
            if criterion.startswith("id="):
                matches = [c for c in self.containers.values() if c["Id"] == criterion[3:]]
            elif criterion.startswith("label="):
                key, value = criterion[6:].split("=", 1)
                matches = [c for c in self.containers.values() if (c["Config"].get("Labels") or {}).get(key) == value]
            else:
                wanted = criterion.removeprefix("name=^/").removesuffix("$")
                matches = [c for name, c in self.containers.items() if name == wanted]
            return {"code": 0, "stdout": "\n".join(c["Id"] for c in matches), "stderr": ""}
        if args[:1] == ["exec"]:
            assert envmod._STOP_WORKERS in args, args
            return {"code": 1 if self.fail_cancel else 0, "stdout": "", "stderr": ""}
        if args[:3] == ["container", "rm", "--force"]:
            identity = args[3]
            found = next((name for name, c in self.containers.items() if c["Id"] == identity), None)
            if self.replace_at_remove and found:
                self.containers[found] = copy.deepcopy(self.containers[found])
                self.containers[found]["Id"] = "f" * 64
                self.containers[found]["Config"]["Labels"] = {}
                return {"code": 1, "stdout": "", "stderr": "original container already disappeared"}
            if self.fail_remove:
                return {"code": 1, "stdout": "", "stderr": "synthetic busy container"}
            if found:
                del self.containers[found]
            return {"code": 0, "stdout": identity, "stderr": ""}
        raise AssertionError(f"unexpected Docker command: {args!r}")

    def count(self, prefix):
        return sum(args[:len(prefix)] == prefix for args in self.calls)


class FakeRunner(EnvironmentRunner):
    def __init__(self, conf, engine):
        super().__init__(conf)
        self.engine = engine
        self._docker_binary = "/synthetic/docker"

    def _docker(self, args, **kwargs):
        return self.engine.call(args, **kwargs)


class TempTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ajx-env-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.base = self.root / "trial"
        self.base.mkdir()

    def fixture(self, name="fixtures"):
        path = self.base / name
        path.mkdir()
        (path / "input.txt").write_text("synthetic starting data\n", encoding="utf-8")
        return path

    def invalid(self, conf, field):
        with self.assertRaisesRegex(ValueError, field):
            normalize_profiles({"test": conf}, self.base)


class ProfileValidationTest(TempTest):
    def test_local_defaults_are_usable_observation_only(self):
        conf = normalize_profiles({"ajx-local": {"backend": "local"}}, self.base)["ajx-local"]
        runner = EnvironmentRunner(conf)
        self.assertEqual(runner.backend, "local")
        self.assertEqual(conf["permissions"], {"filesystem": "observed", "root": "inherit",
                                              "package_install": "unrestricted"})
        self.assertEqual(conf["limits"], {})
        self.assertEqual(conf["network"], "inherit")
        self.assertNotIn("_base_dir", runner.describe())
        self.assertEqual(runner.doctor()["capabilities"]["filesystem_isolation"]["state"], "unsupported")

    def test_unknown_nested_fields_and_cloud_backends_fail_closed(self):
        examples = [
            ({"backend": "aws"}, "backend"),
            ({"backend": "local", "privileged": True}, "privileged"),
            ({"backend": "container", "image": IMAGE, "args": ["--privileged"]}, "args"),
            ({"backend": "container", "image": IMAGE, "permissions": {"sudo": True}}, "permissions.sudo"),
            ({"backend": "container", "image": IMAGE, "limits": {"disk_gb": 1}}, "limits.disk_gb"),
            ({"backend": "local", "tools": [{"name": "x", "argv": ["x"], "hidden": 1}]}, r"tools\[0\].hidden"),
            ({"backend": "local", "_base_dir": str(self.base)}, "_base_dir"),
        ]
        for conf, field in examples:
            with self.subTest(conf=conf):
                self.invalid(conf, field)

    def test_local_rejects_required_sandboxes_and_permission_claims(self):
        for capability in ("filesystem_isolation", "network_isolation", "non_root", "resource_limits", "lifetime"):
            self.invalid({"backend": "local", "required": [capability]}, "required")
        for key, value in (("filesystem", "isolated"), ("root", "forbid"), ("package_install", "user")):
            self.invalid({"backend": "local", "permissions": {key: value}}, "permissions." + key)
        for item in ({"network": "none"}, {"limits": {"cpus": 1}}, {"image": IMAGE}):
            self.invalid({"backend": "local", **item}, next(iter(item)))

    def test_container_rejects_mutable_images_and_unsupported_permissions(self):
        for image in ("python:3-alpine", "latest", "sha256:abc", "--privileged", IMAGE + "\n"):
            self.invalid({"backend": "container", "image": image}, "image")
        for key, value in (("root", "allow"), ("package_install", "system"), ("package_install", "none"),
                           ("filesystem", "observed")):
            self.invalid({"backend": "container", "image": IMAGE, "permissions": {key: value}}, "permissions." + key)
        self.invalid({"backend": "container", "image": IMAGE, "network": "allowlist"}, "network")
        self.invalid({"backend": "container", "image": IMAGE, "network": "bridge",
                      "required": ["network_isolation"]}, "required")

    def test_pins_limits_and_requirements(self):
        digest = "example.invalid/tools@sha256:" + "b" * 64
        conf = profile(self.base, image=digest, platform="linux/amd64",
                       required=["filesystem_isolation", "non_root"])
        self.assertEqual(conf["image"], digest)
        self.assertEqual(conf["limits"]["lifetime_seconds"], 3600)
        for limits in ({"cpus": 0}, {"memory_mb": True}, {"pids": 1.5}, {"lifetime_seconds": -1},
                       {"tmpfs_mb": 2048}, {"cpus": float("nan")}, {"cpus": float("inf")}):
            self.invalid({"backend": "container", "image": IMAGE, "limits": limits}, "limits")
        self.invalid({"backend": "container", "image": IMAGE, "required": ["unsupported"]}, "required")
        self.invalid({"backend": "container", "image": IMAGE, "required": ["lifetime", "lifetime"]}, "required")
        self.invalid({"backend": "container", "image": IMAGE, "docker_host": "tcp://example.invalid:2375"}, "docker_host")
        self.invalid({"backend": "container", "image": IMAGE, "docker_host": "unix:///tmp/../engine"}, "docker_host")
        self.invalid({"backend": "local", "id": "different"}, ".id")

    def test_mount_targets_and_sources_must_be_explicit_and_bounded(self):
        fixture = self.fixture()
        good = {"source": "fixtures", "target": "/fixtures", "access": "read"}
        conf = profile(self.base, mounts=[good])
        self.assertEqual(conf["mounts"][0]["source"], str(fixture))
        for source in (".", "..", "../trial/fixtures", str(self.root), str(Path.home()), "missing"):
            self.invalid({"backend": "container", "image": IMAGE,
                          "mounts": [{**good, "source": source}]}, "source")
        for target in ("/", "/etc/fixture", "/proc", "/sys", "/dev", "/run/docker.sock", "/root",
                       "/tmp", "/out/../fixture", "/a,b", "/reports", "/unusual/.aws"):
            self.invalid({"backend": "container", "image": IMAGE,
                          "mounts": [{**good, "target": target}]}, "target")
        self.invalid({"backend": "container", "image": IMAGE,
                      "mounts": [{"source": "fixtures", "target": "/fixtures"}]}, "access")

    def test_mount_overlap_is_rejected(self):
        source = self.fixture()
        (source / "nested").mkdir()
        self.fixture("other")
        one = {"source": "fixtures", "target": "/fixtures", "access": "read"}
        for other in ({"source": "fixtures/nested", "target": "/other", "access": "read"},
                      {"source": "other", "target": "/fixtures/nested", "access": "write"}):
            self.invalid({"backend": "container", "image": IMAGE, "mounts": [one, other]}, "overlap")

    def test_symlink_paths_and_nested_symlinks_are_rejected_even_inside_base(self):
        source = self.fixture()
        alias = self.base / "alias"
        alias.symlink_to(source, target_is_directory=True)
        self.invalid({"backend": "container", "image": IMAGE,
                      "mounts": [{"source": "alias", "target": "/fixture", "access": "read"}]}, "symlink")
        (source / "alias.txt").symlink_to(source / "input.txt")
        self.invalid({"backend": "container", "image": IMAGE,
                      "mounts": [{"source": "fixtures", "target": "/fixture", "access": "read"}]}, "symlink")

    def test_special_files_hardlinks_and_configuration_trees_are_rejected(self):
        source = self.fixture()
        mount = {"source": "fixtures", "target": "/fixture", "access": "read"}
        hardlink = source / "hardlink"
        os.link(source / "input.txt", hardlink)
        self.invalid({"backend": "container", "image": IMAGE, "mounts": [mount]}, "hardlink")
        hardlink.unlink()
        fifo = source / "fifo"
        os.mkfifo(fifo)
        self.invalid({"backend": "container", "image": IMAGE, "mounts": [mount]}, "special")
        fifo.unlink()
        for name in (".aws", ".git", ".env", "report.html", "credentials.json"):
            file = source / name
            file.write_text("synthetic only")
            self.invalid({"backend": "container", "image": IMAGE, "mounts": [mount]}, "credential/configuration/report")
            file.unlink()

    def test_snapshot_limits_and_unknown_tool_fields(self):
        source = self.fixture()
        with mock.patch.object(envmod, "_MAX_BYTES", 1):
            self.invalid({"backend": "container", "image": IMAGE,
                          "mounts": [{"source": str(source), "target": "/fixture", "access": "read"}]}, "exceeds")
        self.invalid({"backend": "local", "tools": [{"name": "x", "argv": ["x"], "required": "yes"}]}, "required")
        self.invalid({"backend": "local", "tools": [{"name": "x", "argv": []}]}, "argv")
        self.invalid({"backend": "local", "tools": [{"name": "x", "argv": ["x"]},
                                                  {"name": "x", "argv": ["x"]}]}, "name")

    def test_cell_cannot_override_environment_or_use_legacy_runner(self):
        conf = profile(self.base)
        for cell in ({"id": "c", "runner": {"type": "wrapper"}},
                     {"id": "c", "env": {"HOME": "/elsewhere"}},
                     {"id": "c", "env": {"DOCKER_HOST": "tcp://example.invalid"}},
                     {"id": "c", "unset_env": ["XDG_*"]}):
            with self.subTest(cell=cell), self.assertRaises(ValueError):
                validate_for_cell(conf, cell)
        validate_for_cell(conf, {"id": "c", "env": {"EXPLICIT_TOKEN": "synthetic"}})

    def test_file_swapped_to_symlink_during_snapshot_cannot_escape(self):
        source = self.fixture()
        outside = self.root / "outside.txt"
        outside.write_text("synthetic excluded content")
        real_open = os.open
        swapped = False

        def replace_file(path, flags, *args, **kwargs):
            nonlocal swapped
            if path == "input.txt" and kwargs.get("dir_fd") is not None and not swapped:
                swapped = True
                (source / "input.txt").unlink()
                (source / "input.txt").symlink_to(outside)
            return real_open(path, flags, *args, **kwargs)

        with mock.patch.object(envmod.os, "open", side_effect=replace_file):
            with self.assertRaisesRegex(ValueError, "safely inspect/copy"):
                envmod._tree_manifest(source, "test.fixture", destination=self.root / "snapshot")
        self.assertFalse((self.root / "snapshot" / "input.txt").exists())


class LocalEnvironmentTest(TempTest):
    def local_runner(self, **options):
        return EnvironmentRunner(normalize_profiles({"local": {"backend": "local", **options}}, self.base)["local"])

    def test_empty_argument_values_reach_the_process_and_tool_probe(self):
        argv = [sys.executable, "-I", "-c",
                "import json, sys; assert sys.argv[1:] == ['--tools', '']; print(json.dumps(sys.argv[1:]))",
                "--tools", ""]
        runner = self.local_runner(tools=[{"name": "arguments", "argv": argv}])
        ctx = context(self.root)
        persist(runner, ctx)
        runner.prepare(ctx)
        self.addCleanup(runner.release, ctx)
        worker_env = runner._worker_env(ctx)
        proc = subprocess.run(runner.wrap(tuple(argv), ctx, worker_env), cwd=ctx["workspace"],
                              env=worker_env, capture_output=True, text=True, check=True, timeout=10)
        self.assertEqual(json.loads(proc.stdout), ["--tools", ""])

    def test_invalid_command_arguments_are_rejected(self):
        runner, ctx = self.local_runner(), context(self.root)
        for argv in (None, "echo", [], [""], ["--invalid"], [None], ["echo", None], ["echo", "\0"]):
            with self.subTest(argv=argv):
                with self.assertRaisesRegex(ValueError, "environment.argv"):
                    runner.wrap(argv, ctx, {})

    def test_plan_has_no_side_effects_and_requires_durable_state(self):
        runner = self.local_runner()
        ctx = context(self.root, create=False)
        plan = runner.plan(ctx)
        self.assertTrue(all(not Path(ctx[k]).exists() for k in envmod._ROOTS))
        self.assertEqual(list(Path(ctx["run_dir"]).iterdir()), [])
        json.dumps(plan)
        with self.assertRaisesRegex(EnvironmentError, "Persist"):
            runner.prepare(ctx)
        ctx["state"]["environment"] = plan
        report = runner.prepare(ctx)
        self.assertEqual(report["inventory"]["phase"], "initial")
        for capability in envmod._REQUIRED:
            self.assertEqual(report["capabilities"][capability]["state"], "unsupported")
        self.assertNotEqual(report["platform"]["effective_uid"], None)
        self.assertEqual(runner.release(ctx)["worker_stop"], "caller-managed")

    def test_incomplete_persisted_plan_is_not_silently_replaced(self):
        runner, ctx = self.local_runner(), context(self.root)
        ctx["state"]["environment"] = {}
        with self.assertRaisesRegex(EnvironmentError, "Persist"):
            runner.plan(ctx)
        self.assertEqual(ctx["state"]["environment"], {})

    def test_no_ambient_env_and_unset_wins_over_overrides(self):
        runner = self.local_runner()
        ambient = {"AWS_ACCESS_KEY_ID": "synthetic-host-key", "ANTHROPIC_API_KEY": "synthetic-host-token",
                   "HTTP_PROXY": "http://example.invalid", "XDG_CONFIG_HOME": "/synthetic/config",
                   "PATH": "/synthetic/host-path", "HOME": "/synthetic/home"}
        with mock.patch.dict(os.environ, ambient):
            env = runner.child_env({"EXPLICIT_TOKEN": "synthetic-declared", "DROP_ME": "value"}, ["DROP_*"])
        self.assertNotIn("AWS_ACCESS_KEY_ID", env)
        self.assertNotIn("ANTHROPIC_API_KEY", env)
        self.assertNotIn("HTTP_PROXY", env)
        self.assertNotIn("DROP_ME", env)
        self.assertEqual(env["HOME"], "/nonexistent")
        self.assertEqual(env["EXPLICIT_TOKEN"], "synthetic-declared")
        for key in ("BASH_ENV", "ENV", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "DOCKER_CONFIG", "SSH_AUTH_SOCK"):
            with self.subTest(key=key), self.assertRaisesRegex(ValueError, key):
                runner.child_env({key: "synthetic"})

    def test_real_local_shell_has_isolated_home_and_persistent_user_tool(self):
        runner = self.local_runner()
        ctx = context(self.root)
        ctx["env"] = {"DECLARED": "present"}
        persist(runner, ctx)
        report = runner.prepare(ctx)
        self.assertEqual(report["env_names"].count("DECLARED"), 1)
        with mock.patch.dict(os.environ, {"AJX_AMBIENT_TEST_SECRET": "must-not-inherit"}):
            setup = runner.shell('printf "#!/bin/sh\\nprintf installed\\n" > "$HOME/.local/bin/probe"; '
                                 'chmod +x "$HOME/.local/bin/probe"; printf "$HOME"', ctx)
            execute = runner.shell('probe; printf "|%s|%s" "$DECLARED" "${AJX_AMBIENT_TEST_SECRET-unset}"', ctx)
        self.assertEqual(setup["stdout"], str(ctx["home_dir"]))
        self.assertEqual(execute["stdout"], "installed|present|unset")
        self.assertEqual(execute["exit_code"], 0)
        for key in ("cmd", "exit_code", "timed_out", "stdout", "stderr", "started_at", "stopped_at"):
            self.assertIn(key, execute)
        with self.assertRaisesRegex(ValueError, "HOME"):
            runner.shell("true", ctx, env={"HOME": str(self.root)})
        self.assertTrue(runner.release(ctx)["confirmed"])
        self.assertTrue(runner.release(ctx)["confirmed"])
        self.assertTrue((ctx["home_dir"] / ".local" / "bin" / "probe").exists())

    def test_declared_path_override_is_supported_without_ambient_env_inheritance(self):
        runner, ctx = self.local_runner(), context(self.root)
        custom_bin = ctx["workspace"] / "tools"
        custom_bin.mkdir()
        tool = custom_bin / "custom-probe"
        tool.write_text("#!/bin/sh\nprintf explicit-path\n")
        tool.chmod(0o700)
        ctx["env"] = {"PATH": str(custom_bin) + ":/usr/bin:/bin"}
        validate_for_cell(runner.profile, {"id": "explicit", "env": ctx["env"]})
        persist(runner, ctx)
        runner.prepare(ctx)
        self.assertEqual(runner.shell("custom-probe", ctx)["stdout"], "explicit-path")

    def test_inventory_record_masks_supplied_env_values(self):
        runner = self.local_runner(tools=[{"name": "echo-env",
                                          "argv": ["/bin/sh", "-c", 'printf %s "$EXPLICIT_TOKEN"']}])
        ctx = context(self.root)
        ctx["env"] = {"EXPLICIT_TOKEN": "synthetic-only-env-value"}
        handle = persist(runner, ctx)
        report = runner.prepare(ctx)
        self.assertIn("EXPLICIT_TOKEN", report["env_names"])
        self.assertNotIn(ctx["env"]["EXPLICIT_TOKEN"], json.dumps(handle))
        self.assertNotIn(ctx["env"]["EXPLICIT_TOKEN"], json.dumps(report))
        self.assertIn("<redacted env:EXPLICIT_TOKEN>", report["inventory"]["tools"][0]["stdout"])

    def test_caller_can_stage_declared_extensions_in_owned_fresh_roots(self):
        runner = self.local_runner()
        ctx = context(self.root)
        (ctx["home_dir"] / "declared-skill.txt").write_text("synthetic selected skill")
        with self.assertRaisesRegex(ValueError, "must start empty"):
            runner.plan(ctx)
        ctx["state"]["paths"] = {k: str(ctx[k]) for k in envmod._ROOTS}
        ctx["state"]["owned_paths"] = list(ctx["state"]["paths"].values())
        persist(runner, ctx)
        self.assertEqual(runner.prepare(ctx)["status"], "prepared")

    def test_root_overlap_report_access_and_symlink_roots_fail_closed(self):
        for key, replacement in (("home_dir", "workspace"), ("cache_dir", "run_dir")):
            runner, ctx = self.local_runner(), context(self.root, name=key)
            ctx[key] = ctx[replacement]
            with self.assertRaises(ValueError):
                runner.plan(ctx)
        runner, ctx = self.local_runner(), context(self.root, name="symlink")
        ctx["home_dir"].rmdir()
        ctx["home_dir"].symlink_to(ctx["cache_dir"], target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "symlink"):
            runner.plan(ctx)

    def test_local_inventory_missing_tool_and_recovery_do_not_replay(self):
        runner = self.local_runner(tools=[{"name": "missing", "argv": ["ajx-definitely-absent-tool"], "required": True}])
        ctx = context(self.root)
        persist(runner, ctx)
        with self.assertRaisesRegex(EnvironmentError, "required starting-inventory"):
            runner.prepare(ctx)
        with self.assertRaisesRegex(EnvironmentError, "previously failed"):
            runner.prepare(ctx)
        recovered = EnvironmentRunner(runner.profile)
        with self.assertRaisesRegex(EnvironmentError, "not confirmed"):
            recovered.prepare(ctx)

    def test_local_recovery_without_confirmed_preparation_is_rejected(self):
        runner, ctx = self.local_runner(), context(self.root)
        persist(runner, ctx)
        recovered = EnvironmentRunner(runner.profile)
        self.assertEqual(recovered.plan(ctx), ctx["state"]["environment"])
        with self.assertRaisesRegex(EnvironmentError, "preparation was not confirmed"):
            recovered.prepare(ctx)
        runner.prepare(ctx)
        recovered = EnvironmentRunner(runner.profile)
        self.assertEqual(recovered.prepare(ctx)["inventory"]["phase"], "recovered")

    def test_released_or_replaced_local_directories_cannot_be_reused(self):
        runner, ctx = self.local_runner(), context(self.root)
        persist(runner, ctx)
        runner.prepare(ctx)
        old = ctx["workspace"].with_name("original-workspace")
        ctx["workspace"].rename(old)
        ctx["workspace"].mkdir()
        with self.assertRaisesRegex(EnvironmentError, "identity changed"):
            EnvironmentRunner(runner.profile).prepare(ctx)
        runner.release(ctx)
        with self.assertRaisesRegex(EnvironmentError, "released"):
            EnvironmentRunner(runner.profile).prepare(ctx)

    def test_local_timeout_kills_process_group_and_bounds_output(self):
        runner, ctx = self.local_runner(), context(self.root)
        persist(runner, ctx)
        runner.prepare(ctx)
        out = runner.shell("sleep 60 & child=$!; printf '%s\\n' \"$child\"; wait", ctx, timeout=0.15)
        self.assertTrue(out["timed_out"])
        self.assertIsNone(out["exit_code"])
        child = int(out["stdout"].strip())
        # A killed child can briefly be a zombie until the host init reaps it.
        for _ in range(50):
            try:
                os.kill(child, 0)
                proc_status = Path(f"/proc/{child}/stat")
                if proc_status.exists() and proc_status.read_text().split()[2] == "Z":
                    break
            except (ProcessLookupError, FileNotFoundError):
                # Reaping can remove the proc entry between exists() and read_text().
                break
            time.sleep(0.01)
        else:
            self.fail("timed-out shell left a running child")
        second, ctx2 = self.local_runner(), context(self.root, name="bounded-output")
        persist(second, ctx2)
        second.prepare(ctx2)
        out = second.shell("i=0; while [ $i -lt 15000 ]; do printf x; i=$((i+1)); done", ctx2, timeout=10)
        self.assertEqual(len(out["stdout"]), 8000)

    def test_local_abort_preserves_cleanup_context_until_final_release(self):
        runner, ctx = self.local_runner(), context(self.root)
        persist(runner, ctx)
        runner.prepare(ctx)
        self.addCleanup(runner.release, ctx)
        outcome = runner.abort(ctx)
        self.assertEqual(outcome["worker_stop"], "caller-managed")
        cleanup = runner.shell('printf cleaned > "$HOME/cleanup-result"', ctx)
        self.assertEqual(cleanup["exit_code"], 0, cleanup)
        recovered = EnvironmentRunner(runner.profile)
        self.assertEqual(recovered.shell('cat "$HOME/cleanup-result"', ctx)["stdout"], "cleaned")
        self.assertTrue(runner.release(ctx)["confirmed"])
        with self.assertRaisesRegex(EnvironmentError, "released"):
            recovered.shell("true", ctx)


class HostEnvironmentTest(TempTest):
    def make_runner(self, coordinator_path):
        with mock.patch.dict(os.environ, {"PATH": coordinator_path}):
            return EnvironmentRunner(profile(self.base))

    def test_host_runtime_and_config_ignore_worker_planted_python_and_startup_files(self):
        ctx = context(self.root)
        worker_bin = ctx["home_dir"] / ".local" / "bin"
        worker_config = ctx["home_dir"] / ".config"
        for path in (worker_bin, worker_config, ctx["config_dir"]):
            path.mkdir(parents=True, exist_ok=True)
            (path / "python3").write_text("#!/bin/sh\nprintf poisoned-worker-runtime\n")
            (path / "python3").chmod(0o755)
            (path / "settings.json").write_text('{"source":"worker"}')
        (ctx["workspace"] / "sitecustomize.py").write_text("raise SystemExit('worker startup executed')\n")
        (ctx["workspace"] / "json.py").write_text("raise SystemExit('worker module imported')\n")
        runtime = self.root / "coordinator-runtime"
        runtime.mkdir()
        (runtime / "python3").symlink_to(sys.executable)
        alias = self.root / "worker-bin-alias"
        alias.symlink_to(worker_bin, target_is_directory=True)
        coordinator_path = f":relative-tools:{worker_bin}:{worker_config}:{ctx['config_dir']}:{alias}:{runtime}:/usr/bin:/bin"
        runner = self.make_runner(coordinator_path)
        ctx["check_env"] = {
            **runner.environment_env(ctx), "PATH": f"{worker_bin}:{worker_config}",
            "CODEX_HOME": str(ctx["config_dir"]), "CLAUDE_CONFIG_DIR": str(ctx["config_dir"]),
            "KIRO_HOME": str(ctx["config_dir"]), "ZDOTDIR": str(ctx["home_dir"]),
            "PYTHONPATH": str(ctx["workspace"]), "PYTHONSTARTUP": str(ctx["workspace"] / "sitecustomize.py"),
            "EXPLICIT_TEST_TOKEN": "synthetic-declared-value",
        }
        with mock.patch.dict(os.environ, {"AJX_HOST_AMBIENT_SECRET": "synthetic-do-not-inherit",
                                         "PATH": str(worker_bin)}):
            env = runner.host_env(ctx, ctx["check_env"], ())
        self.assertEqual(env["PATH"].split(os.pathsep)[0], str(runtime))
        for entry in env["PATH"].split(os.pathsep):
            self.assertTrue(Path(entry).is_absolute())
            self.assertFalse(runner._inside_worker(Path(entry), runner._worker_roots(ctx)))
        self.assertNotIn("PYTHONPATH", env)
        self.assertNotIn("AJX_HOST_AMBIENT_SECRET", env)
        self.assertEqual(env["PYTHONSAFEPATH"], "1")
        self.assertEqual(env["PYTHONNOUSERSITE"], "1")
        for key in ("HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "TMPDIR", "PYTHONUSERBASE",
                    "npm_config_prefix", "CARGO_HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "KIRO_HOME"):
            self.assertTrue(Path(env[key]).is_relative_to(ctx["run_dir"]), key)
            self.assertEqual(Path(env[key]).stat().st_mode & 0o777, 0o700, key)
        settings = Path(env["XDG_CONFIG_HOME"]) / "settings.json"
        settings.write_text('{"source":"coordinator"}')
        script = (
            "import json,os,sys;from pathlib import Path;"
            "print(json.dumps({'runtime':str(Path(sys.executable).resolve()),"
            "'config':json.loads((Path(os.environ['XDG_CONFIG_HOME'])/'settings.json').read_text()),"
            "'token':os.environ['EXPLICIT_TEST_TOKEN'],'home':os.environ['HOME']}))"
        )
        observed = subprocess.run(["python3", "-c", script], cwd=ctx["workspace"], env=env,
                                  capture_output=True, text=True, timeout=10)
        self.assertEqual(observed.returncode, 0, observed.stderr)
        data = json.loads(observed.stdout)
        self.assertEqual(data["runtime"], str(Path(sys.executable).resolve()))
        self.assertEqual(data["config"]["source"], "coordinator")
        self.assertEqual(data["token"], "synthetic-declared-value")
        self.assertNotEqual(data["home"], str(ctx["home_dir"]))
        self.assertEqual(runner.host_env(ctx)["HOME"], env["HOME"])  # stable private context within this attempt

    def test_host_package_caches_cannot_load_worker_installed_packages(self):
        ctx = context(self.root)
        keys = ("npm_config_cache", "YARN_CACHE_FOLDER", "PNPM_STORE_DIR", "UV_CACHE_DIR",
                "PIP_CACHE_DIR", "POETRY_CACHE_DIR", "GOCACHE", "GOMODCACHE", "BUN_INSTALL_CACHE_DIR",
                "DENO_DIR", "NUGET_PACKAGES", "GRADLE_USER_HOME")
        worker_caches = {}
        for key in keys:
            path = ctx["cache_dir"] / key.lower()
            path.mkdir()
            (path / "package-startup").write_text("worker-controlled code")
            worker_caches[key] = str(path)
        ctx["env"] = dict(worker_caches)
        ctx["check_env"] = dict(worker_caches)
        runner = self.make_runner("/usr/bin:/bin")
        env = runner.host_env(ctx, worker_caches, ())
        for key in keys:
            with self.subTest(cache=key):
                path = Path(env[key])
                self.assertNotEqual(str(path), worker_caches[key])
                self.assertTrue(path.is_relative_to(Path(env["XDG_CACHE_HOME"])))
                self.assertTrue(path.is_relative_to(ctx["run_dir"] / "host-environment"))
                self.assertEqual(path.stat().st_mode & 0o777, 0o700)
                self.assertFalse((path / "package-startup").exists())
                self.assertEqual((Path(worker_caches[key]) / "package-startup").read_text(),
                                 "worker-controlled code")
        with self.assertRaisesRegex(ValueError, "cannot unset"):
            runner.host_env(ctx, {}, ["GRADLE_USER_HOME"])

    def test_private_host_roots_reject_symlinks_without_writing_the_target(self):
        ctx = context(self.root)
        outside = self.root / "unrelated"
        outside.mkdir()
        private = ctx["run_dir"] / "host-environment"
        private.symlink_to(outside, target_is_directory=True)
        runner = self.make_runner("/usr/bin:/bin")
        with self.assertRaisesRegex(ValueError, "without symlinks"):
            runner.host_env(ctx)
        self.assertEqual(list(outside.iterdir()), [])
        private.unlink()
        private.mkdir(mode=0o700)
        (private / "cache").symlink_to(outside, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "without symlinks"):
            runner.host_env(ctx)
        self.assertEqual(list(outside.iterdir()), [])

    def test_host_filter_includes_write_mounts_and_preserves_explicit_auth_unset(self):
        ctx = context(self.root)
        extra_write = self.root / "extra-write-mount"
        extra_write.mkdir()
        ctx["state"]["environment"] = {"mounts": [{"access": "write", "host_source": str(extra_write)}]}
        runner = self.make_runner(f"{extra_write}::relative:/usr/bin:/bin")
        env = runner.host_env(ctx, {"DECLARED_TOKEN": "synthetic", "DROP_TOKEN": "synthetic"}, ["DROP_*"])
        self.assertNotIn(str(extra_write), env["PATH"])
        self.assertNotIn("DROP_TOKEN", env)
        self.assertEqual(env["DECLARED_TOKEN"], "synthetic")
        with self.assertRaisesRegex(ValueError, "cannot unset"):
            runner.host_env(ctx, {}, ["HOME"])
        with self.assertRaisesRegex(ValueError, "AWS_CONFIG_FILE"):
            runner.host_env(ctx, {"AWS_CONFIG_FILE": str(ctx["config_dir"] / "config")})
        trusted_config = str(self.root / "coordinator-configuration")
        self.assertEqual(runner.host_env(ctx, {"AWS_CONFIG_FILE": trusted_config})["AWS_CONFIG_FILE"], trusted_config)

    def test_separate_attempts_get_separate_private_host_contexts(self):
        runner = self.make_runner("/usr/bin:/bin")
        a, b = context(self.root, name="a"), context(self.root, name="b")
        first, second = runner.host_env(a), runner.host_env(b)
        self.assertNotEqual(first["HOME"], second["HOME"])
        (Path(first["HOME"]) / "private-file").write_text("coordinator-only")
        self.assertFalse((Path(second["HOME"]) / "private-file").exists())
        # Local execution still has no OS sandbox; host_env changes discovery paths only.
        local = EnvironmentRunner(normalize_profiles({"local": {"backend": "local"}}, self.base)["local"])
        self.assertEqual(local.doctor()["capabilities"]["filesystem_isolation"]["state"], "unsupported")


class ContainerEnvironmentTest(TempTest):
    def setUp(self):
        super().setUp()
        self.engine = FakeDocker()
        self.executions = []
        self.process = mock.patch.object(envmod, "_tail_process", side_effect=self.fake_process).start()
        self.addCleanup(mock.patch.stopall)

    def fake_process(self, argv, cwd, timeout, env):
        self.executions.append({"argv": list(argv), "env": dict(env), "cwd": str(cwd), "timeout": timeout})
        command = argv[-1]
        if command.startswith("printf 'system='"):
            uid, gid = self.engine._option(argv, "--user").split(":")
            return result(f"system=Linux\nrelease=synthetic\nmachine=x86_64\nuid={uid}\ngid={gid}\n"
                          "CapEff:=0000000000000000\nNoNewPrivs:=1\nSeccomp:=2\nsupervisor_uid=0\n"
                          "interface=lo\nos_ID=synthetic-linux\n")
        return result("synthetic-tool 1.0\n" if "version" in command else "")

    def prepared(self, **options):
        runner = FakeRunner(profile(self.base, **options), self.engine)
        ctx = context(self.root)
        handle = persist(runner, ctx)
        report = runner.prepare(ctx)
        return runner, ctx, handle, report

    def test_plan_and_doctor_only_inspect_without_provisioning(self):
        runner = FakeRunner(profile(self.base), self.engine)
        ctx = context(self.root, create=False)
        doctor = runner.doctor(ctx)
        self.assertEqual(doctor["status"], "available")
        self.assertEqual(doctor["capabilities"]["lifetime"]["state"], "unknown")
        handle = persist(runner, ctx)
        self.assertEqual(handle["image"]["id"], IMAGE)
        self.assertEqual(self.engine.count(["run"]), 0)
        self.assertTrue(all(not Path(ctx[k]).exists() for k in envmod._ROOTS))
        self.assertEqual(list(ctx["run_dir"].iterdir()), [])
        self.assertNotIn(IMAGE_SECRET, json.dumps(handle))
        self.assertIn("IMAGE_SECRET", handle["image"]["environment_names"])

    def test_create_has_only_owned_mounts_fixed_security_and_independent_lifetime(self):
        self.fixture()
        runner, ctx, handle, report = self.prepared(
            mounts=[{"source": "fixtures", "target": "/fixtures", "access": "read"}],
            required=["filesystem_isolation", "network_isolation", "non_root", "resource_limits", "lifetime"])
        create = next(args for args in self.engine.calls if args[0] == "run")
        for flag in ("--rm", "--init", "--read-only", "--no-healthcheck"):
            self.assertIn(flag, create)
        self.assertEqual(self.engine._option(create, "--network"), "none")
        self.assertEqual(self.engine._option(create, "--cap-drop"), "ALL")
        self.assertEqual(self.engine._option(create, "--user"), "0:0")
        self.assertEqual(self.engine._option(create, "--entrypoint"), "/usr/bin/env")
        self.assertIn("no-new-privileges=true", create)
        self.assertEqual(create[-5:-1], [IMAGE, "-i", "PATH=" + envmod._PATH, "/bin/sleep"])
        self.assertTrue(0 < int(create[-1]) <= 3600)
        self.assertFalse(handle["worker_user"].startswith("0:"))
        mount_flags = self.engine._options(create, "--mount")
        self.assertEqual(len(mount_flags), 5)
        self.assertTrue(all("bind-recursive=disabled" in value for value in mount_flags))
        self.assertTrue(any("target=/fixtures" in value and "readonly" in value for value in mount_flags))
        self.assertFalse(any(str(ctx["run_dir"]) in value or "docker.sock" in value for value in mount_flags))
        self.assertTrue(all(report["capabilities"][key]["state"] == "enforced" for key in envmod._REQUIRED))
        self.assertEqual(report["image"]["platform"], "linux/amd64")
        self.assertEqual(report["platform"]["system"], "Linux")
        self.assertNotIn(IMAGE_SECRET, json.dumps(report))
        self.assertIn("IMAGE_SECRET=", self.engine._options(create, "--env"))
        self.assertIn("BASH_ENV=", self.engine._options(create, "--env"))

    def test_single_persistent_container_for_setup_execute_narrate_and_recovery(self):
        runner, ctx, handle, report = self.prepared(tools=[{"name": "probe", "argv": ["probe", "--version"]}])
        inventory_calls = sum("version" in call["argv"][-1] for call in self.executions)
        runner.shell("install-user-tool", ctx)
        execute = runner.wrap(["probe", "execute"], ctx, runner._worker_env(ctx))
        narrate = runner.wrap(["probe", "narrate"], ctx, runner._worker_env(ctx))
        identity = self.engine.containers[handle["name"]]["Id"]
        self.assertIn(identity, execute)
        self.assertIn(identity, narrate)
        self.assertEqual(self.engine.count(["run"]), 1)
        self.assertEqual(runner.prepare(ctx), report)
        recovered = FakeRunner(runner.profile, self.engine)
        self.assertEqual(recovered.plan(ctx), handle)
        recovered_report = recovered.prepare(ctx)
        self.assertEqual(recovered_report["status"], "attached")
        self.assertEqual(recovered_report["inventory"]["tools"][0]["status"], "unknown")
        self.assertEqual(inventory_calls, sum("version" in call["argv"][-1] for call in self.executions))
        self.assertEqual(self.engine.count(["run"]), 1)
        self.assertTrue(recovered.release(ctx)["confirmed"])

    def test_exec_preserves_empty_argument_values(self):
        runner, ctx, handle, report = self.prepared()
        self.addCleanup(runner.release, ctx)
        argv = runner.wrap(["claude", "--tools", ""], ctx, runner._worker_env(ctx))
        self.assertEqual(argv[-3:], ["claude", "--tools", ""])

    def test_exec_contains_env_names_not_values_or_image_defaults(self):
        runner, ctx, handle, report = self.prepared()
        supplied = "synthetic-explicit-auth-value"
        ctx["env"] = {"DECLARED_TOKEN": supplied, "DISCARD": "synthetic"}
        ctx["unset_env"] = ["DISCARD"]
        worker_env = runner._worker_env(ctx)
        argv = runner.wrap(["tool", "task"], ctx, worker_env)
        self.assertNotIn(supplied, json.dumps(argv))
        self.assertIn("DECLARED_TOKEN", self.engine._options(argv, "--env"))
        self.assertNotIn("DISCARD", self.engine._options(argv, "--env"))
        self.assertNotIn("IMAGE_SECRET", self.engine._options(argv, "--env"))
        self.assertIn("/usr/bin/env -i", argv[-4])
        self.assertIn('"DECLARED_TOKEN=${DECLARED_TOKEN}"', argv[-4])
        self.assertEqual(argv[0], "/synthetic/docker")
        self.assertEqual(self.engine._option(argv, "--host"), "unix:///var/run/docker.sock")
        self.assertEqual(self.engine._option(argv, "--user"), handle["worker_user"])
        self.assertEqual(worker_env["DECLARED_TOKEN"], supplied)

    def test_write_mount_is_attempt_copy_and_read_source_is_untouched(self):
        source = self.fixture("writable-start")
        runner, ctx, handle, report = self.prepared(
            mounts=[{"source": "writable-start", "target": "/output", "access": "write"}])
        copied = Path(handle["mounts"][0]["host_source"])
        self.assertTrue(copied.is_relative_to(ctx["workspace"]))
        self.assertNotEqual(copied, source)
        self.assertEqual((copied / "input.txt").read_text(), (source / "input.txt").read_text())
        (copied / "input.txt").write_text("changed in attempt")
        self.assertEqual((source / "input.txt").read_text(), "synthetic starting data\n")
        self.assertTrue(runner.release(ctx)["confirmed"])
        self.assertTrue(copied.exists(), "release must preserve evidence for the coordinator's archive")

    def test_source_drift_between_plan_and_prepare_is_rejected(self):
        source = self.fixture()
        runner = FakeRunner(profile(self.base, mounts=[{"source": "fixtures", "target": "/fixtures", "access": "read"}]),
                            self.engine)
        ctx = context(self.root)
        persist(runner, ctx)
        (source / "input.txt").write_text("changed after plan")
        with self.assertRaisesRegex(EnvironmentError, "changed after planning"):
            runner.prepare(ctx)
        self.assertEqual(self.engine.count(["run"]), 0)

    def test_recovery_missing_or_expired_never_creates_or_restarts(self):
        runner = FakeRunner(profile(self.base), self.engine)
        ctx = context(self.root)
        handle = persist(runner, ctx)
        recovered = FakeRunner(runner.profile, self.engine)
        self.assertEqual(recovered.plan(ctx), handle)
        with self.assertRaisesRegex(EnvironmentError, "recovery never creates"):
            recovered.prepare(ctx)
        self.assertEqual(self.engine.count(["run"]), 0)
        with mock.patch.object(envmod.time, "time", return_value=handle["expires_at"] + 1):
            with self.assertRaisesRegex(EnvironmentError, "expired"):
                runner.prepare(ctx)
        self.assertEqual(self.engine.count(["run"]), 0)

    def test_stopped_or_policy_tampered_container_is_never_restarted(self):
        runner, ctx, handle, report = self.prepared()
        data = self.engine.containers[handle["name"]]
        for section, key, bad in (("State", "Running", False), ("HostConfig", "Privileged", True),
                                  ("HostConfig", "ReadonlyRootfs", False), ("Config", "User", "1000:1000"),
                                  ("HostConfig", "NetworkMode", "host")):
            original = data[section][key]
            data[section][key] = bad
            with self.subTest(key=key), self.assertRaises(EnvironmentError):
                runner.wrap(["tool"], ctx, runner._worker_env(ctx))
            data[section][key] = original
        self.assertEqual(self.engine.count(["run"]), 1)

    def test_timeout_stops_workers_and_preserves_the_same_environment_for_teardown(self):
        runner, ctx, handle, report = self.prepared()
        identity = self.engine.containers[handle["name"]]["Id"]
        self.process.side_effect = lambda *a, **kw: result("partial", exit_code=None, timed_out=True)
        response = runner.shell("sleep 600", ctx, timeout=0.1)
        self.assertTrue(response["timed_out"])
        self.assertTrue(response["environment_abort"]["confirmed"])
        self.assertTrue(response["environment_abort"]["environment_retained"])
        stop = next(args for args in self.engine.calls if args[:1] == ["exec"])
        self.assertEqual(self.engine._option(stop, "--user"), handle["worker_user"])
        self.assertEqual(self.engine._option(stop, "--workdir"), "/")
        self.assertIn(identity, stop)
        self.assertIn(envmod._STOP_WORKERS, stop)
        self.assertEqual(stop[-1], handle["worker_user"].split(":")[0])
        self.assertNotIn(["container", "rm", "--force", identity], self.engine.calls)
        self.process.side_effect = self.fake_process
        self.assertEqual(runner.shell("perform-declared-teardown", ctx)["exit_code"], 0)
        recovered = FakeRunner(runner.profile, self.engine)
        self.assertIn(identity, recovered.wrap(["tool"], ctx, {}))
        self.assertTrue(runner.release(ctx)["confirmed"])
        self.assertEqual(self.engine.containers, {})
        self.assertEqual(self.engine.count(["run"]), 1)

    def test_harness_timeout_stops_workers_without_releasing_cleanup_environment(self):
        runner, ctx, handle, _ = self.prepared()
        ctx.update(runner=runner, environment_profile=runner.profile, env=runner.environment_env(ctx))
        with mock.patch.object(basemod, "run_streaming", return_value=result(timed_out=True)):
            response = basemod.Harness().run(ctx, ["synthetic-worker"], "execute")
        self.assertTrue(response["environment_abort"]["environment_retained"])
        self.assertEqual(runner.shell("perform-declared-teardown", ctx)["exit_code"], 0)
        self.assertEqual(self.engine.count(["exec"]), 1)
        self.assertEqual(self.engine.count(["container", "rm"]), 0)

    def test_harness_interruption_stops_workers_before_returning_control(self):
        runner, ctx, handle, _ = self.prepared()
        ctx.update(runner=runner, environment_profile=runner.profile, env=runner.environment_env(ctx))
        interruption = KeyboardInterrupt("synthetic interruption")
        with mock.patch.object(basemod, "run_streaming", side_effect=interruption):
            with self.assertRaises(KeyboardInterrupt) as raised:
                basemod.Harness().run(ctx, ["synthetic-worker"], "execute")
        self.assertIs(raised.exception, interruption)
        self.assertEqual(self.engine.count(["exec"]), 1)
        self.assertEqual(runner.shell("perform-declared-teardown", ctx)["exit_code"], 0)
        self.assertEqual(self.engine.count(["container", "rm"]), 0)
        with mock.patch.object(basemod, "run_streaming", side_effect=interruption), \
                mock.patch.object(runner, "abort", side_effect=RuntimeError("synthetic cancellation failure")):
            with self.assertRaises(KeyboardInterrupt) as raised:
                basemod.Harness().run(ctx, ["synthetic-worker"], "execute")
        self.assertIs(raised.exception, interruption)
        self.assertIn("RuntimeError", raised.exception.__notes__[-1])

    def test_unconfirmed_worker_stop_falls_back_to_owned_container_removal(self):
        runner, ctx, handle, _ = self.prepared()
        self.engine.fail_cancel = True
        stopped = runner.abort(ctx)
        self.assertTrue(stopped["confirmed"])
        self.assertFalse(stopped["environment_retained"])
        self.assertIn("could not be confirmed", stopped["cancellation_error"])
        self.assertEqual(self.engine.containers, {})

    def test_failed_stop_and_removal_never_claim_confirmed_cleanup(self):
        runner, ctx, handle, _ = self.prepared()
        self.engine.fail_cancel = self.engine.fail_remove = True
        stopped = runner.abort(ctx)
        self.assertFalse(stopped["confirmed"])
        self.assertFalse(stopped["environment_retained"])
        self.assertIn(handle["name"], self.engine.containers)

    def test_expiry_at_execution_stops_owned_container_and_does_not_extend_deadline(self):
        runner, ctx, handle, report = self.prepared()
        with mock.patch.object(envmod.time, "time", return_value=handle["expires_at"] + 1):
            with self.assertRaisesRegex(EnvironmentError, "lifetime expired"):
                runner.wrap(["tool"], ctx, {})
        self.assertEqual(self.engine.containers, {})
        self.assertEqual(self.engine.count(["run"]), 1)

    def test_cleanup_verifies_ownership_then_uses_full_id_and_is_idempotent(self):
        runner, ctx, handle, report = self.prepared()
        identity = self.engine.containers[handle["name"]]["Id"]
        self.assertTrue(runner.release(ctx)["confirmed"])
        self.assertEqual(runner.release(ctx)["status"], "already_absent")
        removes = [a for a in self.engine.calls if a[:2] == ["container", "rm"]]
        self.assertEqual(removes, [["container", "rm", "--force", identity]])

    def test_foreign_same_name_container_is_never_removed_or_executed(self):
        runner, ctx, handle, report = self.prepared()
        self.engine.containers[handle["name"]]["Config"]["Labels"] = {}
        self.assertFalse(runner.abort(ctx)["confirmed"])
        self.assertEqual(self.engine.count(["exec"]), 0)
        cleanup = runner.release(ctx)
        self.assertFalse(cleanup["confirmed"])
        self.assertIn("ownership", cleanup["error"])
        self.assertEqual(self.engine.count(["container", "rm"]), 0)
        with self.assertRaisesRegex(EnvironmentError, "ownership"):
            runner.wrap(["tool"], ctx, {})

    def test_null_or_missing_labels_are_not_cleanup_authority(self):
        runner, ctx, handle, report = self.prepared()
        self.engine.containers[handle["name"]]["Config"]["Labels"] = None
        self.assertFalse(runner.release(ctx)["confirmed"])
        self.assertEqual(self.engine.count(["container", "rm"]), 0)

    def test_recovery_cleanup_does_not_need_original_fixture_sources(self):
        source = self.fixture()
        runner, ctx, handle, report = self.prepared(
            mounts=[{"source": "fixtures", "target": "/fixtures", "access": "read"}])
        source.rename(source.with_name("moved-source"))
        recovered = FakeRunner(runner.profile, self.engine)
        self.assertTrue(recovered.release(ctx)["confirmed"])

    def test_interrupted_preparation_cannot_be_reclassified_as_prepared_on_recovery(self):
        runner, ctx, handle, report = self.prepared()
        runner._write_marker(ctx, handle, "preparing")
        recovered = FakeRunner(runner.profile, self.engine)
        with self.assertRaisesRegex(EnvironmentError, "preparation was not confirmed"):
            recovered.prepare(ctx)
        self.assertEqual(self.engine.count(["run"]), 1)
        self.assertTrue(recovered.release(ctx)["confirmed"])

    def test_replacement_between_inspect_and_remove_cannot_delete_new_container(self):
        runner, ctx, handle, report = self.prepared()
        identity = self.engine.containers[handle["name"]]["Id"]
        self.engine.replace_at_remove = True
        cleanup = runner.release(ctx)
        self.assertTrue(cleanup["confirmed"])
        self.assertIn(handle["name"], self.engine.containers)
        self.assertNotEqual(self.engine.containers[handle["name"]]["Id"], identity)
        self.assertIn(["container", "rm", "--force", identity], self.engine.calls)

    def test_cleanup_finds_renamed_owned_container_by_label(self):
        runner, ctx, handle, report = self.prepared()
        owned = self.engine.containers.pop(handle["name"])
        owned["Name"] = "/renamed-by-host"
        self.engine.containers["renamed-by-host"] = owned
        self.assertTrue(runner.release(ctx)["confirmed"])
        self.assertEqual(self.engine.containers, {})
        self.assertIn(["container", "rm", "--force", owned["Id"]], self.engine.calls)

    def test_cleanup_outage_or_removal_failure_stays_unconfirmed(self):
        runner, ctx, handle, report = self.prepared()
        self.engine.unreachable = True
        failed = runner.release(ctx)
        self.assertEqual(failed["status"], "failed")
        self.assertFalse(failed["confirmed"])
        self.engine.unreachable = False
        self.engine.fail_remove = True
        self.assertFalse(runner.release(ctx)["confirmed"])
        self.assertIn(handle["name"], self.engine.containers)
        self.engine.fail_remove = False
        self.assertTrue(runner.release(ctx)["confirmed"])

    def test_create_failure_after_resource_exists_cleans_it_and_cannot_retry(self):
        runner = FakeRunner(profile(self.base), self.engine)
        ctx = context(self.root)
        persist(runner, ctx)
        self.engine.fail_create = True
        with self.assertRaisesRegex(EnvironmentError, "creation failed") as raised:
            runner.prepare(ctx)
        self.assertIn("cleanup", "\n".join(raised.exception.__notes__))
        self.assertEqual(self.engine.containers, {})
        with self.assertRaises(EnvironmentError):
            runner.prepare(ctx)
        self.assertEqual(self.engine.count(["run"]), 1)

    def test_failed_inventory_releases_container(self):
        runner = FakeRunner(profile(self.base, tools=[{"name": "missing", "argv": ["missing", "--version"]}]), self.engine)
        ctx = context(self.root)
        persist(runner, ctx)
        original = self.fake_process
        self.process.side_effect = lambda argv, *a, **kw: (
            result(exit_code=127) if argv[-1] == "missing --version" else original(argv, *a, **kw))
        with self.assertRaisesRegex(EnvironmentError, "required starting-inventory"):
            runner.prepare(ctx)
        self.assertEqual(self.engine.containers, {})

    def test_image_volumes_platform_and_missing_cgroup_support_fail_before_create(self):
        runner = FakeRunner(profile(self.base), self.engine)
        ctx = context(self.root)
        self.engine.image["Config"]["Volumes"] = {"/implicit": {}}
        with self.assertRaisesRegex(EnvironmentError, "VOLUMEs"):
            runner.plan(ctx)
        self.engine.image["Config"]["Volumes"] = None
        self.engine.image["Os"] = "windows"
        with self.assertRaisesRegex(EnvironmentError, "Linux"):
            runner.plan(ctx)
        self.engine.image["Os"] = "linux"
        self.engine.info["PidsLimit"] = False
        with self.assertRaisesRegex(EnvironmentError, "PidsLimit"):
            runner.plan(ctx)
        self.assertEqual(self.engine.count(["run"]), 0)

    def test_management_subprocess_environment_does_not_inherit_host_or_worker_auth(self):
        runner = EnvironmentRunner(profile(self.base))
        runner._docker_binary = "/synthetic/docker"
        completed = subprocess.CompletedProcess([], 0, stdout="{}", stderr="")
        with mock.patch.dict(os.environ, {"SYNTHETIC_HOST_TOKEN": "not-for-docker"}):
            with mock.patch.object(envmod.subprocess, "run", return_value=completed) as command:
                runner._docker(["info"])
        kwargs = command.call_args.kwargs
        self.assertNotIn("SYNTHETIC_HOST_TOKEN", kwargs["env"])
        self.assertEqual(kwargs["env"]["HOME"], "/nonexistent")
        self.assertIn("--config", command.call_args.args[0])
        self.assertIn("--host", command.call_args.args[0])


@unittest.skipUnless(os.environ.get("AJX_TEST_CONTAINER_IMAGE"), "set AJX_TEST_CONTAINER_IMAGE to opt into real Docker")
class RealDockerIntegrationTest(TempTest):
    """Synthetic, credential-free proof against a preloaded Linux Python image."""

    def make_real(self, name, **options):
        runner = EnvironmentRunner(profile(self.base, image=os.environ["AJX_TEST_CONTAINER_IMAGE"], **options))
        ctx = context(self.root, name=name)
        handle = persist(runner, ctx)
        self.addCleanup(self.assert_released, runner, ctx)
        report = runner.prepare(ctx)
        return runner, ctx, handle, report

    def assert_released(self, runner, ctx):
        cleanup = runner.release(ctx)
        self.assertTrue(cleanup["confirmed"], cleanup)

    def test_persistent_tools_write_and_network_boundaries_two_attempts_and_release(self):
        fixture = self.fixture()
        unrelated = self.root / "unrelated-host-directory"
        unrelated.mkdir()
        host_sentinel = unrelated / "private-sentinel.txt"
        host_sentinel.write_text("synthetic unrelated host data")
        options = {"mounts": [{"source": "fixtures", "target": "/fixtures", "access": "read"}],
                   "tools": [{"name": "python", "argv": ["python3", "--version"]}],
                   "required": sorted(envmod._REQUIRED)}
        first, a, plan_a, report = self.make_real("first", **options)
        second, b, plan_b, _ = self.make_real("second", **options)
        self.assertNotEqual(plan_a["owner"], plan_b["owner"])
        self.assertEqual(report["inventory"]["tools"][0]["status"], "observed")
        setup = first.shell(
            'mkdir -p "$HOME/.local/bin"; '
            'printf \'#!/bin/sh\\nprintf "installed-tool-ok\\\\n"\\n\' > "$HOME/.local/bin/ajx-probe"; '
            'chmod +x "$HOME/.local/bin/ajx-probe"; printf persistent > installed-during-setup.txt', a)
        self.assertEqual(setup["exit_code"], 0, setup)
        execute = first.shell("ajx-probe; cat installed-during-setup.txt", a)
        self.assertEqual(execute["exit_code"], 0, execute)
        self.assertEqual(execute["stdout"], "installed-tool-ok\npersistent")
        self.assertEqual(first.shell("ajx-probe", a)["exit_code"], 0)  # narration sees same install
        checks = {
            "read fixture": "test -r /fixtures/input.txt",
            "read-only fixture": "if printf bad >> /fixtures/input.txt 2>/dev/null; then exit 90; fi",
            "read-only root": "if printf bad > /ajx-forbidden-root 2>/dev/null; then exit 90; fi",
            "unrelated host absent": "test ! -e " + shlex.quote(str(host_sentinel)),
            "report absent": "test ! -e " + shlex.quote(str(a["run_dir"])),
            "other workspace absent": "test ! -e " + shlex.quote(str(b["workspace"])),
            "cannot stop supervisor": "if kill -STOP 1 2>/dev/null; then exit 90; fi",
            "non-root identity": 'test "$(id -u)" != 0',
        }
        for label, command in checks.items():
            with self.subTest(boundary=label):
                response = first.shell(command, a)
                self.assertEqual(response["exit_code"], 0, response)
        # A local host-only listener is unreachable from network=none; no public endpoint/credentials.
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            listener.listen()
            port = listener.getsockname()[1]
            script = ("import errno,os,socket\n"
                      "assert sorted(os.listdir('/sys/class/net')) == ['lo']\n"
                      "s=socket.socket();s.settimeout(1)\n"
                      f"try:s.connect(('127.0.0.1',{port}))\n"
                      "except OSError:pass\n"
                      "else:raise SystemExit('host listener unexpectedly reachable')\n"
                      "s=socket.socket();s.settimeout(1)\n"
                      "try:s.connect(('192.0.2.1',9))\n"
                      "except OSError as e:assert e.errno in (errno.ENETUNREACH,errno.EHOSTUNREACH),e\n"
                      "else:raise SystemExit('network unexpectedly reachable')\n")
            response = first.shell("python3 -c " + shlex.quote(script), a)
            self.assertEqual(response["exit_code"], 0, response)
        isolated = second.shell('test ! -e installed-during-setup.txt; test ! -e "$HOME/.local/bin/ajx-probe"', b)
        self.assertEqual(isolated["exit_code"], 0, isolated)
        self.assertEqual((fixture / "input.txt").read_text(), "synthetic starting data\n")
        self.assertTrue(first.release(a)["confirmed"])
        self.assertTrue(second.release(b)["confirmed"])
        self.assertIsNone(first._find(plan_a, a))
        self.assertIsNone(second._find(plan_b, b))
        self.assertEqual((a["workspace"] / "installed-during-setup.txt").read_text(), "persistent")

    def test_timeout_stops_detached_workers_and_keeps_environment_for_teardown(self):
        runner, ctx, handle, _ = self.make_real("timeout")
        worker = ("from pathlib import Path; import time\n"
                  "while True:\n"
                  " with Path('heartbeat').open('a') as out: out.write('alive\\n')\n"
                  " time.sleep(0.02)\n")
        launch = ("import subprocess,time\n"
                  "from pathlib import Path\n"
                  f"subprocess.Popen(['python3','-c',{worker!r}],start_new_session=True)\n"
                  "Path('before-timeout').write_text('evidence')\n"
                  "time.sleep(120)\n")
        response = runner.shell("python3 -c " + shlex.quote(launch), ctx, timeout=1)
        self.assertTrue(response["timed_out"], response)
        self.assertTrue(response["environment_abort"]["confirmed"], response)
        self.assertTrue(response["environment_abort"]["environment_retained"], response)
        self.assertIsNotNone(runner._find(handle, ctx))
        heartbeat = (ctx["workspace"] / "heartbeat").read_bytes()
        self.assertTrue(heartbeat)
        time.sleep(0.1)
        self.assertEqual((ctx["workspace"] / "heartbeat").read_bytes(), heartbeat)
        self.assertEqual((ctx["workspace"] / "before-timeout").read_text(), "evidence")
        cleanup = runner.shell('printf cleaned > "$HOME/teardown-ran"', ctx)
        self.assertEqual(cleanup["exit_code"], 0, cleanup)
        self.assertEqual((ctx["home_dir"] / "teardown-ran").read_text(), "cleaned")
        self.assertTrue(runner.release(ctx)["confirmed"])
        self.assertIsNone(runner._find(handle, ctx))

    def test_harness_timeout_retains_prepared_tools_for_teardown(self):
        runner, ctx, handle, _ = self.make_real("harness-timeout")
        ctx.update(runner=runner, environment_profile=runner.profile, env=runner.environment_env(ctx))
        prepared = runner.shell('printf ready > "$HOME/prepared-tool"', ctx)
        self.assertEqual(prepared["exit_code"], 0, prepared)
        response = basemod.Harness().run(
            ctx, ["/bin/sh", "-c", "printf evidence > before-timeout; sleep 120 & wait"],
            "execute", timeout=1,
        )
        self.assertTrue(response["timed_out"], response)
        self.assertTrue(response["environment_abort"]["environment_retained"], response)
        cleanup = runner.shell('test "$(cat "$HOME/prepared-tool")" = ready && '
                               'printf cleaned > "$HOME/teardown-ran"', ctx)
        self.assertEqual(cleanup["exit_code"], 0, cleanup)
        self.assertEqual((ctx["home_dir"] / "teardown-ran").read_text(), "cleaned")
        self.assertEqual((ctx["workspace"] / "before-timeout").read_text(), "evidence")
        self.assertTrue(runner.release(ctx)["confirmed"])

    def test_lifetime_expiry_without_release_or_a_coordinator_timer(self):
        runner, ctx, handle, _ = self.make_real("expiry", limits={"lifetime_seconds": 12})
        # This launches directly through wrap, bypassing shell's timeout/abort entirely.
        worker_env = runner._worker_env(ctx)
        argv = runner.wrap(["/bin/sh", "-c", "printf started > began; sleep 120"], ctx, worker_env)
        started = time.monotonic()
        proc = subprocess.run(argv, cwd=ctx["workspace"], env=worker_env, capture_output=True, timeout=20)
        self.assertLess(time.monotonic() - started, 20)
        self.assertNotEqual(proc.returncode, 0, proc)
        # Docker's automatic removal may still be finishing after the worker exits.
        deadline = time.monotonic() + 10
        remaining = runner._find(handle, ctx)
        while remaining is not None and time.monotonic() < deadline:
            self.assertFalse(remaining["State"]["Running"])
            time.sleep(0.1)
            remaining = runner._find(handle, ctx)
        self.assertIsNone(remaining)
        recovered = EnvironmentRunner(runner.profile)
        with self.assertRaises(EnvironmentError):
            recovered.prepare(ctx)
        self.assertEqual((ctx["workspace"] / "began").read_text(), "started")


if __name__ == "__main__":
    unittest.main()
