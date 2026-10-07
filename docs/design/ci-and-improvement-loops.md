# Proposal: CI/CD evaluation and improvement loops

**Status: proposed; not implemented.** Reviewed on 2026-10-07.

Product teams should be able to run AJX as a normal CI job: supply a versioned
trial and an acceptance policy, execute a headless command, and receive a
machine-readable decision plus browsable evidence. An optional improvement loop
should propose product changes and retest them until the declared targets are met
or a stopping limit is reached.

Support two modes with the same evaluation contract:

| Mode | Purpose |
|---|---|
| Evaluate once | Check a product revision against usability targets and an optional comparable baseline. Let the team's existing pipeline decide what happens next. |
| Improve and retest | Use observed problems to produce a candidate change, test it in fresh attempts, keep or discard it under a fixed policy, and repeat within explicit limits. |

Both modes must work without a browser, an interactive coordinator interview, or
an operator available to approve individual tool calls. The complete task,
permissions, credentials, budgets, and acceptance policy are supplied before the
job starts. The [live dashboard](live-dashboard.md) is an optional consumer of the
same run data.

## A simple pipeline command

The following is a **proposed interface**, not a command available in AJX today:

```sh
ajx ci usability/trial.toml \
  --policy usability/acceptance.toml \
  --baseline "$AJX_BASELINE_REF" \
  --output "$AJX_RESULTS_DIR"
```

The command should validate the configuration and capabilities, check required
credentials, resolve the baseline, run the selected matrix, evaluate acceptance,
and export results. Omit the baseline for an absolute-target evaluation; a policy
requiring a baseline must fail validation when none is supplied.

Keep the entry point usable from any CI system. Provide small recipes for common
systems after the CLI contract is implemented. A recipe needs to install a pinned
AJX revision and supported harness versions, inject scoped credentials from the
CI secret store, run the command, and retain its artifacts even when it fails.
Live provider trials must be an explicit pipeline choice; the repository's normal
offline tests continue to require no provider credentials or paid trials.

Missing credentials, unsupported headless operation, incompatible environment
requirements, and required human input must produce explicit failures or
inconclusive results rather than waiting indefinitely for an interactive login.
Record model-provider identity separately from the task's account permissions.

### Results and exit status

The proposed result contract should include:

| Output | Content |
|---|---|
| `ci-result.json` | Schema version, run and candidate identities, policy/baseline hashes, decision, reasons, coverage, and artifact references. |
| `junit.xml` | Stable test identities for matrix attempts and acceptance checks, including errors and incomplete evaluation. |
| `baseline-comparison.json` | Matched cells, changes in verified outcomes and available costs, sample counts, and comparability limits. |
| Existing AJX artifacts | Per-run evidence, asks, journeys, measurements, and HTML matrix/report views. |
| Loop ledger, when enabled | Every candidate, its patch/commit, hypothesis, evidence, measurements, decision, and stopping reason. |

Define explicit CI outcomes: `passed`, `failed`, and `inconclusive`. Only `passed`
returns zero. Invalid configuration, infrastructure/authentication failures,
missing required evidence, exhausted limits before acceptance, and cancellation
must not produce a successful check. Include specific reason codes so a pipeline
can distinguish a product regression from an evaluation failure.

Export captured evidence and cleanup status on failure. Reserve time for
collection and teardown before the CI job's hard timeout. Durable checkpoints
and an external cleanup controller are needed to recover after a hard kill;
a final process handler alone cannot guarantee this.

## Define “good enough” before execution

Acceptance is a versioned policy owned by the product team. It should specify:

- Required scenarios, harness/model cells, environment and extension profiles,
  and minimum valid repetitions per cell.
- Required task checks and target success rates, with sample counts and
  uncertainty visible in the result.
- Permitted regressions relative to a compatible baseline, including whether
  every required cell must meet its target.
- Time, token, tool-call, or cost limits only where the relevant measurement
  coverage is sufficient.
- Treatment of infrastructure failures, timeouts, incomplete reporting, missing
  measurements, and cleanup failures.

Do not reduce success to “the process exited” or “the reporter found no asks.”
Verified task outcomes come from the scenario checks. Extracted asks and their
severity remain evidence-linked model judgments; they can guide improvements but
must not be treated as an infallible quality score.

Keep invalid, failed, timed-out, and successful attempts in the result. A quick
failure must not look like a latency improvement. Preserve missing metrics as
missing; a policy depending on unavailable data is inconclusive. Do not pass a
global average that hides a regression in a required harness or environment.

Pin the original baseline's product artifact, documentation, evaluator version,
scenario/check definitions, model and harness configuration, environment recipe,
permission and extension profiles, and measurement policy. Resolve mutable
references before the run. If conditions differ beyond the policy's allowed
comparison dimensions, mark the comparison incompatible instead of inventing
an improvement.

## An optional improve-and-retest loop

