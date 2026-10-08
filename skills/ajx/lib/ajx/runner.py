"""Cell lifecycle orchestration. Each (cell, repetition) is one ordinary AJX run.

Stages (resumable; each writes a marker in state.json):
  prepare -> execute -> verify -> teardown -> normalize -> narrate -> release -> extract -> measure -> render -> archive

Rules:
- A stage runs once its prerequisites reached a terminal status (done, error, or interrupted);
  later stages degrade gracefully so the two primary artifacts exist even for a failed task.
- `execute` is never retried implicitly: a run whose worker crashed or was interrupted keeps
  its evidence and is marked `interrupted`; redoing the task means a new repetition.
- No worker starts unless every [[preflight]] passed in that run's prepare stage.
- Teardown runs once execute has started, including on a later invocation after a crash. Its
  stage status says it ran; state["teardown"]["status"] says whether it could have cleaned up.
- Archival moves evidence under the run dir after confirmed environment release.
  Failed release preserves the original directories for cleanup; --keep-workspace retains them too.
"""

import errno
import json
import os
import platform
import secrets
import shlex
import shutil
import socket
import stat
import subprocess
import traceback
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack
from pathlib import Path

from . import agent_configuration, envcheck, plugins, reporter, render, spec as specmod
from .evidence import assign_ids, build_digest, isolation_scan, measure_run
from .util import context_shell, kill_group, now, read_json, run_shell, sha256_bytes, skill_fingerprint, write_json

STAGES = ("prepare", "execute", "verify", "teardown", "normalize", "narrate", "release", "extract",
          "measure", "render", "archive")
PREREQS = {"execute": ("prepare",), "verify": ("execute",), "teardown": ("prepare",),
           "normalize": ("execute",), "narrate": ("normalize",), "extract": ("narrate",),
           "release": ("prepare",), "measure": ("normalize",), "render": ("measure",), "archive": ("teardown",)}
TERMINAL = ("done", "error", "interrupted")
NOT_REDOABLE = ("prepare", "execute")
NOT_REDOABLE_AFTER_ARCHIVE = ("narrate",)   # resume needs the original cwd and harness config
CREDENTIAL_FILES = ("auth.json", ".credentials.json", "credentials.json", "credentials", "token.json", "tokens.json")


class LockedError(RuntimeError):
    pass


def _pid_alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def acquire_lock(out_dir):
    """One ajx per matrix directory: a second run would mark live workers interrupted and
    archive workspaces still in use."""
    path = Path(out_dir) / ".lock"
    if path.exists():
        info = read_json(path, {}) or {}
        same_host = info.get("host") == socket.gethostname()
        if same_host and info.get("pid") and _pid_alive(int(info["pid"])):
            raise LockedError(f"another ajx (pid {info['pid']}) is running this matrix since {info.get('at')}; "
                              f"stop it or wait. Remove {path} only if that process is gone.")
        if not same_host:
            raise LockedError(f"lock held by {info.get('host')} (pid {info.get('pid')}); remove {path} if stale")
    write_json(path, {"pid": os.getpid(), "host": socket.gethostname(), "at": now()})
    return path


def release_lock(path):
    try:
        Path(path).unlink()
    except FileNotFoundError:
        pass

CACHE_VARS = ("npm_config_cache", "YARN_CACHE_FOLDER", "PNPM_STORE_DIR", "UV_CACHE_DIR",
              "PIP_CACHE_DIR", "POETRY_CACHE_DIR", "GOCACHE", "GOMODCACHE", "BUN_INSTALL_CACHE_DIR",
              "DENO_DIR", "NUGET_PACKAGES", "GRADLE_USER_HOME")

DEFAULT_PROBES = {"python": ["python3", "--version"], "node": ["node", "--version"],
                  "npm": ["npm", "--version"], "uv": ["uv", "--version"], "git": ["git", "--version"],
                  "docker": ["docker", "--version"]}

SENSITIVE_NAME_HINTS = ("KEY", "TOKEN", "SECRET", "PASSWORD", "AWS_", "GOOGLE_", "ANTHROPIC_",
                        "OPENAI_", "AZURE_", "GEMINI_", "CURSOR_", "COPILOT_", "GH_", "CLAUDE_CODE_USE")

JOURNEY_UNAVAILABLE = "unavailable"


def _probe(argv):
    if not argv or not shutil.which(argv[0]):
        return None
    try:
        out = subprocess.run(argv, capture_output=True, text=True, timeout=20)
        text = (out.stdout or out.stderr).strip()
        return text.splitlines()[0][:160] if text else None
    except Exception:  # noqa: BLE001
        return None


def _rand():
    return secrets.token_hex(5)


def _open_directory_nofollow(path):
    """Open every component relative to its pinned parent; never resolve a symlink."""
    path = Path(path)
    if not path.is_absolute() or ".." in path.parts:
        raise RuntimeError("credential roots must be absolute owned paths without parent traversal")
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_NONBLOCK
    fd = os.open(path.anchor, flags)
    try:
        for part in path.parts[1:]:
            child = os.open(part, flags, dir_fd=fd)
            os.close(fd)
            fd = child
        return fd
    except BaseException:
        os.close(fd)
        raise


