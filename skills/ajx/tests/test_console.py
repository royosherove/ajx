"""AJX Console: journal, approval, trial edits, server rules, and full runs of the simulated demo.

Everything is synthetic. The demo trial's worker is a scripted plugin (examples/console-demo); no
agent, model or network is used. Servers bind 127.0.0.1 on a free port and are stopped by each test.
"""

import contextlib
import importlib.util
import io
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
from unittest import mock
import urllib.request
from importlib.machinery import SourceFileLoader
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "lib"))

from ajx import console, envcheck, plugins, spec as specmod  # noqa: E402
from ajx.util import now, read_json, sha256_bytes, write_json  # noqa: E402

AJX = HERE.parent / "bin" / "ajx"


def _cli():
    path = str(AJX)
    spec_ = importlib.util.spec_from_file_location("ajx_cli_console", path, loader=SourceFileLoader("ajx_cli_console", path))
    cli = importlib.util.module_from_spec(spec_)
    spec_.loader.exec_module(cli)
    return cli


def _until(predicate, timeout=60, step=0.2):
    deadline = time.time() + timeout
    while time.time() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(step)
    raise AssertionError("condition not reached in time")


class DemoCase(unittest.TestCase):
    """A fresh copy of the demo trial with its workspaces under the test's temporary directory."""
    pace = "0"

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name).resolve()
        self._pace = os.environ.get("AJX_DEMO_PACE")
        os.environ["AJX_DEMO_PACE"] = self.pace
        self.trial = console.copy_demo(self.tmp / "demo")
        (self.tmp / "ws").mkdir()
        text = console.set_value(self.trial.read_text(), "trial", "workspace_root", str(self.tmp / "ws"))
        self.trial.write_text(text)
        self.procs = []

    def tearDown(self):
        for proc in self.procs:
            if proc.poll() is None:
                proc.send_signal(signal.SIGINT)
                try:
                    proc.wait(30)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
        if self._pace is None:
            os.environ.pop("AJX_DEMO_PACE", None)
        else:
            os.environ["AJX_DEMO_PACE"] = self._pace
        self._tmp.cleanup()

    def out(self):
        return console.output_dir(self.trial, console.load(self.trial)[0])

    def capture(self, proc, entry):
        self.procs.append(proc)

    def kinds(self):
        return [e["kind"] for e in console.read_entries(self.trial)[0]]


class JournalTest(DemoCase):
    def test_cursor_counts_complete_lines_only(self):
        console.record(self.trial, "user", "message", text="one")
        console.record(self.trial, "agent", "note", text="two")
        with open(console.state_dir(self.trial) / "actions.jsonl", "ab") as fh:
            fh.write(b'{"kind": "half-writ')
        entries, cursor = console.read_entries(self.trial)
        self.assertEqual((cursor, [e["n"] for e in entries]), (2, [1, 2]))
        later, _ = console.read_entries(self.trial, 1)
        self.assertEqual([e["text"] for e in later], ["two"])

    def test_wait_wakes_on_a_new_action_and_otherwise_times_out(self):
        _, cursor = console.read_entries(self.trial)
        threading.Timer(0.4, lambda: console.message(self.trial, "why did it fail?")).start()
        began = time.time()
        woke = console.wait(self.trial, cursor, timeout=20)
        self.assertEqual(woke["woke"], "journal")
        self.assertLess(time.time() - began, 10)
        self.assertEqual([m["text"] for m in woke["messages_for_agent"]], ["why did it fail?"])
        quiet = console.wait(self.trial, woke["cursor"], timeout=1)
        self.assertEqual(quiet["woke"], "timeout")
        self.assertEqual(console.presence(self.trial)["state"], "seen")

    def test_a_reply_clears_the_message_for_the_agent(self):
        entry_n = console.read_entries(self.trial)[1] + 1
        console.message(self.trial, "is it done?")
        self.assertEqual(len(console.since(self.trial)["messages_for_agent"]), 1)
        console.note(self.trial, "Yes: verified, three asks.", reply_to=entry_n)
        view = console.since(self.trial)
        self.assertEqual(view["messages_for_agent"], [])
        self.assertIn("Yes: verified, three asks.", [c["text"] for c in view["changes"]])


