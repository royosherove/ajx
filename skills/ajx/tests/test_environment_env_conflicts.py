"""Ambiguous set/unset declarations must fail before an explicit trial starts."""
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))
from ajx import spec as specmod
from ajx.util import child_env


class EnvironmentVariableConflictTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        (self.root / "task.md").write_text("Synthetic trial; no provider calls.")

    def trial(self, *, layer="auth", unset="OPENAI_*", backend="local", isolated=True,
              required=False, configuration=False):
        value = {"OPENAI_API_KEY": "${AJX_TEST_API_KEY}"}
        inline = lambda mapping: "{ " + ", ".join(
            f"{key} = {json.dumps(value)}" for key, value in mapping.items()) + " }"
        selected = 'environment = "worker"\n' if isolated else ""
        if configuration:
            selected += 'agent_configuration = "plain"\n'
        image = 'image = "sha256:' + "a" * 64 + '"\n' if backend == "container" else ""
        path = self.root / "trial.toml"
        path.write_text(
            '[trial]\nid = "env-conflicts"\nproduct = "synthetic"\noutput_dir = "results"\n'
            + f'workspace_root = {json.dumps(str(self.root / "workspaces"))}\n' + selected
            + '[task]\nprompt_file = "task.md"\n[env]\n'
            + ('OPENAI_API_KEY = "${AJX_TEST_API_KEY}"\n' if layer == "trial" else "")
            + '[environments.worker]\n' + f'backend = "{backend}"\n' + image
            + ('[agent_configurations.plain.skills]\nmode = "none"\n' if configuration else "")
            + '[auth.explicit]\ntype = "env"\nisolates_config = true\n'
            + f'env = {inline(value if layer == "auth" else {})}\nunset = {json.dumps([unset])}\n'
            + ('required_env = ["OPENAI_API_KEY"]\n' if required else "")
            + '[reporter]\nharness = "codex"\nauth = "inherit"\n'
            + '[[cells]]\nid = "worker"\nharness = "codex"\nauth = "explicit"\n'
            + f'env = {inline(value if layer == "cell" else {})}\n'
        )
        return path

    def assert_rejected_without_preparation(self, **options):
        with mock.patch.dict(os.environ, {
            "AJX_TEST_API_KEY": "synthetic-replacement",
            "OPENAI_API_KEY": "synthetic-ambient",
        }, clear=True), mock.patch("subprocess.run", side_effect=AssertionError("must not launch")):
            with self.assertRaisesRegex(specmod.SpecError, "both supplied and unset") as raised:
                specmod.load(self.trial(**options))
        self.assertIn("OPENAI_API_KEY", str(raised.exception))
        self.assertNotIn("synthetic-replacement", str(raised.exception))
        self.assertNotIn("synthetic-ambient", str(raised.exception))
        self.assertFalse((self.root / "results").exists())
        self.assertFalse((self.root / "workspaces").exists())

    def test_exact_and_prefix_conflicts_fail_for_all_declared_layers_and_backends(self):
        for backend in ("local", "container"):
            for layer in ("trial", "cell", "auth"):
                for unset in ("OPENAI_API_KEY", "OPENAI_*"):
                    with self.subTest(backend=backend, layer=layer, unset=unset):
                        self.assert_rejected_without_preparation(
                            backend=backend, layer=layer, unset=unset)

    def test_required_forwarded_variables_cannot_be_silently_removed(self):
        self.assert_rejected_without_preparation(layer="none", required=True)

    def test_implicit_environment_for_agent_configuration_also_rejects_conflicts(self):
        self.assert_rejected_without_preparation(isolated=False, configuration=True)

    def test_disjoint_unset_is_valid_and_legacy_override_precedence_is_preserved(self):
        with mock.patch.dict(os.environ, {"AJX_TEST_API_KEY": "synthetic-replacement"}, clear=True):
            specmod.load(self.trial(unset="OTHER_PROVIDER_*"))
            spec = specmod.load(self.trial(isolated=False))
            cell = spec["cells"][0]
            auth = specmod.auth_for(spec, cell)
            values = child_env(specmod.worker_env(spec, cell, auth), auth.unset())
        self.assertEqual(values["OPENAI_API_KEY"], "synthetic-replacement")


if __name__ == "__main__":
    unittest.main()
