"""Synthetic regressions for host verification and isolated-environment auth validation."""

import http.server
import os
import sys
import tempfile
import threading
import unittest
import urllib.request
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "lib"))
from ajx import spec as specmod
from ajx.builtin.generic import FileExistsCheck, HttpCheck


class WorkspaceFileVerificationTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        self.outside = self.root / "outside"
        self.outside.mkdir()
        (self.outside / "result.txt").write_text("synthetic external data")
        self.checker = FileExistsCheck()

    def check(self, path, **options):
        return self.checker.run({"path": path, **options}, {"workspace": self.workspace})

    def test_regular_nested_artifacts_and_content_counts(self):
        output = self.workspace / "output"
        output.mkdir()
        (output / "one.txt").write_text("expected row")
        (output / "two.txt").write_text("different row")
        self.assertTrue(self.check("output/*.txt", min_count=2)["passed"])
        self.assertTrue(self.check("output/*.txt", contains="expected")["passed"])
        self.assertFalse(self.check("output/*.txt", contains="expected", min_count=2)["passed"])

    def test_leaf_and_directory_symlinks_cannot_supply_host_evidence(self):
        (self.workspace / "result.txt").symlink_to(self.outside / "result.txt")
        (self.workspace / "linked").symlink_to(self.outside, target_is_directory=True)
        for pattern in ("result.txt", "linked/result.txt", "linked/*.txt"):
            for contains in (None, "synthetic external"):
                with self.subTest(pattern=pattern, contains=contains):
                    result = self.check(pattern, contains=contains)
                    self.assertFalse(result["passed"])
                    self.assertEqual(result["detail"], "0 match(es): []")

    def test_symlink_to_an_internal_file_is_also_rejected(self):
        (self.workspace / "original.txt").write_text("expected")
        (self.workspace / "alias.txt").symlink_to("original.txt")
        self.assertFalse(self.check("alias.txt")["passed"])
        self.assertTrue(self.check("*.txt", min_count=1)["passed"])
        self.assertFalse(self.check("*.txt", min_count=2)["passed"])

    def test_file_replaced_by_symlink_before_open_is_rejected(self):
        artifact = self.workspace / "result.txt"
        artifact.write_text("expected")
        open_file = os.open
        replaced = False

        def replace_then_open(path, flags, *args, **kwargs):
            nonlocal replaced
            if path == "result.txt" and kwargs.get("dir_fd") is not None and not replaced:
                replaced = True
                artifact.unlink()
                artifact.symlink_to(self.outside / "result.txt")
            return open_file(path, flags, *args, **kwargs)

        with mock.patch("ajx.builtin.generic.os.open", side_effect=replace_then_open):
            result = self.check("result.txt", contains="synthetic external")
        self.assertTrue(replaced)
        self.assertFalse(result["passed"])

    def test_symlinked_workspace_and_special_files_are_rejected(self):
        linked_workspace = self.root / "linked-workspace"
        linked_workspace.symlink_to(self.outside, target_is_directory=True)
        result = self.checker.run({"path": "result.txt"}, {"workspace": linked_workspace})
        self.assertFalse(result["passed"])
        os.mkfifo(self.workspace / "output.txt")
        self.assertFalse(self.check("output.txt")["passed"])

    def test_directory_candidates_do_not_leak_file_descriptors(self):
        (self.workspace / "directory").mkdir()
        open_file, descriptors = os.open, []

        def record_open(*args, **kwargs):
            descriptor = open_file(*args, **kwargs)
            descriptors.append(descriptor)
            return descriptor

        with mock.patch("ajx.builtin.generic.os.open", side_effect=record_open):
            result = self.check("*")
        leaked = []
        for descriptor in set(descriptors):
            try:
                os.fstat(descriptor)
            except OSError:
                continue
            leaked.append(descriptor)
            os.close(descriptor)
        self.assertFalse(result["passed"])
        self.assertEqual(leaked, [], "directory verification must release its descriptors")

    def test_globs_cannot_traverse_outside_the_workspace(self):
        for pattern in ("../outside/*.txt", str(self.outside / "result.txt"), ""):
            with self.subTest(pattern=pattern):
                self.assertFalse(self.check(pattern)["passed"])