class PlanTest(DemoCase):
    def test_plan_covers_the_trial_file_prompt_and_starting_files(self):
        spec = console.load(self.trial)[0]
        first = console.plan_sha(self.trial, spec)
        (self.trial.parent / "fixture" / "orders.json").write_text("[]\n")
        second = console.plan_sha(self.trial, spec)
        (self.trial.parent / "task-prompt.md").write_text("Convert the orders.\n")
        third = console.plan_sha(self.trial, spec)
        self.assertEqual(len({first, second, third}), 3)

    def test_an_approval_goes_stale_when_the_task_changes(self):
        snap = console.snapshot(self.trial)
        console.record(self.trial, "user", "approved", plan=snap["plan"]["sha"], via="console")
        self.assertEqual(console.snapshot(self.trial)["plan"]["approval"]["state"], "valid")
        console.save_prompt(self.trial, "Convert orders.json to out.csv.\n", snap["trial"]["prompt"]["sha256"])
        self.assertEqual(console.snapshot(self.trial)["plan"]["approval"]["state"], "stale")

    def test_a_plan_id_prefix_is_accepted_and_a_wrong_one_explained(self):
        plan = console.snapshot(self.trial)["plan"]["sha"]
        console.preflight_start(self.trial, plan[:12])
        with self.assertRaisesRegex(console.Conflict, "plan mismatch: you passed 000000000000, the current plan is " + plan[:12]):
            console.preflight_start(self.trial, "000000000000")

    def test_the_plan_covers_trial_plugins_but_not_their_caches(self):
        first = console.plan_sha(self.trial, console.load(self.trial)[0])
        cache = self.trial.parent / "plugins" / "__pycache__"
        cache.mkdir(exist_ok=True)
        (cache / "x.pyc").write_bytes(b"cache")
        self.assertEqual(console.plan_sha(self.trial, console.load(self.trial)[0]), first)
        plugin = self.trial.parent / "plugins" / "console_demo.py"
        plugin.write_text(plugin.read_text() + "\n# edited\n")
        self.assertNotEqual(console.plan_sha(self.trial, console.load(self.trial)[0]), first)

    def test_a_start_request_nothing_followed_does_not_stay_starting(self):
        line = json.dumps({"at": "2026-01-01T10:00:00Z", "actor": "user", "kind": "start_requested"}) + "\n"
        with open(console.state_dir(self.trial) / "actions.jsonl", "a") as fh:
            fh.write(line)
        self.assertEqual(console.snapshot(self.trial)["process"]["state"], "idle")

    def test_prompt_lint_flags_hints_without_blocking(self):
        found = console.lint_prompt("This is a test for AJX; there is a known bug, so find a workaround.")
        self.assertEqual({f["why"] for f in found},
                         {"names AJX", "tells the worker it is observed", "mentions an expected problem", "suggests a workaround"})
        self.assertEqual(console.lint_prompt("Convert the three orders to CSV."), [])