def _purge_credential_tree(fd):
    """Unlink known names only inside pinned directories, including a symlink entry itself."""
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_NONBLOCK
    for name in os.listdir(fd):
        try:
            child = os.open(name, flags, dir_fd=fd)
        except FileNotFoundError:
            continue
        except OSError as exc:
            if exc.errno not in (errno.ENOTDIR, errno.ELOOP):
                raise
            if name in CREDENTIAL_FILES:
                try:
                    os.unlink(name, dir_fd=fd)
                except FileNotFoundError:
                    pass
        else:
            try:
                _purge_credential_tree(child)
            finally:
                os.close(child)


class Run:
    def __init__(self, spec, cell, rep, log):
        self.spec, self.cell, self.rep, self.log = spec, cell, rep, log
        self.run_id = f"{cell['id']}-r{rep}"
        self.out_dir = Path(spec["trial"]["output_dir"])
        self.run_dir = self.out_dir / "runs" / self.run_id
        self.run_dir.mkdir(parents=True, exist_ok=True)
        self.state_path = self.run_dir / "state.json"
        self.state = read_json(self.state_path, {"stages": {}})
        self.harness = specmod.harness_for(spec, cell["harness"])
        self.auth = specmod.auth_for(spec, cell)
        self.runner = specmod.runner_for(spec, cell)
        self.environment_profile = specmod.environment_for(spec, cell)
        self.configuration_profile = specmod.agent_configuration_for(spec, cell)
        contract = {"environment": self.environment_profile,
                    "agent_configuration": agent_configuration.resume_contract(self.configuration_profile),
                    "cell": cell, "prompt_sha256": spec["task"]["prompt_sha256"]}
        self.contract_sha256 = sha256_bytes(json.dumps(contract, sort_keys=True).encode())
        if self.state.get("environment_contract_sha256"):
            if self.state["environment_contract_sha256"] != self.contract_sha256:
                # Upgrade old fingerprints only when every original field still matches.
                legacy = {**contract, "agent_configuration": self.configuration_profile}
                legacy_sha256 = sha256_bytes(json.dumps(legacy, sort_keys=True).encode())
                if self.state["environment_contract_sha256"] != legacy_sha256:
                    raise specmod.SpecError(f"{self.run_id}: environment, agent configuration, or task changed; "
                                            "use a new trial output directory for a new comparison")
                self.state["environment_contract_sha256"] = self.contract_sha256
                self.save()
        elif self.environment_profile and self.state.get("paths"):
            raise specmod.SpecError(f"{self.run_id}: cannot add environment profiles to an existing attempt; "
                                    "use a new trial output directory")

    # ------------------------------------------------------------------ state

    def save(self):
        write_json(self.state_path, self.state)

    def status(self, stage):
        return self.state["stages"].get(stage, {}).get("status")

    def done(self, stage):
        return self.status(stage) == "done"

    def mark(self, stage, status, **extra):
        self.state["stages"][stage] = {"status": status, "at": now(), **extra}
        self.save()

    def ctx(self):
        paths = self.state.get("paths", {})
        archived = self.state.get("archived", {})
        env = specmod.worker_env(self.spec, self.cell, self.auth)
        if self.spec["trial"]["cache"] == "isolated" and paths.get("cache_dir"):
            env.update({v: str(Path(paths["cache_dir"]) / v.lower()) for v in CACHE_VARS})
        workspace = archived.get("workspace") or paths.get("workspace") or str(self.run_dir / "workspace")
        config_dir = archived.get("config_dir") or paths.get("config_dir", "")
        ctx = {
            "spec": self.spec, "cell": self.cell, "run_dir": self.run_dir, "workspace": Path(workspace),
            "config_dir": config_dir, "cache_dir": paths.get("cache_dir", ""),
            "home_dir": archived.get("home_dir") or paths.get("home_dir", ""),
            "run_token": self.state.get("run_token", ""),
            "timeout": int(self.spec["trial"]["timeout_seconds"]),
            "narrate_timeout": int(self.spec["trial"]["narrate_timeout_seconds"]),
            "prompt_text": self.spec["task"]["prompt_text"], "state": self.state, "env": env,
            "unset_env": self.auth.unset(), "runner": self.runner, "auth": self.auth,
            "check_env": {**env, "AJX_RUN_TOKEN": self.state.get("run_token", "")},
            "environment_profile": self.environment_profile,
            "agent_configuration_profile": self.configuration_profile,
        }
        if self.environment_profile and ctx["home_dir"] and not archived:
            declared_path = env.get("PATH")
            env.update(self.runner.environment_env(ctx))
            if declared_path is not None:
                env["PATH"] = declared_path
            ctx["check_env"] = {**env, "AJX_RUN_TOKEN": self.state.get("run_token", "")}
        return ctx

    def _shell_env(self, ctx):
        if self.environment_profile:
            return self.runner.child_env(ctx["check_env"], ctx["unset_env"])
        return envcheck.command_env(ctx["env"], ctx["unset_env"], ctx["run_token"])

    def _command(self, command, ctx, default_location="environment"):
        return context_shell(command["run"], ctx, timeout=command["timeout"],
                             location=command.get("location", default_location))

    # ------------------------------------------------------------------ stages

    def stage_prepare(self):
        if not self.environment_profile and self.runner.name == "local" and not self.harness.available():
            raise RuntimeError(f"harness binary {self.harness.binary!r} not found on PATH")
        root = Path(self.spec["trial"]["workspace_root"])
        root.mkdir(parents=True, exist_ok=True)
        # Unnamed random dirs: nothing in the path reveals an evaluation, a cell, or a sibling run.
        paths = {"workspace": str((root / _rand()).resolve()), "config_dir": str((root / _rand()).resolve()),
                 "cache_dir": str((root / _rand()).resolve())}
        if self.environment_profile:
            paths["home_dir"] = str((root / _rand()).resolve())
            self.state["environment_contract_sha256"] = self.contract_sha256
        self.state["paths"] = paths
        self.state["owned_paths"] = []
        self.save()
        for p in paths.values():
            Path(p).mkdir(mode=0o700, parents=True, exist_ok=False)
            self.state["owned_paths"].append(p)
            self.save()
        # A retry after a failed prepare (say, a preflight) must not reuse the first attempt's archive:
        # ctx() prefers archived paths (this worker would land inside the report dir), and a done
        # archive stage would leave these new dirs, and any copied credentials, in workspace_root.
        self.state.pop("archived", None)
        self.state["stages"].pop("archive", None)
        self.state["run_token"] = _rand()
        self.save()
        if self.spec["task"]["fixture_dir"]:
            if self.environment_profile:
                for source in Path(self.spec["task"]["fixture_dir"]).rglob("*"):
                    mode = source.lstat().st_mode
                    if not (stat.S_ISREG(mode) or stat.S_ISDIR(mode)):
                        raise RuntimeError("environment fixtures must contain only regular files and directories; "
                                           "symlinks and special files are not copied")
            shutil.copytree(self.spec["task"]["fixture_dir"], paths["workspace"], dirs_exist_ok=True)
        ctx = self.ctx()
        problems = self.auth.problems()
        if problems and self.environment_profile:
            raise RuntimeError(f"environment authentication configuration is not ready: {'; '.join(problems)}")
        if self.environment_profile:
            self.state["environment"] = self.runner.plan(ctx)
            self.save()  # Durable ownership intent precedes any backend side effect.
            ctx = self.ctx()
            self.state["environment_report"] = self.runner.prepare(ctx)
            write_json(self.run_dir / "environment.json",
                       {"execution_environment": self.state["environment_report"],
                        "agent_configuration": self.state.get("agent_configuration")})
            self.save()
        if self.configuration_profile:
            manifest = agent_configuration.prepare(self.configuration_profile, ctx, self.harness)
            self.state["agent_configuration"] = manifest
            write_json(self.run_dir / "agent-configuration.json", manifest)
            self.save()
            ctx = self.ctx()
        self.auth.prepare(ctx)
        if self.spec["preflight"]:  # same shell and env as verify/teardown: a run that cannot clean up never starts
            preflight = envcheck.run_preflight(self.spec["preflight"], ctx["workspace"], self._shell_env(ctx),
                                               stop_on_failure=True, executor=lambda cmd: self._command(cmd, ctx))
            write_json(self.run_dir / "preflight.json", preflight)
            failed = [c for c in preflight if not c["passed"]]
            if failed:
                raise RuntimeError(f"preflight {failed[0]['name']!r} failed ({envcheck.preflight_failure(failed[0])}); "
                                   "the worker was not started")
        setup_results = []
        for cmd in self.spec["setup"]:
            res = self._command(cmd, ctx)
            setup_results.append({k: res[k] for k in ("cmd", "exit_code", "timed_out", "started_at",
                                                       "stopped_at", "stdout", "stderr", "location")})
            write_json(self.run_dir / "setup.json", setup_results)
            if res["exit_code"] != 0:
                raise RuntimeError(f"setup command failed: {cmd['run']!r} exit={res['exit_code']}")
        write_json(self.run_dir / "setup.json", setup_results)
        if self.environment_profile:
            report = self.state.get("environment_report") or {}
            actual_platform = report.get("platform") or {}
            probes = {"os": " ".join(str(actual_platform.get(key) or "")
                                     for key in ("system", "release", "machine")).strip()}
            for tool in (report.get("inventory") or {}).get("tools", []):
                if tool.get("status") == "observed":
                    probes[tool["name"]] = (tool.get("stdout") or tool.get("stderr") or "").strip()[:160]
            harness_facts = self._harness_facts(ctx)
            if not harness_facts["available"]:
                raise RuntimeError(f"harness {self.harness.binary!r} is unavailable in the selected environment")
        else:
            probes = {k: _probe(v) for k, v in DEFAULT_PROBES.items()}
            probes["os"] = f"{platform.system()} {platform.release()} {platform.machine()}"
            harness_facts = {"version_before": self.harness.version(), "available": self.harness.available()}
        checks, view = envcheck.inspect_cell(self.spec, self.cell, self.harness, self.auth, self.runner,
                                             ctx["workspace"], ctx, identities=True)
        snapshot = {
            "taken_at": now(), "probes": probes,
            "harness": {"name": self.harness.name, **harness_facts, "verified_live": self.harness.verified_live,
                        "clean_supported": self.harness.clean_supported, "can_resume": self.harness.can_resume,
                        "telemetry": self.harness.telemetry},
            "product_version_before": self._product_version(ctx),
            "auth": {**self.auth.describe(), "profile": self.auth.profile, "problems": problems,
                     "identity": checks["auth_identity"],
                     **({"provider_hint": self.auth.provider_hint(view["effective"])}
                        if hasattr(self.auth, "provider_hint") else {})},
            "task_identity": checks["task_identity"],
            "default_aws_identity": checks["default_aws_identity"],
            "env_checks": envcheck.record(checks),
            "runner": self.runner.describe(),
            "worker_env_names": sorted(ctx["env"]),
            "caller_env_names": sorted(k for k in os.environ if any(h in k for h in SENSITIVE_NAME_HINTS)),
            "cache_state": "isolated empty per-run package caches" if self.spec["trial"]["cache"] == "isolated"
                           else "shared caller caches (may be warm)",
            "config": self.cell["config"],
            "execution_environment": self.state.get("environment_report"),
            "agent_configuration": self.state.get("agent_configuration"),
        }
        write_json(self.run_dir / "environment.json", snapshot)
        if problems:
            self.log(f"[{self.run_id}] auth warnings: {problems}")
        for warning in envcheck.warnings(checks, self.auth, view) + [envcheck.default_identity_warning(checks)]:
            if warning:
                self.log(f"[{self.run_id}] env warning: {warning}")
        if checks["task_identity"] and not checks["task_identity"]["ok"]:
            self.log(f"[{self.run_id}] task identity check failed in the worker's tool shell: "
                     f"{checks['task_identity']['output']}")

    def _product_version(self, ctx):
        cmd = self.spec["trial"].get("product_version_cmd")
        if not cmd:
            return None
        try:
            res = context_shell(cmd, ctx, timeout=60, location="environment")
        except RuntimeError as exc:
            return f"unavailable ({type(exc).__name__})"
        text = (res["stdout"] or res["stderr"]).strip()
        return text.splitlines()[0][:160] if res["exit_code"] == 0 and text else f"unavailable (exit {res['exit_code']})"

    def _harness_facts(self, ctx, refresh=False):
        manifest = self.state.get("agent_configuration")
        if manifest and not refresh:
            return {"available": True, "version_before": manifest.get("capabilities", {}).get("version"),
                    "checked_in": "agent environment during profile capability checks"}
        binary = manifest.get("capabilities", {}).get("binary") if manifest else self.harness.binary
        if not binary:
            return {"available": False, "version_before": None}
        available = context_shell(f"command -v {shlex.quote(binary)}", ctx, timeout=20, location="environment")
        version = None
        if available["exit_code"] == 0 and self.harness.version_argv:
            argv = [binary, *self.harness.version_argv[1:]]
            result = context_shell(shlex.join(argv), ctx, timeout=20, location="environment")
            text = (result["stdout"] or result["stderr"]).strip()
            version = text.splitlines()[0][:160] if result["exit_code"] == 0 and text else None
        return {"available": available["exit_code"] == 0, "version_before": version,
                "checked_in": "agent environment"}

    def stage_execute(self):
        ctx = self.ctx()
        self.log(f"[{self.run_id}] executing task with {self.harness.name} model={self.cell.get('model')}")
        # Record what we can before launching so a crash mid-run still leaves a findable session.
        planned = self.harness.plan(ctx) or {}
        self.state["execute"] = {**planned, "started_at": now(), "stopped_at": None, "stop_reason": "running"}
        self.save()
        proc = self.harness.execute(self.ctx())
        stop = "timeout" if proc["timed_out"] else ("exit" if proc["exit_code"] == 0 else f"exit_{proc['exit_code']}")
        self.state["execute"] = {**self.state["execute"], **proc, "stop_reason": stop}
        self.save()

    def stage_verify(self):
        ctx = self.ctx()
        results = []
        started = now()
        for check in self.spec["verify"]:
            plugin = plugins.get("check", check["type"])()
            try:
                res = plugin.run(check, ctx)
            except Exception as exc:  # noqa: BLE001
                res = {"passed": False, "detail": f"check crashed: {type(exc).__name__}: {exc}"}
            # A check that could not authenticate says nothing about the agent's work, pass or fail
            # (`! aws ... | grep -q i-` passes when aws has no credentials). stderr only: stdout is
            # often the product's own output (a 401 page, CloudTrail records, lint codes).
            errors = [] if check.get("allow_auth_errors") else envcheck.auth_error_lines(res.get("stderr"))
            results.append({"name": check["name"], "type": check["type"], "scope": check.get("scope"),
                            "definition": {k: v for k, v in check.items() if k != "name"}, **res,
                            **({"auth_errors": errors} if errors else {})})
        passed = sum(1 for r in results if r["passed"])
        total = len(results)
        outcome = "not_run" if total == 0 else "succeeded" if passed == total else "partial" if passed else "failed"
        write_json(self.run_dir / "verify.json", {
            "window": {"start": started, "end": now()}, "passed": passed, "total": total, "outcome": outcome,
            "note": "ajx reporter verification, run after the task agent stopped; not part of the task",
            "checks_with_auth_errors": [r["name"] for r in results if r.get("auth_errors")],
            "checks": results})

    def stage_teardown(self):
        ctx = self.ctx()
        results = []
        pid = (self.state.get("execute") or {}).get("pid")
        if pid and self.status("execute") == "done" and not self.state["execute"].get("reaped"):
            kill_group(int(pid))  # servers the agent left running were kept alive for verification
            self.state["execute"]["reaped"] = True  # a redone teardown must not signal a reused pid
            self.save()
        ran = Path(ctx["workspace"]).exists()
        if ran:
            for cmd in self.spec["teardown"]:
                res = self._command(cmd, ctx)
                results.append({**{k: res[k] for k in ("cmd", "exit_code", "timed_out", "started_at", "stopped_at",
                                                        "stdout", "stderr", "location")},
                                "auth_errors": [] if cmd.get("allow_auth_errors")
                                else envcheck.auth_error_lines(res["stdout"], res["stderr"])})
        write_json(self.run_dir / "teardown.json", results)
        self.state["teardown"] = envcheck.teardown_summary(results, len(self.spec["teardown"]), ran)
        self.save()
        if self.state["teardown"]["status"] != "ok":
            self.log(f"[{self.run_id}] teardown {self.state['teardown']['status']}: resources the task created may "
                     f"still exist; see teardown.json, then `ajx run <trial> --cells {self.cell['id']} --stages teardown,render`")
        env = read_json(self.run_dir / "environment.json", {})
        if self.environment_profile:
            try:
                env["harness_version_after"] = self._harness_facts(ctx, refresh=True)["version_before"]
            except RuntimeError:
                env["harness_version_after"] = None
        else:
            env["harness_version_after"] = self.harness.version()
        env["product_version_after"] = self._product_version(ctx) if Path(ctx["workspace"]).exists() else None
        write_json(self.run_dir / "environment.json", env)

    def stage_normalize(self):
        ctx = self.ctx()
        tel = self.harness.normalize(ctx, "execute")
        assign_ids(tel)
        tel["isolation_flags"] = isolation_scan(tel, self.spec, self.state)
        if self.status("execute") != "done":
            tel["limitations"].append(f"execute stage status {self.status('execute')}: evidence may be partial")
        write_json(self.run_dir / "telemetry.execute.json", tel)
        with (self.run_dir / "events.jsonl").open("w", encoding="utf-8") as fh:
            for ev in tel["events"]:
                fh.write(json.dumps({k: v for k, v in ev.items() if k != "input_raw"}, ensure_ascii=False) + "\n")
        verify = read_json(self.run_dir / "verify.json", {})
        environment = read_json(self.run_dir / "environment.json", {})
        (self.run_dir / "digest.md").write_text(
            build_digest(self.spec, self.cell, self.state, tel, verify, environment), encoding="utf-8")
        (self.run_dir / "digest.narrator.md").write_text(
            build_digest(self.spec, self.cell, self.state, tel, verify, environment, include_verification=False),
            encoding="utf-8")

    def _write_journey(self, text, provenance):
        header = f"<!-- provenance: {provenance} -->\n"
        (self.run_dir / "journey.md").write_text(header + text.strip() + "\n", encoding="utf-8")
        self.state["journey_provenance"] = provenance
        self.save()

    def stage_narrate(self):
        ctx = self.ctx()
        tel = read_json(self.run_dir / "telemetry.execute.json")
        if self.status("execute") not in ("done", "interrupted") or tel is None:
            self._write_journey(
                f"# Journey unavailable\n\nThe task never ran to a recordable stopping point (execute stage status: "
                f"{self.status('execute')}). There is no agent experience to narrate; see state.json and digest.md.",
                JOURNEY_UNAVAILABLE)
            return
        narrator_digest = self.run_dir / "digest.narrator.md"
        digest = (narrator_digest if narrator_digest.exists() else self.run_dir / "digest.md").read_text(encoding="utf-8")
        prompt = reporter.narrate_prompt(self.spec, self.cell, self.run_id, digest)
        (self.run_dir / "narrate.prompt.md").write_text(prompt, encoding="utf-8")
        proc, text = None, ""
        if self.harness.can_resume and (self.state.get("execute") or {}).get("session_id"):
            try:
                proc = self.harness.narrate(ctx, prompt)
            except Exception as exc:  # noqa: BLE001
                self.log(f"[{self.run_id}] self-narration failed: {exc}")
                proc = None
            if proc:
                self.state["narrate"] = proc
                self.save()
                ntel = self.harness.normalize(ctx, "narrate", exclude_message_ids=tel.get("message_ids") or [])
                write_json(self.run_dir / "telemetry.narrate.json", ntel)
                text = (ntel.get("final_text") or "").strip()
        if len(text) >= 200:
            restriction = proc.get("tools_disabled") if proc else None
            restriction = "tools disabled" if restriction is True else restriction or "tool restrictions unrecorded"
            self._write_journey(text, f"self-narrated: the original task agent, resumed after the task; {restriction}")
            return
        if proc:
            self.log(f"[{self.run_id}] self-narration empty or short; falling back to a labeled reconstruction")
        try:
            result = reporter.reconstruct(self.spec, self.run_dir)
        except Exception as exc:  # noqa: BLE001
            self._write_journey(f"# Journey unavailable\n\nSelf-narration was not possible and the reporter "
                                f"reconstruction failed: {type(exc).__name__}: {exc}. The evidence digest is in digest.md.",
                                JOURNEY_UNAVAILABLE)
            raise
        self.state["narrate"] = {**result["proc"], "reconstruction": True}
        if result["text"].strip():
            self._write_journey(result["text"],
                                "EDITORIAL RECONSTRUCTION by the reporter agent from recorded evidence; the original "
                                "agent could not narrate (harness cannot resume or resume failed). No recollection is claimed.")
        else:
            self._write_journey("# Journey unavailable\n\nThe reporter returned no text.", JOURNEY_UNAVAILABLE)

    def stage_extract(self):
        if self.state.get("journey_provenance") == JOURNEY_UNAVAILABLE:
            reporter.write_asks_stub(self.run_dir, "skipped: no journey to extract from")
            return
        result = reporter.extract(self.spec, self.run_dir)
        self.state["extract"] = result["proc"]
        self.save()

    def stage_release(self):
        """Release the worker after narration; reporting consumes exported evidence."""
        if not self.environment_profile or not self.state.get("environment"):
            return
        result = self.runner.release(self.ctx())
        self.state["environment_cleanup"] = result
        self.save()
        environment = read_json(self.run_dir / "environment.json", {})
        environment["environment_cleanup"] = result
        write_json(self.run_dir / "environment.json", environment)
        if result.get("confirmed") is not True:
            raise RuntimeError(f"environment cleanup was not confirmed: {result.get('status', 'unknown')}")

    def stage_measure(self):
        measure_run(self.spec, self.cell, self.run_dir, self.state)

    def stage_render(self):
        render.write_run_json(self.spec, self.cell, self.rep, self.run_dir, self.state, self.harness, self.auth,
                              self.runner)
        render.run_report(self.run_dir)

    def _purge_credentials(self):
        """Purge known files through owned directory fds; defer hooks until confirmed release."""
        paths = self.state.get("paths", {})
        archived = self.state.get("archived") or {}
        owned = self.state.get("owned_paths")
        confirmed = (not self.environment_profile or not self.state.get("environment")
                     or (self.state.get("environment_cleanup") or {}).get("confirmed") is True)
        with ExitStack() as descriptors:
            roots, errors = {}, []
            for key in ("config_dir", "home_dir"):
                original = paths.get(key)
                if not original or (owned is not None and original not in owned):
                    continue
                root = archived.get(key) or original
                try:
                    fd = _open_directory_nofollow(root)
                except FileNotFoundError:
                    continue
                except OSError as exc:
                    errors.append(f"{key}: {type(exc).__name__}")
                else:
                    descriptors.callback(os.close, fd)
                    roots[key] = (root, fd)
            try:
                # Hooks take filesystem paths, not fds. Never give a still-active worker
                # the opportunity to redirect those paths during unconfirmed release.
                if confirmed and "config_dir" in roots and not errors:
                    ctx = self.ctx()
                    ctx["config_dir"] = roots["config_dir"][0]
                    ctx["home_dir"] = roots["home_dir"][0] if "home_dir" in roots else ""
                    try:
                        self.auth.cleanup(ctx)
                    except Exception as exc:  # noqa: BLE001
                        self.log(f"[{self.run_id}] auth cleanup failed: {exc}")
            finally:
                # Also runs if an auth hook is interrupted; descriptors stay pinned if a
                # worker replaces a root, ancestor, or descendant with a symlink.
                for key, (_, fd) in roots.items():
                    try:
                        _purge_credential_tree(fd)
                    except OSError as exc:
                        errors.append(f"{key}: {type(exc).__name__}")
            if errors:
                raise RuntimeError(f"credential purge could not safely complete: {'; '.join(errors)}")

    def stage_archive(self):
        """Move the workspace and harness config under the run dir so later workers cannot discover
        them; purge copied credentials; drop package caches; repoint evidence paths."""
        paths = self.state.get("paths", {})
        moved = dict(self.state.get("archived") or {})
        # A confirmed release is durable; archiving no longer needs the backend to be available.
        cleanup = self.state.get("environment_cleanup") or {}
        try:
            if self.environment_profile and self.state.get("environment") and cleanup.get("confirmed") is not True:
                self.stage_release()
        except BaseException as exc:
            # Keep the original ownership and paths for retry, but never retain known
            # staged credentials just because the backend cannot confirm release.
            try:
                self._purge_credentials()
            except BaseException as cleanup_exc:
                exc.add_note(f"Credential purge also failed: {type(cleanup_exc).__name__}")
            raise
        self._purge_credentials()
        for key, dest in (("workspace", "workspace"), ("config_dir", "harness-config"), ("home_dir", "agent-home")):
            src = paths.get(key)
            owned = self.state.get("owned_paths")
            if src and (owned is None or src in owned) and Path(src).exists():
                target = self.run_dir / dest
                if target.exists():
                    shutil.rmtree(target)
                shutil.move(src, target)
                moved[key] = str(target)
                self.state["archived"] = dict(moved)
                self.save()  # Keep moved evidence findable even if a later cleanup step fails.
        if paths.get("cache_dir") and Path(paths["cache_dir"]).exists() \
                and (self.state.get("owned_paths") is None or paths["cache_dir"] in self.state["owned_paths"]):
            shutil.rmtree(paths["cache_dir"], ignore_errors=True)
        host_context = self.run_dir / "host-environment"
        if self.environment_profile and host_context.exists():
            if host_context.is_symlink():
                raise RuntimeError("host check context was replaced by a symlink; refusing to archive it")
            shutil.rmtree(host_context)
        self.state["archived"] = moved
        self.save()
        # evidence pointers recorded before the move
        for name in ("telemetry.execute.json", "telemetry.narrate.json", "run.json"):
            data = read_json(self.run_dir / name)
            if not data:
                continue
            changed = False
            for holder, key in ((data, "transcript_path"), (data.get("evidence") or {}, "transcript_path")):
                old = holder.get(key)
                if old and paths.get("config_dir") and old.startswith(paths["config_dir"]) and moved.get("config_dir"):
                    holder[key] = moved["config_dir"] + old[len(paths["config_dir"]):]
                    changed = True
            if changed:
                write_json(self.run_dir / name, data)

    # ------------------------------------------------------------------ driver

    def _cleanup_interrupted(self, stage, keep_workspace):
        """Try each cleanup step once; keep failed release recoverable and the interruption intact."""
        cleanup = self.state["interruption_cleanup"] = {"stage": stage, "at": now(), "steps": {}}

        def attempt(name, action):
            tracked = name in STAGES and name != stage
            try:
                if tracked:
                    self.mark(name, "running")
                action()
            except BaseException as exc:
                result = {"status": "error" if isinstance(exc, Exception) else "interrupted",
                          "error": f"{type(exc).__name__}: {exc}"}
            else:
                result = {"status": "done"}
            cleanup["steps"][name] = result
            if tracked:
                self.mark(name, **result)
            else:
                self.save()

        def stop_processes():
            execution = self.state.get("execute") or {}
            if execution.get("pid") and not execution.get("reaped"):
                kill_group(int(execution["pid"]))
                execution["reaped"] = True
                self.save()

        def released():
            return (not self.environment_profile or not self.state.get("environment")
                    or (self.state.get("environment_cleanup") or {}).get("confirmed") is True)

        try:
            # Harness.run reaps an interrupted launch and cancels container workers.
            # A completed execution can still have descendants retained for verification.
            attempt("stop_processes", stop_processes)
            if self.done("prepare") and self.status("execute") in TERMINAL \
                    and not self.done("teardown") and stage != "teardown":
                attempt("teardown", self.stage_teardown)
            if not released():
                attempt("release", self.stage_release)
            if not keep_workspace and self.state.get("paths"):
                if released():
                    attempt("archive", self.stage_archive)
                elif stage != "archive":
                    self.mark("archive", "error", error="environment release was not confirmed; "
                              "retaining original owned paths for cleanup retry")
        finally:
            if not self.done("archive"):
                # Failed release and --keep-workspace retain evidence roots, never copied auth.
                attempt("credentials", self._purge_credentials)

    def _detect_interrupted(self):
        if self.environment_profile and self.status("prepare") == "running":
            self.mark("prepare", "interrupted", note="ajx stopped during preparation; keeping the original "
                                                     "owned paths and environment for cleanup without replay")
            self.log(f"[{self.run_id}] prepare was interrupted; cleaning up the recorded attempt")
        if self.status("execute") == "running":
            ex = self.state.get("execute") or {}
            self.state["execute"] = {**ex, "stop_reason": "interrupted", "interrupted": True}
            self.mark("execute", "interrupted", note="ajx was not running when this run was resumed; the worker "
                                                     "may have been killed or finished unobserved")
            self.log(f"[{self.run_id}] execute was interrupted; keeping its evidence, not re-running the task")

    def _ready(self, stage):
        if stage == "execute":
            return self.done("prepare")
        return all(self.status(p) in TERMINAL for p in PREREQS.get(stage, ()))

    def _skip(self, stage):
        st = self.status(stage)
        if stage == "prepare" and self.environment_profile and st in TERMINAL:
            return True  # A failed owned attempt is evidence, not permission to allocate a replacement.
        return st in ("done", "interrupted") or (stage == "execute" and st == "error")

    def go(self, stages=None, keep_workspace=False):
        self._detect_interrupted()
        if stages:
            unknown = [s for s in stages if s not in STAGES]
            if unknown:
                raise ValueError(f"unknown stage(s) {unknown}; stages are {list(STAGES)}")
        if self.environment_profile and self.status("prepare") == "interrupted":
            # As with failed preparation, release before archiving and purging credentials.
            # Retry unconfirmed cleanup against the same paths, even with --stages prepare.
            if not self.done("archive"):
                if keep_workspace:
                    self._cleanup_interrupted("prepare", keep_workspace=True)
                else:
                    self._safe("archive")
            return self.state
        if stages:
            blocked = [s for s in stages if s in NOT_REDOABLE and self.status(s) in TERMINAL]
            if blocked:
                self.log(f"[{self.run_id}] refusing to redo {blocked}: a task runs once per repetition; add a "
                         f"repetition or delete {self.run_dir}")
                return self.state
            if self.state.get("archived"):
                late = [s for s in stages if s in NOT_REDOABLE_AFTER_ARCHIVE]
                if late:
                    self.log(f"[{self.run_id}] refusing to redo {late} after archive: the session can no longer be "
                             f"resumed from its original workspace; the existing journey is kept")
                    stages = [s for s in stages if s not in late]
            elif self.state.get("environment_cleanup") and "narrate" in stages:
                self.log(f"[{self.run_id}] refusing to resume narration after environment release; "
                         "keeping the existing journey")
                stages = [s for s in stages if s != "narrate"]
            for s in stages:  # explicitly requested post-processing stages are redone
                self.state["stages"].pop(s, None)
            self.save()
        wanted = [s for s in STAGES if not stages or s in stages]
        if keep_workspace and "archive" in wanted:
            wanted.remove("archive")
        if self.status("execute") in TERMINAL and not self.done("teardown") and "teardown" not in wanted:
            wanted.append("teardown")  # a crash between execute and teardown must still tear down
        for stage in STAGES:
            if stage not in wanted or self._skip(stage):
                continue
            if stage == "archive" and not self.state.get("paths"):
                continue
            if not self._ready(stage):
                self.log(f"[{self.run_id}] skip {stage}: prerequisites {PREREQS.get(stage)} not reached")
                continue
            self.mark(stage, "running")
            try:
                getattr(self, f"stage_{stage}")()
                self.mark(stage, "done")
            except Exception as exc:  # noqa: BLE001
                self.mark(stage, "error", error=f"{type(exc).__name__}: {exc}", trace=traceback.format_exc()[-3000:])
                self.log(f"[{self.run_id}] {stage} failed: {exc}")
                if stage == "prepare":
                    self._safe("archive")
                    break
            except BaseException as exc:
                try:
                    if stage == "execute":
                        execution = self.state.get("execute") or {}
                        self.state["execute"] = {**execution, "stop_reason": "interrupted", "interrupted": True,
                                                 "stopped_at": execution.get("stopped_at") or now()}
                    try:
                        self.mark(stage, "interrupted", note="execution interrupted; keeping captured evidence")
                    finally:
                        self._cleanup_interrupted(stage, keep_workspace)
                except BaseException as cleanup_exc:
                    exc.add_note(f"AJX interruption cleanup also failed: {type(cleanup_exc).__name__}")
                raise
        return self.state

    def _safe(self, stage):
        try:
            self.mark(stage, "running")
            getattr(self, f"stage_{stage}")()
            self.mark(stage, "done")
        except Exception as exc:  # noqa: BLE001
            self.mark(stage, "error", error=str(exc))


