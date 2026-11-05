"""The public release must be a complete, verified set from a single commit."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "publish_release", Path(__file__).with_name("publish_release.py")
)
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class MainBuildTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ajx-main-build-test-")
        self.addCleanup(self.temp.cleanup)
        self.packages = Path(self.temp.name) / "packages"
        self.commit = "a" * 40
        self.run = {
            "id": 12, "run_attempt": 1, "head_sha": self.commit, "head_branch": "main",
            "event": "push", "path": ".github/workflows/ci.yml",
            "status": "completed", "conclusion": "success",
            "repository": {"id": 7, "full_name": release.REPOSITORY},
            "head_repository": {"id": 7, "full_name": release.REPOSITORY},
        }
        self.inventory = {
            "total_count": 4,
            "artifacts": [
                {
                    "id": index, "name": f"ajx-{target}", "expired": False,
                    "workflow_run": {
                        "id": self.run["id"], "head_sha": self.commit, "head_branch": "main",
                        "repository_id": 7, "head_repository_id": 7,
                    },
                }
                for index, target in enumerate(release.TARGETS, start=1)
            ],
        }

    def download(self, responses):
        with mock.patch.object(release, "github_json", side_effect=responses), \
                mock.patch.object(release.subprocess, "run") as command:
            selected = release.download_main_packages(self.packages, self.commit)
        return selected, command

    def test_downloads_only_the_four_packages_from_exact_successful_main_build(self):
        selected, command = self.download([
            {"workflow_runs": [self.run]}, self.inventory, self.run, self.inventory,
        ])
        self.assertEqual(selected, self.run["id"])
        args = command.call_args.args[0]
        self.assertEqual(args[:4], ["gh", "run", "download", str(self.run["id"])])
        self.assertEqual(args[args.index("--repo") + 1], release.REPOSITORY)
        self.assertEqual(
            [args[i + 1] for i, arg in enumerate(args) if arg == "--name"],
            [f"ajx-{target}" for target in release.TARGETS],
        )
        self.assertTrue(command.call_args.kwargs["check"])

    def test_manual_successful_main_rebuild_is_eligible(self):
        self.run["event"] = "workflow_dispatch"
        selected, _ = self.download([
            {"workflow_runs": [self.run]}, self.inventory, self.run, self.inventory,
        ])
        self.assertEqual(selected, self.run["id"])

    def test_untrusted_or_unfinished_runs_never_download(self):
        invalid = [
            ("head_sha", "b" * 40), ("head_branch", "feature/example"),
            ("event", "pull_request"), ("event", "pull_request_target"),
            ("path", ".github/workflows/other.yml"),
            ("status", "in_progress"), ("conclusion", "failure"),
            ("conclusion", "cancelled"), ("id", "12"), ("run_attempt", 0),
            ("repository", {"id": 8, "full_name": "example/other"}),
            ("head_repository", {"id": 8, "full_name": "example/fork"}),
        ]
        for field, value in invalid:
            with self.subTest(field=field, value=value):
                run = {**self.run, field: value}
                with mock.patch.object(release, "github_json",
                                       return_value={"workflow_runs": [run]}), \
                        mock.patch.object(release.subprocess, "run") as command:
                    with self.assertRaisesRegex(ValueError, "No successful main CI"):
                        release.download_main_packages(self.packages, self.commit)
                    command.assert_not_called()

    def test_missing_main_build_fails_with_recovery_instructions(self):
        with mock.patch.object(release, "github_json", return_value={"workflow_runs": []}):
            with self.assertRaisesRegex(ValueError, "Rerun main CI"):
                release.download_main_packages(self.packages, self.commit)
        self.assertFalse(self.packages.exists())

    def test_incomplete_expired_or_untrusted_artifacts_never_download(self):
        invalid = []
        missing = copy.deepcopy(self.inventory)
        missing["artifacts"].pop()
        missing["total_count"] = 3
        invalid.append(missing)
        duplicate = copy.deepcopy(self.inventory)
        duplicate["artifacts"][0] = copy.deepcopy(duplicate["artifacts"][1])
        invalid.append(duplicate)
        for field, value in (("expired", True), ("name", "unexpected"), ("id", "1")):
            inventory = copy.deepcopy(self.inventory)
            inventory["artifacts"][0][field] = value
            invalid.append(inventory)
        for field, value in (
            ("id", 99), ("head_sha", "b" * 40), ("head_branch", "feature/example"),
            ("repository_id", 8), ("head_repository_id", 8),
        ):
            inventory = copy.deepcopy(self.inventory)
            inventory["artifacts"][0]["workflow_run"][field] = value
            invalid.append(inventory)
        invalid.append({**self.inventory, "total_count": 101})
        for index, inventory in enumerate(invalid):
            with self.subTest(case=index):
                with mock.patch.object(release, "github_json", side_effect=[
                    {"workflow_runs": [self.run]}, inventory,
                ]), mock.patch.object(release.subprocess, "run") as command:
                    with self.assertRaisesRegex(ValueError, "No successful main CI"):
                        release.download_main_packages(self.packages, self.commit)
                    command.assert_not_called()

    def test_expired_newer_run_does_not_hide_an_available_successful_build(self):
        newer = {**self.run, "id": 13}
        expired = copy.deepcopy(self.inventory)
        for artifact in expired["artifacts"]:
            artifact["expired"] = True
            artifact["workflow_run"]["id"] = newer["id"]
        selected, _ = self.download([
            {"workflow_runs": [self.run, newer]}, expired, self.inventory,
            self.run, self.inventory,
        ])
        self.assertEqual(selected, self.run["id"])

    def test_rerun_or_artifact_replacement_during_download_blocks_publication(self):
        changed_artifacts = copy.deepcopy(self.inventory)
        changed_artifacts["artifacts"][0]["id"] = 99
        changed = [
            ({**self.run, "run_attempt": 2}, self.inventory),
            ({**self.run, "status": "in_progress"}, self.inventory),
            (self.run, changed_artifacts),
        ]
        for run, inventory in changed:
            with self.subTest(run=run, inventory=inventory):
                with self.assertRaisesRegex(ValueError, "changed during download"):
                    self.download([
                        {"workflow_runs": [self.run]}, self.inventory, run, inventory,
                    ])

    def test_invalid_commit_and_existing_files_fail_before_network_access(self):
        with mock.patch.object(release, "github_json") as api:
            with self.assertRaisesRegex(ValueError, "full Git SHA"):
                release.download_main_packages(self.packages, "main")
            self.packages.mkdir()
            (self.packages / "keep.txt").write_text("existing file")
            with self.assertRaisesRegex(ValueError, "must be empty"):
                release.download_main_packages(self.packages, self.commit)
            api.assert_not_called()


class ReleaseTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ajx-release-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.packages = self.root / "packages"
        self.output = self.root / "release"
        self.commit = "a" * 40
        for target in release.TARGETS:
            folder = self.packages / f"ajx-{target}"
            folder.mkdir(parents=True)
            os_name, arch = target.split("-")
            name = f"ajx-{target}.tar.gz"
            payload = ("synthetic " + target).encode()
            (folder / name).write_bytes(payload)
            (folder / "SHA256SUMS").write_text(f"{hashlib.sha256(payload).hexdigest()}  {name}\n")
            (folder / "build.json").write_text(json.dumps({
                "commit": self.commit, "version": "1.0.0", "package_format": 1,
                "os": os_name, "arch": arch,
                "skill_sha256": "b" * 64,
            }))

    def prepare(self, tag="v1.0.0"):
        return release.prepare(self.packages, self.output, tag, self.commit)

    def test_complete_release_stages_anonymous_installer_assets(self):
        assets = self.prepare()
        self.assertEqual(len(assets), 9)
        self.assertEqual((self.output / "VERSION").read_text(), "v1.0.0\n")
        self.assertEqual((self.output / "BUILD_COMMIT").read_text(), self.commit + "\n")
        self.assertEqual(json.loads((self.output / "RELEASE.json").read_text()),
                         {"schema": 1, "version": "1.0.0", "commit": self.commit, "skill_sha256": "b" * 64})
        for line in (self.output / "SHA256SUMS").read_text().splitlines():
            checksum, name = line.split()
            self.assertEqual(hashlib.sha256((self.output / name).read_bytes()).hexdigest(), checksum)

    def test_version_mismatch_blocks_all_publication(self):
        with self.assertRaisesRegex(ValueError, "does not match"):
            self.prepare("v2.0.0")
        self.assertFalse(self.output.exists())

    def test_mixed_commit_blocks_all_publication(self):
        manifest = self.packages / "ajx-linux-arm64/build.json"
        data = json.loads(manifest.read_text())
        data["commit"] = "b" * 40
        manifest.write_text(json.dumps(data))
        with self.assertRaisesRegex(ValueError, "does not match"):
            self.prepare()
        self.assertFalse(self.output.exists())

    def test_damaged_package_blocks_all_publication(self):
        (self.packages / "ajx-linux-arm64/ajx-linux-arm64.tar.gz").write_bytes(b"corrupt")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            self.prepare()
        self.assertFalse(self.output.exists())

    def test_different_skill_contents_block_publication(self):
        path = self.packages / "ajx-linux-arm64/build.json"
        manifest = json.loads(path.read_text())
        manifest["skill_sha256"] = "c" * 64
        path.write_text(json.dumps(manifest))
        with self.assertRaisesRegex(ValueError, "skill content"):
            self.prepare()
        self.assertFalse(self.output.exists())

    def test_missing_platform_blocks_all_publication(self):
        (self.packages / "ajx-linux-arm64/build.json").unlink()
        with self.assertRaises(FileNotFoundError):
            self.prepare()
        self.assertFalse(self.output.exists())


if __name__ == "__main__":
    unittest.main()
