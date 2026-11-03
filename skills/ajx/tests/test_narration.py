"""Which self-narrations can stand as the journey. A safeguard can stop the account partway and the
harness's continuation can be a refusal; a reply without the requested `# Journey` document is not an
account either. Both fall back to the labeled reconstruction instead of being labeled self-narrated."""

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "lib"))

from ajx import runner  # noqa: E402

ACCOUNT = "# Journey: convert orders\n\n## Outcome I declared\nI wrote orders.csv [E-002].\n" + "detail " * 40


class NarrationRejectionTest(unittest.TestCase):
    def test_a_complete_account_is_accepted(self):
        self.assertIsNone(runner.narration_rejection(ACCOUNT, {"harness_reported": {}}))

    def test_a_safeguard_stop_rejects_even_a_long_text(self):
        tel = {"harness_reported": {"safety_stops": 1}}
        self.assertEqual(runner.narration_rejection(ACCOUNT, tel), "safety_stop")

    def test_a_refusal_without_the_heading_is_rejected(self):
        refusal = "Understood. I won't write that account again, in any wording. " * 5
        self.assertEqual(runner.narration_rejection(refusal, {}), "no_journey_heading")

    def test_short_or_empty_text_is_rejected(self):
        self.assertEqual(runner.narration_rejection("", {}), "empty_or_short")
        self.assertEqual(runner.narration_rejection("# Journey: x\nshort", {}), "empty_or_short")

    def test_a_preamble_before_the_heading_is_dropped(self):
        text = "Here is my account.\n\n" + ACCOUNT
        self.assertIsNone(runner.narration_rejection(text, {}))
        self.assertTrue(runner.journey_text(text).startswith("# Journey: convert orders"))

    def test_every_rejection_has_a_log_reason(self):
        self.assertEqual(set(runner.NARRATION_REJECTED), {"safety_stop", "no_journey_heading", "empty_or_short"})


if __name__ == "__main__":
    unittest.main()