class EditTest(DemoCase):
    SAMPLE = '\n'.join([
        "[trial]", 'id = "x"', "timeout_seconds = 60   # keep this note", "# max_budget_usd = 5   # example", "",
        "[[cells]]", 'id = "a"', 'harness = "fake"', "", "[[cells]]", 'id = "b"', 'harness = "fake"', '# model = "m"', "",
        "[reporter]", 'harness = "fake"', ""])

    def test_set_value_keeps_comments_and_replaces_commented_examples(self):
        text = console.set_value(self.SAMPLE, "trial", "timeout_seconds", 120)
        self.assertIn("timeout_seconds = 120  # keep this note", text)
        text = console.set_value(text, "trial", "max_budget_usd", 2.5)
        self.assertIn("max_budget_usd = 2.5\n", text)
        self.assertNotIn("# max_budget_usd", text)
        text = console.set_value(text, "cells", "model", "big-model", index=1)
        self.assertIn('id = "b"\nharness = "fake"\nmodel = "big-model"', text)
        self.assertNotIn('# model = "m"', text)
        text = console.set_value(text, "cells", "model", None, index=1)
        self.assertNotIn("big-model", text)
        text = console.set_value(text, "reporter", "model", "small")
        self.assertIn('[reporter]\nharness = "fake"\nmodel = "small"', text)

    def test_set_value_refuses_what_it_cannot_edit_safely(self):
        with self.assertRaises(console.ConsoleError):
            console.set_value('trial = { id = "x" }\n', "trial", "repetitions", 2)
        with self.assertRaises(console.ConsoleError):
            console.set_value(self.SAMPLE, "cells", "model", "m", index=5)

    def test_settings_are_validated_saved_and_conflicts_refused(self):
        base = sha256_bytes(self.trial.read_bytes())
        console.save_settings(self.trial, {"timeout_minutes": 2, "repetitions": 3}, base)
        spec = console.load(self.trial)[0]
        self.assertEqual((spec["trial"]["timeout_seconds"], spec["trial"]["repetitions"]), (120, 3))
        self.assertIn("settings_changed", self.kinds())
        with self.assertRaises(console.Conflict):
            console.save_settings(self.trial, {"repetitions": 1}, base)
        before = self.trial.read_bytes()
        with self.assertRaises(console.ConsoleError):
            console.save_settings(self.trial, {"repetitions": 0}, sha256_bytes(before))
        self.assertEqual(self.trial.read_bytes(), before)

    def test_an_edit_that_ajx_cannot_load_is_not_saved(self):
        before = self.trial.read_text()
        broken = before.replace('harness = "console-demo"\n\n[[cells]]', 'harness = "no-such-agent"\n\n[[cells]]')
        self.assertNotEqual(broken, before)
        with self.assertRaises(console.ConsoleError):
            console.save_trial_text(self.trial, broken, sha256_bytes(before.encode()))
        self.assertEqual(self.trial.read_text(), before)
        console.save_trial_text(self.trial, before.replace("timeout_seconds = 300", "timeout_seconds = 240"),
                                sha256_bytes(before.encode()))
        self.assertEqual(console.load(self.trial)[0]["trial"]["timeout_seconds"], 240)

    def test_the_task_is_locked_once_the_output_folder_holds_attempts(self):
        out = self.out()
        write_json(out / "runs" / "demo-r1" / "state.json", {"stages": {"prepare": {"status": "done"}}})
        prompt = self.trial.parent / "task-prompt.md"
        with self.assertRaisesRegex(console.ConsoleError, "fresh output folder"):
            console.save_prompt(self.trial, "Something else.\n", sha256_bytes(prompt.read_bytes()))
        result = console.fresh_output(self.trial, sha256_bytes(self.trial.read_bytes()))
        self.assertNotEqual(console.output_dir(self.trial, console.load(self.trial)[0]), out)
        self.assertTrue(result["output_dir"].startswith("ajx-reports/"))
        console.save_prompt(self.trial, "Something else.\n", sha256_bytes(prompt.read_bytes()))
        self.assertEqual(prompt.read_text(), "Something else.\n")


class SnapshotTest(DemoCase):
    def _state(self, stages, **extra):
        marks = {name: {"status": status, "at": "2026-01-01T10:00:00Z"} for name, status in stages.items()}
        write_json(self.out() / "runs" / "demo-r1" / "state.json", {"stages": marks, **extra})

    def test_a_run_in_reporting_shows_its_stage_cleanup_and_pending_asks(self):
        done = ("prepare", "execute", "verify", "teardown", "normalize", "narrate", "release")
        self._state({**{s: "done" for s in done}, "extract": "running"}, teardown={"status": "failed"})
        write_json(self.out() / "runs" / "demo-r1" / "verify.json",
                   {"outcome": "partial", "passed": 1, "total": 2, "checks": [{"name": "a", "passed": True}]})
        snap = console.snapshot(self.trial)
        run = snap["runs"][0]
        self.assertEqual((run["current"], run["asks"]["status"], run["verify"]["outcome"]), ("extract", "generating", "partial"))
        self.assertEqual([r["stages"] for r in run["recovery"]], ["teardown,render"])
        self.assertEqual((snap["output"]["started"], snap["output"]["complete"], snap["output"]["resumable"]), (1, 0, True))
        self.assertIn("fresh output folder", snap["locks"]["prompt"])

    def test_a_changed_task_cannot_resume_an_output_folder(self):
        self._state({"prepare": "done", "execute": "done", "render": "done"})
        write_json(self.out() / "matrix-plan.json", {"task_prompt_sha256": "0" * 64, "order": ["demo-r1"]})
        snap = console.snapshot(self.trial)
        self.assertEqual(snap["output"]["blocker_kind"], "changed")
        with self.assertRaisesRegex(console.ConsoleError, "different task prompt"):
            console.preflight_start(self.trial)

    def test_a_finished_output_offers_another_attempt(self):
        self._state({"prepare": "done", "execute": "done", "render": "done"})
        spec = console.load(self.trial)[0]
        write_json(self.out() / "matrix-plan.json", {"task_prompt_sha256": spec["task"]["prompt_sha256"], "order": ["demo-r1"]})
        self.assertEqual(console.snapshot(self.trial)["output"]["blocker_kind"], "complete")
        console.add_attempt(self.trial, sha256_bytes(self.trial.read_bytes()))
        snap = console.snapshot(self.trial)
        self.assertEqual((snap["output"]["blocker"], snap["output"]["planned"], snap["output"]["resumable"]), (None, 2, True))


