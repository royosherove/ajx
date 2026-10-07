# Proposal: reproducible execution environments

**Status: proposed; not implemented.** Reviewed on 2026-10-07.

AJX should let a product team ask: **Can agents install and use this product
successfully across the environments our users have?** A test environment should
be as explicit as the harness, model, and product version in a comparison.

The first implementation should provision a disposable EC2 developer machine,
with an isolated workspace, a declared permission profile, and a verified cleanup
deadline. AgentCore is a candidate for a later managed backend. Provisioning is
optional: local product reviews must continue to work without an AWS account.

Today AJX has local, Docker, and wrapper runners, setup/verification/teardown
checks, and recorded evidence. It does not provision these proposed environments
or enforce the policies below. An isolated harness configuration is not a
filesystem sandbox.

## What to bring over from aws-bench

This review uses aws-bench commit
[`542037a`](https://github.com/aws-bench/aws-bench/tree/542037acd5a63287cd0ba8182d864952cc0a1841).
The following are design adaptations; no aws-bench implementation is copied.

| Observed in aws-bench | Proposed use in AJX |
|---|---|
| Separate scenario infrastructure, task instructions, agent container, and verifier [1] | Separate the **worker machine** from the **product environment**. A CLI review may need only a machine; a deployment review also needs a disposable cloud target. |
| Versioned datasets and a hash covering scenario definitions and scripts [1, 2] | Version task prompts, fixtures, environment recipes, permissions, and checks. Save their resolved hashes with every attempt so a comparison can be reproduced. |
| Programmatic checks for mutation tasks, reference answers for diagnosis, and an optional reference solution [1] | Define success independently of the agent's claim. Use a reference solution to validate that a positive scenario is achievable; keep it outside the worker's view. |
| Distinct role configuration and credential handling for agent, verifier, and lifecycle phases [3] | Separate provisioning, task, model-provider, and verifier/cleanup access. Give each phase only the access it needs and record identity provenance without credential values. |
| Admission control permits shared read-only trials and holds mutating trials exclusively through reset [4] | Allocate a fresh environment per attempt initially. If targets are shared later, hold an exclusive lease across preparation, execution, verification, and reset. |
| Post-trial reset and checks that refuse contaminated accounts [3] | Require evidence that the baseline was restored before reuse. Preserve cleanup failures as visible results and quarantine the affected environment. |
| Reward, error, duration, token, and tool metrics [5] | Retain outcome and cost measurements, then connect them to product asks, gates, and journey events. The product team needs an explanation and a fix it can test. |

Two details need particular care. aws-bench's concurrency declaration is
explicitly a scheduling choice, not an enforced IAM policy [6]. AJX must report
declared, enforced, and observed permissions separately. Also, the reviewed
aws-bench implementation uses a shared verifier environment [6]. Running trusted
AJX verification outside a worker-controlled machine would be an additional
design choice, not a capability inherited from aws-bench.

The account machinery can remain optional. aws-bench also documents an
experimental backend for existing accounts [7]. AJX's first cloud backend can
use a dedicated test account supplied by the operator; automatically creating an
Organization or an account fleet need not be a prerequisite.

## The scenario is a contract

A scenario should answer these questions before a worker starts:

| Contract | Required information |
|---|---|
| User goal | The task, intended user context, public materials supplied, and whether a human can answer questions. |
| Starting state | Product absent or installed; fixtures; existing cloud resources; OS and architecture; tools already present; empty or warm caches. |
| Environment needs | Shell, native binaries, package installation, outbound network, writable paths, background processes, local ports, and optional PTY/SSH requirements. |
| Permissions | Filesystem grants, access to adjacent directories, privilege escalation, tool approval behavior, network destinations, and task account permissions. |
| Success | Observable outputs and required checks, with an explicit scope for what each check proves. |
| Stop conditions | Worker completion, harness failure, cancellation, wall-time limit, and any enforceable token/tool/spend limits. |
| Evidence and cleanup | What must be captured, where it is stored, retention, resource ownership, cleanup verification, and maximum environment lifetime. |

“Empty” means empty of the product, its configuration, solved examples, and
previous attempts. The base OS, harness, execution adapter, and declared bootstrap
tools still exist. Record that inventory. A package-rich managed runtime and a
minimal Linux image are different starting states.

The coordinator can propose this contract from the user's goal and check it for
contradictions. It should disclose relevant restrictions to the worker as normal
task context without exposing verifier implementations, reference solutions, or
reporting instructions. Fresh sessions cannot erase a model's prior training.

For a successful-use scenario, validate a reference path under the same
permissions. For an intentional negative test, such as installing a root-only
product without root, define the expected refusal or diagnostic instead. Do not
silently relax permissions to make a cell pass.

## Completion and stopping are different results

Keep these results separate:

| Result | Example |
|---|---|
| Execution stop reason | Agent finished, process crashed, deadline reached, or user cancelled. |
| Task verification | All required checks passed, partial completion, failure, or verification unavailable. |
| Environment validity | The promised baseline and permissions were confirmed, unsupported, or could not be established. |
| Cleanup | Confirmed, failed, or unknown. |

A zero process exit or “done” in the final message does not prove completion.
An agent can also time out after producing a valid artifact. Preserve both facts.
Infrastructure failure before the worker starts is an invalid attempt, with its
own count, rather than a product failure or a missing row.

For an export task, a useful success contract might require an output file that
parses as CSV, contains exactly the expected rows and values, and leaves the input
unchanged. File existence alone is insufficient. For a deployment task, check the
live endpoint and its behavior under a separately scoped verifier identity.
Bound polling for eventual consistency and record the polling window.

Stop or quiesce the worker, capture its outputs, and then verify. Keep trusted
check code and credentials out of worker-controlled storage. Treat submitted
files as untrusted input; do not execute agent-generated verification code with
the verifier's privileges. Use deterministic checks where possible. If a judgment
requires a model, record its rubric, model, uncertainty, and evidence separately
from the worker and reporter.

## Permissions must describe real boundaries

The environment provider should resolve a policy into enforceable settings and
return a capability report. Probe permitted and denied operations on synthetic
sentinel resources before the task. Probes provide evidence of tested operations,
not proof that every possible escape path is absent.

| Dimension | What a run records |
|---|---|
| Filesystem | Workspace, isolated agent home, scratch space, read-only fixtures, system runtime paths, and each additional mounted directory with read/write access. Unlisted personal and sibling workspaces are absent. |
| Privilege | Effective user, allowed package installation locations, whether sudo/root is available, and the boundary outside which guest administration grants no access. |
| Harness tools | Shell/file tools, approval mode, headless behavior, and harness sandbox settings. Auto-approval is distinct from OS or IAM access. |
| Network | Registry, documentation, model-provider and product endpoints; offline/public/allowlisted mode; inbound ports; metadata access; enforcement mechanism. |
| Cloud identity | Task role and allowed resources/regions, separate from credentials used for model inference, provisioning, and verification. |
| Runtime | CPU, memory, disk, process/command limits, background process and PTY support, and lifetime limits. |

Start with three profiles:

1. **Workspace only:** read system tools and fixtures; write the workspace,
   private agent home, and scratch space; install user-level tools; no sudo or
   access to other workspaces.
2. **Guest administrator:** allow system package installation inside a disposable
   VM. Do not claim directory restrictions inside that VM remain secure against
   its administrator. The meaningful boundary is outside the guest.
3. **Restricted workspace:** allow workspace outputs but constrain package
   installation and network access. Useful for testing offline, locked-down, or
   noninteractive onboarding.

A read-only *cloud task* may still need local write access for configuration and
results. Filesystem policy and cloud mutation policy are separate dimensions.
SSH is an execution or debugging transport, not a permission profile.

Every policy item needs an enforcement state: `enforced`, `observed-only`,
`unsupported`, or `unknown`. A backend that cannot enforce a required restriction
must reject that configuration before execution. An explicitly observational run
can proceed under a different, honestly labeled contract.

Provider management credentials must remain outside the worker. Do not expose a
privileged instance profile through metadata, a container-engine socket, or host
mounts. A network allowlist needs actual enforcement; a list in TOML or a prompt
is not a network boundary.

## Choose a backend by capabilities

Service details below were checked against AWS documentation on 2026-10-07.
Backend qualification still requires actual integration trials.

| Candidate | Fit for an AJX developer machine | Constraints and proposed role |
|---|---|---|
| **EC2 with SSM, with optional SSH** | Closest match for ordinary CLI harnesses, native packages, shell sessions, and an explicitly administered guest. | First backend. Pin the image and architecture, create one machine per attempt, and implement external lifetime enforcement. SSM supports shell access without opening inbound ports [8]. |
| **AgentCore Runtime microVMs** | Custom container with a worker adapter; isolated sessions and asynchronous execution. | Managed candidate after EC2. The documented HTTP contract requires ARM64 and invocation/health endpoints. MicroVM lifetime is at most eight hours; export evidence before termination [9, 10]. |
| **AgentCore Code Interpreter** | Stateful managed shell/code/file operations through a tool API. Sessions can last up to eight hours [11, 12]. | Candidate for suitable shell workloads. It includes preinstalled packages [13]; test native installations, PTY needs, background processes, and filesystem controls. Do not assume it is a blank VM or a drop-in host for every CLI. |
| **AgentCore Runtime Instances** | Managed EC2-backed sessions, including x86_64/ARM64 and persistent storage [14]. | Additional candidate for longer or specialized workloads. Explicitly delete session storage between independent attempts; resuming a session preserves state. |
| **Lambda** | Short verification or control-plane jobs. | Ordinary functions have a 900-second maximum. The current timeout documentation separately permits up to 5,400 seconds for certain asynchronous and event-source invocations on Lambda Managed Instances [15]. Neither is an eight-hour interactive developer-machine contract. |

The eight-hour option in this proposal refers to AgentCore microVM sessions.
Session lifetime, individual command timeout, worker deadline, and cleanup
deadline are different limits and must all be recorded.

Hosting a harness inside the environment and giving a local harness a remote
shell tool are also different test configurations. The latter changes the tool
interface the agent uses. Prefer hosting the CLI in the machine for the first
implementation and label any later remote-tool arrangement separately.

## Make the environment a matrix dimension

An attempt is identified by:

```text
product artifact + documentation revision
× scenario revision
× harness version + model/provider + agent settings
× resolved environment + permission profile + starting state
× repetition
```

Vary one dimension at a time for causal comparisons. For example, hold the
scenario, harness, model, and permission profile fixed while comparing two
product versions; then hold the product fixed while exploring harness coverage.
Cross-environment comparisons remain useful but should show all differing inputs.

Use pinned artifacts and resolved versions, not only mutable image tags or model
aliases. Interleave repeated cells with a recorded ordering seed. Model output
can still vary. Share neither writable caches nor solved-task state across
attempts; a warm-cache scenario must explicitly define what is warmed.

Measure infrastructure provisioning and harness bootstrap separately from the
task. If installation is part of the user's product journey, its downloads,
dependency setup, and failures belong inside task time. Report verification,
narration, extraction, and cleanup overhead separately too.

For each comparison, show planned, valid, invalid, completed, timed-out, and
verified-success counts. Preserve missing measurements. Keep unsuccessful
attempts visible when displaying time-to-success; averaging only successful runs
would hide the cost of failure. Report distributions and sample counts before
making improvement claims. Do not rank harnesses from one attempt.

## Explain failures without automatically blaming the product

Link each proposed ask to the events, active policy, and checks that support it.
Allow multiple contributing causes and an `unknown` explanation:

| Observation | Interpretation to test |
|---|---|
| Installation fails because the scenario denies all downloads | Environment constraint; assess the product's offline experience only if that was the intended scenario. |
| A user-level installation path exists, but documentation only describes sudo | Possible documentation/onboarding ask under the workspace-only profile. Validate the working path in a separate follow-up attempt. |
| The harness refuses a command before the product runs | Harness approval or tool restriction; preserve the gate and its impact. |
| The CLI emits an unhelpful permission error for a declared unsupported operation | The restriction may be correct while the diagnostic still deserves a product ask. |
| EC2 bootstrap fails or the verifier's credentials expire | Infrastructure or verification failure; do not infer product quality from it. |

The HTML reports should continue to put asks first for every configuration.
Add environment and permission summaries beside the existing product/harness/
model labels, comparison filters, completion checks, and cleanup status.
`pretty-journey.html` should link gates, roadblocks, forks, and detours to the
specific observed operation and applicable policy. Separate lifecycle events
from the agent's product journey. A blocked call is not automatically a security
violation, and a matrix difference is not automatically a measured product fix.

## Lifecycle and integration

```mermaid
flowchart LR
    A["Resolve scenario and matrix"] --> B["Provision fresh worker environment"]
    B --> C["Probe baseline and permissions"]
    C --> D["Run harness and capture evidence"]
    D --> E["Stop worker and verify outputs"]
    E --> F["Export evidence and destroy environment"]
    F --> G["Render asks and journey"]
    C -->|Invalid setup| F
    D -->|Timeout or failure| E
    E -->|Verification failure| F
    H["Independent expiry controller"] --> F
```

A proposed environment provider should support plan, provision, inspect,
execute, collect, and destroy operations, with durable attempt handles.
Keep the existing harness adapters responsible for their CLI, transcript format,
and resumption semantics.

`Runner.wrap()` alone is insufficient for a cloud backend: fixture transfer,
setup and teardown location, transcript collection, remote paths, process
cancellation, verification access, and narrator resumption also need explicit
contracts. Run any same-session narration before destroying its environment;
if that cannot be done, retain the existing labeled reconstruction behavior.
Reporter calls can use exported evidence after the worker is gone.

Persist lifecycle intent and resource identifiers before launching resources,
and persist execution intent before starting the worker. Recovery should collect
or terminate an existing attempt, never silently run the task again. Use
idempotent cleanup and a controller outside the worker that expires leases even
if the local coordinator crashes. A failed destroy operation stays actionable;
successful task verification does not erase it.

Track compute, storage, logs, network, and model charges separately where
available. A deadline can be enforced without claiming an exact spend cap.
Provider billing can arrive late, so label estimates and enforcement limits.
Cleanup must cover resources created by the task as well as its worker machine,
within the declared account/resource boundary; do not delete unrelated resources.
Budget for evidence export and cleanup before the environment's hard lifetime.

## Illustrative contract

This is a **design sketch**, not a runnable AJX configuration. Names and fields
are provisional.

```toml
[scenario]
id = "export-orders"
revision = "1"
prompt_file = "task.md"
human = "unavailable"
start_state = "product-absent"

[environment]
backend = "ec2"
image = "resolved-pinned-linux-image"
architecture = "x86_64"
fresh_per_attempt = true

[environment.permissions]
profile = "workspace-only"
read = ["system-runtime", "fixtures"]
write = ["workspace", "isolated-agent-home", "scratch"]
other_directories = "absent"
package_install = "user-level"
sudo = false
network = "declared-endpoints-only"
task_cloud_access = "none"

[completion]
required_checks = ["csv-values-match", "input-unchanged"]
verifier_location = "outside-worker"
verify_timeout_seconds = 120

[limits]
worker_timeout_seconds = 1800
environment_lifetime_seconds = 2700
on_expiry = "stop-capture-cleanup"

[lifecycle]
reuse = "never"
cleanup_verification = "required"
```

The coordinator would resolve the image, endpoint policy, tools, credentials,
checks, and resource plan before this becomes executable. A profile name or
symbolic path is not sufficient evidence that its policy has been enforced.

## Delivery order and acceptance

1. **Contract and offline validation:** scenario/environment schema, capability
   negotiation, clear outcome categories, fake-provider lifecycle tests, and
   synthetic report examples. Keep current local workflows compatible.
2. **EC2 pilot:** one supported Linux image, one fresh machine per attempt, SSM
   transport, workspace-only and guest-administrator profiles, an independent
   expiry controller, and evidence collection on failure.
3. **Reproducible comparisons:** pinned images, policy probes, repeated matrices,
   lifecycle cost/coverage reporting, and before/after product comparisons.
4. **Managed backend qualification:** evaluate AgentCore Runtime, Code
   Interpreter, and Runtime Instances against the same capability contract.
   Expose only the combinations that have passed integration trials.

The pilot is complete only when an authorized live trial demonstrates:

- A target CLI can be installed from a declared empty starting state.
- Permitted writes succeed and denied synthetic directory access fails.
- Two fresh attempts cannot read each other's files or configuration.
- Independent checks distinguish task completion from an agent's claim.
- Worker timeout and coordinator termination both preserve evidence and trigger
  cleanup; failed cleanup prevents reuse.
- At least two CLI harnesses produce comparable, honestly labeled results under
  the same supported environment profile.

No cloud resources are provisioned by this proposal.

## Sources

1. [aws-bench dataset and task authoring](https://github.com/aws-bench/aws-bench/blob/542037acd5a63287cd0ba8182d864952cc0a1841/docs/datasets-development.md) and [overview](https://github.com/aws-bench/aws-bench/blob/542037acd5a63287cd0ba8182d864952cc0a1841/README.md).
2. [Scenario hashing](https://github.com/aws-bench/aws-bench/blob/542037acd5a63287cd0ba8182d864952cc0a1841/aws_bench/scenario/hashing.py).
3. [Trial lifecycle and staged credentials](https://github.com/aws-bench/aws-bench/blob/542037acd5a63287cd0ba8182d864952cc0a1841/aws_bench/task/aws_trial.py).
4. [Scenario admission control](https://github.com/aws-bench/aws-bench/blob/542037acd5a63287cd0ba8182d864952cc0a1841/aws_bench/task/queue.py).
5. [Metric aggregation](https://github.com/aws-bench/aws-bench/blob/542037acd5a63287cd0ba8182d864952cc0a1841/aws_bench/metrics/aggregation.py).
6. [Task configuration and verifier environment constraints](https://github.com/aws-bench/aws-bench/blob/542037acd5a63287cd0ba8182d864952cc0a1841/aws_bench/dataset/task_config.py).
7. [Existing-account backend and its limitations](https://github.com/aws-bench/aws-bench/blob/542037acd5a63287cd0ba8182d864952cc0a1841/docs/preexisting-accounts.md).
8. [AWS Systems Manager Session Manager](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager.html).
9. [AgentCore Runtime microVMs](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-how-it-works.html) and [HTTP container contract](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-http-protocol-contract.html).
10. [AgentCore lifecycle settings](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-lifecycle-settings.html).
11. [Code Interpreter sessions](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/code-interpreter-session-characteristics.html).
12. [Code Interpreter terminal commands](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/code-interpreter-s3-integration.html).
13. [Code Interpreter preinstalled libraries](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/code-interpreter-preinstalled-libraries.html).
14. [AgentCore Runtime Instances](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-instances-how-it-works.html).
15. [Lambda timeout configuration](https://docs.aws.amazon.com/lambda/latest/dg/configuration-timeout.html).
