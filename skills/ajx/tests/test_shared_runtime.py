"""Shared runtime rules retain the behavior of their CLI and console callers."""

import json
import os
import runpy
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

SKILL = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SKILL / "lib"))

from ajx import console, model_catalog, plan_edit, spec  # noqa: E402
from ajx.util import child_env, env_name_matches  # noqa: E402


class EnvironmentNameTest(unittest.TestCase):
    def test_only_exact_names_and_trailing_star_prefixes_match(self):
        for name, patterns, expected in (
            ("KEY", (), False),
            ("KEY", ("OTHER", "KEY"), True),
            ("KEY_EXTRA", ("KEY",), False),
            ("KEY_EXTRA", ("KEY_*",), True),
            ("key_EXTRA", ("KEY_*",), False),
            ("KEY", ("*",), True),
            ("KEY_EXTRA", ("KEY_?XTRA",), False),
            ("KEY_EXTRA", ("KEY_*TRA",), False),
        ):
            with self.subTest(name=name, patterns=patterns):
                self.assertEqual(env_name_matches(name, patterns), expected)

    def test_legacy_overrides_are_applied_after_inherited_variables_are_dropped(self):
        inherited = {"KEEP": "inherited", "DROP_ONE": "inherited", "DROP_TWO": "inherited"}
        with mock.patch.dict(os.environ, inherited, clear=True):
            result = child_env({"DROP_ONE": "explicit"}, ("DROP_*",))
            self.assertEqual(dict(os.environ), inherited)
        self.assertEqual(result, {"KEEP": "inherited", "DROP_ONE": "explicit"})


class RunHarnessTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.cli = runpy.run_path(str(SKILL / "bin" / "ajx"))

    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.run_dir = Path(temporary.name) / "worker-r2"
        self.run_dir.mkdir()
        self.views = (self.cli["_run_harness"], console._harness_for_run)

    def write(self, filename, data):
        (self.run_dir / filename).write_text(json.dumps(data), encoding="utf-8")

    def test_matching_trial_cell_preserves_its_declarative_adapter(self):
        trial = {"cells": [{"id": "worker", "harness": "custom"}],
                 "adapters": {"custom": {"execute": ["synthetic", "{prompt}"]}}}
        # A matched cell must not depend on files that rendering has not finished.
        (self.run_dir / "run.json").write_text("{", encoding="utf-8")
        for view in self.views:
            with self.subTest(view=view.__name__):
                harness, error = view(trial, self.run_dir)
                self.assertIsNone(error)
                self.assertEqual(harness.name, "custom")
                self.assertEqual(harness.conf["execute"], ["synthetic", "{prompt}"])

    def test_recorded_harness_fallback_and_precedence(self):
        marker = object()
        self.write("environment.json", {"harness": {"name": "prepared"}})
        for rendered in (False, True):
            if rendered:
                self.write("run.json", {"agent": {"harness_name": "rendered"}})
            expected = "rendered" if rendered else "prepared"
            for view in self.views:
                with self.subTest(rendered=rendered, view=view.__name__):
                    with mock.patch.object(spec.plugins, "get", return_value=lambda: marker) as get:
                        harness, error = view(None, self.run_dir)
                    self.assertIs(harness, marker)
                    self.assertIsNone(error)
                    get.assert_called_once_with("harness", expected)

    def test_non_numeric_repetition_does_not_match_a_trial_cell(self):
        self.run_dir = self.run_dir.with_name("worker-r2-extra")
        self.run_dir.mkdir()
        self.write("environment.json", {"harness": {"name": "prepared"}})
        trial = {"cells": [{"id": "worker", "harness": "incorrect"}]}
        with mock.patch.object(spec.plugins, "get", return_value=lambda: object()) as get:
            spec.harness_for_run(trial, self.run_dir)
        get.assert_called_once_with("harness", "prepared")

    def test_unknown_harness_keeps_each_views_message(self):
        self.assertEqual(self.views[0](None, self.run_dir), (
            None, "harness unknown: no trial cell matches this run and it records no harness yet"))
        self.assertEqual(self.views[1](None, self.run_dir), (
            None, "The agent for this run is not known yet."))

    def test_unavailable_adapter_keeps_cli_details_out_of_console_message(self):
        self.write("run.json", {"agent": {"harness_name": "missing"}})
        with mock.patch.object(spec.plugins, "get", side_effect=LookupError("synthetic detail")):
            self.assertEqual(self.views[0](None, self.run_dir), (
                None, "harness 'missing' unavailable: LookupError: synthetic detail"))
            self.assertEqual(self.views[1](None, self.run_dir), (
                None, "agent 'missing' unavailable: LookupError"))

    def test_partially_written_evidence_does_not_break_either_view(self):
        (self.run_dir / "run.json").write_text("{", encoding="utf-8")
        for view in self.views:
            with self.subTest(view=view.__name__):
                harness, error = view(None, self.run_dir)
                self.assertIsNone(harness)
                self.assertIn("None unavailable: JSONDecodeError", error)


class ModelNameTest(unittest.TestCase):
    def test_shared_parser_handles_defaults_aliases_and_custom_ids(self):
        for value, harness, expected in (
            (None, "codex", None), (" \t", "codex", None),
            (" Sol ", "codex", "gpt-6.1-sol"),
            ("gateway/MyModel-v2", "claude-code", "gateway/MyModel-v2"),
            ("x" * 200, "custom", "x" * 200),
        ):
            with self.subTest(value=value, harness=harness):
                self.assertEqual(model_catalog.parse_name(value, harness), expected)
                self.assertEqual(console._model_name(value, harness), expected)
                self.assertEqual(plan_edit._model_name(value, harness), expected)

    def test_invalid_names_preserve_caller_specific_guidance(self):
        for value in ("two words", "--option", "x" * 201):
            with self.subTest(value=value):
                with self.assertRaises(model_catalog.InvalidModelName) as error:
                    model_catalog.parse_name(value, "codex")
                self.assertEqual(error.exception.name, value)
                with self.assertRaises(console.ConsoleError) as error:
                    console._model_name(value, "codex")
                self.assertEqual(str(error.exception),
                                 f"{value!r} is not a model name an agent CLI accepts: use an alias such as sonnet, opus or "
                                 "haiku, or the provider's model id (no spaces), or leave it empty for the agent's default")
                with self.assertRaises(plan_edit.PlanError) as error:
                    plan_edit._model_name(value, "codex")
                self.assertEqual(str(error.exception),
                                 f"{value!r} is not a model name an agent CLI accepts: use an alias such as sonnet, or the "
                                 "provider's model id (no spaces)")

    def test_wrong_harness_alias_preserves_resolver_guidance(self):
        with self.assertRaises(ValueError) as source:
            model_catalog.resolve("codex", "opus")
        for parse, error_type in ((console._model_name, console.ConsoleError),
                                  (plan_edit._model_name, plan_edit.PlanError)):
            with self.assertRaises(error_type) as error:
                parse("opus", "codex")
            self.assertEqual(str(error.exception), str(source.exception))

    def test_lenient_plan_reading_does_not_validate_or_resolve_existing_values(self):
        with plan_edit._lenient():
            self.assertEqual(plan_edit._model_name("  invalid name  ", "codex"), "invalid name")
            self.assertEqual(plan_edit._model_name(" Sol ", "codex"), "Sol")
            self.assertIsNone(plan_edit._model_name(" "))
        with self.assertRaises(plan_edit.PlanError):
            plan_edit._model_name("invalid name", "codex")


if __name__ == "__main__":
    unittest.main()
