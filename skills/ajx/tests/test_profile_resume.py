"""Resume real saved profiles with the existing offline lifecycle harness."""

import json
import os
import shutil
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "lib"))

from ajx import agent_configuration as ac, plugins, runner, spec as specmod  # noqa: E402
from ajx.builtin.codex import Codex  # noqa: E402
from test_environment_lifecycle import EnvironmentProbe, no_model_extract  # noqa: E402


class ProfileResumeTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="ajx-profile-resume-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name).resolve()
        self.source = self.root / "skills" / "product-guide"
        self.source.mkdir(parents=True)
        (self.source / "SKILL.md").write_text(
            "---\nname: product-guide\ndescription: Use the fictional widget tool.\n---\n"
            "Read guide.txt for the fictional procedure.\n", encoding="utf-8")
        (self.source / "guide.txt").write_text("Original fictional instructions.\n", encoding="utf-8")
        self.login = self.root / "login"
        self.login.mkdir()
        self.credential = self.login / "auth.json"
        self.credential.write_text('{"synthetic": "not-a-real-credential"}\n', encoding="utf-8")
        self.credential.chmod(0o600)
        (self.root / "task.md").write_text("Create the synthetic result file.", encoding="utf-8")
        self.trial = self.root / "trial.toml"
        self.trial.write_text(f"""
[trial]
id = "profile-resume"
product = "synthetic"
workspace_root = {json.dumps(str(self.root / "workspaces"))}
output_dir = "results"
timeout_seconds = 10
agent_configuration = "guided"

[task]
prompt_file = "task.md"

[agent_configurations.guided]
skills = {{ mode = "selected", paths = ["skills/product-guide"] }}

[auth.test]
type = "codex-chatgpt"
codex_home = {json.dumps(str(self.login))}

[reporter]
harness = "codex"
auth = "test"

[[setup]]
run = 'printf ready > "$HOME/installed-tool"'

[[teardown]]
run = 'touch "$HOME/teardown-ran"'

[[cells]]
id = "local"
harness = "codex"
auth = "test"
""", encoding="utf-8")
        patches = [
            mock.patch.object(plugins, "plugin_dirs", return_value=[]),
            mock.patch.object(ac, "_SYSTEM_PATHS", {name: () for name in ac._HARNESSES}),
            mock.patch.object(ac, "_probe", return_value={
                "binary": sys.executable, "version": "synthetic", "backend": "local",
                "provider_called": False, "live_extension_activation_verified": False}),
            # Keep the supported profile adapter, using only the existing fake
            # harness's local Python processes for worker/narrator behavior.
            mock.patch.object(Codex, "execute", EnvironmentProbe.execute),
            mock.patch.object(Codex, "narrate", EnvironmentProbe.narrate),
            mock.patch.object(Codex, "normalize", EnvironmentProbe.normalize),
            mock.patch.object(runner.Run, "stage_extract", no_model_extract),
            mock.patch.object(Codex, "report", side_effect=AssertionError("no real reporters")),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def start_attempt(self):
        spec = specmod.load(self.trial)
        attempt = runner.Run(spec, spec["cells"][0], 1, lambda _: None)
        attempt.go(["prepare", "execute"])
        self.assertTrue(attempt.done("prepare"), attempt.state)
        self.assertTrue(attempt.done("execute"), attempt.state)
        self.assertFalse(attempt.done("teardown"))
        self.assertTrue((Path(attempt.state["paths"]["config_dir"]) / "auth.json").exists())
        return spec, attempt

    def assert_cleaned(self, attempt):
        for stage in ("teardown", "release", "archive"):
            self.assertTrue(attempt.done(stage), attempt.state)
        self.assertTrue(attempt.state["environment_cleanup"]["confirmed"])
        self.assertTrue((attempt.run_dir / "agent-home" / "teardown-ran").exists())
        self.assertEqual((attempt.run_dir / "workspace" / "executions.txt").read_text(), "one execution")
        self.assertFalse((attempt.run_dir / "harness-config" / "auth.json").exists())
        self.assertTrue(self.credential.exists())
        self.assertTrue(all(not Path(path).exists() for path in attempt.state["paths"].values()))

    def test_touching_skill_source_allows_resume_and_pending_cleanup(self):
        original, attempt = self.start_attempt()
        source = self.source / "SKILL.md"
        before = source.stat()
        os.utime(source, ns=(before.st_atime_ns, before.st_mtime_ns + 1_000_000_000))
        reloaded = specmod.load(self.trial)
        old_skill = original["agent_configurations"]["guided"]["skills"]["entries"][0]
        new_skill = reloaded["agent_configurations"]["guided"]["skills"]["entries"][0]
        self.assertEqual(old_skill["sha256"], new_skill["sha256"])
        self.assertNotEqual(old_skill["_identity"], new_skill["_identity"])
        resumed = runner.Run(reloaded, reloaded["cells"][0], 1, lambda _: None)
        self.assertEqual(resumed.contract_sha256, attempt.contract_sha256)
        with mock.patch.object(Codex, "execute", side_effect=AssertionError("must not replay the worker")):
            resumed.go()
        self.assertTrue(all(resumed.done(stage) for stage in runner.STAGES), resumed.state)
        self.assert_cleaned(resumed)

    def test_unchanged_legacy_fingerprint_upgrades_before_metadata_changes(self):
        with mock.patch.object(ac, "resume_contract", side_effect=lambda profile: profile):
            _, attempt = self.start_attempt()
        legacy_sha256 = attempt.contract_sha256
        loaded = specmod.load(self.trial)
        upgraded = runner.Run(loaded, loaded["cells"][0], 1, lambda _: None)
        self.assertNotEqual(upgraded.contract_sha256, legacy_sha256)
        self.assertEqual(json.loads(upgraded.state_path.read_text())["environment_contract_sha256"],
                         upgraded.contract_sha256)
        source = self.source / "SKILL.md"
        before = source.stat()
        os.utime(source, ns=(before.st_atime_ns, before.st_mtime_ns + 1_000_000_000))
        reloaded = specmod.load(self.trial)
        resumed = runner.Run(reloaded, reloaded["cells"][0], 1, lambda _: None)
        with mock.patch.object(Codex, "execute", side_effect=AssertionError("must not replay the worker")):
            resumed.go()
        self.assert_cleaned(resumed)

    def test_changed_skill_content_rejects_resume_without_discarding_cleanup(self):
        original, attempt = self.start_attempt()
        (self.source / "guide.txt").write_text("Different fictional instructions.\n", encoding="utf-8")
        changed = specmod.load(self.trial)
        self.assertNotEqual(
            original["agent_configurations"]["guided"]["skills"]["entries"][0]["sha256"],
            changed["agent_configurations"]["guided"]["skills"]["entries"][0]["sha256"])
        with self.assertRaisesRegex(specmod.SpecError, "changed"):
            runner.Run(changed, changed["cells"][0], 1, lambda _: None)
        # The already loaded, unchanged contract can still finish its cleanup.
        attempt.go(["teardown", "release", "archive"])
        self.assert_cleaned(attempt)

    def test_source_path_and_selection_mode_remain_part_of_resume_contract(self):
        _, attempt = self.start_attempt()
        original_trial = self.trial.read_text(encoding="utf-8")
        alternate = self.root / "alternate" / "product-guide"
        shutil.copytree(self.source, alternate)
        changes = (
            original_trial.replace('mode = "selected"', 'mode = "snapshot"'),
            original_trial.replace('paths = ["skills/product-guide"]', 'paths = ["alternate/product-guide"]'),
        )
        for text in changes:
            with self.subTest(trial=text):
                self.trial.write_text(text, encoding="utf-8")
                changed = specmod.load(self.trial)
                with self.assertRaisesRegex(specmod.SpecError, "changed"):
                    runner.Run(changed, changed["cells"][0], 1, lambda _: None)
        attempt.go(["teardown", "release", "archive"])
        self.assert_cleaned(attempt)
