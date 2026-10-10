"""Coordinator-experience fixes, offline with synthetic fixtures: a read-only `ajx status` load that
needs no caller secrets, AWS_DEFAULT_REGION mirroring for claude-bedrock, AJX_TRIAL_DIR for lifecycle
commands, and a wider credential purge. No harness binary, model provider, or container is used."""

import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))

from ajx import envcheck, plugins, runner, spec as specmod  # noqa: E402
from ajx.base import Harness  # noqa: E402
from ajx.util import read_json  # noqa: E402

IMAGE = "sha256:" + "a" * 64
REGION = {"AWS_REGION": "us-east-1"}
KEYS = {"AWS_ACCESS_KEY_ID": "synthetic-access", "AWS_SECRET_ACCESS_KEY": "synthetic-secret"}


def inline(mapping):
    return "{ " + ", ".join(f"{key} = {json.dumps(value)}" for key, value in mapping.items()) + " }"


def bedrock_trial(root, *, auth_env, env=None, cell_env=None, unset=()):
    """A container-backed claude-bedrock cell: nothing here can launch, so loading is pure validation."""
    root = Path(root)
    (root / "task.md").write_text("Synthetic trial; no provider calls.\n")
    path = root / "trial.toml"
    path.write_text(
        '[trial]\nid = "coordinator"\nproduct = "synthetic"\noutput_dir = "results"\n'
        f'workspace_root = {json.dumps(str(root / "workspaces"))}\nenvironment = "worker"\n'
        '[task]\nprompt_file = "task.md"\n'
        '[env]\n' + "".join(f"{key} = {json.dumps(value)}\n" for key, value in (env or {}).items())
        + f'[environments.worker]\nbackend = "container"\nimage = "{IMAGE}"\n'
        + f'[auth.plain]\ntype = "claude-bedrock"\nenv = {inline(auth_env)}\nunset = {json.dumps(list(unset))}\n'
        + '[reporter]\nharness = "claude-code"\nauth = "inherit"\n'
        + '[[cells]]\nid = "plain"\nharness = "claude-code"\nauth = "plain"\n'
        + f'env = {inline(cell_env or {})}\n')
    return path


def cli():
    import importlib.util
    from importlib.machinery import SourceFileLoader
    path = str(HERE.parent / "bin" / "ajx")
    loader = SourceFileLoader("ajx_cli_coordinator", path)
    module_spec = importlib.util.spec_from_file_location("ajx_cli_coordinator", path, loader=loader)
    module = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(module)
    return module


