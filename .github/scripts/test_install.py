"""Offline installer regression tests; optionally exercise a real native package."""
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
REAL_PACKAGE = os.environ.get("AJX_INSTALL_TEST_PACKAGE")
TARGET = ("darwin" if sys.platform == "darwin" else "linux") + "-" + (
    "arm64" if platform.machine() in ("aarch64", "arm64") else "amd64"
)
COMMIT = "a" * 40
INSTALL_ONE_LINER = (
    "bash -o pipefail -c 'curl -fsSL "
    "https://github.com/royosherove/ajx/releases/latest/download/install.sh | bash'"
    ' && export PATH="$HOME/.local/bin:$PATH"'
)


class InstallTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ajx-install-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root / "home with spaces"
        self.home.mkdir()
        self.package = self.root / "package"
        self.package.mkdir()
        self.fakebin = self.root / "fakebin"
        self.fakebin.mkdir()
        self.env = {
            "HOME": str(self.home), "PATH": str(self.fakebin) + ":/usr/bin:/bin",
            "SHELL": "/bin/zsh", "TMPDIR": str(self.root),
            "CI": "true", "NO_COLOR": "1", "AJX_DEMO_PACE": "0.01",
        }
        self.archive = self.package / f"ajx-{TARGET}.tar.gz"
        if REAL_PACKAGE:
            shutil.copytree(REAL_PACKAGE, self.package, dirs_exist_ok=True)
            manifest = json.loads((self.package / "build.json").read_text())
            self.commit, self.version = manifest["commit"], manifest["version"]
        else:
            self.commit = COMMIT
            self.version = "1.0.0"
            version = json.dumps({"implementation": "go", "build": COMMIT, "version": "1.0.0"})
            binary = "#!/bin/sh\nprintf '%s\\n' '" + version + "'\n"
            files = {
                "ajx": binary, "skill/bin/ajx": binary, "PACKAGE_FORMAT": "1\n",
                "BUILD_COMMIT": COMMIT + "\n", "skill/SKILL.md": "# AJX\n",
                "skill/references/briefing.md": "bundled reference\n",
            }
            with tarfile.open(self.archive, "w:gz") as archive:
                for name, content in files.items():
                    entry = tarfile.TarInfo(f"ajx-{TARGET}/{name}")
                    payload = content.encode()
                    entry.size = len(payload)
                    entry.mode = 0o755 if name.endswith("ajx") else 0o644
                    archive.addfile(entry, io.BytesIO(payload))
            self.checksum()

    def checksum(self):
        digest = hashlib.sha256(self.archive.read_bytes()).hexdigest()
        (self.package / "SHA256SUMS").write_text(f"{digest}  {self.archive.name}\n")

    def install(self, *args, online=False, ok=True):
        cmd = ["/bin/bash", str(ROOT / "install.sh")]
        if not online:
            cmd += ["--package-dir", str(self.package)]
        result = subprocess.run(cmd + list(args), env=self.env, cwd=self.root,
                                text=True, capture_output=True, timeout=40)
        if ok:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        return result

    def assert_not_installed(self):
        self.assertFalse((self.home / ".local/share/ajx").exists())
        self.assertFalse((self.home / ".local/bin/ajx").exists())

    def run_one_liner(self, command):
        return subprocess.run(
            ["/bin/bash", "-c", command], env=self.env, cwd=self.root,
            stdin=subprocess.DEVNULL, text=True, capture_output=True, timeout=40,
        )

    def replacement_from(self, result):
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("preserve existing files as backups", result.stderr)
        commands = [line for line in result.stderr.splitlines() if line.startswith("bash ")]
        self.assertEqual(len(commands), 1, result.stderr)
        return commands[0]

    def test_clean_install_and_update_preserve_user_config(self):
        rc = self.home / ".zshrc"
        rc.write_text("# keep my settings\n")
        installed = self.install()
        self.assertIn("Installing the AJX CLI + matching agent skills.", installed.stdout)
        self.assertIn("CLI + agent skills (3 locations)", installed.stdout)
        self.assertIn("No separate skill installation is needed.", installed.stdout)
        binary = self.home / ".local/bin/ajx"
        version = json.loads(subprocess.check_output(
            [str(binary), "version", "--json"], env=self.env, text=True))
        self.assertEqual(version["build"], self.commit)
        for host in (".agents", ".claude", ".kiro"):
            skill = self.home / host / "skills/ajx"
            self.assertTrue(skill.is_symlink())
            self.assertTrue((skill / "SKILL.md").is_file())
            self.assertTrue((skill / "references/briefing.md").is_file())
            self.assertEqual((skill / "bin/ajx").read_bytes(), binary.read_bytes())
        self.install()
        self.assertEqual(rc.read_text().count("# AJX installer"), 1)
        self.assertTrue(rc.read_text().startswith("# keep my settings\n"))
        shell = subprocess.check_output(
            ["/bin/bash", "-c", '. "$HOME/.zshrc"; command -v ajx'], env=self.env, text=True)
        self.assertEqual(shell.strip(), str(binary))
        self.assertFalse(list(self.home.rglob("*.ajx-backup-*")))

    def test_conflict_is_detected_before_installing(self):
        skill = self.home / ".agents/skills/ajx"
        skill.mkdir(parents=True)
        (skill / "SKILL.md").write_text("my existing skill")
        result = self.install(ok=False)
        self.assertIn("--replace", result.stderr)
        self.assertEqual((skill / "SKILL.md").read_text(), "my existing skill")
        self.assert_not_installed()
        replacement = self.replacement_from(result)
        self.assertNotIn("curl", replacement)
        retried = self.run_one_liner(replacement)
        self.assertEqual(retried.returncode, 0, retried.stdout + retried.stderr)
        self.assertEqual(next(skill.parent.glob("ajx.ajx-backup-*/SKILL.md")).read_text(),
                         "my existing skill")

    def test_conflict_one_liner_can_be_copied_to_replace(self):
        skill = self.home / ".agents/skills/ajx"
        skill.mkdir(parents=True)
        (skill / "SKILL.md").write_text("my existing skill")
        binary = self.home / ".local/bin/ajx"
        binary.parent.mkdir(parents=True)
        binary.write_text("old cli")
        self.fake_curl()
        failed = self.run_one_liner(INSTALL_ONE_LINER)
        replacement = self.replacement_from(failed)
        self.assertEqual(replacement, INSTALL_ONE_LINER.replace(
            "| bash'", "| bash -s -- --replace'"))
        self.assertEqual((skill / "SKILL.md").read_text(), "my existing skill")
        self.assertEqual(binary.read_text(), "old cli")
        self.assertFalse((self.home / ".local/share/ajx").exists())
        retried = self.run_one_liner(replacement)
        self.assertEqual(retried.returncode, 0, retried.stdout + retried.stderr)
        self.assertTrue(skill.is_symlink())
        self.assertTrue(binary.is_symlink())
        self.assertEqual(next(skill.parent.glob("ajx.ajx-backup-*/SKILL.md")).read_text(),
                         "my existing skill")
        self.assertEqual(next(binary.parent.glob("ajx.ajx-backup-*")).read_text(), "old cli")

    def test_conflict_retry_preserves_quoted_options(self):
        custom = self.home / "agent's $(touch SHOULD_NOT_EXIST)" / "ajx"
        custom.mkdir(parents=True)
        (custom / "SKILL.md").write_text("custom skill")
        self.fake_curl()
        failed = self.install("--version", f"v{self.version}", "--skill-dir", str(custom),
                              "--no-path", online=True, ok=False)
        replacement = self.replacement_from(failed)
        retried = self.run_one_liner(replacement)
        self.assertEqual(retried.returncode, 0, retried.stdout + retried.stderr)
        self.assertTrue(custom.is_symlink())
        self.assertEqual(next(custom.parent.glob("ajx.ajx-backup-*/SKILL.md")).read_text(),
                         "custom skill")
        self.assertFalse((self.home / ".agents").exists())
        self.assertFalse((self.home / ".zshrc").exists())
        self.assertFalse((self.root / "SHOULD_NOT_EXIST").exists())
        self.assertNotIn("export PATH", replacement)
        requests = (self.root / "requests").read_text().splitlines()
        self.assertFalse(any(url.endswith("/VERSION") for url in requests))
        self.assertTrue(all(f"/download/v{self.version}/" in url for url in requests
                            if not url.endswith("/install.sh")))

    def test_replace_backs_up_existing_directory_and_binary(self):
        skill = self.home / ".agents/skills/ajx"
        skill.mkdir(parents=True)
        (skill / "SKILL.md").write_text("my existing skill")
        binary = self.home / ".local/bin/ajx"
        binary.parent.mkdir(parents=True)
        binary.write_text("old cli")
        self.install("--replace")
        self.assertEqual(next(skill.parent.glob("ajx.ajx-backup-*/SKILL.md")).read_text(),
                         "my existing skill")
        self.assertEqual(next(binary.parent.glob("ajx.ajx-backup-*")).read_text(), "old cli")
        self.assertTrue(skill.is_symlink())
        self.assertTrue(binary.is_symlink())

    def test_cli_only_does_not_require_a_skill_or_shell_setup(self):
        self.install("--no-skills", "--no-path")
        self.assertTrue((self.home / ".local/bin/ajx").exists())
        self.assertFalse((self.home / ".agents").exists())
        self.assertFalse((self.home / ".zshrc").exists())

    def test_custom_skill_directory(self):
        custom = self.home / "another host/skills/ajx"
        self.install("--skill-dir", str(custom), "--no-path")
        self.assertTrue((custom / "bin/ajx").is_file())
        self.assertFalse((self.home / ".agents").exists())

    def test_replace_cannot_move_the_home_or_install_root(self):
        (self.home / "home-link").symlink_to(self.home, target_is_directory=True)
        for dest in (self.home, self.home / ".local/share/ajx",
                     self.home / ".local/bin/ajx", self.home / "x/../ajx",
                     self.home / "home-link/.local/share/ajx"):
            with self.subTest(dest=dest):
                self.install("--replace", "--skill-dir", str(dest), ok=False)
                self.assertTrue(self.home.is_dir())
                self.assert_not_installed()

    def test_checksum_failure_changes_nothing(self):
        with self.archive.open("ab") as archive:
            archive.write(b"corrupt")
        self.assertIn("checksum mismatch", self.install(ok=False).stderr)
        self.assert_not_installed()

    def test_wrong_platform_changes_nothing(self):
        sums = self.package / "SHA256SUMS"
        sums.write_text(sums.read_text().replace(TARGET, "unsupported-platform"))
        self.assertIn("different platform", self.install(ok=False).stderr)
        self.assert_not_installed()

    def test_archive_traversal_and_links_are_rejected(self):
        for name, symlink in ((f"ajx-{TARGET}/../escaped", False),
                              (f"ajx-{TARGET}/link", True)):
            with self.subTest(name=name), tarfile.open(self.archive, "w:gz") as archive:
                entry = tarfile.TarInfo(name)
                if symlink:
                    entry.type = tarfile.SYMTYPE
                    entry.linkname = "../escaped"
                archive.addfile(entry)
            self.checksum()
            self.install(ok=False)
            self.assertFalse((self.root / "escaped").exists())
            self.assert_not_installed()

    def fake_curl(self, version=None, missing=False, commit=None):
        # Serve public release assets through a fake curl, without credentials/network.
        commit = commit or self.commit
        version = version or f"v{self.version}"
        script = f"""#!{sys.executable}
import pathlib, shutil, sys, urllib.parse
a = sys.argv[1:]
url = next(x for x in a if x.startswith("https://"))
with open({str(self.root / 'requests')!r}, "a") as log:
    log.write(url + "\\n")
if {missing!r}:
    sys.exit(22)
name = urllib.parse.urlparse(url).path.rsplit("/", 1)[-1]
if name == "install.sh":
    sys.stdout.write(pathlib.Path({str(ROOT / "install.sh")!r}).read_text())
    sys.exit(0)
dest = pathlib.Path(a[a.index("--output") + 1])
if name == "VERSION":
    dest.write_text({version!r} + "\\n")
elif name == "BUILD_COMMIT":
    dest.write_text({commit!r} + "\\n")
elif name in ("SHA256SUMS", "ajx-{TARGET}.tar.gz"):
    shutil.copyfile(pathlib.Path({str(self.package)!r}) / name, dest)
else:
    sys.exit(22)
"""
        curl = self.fakebin / "curl"
        curl.write_text(script)
        curl.chmod(0o755)

    def test_latest_release_download_without_github_login(self):
        with (self.package / "SHA256SUMS").open("a") as sums:
            sums.write("c" * 64 + "  another-platform.tar.gz\n")
        self.fake_curl()
        self.install(online=True)
        self.assertTrue((self.home / ".local/bin/ajx").is_file())
        requests = (self.root / "requests").read_text().splitlines()
        self.assertTrue(requests[0].endswith("/latest/download/VERSION"))
        self.assertTrue(all(f"/download/v{self.version}/" in url for url in requests[1:]))

    def test_duplicate_checksum_is_rejected(self):
        sums = self.package / "SHA256SUMS"
        sums.write_text(sums.read_text() * 2)
        self.assertIn("duplicate checksum", self.install(ok=False).stderr)
        self.assert_not_installed()

    def test_pinned_release_does_not_follow_latest(self):
        self.fake_curl()
        self.install("--version", f"v{self.version}", online=True)
        self.assertNotIn("/latest/", (self.root / "requests").read_text())

    def test_invalid_version_and_wrong_commit_are_refused(self):
        self.fake_curl(version="../../bad")
        self.assertIn("Invalid release tag", self.install(online=True, ok=False).stderr)
        self.assert_not_installed()
        self.fake_curl(commit="b" * 40)
        self.assertIn("commit does not match", self.install(online=True, ok=False).stderr)
        self.assert_not_installed()

    def test_missing_release_has_actionable_error(self):
        self.fake_curl(missing=True)
        result = self.install(online=True, ok=False)
        self.assertIn("Could not find a stable AJX release", result.stderr)
        self.assert_not_installed()

    @unittest.skipUnless(REAL_PACKAGE, "real-package smoke test runs in native packaging CI")
    def test_installed_skill_runs_console_and_headless_demo(self):
        self.install("--no-path")
        binary = self.home / ".agents/skills/ajx/bin/ajx"
        trial = self.root / "demo"
        def cli(*args):
            result = subprocess.run([str(binary), *args], cwd=self.root, env=self.env,
                                    text=True, capture_output=True, timeout=60)
            self.assertEqual(result.returncode, 0, result.stderr)
            return result.stdout
        try:
            output = cli("console", "demo", str(trial), "--no-open")
            url = re.search(r"http://127\.0\.0\.1:\d+\S*", output).group(0)
            with urllib.request.urlopen(url, timeout=10) as response:
                self.assertEqual(response.status, 200)
                self.assertIn(b"AJX", response.read())
            results = self.root / "results"
            cli("ci", str(trial), "--output", str(results), "--no-synthesis")
            self.assertEqual(json.loads((results / "ci-result.json").read_text())["decision"], "passed")
            self.assertTrue(list(results.rglob("report.html")))
            self.assertTrue(list(results.rglob("pretty-journey.html")))
        finally:
            if trial.exists():
                cli("console", "stop", str(trial))


if __name__ == "__main__":
    unittest.main()