class ServerCase(DemoCase):
    def setUp(self):
        super().setUp()
        self.httpd, self.app, self.info = console.make_server(self.trial)
        self.thread = threading.Thread(target=self.httpd.serve_forever, kwargs={"poll_interval": 0.1}, daemon=True)
        self.thread.start()

    def tearDown(self):
        _until(lambda: not self.app.checking.is_set(), 120)  # background starts finish before cleanup
        self.httpd.shutdown()
        self.httpd.server_close()
        for proc in list(self.app.children.values()):
            self.procs.append(proc)
        super().tearDown()

    def request(self, path, body=None, token=True, headers=None):
        hdrs = {"X-AJX-Token": self.info["token"]} if token else {}
        hdrs.update(headers or {})
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            hdrs["Content-Type"] = "application/json"
        req = urllib.request.Request(f"http://127.0.0.1:{self.info['port']}{path}", data=data, headers=hdrs,
                                     method="POST" if body is not None else "GET")
        try:
            with urllib.request.urlopen(req, timeout=20) as res:
                return res.status, res.headers, res.read()
        except urllib.error.HTTPError as exc:
            with exc:
                return exc.code, exc.headers, exc.read()

    def state(self):
        status, _, raw = self.request("/api/state")
        self.assertEqual(status, 200)
        return json.loads(raw)


class ServerTest(ServerCase):
    def test_actions_need_the_key_and_a_local_host_name(self):
        status, headers, _ = self.request("/", token=False)
        self.assertEqual(status, 200)
        self.assertIn("script-src 'self'", headers["Content-Security-Policy"])
        self.assertEqual(self.request("/api/state", token=False)[0], 403)
        self.assertEqual(self.request("/api/state", headers={"Host": f"attacker.example:{self.info['port']}"})[0], 403)
        self.assertEqual(self.state()["trial"]["id"], "console-demo")
        status, headers, _ = self.request("/api/session", {})
        cookie = headers["Set-Cookie"].split(";")[0]
        self.assertIn("SameSite=Lax", headers["Set-Cookie"])
        # A link opened from the page carries the cookie; an action never counts on it.
        self.assertEqual(self.request("/api/state", token=False, headers={"Cookie": cookie})[0], 200)
        self.assertEqual(self.request("/api/message", {"text": "hi"}, token=False, headers={"Cookie": cookie})[0], 403)
        self.assertEqual(self.request("/api/message", {"text": "hi"})[0], 200)

    def test_files_are_served_only_from_the_output_folder(self):
        report = self.out() / "runs" / "demo-r1" / "report.html"
        report.parent.mkdir(parents=True)
        report.write_text("<p>report</p>")
        self.assertEqual(self.request("/files/runs/demo-r1/report.html")[2], b"<p>report</p>")
        self.assertEqual(self.request("/files/%2e%2e/%2e%2e/%2e%2e/trial.toml")[0], 404)
        self.assertEqual(self.request("/files/runs/demo-r1/state.unknown")[0], 404)

    def test_reports_run_sandboxed_and_bad_requests_get_400(self):
        report = self.out() / "runs" / "demo-r1" / "report.html"
        report.parent.mkdir(parents=True)
        report.write_text("<p>report</p>")
        _, headers, _ = self.request("/files/runs/demo-r1/report.html")
        self.assertTrue(headers["Content-Security-Policy"].startswith("sandbox "))
        req = urllib.request.Request(f"http://127.0.0.1:{self.info['port']}/api/message", data=b"{not json",
                                     headers={"X-AJX-Token": self.info["token"], "Content-Type": "application/json"})
        with self.assertRaises(urllib.error.HTTPError) as caught:
            urllib.request.urlopen(req, timeout=10)
        with caught.exception as exc:
            self.assertEqual(exc.code, 400)

    def test_a_start_for_a_plan_the_user_did_not_see_is_refused(self):
        status, _, raw = self.request("/api/start", {"plan": "0" * 64})
        self.assertEqual(status, 409)
        self.assertIn("plan mismatch", json.loads(raw)["error"])
        self.assertNotIn("job_started", self.kinds())