The experiment pattern is inspired by
[autoresearch's baseline and measured-change workflow](https://github.com/karpathy/autoresearch)
and its [agent instructions](https://github.com/karpathy/autoresearch/blob/master/program.md),
reviewed on 2026-10-07. This proposal adapts the pattern to product usability
across a matrix; it does not copy its implementation or require it as a dependency.

The following is also a **proposed interface**:

```sh
ajx improve usability/trial.toml \
  --policy usability/acceptance.toml \
  --loop-policy usability/improvement.toml \
  --output "$AJX_RESULTS_DIR"
```

An improvement policy should identify the allowed product files/surfaces, the
repair agent and its permissions, how to compare candidates, and finite stopping
limits. The loop follows these steps:

1. **Establish the baseline.** Resolve all inputs, run or validate compatible
   baseline evidence, and retain the original revision and acceptance policy.
2. **Select a supported problem.** Use task failures, asks, gates, and trace
   evidence to state a specific improvement hypothesis.
3. **Create an isolated candidate.** A repair agent changes only the allowed
   product surface in its own checkout or branch. Product code, documentation,
   or the product's skill can be eligible when explicitly in scope.
4. **Check the candidate.** Run the product's required build and tests before
   spending on the next agent matrix. Record failed candidates too.
5. **Retest from fresh state.** New workers receive the same customer task and
   declared starting conditions, without the repair agent's discussion, previous
   solutions, or earlier worker memory.
6. **Keep or discard by policy.** Compare the candidate with the best retained
   revision while continuing to check regressions against the original baseline.
   Preserve evidence for both accepted and discarded experiments.
7. **Confirm acceptance.** Run the declared confirmation suite before declaring
   the target met. A screening subset or a single favorable attempt is insufficient.
8. **Return the result.** Export the best candidate, its change history, final
   decision, evidence, remaining problems, and stopping reason.

Discarding a candidate affects only the loop's isolated working state. Preserve
the user's checkout and concurrent work. Promotion of a candidate into a shared
branch, pull request, release, or deployment is a separate pipeline policy.

### Protect the evaluation contract

Keep the evaluator, acceptance thresholds, required cells, task prompts,
verification code, fixtures, expected outputs, budgets, and reference solutions
outside the repair agent's write scope. Fetch protected policies from a trusted
revision, not from the candidate's modified checkout.

If the product's own skill or documentation is the surface being improved, its
content revision is an explicit candidate variable. Keep the rest of the agent
configuration fixed and record the changed manifest. Changing the benchmark to
make a candidate pass requires a new evaluation contract and baseline.

Use a separate confirmation or held-out scenario set to check that a candidate
has not merely learned the development examples. Record repeated measurements
and their uncertainty; do not select solely from the best observed run. A repair
agent's claim of success never replaces the acceptance evaluator.

### Bound the loop

Stop when the acceptance policy passes, or when a declared limit is reached:
maximum iterations, total wall time, available budget, consecutive infrastructure
failures, or a configured number of candidates without a qualifying improvement.
A team can also cancel the loop.

Keep the best known candidate and its evidence when limits are reached, but
return a nonpassing decision if acceptance was not established. Do not promise
that every target is achievable or silently raise a budget to continue.

Separate product execution, repair-agent, reporter/verifier, infrastructure,
storage, and transfer costs where available. State whether each budget is
enforced, estimated, or unobservable. Unknown or delayed billing cannot support
a claim of an exact hard spend cap. Enforce independent time and resource limits
and reserve cleanup capacity.

## CI execution, isolation, and recovery

The [environment and extension contracts](execution-environments.md) should be
resolved before launch. Each matrix attempt needs isolated configuration,
workspace, cache policy, and scoped task credentials. The repair/build process
also needs an explicit permission boundary.

Treat candidate changes, dependencies, and agent output as untrusted. Do not give
an untrusted contribution privileged pipeline credentials simply because it
requests an AJX run. Separate trusted evaluation configuration from the product
checkout being tested, and scope artifact access to the authorized audience.

Give each pipeline execution, candidate, cell, and repetition a stable identity.
Support independent CI jobs or shards with durable manifests and an aggregation
step. The gate must account for every required cell; duplicate uploads or missing
shards cannot turn into extra successes or a smaller denominator.

Resume orchestration and evidence collection without silently rerunning a worker.
An explicitly authorized retry gets a new attempt identity and remains visible.
Preserve the existing rule that recovery does not repeat a task implicitly.
Temporary agent environments need ownership records, cleanup verification, and
expiration outside the ephemeral CI process. Attached environments release only
the job's session and owned state.

Send status, trace updates, and artifact revisions to the live dashboard while
jobs run. Do not rely only on a final CI artifact-upload step for live visibility.
After the job ends, retain static reports and captured evidence under the team's
access and retention policy.

## Fit with the current CLI

AJX already has `validate`, `doctor`, `run`, `status`, and `report` commands and a
declarative trial file. These provide the execution foundation.

The proposed `ci` and `improve` commands, acceptance policy, JUnit exporter,
baseline gate, distributed aggregation, and automatic product repair loop do not
exist yet. In the current command handler, stage errors and cleanup problems
affect exit status, but a failed task verification alone is not a full acceptance
gate. A successful current `ajx run` process must not be advertised as proof that
a product met the future CI policy.

Implement evaluation and result export before the repair loop. Product teams can
then use their existing pipelines to trigger evaluations and fix issues manually
or through their chosen automation.

## Delivery and acceptance

1. **Headless evaluation contract:** versioned policy, capability/auth preflight,
   machine-readable decisions, stable nonzero failure outcomes, and artifact
   export on failure.
2. **Pipeline integration:** portable recipes, trusted configuration, comparable
   baselines, required-cell accounting, and optional live dashboard publishing.
3. **Bounded improvement loop:** isolated candidates, allowed-change enforcement,
   repeated comparisons, immutable experiment ledger, and stopping policies.
4. **Confirmation and promotion integration:** held-out/full-matrix acceptance,
   reviewed candidate artifacts, and the team's explicit promotion policy.

Offline tests should use fake harnesses and synthetic candidate repositories to
cover failed verification with a zero worker exit, missing cells/metrics, baseline
drift, duplicate shards, forbidden evaluator edits, budget exhaustion, cleanup
failure, interrupted recovery, and a candidate that improves one cell but regresses
another. Authorized integration trials should demonstrate a fully unattended
pipeline and a bounded repair loop with reproducible evidence.

This proposal enables no scheduled jobs, paid evaluations, automatic edits,
pull requests, or deployments.
