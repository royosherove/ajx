"""Crash recovery for coordinator-owned environment lifecycle records."""
import json
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
from test_environments import context, persist
from ajx import environments as envmod


class LifecycleMarkerTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.ctx = context(self.root)
        self.profile = envmod.normalize_profiles(
            {"local": {"backend": "local"}}, self.root)["local"]
        self.runner = envmod.EnvironmentRunner(self.profile)
        self.handle = persist(self.runner, self.ctx)
        self.marker = self.runner._marker_path(self.ctx, self.handle)

    def recovered(self):
        return envmod.EnvironmentRunner(self.profile)

    @staticmethod
    def interrupted_dump(data, stream):
        stream.write('{"owner":')
        stream.flush()
        raise KeyboardInterrupt("synthetic interruption during lifecycle write")

    def test_legacy_stale_temp_does_not_block_preparation_or_release(self):
        stale = self.marker.with_suffix(".tmp")
        stale.write_text('{"owner":')
        self.runner.prepare(self.ctx)
        self.assertEqual(self.runner._read_marker(self.ctx, self.handle)["status"], "prepared")
        self.assertTrue(self.recovered().release(self.ctx)["confirmed"])
        self.assertEqual(self.runner._read_marker(self.ctx, self.handle)["status"], "released")
        self.assertEqual(stale.read_text(), '{"owner":')

    def test_stale_temp_symlink_is_not_followed_or_deleted(self):
        outside = self.root / "untouched"
        outside.write_text("synthetic outside content")
        stale = self.marker.with_suffix(".tmp")
        stale.symlink_to(outside)
        self.runner.prepare(self.ctx)
        self.assertTrue(self.recovered().release(self.ctx)["confirmed"])
        self.assertTrue(stale.is_symlink())
        self.assertEqual(outside.read_text(), "synthetic outside content")

    def test_handled_interruption_preserves_marker_and_allows_release_retry(self):
        self.runner._write_marker(self.ctx, self.handle, "preparing", initial=True)
        original = self.marker.read_bytes()
        for status in ("prepared", "released"):
            with self.subTest(status=status):
                with mock.patch.object(envmod.json, "dump", side_effect=self.interrupted_dump):
                    with self.assertRaises(KeyboardInterrupt):
                        self.runner._write_marker(self.ctx, self.handle, status)
                self.assertEqual(self.marker.read_bytes(), original)
                self.assertEqual(list(self.marker.parent.glob(self.marker.name + ".*.tmp")), [])
        self.assertTrue(self.recovered().release(self.ctx)["confirmed"])
        with self.assertRaisesRegex(envmod.EnvironmentError, "not confirmed or has been released"):
            self.recovered().prepare(self.ctx)

    def test_interrupted_initial_write_does_not_publish_a_partial_marker(self):
        with mock.patch.object(envmod.json, "dump", side_effect=self.interrupted_dump):
            with self.assertRaises(KeyboardInterrupt):
                self.runner._write_marker(self.ctx, self.handle, "preparing", initial=True)
        self.assertFalse(self.marker.exists())
        self.assertEqual(list(self.marker.parent.glob(self.marker.name + ".*.tmp")), [])
        self.assertTrue(self.recovered().release(self.ctx)["confirmed"])
        with self.assertRaisesRegex(envmod.EnvironmentError, "preparation was not confirmed"):
            self.recovered().prepare(self.ctx)

    def test_initial_publication_does_not_replace_an_existing_marker(self):
        self.runner._write_marker(self.ctx, self.handle, "preparing", initial=True)
        original = self.marker.read_bytes()
        with self.assertRaisesRegex(envmod.EnvironmentError, "already attempted"):
            self.runner._write_marker(self.ctx, self.handle, "released", initial=True)
        self.assertEqual(self.marker.read_bytes(), original)
        self.assertEqual(stat.S_IMODE(self.marker.stat().st_mode), 0o600)
        self.assertEqual(self.marker.stat().st_nlink, 1)
        self.assertEqual(list(self.marker.parent.glob(self.marker.name + ".*.tmp")), [])

    def test_abrupt_exit_leaves_an_orphan_that_cannot_block_cleanup_retry(self):
        self.runner._write_marker(self.ctx, self.handle, "preparing", initial=True)
        original = self.marker.read_bytes()
        # Exit immediately before atomic publication: finally blocks cannot run.
        # The previous authoritative marker must still be usable by a new runner.
        script = """
import json, os, sys
sys.path.insert(0, sys.argv[1])
from ajx import environments
profile, ctx, handle = json.loads(sys.argv[2])
def crash(*args, **kwargs):
    os._exit(73)
environments.os.replace = crash
environments.EnvironmentRunner(profile)._write_marker(ctx, handle, "released")
"""
        process = subprocess.run(
            [sys.executable, "-I", "-c", script, str(HERE.parent / "lib"),
             json.dumps([self.profile, self.ctx, self.handle], default=str)],
            stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=15)
        self.assertEqual(process.returncode, 73, process.stderr)
        self.assertEqual(self.marker.read_bytes(), original)
        leftovers = list(self.marker.parent.glob(self.marker.name + ".*.tmp"))
        self.assertEqual(len(leftovers), 1)
        self.assertEqual(stat.S_IMODE(leftovers[0].stat().st_mode), 0o600)
        self.assertTrue(self.recovered().release(self.ctx)["confirmed"])
        self.assertEqual(self.runner._read_marker(self.ctx, self.handle)["status"], "released")
        self.assertTrue(leftovers[0].is_file())


if __name__ == "__main__":
    unittest.main()
