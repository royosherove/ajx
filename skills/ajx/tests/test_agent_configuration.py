"""Synthetic extension-profile tests, plus opt-in Docker backend wiring checks.

AJX_TEST_CONTAINER_IMAGE enables the real-container test with a preloaded immutable
Linux Python image (CI uses python:3.13-alpine's full image ID). No images are
pulled and network access is disabled. Synthetic executables verify control
arguments, isolated environment variables, and skill-file visibility, not actual
skill discovery/invocation by vendor harnesses. Docker and Harness.run are real.
"""

import copy
import hashlib
import json
import os
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "lib"))

from ajx import agent_configuration as ac  # noqa: E402
from ajx import environments as envmod  # noqa: E402
from ajx.environments import EnvironmentRunner  # noqa: E402
from ajx.base import Auth  # noqa: E402
from ajx.builtin.claude_code import ClaudeCode, ClaudeSubscription  # noqa: E402
from ajx.builtin.codex import Codex, CodexChatGPT, CodexProvider, OpenAIApi  # noqa: E402
from ajx.builtin.generic import EnvAuth, LocalRunner  # noqa: E402
from ajx.builtin.kiro_cli import KiroCli, KiroLogin  # noqa: E402


class FakeEnvironmentRunner:
    """Backend contract exercised without Docker, a provider, or a real agent."""
    def __init__(self, backend="container"):
        self.backend = self.name = backend
        self.calls = []
        self.scope_violation = ""

    def environment_env(self, ctx):
        return {"HOME": str(ctx["home_dir"]), "XDG_CONFIG_HOME": str(ctx["config_dir"]),
                "XDG_DATA_HOME": str(ctx["home_dir"] / ".local/share"),
                "XDG_STATE_HOME": str(ctx["home_dir"] / ".local/state"),
                "XDG_CACHE_HOME": str(ctx["cache_dir"]), "PATH": "/usr/local/bin:/usr/bin:/bin",
                "PYTHONUSERBASE": str(ctx["home_dir"] / ".local")}

    def child_env(self, overrides=None, unset=()):
        return EnvironmentRunner.child_env(self, overrides, unset)

    def shell(self, command, ctx, timeout=600, env=None):
        self.calls.append((command, ctx, env))
        if command.startswith("ajx_probe_text=$("):
            if "features list" in command:
                text = "\n".join(k + " stable false" for k in ("plugins", "hooks", "apps", "skill_mcp_dependency_install"))
            else:
                text = "--strict-config --config --disable --setting-sources --settings --disable-slash-commands " \
                       "--strict-mcp-config --agent --agent-engine --no-interactive"
            return {"exit_code": 0, "stdout": text, "timed_out": False}
        args = shlex.split(command)
        if command.startswith("if [ -e "):
            return {"exit_code": 42 if self.scope_violation else 0,
                    "stdout": self.scope_violation, "timed_out": False}
        if args[:2] == ["command", "-v"]:
            return {"exit_code": 0, "stdout": "/container/bin/" + args[2] + "\n", "timed_out": False}
        executable = Path(args[0]).name
        versions = {"codex": "0.160.1", "claude": "2.1.293", "kiro-cli-chat": "2.28.0"}
        if args[1:] == ["--version"]:
            text = versions[executable]
        elif args[-2:] == ["features", "list"]:
            text = "\n".join(k + " stable false" for k in ("plugins", "hooks", "apps", "skill_mcp_dependency_install"))
        elif args[-1:] == ["--help"]:
            text = "--strict-config --config --disable --setting-sources --settings --disable-slash-commands " \
                   "--strict-mcp-config --agent --agent-engine --no-interactive"
        else:
            raise AssertionError("Non-capability command in fake environment: " + command)
        return {"exit_code": 0, "stdout": text, "timed_out": False}

    def wrap(self, argv, ctx, env):
        return list(argv)


class ProfileTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ajx-profile-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.source = self.root / "sources" / "product-guide"
        self.source.mkdir(parents=True)
        (self.source / "SKILL.md").write_text(
            "---\nname: product-guide\ndescription: Use the fictional widget tool.\n---\n"
            "Read assets/guide.txt and run scripts/check.sh when needed.\n", encoding="utf-8")
        (self.source / "assets").mkdir()
        (self.source / "assets" / "guide.txt").write_text("Fictional reference material.\n")
        (self.source / "scripts").mkdir()
        (self.source / "scripts" / "check.sh").write_text("#!/bin/sh\nprintf 'synthetic\\n'\n")
        (self.source / "scripts" / "check.sh").chmod(0o700)
        self.system = mock.patch.object(ac, "_SYSTEM_PATHS", {k: () for k in ac._HARNESSES})
        self.system.start()
        self.addCleanup(self.system.stop)
        self.probe = mock.patch.object(ac, "_probe", side_effect=self.fake_probe)
        self.probe.start()
        self.addCleanup(self.probe.stop)
        self.counter = 0

    @staticmethod
    def fake_probe(name, ctx):
        return {"binary": {"codex": "codex", "claude-code": "claude", "kiro-cli": "kiro-cli-chat"}[name],
                "version": "synthetic", "provider_called": False, "live_extension_activation_verified": False}

    def profile(self, mode="selected", **extra):
        skills = {"mode": mode}
        if mode != "none":
            skills["paths"] = [str(self.source.relative_to(self.root))]
        return ac.normalize_profiles({"product": {"skills": skills, **extra}}, self.root)["product"]

    def ctx(self, harness, auth=None):
        self.counter += 1
        base = self.root / ("attempt-" + str(self.counter))
        base.mkdir()
        paths = {}
        for name in ("workspace", "config_dir", "cache_dir", "home_dir", "run_dir"):
            paths[name] = base / name
            paths[name].mkdir()
        if auth is None:
            auth = EnvAuth({"required_env": ["KIRO_API_KEY"]}) if harness.name == "kiro-cli" else EnvAuth()
        return {**paths, "cell": {"id": "synthetic", "harness": harness.name, "config": "clean",
                                 "agent_configuration": "product", "args": [], "env": {}},
                "spec": {"trial": {}, "env": {}}, "env": {}, "auth": auth, "runner": LocalRunner(),
                "state": {"execute": {"session_id": "synthetic-session", "config_isolated": True}},
                "unset_env": [], "timeout": 2, "prompt_text": "Use the fictional widget."}

    def prepared(self, harness, mode="selected", auth=None):
        ctx = self.ctx(harness, auth)
        manifest = ac.prepare(self.profile(mode), ctx, harness)
        ctx["state"]["agent_configuration"] = manifest
        return ctx, manifest

    def capture(self, harness, ctx, stage="execute"):
        captured = {}

        def run(call_ctx, argv, actual_stage, **kw):
            captured.update(ctx=call_ctx, argv=argv, stage=actual_stage, **kw)
            return {"exit_code": 0}

        with mock.patch.object(harness, "run", side_effect=run):
            if stage == "execute":
                harness.execute(ctx)
            else:
                harness.narrate(ctx, "Describe the recorded attempt.")
        return captured

    def test_schema_defaults_and_complete_pins(self):
        default = ac.normalize_profiles({"plain": {}}, self.root)["plain"]
        self.assertEqual(default["skills"]["mode"], "none")
        self.assertEqual(default["plugins"], {"mode": "none", "entries": []})
        selected = self.profile()
        entry = selected["skills"]["entries"][0]
        self.assertEqual(entry["name"], "product-guide")
        self.assertEqual({f["path"] for f in entry["files"] if f["type"] == "file"},
                         {"SKILL.md", "scripts/check.sh", "assets/guide.txt"})
        self.assertTrue(next(f for f in entry["files"] if f["path"] == "scripts/check.sh")["executable"])
        pinned = {"skills": {"mode": "snapshot", "paths": [str(self.source)],
                             "pins": {"product-guide": entry["sha256"]}}}
        self.assertEqual(ac.normalize_profiles({"pinned": pinned}, self.root)["pinned"]["skills"]["entries"][0]["sha256"],
                         entry["sha256"])
        pinned["skills"]["pins"]["product-guide"] = "0" * 64
        with self.assertRaisesRegex(ValueError, "pin mismatch"):
            ac.normalize_profiles({"pinned": pinned}, self.root)

    def test_hash_covers_all_content_and_executable_bit(self):
        first = self.profile()["skills"]["entries"][0]["sha256"]
        script = self.source / "scripts/check.sh"
        script.write_text(script.read_text() + "# changed\n")
        second = self.profile()["skills"]["entries"][0]["sha256"]
        self.assertNotEqual(first, second)
        script.chmod(0o600)
        self.assertNotEqual(second, self.profile()["skills"]["entries"][0]["sha256"])
        (self.source / "assets/guide.txt").write_text("Another reference.")
        self.assertNotEqual(first, self.profile()["skills"]["entries"][0]["sha256"])

    def test_local_markdown_dependencies_must_be_bundled_and_cannot_escape(self):
        skill = self.source / "SKILL.md"
        original = skill.read_text()
        skill.write_text(original + "\n[Guide](assets/guide.txt)\n")
        self.profile()
        for link in ("../outside.txt", "/synthetic/private.md", "assets/missing.md", "file:///synthetic/private.md"):
            skill.write_text(original + "\n[Reference](" + link + ")\n")
            with self.subTest(link=link), self.assertRaisesRegex(ValueError, "skill snapshot"):
                self.profile()
        skill.write_text(original + "\n[Public documentation](https://example.invalid/reference)\n")
        self.profile()

    def test_claude_invocation_semantics_are_preserved_without_rewriting(self):
        path = self.source / "SKILL.md"
        path.write_text(path.read_text().replace("description:", "disable-model-invocation: true\n"
                                                "user-invocable: false\ndescription:"))
        profile = self.profile()
        ctx = self.ctx(ClaudeCode())
        manifest = ac.prepare(profile, ctx, ClaudeCode())
        skill = manifest["skills"]["entries"][0]
        self.assertFalse(skill["automatic_invocation"])
        self.assertFalse(skill["user_invocable"])
        self.assertEqual((ac._contained_path(skill["location"], ctx) / "SKILL.md").read_bytes(), path.read_bytes())
        for harness in (Codex(), KiroCli()):
            ctx = self.ctx(harness)
            with self.assertRaisesRegex(ValueError, "semantics cannot be preserved"):
                ac.validate_for_cell(profile, ctx["cell"], harness, ctx["auth"])

    def test_rejects_unbounded_or_implicit_snapshot_paths(self):
        for paths in ([], ["/"], ["sources/*"], ["sources"], ["https://example.invalid/skill"]):
            with self.subTest(paths=paths), self.assertRaises(ValueError):
                ac.normalize_profiles({"test": {"skills": {"mode": "snapshot", "paths": paths}}}, self.root)
        with self.assertRaisesRegex(ValueError, "root"):
            with mock.patch.object(Path, "home", return_value=self.source):
                self.profile("snapshot")

    def test_rejects_unknown_controls_dependencies_and_collisions(self):
        bad_profiles = [
            {"unknown": True}, {"plugins": {"mode": "selected", "paths": ["x"]}},
            {"hooks": {"mode": "snapshot", "paths": ["x"]}},
            {"skills": {"mode": "none", "paths": [str(self.source)]}},
            {"skills": {"mode": "selected", "paths": [str(self.source)], "dependencies": ["plugin"]}},
            {"skills": {"mode": "selected", "paths": [str(self.source), str(self.source)]}},
            {"require_absence": "yes"},
        ]
        for raw in bad_profiles:
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                ac.normalize_profiles({"test": raw}, self.root)
        skill = self.source / "SKILL.md"
        original = skill.read_text()
        for field in ("hooks: {}", "allowed-tools: Bash", "context: fork", "dependencies: plugin"):
            skill.write_text(original.replace("description:", field + "\ndescription:"))
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, "unsupported"):
                self.profile()
        skill.write_text(original)
        (self.source / "agents").mkdir()
        (self.source / "agents" / "openai.yaml").write_text("dependencies:\n  tools: []\n")
        with self.assertRaisesRegex(ValueError, "dependency"):
            self.profile()

    def test_rejects_name_mismatch_and_case_path_collisions(self):
        path = self.source / "SKILL.md"
        path.write_text(path.read_text().replace("name: product-guide", "name: other"))
        with self.assertRaisesRegex(ValueError, "match"):
            self.profile()
        path.write_text(path.read_text().replace("name: other", "name: product-guide"))
        (self.source / "Guide.txt").write_text("one")
        if not (self.source / "guide.txt").exists():  # case-sensitive filesystems
            (self.source / "guide.txt").write_text("two")
            with self.assertRaisesRegex(ValueError, "collision"):
                self.profile()

    def test_rejects_symlinks_special_files_hidden_files_and_caches(self):
        outside = self.root / "outside.txt"
        outside.write_text("Synthetic private content.")
        (self.source / "link").symlink_to(outside)
        with self.assertRaisesRegex(ValueError, "symlink"):
            self.profile()
        (self.source / "link").unlink()
        (self.source / "inside-link").symlink_to(self.source / "SKILL.md")
        with self.assertRaisesRegex(ValueError, "symlink"):
            self.profile()
        (self.source / "inside-link").unlink()
        fifo = self.source / "pipe"
        os.mkfifo(fifo)
        with self.assertRaisesRegex(ValueError, "special file"):
            self.profile()
        fifo.unlink()
        for name in (".env", ".git", "__pycache__", "node_modules", "credentials.json", "private.key"):
            path = self.source / name
            path.write_text("Synthetic secret/cache placeholder.")
            with self.subTest(name=name), self.assertRaisesRegex(ValueError, "disallowed"):
                self.profile()
            path.unlink()

    def test_bounds_apply_before_reading_oversize_content(self):
        with mock.patch.object(ac, "_MAX_FILES", 2), self.assertRaisesRegex(ValueError, "too many"):
            self.profile()
        with mock.patch.object(ac, "_MAX_FILE", 10), self.assertRaisesRegex(ValueError, "size limit"):
            self.profile()
        with mock.patch.object(ac, "_MAX_BYTES", 10), self.assertRaisesRegex(ValueError, "size limit"):
            self.profile()

    def test_source_mutation_before_prepare_is_rejected(self):
        profile = self.profile()
        (self.source / "assets/guide.txt").write_text("Changed after resolution.")
        with self.assertRaisesRegex(ValueError, "changed after resolution"):
            ac.prepare(profile, self.ctx(Codex()), Codex())

    def test_source_mutation_during_copy_is_rejected_and_partial_copy_removed(self):
        profile = self.profile()
        ctx = self.ctx(Codex())
        original = ac._write_file

        def change_source(path, data):
            original(path, data)
            if path.name == "SKILL.md":
                (self.source / "assets/guide.txt").write_text("Changed during copy.")

        with mock.patch.object(ac, "_write_file", side_effect=change_source):
            with self.assertRaisesRegex(ValueError, "changed during copy"):
                ac.prepare(profile, ctx, Codex())
        self.assertFalse((ctx["home_dir"] / ".agents/skills/product-guide").exists())
        self.assertNotIn("agent_configuration", ctx)

    def test_selected_launch_configuration_for_every_harness(self):
        for harness in (Codex(), ClaudeCode(), KiroCli()):
            with self.subTest(harness=harness.name):
                ctx, manifest = self.prepared(harness)
                captured = self.capture(harness, ctx)
                env, argv = captured["extra_env"], captured["argv"]
                self.assertEqual(env["HOME"], str(ctx["home_dir"]))
                skill = manifest["skills"]["entries"][0]
                installed = ac._contained_path(skill["location"], ctx)
                self.assertEqual((installed / "SKILL.md").read_bytes(), (self.source / "SKILL.md").read_bytes())
                self.assertEqual((installed / "scripts/check.sh").read_bytes(), (self.source / "scripts/check.sh").read_bytes())
                self.assertEqual(ac._read_tree(installed)["sha256"], skill["sha256"])
                if harness.name == "codex":
                    self.assertEqual(env["CODEX_HOME"], str(ctx["config_dir"]))
                    self.assertIn("features.plugins=false", argv)
                    self.assertIn("features.hooks=false", argv)
                    self.assertIn("skills.bundled.enabled=false", argv)
                    self.assertTrue(any("skills.config=" in a and "product-guide/SKILL.md" in a for a in argv))
                elif harness.name == "claude-code":
                    self.assertNotIn("--safe-mode", argv)
                    self.assertNotIn("--disable-slash-commands", argv)
                    self.assertEqual(argv[argv.index("--setting-sources") + 1], "user")
                    settings = json.loads((ctx["config_dir"] / "settings.json").read_text())
                    self.assertTrue(settings["disableAllHooks"])
                    self.assertTrue(settings["disableBundledSkills"])
                    self.assertEqual(settings["enabledPlugins"], {})
                else:
                    self.assertEqual(argv[0], "kiro-cli-chat")
                    self.assertEqual(env["KIRO_HOME"], str(ctx["config_dir"]))
                    self.assertEqual(argv[argv.index("--agent-engine") + 1], "v2")
                    agent = json.loads((ctx["config_dir"] / "agents/ajx-profile.json").read_text())
                    self.assertEqual(agent["resources"], ["skill://" + str(installed) + "/SKILL.md"])
                    self.assertEqual(agent["hooks"], {})
                    self.assertFalse(agent["includeMcpJson"])

    def test_none_uses_real_controls_and_installs_no_optional_skills(self):
        for harness in (Codex(), ClaudeCode(), KiroCli()):
            with self.subTest(harness=harness.name):
                ctx, manifest = self.prepared(harness, "none")
                captured = self.capture(harness, ctx)
                self.assertEqual(manifest["skills"]["entries"], [])
                self.assertEqual(captured.get("stdin_data", ctx["prompt_text"]), ctx["prompt_text"])
                if harness.name == "codex":
                    self.assertIn("skills.config=[]", captured["argv"])
                    self.assertIn("skills.bundled.enabled=false", captured["argv"])
                elif harness.name == "claude-code":
                    self.assertIn("--disable-slash-commands", captured["argv"])
                else:
                    conf = json.loads((ctx["config_dir"] / "agents/ajx-profile.json").read_text())
                    self.assertEqual(conf["resources"], [])

    def test_manifest_is_safe_and_unobserved_usage_stays_unknown(self):
        for harness in (Codex(), ClaudeCode(), KiroCli()):
            ctx, manifest = self.prepared(harness)
            encoded = json.dumps(manifest)
            self.assertNotIn(str(self.source), encoded)
            self.assertNotIn(str(self.root), encoded)
            self.assertNotIn('"_source"', encoded)
            self.assertNotIn('"_identity"', encoded)
            self.assertEqual(manifest["content_absence"], "unsupported")
            for entry in manifest["skills"]["entries"]:
                self.assertTrue(entry["installed"])
                self.assertEqual(entry["discoverable"], "unknown")
                self.assertTrue(entry["discovery_configured"])
                self.assertTrue(entry["enabled"])
                self.assertEqual(entry["loaded"], "unknown")
                self.assertEqual(entry["invoked"], "unknown")
            self.assertEqual(manifest["hooks"]["invoked"], "unknown")
            self.assertEqual(manifest["plugins"]["invoked"], "unknown")
            self.assertTrue(manifest["uncontrolled_sources"])

    def test_narration_retains_persisted_profile_without_source(self):
        profile = self.profile()
        for harness in (Codex(), ClaudeCode(), KiroCli()):
            ctx = self.ctx(harness)
            manifest = ac.prepare(profile, ctx, harness)
            ctx["state"]["agent_configuration"] = json.loads(json.dumps(manifest))
            ctx.pop("agent_configuration")
            worker_args = ac.launch_args(ctx, harness)
            captured = self.capture(harness, ctx, "narrate")
            self.assertEqual(captured["extra_env"], ac.launch_env(ctx, harness))
            for value in worker_args:
                self.assertIn(value, captured["argv"])
            self.assertIn("synthetic-session", captured["argv"])
        # Normalized source locations are irrelevant after a persisted prepare.
        shutil.rmtree(self.source)
        self.capture(harness, ctx, "narrate")

    def test_reporter_rejects_worker_profile_and_clean_reporter_never_receives_it(self):
        for harness in (Codex(), ClaudeCode(), KiroCli()):
            with self.subTest(harness=harness.name):
                worker, _ = self.prepared(harness)
                worker["stage_name"] = "extract"
                with mock.patch.object(harness, "run") as run:
                    with self.assertRaisesRegex(ValueError, "separate fixed context"):
                        harness.report(worker, "Synthetic evidence")
                    run.assert_not_called()
                reporter = self.ctx(harness)
                reporter["cell"].pop("agent_configuration")
                reporter["stage_name"] = "extract"
                with mock.patch.object(harness, "run", return_value={}) as run:
                    harness.report(reporter, "Synthetic evidence")
                args, kwargs = run.call_args
                self.assertNotIn("agent_configuration", args[0])
                self.assertNotIn("--agent-engine", args[1])
                self.assertNotIn("--setting-sources", args[1])
                self.assertNotIn("features.plugins=false", args[1])
                self.assertNotIn(str(worker["config_dir"]), json.dumps(args[1]))
                self.assertNotIn(str(worker["home_dir"]), json.dumps(kwargs.get("extra_env", {}), default=str))

    def test_cell_auth_env_and_permission_argument_conflicts(self):
        conflicts = {
            "codex": [["--profile", "ambient"], ["-c", "features.plugins=true"], ["--enable=plugins"],
                      ["--cd", "/"], ["-cskills.config=[]"], ["--config=model=x\nfeatures.plugins=true"]],
            "claude-code": [["--settings", "{}"], ["--plugin-dir", "/"], ["--add-dir=/"],
                            ["--safe-mode"], ["--disable-slash-commands"]],
            "kiro-cli": [["--agent", "ambient"], ["--agent-engine=v3"], ["--cloud"], ["--resume-id", "other"]],
        }
        for harness in (Codex(), ClaudeCode(), KiroCli()):
            for args in conflicts[harness.name]:
                ctx = self.ctx(harness)
                ctx["cell"]["args"] = args
                with self.subTest(harness=harness.name, args=args), self.assertRaises(ValueError):
                    ac.validate_for_cell(self.profile(), ctx["cell"], harness, ctx["auth"])
            ctx = self.ctx(harness)
            for key in ("HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "KIRO_HOME", "XDG_CONFIG_HOME", "NODE_OPTIONS"):
                ctx["cell"]["env"] = {key: "synthetic-override"}
                with self.subTest(key=key), self.assertRaisesRegex(ValueError, "conflicts"):
                    ac.validate_for_cell(self.profile(), ctx["cell"], harness, ctx["auth"])
        ctx = self.ctx(Codex(), EnvAuth({"args": ["-c", "notify=['synthetic']"]}))
        with self.assertRaisesRegex(ValueError, "auth.args"):
            ac.validate_for_cell(self.profile(), ctx["cell"], Codex(), ctx["auth"])
        ctx = self.ctx(Codex(), EnvAuth({"unset": ["HOME"]}))
        with self.assertRaisesRegex(ValueError, "auth.unset"):
            ac.validate_for_cell(self.profile(), ctx["cell"], Codex(), ctx["auth"])
        ctx = self.ctx(Codex())
        ctx["cell"]["permission_args"] = ["--enable", "plugins"]
        with self.assertRaisesRegex(ValueError, "permission_args"):
            ac.validate_for_cell(self.profile(), ctx["cell"], Codex(), ctx["auth"])

    def test_supported_model_and_provider_arguments(self):
        auth = CodexProvider({"provider": "synthetic", "config": [
            'model_providers.synthetic.base_url="https://example.invalid"',
            'model_providers.synthetic.env_key="SYNTHETIC_MODEL_KEY"']})
        ctx = self.ctx(Codex(), auth)
        ctx["cell"]["args"] = ["--model", "synthetic-model", "-c", 'model_reasoning_effort="low"']
        ac.validate_for_cell(self.profile(), ctx["cell"], Codex(), auth)

    def test_incompatible_auth_and_strict_absence_rejected(self):
        for harness, auth in ((ClaudeCode(), ClaudeSubscription()), (KiroCli(), KiroLogin())):
            ctx = self.ctx(harness, auth)
            with self.subTest(auth=auth.name), self.assertRaisesRegex(ValueError, "caller configuration"):
                ac.validate_for_cell(self.profile(), ctx["cell"], harness, auth)
        ctx = self.ctx(KiroCli(), EnvAuth())
        with self.assertRaisesRegex(ValueError, "KIRO_API_KEY"):
            ac.validate_for_cell(self.profile(), ctx["cell"], KiroCli(), ctx["auth"])
        ctx = self.ctx(Codex())
        with self.assertRaisesRegex(ValueError, "cannot hide"):
            ac.validate_for_cell(self.profile(require_absence=True), ctx["cell"], Codex(), ctx["auth"])
        with self.assertRaisesRegex(ValueError, "EnvironmentRunner"):
            ac.validate_for_cell(self.profile(), ctx["cell"], Codex(), ctx["auth"], {"backend": "remote"})
        ac.validate_for_cell(self.profile(), ctx["cell"], Codex(), ctx["auth"], {"backend": "container"})

    def test_codex_copied_auth_is_not_read_or_changed(self):
        ctx = self.ctx(Codex(), CodexChatGPT())
        auth = ctx["config_dir"] / "auth.json"
        auth.write_text("synthetic-login-fixture")
        before = auth.read_bytes()
        with mock.patch.object(Path, "read_bytes", side_effect=AssertionError("no credential reads")):
            manifest = ac.prepare(self.profile("none"), ctx, Codex())
        self.assertEqual(auth.read_bytes(), before)
        self.assertNotIn("synthetic-login", json.dumps(manifest))

    def test_environment_only_codex_auth_copy_gets_container_owner(self):
        source = self.root / "synthetic-login"
        source.mkdir()
        original = source / "auth.json"
        original.write_text('{"synthetic": "not-a-real-credential"}\n')
        original.chmod(0o600)
        auth = CodexChatGPT({"codex_home": str(source)})
        ctx = self.ctx(Codex(), auth)
        ctx["cell"].pop("agent_configuration")
        ctx.update(runner=FakeEnvironmentRunner(), environment_profile={"backend": "container"})
        self.assertIsNone(ac.configuration(ctx))
        owner = ctx["config_dir"].stat()
        with mock.patch("ajx.builtin.codex.os.geteuid", return_value=0):
            with mock.patch("ajx.builtin.codex.os.chown") as chown:
                auth.prepare(ctx)
        target = ctx["config_dir"] / "auth.json"
        chown.assert_called_once_with(target, owner.st_uid, owner.st_gid)
        self.assertEqual(target.read_bytes(), original.read_bytes())
        self.assertEqual(target.stat().st_mode & 0o777, 0o600)
        auth.cleanup(ctx)
        self.assertFalse(target.exists())
        self.assertTrue(original.exists())

    def test_managed_config_detected_without_reading_values(self):
        policy = self.root / "managed-settings.json"
        policy.write_text("Synthetic policy secret.")
        ctx = self.ctx(ClaudeCode())
        with mock.patch.dict(ac._SYSTEM_PATHS, {"claude-code": (str(policy),)}):
            with self.assertRaisesRegex(ValueError, "admin configuration"):
                ac.prepare(self.profile(), ctx, ClaudeCode())
        self.assertEqual(policy.read_text(), "Synthetic policy secret.")

    def test_fresh_home_and_configuration_scope_required(self):
        ctx = self.ctx(Codex())
        (ctx["home_dir"] / "ambient").write_text("Keep me.")
        with self.assertRaisesRegex(ValueError, "fresh empty"):
            ac.prepare(self.profile(), ctx, Codex())
        self.assertEqual((ctx["home_dir"] / "ambient").read_text(), "Keep me.")
        ctx = self.ctx(Codex())
        (ctx["config_dir"] / "config.toml").write_text("synthetic = true")
        with self.assertRaisesRegex(ValueError, "fresh config"):
            ac.prepare(self.profile(), ctx, Codex())
        ctx = self.ctx(Codex())
        ctx["home_dir"] = ctx["config_dir"]
        with self.assertRaisesRegex(ValueError, "separate directories"):
            ac.prepare(self.profile(), ctx, Codex())

    def test_bootstrap_directories_cannot_contain_unexpected_files(self):
        ctx = self.ctx(Codex())
        ctx["runner"] = FakeEnvironmentRunner()
        ctx["environment_profile"] = {"backend": "container"}
        path = ctx["home_dir"] / ".local/bin/unexpected"
        path.parent.mkdir(parents=True)
        path.write_text("Do not change this file.")
        with self.assertRaisesRegex(ValueError, "bootstrap"):
            ac.prepare(self.profile(), ctx, Codex())
        self.assertEqual(path.read_text(), "Do not change this file.")

    def test_root_coordinator_assigns_copied_files_to_container_directory_owner(self):
        ctx = self.ctx(ClaudeCode())
        ctx["runner"] = FakeEnvironmentRunner()
        ctx["environment_profile"] = {"backend": "container"}
        with mock.patch.object(ac.os, "geteuid", return_value=0), mock.patch.object(ac.os, "chown") as chown:
            ac.prepare(self.profile(), ctx, ClaudeCode())
        changed = [Path(call.args[0]) for call in chown.call_args_list]
        self.assertTrue(any(path.name == "SKILL.md" for path in changed))
        self.assertTrue(any(path.name == "settings.json" for path in changed))
        self.assertTrue(all(path.is_relative_to(ctx["home_dir"]) or path.is_relative_to(ctx["config_dir"])
                            for path in changed))

    def test_setup_cannot_add_workspace_or_home_discovery_sources(self):
        for harness, relative in ((Codex(), ".agents/skills/rogue"), (ClaudeCode(), ".claude/skills/rogue"),
                                  (KiroCli(), ".kiro/agents")):
            ctx, _ = self.prepared(harness)
            added = ctx["workspace"] / relative
            added.mkdir(parents=True)
            with self.subTest(harness=harness.name), self.assertRaisesRegex(ValueError, "workspace or ancestor"):
                self.capture(harness, ctx)
        for harness in (Codex(), ClaudeCode(), KiroCli()):
            ctx, manifest = self.prepared(harness, "none")
            base = ctx["home_dir"] / ".agents/skills" if harness.name == "codex" else ctx["config_dir"] / "skills"
            (base / "rogue").mkdir(parents=True)
            with self.subTest(harness=harness.name), self.assertRaisesRegex(ValueError, "additional"):
                self.capture(harness, ctx)

    def test_mutated_snapshot_or_settings_blocks_narration(self):
        for harness in (Codex(), ClaudeCode(), KiroCli()):
            ctx, manifest = self.prepared(harness)
            path = ac._contained_path(manifest["skills"]["entries"][0]["location"], ctx) / "SKILL.md"
            path.write_text("Changed by synthetic worker.")
            with self.assertRaisesRegex(ValueError, "changed"):
                self.capture(harness, ctx, "narrate")
        ctx, _ = self.prepared(ClaudeCode())
        (ctx["config_dir"] / "settings.json").write_text('{"disableAllHooks":false}')
        with self.assertRaisesRegex(ValueError, "configuration changed"):
            self.capture(ClaudeCode(), ctx)

    def test_installed_parent_symlink_replacement_rejected(self):
        ctx, _ = self.prepared(ClaudeCode())
        skill_root = ctx["config_dir"] / "skills"
        saved = ctx["config_dir"] / "saved-skills"
        skill_root.rename(saved)
        skill_root.symlink_to(saved, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "symlink"):
            self.capture(ClaudeCode(), ctx)

    def test_inherited_controls_are_removed_only_for_profiles(self):
        ctx, _ = self.prepared(ClaudeCode(), "none")
        injected = {"CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD": "1", "NODE_OPTIONS": "--synthetic",
                    "CODEX_HOME": "synthetic", "KIRO_HOME": "synthetic", "CLAUDE_CODE_USE_BEDROCK": "1"}
        with mock.patch.dict(os.environ, injected):
            captured = self.capture(ClaudeCode(), ctx)
        dropped = captured["ctx"]["unset_env"]
        for name in injected:
            if name != "CLAUDE_CODE_USE_BEDROCK":
                self.assertIn(name, dropped)
        self.assertNotIn("CLAUDE_CODE_USE_BEDROCK", dropped)
        self.assertEqual(ctx["unset_env"], [])

    def test_plain_launches_preserve_legacy_configuration(self):
        for harness in (Codex(), ClaudeCode(), KiroCli()):
            ctx = self.ctx(harness)
            ctx["cell"].pop("agent_configuration")
            ctx["cell"]["args"] = ["--synthetic-legacy-option"]
            captured = self.capture(harness, ctx)
            self.assertIn("--synthetic-legacy-option", captured["argv"])
            self.assertNotIn("HOME", captured.get("extra_env") or {})
            self.assertIs(captured["ctx"], ctx)
            if harness.name == "claude-code":
                self.assertIn("--safe-mode", captured["argv"])
            if harness.name == "kiro-cli":
                self.assertEqual(captured["argv"][0], "kiro-cli")

    def test_runtime_env_conflicts_are_rejected(self):
        ctx = self.ctx(Codex())
        ctx["spec"]["env"] = {"CODEX_HOME": "synthetic"}
        with self.assertRaisesRegex(ValueError, "trial.env"):
            ac.prepare(self.profile(), ctx, Codex())
        ctx, _ = self.prepared(Codex())
        ctx["env"]["CODEX_HOME"] = "synthetic"
        with self.assertRaisesRegex(ValueError, "worker env"):
            self.capture(Codex(), ctx)

    def test_local_probes_use_only_help_version_and_config_parse_with_scratch_env(self):
        self.probe.stop()
        commands = []
        ctx = self.ctx(Codex())

        def fake(argv, **kwargs):
            commands.append((argv, kwargs))
            if argv[1:] == ["--version"]:
                text = "codex-cli 0.160.1"
            elif argv[1:] == ["exec", "--help"]:
                text = "--strict-config --config --disable"
            else:
                self.assertEqual(argv[-2:], ["features", "list"])
                text = "\n".join(k + " stable false" for k in ("plugins", "hooks", "apps", "skill_mcp_dependency_install"))
            return subprocess.CompletedProcess(argv, 0, text, "")

        with mock.patch.object(ac.shutil, "which", return_value="/synthetic/bin/codex"):
            with mock.patch.object(ac.subprocess, "run", side_effect=fake):
                result = ac._probe("codex", ctx)
        self.assertFalse(result["provider_called"])
        self.assertEqual(len(commands), 3)
        for argv, kwargs in commands:
            self.assertTrue(str(kwargs["cwd"]).startswith(str(ctx["cache_dir"])))
            self.assertNotIn("OPENAI_API_KEY", kwargs["env"])
            self.assertNotEqual(kwargs["env"]["HOME"], str(ctx["home_dir"]))
            self.assertNotEqual(kwargs["env"]["CODEX_HOME"], str(ctx["config_dir"]))
            self.assertFalse(any("login" == a for a in argv))
        self.probe.start()

    def test_none_and_selected_inside_container_backend(self):
        self.probe.stop()
        for harness in (Codex(), ClaudeCode(), KiroCli()):
            for mode in ("none", "selected"):
                with self.subTest(harness=harness.name, mode=mode):
                    ctx = self.ctx(harness)
                    backend = FakeEnvironmentRunner()
                    ctx["runner"] = backend
                    ctx["environment_profile"] = {"backend": "container"}
                    ctx["env"] = {**backend.environment_env(ctx), "SYNTHETIC_PROVIDER_KEY": "do-not-forward"}
                    for subdir in (".local/bin", ".local/share", ".local/state"):
                        (ctx["home_dir"] / subdir).mkdir(parents=True, exist_ok=True)
                    with mock.patch.object(ac.shutil, "which", side_effect=AssertionError("container must resolve internally")):
                        manifest = ac.prepare(self.profile(mode), ctx, harness)
                    ctx["state"]["agent_configuration"] = manifest
                    capture = self.capture(harness, ctx)
                    binary = {"codex": "codex", "claude-code": "claude", "kiro-cli": "kiro-cli-chat"}[harness.name]
                    self.assertEqual(capture["argv"][0], "/container/bin/" + binary)
                    self.assertEqual(manifest["backend"], "container")
                    self.assertEqual(capture["extra_env"]["XDG_CONFIG_HOME"], str(ctx["config_dir"]))
                    self.assertNotIn("BASH_ENV", capture["extra_env"])
                    self.assertNotIn("ENV", capture["extra_env"])
                    self.assertEqual(capture["ctx"]["unset_env"], [])
                    self.assertFalse(any("Local isolation" in s for s in manifest["limitations"]))
                    self.assertTrue(any(command == "command -v " + binary for command, _, _ in backend.calls))
                    for command, probe, env in backend.calls:
                        self.assertIsNone(probe["auth"])
                        self.assertNotIn("SYNTHETIC_PROVIDER_KEY", probe["env"])
                        self.assertNotIn("SYNTHETIC_PROVIDER_KEY", env)
                        self.assertEqual(probe["env"]["HOME"], str(ctx["home_dir"]))
                    narration = self.capture(harness, ctx, "narrate")
                    self.assertEqual(narration["argv"][0], "/container/bin/" + binary)
        self.probe.start()

    def test_container_admin_scope_failure_is_detected_in_container(self):
        self.probe.stop()
        ctx = self.ctx(Codex())
        backend = FakeEnvironmentRunner()
        backend.scope_violation = "admin-configuration"
        ctx.update(runner=backend, environment_profile={"backend": "container"})
        with mock.patch.dict(ac._SYSTEM_PATHS, {"codex": ("/synthetic/admin-policy",)}):
            with self.assertRaisesRegex(ValueError, "admin configuration"):
                ac.prepare(self.profile(), ctx, Codex())
        self.assertNotIn("agent_configuration", ctx)
        self.probe.start()

    def test_local_backend_discovers_from_caller_path_and_persists_binary(self):
        self.probe.stop()
        ctx = self.ctx(Codex())
        backend = FakeEnvironmentRunner("local")
        ctx.update(runner=backend, environment_profile={"backend": "local"}, env=backend.environment_env(ctx))
        with mock.patch.object(ac.shutil, "which", return_value="/trusted/install/codex") as which:
            manifest = ac.prepare(self.profile(), ctx, Codex())
        self.assertEqual(which.call_args.kwargs["path"], os.environ.get("PATH", ""))
        self.assertNotEqual(which.call_args.kwargs["path"], ctx["env"]["PATH"])
        self.assertEqual(manifest["launch"]["binary"], "/trusted/install/codex")
        self.assertEqual(self.capture(Codex(), ctx)["argv"][0], "/trusted/install/codex")
        self.probe.start()

    def test_container_profiles_through_real_harness_run_and_environment_child_env(self):
        self.probe.stop()
        for harness in (Codex(), ClaudeCode(), KiroCli()):
            for mode in ("none", "selected"):
                with self.subTest(harness=harness.name, mode=mode):
                    ctx = self.ctx(harness)
                    backend = FakeEnvironmentRunner()
                    ctx.update(runner=backend, environment_profile={"backend": "container"},
                               env=backend.environment_env(ctx))
                    manifest = ac.prepare(self.profile(mode), ctx, harness)
                    ctx["state"]["agent_configuration"] = manifest
                    ctx.pop("agent_configuration")
                    for stage in ("execute", "narrate"):
                        with mock.patch("ajx.base.run_streaming", return_value={"exit_code": 0, "timed_out": False}) as run:
                            if stage == "execute":
                                harness.execute(ctx)
                            else:
                                harness.narrate(ctx, "Describe the recorded attempt.")
                        argv = run.call_args.args[0]
                        env = run.call_args.kwargs["env"]
                        self.assertEqual(argv[0], manifest["launch"]["binary"])
                        self.assertEqual(env["HOME"], str(ctx["home_dir"]))
                        self.assertEqual(env["XDG_CONFIG_HOME"], str(ctx["config_dir"]))
                        self.assertEqual(env["XDG_CACHE_HOME"], str(ctx["cache_dir"]))
                        self.assertNotIn("BASH_ENV", env)
                        self.assertNotIn("ENV", env)
                        self.assertEqual(env[{"codex": "CODEX_HOME", "claude-code": "CLAUDE_CONFIG_DIR",
                                              "kiro-cli": "KIRO_HOME"}[harness.name]], str(ctx["config_dir"]))
        self.probe.start()

    def test_unsupported_cli_capability_is_preflight_failure(self):
        self.probe.stop()
        ctx = self.ctx(ClaudeCode())
        with mock.patch.object(ac.shutil, "which", return_value="/synthetic/bin/claude"):
            with mock.patch.object(ac.subprocess, "run",
                                   return_value=subprocess.CompletedProcess([], 0, "2.0.0", "")):
                with self.assertRaisesRegex(ValueError, "require version"):
                    ac.prepare(self.profile(), ctx, ClaudeCode())
        self.assertNotIn("agent_configuration", ctx)
        self.probe.start()


_DOCKER_PROFILE_CLI = r'''#!/usr/bin/env python3
"""Synthetic CLI fixture: checks backend wiring; never loads a vendor or model."""
import hashlib
import json
import os
import sys
import tomllib
from pathlib import Path

binary, args = Path(sys.argv[0]).name, sys.argv[1:]
versions = {"codex": "0.160.1", "claude": "2.1.293", "kiro-cli-chat": "2.28.0"}
if args == ["--version"]:
    print(versions[binary])
    raise SystemExit(0)
if "--help" in args:
    print("--strict-config --config --disable --setting-sources --settings")
    print("--disable-slash-commands --strict-mcp-config --agent --agent-engine --no-interactive")
    raise SystemExit(0)

overrides = {}
for index, arg in enumerate(args):
    if arg == "-c":
        key, value = args[index + 1].split("=", 1)
        overrides[key] = tomllib.loads("value = " + value)["value"]
features = ("plugins", "hooks", "apps", "skill_mcp_dependency_install")
if args[-2:] == ["features", "list"]:
    for feature in features:
        print(feature, "stable", str(overrides.get("features." + feature, True)).lower())
    raise SystemExit(0)

def option(flag):
    return args[args.index(flag) + 1]

case = json.loads(Path("synthetic-case.json").read_text())
assert binary == case["binary"], binary
assert os.geteuid() != 0, "container worker must be non-root"
assert Path("/.dockerenv").exists(), "this fixture must execute inside Docker"
for key, expected in case["env"].items():
    assert os.environ.get(key) == expected, (key, os.environ.get(key), expected)
for key in ("BASH_ENV", "ENV", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "KIRO_API_KEY",
            "CLAUDE_CODE_OAUTH_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"):
    assert key not in os.environ, "unexpected startup variable or credential: " + key

home, config = Path(case["env"]["HOME"]), Path(case["env"]["XDG_CONFIG_HOME"])
root = home / ".agents/skills" if binary == "codex" else config / "skills"
selected = case["mode"] == "selected"
names = ["product-guide"] if selected else []
assert sorted(p.name for p in root.iterdir()) == names if root.exists() else not names
skill_files = {}
for name in names:
    observed = {str(p.relative_to(root / name)): hashlib.sha256(p.read_bytes()).hexdigest()
                for p in (root / name).rglob("*") if p.is_file()}
    assert observed == case["files"], observed
    assert os.access(root / name / "scripts/check.sh", os.X_OK)
    skill_files[name] = observed

if binary == "codex":
    assert "--strict-config" in args
    assert all(overrides.get("features." + feature) is False for feature in features)
    assert overrides["skills.bundled.enabled"] is False
    expected = [{"path": str(root / "product-guide/SKILL.md"), "enabled": True}] if selected else []
    assert overrides["skills.config"] == expected, overrides
    if "resume" in args:
        assert args.index("--sandbox") < args.index("resume")
        assert option("--sandbox") == "read-only"
elif binary == "claude":
    assert option("--setting-sources") == "user" and "--strict-mcp-config" in args
    assert "--safe-mode" not in args
    assert ("--disable-slash-commands" in args) == (not selected)
    settings = json.loads(Path(option("--settings")).read_text())
    assert settings["disableAllHooks"] and settings["disableBundledSkills"]
    assert settings["enabledPlugins"] == {}
else:
    assert option("--agent-engine") == "v2" and "--no-interactive" in args
    agent = json.loads((config / "agents" / (option("--agent") + ".json")).read_text())
    expected = ["skill://" + str(root / "product-guide/SKILL.md")] if selected else []
    assert agent["resources"] == expected, agent
    assert agent["hooks"] == {} and agent["mcpServers"] == {} and not agent["includeMcpJson"]

stage = "narrate" if any(arg in args for arg in ("resume", "--resume", "--resume-id")) else "execute"
prompt = args[-1] if binary == "kiro-cli-chat" else sys.stdin.read()
assert prompt == case["prompts"][stage], prompt
observed = {"stage": stage, "binary": binary, "mode": case["mode"], "skills": skill_files,
            "env": {key: os.environ[key] for key in case["env"]}, "argv": args, "uid": os.geteuid()}
Path("observed-" + stage + ".json").write_text(json.dumps(observed))
sid = case["session_id"]
if binary == "codex":
    events = [{"type": "thread.started", "thread_id": sid},
              {"type": "item.completed", "item": {"id": "synthetic", "type": "agent_message",
                                                "text": "Synthetic profile wiring verified."}},
              {"type": "turn.completed", "usage": {"input_tokens": 0, "output_tokens": 0}}]
elif binary == "claude":
    events = [{"type": "system", "subtype": "init", "session_id": sid},
              {"type": "result", "subtype": "success", "session_id": sid,
               "result": "Synthetic profile wiring verified.", "usage": {"output_tokens": 0}}]
else:
    events = [{"type": "metadata", "data": {"sessionId": sid}},
              {"type": "runFinished", "data": {"sessionId": sid, "stopReason": "completed",
                                             "finalText": "Synthetic profile wiring verified."}}]
for event in events:
    print(json.dumps(event), flush=True)
'''


class _WorkspaceCLIRunner(EnvironmentRunner):
    """Only add trusted workspace fixture executables to the real backend's PATH."""

    def environment_env(self, ctx):
        env = super().environment_env(ctx)
        return {**env, "PATH": str(Path(ctx["workspace"]) / "synthetic-bin") + ":" + env["PATH"]}


@unittest.skipUnless(os.environ.get("AJX_TEST_CONTAINER_IMAGE"),
                     "set AJX_TEST_CONTAINER_IMAGE to opt into real Docker profile wiring")
class RealDockerProfileIntegrationTest(unittest.TestCase):
    """Real prepare/probe/launch/resume/release with synthetic CLIs, no provider credentials."""

    def test_none_and_selected_for_all_harnesses_through_real_container(self):
        with tempfile.TemporaryDirectory(prefix="ajx-profile-docker-") as temp:
            root = Path(temp).resolve()
            base = root / "trial"
            source = base / "skills/product-guide"
            source.mkdir(parents=True)
            content = {
                "SKILL.md": "---\nname: product-guide\ndescription: Synthetic wiring fixture.\n---\n"
                            "Read [the guide](assets/guide.txt) and the bundled check script.\n",
                "assets/guide.txt": "Fictional widget reference.\n",
                "scripts/check.sh": "#!/bin/sh\nprintf 'synthetic asset\\n'\n",
            }
            for relative, text in content.items():
                path = source / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(text, encoding="utf-8")
                path.chmod(0o755 if relative.endswith(".sh") else 0o644)
            hashes = {relative: hashlib.sha256(text.encode()).hexdigest() for relative, text in content.items()}
            profiles = ac.normalize_profiles({
                "none": {"skills": {"mode": "none"}},
                "selected": {"skills": {"mode": "selected", "paths": ["skills/product-guide"]}},
            }, base)
            environment = envmod.normalize_profiles({"integration": {
                "backend": "container", "image": os.environ["AJX_TEST_CONTAINER_IMAGE"], "network": "none",
                "limits": {"lifetime_seconds": 180},
                "tools": [{"name": "python", "argv": ["python3", "--version"], "required": True}],
            }}, base)["integration"]
            for harness in (Codex(), ClaudeCode(), KiroCli()):
                for mode in ("none", "selected"):
                    with self.subTest(harness=harness.name, mode=mode):
                        attempt = root / (harness.name + "-" + mode)
                        paths = {key: attempt / key for key in
                                 ("workspace", "home_dir", "config_dir", "cache_dir", "run_dir")}
                        for path in paths.values():
                            path.mkdir(parents=True, mode=0o700)
                        binary = {"codex": "codex", "claude-code": "claude", "kiro-cli": "kiro-cli-chat"}[harness.name]
                        executable = paths["workspace"] / "synthetic-bin" / binary
                        executable.parent.mkdir()
                        executable.write_text(_DOCKER_PROFILE_CLI, encoding="utf-8")
                        executable.chmod(0o755)
                        runner = _WorkspaceCLIRunner(environment)
                        sid = "00000000-0000-4000-8000-000000000001"
                        # Declaration satisfies the Kiro adapter's compatibility check.
                        # No value is supplied: the synthetic CLI verifies there is no key.
                        auth = EnvAuth({"required_env": ["KIRO_API_KEY"]}) if harness.name == "kiro-cli" else EnvAuth()
                        ctx = {**paths, "runner": runner, "environment_profile": environment, "auth": auth,
                               "state": {"execute": {"session_id": sid}}, "env": {}, "unset_env": [],
                               "cell": {"id": harness.name + "-" + mode, "harness": harness.name, "config": "clean",
                                        "agent_configuration": mode, "args": [], "env": {}},
                               "spec": {"trial": {}, "env": {}}, "timeout": 30, "narrate_timeout": 30,
                               "prompt_text": "Verify synthetic profile wiring.", "run_token": "synthetic"}
                        ctx["state"]["environment"] = json.loads(json.dumps(runner.plan(ctx)))
                        try:
                            report = runner.prepare(ctx)
                            self.assertEqual(report["backend"], "container")
                            ctx["env"] = runner.environment_env(ctx)
                            manifest = ac.prepare(profiles[mode], ctx, harness)
                            ctx["state"]["agent_configuration"] = json.loads(json.dumps(manifest))
                            ctx.pop("agent_configuration")  # Use the persisted-state path on both launches.
                            (paths["run_dir"] / "state.json").write_text(json.dumps(ctx["state"]))
                            (paths["run_dir"] / "agent-configuration.json").write_text(json.dumps(manifest))
                            self.assertEqual(manifest["launch"]["binary"], str(executable))
                            expected_env = {
                                "HOME": str(paths["home_dir"]), "XDG_CONFIG_HOME": str(paths["config_dir"]),
                                "XDG_CACHE_HOME": str(paths["cache_dir"]),
                                "XDG_DATA_HOME": str(paths["home_dir"] / ".local/share"),
                                "XDG_STATE_HOME": str(paths["home_dir"] / ".local/state"),
                                "ZDOTDIR": str(paths["home_dir"]),
                                {"codex": "CODEX_HOME", "claude-code": "CLAUDE_CONFIG_DIR",
                                 "kiro-cli": "KIRO_HOME"}[harness.name]: str(paths["config_dir"]),
                            }
                            narrative = "Summarize synthetic wiring."
                            case = {"binary": binary, "mode": mode, "files": hashes, "env": expected_env,
                                    "session_id": sid, "prompts": {"execute": ctx["prompt_text"], "narrate": narrative}}
                            case_path = paths["workspace"] / "synthetic-case.json"
                            case_path.write_text(json.dumps(case))
                            case_path.chmod(0o644)  # Root CI coordinators must keep this fixture worker-readable.
                            executed = harness.execute(ctx)
                            self.assertEqual(executed["exit_code"], 0, (paths["run_dir"] / "execute.stderr.txt").read_text())
                            self.assertFalse(executed["timed_out"])
                            self.assertEqual(executed["session_id"], sid)
                            ctx["state"]["execute"] = executed
                            narrated = harness.narrate(ctx, narrative)
                            self.assertIsNotNone(narrated)
                            self.assertEqual(narrated["exit_code"], 0, (paths["run_dir"] / "narrate.stderr.txt").read_text())
                            self.assertFalse(narrated["timed_out"])
                            for stage in ("execute", "narrate"):
                                observed = json.loads((paths["workspace"] / ("observed-" + stage + ".json")).read_text())
                                self.assertEqual(observed["skills"], {"product-guide": hashes} if mode == "selected" else {})
                                self.assertEqual(observed["env"], expected_env)
                                self.assertEqual(observed["stage"], stage)
                                self.assertNotEqual(observed["uid"], 0)
                                records = [json.loads(line) for line in
                                           (paths["run_dir"] / (stage + ".raw.jsonl")).read_text().splitlines()]
                                self.assertTrue(any("type" in json.loads(record["line"]) for record in records))
                            for entry in manifest["skills"]["entries"]:
                                self.assertEqual(entry["loaded"], "unknown")
                                self.assertEqual(entry["invoked"], "unknown")
                        finally:
                            cleanup = runner.release(ctx)
                            self.assertTrue(cleanup["confirmed"], cleanup)
            self.assertEqual({name: (source / name).read_text() for name in content}, content)


if __name__ == "__main__":
    unittest.main()
