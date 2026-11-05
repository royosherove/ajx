"""Prepared launch controls and source facts must not imply observed access."""

import sys
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "lib"))

from ajx import envcheck


class ConfigurationEvidenceTest(unittest.TestCase):
    def test_auth_requirement_is_distinct_from_runtime_isolation(self):
        config = envcheck.configuration_controls(
            {"config": "clean"}, SimpleNamespace(clean_supported=True),
            SimpleNamespace(isolates_config=False))
        self.assertFalse(config["auth_allows_isolation"])
        self.assertIn("requires caller configuration", config["note"])
        self.assertNotIn("config_isolated", config)
        config = envcheck.configuration_controls(
            {"config": "clean"}, SimpleNamespace(clean_supported=True),
            SimpleNamespace(isolates_config=True))
        self.assertTrue(config["auth_allows_isolation"])
        self.assertIn("runtime evidence", config["note"])

    def test_only_prepared_shell_controls_are_recorded(self):
        ctx = {"run_token": "doctor", "run_dir": "/synthetic/run",
               "env": {"ZDOTDIR": "/synthetic/run/no-shell-startup"},
               "unset_env": ["BASH_ENV", "ENV"]}
        controls = envcheck.shell_startup_controls({"shell_startup": "skip"}, ctx)
        self.assertEqual(controls["evidence_scope"], "doctor_probe")
        self.assertEqual(controls["configured_env_names"], ["ZDOTDIR"])
        self.assertEqual(controls["unset_env_names"], ["BASH_ENV", "ENV"])
        self.assertIsNone(controls["startup_files_read"])
        controls = envcheck.shell_startup_controls({"shell_startup": "skip"}, {"run_token": "run"})
        self.assertEqual(controls["configured_env_names"], [])
        self.assertEqual(controls["unset_env_names"], [])
        self.assertIn("only the listed", controls["note"])
        ctx["env"]["ZDOTDIR"] = "/synthetic/user-config"
        controls = envcheck.shell_startup_controls({"shell_startup": "skip"}, ctx)
        self.assertEqual(controls["configured_env_names"], [])
        ctx["environment_profile"] = {"backend": "local"}
        controls = envcheck.shell_startup_controls({"shell_startup": "skip"}, ctx)
        self.assertEqual(controls["configured_env_names"], [])
        self.assertIn("not applied", controls["note"])

    def test_settings_sources_follow_effective_precedence(self):
        harness = SimpleNamespace(
            clean_env=lambda ctx: {}, strip_env=[],
            settings_env=lambda process, workspace: [
                ("user.json", {"AWS_PROFILE": "discarded-profile", "AWS_REGION": "synthetic-region"}),
                ("managed.json", {"AWS_PROFILE": "effective-profile"}),
            ],
            tool_shell=lambda env: "/bin/sh")
        with mock.patch.object(envcheck, "child_env", side_effect=lambda env, unset: dict(env)):
            view = envcheck.worker_view({}, harness, SimpleNamespace(unset=lambda: []),
                                        {"env": {}, "workspace": "/synthetic"})
        sources = envcheck._aws_settings_sources(view)
        self.assertEqual(sources, [
            {"source": "harness settings managed.json", "names": ["AWS_PROFILE"]},
            {"source": "harness settings user.json", "names": ["AWS_REGION"]},
        ])
        self.assertNotIn("discarded-profile", str(sources))
        self.assertNotIn("effective-profile", str(sources))

    def test_identity_warning_names_effective_settings_sources(self):
        sources = envcheck._aws_settings_sources({"source": {
            "AWS_PROFILE": "harness settings fixture.json",
            "AWS_REGION": "harness settings fixture.json",
            "AWS_PAGER": "harness settings unrelated.json",
            "OTHER": "harness settings unrelated.json",
        }})
        self.assertEqual(sources, [{"source": "harness settings fixture.json",
                                    "names": ["AWS_PROFILE", "AWS_REGION"]}])
        text = envcheck.default_identity_warning({
            "default_aws_identity": {"ok": True, "output": "synthetic-model"},
            "auth_identity": "synthetic-model", "aws_settings_sources": sources})
        self.assertIn("AWS_PROFILE, AWS_REGION", text)
        self.assertIn("fixture.json", text)
        self.assertIn("does not disable these harness settings", text)
        self.assertNotIn("unrelated.json", text)


if __name__ == "__main__":
    unittest.main()
