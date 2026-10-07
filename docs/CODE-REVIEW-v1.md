# Code review of the first build (2026-10-06)

Two passes after the initial implementation: a line-by-line self review at full reasoning effort, and an
independent adversarial review by a separate agent with no authoring context. Both ran the test suite and
probed behavior with throwaway scripts; neither modified the repo. The fixes
described below are included in the GitHub source snapshot unless marked otherwise.
This is a historical review record, not a certification of the current release.

## Self review (fixes included)

| # | Defect | Fix |
|---|---|---|
| S1 | Auth `args` values (e.g. `--api-key ${KEY}`) written verbatim into `state.json`/`run.json` via recorded argv | `Harness.safe_argv`: prompt marker, auth env values and non-flag auth args redacted |
| S2 | Reporter structured-output failure left `asks.md` indistinguishable from a clean run | `extraction_status` recorded, rendered as a reporting failure, forces `review_status: incomplete` |
| S3 | `--no-synthesis` ignored by `run_matrix`; matrix rendered twice | flag plumbed through; single render |
| S4 | Session id saved only after the worker exited, so a crash lost the transcript pointer | `Harness.plan()` saved before launch |
| S5 | Resume re-executed an interrupted worker in the dirty workspace; `--stages execute` after archive used a moved cwd | explicit prerequisites; `interrupted` status; `prepare`/`execute` never redone |
| S6 | Failed `prepare` leaked random dirs in `workspace_root`; no `journey.md` when narration and reconstruction both failed | archive on failure; stub artifacts without LLM spend |
| S7 | `str.format` on user argv templates broke on literal `{` | `util.fill` replaces known placeholders only |
| S8 | stdin written synchronously (deadlock, `BrokenPipeError`); stdout loop blocked on grandchildren | threaded feeder/reader with grace and reap |
| S9 | Unbounded digest; `narrate_timeout_seconds` unused; isolation needles not scrubbed; provenance comment rendered literally; fingerprint included example outputs; unknown `--cells` ran nothing | all fixed |

## Independent review (fixes included)

| # | Finding | Disposition |
|---|---|---|
| 1 | Codex `auth.json` copied for isolated runs was archived into `runs/<run>/harness-config/` | `Auth.cleanup()` hook plus purge of known credential filenames before archive; test |
| 2 | No run lock: a second `ajx2 run` marked live workers interrupted and archived workspaces in use | `.lock` (pid, host) per matrix dir; stale locks replaced; test |
| 3 | Verification PASS/FAIL reached the narrator before it wrote "Outcome I declared"; `report_instructions_visible_during_task` hardcoded false although `config="user"` Claude cells list `ajx*` skills | narrator gets `digest.narrator.md` without verification or isolation sections; flag derived from `init.slash_commands`; limitation recorded; test |
| 4 | `gaps_over_60s` counted a running tool call as idle time | measured from the previous event's end; test |
| 5 | `transcript_path` pointed at the pre-archive config dir; post-archive `--stages narrate` silently degraded to reconstruction | paths repointed at archive; narrate refused after archive with a message; test |
| 6 | Isolation scan flagged `tempfile` dirs under `/tmp` as sibling runs | only the 10-hex `_rand()` shape counts; test |
| 7 | Rerunning extract appended a second "Corrections" block the next extraction then read | delimited block replaced in place; test |
| 8 | Unvalidated `json.loads(result text)` fallback could crash render on missing keys | shape-checked; invalid registers become `extraction_status: failed` |
| 9 | `--stages` names never validated | validated in `run_matrix` and `Run.go`; CLI exit 2; test |
| 10 | Gemini `timestamp` assumed ISO string | non-string timestamps fall back to arrival time |
| 11 | Post-exit reap killed a dev server before `http` verification could reach it | execute stage keeps survivors; reaped after teardown |
| 12 | `WrapperRunner` appended argv unquoted after `ssh ... &&` | `shlex.join` when the prefix ends in a shell operator or `shell_join = true` |
| 13 | Partial token subtotals aggregated with complete ones in per-cell variance | only reconciled runs aggregate; note in matrix comparability |
| 14 | Digest code fence broke on agent output containing ``` | fence longer than any backtick run |
| 15 | Tests never invoked the CLI `main`, the timeout path, `exclude_message_ids` on a narrate transcript, or `parallel>1` | all added (33 tests) |
| 16 | Dead code (`dig`, `model_flag`, unused import); `permission_mode`/`permission_args`/`narrate_timeout_seconds` undocumented; "counted once in totals" wording | removed / documented / reworded |

Checked and found sound by the independent reviewer: Claude token and tool-id reconciliation, half-open
phase windows and v1 calculator inputs, `parallel>1` state isolation, mdlite escaping, argv/prompt redaction.

## Known limits after review

- Only `claude-code` and `kiro-cli` adapters are verified against real CLIs; `codex`, `gemini-cli`, `cursor-agent`,
  `copilot-cli` follow documented formats and are flagged `verified_live: false` in `run.json`.
- `auth = "inherit"` runs Claude Code `--safe-mode` inside the user's config dir; full config isolation needs an
  env-driven profile or `isolates_config = true`.
- A worker orphaned by an ajx2 crash is not killed on resume (pid reuse risk); it is reported as `interrupted`.
- The `.lock` is per matrix directory and host; two machines sharing a report directory are refused, not coordinated.
