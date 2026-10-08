"""Synthetic profile reports; no worker, provider, or network calls."""

import copy
import json
import sys
import tempfile
import unittest
from html.parser import HTMLParser
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))

from ajx import render, report_ui  # noqa: E402
from ajx.util import read_json, write_json  # noqa: E402


class HTMLRecords(HTMLParser):
    def __init__(self, text):
        super().__init__()
        self.tags, self.rows = [], []
        self.row = self.cell = None
        self.feed(text)

    def handle_starttag(self, tag, attrs):
        self.tags.append((tag, dict(attrs)))
        if tag == "tr":
            self.row = []
        elif tag in ("td", "th"):
            self.cell = []

    def handle_data(self, data):
        if self.cell is not None:
            self.cell.append(data)

    def handle_endtag(self, tag):
        if tag in ("td", "th") and self.cell is not None:
            self.row.append("".join(self.cell))
            self.cell = None
        elif tag == "tr" and self.row is not None:
            self.rows.append(self.row)
            self.row = None


def environment(profile="local-observational", backend="local"):
    return {
        "profile_id": profile, "backend": backend,
        "inventory": {"os": "synthetic OS", "tools": [{"name": "sample-tool", "version": "1.0"}]},
        "capabilities": {
            "filesystem": {"declared": "workspace only", "enforcement": "unsupported", "observed": None},
            "network": {"state": "observed-only", "observed": "synthetic probe", "evidence": ["probe-1"]},
        },
        "limitations": ["Host permissions apply; directory isolation is not enforced."],
    }


def configuration(profile="product-guide", mode="selected", digest="a" * 64):
    return {
        "profile_id": profile,
        "skills": {"mode": mode, "entries": [{
            "name": "product-guide", "content_hash": digest, "discovery_scope": "attempt",
            "states": {"installed": True, "discoverable": True, "enabled": True,
                       "loaded_into_context": None, "invoked": "unknown"},
            "contributions": {"tools": ["sample-inspect"], "hashes": {"instructions": digest}},
        }] if mode == "selected" else []},
        "plugins": {"mode": "none"}, "hooks": {"mode": "none"},
        "capabilities": {"skill_discovery": {"enforcement": "enforced", "observed": None}},
        "limitations": ["The adapter does not report skill invocation events."],
    }


class ProfileReportsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.out = Path(self.tmp.name)
        self.spec = {
            "sha256": "f" * 64, "teardown": [], "reporter": {},
            "trial": {"id": "profiles", "product": "Synthetic CLI", "timeout_seconds": 60,
                      "output_dir": str(self.out), "repetitions": 2, "parallel": 1, "order_seed": 0},
            "task": {"prompt_path": "task.md", "prompt_sha256": "b" * 64, "human": "unavailable"},
        }
        self.harness = SimpleNamespace(name="synthetic", clean_supported=True)
        for patcher in (
            mock.patch.object(render, "synthesize_clusters", side_effect=AssertionError("No provider calls")),
            mock.patch("subprocess.Popen", side_effect=AssertionError("Rendering must not launch processes")),
            mock.patch("socket.create_connection", side_effect=AssertionError("No network calls")),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)

    def make_run(self, cell_id="sample", rep=1, actual=None, manifest=None, plan=None,
                 cleanup=None, references=None, state_extra=None, artifacts=True):
        rd = self.out / "runs" / f"{cell_id}-r{rep}"
        rd.mkdir(parents=True)
        cell = {"id": cell_id, "config": "clean", **(references or {})}
        state = {
            "journey_provenance": "self-narrated synthetic account",
            "stages": {"execute": {"status": "done"}, "render": {"status": "done"}},
            "execute": {"started_at": "2026-01-01T00:00:00Z", "stopped_at": "2026-01-01T00:00:02Z",
                        "stop_reason": "completed"},
            **(state_extra or {}),
        }
        snapshot = {"harness": {"version_before": "synthetic-1"}, "product_version_before": "1.0"}
        if plan is not None:
            state["environment"] = plan
        if actual is not None:
            state["environment_report"] = actual
            snapshot["execution_environment"] = actual
        if manifest is not None:
            state["agent_configuration"] = manifest
        if cleanup is not None:
            state["environment_cleanup"] = cleanup
        if artifacts:
            write_json(rd / "environment.json", snapshot)
            if manifest is not None:
                write_json(rd / "agent-configuration.json", manifest)
        write_json(rd / "state.json", state)
        write_json(rd / "verify.json", {"outcome": "succeeded", "passed": 1, "total": 1,
                                      "checks": [{"name": "synthetic output"}]})
        write_json(rd / "telemetry.execute.json", {"events": [], "usage_status": "unavailable"})
        write_json(rd / "measurements.json", {"task_summary": {
            "elapsed_seconds": 2, "output_tokens": None, "usage_status": "unavailable"}})
        write_json(rd / "asks.json", {
            "extraction_status": "ok", "asks": [{
                "id": "ASK-001", "title": "Explain the synthetic option",
                "requested_behavior": "Describe the argument.", "evidence_status": "observed_friction",
                "how_observed": "Synthetic evidence.", "consequence": "Extra lookup.",
                "priority_rationale": "Make the option discoverable.", "verification": "Read help.",
                "measured": {"tool_calls": None, "failed_tool_calls": None, "span_seconds": None,
                             "tool_seconds": None, "output_tokens": None, "basis": "unavailable"},
            }],
        })
        (rd / "journey.md").write_text("# Synthetic journey\n\nNo live task was run.\n", encoding="utf-8")
        run = render.write_run_json(self.spec, cell, rep, rd, state, self.harness, None, None)
        render.run_report(rd)
        return rd, run, state

    def matrix(self):
        return render.matrix_report(self.spec, synthesize=False, log=lambda message: None)

    def test_actual_report_and_declared_plan_remain_distinct(self):
        actual, manifest = environment("resolved-local"), configuration()
        plan = {"profile_id": "requested-local", "backend": "local", "ownership": "borrowed",
                "capabilities": {"filesystem": "enforced"}}
        rd, run, _ = self.make_run(actual=actual, manifest=manifest, plan=plan,
                                   references={"environment": "requested-local", "agent_configuration": "product-guide"})
        self.assertEqual(run["execution_environment"]["profile_id"], "resolved-local")
        self.assertEqual(run["execution_environment"]["declared_profile_id"], "requested-local")
        self.assertEqual(run["execution_environment"]["plan"]["ownership"], "borrowed")
        self.assertEqual(run["execution_environment"]["capabilities"], actual["capabilities"])
        self.assertEqual(run["execution_environment"]["inventory"], actual["inventory"])
        self.assertEqual(run["agent_configuration"]["skills"], manifest["skills"])
        for filename in ("report.html", "pretty-journey.html"):
            text = (rd / filename).read_text()
            self.assertIn("resolved-local (declared: requested-local)", text)
            self.assertIn("local (reported)", text)
            self.assertIn("unsupported", text)
            self.assertIn("observed-only", text)
            self.assertIn("not a filesystem sandbox", text)
            self.assertIn('href="environment.json"', text)
            self.assertIn('href="agent-configuration.json"', text)
        report = (rd / "report.html").read_text()
        self.assertLess(report.index('id="ASK-001"'), report.index('id="configuration"'))
        markdown = (rd / "asks.md").read_text()
        self.assertLess(markdown.index("## Prioritized register"), markdown.index("## Execution environment"))
        self.assertIn("requested-local", markdown)

    def test_unknown_use_is_not_inferred_from_enabled_or_disabled(self):
        manifest = configuration()
        entries = manifest["skills"]["entries"]
        entries += [
            {"name": "disabled", "states": {"enabled": False}},
            {"name": "observed", "states": {"loaded": True, "invoked": False}},
        ]
        rd, _, _ = self.make_run(manifest=manifest)
        for filename in ("report.html", "pretty-journey.html"):
            rows = HTMLRecords((rd / filename).read_text()).rows
            selected = next(row for row in rows if row[0] == "skills: product-guide")
            disabled = next(row for row in rows if row[0] == "skills: disabled")
            observed = next(row for row in rows if row[0] == "skills: observed")
            self.assertEqual(selected[2:], ["true", "true", "true", "unknown", "unknown"])
            self.assertEqual(disabled[2:], ["unknown", "unknown", "false", "unknown", "unknown"])
            self.assertEqual(observed[-2:], ["true", "false"])
        saved = read_json(rd / "run.json")["agent_configuration"]["skills"]["entries"]
        self.assertIsNone(saved[0]["states"]["loaded_into_context"])
        self.assertNotIn("invoked", saved[1]["states"])
        self.assertIn("unknown, not false", (rd / "asks.md").read_text())

    def test_profile_modes_backend_hashes_and_limits_survive_matrix(self):
        self.make_run("plain", actual=environment(), manifest=configuration("plain", "none"))
        self.make_run("guided", actual=environment("container-profile", "container"), manifest=configuration())
        matrix = self.matrix()
        rows = {row["run_id"]: row for row in matrix["runs"]}
        self.assertEqual(rows["plain-r1"]["agent_configuration"]["skills"]["mode"], "none")
        self.assertEqual(rows["guided-r1"]["execution_environment"]["backend"], "container")
        self.assertEqual(rows["guided-r1"]["agent_configuration"]["skills"]["entries"][0]["content_hash"], "a" * 64)
        self.assertIsNone(rows["guided-r1"]["output_tokens"])
        self.assertIn("container-profile", " ".join(matrix["consistency"]["environment_profiles"]))
        for filename in ("index.html", "matrix.md"):
            text = (self.out / filename).read_text()
            self.assertIn("product-guide", text)
            self.assertIn("container-profile", text)
            self.assertIn("unsupported", text)
            self.assertIn("does not report skill invocation events", text)
            self.assertIn("agent-configuration.json", text)
        text = (self.out / "index.html").read_text()
        self.assertLess(text.index("Explain the synthetic option"), text.index("Configuration coverage and limits"))
        self.assertLess(text.index("Asks by configuration"), text.index("Technical comparison"))

    def test_repetitions_with_same_profile_name_keep_distinct_resolved_content(self):
        for rep, digest in ((1, "a" * 64), (2, "b" * 64)):
            self.make_run("paired", rep, actual=environment(), manifest=configuration(digest=digest))
        matrix = self.matrix()
        self.assertEqual(len(matrix["cells"]), 1)
        variants = matrix["cells"][0]["agent_configurations"]
        self.assertEqual({item["skills"]["entries"][0]["content_hash"] for item in variants},
                         {"a" * 64, "b" * 64})
        self.assertIn("shared profile name", matrix["consistency"]["configuration_note"])
        self.assertIn("b" * 64, (self.out / "matrix.md").read_text())

    def test_launch_values_are_excluded_without_losing_manifest_evidence(self):
        manifest = configuration()
        manifest.update(launch={"env": {"EXAMPLE_KEY": "private-launch-value"}},
                        settings={"private": "private-settings-value"},
                        credentials={"session_token": "private-credential-value"},
                        adapter_evidence={"control": "isolated discovery", "settings": {"key": "nested-private-value"}})
        plan = {"backend": "local", "ownership": "borrowed", "env": {"KEY": "private-plan-value"}}
        before = copy.deepcopy(manifest)
        rd, run, state = self.make_run(manifest=manifest, plan=plan)
        matrix = self.matrix()
        self.assertEqual(state["agent_configuration"], before)
        self.assertEqual(run["agent_configuration"]["skills"], before["skills"])
        self.assertEqual(run["agent_configuration"]["adapter_evidence"], {"control": "isolated discovery"})
        outputs = json.dumps(matrix) + (rd / "run.json").read_text()
        outputs += (rd / "report.html").read_text() + (rd / "asks.md").read_text()
        for value in ("private-launch-value", "private-settings-value", "private-credential-value",
                      "nested-private-value", "private-plan-value"):
            self.assertNotIn(value, outputs)

    def test_untrusted_profiles_capabilities_states_and_cleanup_are_escaped(self):
        attack = '<img src=x onerror="attack()"><script>attack()</script> | [bad](javascript:bad) & "'
        actual, manifest = environment(attack, attack), configuration(attack)
        actual["capabilities"][attack] = {"declared": attack, "observed": attack}
        actual["limitations"] = [attack]
        manifest["skills"]["entries"][0].update(name=attack, content_hash=attack)
        manifest["skills"]["entries"][0]["states"]["invoked"] = attack
        manifest["limitations"] = [attack]
        rd, _, _ = self.make_run(actual=actual, manifest=manifest, cleanup={"status": "failed", "error": attack})
        self.matrix()
        for path in (rd / "report.html", rd / "pretty-journey.html", self.out / "index.html"):
            text = path.read_text()
            parsed = HTMLRecords(text)
            self.assertFalse(any(tag == "img" for tag, _ in parsed.tags))
            self.assertEqual(sum(tag == "script" for tag, _ in parsed.tags), 1)  # bundled offline controls
            self.assertFalse(any(key.startswith("on") for _, attrs in parsed.tags for key in attrs))
            self.assertIn("&lt;img", text)
            self.assertNotIn('href="javascript:', text)
            self.assertNotRegex(text, r'<(?:script|link)[^>]+(?:src|href)="https?://')
        for path in (rd / "asks.md", self.out / "matrix.md"):
            text = path.read_text()
            self.assertNotIn("<img", text)
            self.assertNotIn("[bad](javascript:bad)", text)

    def test_cleanup_failure_is_visible_even_with_verified_success(self):
        rd, run, _ = self.make_run(actual=environment(), cleanup={"status": "failed", "error": "synthetic release failure"},
                                   state_extra={"stages": {"release": {"status": "error", "error": "release failed"}}})
        matrix = self.matrix()
        self.assertEqual(run["task_outcome_verified"], "succeeded")
        self.assertEqual(run["stages"]["release"], "error")
        self.assertEqual(matrix["runs"][0]["environment_cleanup"]["status"], "failed")
        self.assertEqual(matrix["runs"][0]["verified"], "succeeded")
        for path in (rd / "report.html", rd / "pretty-journey.html", rd / "asks.md",
                     self.out / "index.html", self.out / "matrix.md"):
            self.assertIn("Environment cleanup not confirmed", path.read_text())
        self.assertIn("synthetic release failure", (rd / "report.html").read_text())
        self.assertEqual(run["teardown"]["status"], "none defined")

    def test_legacy_records_and_missing_artifacts_remain_renderable(self):
        rd, _, _ = self.make_run(artifacts=False)
        old = read_json(rd / "run.json")
        for key in ("execution_environment", "agent_configuration", "environment_cleanup"):
            old.pop(key)
        old["runner"] = {"type": "local"}
        old["artifacts"].update(environment="javascript:bad", agent_configuration="missing.json")
        write_json(rd / "run.json", old)
        render.run_report(rd)
        matrix = self.matrix()
        self.assertIsNone(matrix["runs"][0]["agent_configuration"])
        self.assertIsNone(matrix["runs"][0]["environment_cleanup"])
        for filename in ("report.html", "pretty-journey.html"):
            text = (rd / filename).read_text()
            self.assertIn("legacy runner; capabilities unknown", text)
            self.assertIn("Loaded / invoked telemetry is unknown", text)
            self.assertNotIn('href="environment.json"', text)
            self.assertNotIn('href="agent-configuration.json"', text)
            self.assertNotIn("Environment cleanup not confirmed", text)
            self.assertNotIn('href="javascript:', text)

    def test_declared_only_profile_never_becomes_capability_evidence(self):
        rd, run, _ = self.make_run(
            references={"environment": "unprepared", "agent_configuration": "requested"},
            plan={"id": "unprepared", "backend": "local", "capabilities": {"filesystem": "enforced"}},
            artifacts=False)
        self.assertNotIn("capabilities", run["execution_environment"])
        text = (rd / "report.html").read_text()
        self.assertIn("unprepared (declared only)", text)
        self.assertIn("requested (declared only)", text)
        self.assertIn("local (declared only)", text)
        self.assertIn("Capability coverage unknown", text)

    def test_state_report_and_artifact_only_records_are_supported(self):
        actual, manifest = environment(), configuration()
        rd, run, _ = self.make_run(actual=actual, manifest=manifest, artifacts=False)
        self.assertEqual(run["execution_environment"]["report_source"], "state.environment_report")
        self.assertEqual(run["execution_environment"]["capabilities"], actual["capabilities"])
        write_json(rd / "state.json", {})
        write_json(rd / "environment.json", {"execution_environment": actual,
                                          "environment_cleanup": {"status": "failed", "error": "release retry needed"}})
        write_json(rd / "agent-configuration.json", manifest)
        old = read_json(rd / "run.json")
        for key in ("execution_environment", "agent_configuration", "environment_cleanup"):
            old.pop(key)
        write_json(rd / "run.json", old)
        render.run_report(rd)
        refreshed = read_json(rd / "run.json")
        self.assertEqual(refreshed["agent_configuration"], manifest)
        self.assertEqual(refreshed["execution_environment"]["report_source"], "environment.json")
        self.assertEqual(refreshed["environment_cleanup"]["status"], "failed")
        self.assertIn("Environment cleanup not confirmed", (rd / "report.html").read_text())

    def test_boolean_capability_support_does_not_claim_enforcement(self):
        actual = environment()
        actual["capabilities"] = {"shell": True, "network": False, "filesystem": None}
        rd, _, _ = self.make_run(actual=actual)
        rows = HTMLRecords((rd / "report.html").read_text()).rows
        capabilities = [row for row in rows if row[0].startswith("Environment: ")]
        self.assertEqual(len(capabilities), 3)
        self.assertTrue(all(row[1:4] == ["unknown", "unknown", "unknown"] for row in capabilities))
        self.assertEqual({row[4] for row in capabilities}, {"true", "false", "unknown"})

    def test_current_adapter_shapes_and_uncontrolled_sources_keep_evidence(self):
        manifest = {
            "name": "adapter-profile", "boundary": "optional activation", "content_absence": "unsupported",
            "skills": {"mode": "snapshot", "entries": [
                {"id": "skill:guide", "name": "guide", "sha256": "c" * 64,
                 "installed": True, "discoverable": True, "enabled": True,
                 "loaded": "unknown", "invoked": "unknown"}]},
            "plugins": {"mode": "none", "optional_activation": "disabled", "invoked": "unknown"},
            "hooks": {"mode": "none", "optional_activation": "disabled", "invoked": "unknown"},
            "capabilities": {"version": "1.2.3", "provider_called": False,
                             "live_extension_activation_verified": False},
            "uncontrolled_sources": [
                {"id": "managed-policy", "installed": "unknown", "enabled": "unknown", "control": "unknown"}],
            "configuration_files": [{"location": "{config_dir}/settings.json", "sha256": "d" * 64}],
            "launch": {"config": {"private_option": "private-launch-config"}, "env": {}},
        }
        actual = {"id": "observed-container", "backend": "container", "capabilities": {
            "non_root": {"declared": "denied", "state": "enforced", "observations": {"uid": "1000"}}}}
        cleanup = {"status": "already_absent", "confirmed": True, "limitations": ["Expiry cause unknown."]}
        rd, run, _ = self.make_run(
            actual=actual, manifest=manifest, cleanup=cleanup,
            plan={"id": "observed-container", "backend": "container", "owner": "synthetic-owner",
                  "profile_sha256": "e" * 64})
        self.assertEqual(run["agent_configuration"]["configuration_files"], manifest["configuration_files"])
        self.assertEqual(run["execution_environment"]["plan"]["owner"], "synthetic-owner")
        text = (rd / "report.html").read_text()
        rows = HTMLRecords(text).rows
        self.assertIn("adapter-profile", text)
        self.assertIn("snapshot; guide", text)
        observed = next(row for row in rows if row[0] == "Environment: non_root")
        self.assertEqual(observed[2:4], ["enforced", '{"uid": "1000"}'])
        uncontrolled = next(row for row in rows if row[0] == "uncontrolled sources: managed-policy")
        self.assertTrue(all(value == "unknown" for value in uncontrolled[2:]))
        self.assertIn("c" * 64, text)
        self.assertIn("Expiry cause unknown", text)
        self.assertNotIn("private-launch-config", text)
        self.assertNotIn("Environment cleanup not confirmed", text)
        matrix = self.matrix()
        self.assertEqual(matrix["runs"][0]["environment_cleanup"]["status"], "already_absent")
        self.assertIn("Expiry cause unknown", (self.out / "index.html").read_text())


if __name__ == "__main__":
    unittest.main()
