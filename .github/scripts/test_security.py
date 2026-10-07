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


class PublicationChecks(unittest.TestCase):
    def test_rules_and_safe_examples(self):
        # Deliberately generated dummy values; no credentials are used or verified.
        cases = {
            "personal-email": "fictional-person" + "@" + "invalid.test",
            "personal-home-path": "/" + "Users" + "/fictional-person/project",
            "internal-hostname": "git" + "." + "amazon.com",
            "cloud-account-id": "account=" + "987654" + "321098",
            "captured-agent-metadata": json.dumps({"request" + "_id": "synthetic"}),
            "aws-access-token": "AKIA" + "BCDEFGHIJKLMNOPQ",
        }
        with tempfile.TemporaryDirectory() as tmp:
            for rule, value in cases.items():
                with self.subTest(rule=rule):
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
        safe = "ajx@example.com\nexample@users.noreply.github.com\n/tmp/example-workspace\n"
        proc = subprocess.run([
            SCANNER, "stdin", "--config", str(ROOT / ".gitleaks.toml"),
            "--redact", "--no-banner",
        ], input=safe, text=True, capture_output=True)
        self.assertEqual(proc.returncode, 0, "Safe synthetic examples were rejected")

    def test_hook_checks_staged_bytes_and_private_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)

            def git(*args):
                return subprocess.run(["git", "-C", tmp, *args], check=True,
                                      capture_output=True, text=True)

            git("init", "-b", "main")
            git("config", "user.name", "Synthetic Test")
            git("config", "user.email", "test@example.com")
            git("config", "core.hooksPath", str(ROOT / ".githooks"))
            git("config", "ajx.gitleaksPath", SCANNER)
            shutil.copy(ROOT / ".gitleaks.toml", root / ".gitleaks.toml")
            (root / "notes.txt").write_text("Synthetic public content.\n")
            git("add", ".")
            git("commit", "-m", "Synthetic clean fixture")
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