class DemoRunTest(ServerCase):
    def test_start_in_the_console_checks_approves_runs_and_reports(self):
        plan = self.state()["plan"]["sha"]
        self.assertEqual(self.request("/api/start", {"plan": plan})[0], 202)
        snap = _until(lambda: (lambda s: s if s["process"]["state"] == "idle" and s["runs"][0]["complete"] else None)(self.state()), 120)
        run = snap["runs"][0]
        self.assertEqual(run["verify"]["outcome"], "succeeded")
        self.assertEqual((run["asks"]["status"], run["asks"]["count"]), ("ready", 3))
        self.assertEqual(run["review_status"], "ready")
        self.assertIn("report", run["links"])
        entries = console.read_entries(self.trial)[0]
        kinds = [e["kind"] for e in entries]
        for kind in ("start_requested", "checked", "approved", "job_started", "job_finished"):
            self.assertIn(kind, kinds)
        self.assertEqual(next(e for e in entries if e["kind"] == "job_finished")["exit_code"], 0)
        self.assertEqual(next(e for e in entries if e["kind"] == "approved")["plan"], plan)
        progress = [e["text"] for e in entries if e["kind"] == "progress"]
        for text in ("demo-r1: the worker finished", "demo-r1: verification succeeded (1/1 checks passed)",
                     "demo-r1: 3 ask(s) extracted", "demo-r1: report ready"):
            self.assertIn(text, progress)
        view = console.since(self.trial)
        self.assertTrue(view["headline"].startswith("1 of 1 attempts finished"), view["headline"])
        self.assertTrue(view["runs"][0]["files"]["report"].endswith("runs/demo-r1/report.html"))
        self.assertEqual(view["runs"][0]["asks"], 3)
        self.assertEqual(snap["output"]["blocker_kind"], "complete")
        exported = self.request("/api/export", {"run": "demo-r1"})
        self.assertEqual(exported[0], 200)
        self.assertEqual(len(json.loads(exported[2])["files"]), 3)


class StopTest(DemoCase):
    pace = "1"

    def _executing(self):
        run = console.snapshot(self.trial)["runs"][0]
        return run["execute"]["status"] == "running" and run["feed"]

    def test_stop_interrupts_the_worker_and_takes_the_handled_cleanup(self):
        entry = console.start(self.trial, actor="agent", via="chat", quote="Looks right, go ahead.", on_launch=self.capture)
        self.assertEqual(entry["via"], "chat")
        _until(self._executing, 60)
        self.assertEqual(console.snapshot(self.trial)["process"]["state"], "running")
        console.stop(self.trial, "user")
        with self.assertRaisesRegex(console.Conflict, "already stopping"):
            console.stop(self.trial, "user")
        self.assertEqual(self.procs[0].wait(60), 130)
        state = read_json(self.out() / "runs" / "demo-r1" / "state.json")
        self.assertEqual(state["stages"]["execute"]["status"], "interrupted")
        self.assertFalse((self.out() / ".lock").exists())
        approved = next(e for e in console.read_entries(self.trial)[0] if e["kind"] == "approved")
        self.assertEqual((approved["via"], approved["quote"]), ("chat", "Looks right, go ahead."))
        self.assertIn("stop_requested", self.kinds())

    def test_sigterm_also_takes_the_handled_cleanup(self):
        proc = subprocess.Popen([sys.executable, str(AJX), "run", str(self.trial)], stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, start_new_session=True)
        self.procs.append(proc)
        _until(self._executing, 60)
        proc.send_signal(signal.SIGTERM)
        self.assertEqual(proc.wait(60), 130)
        state = read_json(self.out() / "runs" / "demo-r1" / "state.json")
        self.assertEqual(state["stages"]["execute"]["status"], "interrupted")
        self.assertFalse((self.out() / ".lock").exists())


class ProcessIdentityTest(DemoCase):
    def _sleeper(self, *argv):
        proc = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)", *argv], start_new_session=True,
                                stderr=subprocess.DEVNULL)
        self.procs.append(proc)
        return proc

    def _journal_job(self, proc):
        console.record(self.trial, "user", "job_started", job="run", pid=proc.pid, host=__import__("socket").gethostname(),
                       command="ajx run", log="none.log")

    def test_a_reused_pid_is_never_treated_as_the_run(self):
        self._journal_job(self._sleeper("not-it"))
        self.assertEqual(console.snapshot(self.trial)["process"]["state"], "idle")
        with self.assertRaisesRegex(console.ConsoleError, "no run is in progress"):
            console.stop(self.trial, "user")

    def test_stop_is_refused_for_parallel_runs(self):
        text = console.set_value(self.trial.read_text(), "trial", "parallel", 2)
        self.trial.write_text(text)
        self._journal_job(self._sleeper("ajx", "run"))
        self.assertEqual(console.snapshot(self.trial)["process"]["state"], "running")
        with self.assertRaisesRegex(console.ConsoleError, "parallel"):
            console.stop(self.trial, "user")

    def test_ajx_run_refuses_a_plan_that_changed_after_approval(self):
        res = subprocess.run([sys.executable, str(AJX), "run", str(self.trial), "--expect-plan", "0" * 64],
                             capture_output=True, text=True, timeout=60)
        self.assertEqual(res.returncode, 2)
        self.assertIn("changed after it was approved", res.stderr)
        self.assertFalse((self.out() / "runs").exists())