class HostHttpVerificationTest(unittest.TestCase):
    def server(self, body):
        calls = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                calls.append(("GET", self.path))
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_CONNECT(self):
                calls.append(("CONNECT", self.path))
                self.send_error(502)

            def log_message(self, *args):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()

        def close():
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)

        self.addCleanup(close)
        return f"http://127.0.0.1:{server.server_port}", calls

    def test_explicit_environment_http_cannot_be_satisfied_by_ambient_proxy(self):
        origin, origin_calls = self.server(b"origin-proof")
        proxy, proxy_calls = self.server(b"proxy-proof")
        env = {"http_proxy": proxy, "https_proxy": proxy, "HTTP_PROXY": proxy, "HTTPS_PROXY": proxy,
               "NO_PROXY": "", "no_proxy": ""}
        check = {"url": origin + "/result", "timeout": 2, "expect_body": "proxy-proof"}
        checker = HttpCheck()
        with mock.patch.dict(os.environ, env, clear=True), mock.patch.object(urllib.request, "_opener", None):
            legacy = checker.run(check, {})
            global_opener = urllib.request._opener
            isolated = checker.run(check, {"environment_profile": {"backend": "local"}})
            direct = checker.run({**check, "expect_body": "origin-proof"},
                                 {"environment_profile": {"backend": "container"}})
            self.assertIs(urllib.request._opener, global_opener)
        self.assertTrue(legacy["passed"], legacy)
        self.assertEqual(legacy["proxy_policy"], "urllib_default")
        self.assertFalse(isolated["passed"], isolated)
        self.assertEqual(isolated["stdout"], "origin-proof")
        self.assertTrue(direct["passed"], direct)
        self.assertEqual(direct["proxy_policy"], "disabled")
        self.assertEqual(len(proxy_calls), 1)
        self.assertEqual(origin_calls, [("GET", "/result"), ("GET", "/result")])

    def test_explicit_environment_https_failure_does_not_contact_ambient_proxy(self):
        origin, _ = self.server(b"plain-http-only")
        proxy, proxy_calls = self.server(b"proxy-proof")
        # A plain HTTP origin cannot complete TLS. The failure must come from the
        # direct connection, without a CONNECT request to the ambient proxy.
        with mock.patch.dict(os.environ, {"https_proxy": proxy, "HTTPS_PROXY": proxy,
                                          "NO_PROXY": "", "no_proxy": ""}, clear=True):
            result = HttpCheck().run(
                {"url": origin.replace("http:", "https:") + "/result", "timeout": 1},
                {"environment_profile": {"backend": "local"}},
            )
        self.assertFalse(result["passed"], result)
        self.assertEqual(result["proxy_policy"], "disabled")
        self.assertEqual(proxy_calls, [])


class EnvironmentAuthValidationTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / "task.md").write_text("A synthetic task; no agent is launched.")

    def trial(self, harness, *, isolated=True, auth=None, auth_config="", verify=""):
        environment = 'environment = "developer"\n' if isolated else ""
        cell_auth = f'auth = "{auth}"\n' if auth else ""
        path = self.root / "trial.toml"
        path.write_text(
            '[trial]\nid = "auth-validation"\nproduct = "synthetic"\n'
            + environment
            + '[task]\nprompt_file = "task.md"\n'
            + '[environments.developer]\nbackend = "local"\n'
            + auth_config
            + '[[cells]]\nid = "worker"\n'
            + f'harness = "{harness}"\n'
            + cell_auth
            + verify
        )
        return path

    def test_environment_without_agent_profile_rejects_home_dependent_login(self):
        for harness in ("claude-code", "kiro-cli"):
            with self.subTest(harness=harness):
                with self.assertRaisesRegex(specmod.SpecError, "fresh HOME/config"):
                    specmod.load(self.trial(harness))
        with self.assertRaisesRegex(specmod.SpecError, "fresh HOME/config"):
            specmod.load(self.trial(
                "codex", auth="custom",
                auth_config='[auth.custom]\ntype = "env"\nisolates_config = false\n',
            ))

    def test_environment_accepts_staged_login_and_environment_auth(self):
        staged = specmod.load(self.trial("codex"))
        self.assertEqual(specmod.auth_for(staged, staged["cells"][0]).name, "codex-chatgpt")
        for harness in ("claude-code", "kiro-cli"):
            with self.subTest(harness=harness):
                loaded = specmod.load(self.trial(
                    harness, auth="custom",
                    auth_config='[auth.custom]\ntype = "env"\nisolates_config = true\n',
                ))
                self.assertTrue(specmod.auth_for(loaded, loaded["cells"][0]).isolates_config)

    def test_legacy_local_runs_still_allow_the_callers_login(self):
        for harness in ("claude-code", "kiro-cli"):
            with self.subTest(harness=harness):
                loaded = specmod.load(self.trial(harness, isolated=False))
                self.assertIsNone(specmod.environment_for(loaded, loaded["cells"][0]))

    def test_file_check_globs_are_validated_before_launch(self):
        for pattern in ("../outside/*.txt", "/synthetic/result.txt", ""):
            with self.subTest(pattern=pattern):
                with self.assertRaisesRegex(specmod.SpecError, "beneath the workspace"):
                    specmod.load(self.trial(
                        "codex",
                        verify='[[verify]]\ntype = "file_exists"\n' + f'path = "{pattern}"\n',
                    ))


if __name__ == "__main__":
    unittest.main()
