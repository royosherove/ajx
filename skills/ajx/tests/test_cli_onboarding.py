"""Offline coverage for documentation discovery and doctor output."""

import contextlib
import importlib.util
import io
from importlib.machinery import SourceFileLoader
from pathlib import Path
import unittest
from unittest import mock


SKILL = Path(__file__).resolve().parents[1]


def load_cli():
    path = str(SKILL / "bin" / "ajx")
    spec = importlib.util.spec_from_file_location("ajx_cli_onboarding", path,
                                                 loader=SourceFileLoader("ajx_cli_onboarding", path))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class OnboardingTest(unittest.TestCase):
    def test_root_help_entry_points_share_banner(self):
        cli = load_cli()
        prefix = (SKILL / "assets/logo.txt").read_text(encoding="utf-8") + f"v{cli.__version__}\n\n"
        baseline = None
        for args in ([], ["help"], ["--help"], ["-h"]):
            with self.subTest(args=args):
                output, diagnostic = io.StringIO(), io.StringIO()
                with mock.patch("subprocess.run", side_effect=AssertionError("no commands allowed")), \
                        contextlib.redirect_stdout(output), contextlib.redirect_stderr(diagnostic), \
                        self.assertRaises(SystemExit) as result:
                    cli.main(args)
                self.assertEqual(result.exception.code, 0)
                self.assertEqual(diagnostic.getvalue(), "")
                self.assertTrue(output.getvalue().startswith(prefix + "usage: ajx"))
                self.assertEqual(output.getvalue().count(prefix), 1)
                if baseline is not None:
                    self.assertEqual(output.getvalue(), baseline)
                baseline = output.getvalue()

    def test_banner_does_not_decorate_subcommand_help_version_or_errors(self):
        cli = load_cli()
        logo = (SKILL / "assets/logo.txt").read_text(encoding="utf-8")
        for args, code in ((["doctor", "--help"], 0), (["--version"], 0),
                           (["unknown-command"], 2), (["doctor", "--unknown"], 2),
                           (["help", "--unknown"], 2)):
            with self.subTest(args=args):
                output, diagnostic = io.StringIO(), io.StringIO()
                with contextlib.redirect_stdout(output), contextlib.redirect_stderr(diagnostic), \
                        self.assertRaises(SystemExit) as result:
                    cli.main(args)
                self.assertEqual(result.exception.code, code)
                self.assertNotIn(logo, output.getvalue() + diagnostic.getvalue())

    def test_docs_reads_local_reference_without_processes(self):
        cli = load_cli()
        for topic, relative in [
            ("trial", "examples/trial.toml"),
            ("artifacts", "references/artifacts.md"),
            ("briefing", "references/briefing.md"),
            ("environments", "references/environments.md"),
            ("console", "references/console.md"),
        ]:
            with self.subTest(topic=topic):
                output = io.StringIO()
                with mock.patch("subprocess.run", side_effect=AssertionError("no commands allowed")), \
                        contextlib.redirect_stdout(output):
                    self.assertEqual(cli.main(["docs", topic]), 0)
                self.assertEqual(output.getvalue(), (SKILL / relative).read_text())

    def test_preparation_docs_are_discoverable(self):
        cli = load_cli()
        output = io.StringIO()
        with mock.patch("subprocess.run", side_effect=AssertionError("no commands allowed")), \
                contextlib.redirect_stdout(output):
            self.assertEqual(cli.main(["docs"]), 0)
        lines = output.getvalue().splitlines()
        for topic in ("briefing", "environments", "console"):
            self.assertIn(f"ajx docs {topic:12s} references/{topic}.md", lines)
        output = io.StringIO()
        with contextlib.redirect_stdout(output), self.assertRaises(SystemExit) as result:
            cli.main(["docs", "--help"])
        self.assertEqual(result.exception.code, 0)
        for topic in ("briefing", "environments", "console"):
            self.assertIn(topic, output.getvalue())

    def test_doctor_prints_final_ready_verdict(self):
        cli = load_cli()
        output = io.StringIO()
        with mock.patch.object(cli.plugins, "load"), mock.patch.object(cli.plugins, "names", return_value=[]), \
                mock.patch("subprocess.run", side_effect=AssertionError("no commands allowed")), \
                contextlib.redirect_stdout(output):
            self.assertEqual(cli.main(["doctor"]), 0)
        self.assertTrue(output.getvalue().splitlines()[-1].startswith("READY:"))
        prefix = (SKILL / "assets/logo.txt").read_text(encoding="utf-8") + f"v{cli.__version__}\n\n"
        self.assertTrue(output.getvalue().startswith(prefix + "python "))
        self.assertEqual(output.getvalue().count(prefix), 1)

    def test_doctor_keeps_startup_source_in_identity_warning(self):
        cli = load_cli()
        auth = mock.Mock(profile="synthetic", name="env", isolates_config=True)
        auth.problems.return_value = []
        auth.env.return_value = {}
        auth.unset.return_value = []
        runner = mock.Mock(name="local")
        runner.name = "local"
        runner.describe.return_value = {"type": "local"}
        spec = {
            "reporter": {"harness": "synthetic"},
            "cells": [{"id": "worker", "harness": "synthetic", "config": "clean"}],
            "task": {}, "preflight": [],
        }
        checks = {
            "harness_settings": [], "local_runner": True, "tool_shell": "/bin/sh",
            "tool_shell_probed": True, "overrides": [], "collisions": [],
            "snapshot_restored": [], "errors": [],
        }
        output = io.StringIO()
        patches = [
            mock.patch.object(cli.specmod, "auth_for", return_value=auth),
            mock.patch.object(cli.specmod, "harness_for", return_value=mock.Mock()),
            mock.patch.object(cli.specmod, "runner_for", return_value=runner),
            mock.patch.object(cli.specmod, "environment_for", return_value=None),
            mock.patch.object(cli.specmod, "worker_env", return_value={}),
            mock.patch.object(cli.envcheck, "scratch_workspace", return_value=Path("/tmp")),
            mock.patch.object(cli.envcheck, "placeholder_ctx", return_value={}),
            mock.patch.object(cli.envcheck, "inspect_cell",
                              return_value=(checks, {"effective": {}, "shell": "/bin/sh"})),
            mock.patch.object(cli.envcheck, "auth_identity", return_value="synthetic-model"),
            mock.patch.object(cli.envcheck, "uses_aws", return_value=True),
            mock.patch.object(cli.envcheck, "identity_result",
                              return_value={"ok": True, "output": "synthetic-default"}),
            mock.patch.object(cli.envcheck, "startup_source", return_value="~/.zshenv line 1"),
            mock.patch.object(cli.envcheck, "command_env", return_value={}),
            mock.patch("subprocess.run", side_effect=AssertionError("no commands allowed")),
        ]
        with contextlib.ExitStack() as stack, contextlib.redirect_stdout(output):
            for patch in patches:
                stack.enter_context(patch)
            self.assertTrue(cli._doctor_cells(spec, Path("/tmp")))
        self.assertIn("~/.zshenv line 1", output.getvalue())
        self.assertIn('shell_startup = "skip"', output.getvalue())


if __name__ == "__main__":
    unittest.main()