class CliTest(DemoCase):
    def test_note_and_since_commands(self):
        cli = _cli()
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            self.assertEqual(cli.main(["console", "note", str(self.trial), "Reading the report now."]), 0)
        self.assertIn("posted note #1", out.getvalue())
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(cli.main(["console", "note", str(self.trial), "hi", "--reply-to", "99"]), 2)
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            self.assertEqual(cli.main(["console", "since", str(self.trial.parent)]), 0)
        view = json.loads(buf.getvalue())
        self.assertIn("Reading the report now.", [c["text"] for c in view["changes"]])
        self.assertTrue(view["headline"].startswith("Not started"))
        self.assertFalse(view["headline"].endswith(".."))

    def test_a_bare_trial_path_opens_the_console(self):
        cli = _cli()
        calls = []
        fake = lambda trial, **kw: calls.append((trial, kw)) or {"url": "http://x/", "pid": 1}  # noqa: E731
        with mock.patch.object(cli.consolemod, "open_console", fake), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(cli.main(["console", str(self.trial), "--no-open"]), 0)
        self.assertEqual(calls[0][1]["open_browser"], False)

    def test_open_reuses_the_background_server_and_stop_ends_it(self):
        first = console.open_console(self.trial, open_browser=False)
        try:
            self.assertTrue(first.get("started"))
            again = console.open_console(self.trial.parent, open_browser=False)
            self.assertEqual((again["pid"], again.get("started")), (first["pid"], None))
            self.assertEqual(console.server_info(self.trial.parent)["pid"], first["pid"])
        finally:
            self.assertTrue(console.stop_console(self.trial))
        self.assertIsNone(console.server_info(self.trial))


if __name__ == "__main__":
    unittest.main()


class RunnerFixesTest(DemoCase):
    def _run(self, *extra):
        return subprocess.run([sys.executable, str(AJX), "run", str(self.trial), *extra], capture_output=True,
                              text=True, timeout=180)

    def test_an_interrupted_report_is_finished_on_resume(self):
        self.assertEqual(self._run().returncode, 0)
        rd = self.out() / "runs" / "demo-r1"
        state = read_json(rd / "state.json")
        state["stages"]["narrate"]["status"] = "interrupted"
        for stage in ("extract", "measure", "render"):
            state["stages"].pop(stage, None)
        write_json(rd / "state.json", state)
        (rd / "journey.md").unlink()
        (rd / "asks.json").unlink()
        res = self._run()
        self.assertEqual(res.returncode, 0, res.stdout + res.stderr)
        self.assertEqual(read_json(rd / "state.json")["stages"]["narrate"]["status"], "done")
        self.assertEqual(read_json(rd / "asks.json")["extraction_status"], "ok")
        self.assertEqual(read_json(rd / "run.json")["review_status"], "ready")

    def test_extract_without_a_journey_writes_a_labeled_stub(self):
        self.assertEqual(self._run().returncode, 0)
        rd = self.out() / "runs" / "demo-r1"
        (rd / "journey.md").unlink()
        self._run("--stages", "extract,measure,render")
        self.assertTrue(read_json(rd / "asks.json")["extraction_status"].startswith("skipped: journey.md missing"))
        self.assertEqual(read_json(rd / "state.json")["stages"]["extract"]["status"], "done")

    def test_a_changed_model_cannot_join_an_output_folder_with_attempts(self):
        text = console.set_value(self.trial.read_text(), "cells", "model", "demo-model", index=0)
        self.trial.write_text(text)
        self.assertEqual(self._run().returncode, 0)
        rd = self.out() / "runs" / "demo-r1"
        self.assertEqual(read_json(rd / "run.json")["agent"]["model_requested"], "demo-model")
        text = console.set_value(self.trial.read_text(), "cells", "model", "other-model", index=0)
        self.trial.write_text(console.set_value(text, "trial", "repetitions", 2))
        res = self._run()
        self.assertEqual(res.returncode, 2)
        self.assertIn("other settings", res.stderr)
        self.assertIn("'demo-model' -> 'other-model'", res.stderr)
        self.trial.write_text(console.set_value(self.trial.read_text(), "cells", "model", "demo-model", index=0))
        self.assertEqual(self._run().returncode, 0)
        self.assertTrue((self.out() / "runs" / "demo-r2" / "run.json").is_file())


