# The Go CLI

`ajx` is also available as a single native binary built from `cmd/ajx`. It has
the same commands, options, output lines and exit codes as the Python CLI in
`skills/ajx/bin/ajx`, and it reads and writes the same trial files and output
folders. A trial started with one can be checked, resumed, reported, exported or
watched with the other.

The Go CLI adds:

- an interactive setup wizard for `ajx init`, with flags for every question;
- `--json` output on `init`, `validate`, `doctor`, `run`, `status`, `report`,
  `export` and `plugins`;
- `ajx ci`, a headless evaluation that decides `passed`, `failed` or
  `inconclusive` and writes `ci-result.json` and `junit.xml`;
- `ajx version` and `ajx completion bash|zsh|fish`;
- a trial folder wherever a trial file is expected: `ajx validate trials/my-cli`,
  `ajx run trials/my-cli` and `ajx status trials/my-cli` read its `trial.toml`,
  as `ajx console` already does.

## Download and install

Every push to `main` runs the Python, Go, container, and privacy checks. After
they all pass, CI builds installable packages on four native runners, runs the
Go tests and smoke-tests each executable, and uploads the successful packages.

Open **Actions → CI → a successful main run → Artifacts** and download the
package for your machine. GitHub requires you to sign in to download Actions
artifacts. Packages are retained for 30 days; these are builds of `main`, not
tagged releases.

| Machine | Artifact |
|---|---|
| macOS, Apple silicon | `ajx-darwin-arm64` |
| macOS, Intel | `ajx-darwin-amd64` |
| Linux, x86-64 | `ajx-linux-amd64` |
| Linux, ARM64 | `ajx-linux-arm64` |

You can also download with the GitHub CLI. Choose the run ID from the first
command, then substitute it for `RUN_ID` in the second:

```sh
gh run list --repo royosherove/ajx --workflow CI --branch main --event push --status success --limit 5
gh run download RUN_ID --repo royosherove/ajx --name ajx-darwin-arm64 --dir ajx-download
cd ajx-download
shasum -a 256 -c SHA256SUMS
tar -xzf ajx-darwin-arm64.tar.gz
mkdir -p "$HOME/.local/bin"
install -m 755 ajx-darwin-arm64/ajx "$HOME/.local/bin/ajx"
export PATH="$HOME/.local/bin:$PATH"
ajx version
ajx console demo trials/console-demo
```

The example is for Apple silicon. Use your artifact name in the download,
archive, and install paths; on Linux, use `sha256sum -c SHA256SUMS` to verify the
archive. If downloading in the browser, unzip the Actions download first.
Add the `PATH` export to your shell profile to keep it in new terminals.

Each download contains the archive, its SHA-256 checksum, and `build.json`
with the source commit and platform. `ajx version` prints that commit too.
The archive preserves the executable's permissions and includes installation
instructions and dependency licenses. Builds are not signed or notarized.
The demo needs no agent or cloud credentials; real trials need the selected
agent CLI and authentication. Python plugins still need Python 3.11 or newer.

## Build from source

You need Go 1.25 or newer. Dependencies are vendored, so no network access is
needed to build.

```sh
go build -o bin/ajx ./cmd/ajx        # from the AJX checkout
bin/ajx version
```

To install a pinned revision into `$(go env GOPATH)/bin`:

```sh
go install github.com/royosherove/ajx/cmd/ajx@<rev>
```

The skill's prompts, schemas, references, page assets and examples are compiled
into the binary. Set `AJX_SKILL_DIR` to a `skills/ajx` checkout to read them from
disk instead. `ajx version` prints the fingerprint of the resources in use.

## Relation to the Python CLI

Use either one. The coordinating skill and the AJX Console run whichever `ajx`
you invoke. Both CLIs:

- validate trial files with the same rules and messages;
- write `state.json`, `run.json`, `verify.json`, reports and console journals with
  the same keys and values (JSON key order is sorted in Go);
