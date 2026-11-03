"""The public release must be a complete, verified set from a single commit."""
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


spec = importlib.util.spec_from_file_location(
    "publish_release", Path(__file__).with_name("publish_release.py")
)
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


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
            }))

    def prepare(self, tag="v1.0.0"):
        return release.prepare(self.packages, self.output, tag, self.commit)

    def test_complete_release_stages_anonymous_installer_assets(self):
        assets = self.prepare()
        self.assertEqual(len(assets), 8)
        self.assertEqual((self.output / "VERSION").read_text(), "v1.0.0\n")
        self.assertEqual((self.output / "BUILD_COMMIT").read_text(), self.commit + "\n")
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

    def test_missing_platform_blocks_all_publication(self):
        (self.packages / "ajx-linux-arm64/build.json").unlink()
        with self.assertRaises(FileNotFoundError):
            self.prepare()
        self.assertFalse(self.output.exists())


if __name__ == "__main__":
    unittest.main()
