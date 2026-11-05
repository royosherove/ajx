"""Progress must reach callers before reporting work and reflect saved state."""

import tempfile
import unittest
from unittest import mock

import test_reporting_lifecycle as fixtures


class StageProgressTest(unittest.TestCase):
    def test_reports_work_start_and_failure_without_losing_cleanup(self):
        with tempfile.TemporaryDirectory() as tmp:
            run = fixtures.ReportingLifecycleTest().make_run(tmp)
            logs = []
            run.log = logs.append

            def fail_extract():
                self.assertTrue(logs[-1].endswith("stage extract: running"))
                state = fixtures.read_json(run.state_path)
                self.assertEqual(state["stages"]["extract"]["status"], "running")
                raise RuntimeError("synthetic reporting failure")

            run.stage_extract = fail_extract
            run.go()
            output = "\n".join(logs)
            for message in ("stage extract: error", "stage render: running",
                            "stage render: done", "stage archive: done"):
                self.assertIn(message, output)
            self.assertNotIn("stage extract: done", output)

    def test_does_not_claim_an_unsaved_transition(self):
        with tempfile.TemporaryDirectory() as tmp:
            run = fixtures.ReportingLifecycleTest().make_run(tmp)
            logs = []
            run.log = logs.append
            with mock.patch.object(run, "save", side_effect=OSError("synthetic write failure")):
                with self.assertRaises(OSError):
                    run.mark("extract", "done")
            self.assertEqual(logs, [])

    def test_closed_output_does_not_fail_a_saved_transition(self):
        with tempfile.TemporaryDirectory() as tmp:
            run = fixtures.ReportingLifecycleTest().make_run(tmp)
            run.log = mock.Mock(side_effect=BrokenPipeError("output closed"))
            run.mark("release", "done")
            self.assertEqual(fixtures.read_json(run.state_path)["stages"]["release"]["status"], "done")


if __name__ == "__main__":
    unittest.main()