- take the same matrix lock, so they never run the same output folder at once;
- resume each other's attempts without repeating a task.

The repository's parity tests run both CLIs side by side; see
[Testing](#testing).

## Set up a trial with the wizard

In a terminal, `ajx init` asks for whatever its flags leave out:

```sh
ajx init                    # asks for everything
ajx init trials/my-cli      # asks for the rest
ajx init trials/my-cli --product my-cli --wizard   # asks even though flags are complete
```

The wizard asks for the product, the folder, the outcome the user wants (written
into `task-prompt.md`), the materials the worker may use, the worker harness
(with whether each CLI is installed and its version), the model, the reporter, a
success check, the time limit, the attempts per cell, a folder of starting files,
and where the worker runs (local, or a prepared local container image). It shows
a summary, writes `trial.toml` and `task-prompt.md`, checks that the trial loads,
and offers to open the AJX Console, run `ajx doctor`, or exit.

Set `ACCESSIBLE=1` for a line-by-line version of the form that works with screen
readers.

The wizard never starts when stdin or stdout is not a terminal, when `CI` is set,
or with `--no-input`, `--yes` or `--json`. Then `--product` and the folder are
required, as in the Python CLI, and a missing one is a usage error (exit 2).

Every question has a flag, so an agent can scaffold a complete trial in one call:

```sh
ajx init trials/jq --product jq --harness codex --no-input \
  --task "Convert the three orders in orders.json to orders.csv, keeping every value." \
  --material "jq --help" \
  --check "test -s orders.csv" --check "file:orders.csv" \
  --model example-model --timeout 600 --repetitions 2 --fixture fixtures/orders
```

| Flag | Writes |
|---|---|
| `--task TEXT` | `task-prompt.md`: "I want to try PRODUCT." followed by the task and a list of the materials |
| `--material TEXT` (repeatable) | `[task] materials` |
| `--check CHECK` (repeatable) | a `[[verify]]` entry: a shell command (or `shell:CMD`), `file:GLOB` beneath the workspace, or `http:URL` |
| `--model`, `--reporter-model` | the cell's and the reporter's `model` |
| `--timeout SECONDS`, `--repetitions N` | `timeout_seconds`, `repetitions` |
| `--fixture DIR` | `[task] fixture_dir`; the folder must exist |
| `--environment container --image IMAGE` | an `[environments.container]` profile with the documented defaults, selected by the cell |

Without these flags, `ajx init` writes exactly the Python CLI's scaffold. A
container trial still needs an image prepared as described in
[environments](../skills/ajx/references/environments.md), and credentials that
work in a fresh HOME; `ajx init` reports when the written trial does not load yet.

## Output for agents and pipelines

Human-readable text stays the default. `--json` prints one JSON document on
stdout instead; progress lines of `run --json` go to stderr. No command writes
ANSI escape codes unless stdout is a terminal, and `NO_COLOR` turns styling off.

Every document has `schema_version` (currently `1`) and `command`. New fields
may appear within a schema version; existing fields keep their meaning.

| Command | Main fields |
|---|---|
| `validate --json` | `ok`, `trial` (`id`, `product`, `path`, `sha256`), `prompt` (`path`, `sha256`), `runs[]` (`run_id`, `cell`, `repetition`, `harness`, `model`, `config`, `auth`, `environment`, `agent_configuration`, `backend`), `verify_checks`, `setup`, `teardown`, `preflight`, `identity_cmd_set`, `output_dir`, `warnings[]` (`cell`, `text`), `profiles[]`, `lines[]` |
| `doctor --json` | `ready`, `exit_code`, `harnesses[]` (`name`, `checked`: host or container, `installed`, `version`, `verified_live`, `resume`, `reporter`, `clean`, `telemetry`), `reporter` (`harness`, `model`, `auth`, `problems[]`), `cells[]` (`auth`, `auth_identity`, `task_identity`, `default_aws_identity`, `warnings[]`, `problems[]`, `runner`, or for environment profiles `backend`, `backend_prerequisites`, `mount_probe`), `command_environments[]` (`cells`, `task_identity`, `preflight[]`, `warnings[]`), `verify[]`, `lines[]` |
| `status --json` | `ok`, `exit_code`, `output_dir`, `stages[]`, `runs[]` (`run_id`, `dir`, `stages` (stage to status), `marks[]`, `verified`, `checks_with_auth_errors[]`, `teardown_status`, `cleanup_confirmed`, `stage_errors[]`, and `events[]` with `--tail N`), `cleanup_not_confirmed[]` |
| `run --json` | `ok`, `exit_code`, `output_dir`, `matrix_report`, `markdown`, `order[]`, `status` (the `status --json` document); with `--dry-run`, `plan` (the matrix plan) |
| `report --json` | `index`, `matrix_markdown`, `matrix_json`, `runs_rendered[]`, `synthesis` |
| `export --json` | `run_id`, `dest`, `files[]`, `replacements` (label to count), `replacement_order[]`, `warnings[]` |
| `init --json` | `dir`, `trial_file`, `prompt_file`, `written[]`, `kept[]`, `trial_id`, `harness`, `reporter`, `valid`, `problem`, `next[]` |
| `plugins --json` | `harness[]`, `auth[]`, `runner[]`, `check[]` (`name`, `source`, and the same attributes as the text listing), `notes[]` |
| `version --json` | `version`, `implementation`, `build`, `skill_fingerprint`, `skill_resources` |

When a `--json` command fails, it prints the usual error line on stderr and a
document with `ok: false`, `exit_code` and `error` (`type`: `trial_file_error`,
`console_error`, `locked`, `interrupted` or `error`; `message`) on stdout.
`ajx console state`, `since`, `wait` and `list --json` print the console's own
JSON, as in the Python CLI.

## Headless evaluation: `ajx ci`

`ajx ci` runs a complete, already briefed trial in a pipeline and decides whether
the product met the declared target. It never prompts, opens a browser or waits
for a login.

```sh
ajx ci usability/trial.toml --output "$AJX_RESULTS_DIR" \
  --policy usability/acceptance.toml --timeout-grace 60 </dev/null
```

It performs these steps, in order:

1. Loads the acceptance policy, then runs the same checks as `ajx validate`.
2. Refuses an output folder that already holds attempts, unless `--allow-reuse`
   (resuming never repeats a finished task, so a reused folder does not test a
   new product revision).
3. Writes a provisional `ci-result.json` with decision `inconclusive` (reason
   `incomplete`), so a job killed later still leaves a non-passing result.
4. Runs the same checks as `ajx doctor` and saves them in `ci-doctor.txt`. When
   doctor reports a problem, it stops before any worker starts (reason
   `not_ready`), unless `--skip-doctor`.
5. Runs the planned matrix like `ajx run`, with the same lifecycle, cleanup and
   evidence.
6. Evaluates each planned attempt from its recorded files and writes
   `ci-result.json` and `junit.xml`.

| Option | Meaning |
|---|---|
| `--output DIR` | output folder for this evaluation, instead of the trial's `output_dir` |
| `--result FILE`, `--junit FILE` | where to write them (default: in the output folder) |
| `--policy FILE` | acceptance policy, below |
| `--min-success-rate R` | the verified-success rate each cell needs, 0 to 1 (default 1: every attempt) |
| `--require-cleanup` | unconfirmed cleanup fails the evaluation |
| `--allow-reuse` | resume an output folder that already holds attempts |
| `--timeout-grace S` | after SIGINT, SIGTERM or SIGHUP, wait at most S seconds for cleanup, then write the result and exit 130 |
| `--cells A,B` | evaluate only these cells or model groups |
| `--no-synthesis` | skip the model-written matrix synthesis |
| `--skip-doctor` | run even when doctor reports problems (recorded) |

Command-line options override the policy file. An `ajx status` of a CI
evaluation takes the output folder: `ajx status "$AJX_RESULTS_DIR"`.

### Acceptance policy

```toml
schema_version = 1

[acceptance]
min_success_rate = 1.0     # verified-success rate each cell needs, over its planned attempts
min_repetitions = 1        # verified attempts (passed or failed) each cell needs
require_cleanup = true     # unconfirmed cleanup fails the evaluation
require_reports = false    # an unfinished report makes the evaluation inconclusive
required_cells = ["cc-sonnet", "codex"]   # cells the plan must include
```

Unknown keys are an error, so a misspelled setting cannot be ignored silently.
The result records the policy file's SHA-256.

### How the decision is made

Only the trial's deterministic `[[verify]]` checks count. An attempt passes when
`verify.json` says `succeeded` and no check printed credential or permission
errors. It fails when the checks report `failed` or `partial`. Any other attempt
has no trustworthy outcome: it is missing, stopped with a stage error,
interrupted, its checks did not run, or its checks hit credential errors.

Each cell is judged separately, so a good average cannot hide a failing cell:

- **failed**: the cell cannot reach the target even if every attempt without an
  outcome had succeeded; or cleanup is not confirmed and the policy requires it.
- **inconclusive**: the target depends on attempts without an outcome, a cell has
  fewer verified attempts than `min_repetitions`, the trial has no `[[verify]]`
  checks, doctor reported a problem, or the run stopped early.
- **passed**: every cell reaches the target and nothing above applies.

An empty asks list, a zero exit code from the worker or a finished report never
counts as success.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | passed |
| 1 | failed |
| 2 | invalid configuration: trial file or policy error, or an output folder that is not fresh |
| 3 | locked: another `ajx` is running this output folder |
| 4 | inconclusive |
| 130 | interrupted (SIGINT, SIGTERM or SIGHUP); cleanup ran first, or `--timeout-grace` expired |

### `ci-result.json`

| Field | Content |
|---|---|
| `schema_version` | `1` |
| `tool` | `name`, `implementation` (`go`), `version` |
| `started_at`, `finished_at` | UTC timestamps |
| `decision`, `exit_code` | as above |
| `reasons[]` | `code`, `severity` (`failure`, `inconclusive` or `warning`), `message`, and `run_id` or `cell` when specific |
| `trial` | `path`, `id`, `product`, `sha256` of `trial.toml`, `prompt_sha256` |
| `plan` | `sha256` (the plan ID the AJX Console approves: trial file, task prompt, starting files and the trial's plugins), `runs[]`, `cells[]`, `verify_checks`, `only_cells[]` |
| `policy` | the effective policy and the file's `path` and `sha256` |
| `validate`, `doctor` | their warnings and output lines; doctor's `ready` and `file` |
| `runs[]` | per planned attempt: `run_id`, `cell`, `repetition`, `harness`, `model`, `outcome` (`passed`, `failed`, `missing`, `error`, `interrupted`, `untrusted`, `not_run`), `stages`, `stage_errors[]`, `execute_seconds`, `verification` (`outcome`, `passed`, `total`, `checks[]`, `checks_with_auth_errors[]`), `cleanup` (`status`, `confirmed`), `report_complete`, `artifacts` (report, journey, asks, run/verify/state JSON, measurements) |
| `cells[]` | `cell`, `planned`, `passed`, `failed`, `unknown`, `success_rate`, `target`, `status` |
| `coverage` | counts: `planned`, `recorded`, `verified`, `passed`, `failed`, `unknown`, `cleanup_confirmed`, `reports_complete` |
| `artifacts` | `output_dir`, `matrix_report`, `matrix_json`, `matrix_markdown`, `result`, `junit`, `doctor` |

Reason codes: `invalid_config`, `output_not_fresh`, `not_ready`, `locked`,
`interrupted`, `run_error`, `incomplete`, `run_missing`, `stage_error`,
`verification_missing`, `verification_untrusted`, `verification_failed`
(warning), `success_rate_below_target`, `insufficient_evidence`,
`insufficient_repetitions`, `no_checks`, `cleanup_not_confirmed`,
`reporting_incomplete`.

When the trial file cannot be loaded, the output folder is unknown: the result is
written only where `--result` or `--output` points.

### `junit.xml`

One `testsuites` document with two suites:

- `ajx.<trial-id>.runs`: one test case per planned attempt (`classname`
  `ajx.<trial-id>.<cell>`, name the run ID, time the worker's wall clock). A
  failed verification is a `<failure>`; an attempt without a trustworthy outcome
  is an `<error>`. `<system-out>` lists the checks, stage errors, cleanup status
  and the report's path.
- `ajx.<trial-id>.acceptance`: one test case per cell's success-rate target, plus
  "verification checks defined", "cleanup confirmed" and "evaluation completed".

### What `ajx ci` does not cover

- No baseline comparison, regression gate or `baseline-comparison.json`.
- No `ajx improve` loop, automatic product changes or candidate promotion.
- Time, token, tool-call and cost limits are not part of the policy.
- No aggregation of shards run by separate jobs.
- A hard kill (SIGKILL, or a job deadline without a grace signal) can leave
  cleanup unfinished. The provisional result stays `inconclusive`; rerun the same
  command with `--allow-reuse` to finish cleanup and reporting.
- Harnesses, auth profiles and setup commands must themselves work without
  interaction. `ajx ci` cannot make an interactive login unattended.

Keep the whole output folder as a CI artifact, including on failure.

## Interruption

`ajx run` and `ajx ci` treat SIGINT, SIGTERM and SIGHUP alike: the worker is
stopped, teardown runs, the environment is released and copied credentials are
purged before the command exits with 130. A launcher that left SIGINT ignored,
such as a background job of a non-interactive shell, does not prevent this.
After SIGHUP, output goes to `/dev/null` so cleanup cannot fail on a closed
terminal, and a closed pipe on stdout (`ajx run trial.toml | head`) does not end
the run before cleanup. A further signal during cleanup interrupts only the cleanup step in
progress, as in the Python CLI.

## Shell completion

```sh
source <(ajx completion bash)          # bash; add it to ~/.bashrc
source <(ajx completion zsh)           # zsh; add it to ~/.zshrc
ajx completion fish | source           # fish
```

## Known differences from the Python CLI

- **Python plugins are not executed.** The Go CLI cannot load `plugins/*.py`. A
  trial whose harness or check comes from one fails `ajx validate` and `ajx run`
  (exit 2) with a message naming the plugin, and `ajx doctor` lists it as a
  problem. Declarative `[adapters.*]` harnesses and `[auth.*]` profiles work in
  both CLIs. The console demo's simulated worker is built in.
- `ajx doctor` prints `go <version> (ajx <version>)` as its first line instead of
  `python <version>`.
- `ajx --help` and the usage lines list the added commands and options. Error
  messages for usage mistakes are otherwise the same, with exit code 2.
- JSON files list keys in sorted order.
- `ajx init` writes TOML-escaped values. The Python CLI substitutes values
  unescaped, so a product name containing a quote produces an invalid trial there.

## Testing

```sh
go vet ./... && go test ./...
AJX_PARITY=1 go test ./internal/parity/ -run Parity -v    # needs Python 3.11+ (AJX_PYTHON to choose)
```

The parity tests build the Go binary and run both CLIs on synthetic inputs:
`plugins`, usage errors and help, `init` (both scaffolds validate in both CLIs),
`validate` and `doctor` on the bundled examples with fake harness binaries on
`PATH`, `example`, `status` and `export` over a copy of `docs/example-report`,
`console state` and `console since`, a full run, status and report of the console
demo and of a `/bin/sh` declarative adapter, resuming one CLI's attempt with the
other, and interruption by SIGTERM and by SIGINT from a launcher that ignored it.
They use a temporary HOME, a shadowed `aws`, and no agent, model provider,
network service or cloud account.
