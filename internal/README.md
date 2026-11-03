# AJX Go CLI: package layout and porting conventions

The Go CLI (`cmd/ajx`) is a native implementation of the Python CLI in
`skills/ajx/bin/ajx` and `skills/ajx/lib/ajx`. Both read the same trial files
and write the same output files, so a trial can be run, resumed, reported or
watched by either one. The skill's prompts, schemas, references, page assets
and examples are embedded from `skills/ajx` (`embed.go`); set `AJX_SKILL_DIR`
to a `skills/ajx` checkout to read them from disk instead.

Build and test (dependencies are vendored; no network needed):

```sh
go build -o bin/ajx ./cmd/ajx
go vet ./... && go test ./...
```

## Packages

| Python module | Go package | Notes |
|---|---|---|
| `util.py` | `internal/util` | JSON-shaped values (`util.M`), accessors, time, hashing, processes |
| `base.py`, `plugins.py` | `internal/plugin` | Harness/Auth/Runner/Check contracts, run context `plugin.Ctx`, registry |
| skill resources | `internal/res` | prompts, schemas, assets, examples; fingerprint |
| `spec.py` | `internal/spec` | trial loading, validation, plugin resolution, run plan |
| `envcheck.py` | `internal/envcheck` | the three credential environments, warnings, preflight |
| `evidence.py`, `measure.py` | `internal/evidence`, `internal/measure` | |
| `cloud_auth.py`, `builtin/*.py` | `internal/cloudauth`, `internal/builtin` | adapters register in `init()` |
| `environments.py` | `internal/environments` | `EnvironmentRunner` implements `plugin.ManagedRunner` |
| `agent_configuration.py` | `internal/agentconfig` | |
| `reporter.py`, `mdlite.py`, `report_ui.py`, `render.py`, `export.py`, `demo.py` | same names under `internal/` (`report_ui` -> `reportui`) | |
| `runner.py` | `internal/runner` | lifecycle stages, resume, lock |
| `console.py` | `internal/console` | same HTTP API and journal as the Python console |
| `plan_edit.py` | `internal/planedit` | the Plan tab's trial editor; ordered `Dict` values so rewritten tables keep Python's key order, and a port of difflib's `unified_diff` |
| `bin/ajx` | `cmd/ajx`, `internal/cli` | plus the wizard (`internal/wizard`) and CI mode (`internal/ci`) |

## Conventions

- **Names.** A Python function used by another module becomes an exported Go
  function with the CamelCase name in the same package mapping:
  `envcheck.inspect_cell` -> `envcheck.InspectCell`,
  `render.run_report` -> `render.RunReport`, `runner.STAGES` -> `runner.Stages`.
  Keep argument order. Optional keyword arguments become a trailing
  `Opts` struct (`spec.Load(path, spec.LoadOpts{CleanupOnly: true})`).
  Leading-underscore helpers used across modules (`runner._pid_alive`) are
  exported without the underscore (`runner.PidAlive`).
- **Values.** Python dicts are `util.M` (`map[string]any`), lists `[]any`,
  `None` is `nil`. Read them with `util.Get`, `util.S`, `util.Str`, `util.Int`,
  `util.Num`, `util.Truthy`, `util.Maps`, `util.Strings`; never type-assert
  numbers directly (TOML gives `int64`, JSON gives `float64`). Environments
  are `map[string]string`.
- **Files stay compatible.** `state.json`, `run.json`, `verify.json`,
  `measurements.json`, `events.jsonl`, console journals and every other record
  keep the Python keys and value types. Write JSON with `util.WriteJSON`
  (indent 2, UTF-8, no HTML escaping, atomic). Key order is sorted; that is the
  only intended difference.
- **Text.** Python slices count characters: use `util.Clip`, `util.Tail`,
  `util.RuneLen`, never byte slicing of user text. User-facing messages,
  CLI output lines and report text match the Python wording unless a change
  is a deliberate improvement noted in the change description.
- **Errors.** Python exceptions become returned errors. Typed errors that the
  CLI maps to exit codes: `spec.Error` (2), `console.Error` (2),
  `runner.LockedError` (3). `KeyboardInterrupt` is context cancellation:
  `plugin.Ctx.Go` is cancelled on SIGINT/SIGTERM/SIGHUP, process helpers
  return `util.ErrInterrupted`, and cleanup still runs (exit 130).
- **Contracts.** Adapters embed `plugin.BaseHarness`, `*plugin.AuthBase` or
  `plugin.RunnerBase` and override methods; base methods dispatch through
  `Self`, like Python's `self`. Use `plugin.Register*` in an `init()`.
- **Python plugins.** The Go CLI cannot execute `plugins/*.py`. Built-ins
  that replace a bundled Python plugin set `plugin.NativeEquivalents`;
  other Python plugins load as placeholders that fail with a clear message.
  Declarative `[adapters.*]` harnesses work in both CLIs.
- **Tests.** Port the Python tests that cover the module into Go tests in the
  same package. Use synthetic fixtures (`testdata/`, or
  `../../skills/ajx/tests/fixtures`) and fake harnesses. Never call a real
  agent, model provider or cloud account from tests. Container checks run only
  when `AJX_TEST_CONTAINER_IMAGE` is set.
- **Dependencies.** Standard library plus the agreed, vendored set:
  `github.com/BurntSushi/toml`, `charm.land/huh/v2` and `charm.land/lipgloss/v2`
  (with their own dependencies). Do not add others. Stay on the v2 Charm
  libraries: v1 Bubble Tea queries the terminal in a package init, which
  stalls every command for 5 s on terminals that never answer
  (`cmd/ajx` has a test for it).
