"""Model choices and aliases must mean the same thing across AJX editors."""

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "lib"))

from ajx import model_catalog


class ModelCatalogTest(unittest.TestCase):
    def test_harness_specific_aliases_and_custom_ids(self):
        for harness, name, expected in (
            ("claude-code", "opus", "opus"),
            ("kiro-cli", " Opus ", "claude-opus-5.5"),
            ("copilot-cli", "haiku", "claude-haiku-5.5"),
            ("codex", "sol", "gpt-6.1-sol"),
            ("codex", "Astra", "gpt-6-astra"),
            ("codex", "terra", "gpt-6.1-terra"),
            ("codex", "luna", "gpt-6-luna"),
            ("kiro-cli", "sol", "gpt-5.6-sol"),
            ("cursor-agent", "terra", "gpt-5.6-terra"),
            ("codex", "", ""),
            ("claude-code", "gateway/MyCustomModel-v2", "gateway/MyCustomModel-v2"),
            ("custom-harness", "opus", "opus"),
        ):
            with self.subTest(harness=harness, name=name):
                self.assertEqual(model_catalog.resolve(harness, name), expected)

    def test_wrong_harness_alias_is_actionable(self):
        for harness, name in (("codex", "opus"), ("claude-code", "astra"),
                              ("gemini-cli", "sonnet"), ("kiro-cli", "astra")):
            with self.subTest(harness=harness, name=name):
                with self.assertRaisesRegex(ValueError, f"not a model alias for {harness}"):
                    model_catalog.resolve(harness, name)

    def test_catalog_integrity(self):
        for harness, entry in model_catalog.CATALOG.items():
            with self.subTest(harness=harness):
                self.assertGreaterEqual(len(entry["models"]), 5)
                self.assertEqual(len(entry["models"]), len(set(entry["models"])))
                self.assertTrue(entry["sources"])
                self.assertTrue(entry["hint"])
                for model in entry["aliases"].values():
                    self.assertIn(model, entry["models"])


if __name__ == "__main__":
    unittest.main()