class OfflineTrialCase(unittest.TestCase):
    """A temp trial dir, a caller environment holding only HOME, and no subprocess at all."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        home = self.root / "caller-home"
        home.mkdir()
        for patch in (mock.patch.dict(os.environ, {"HOME": str(home)}, clear=True),
                      mock.patch("subprocess.run", side_effect=AssertionError("must not launch anything"))):
            patch.start()
            self.addCleanup(patch.stop)

    def load(self, **options):
        spec = specmod.load(bedrock_trial(self.root, **options))
        cell = spec["cells"][0]
        return spec, cell, specmod.auth_for(spec, cell)


class StatusWithoutSecretsTest(OfflineTrialCase):
    """`ajx status` loads the trial with cleanup_only=True. A read-only command must not need the
    secrets an auth profile references (${VAR}) to be exported in the shell it runs from."""

    REFERENCES = {**REGION, "AWS_ACCESS_KEY_ID": "${AJX_TEST_MISSING_KEY}",
                  "AWS_SECRET_ACCESS_KEY": "${AJX_TEST_MISSING_SECRET}"}

    def test_cleanup_only_load_does_not_resolve_caller_secrets(self):
        path = bedrock_trial(self.root, auth_env=self.REFERENCES)
        self.assertNotIn("AJX_TEST_MISSING_KEY", os.environ)
        with self.assertRaisesRegex(specmod.SpecError, "not ready for the selected environment") as raised:
            specmod.load(path)   # a run still needs the secrets
        self.assertIn("${AJX_TEST_MISSING_KEY}", str(raised.exception))
        spec = specmod.load(path, cleanup_only=True)
        self.assertTrue(spec["_cleanup_only"])
        self.assertEqual(spec["trial"]["output_dir"], str(self.root / "results"))
        # cells and the reporter still resolve to auth objects without touching the caller's environment
        cell = spec["cells"][0]
        auth = specmod.auth_for(spec, cell)
        self.assertEqual((auth.profile, auth.name), ("plain", "claude-bedrock"))
        reporter = spec["reporter"]
        self.assertEqual(specmod.auth_for(spec, {"id": "reporter", "harness": reporter["harness"],
                                                 "auth": reporter.get("auth")}).name, "inherit")
        self.assertFalse((self.root / "results").exists())
        self.assertFalse((self.root / "workspaces").exists())

    def test_cleanup_only_load_keeps_structural_validation(self):
        path = bedrock_trial(self.root, auth_env=self.REFERENCES, unset=["AWS_REGION"])
        with self.assertRaisesRegex(specmod.SpecError, "both supplied and unset"):
            specmod.load(path, cleanup_only=True)
        with self.assertRaisesRegex(specmod.SpecError, "both supplied and unset"):
            specmod.load(path)

    def test_status_command_runs_without_the_trial_secrets(self):
        path = bedrock_trial(self.root, auth_env=self.REFERENCES)
        import contextlib
        import io
        out = io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()):
            code = cli().main(["status", str(path)])
        self.assertEqual(code, 0, out.getvalue())
        self.assertIn("verified", out.getvalue())
        self.assertNotIn("AJX_TEST_MISSING_KEY", out.getvalue())


class BedrockRegionMirrorTest(OfflineTrialCase):
    """botocore reads AWS_DEFAULT_REGION while Claude Code and the AWS CLI read AWS_REGION: a claude-bedrock
    profile that declares only AWS_REGION gives the worker both, and an identical task-side value is
    not a collision."""

    def launch_env(self, spec, cell, auth):
        rn = specmod.runner_for(spec, cell)
        ctx = {"env": specmod.worker_env(spec, cell, auth), "unset_env": auth.unset(), "auth": auth,
               "environment_profile": specmod.environment_for(spec, cell), "runner": rn,
               "workspace": self.root, "run_dir": self.root, "timeout": 1}
        with mock.patch.object(rn, "wrap", return_value=["synthetic-harness"]), \
                mock.patch("ajx.base.run_streaming", return_value={"exit_code": 0, "timed_out": False}) as launch:
            Harness().run(ctx, ["synthetic-harness"], "execute")
        return launch.call_args.kwargs["env"]

    def view(self, spec, cell, auth):
        return {"effective": specmod.worker_env(spec, cell, auth)}

    def inspect(self, spec, cell, auth):
        harness = specmod.harness_for(spec, cell["harness"])
        rn = specmod.runner_for(spec, cell)
        scratch = self.root / "scratch"
        scratch.mkdir(exist_ok=True)
        with mock.patch.object(type(harness), "managed_settings", scratch / "no-managed-settings.json"):
            ctx = envcheck.placeholder_ctx(spec, cell, auth, rn, scratch)
            return envcheck.inspect_cell(spec, cell, harness, auth, rn, envcheck.scratch_workspace(spec, scratch), ctx)

    def test_worker_receives_aws_default_region_mirrored_from_aws_region(self):
        spec, cell, auth = self.load(auth_env={**REGION, **KEYS})
        self.assertEqual(auth.problems(), [])
        self.assertEqual(auth.env()["AWS_DEFAULT_REGION"], "us-east-1")
        worker = specmod.worker_env(spec, cell, auth)
        self.assertEqual(worker["AWS_DEFAULT_REGION"], "us-east-1")
        self.assertEqual(self.launch_env(spec, cell, auth)["AWS_DEFAULT_REGION"], "us-east-1")
        self.assertIn("AWS_DEFAULT_REGION", auth.model_vars(worker))
        self.assertIn("AWS_DEFAULT_REGION", auth.describe()["env_names"])
        for value in KEYS.values():
            self.assertNotIn(value, json.dumps(auth.describe()))

    def test_mirror_follows_references_and_defers_to_explicit_declarations(self):
        with mock.patch.dict(os.environ, {"AJX_TEST_REGION": "eu-central-1"}):
            spec, cell, auth = self.load(auth_env={"AWS_REGION": "${AJX_TEST_REGION}", **KEYS})
            self.assertEqual(specmod.worker_env(spec, cell, auth)["AWS_DEFAULT_REGION"], "eu-central-1")
        spec, cell, auth = self.load(auth_env={**REGION, "AWS_DEFAULT_REGION": "us-west-2", **KEYS})
        self.assertEqual(specmod.worker_env(spec, cell, auth)["AWS_DEFAULT_REGION"], "us-west-2")
        spec, cell, auth = self.load(auth_env={**REGION, **KEYS}, unset=["AWS_DEFAULT_REGION"])
        self.assertNotIn("AWS_DEFAULT_REGION", specmod.worker_env(spec, cell, auth))
        self.assertNotIn("AWS_DEFAULT_REGION", self.launch_env(spec, cell, auth))
        bedrock = plugins.get("auth", "claude-bedrock")
        self.assertNotIn("AWS_DEFAULT_REGION", bedrock({"env": {"AWS_REGION": "${AJX_TEST_MISSING_REGION}"}}).env())
        self.assertNotIn("AWS_DEFAULT_REGION", bedrock({"env": {"AWS_REGION": "  "}}).env())
        self.assertNotIn("AWS_DEFAULT_REGION", bedrock({}).env())

    def test_identical_task_side_region_is_not_reported_as_a_collision(self):
        for layer in ("env", "cell_env"):
            with self.subTest(layer=layer):
                spec, cell, auth = self.load(auth_env={**REGION, **KEYS}, **{layer: {"AWS_DEFAULT_REGION": "us-east-1"}})
                self.assertEqual(envcheck.collision_findings(spec, cell, auth, self.view(spec, cell, auth)), [])
        # the same holds for any key the profile supplies verbatim, and through doctor's full inspection
        spec, cell, auth = self.load(auth_env={**REGION, **KEYS}, env={"AWS_REGION": "us-east-1"})
        self.assertEqual(envcheck.collision_findings(spec, cell, auth, self.view(spec, cell, auth)), [])
        spec, cell, auth = self.load(auth_env={**REGION, **KEYS}, env={"AWS_DEFAULT_REGION": "us-east-1"})
        checks, view = self.inspect(spec, cell, auth)
        self.assertEqual(checks["collisions"], [])
        self.assertFalse([w for w in envcheck.warnings(checks, auth, view) if "model access" in w])
        self.assertEqual(envcheck.record(checks)["collisions"], [])

    def test_different_task_side_value_is_still_a_collision_without_printing_values(self):
        spec, cell, auth = self.load(auth_env={**REGION, **KEYS}, env={"AWS_DEFAULT_REGION": "eu-west-1"})
        view = self.view(spec, cell, auth)
        names = envcheck.collision_findings(spec, cell, auth, view)
        self.assertEqual(names, ["AWS_DEFAULT_REGION"])
        text = envcheck.collision_text(names, auth, view)
        self.assertIn("AWS_DEFAULT_REGION", text)
        for value in ("eu-west-1", "us-east-1"):
            self.assertNotIn(value, text)
        checks, view = self.inspect(spec, cell, auth)
        self.assertEqual(checks["collisions"], ["AWS_DEFAULT_REGION"])
        self.assertTrue([w for w in envcheck.warnings(checks, auth, view) if "model access" in w])
        # a differing secret is reported by name only
        spec, cell, auth = self.load(auth_env={**REGION, **KEYS},
                                     cell_env={"AWS_SECRET_ACCESS_KEY": "synthetic-other-secret"})
        view = self.view(spec, cell, auth)
        names = envcheck.collision_findings(spec, cell, auth, view)
        self.assertEqual(names, ["AWS_SECRET_ACCESS_KEY"])
        for value in ("synthetic-other-secret", "synthetic-secret"):
            self.assertNotIn(value, envcheck.collision_text(names, auth, view))
        # a cell value that loses to the profile is reported even when [env] happens to match
        spec, cell, auth = self.load(auth_env={**REGION, **KEYS}, env={"AWS_DEFAULT_REGION": "us-east-1"},
                                     cell_env={"AWS_DEFAULT_REGION": "eu-west-1"})
        self.assertEqual(envcheck.collision_findings(spec, cell, auth, self.view(spec, cell, auth)),
                         ["AWS_DEFAULT_REGION"])


@plugins.register("harness", "coordinator-fake")
class CoordinatorFakeHarness(Harness):
    """Exits 0 after recording whether the worker saw AJX_TRIAL_DIR; the lifecycle commands around it
    are what these tests observe."""
    binary = "true"
    clean_supported = True

    def plan(self, ctx):
        return {"session_id": "coordinator-sid"}

    def execute(self, ctx):
        proc = self.run(ctx, ["sh", "-c", 'printf %s "${AJX_TRIAL_DIR-unset}" > worker-saw.txt'], "execute")
        return {**proc, "session_id": "coordinator-sid"}


class TrialDirForLifecycleCommandsTest(unittest.TestCase):
    """setup, preflight, verify and teardown commands see AJX_TRIAL_DIR (the directory holding trial.toml)
    next to AJX_RUN_TOKEN, on the host and inside an environment; the worker's harness never does."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        (self.root / "task-prompt.md").write_text("x\n")

    def trial(self, body, cells='[[cells]]\nid = "f"\nharness = "coordinator-fake"\n', trial_extra=""):
        path = self.root / "trial.toml"
        path.write_text(f'[trial]\nid = "t"\nproduct = "p"\nworkspace_root = {json.dumps(str(self.root / "ws"))}\n'
                        f'{trial_extra}[task]\nprompt_file = "task-prompt.md"\n{body}\n{cells}')
        return specmod.load(path)

    def test_check_env_carries_the_trial_dir_but_the_worker_env_does_not(self):
        spec = self.trial("")
        cell = spec["cells"][0]
        auth = specmod.auth_for(spec, cell)
        rn = specmod.runner_for(spec, cell)
        expected = str(self.root)
        self.assertEqual(envcheck.check_vars(spec, "doctor"), {"AJX_RUN_TOKEN": "doctor", "AJX_TRIAL_DIR": expected})
        self.assertEqual(envcheck.check_vars(None, "t"), {"AJX_RUN_TOKEN": "t"})
        env = envcheck.command_env({"TASK_FLAG": "1"}, [], "tok", spec)
        self.assertEqual((env["AJX_RUN_TOKEN"], env["AJX_TRIAL_DIR"], env["TASK_FLAG"]), ("tok", expected, "1"))
        self.assertNotIn("AJX_TRIAL_DIR", envcheck.command_env({}, [], "tok"))   # callers without a spec
        ctx = envcheck.placeholder_ctx(spec, cell, auth, rn, self.root / "scratch")
        self.assertEqual(ctx["check_env"]["AJX_TRIAL_DIR"], expected)
        self.assertNotIn("AJX_TRIAL_DIR", ctx["env"])
        self.assertNotIn("AJX_TRIAL_DIR", specmod.worker_env(spec, cell, auth))
        run_ctx = runner.Run(spec, cell, 1, lambda m: None).ctx()
        self.assertEqual(run_ctx["check_env"]["AJX_TRIAL_DIR"], expected)
        self.assertNotIn("AJX_TRIAL_DIR", run_ctx["env"])

    def test_host_lifecycle_commands_see_the_trial_dir(self):
        spec = self.trial(
            '[[preflight]]\nname = "trial dir"\nrun = \'test -f "$AJX_TRIAL_DIR/trial.toml" && test -n "$AJX_RUN_TOKEN"\'\n'
            '[[setup]]\nrun = \'printf %s "$AJX_TRIAL_DIR" > setup-saw.txt\'\n'
            f'[[verify]]\nname = "same dir"\nrun = \'test "$AJX_TRIAL_DIR" = "{self.root}"\'\n'
            '[[teardown]]\nrun = \'echo "dir=$AJX_TRIAL_DIR token=$AJX_RUN_TOKEN"\'\n')
        run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
        state = run.go(stages=["prepare", "execute", "verify", "teardown"])
        statuses = {k: v["status"] for k, v in state["stages"].items()}
        self.assertEqual(statuses, dict.fromkeys(("prepare", "execute", "verify", "teardown"), "done"), state["stages"])
        self.assertTrue(read_json(run.run_dir / "preflight.json")[0]["passed"])
        workspace = Path(state["paths"]["workspace"])
        self.assertEqual((workspace / "setup-saw.txt").read_text(), str(self.root))
        self.assertEqual((workspace / "worker-saw.txt").read_text(), "unset")   # never the harness
        self.assertEqual(read_json(run.run_dir / "verify.json")["outcome"], "succeeded")
        self.assertIn(f"dir={self.root} token={state['run_token']}", read_json(run.run_dir / "teardown.json")[0]["stdout"])

    def test_environment_and_host_located_commands_see_the_trial_dir(self):
        spec = self.trial(
            '[environments.local]\nbackend = "local"\n'
            '[auth.test]\ntype = "env"\nisolates_config = true\n[reporter]\nharness = "codex"\n'
            '[[preflight]]\nname = "trial dir"\nrun = \'test -f "$AJX_TRIAL_DIR/trial.toml"\'\n'
            '[[setup]]\nrun = \'printf %s "$AJX_TRIAL_DIR" > setup-saw.txt\'\n'
            f'[[verify]]\nname = "host sees it"\nrun = \'test "$AJX_TRIAL_DIR" = "{self.root}" && test -n "$AJX_RUN_TOKEN"\'\n'
            f'[[verify]]\nname = "environment saw it"\ntype = "file_exists"\npath = "setup-saw.txt"\ncontains = {json.dumps(str(self.root))}\n'
            '[[teardown]]\nlocation = "host"\nrun = \'echo "dir=$AJX_TRIAL_DIR"\'\n',
            cells='[[cells]]\nid = "local"\nharness = "coordinator-fake"\nauth = "test"\n',
            trial_extra='environment = "local"\n')
        self.assertEqual(specmod.environment_for(spec, spec["cells"][0])["backend"], "local")
        self.assertEqual([c["location"] for c in spec["preflight"] + spec["setup"]], ["environment", "environment"])
        self.assertEqual([c["location"] for c in spec["verify"] + spec["teardown"]], ["host", "host", "host"])
        run = runner.Run(spec, spec["cells"][0], 1, lambda m: None)
        try:
            state = run.go(stages=["prepare", "execute", "verify", "teardown"])
            statuses = {k: v["status"] for k, v in state["stages"].items()}
            self.assertEqual(statuses, dict.fromkeys(("prepare", "execute", "verify", "teardown"), "done"),
                             state["stages"])
            preflight = read_json(run.run_dir / "preflight.json")[0]
            self.assertEqual((preflight["passed"], preflight["location"]), (True, "environment"))
            workspace = Path(state["paths"]["workspace"])
            self.assertEqual((workspace / "setup-saw.txt").read_text(), str(self.root))
            self.assertEqual((workspace / "worker-saw.txt").read_text(), "unset")
            verify = read_json(run.run_dir / "verify.json")
            self.assertEqual((verify["outcome"], verify["passed"]), ("succeeded", 2), verify["checks"])
            teardown = read_json(run.run_dir / "teardown.json")[0]
            self.assertEqual(teardown["location"], "host")
            self.assertIn(f"dir={self.root}", teardown["stdout"])
        finally:
            run.stage_archive()