class SettingsAndErrorsTest(DemoCase):
    def test_model_names_an_agent_cannot_take_are_refused(self):
        base = sha256_bytes(self.trial.read_bytes())
        for bad in ("sonnet 5.5", "--dangerous"):
            with self.assertRaisesRegex(console.ConsoleError, "not a model name"):
                console.save_settings(self.trial, {"cell_models": {"0": bad}}, base)
        console.save_settings(self.trial, {"cell_models": {"0": "sonnet"}, "timeout_minutes": 4}, base)
        note = [e for e in console.read_entries(self.trial)[0] if e["kind"] == "settings_changed"][-1]
        self.assertIn("time limit 5 -> 4 min", note["changes"])
        self.assertIn("cell 1 model default -> sonnet", note["changes"])
        self.assertIn("sonnet", console.snapshot(self.trial)["trial"]["cells"][0]["model"])

    def test_settings_lock_once_attempts_exist_but_attempts_can_grow(self):
        write_json(self.out() / "runs" / "demo-r1" / "state.json", {"stages": {"prepare": {"status": "done"}}})
        base = sha256_bytes(self.trial.read_bytes())
        with self.assertRaisesRegex(console.ConsoleError, "locked for this output folder"):
            console.save_settings(self.trial, {"timeout_minutes": 3}, base)
        console.save_settings(self.trial, {"repetitions": 2}, base)
        self.assertIn("Locked for this output folder", console.snapshot(self.trial)["locks"]["settings"])

    def test_a_worker_that_never_started_is_named_with_its_error(self):
        rd = self.out() / "runs" / "demo-r1"
        marks = {s: {"status": "done", "at": "2026-01-01T10:00:05Z"} for s in ("prepare", "execute", "verify")}
        write_json(rd / "state.json", {"stages": marks, "execute": {
            "exit_code": 1, "stop_reason": "exit_1", "started_at": "2026-01-01T10:00:00Z",
            "stopped_at": "2026-01-01T10:00:03Z", "requested": {"harness": "console-demo", "model": "sonnet 5.5"}}})
        write_json(rd / "telemetry.execute.json", {"events": [{"kind": "text", "text": "API Error"}],
                                                   "final_text": "API Error (sonnet 5.5): 400 The provided model identifier is invalid."})
        snap = console.snapshot(self.trial)
        run = snap["runs"][0]
        self.assertTrue(run["result"]["could_not_start"])
        self.assertTrue(run["result"]["error"].startswith("API Error (sonnet 5.5)"))
        self.assertEqual(run["model"], "sonnet 5.5")
        self.assertTrue(any(e["text"].startswith("demo-r1: the worker could not start: API Error (sonnet 5.5)")
                            for e in snap["journal"]["entries"]))
        self.assertTrue(console.since(self.trial)["runs"][0]["could_not_start"])

    def test_readiness_warnings_on_an_account_task_need_acceptance(self):
        plan = console.plan_sha(self.trial, console.load(self.trial)[0])
        console._write_private(console.state_dir(self.trial) / "readiness.json", {
            "status": "ready", "plan": plan, "checked_at": now(), "steps": [], "problems": [],
            "warnings": ["an aws command without --profile acts in the model's account"], "needs_ack": True})
        with self.assertRaisesRegex(console.ConsoleError, "need your acceptance"):
            console.start(self.trial, actor="agent", via="chat", quote="go", on_launch=self.capture)
        console.start(self.trial, actor="agent", via="chat", quote="go, I accept the warning", accept_warnings=True,
                      on_launch=self.capture)
        self.assertEqual(self.procs[0].wait(120), 0)
        approved = [e for e in console.read_entries(self.trial)[0] if e["kind"] == "approved"][-1]
        self.assertEqual(approved["accepted_warnings"], ["an aws command without --profile acts in the model's account"])

    def test_icons_and_meanings_come_with_the_snapshot(self):
        vocab = console.snapshot(self.trial)["vocabulary"]
        self.assertEqual(vocab["labels"]["Misdirection"][0], "\U0001f534")
        self.assertIn("contradicted", vocab["meanings"]["Misdirection"])
        self.assertEqual(set(vocab["meanings"]), set(vocab["labels"]))


