"""Offline integration checks for publication rules and the staged-content hook."""

import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SCANNER = shutil.which(sys.argv[1] if len(sys.argv) > 1 else "gitleaks")
sys.argv[1:] = []
if not SCANNER:
    raise SystemExit("Gitleaks is required.")
SCANNER = str(Path(SCANNER).resolve())


def synthetic_repository(directory, *, hooks=False):
    """Create an isolated public fixture; return its path and Git runner."""
    root = Path(directory)

    def git(*args):
        return subprocess.run(["git", "-C", str(root), *args], check=True,
                              capture_output=True, text=True)

    git("init", "-b", "main")
    # History tests create rejected commits before installing a hook. The staged
    # content test enables the real hook, so it exercises Git's hook invocation.
    git("config", "core.hooksPath", str(ROOT / ".githooks" if hooks else root / "empty-hooks"))
    git("config", "user.name", "Synthetic Test")
    git("config", "user.email", "test@example.com")
    git("config", "ajx.gitleaksPath", SCANNER)
    shutil.copy(ROOT / ".gitleaks.toml", root / ".gitleaks.toml")
    (root / "notes.txt").write_text("Safe fixture.\n")
    git("add", ".")
    git("commit", "-m", "Safe synthetic history")
    return root, git


class PublicationChecks(unittest.TestCase):
    def test_pre_push_blocks_private_history_and_author_metadata(self):
        for private_kind in ("content", "author", "unreferenced"):
            with self.subTest(private_kind=private_kind), tempfile.TemporaryDirectory() as tmp:
                root, git = synthetic_repository(tmp)
                gate = [str(ROOT / ".githooks/pre-push")]
                clean_head = git("rev-parse", "HEAD").stdout.strip()
                deletion = "(delete) " + "0" * 40 + " refs/heads/old " + clean_head + "\n"
                clean = subprocess.run(gate, cwd=root, input=deletion, capture_output=True, text=True)
                self.assertEqual(clean.returncode, 0, clean.stderr)
                marker = "fictional-person" + "@" + "invalid.test"
                if private_kind in ("content", "unreferenced"):
                    (root / "notes.txt").write_text(marker + "\n")
                else:
                    git("config", "user.email", marker)
                    (root / "notes.txt").write_text("Another safe fixture.\n")
                git("add", "notes.txt")
                git("commit", "-m", "Synthetic rejected history")
                outgoing = git("rev-parse", "HEAD").stdout.strip()
                if private_kind == "unreferenced":
                    git("reset", "--hard", clean_head)
                refs = "HEAD " + outgoing + " refs/heads/result " + "0" * 40 + "\n"
                rejected = subprocess.run(gate, cwd=root, input=refs, capture_output=True, text=True)
                self.assertNotEqual(rejected.returncode, 0)
                self.assertNotIn(marker, rejected.stdout + rejected.stderr)

    def test_pre_push_scans_annotated_tag_metadata(self):
        for kind in ("safe", "tagger", "message", "nested"):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as tmp:
                root, git = synthetic_repository(tmp)
                marker = "fictional-person" + "@" + "invalid.test"
                if kind == "tagger":
                    git("config", "user.email", marker)
                message = marker if kind in ("message", "nested") else "Safe release"
                git("tag", "-a", "synthetic", "-m", message)
                if kind == "nested":
                    git("tag", "-a", "outer", "synthetic", "-m", "Safe outer tag")
                    outgoing = git("rev-parse", "outer").stdout.strip()
                else:
                    outgoing = git("rev-parse", "synthetic").stdout.strip()
                gate = [str(ROOT / ".githooks/pre-push")]
                # CI scans reachable tags with no pre-push input.
                for unreferenced in (False, True):
                    if unreferenced:
                        git("tag", "-d", "synthetic")
                        if kind == "nested":
                            git("tag", "-d", "outer")
                    refs = ("tag " + outgoing + " refs/tags/release " + "0" * 40 + "\n"
                            if unreferenced else "")
                    result = subprocess.run(gate, cwd=root, input=refs,
                                            capture_output=True, text=True)
                    if kind == "safe":
                        self.assertEqual(result.returncode, 0, result.stderr)
                    else:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertNotIn(marker, result.stdout + result.stderr)

    def test_rules_and_safe_examples(self):
        # Deliberately generated dummy values; no credentials are used or verified.
        cases = [
            ("personal-email", "fictional-person" + "@" + "invalid.test"),
            ("personal-email", "fictional-person" + "@" + "github.com"),
            ("personal-email", "support" + "@" + "github.com.invalid.test"),
            ("personal-email", "fictional-person" + "@" + "anthropic.com"),
            ("personal-email", "noreply" + "@" + "anthropic.com.invalid.test"),
            ("personal-email", "not-noreply" + "@" + "anthropic.com"),
            ("personal-home-path", "/" + "Users" + "/fictional-person/project"),
            ("internal-hostname", "git" + "." + "amazon.com"),
            ("cloud-account-id", "account=" + "987654" + "321098"),
            ("captured-agent-metadata", json.dumps({"request" + "_id": "synthetic"})),
            ("aws-access-token", "AKIA" + "BCDEFGHIJKLMNOPQ"),
        ]
        with tempfile.TemporaryDirectory() as tmp:
            for index, (rule, value) in enumerate(cases):
                with self.subTest(rule=rule, case=index):
                    report = Path(tmp) / "report.json"
                    report.unlink(missing_ok=True)
                    proc = subprocess.run([
                        SCANNER, "stdin", "--config", str(ROOT / ".gitleaks.toml"),
                        "--redact", "--no-banner", "--report-format", "json",
                        "--report-path", str(report),
                    ], input=value + "\n", text=True, capture_output=True)
                    self.assertEqual(proc.returncode, 1, f"{rule} was not rejected")
                    findings = json.loads(report.read_text())
                    self.assertIn(rule, {f["RuleID"] for f in findings})
                    self.assertNotIn(value, proc.stdout + proc.stderr + report.read_text())
        safe = "\n".join([
            "ajx@example.com",
            "example@users.noreply.github.com",
            "noreply@github.com",
            "support@github.com",
            "Co-Authored-By: Claude <noreply@anthropic.com>",
            "/tmp/example-workspace",
            "",
        ])
        proc = subprocess.run([
            SCANNER, "stdin", "--config", str(ROOT / ".gitleaks.toml"),
            "--redact", "--no-banner",
        ], input=safe, text=True, capture_output=True)
        self.assertEqual(proc.returncode, 0, "Safe synthetic examples were rejected")

    def test_hook_checks_staged_bytes_and_private_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, git = synthetic_repository(tmp, hooks=True)
            clean_head = git("rev-parse", "HEAD").stdout

            marker = "person" + "@" + "invalid.test"
            (root / "notes.txt").write_text(marker + "\n")
            git("add", "notes.txt")
            (root / "notes.txt").write_text("Clean working tree; index still has dummy private data.\n")
            proc = subprocess.run(["git", "-C", tmp, "commit", "-m", "Must be rejected"],
                                  capture_output=True, text=True)
            self.assertNotEqual(proc.returncode, 0, "Hook accepted private staged content")
            self.assertEqual(git("rev-parse", "HEAD").stdout, clean_head)
            self.assertNotIn(marker, proc.stdout + proc.stderr)
            git("restore", "--staged", "notes.txt")

            (root / "Config").write_text("synthetic internal package metadata\n")
            git("add", "Config")
            proc = subprocess.run([str(ROOT / ".githooks/pre-commit")],
                                  cwd=root, capture_output=True, text=True)
            self.assertNotEqual(proc.returncode, 0, "Hook accepted a private packaging file")

            git("config", "ajx.gitleaksPath", str(root / "missing-scanner"))
            proc = subprocess.run([str(ROOT / ".githooks/pre-commit")],
                                  cwd=root, capture_output=True, text=True)
            self.assertNotEqual(proc.returncode, 0, "Hook accepted a missing scanner")


if __name__ == "__main__":
    unittest.main()