class CredentialPurgeTest(unittest.TestCase):
    """Registry and netrc-style token files a setup step leaves in the worker's home are purged by name
    before the home is archived; everything else stays, and symlinks are removed without being followed."""

    def test_purge_removes_token_dotfiles_and_keeps_other_files(self):
        for name in (".npmrc", ".netrc", ".pypirc", ".git-credentials"):
            self.assertIn(name, runner.CREDENTIAL_FILES)
        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp) / "home"
            nested = home / "project" / "nested"
            nested.mkdir(parents=True)
            (home / ".npmrc").write_text("//registry.example.test/:_authToken=synthetic-registry-token\n")
            (nested / ".npmrc").write_text("registry=https://registry.example.test/\n")
            (home / ".netrc").write_text("machine example.test login user password synthetic\n")
            (nested / ".pypirc").write_text("[pypi]\npassword = synthetic\n")
            (home / ".git-credentials").write_text("https://user:synthetic@example.com\n")
            outside = Path(tmp) / "outside-target.txt"
            outside.write_text("link target stays")
            (nested / ".netrc").symlink_to(outside)
            (home / "notes.txt").write_text("keep")
            (home / "project" / "package.json").write_text("{}")
            (home / "project" / "npmrc").write_text("not a dotfile")
            (home / "project" / ".npmrc.bak").write_text("different name")
            fd = os.open(home, os.O_RDONLY | os.O_DIRECTORY)
            try:
                runner._purge_credential_tree(fd)
            finally:
                os.close(fd)
            remaining = sorted(str(p.relative_to(home)) for p in home.rglob("*") if not p.is_dir())
            self.assertEqual(remaining, ["notes.txt", "project/.npmrc.bak", "project/npmrc", "project/package.json"])
            self.assertEqual(outside.read_text(), "link target stays")


if __name__ == "__main__":
    unittest.main()