def run_matrix(spec, only_cells=None, stages=None, keep_workspace=False, log=print, dry_run=False, synthesize=True):
    out = Path(spec["trial"]["output_dir"])
    out.mkdir(parents=True, exist_ok=True)
    unknown = [s for s in (stages or []) if s not in STAGES]
    if unknown:
        raise specmod.SpecError(f"unknown stage(s) {unknown}; stages are {list(STAGES)}")
    plan = specmod.run_plan(spec, only_cells)
    parallel = int(spec["trial"]["parallel"])
    lock = acquire_lock(out)
    try:
        runs = [] if dry_run else [Run(spec, c, r, log) for c, r in plan]
        meta = read_json(out / "matrix-plan.json", {}) or {}
        meta.update({
            "trial_id": spec["trial"]["id"], "product": spec["trial"]["product"], "trial_file": spec["path"],
            "trial_sha256": spec["sha256"], "task_prompt_sha256": spec["task"]["prompt_sha256"],
            "skill_revision": skill_fingerprint(), "order_seed": spec["trial"]["order_seed"],
            "order": [f"{c['id']}-r{r}" for c, r in plan], "parallel": parallel,
            "wall_clock_comparable": parallel == 1, "planned_at": now(),
            "configurations": [{
                "cell": c["id"], "environment": c.get("environment"),
                "agent_configuration": c.get("agent_configuration"),
                "environment_profile_sha256": sha256_bytes(json.dumps(
                    specmod.environment_for(spec, c), sort_keys=True).encode())
                    if specmod.environment_for(spec, c) else None,
                "agent_configuration_sha256": sha256_bytes(json.dumps(
                    specmod.agent_configuration_for(spec, c), sort_keys=True).encode())
                    if specmod.agent_configuration_for(spec, c) else None,
            } for c in spec["cells"]],
        })
        write_json(out / "matrix-plan.json", meta)
        shutil.copy2(spec["task"]["prompt_path"], out / "task-prompt.md")
        if dry_run:
            return meta
        if parallel == 1:
            for run in runs:
                run.go(stages, keep_workspace)
        else:
            with ThreadPoolExecutor(max_workers=parallel) as pool:
                list(pool.map(lambda r: r.go(stages, keep_workspace), runs))
        render.matrix_report(spec, synthesize=synthesize, log=log)
    finally:
        release_lock(lock)
    return meta