class RunAgainTest(ServerCase):
    def _complete(self, count):
        def ready():
            snap = self.state()
            done = [r for r in snap["runs"] if r["complete"]]
            return snap if snap["process"]["state"] == "idle" and len(done) >= count else None
        return _until(ready, 180)

    def test_run_again_here_adds_a_comparable_attempt_and_a_fresh_folder_starts_over(self):
        self.assertEqual(self.request("/api/start", {"plan": self.state()["plan"]["sha"]})[0], 202)
        self._complete(1)
        first = self.out()
        self.assertEqual(self.request("/api/again", {"mode": "same-folder", "plan": self.state()["plan"]["sha"]})[0], 202)
        snap = self._complete(2)
        self.assertEqual([r["id"] for r in snap["runs"] if r["complete"]], ["demo-r1", "demo-r2"])
        self.assertEqual(self.request("/api/again", {"mode": "fresh-folder", "plan": self.state()["plan"]["sha"]})[0], 202)
        snap = _until(lambda: (lambda s: s if s["output"]["dir"] != str(first) and s["process"]["state"] == "idle"
                               and sum(r["complete"] for r in s["runs"]) == 2 else None)(self.state()), 180)
        self.assertTrue(all(r["verify"]["outcome"] == "succeeded" for r in snap["runs"]))
        kinds = self.kinds()
        self.assertEqual(kinds.count("run_again"), 2)
        self.assertEqual(kinds.count("approved"), 3)

    def test_restart_stops_the_run_then_starts_a_new_attempt(self):
        os.environ["AJX_DEMO_PACE"] = "1"
        self.request("/api/start", {"plan": self.state()["plan"]["sha"]})
        _until(lambda: self.state()["runs"][0]["feed"], 60)
        os.environ["AJX_DEMO_PACE"] = "0"
        self.assertEqual(self.request("/api/restart", {"mode": "same-folder"})[0], 202)
        snap = self._complete(2)
        r1, r2 = snap["runs"][0], snap["runs"][1]
        self.assertTrue(r1["execute"]["interrupted"])
        self.assertEqual(r2["verify"]["outcome"], "succeeded")
        kinds = self.kinds()
        for kind in ("restart_requested", "stop_requested", "run_again"):
            self.assertIn(kind, kinds)

    def test_shutdown_stops_the_server_but_not_runs(self):
        status, _, raw = self.request("/api/shutdown", {})
        self.assertEqual(status, 200)
        self.assertIn("ajx console open", json.loads(raw)["reopen"])
        _until(lambda: not self.thread.is_alive(), 10)


class ConsoleListTest(DemoCase):
    def test_list_shows_a_running_console_and_stop_all_stops_it(self):
        info = console.open_console(self.trial, open_browser=False)
        found = [c for c in console.list_consoles() if c["pid"] == info["pid"]]
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0]["url"], info["url"])
        with mock.patch.object(console, "list_consoles", return_value=found):  # never touch other consoles
            self.assertEqual(console.stop_all(), 1)
        self.assertIsNone(console.server_info(self.trial))


class ShellStartupTest(DemoCase):
    @unittest.skipUnless(shutil.which("zsh"), "zsh not installed")
    def test_skip_keeps_zshenv_out_of_the_tool_shell(self):
        home = self.tmp / "home"
        home.mkdir()
        (home / ".zshenv").write_text("export AWS_PROFILE=from-zshenv\n")
        env = {"HOME": str(home), "PATH": os.environ.get("PATH", ""), "AWS_PROFILE": "from-trial"}
        zsh = shutil.which("zsh")
        self.assertEqual(envcheck.shell_env(zsh, env, self.tmp)["AWS_PROFILE"], "from-zshenv")
        self.assertEqual(envcheck.startup_source("AWS_PROFILE", env), "~/.zshenv line 1")
        quiet = self.tmp / "quiet"
        quiet.mkdir()
        self.assertEqual(envcheck.shell_env(zsh, {**env, "ZDOTDIR": str(quiet)}, self.tmp)["AWS_PROFILE"], "from-trial")

    def test_skip_gives_runs_and_doctor_an_empty_zdotdir(self):
        self.trial.write_text(console.set_value(self.trial.read_text(), "trial", "shell_startup", "skip"))
        spec = specmod.load(self.trial)
        cell = spec["cells"][0]
        auth = specmod.auth_for(spec, cell)
        ctx = envcheck.placeholder_ctx(spec, cell, auth, specmod.runner_for(spec, cell), self.tmp / "doctor")
        self.assertEqual(list(Path(ctx["env"]["ZDOTDIR"]).iterdir()), [])
        self.assertIn("BASH_ENV", ctx["unset_env"])
        self.trial.write_text(console.set_value(self.trial.read_text(), "trial", "shell_startup", "maybe"))
        with self.assertRaises(specmod.SpecError):
            specmod.load(self.trial)
