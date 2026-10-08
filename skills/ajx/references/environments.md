# Configure an agent environment

An environment describes where the task agent runs, its starting tools and
permissions. An agent configuration selects its skills and optional extensions.
Select both per matrix cell. The initial implementation supports local processes
and local Docker containers; AWS and other remote hosts can be added as backends.

## Try a skill comparison

From an AJX checkout, copy the example into the ignored trials directory:

```sh
mkdir -p trials
cp -R skills/ajx/examples/environment-profiles trials/environment-profiles
ajx validate trials/environment-profiles/trial.toml
ajx doctor trials/environment-profiles/trial.toml
```

The example compares a plain Codex worker with one given a small CSV skill. Both
receive the same task and synthetic input. Review the plan and credentials, then
run the trial:

```sh
ajx run trials/environment-profiles/trial.toml
```

Agent and reporter calls use the configured provider. Validation, capability
checks and the repository's synthetic tests do not run paid agent tasks.

## Select named profiles

```toml
[environments.developer]
backend = "local"
tools = [{ name = "python", argv = ["python3", "--version"], required = true }]

[agent_configurations.plain]
skills = { mode = "none" }
plugins = { mode = "none" }
hooks = { mode = "none" }

[agent_configurations.guided]
skills = { mode = "selected", paths = ["skills/csv-guide"] }
plugins = { mode = "none" }
hooks = { mode = "none" }

[[cells]]
id = "guided"
harness = "codex"
config = "clean"
environment = "developer"
agent_configuration = "guided"
```

`trial.environment` and `trial.agent_configuration` supply defaults for cells.
An agent profile without an environment uses an implicit local environment.
Profiles require `config = "clean"` and isolated caches, and cannot be combined
with legacy `[runner]` settings.

Each attempt gets separate workspace, home, harness configuration and cache
directories. The worker receives a minimal environment plus explicitly declared
variables and the selected auth profile's required variables. Other caller
credentials, proxies and startup variables are not inherited. Declare any extra
provider or task variables in the trial or auth profile. An explicit `PATH` can
select additional local tools; otherwise the environment uses its own install
paths and a small system path.

Local profiles isolate starting directories and configuration. They **do not
enforce a filesystem, network or privilege sandbox**. Required restrictions that
this backend cannot enforce are rejected.

## Run inside a container

Prepare a Linux image containing the chosen harness and the initial tools. Pull
or build it explicitly, inspect its immutable image ID, and use that ID or a
repository digest in the profile:

```sh
docker image inspect YOUR-IMAGE --format '{{.Id}}'
```

The image value below is a placeholder that must be replaced:

```toml
[environments.container]
backend = "container"
image = "sha256:REPLACE_WITH_THE_FULL_IMAGE_ID"
network = "bridge"
required = ["filesystem_isolation", "non_root", "resource_limits", "lifetime"]
permissions = { filesystem = "isolated", root = "forbid", package_install = "user" }
limits = { cpus = 1, memory_mb = 1024, pids = 256, tmpfs_mb = 64, lifetime_seconds = 3600 }
```

Images must already exist on the local Linux Docker daemon. Mutable tags,
implicit pulls, privileged containers, arbitrary Docker arguments and remote
daemon endpoints are unsupported. `docker_host` can select an explicit local
Unix socket.

The worker runs as a numeric non-root user with a read-only root filesystem,
dropped capabilities and no privilege escalation. It can install user-level
tools in its own writable directories. Those installations survive setup,
execution and narration in the same container. System package installation as
root is not supported in this initial backend.

`network = "none"` is the default and leaves only loopback. `bridge` permits
outbound access, including reachable host/LAN services; it is not an endpoint
allowlist. A provider-backed harness normally needs outbound access.

Only the attempt's declared directories are mounted. Reports, verifier files,
other attempts, caller credentials and the Docker socket are not automatically
mounted. The image must contain ordinary Linux `sh`, `env`, `sleep`, `uname` and
`id` utilities. AJX checks runtime identity, resource settings and synthetic
allowed/denied operations before launching the agent.

### Additional directories

Declare bounded fixture directories under the trial directory:

```toml
[[environments.container.mounts]]
source = "reference-data"
target = "/reference-data"
access = "read"

[[environments.container.mounts]]
source = "starting-project"
target = "/project"
access = "write"
```

Read mounts are read-only. Write mounts use a fresh copy stored with the attempt,
so the original directory is preserved. Overlapping mounts, traversal, symlinks,
special files and credential/configuration/report directories are rejected.
Recorded hashes describe the starting data; read-mount sources must remain fixed
while a trial runs.

## Select skills without changing your installation

Skills support `none`, `selected` and `snapshot`. Both `selected` and `snapshot`
require explicit local directories containing `SKILL.md`; snapshot mode never
imports a live user configuration directory. AJX copies the complete supported
skill tree, records content hashes, and checks for source changes during copying.
An optional `pins = { "csv-guide" = "FULL_SHA256" }` table asserts expected hashes.

The initial parser accepts portable name/description frontmatter and supported
descriptive metadata. Unsupported execution/dependency fields, hidden caches,
credential-like filenames, symlinks and ambiguous trees fail validation. This
structural check does not scan arbitrary file contents for secrets. Snapshot copies are
checked again before execution and narration. Setup or a previous turn cannot
silently add a different skill or modify the pinned profile.

Codex, Claude Code and Kiro CLI have explicit profile controls. AJX checks the
installed CLI's version, required flags and supported configuration before use,
inside the selected environment. Unsupported versions fail before the task.
Other harnesses can still use existing worker adapters, but explicit skill
profiles require a qualified adapter.

| Harness | Authentication for explicit profiles |
|---|---|
| Codex | Copied `codex-chatgpt` login or a configured API/provider auth profile |
| Claude Code | API-key or cloud-provider auth; subscription/OAuth account skill syncing is not supported |
| Kiro CLI | Explicit `env` auth declaring `KIRO_API_KEY`; the image/local install needs its chat companion executable |

For example, Kiro can use:

```toml
[auth.kiro]
type = "env"
isolates_config = true
required_env = ["KIRO_API_KEY"]
```

Select `auth = "kiro"` on the Kiro cell. Required variable values come from the
caller; do not write credentials into TOML.

Optional plugins and hooks currently support **`none` only**. Their controls
are separate from skills; selecting plugins/hooks is rejected until their
dependencies and permissions have an implemented adapter. Managed policy and
harness built-ins may remain and are reported separately. `require_absence`
is not supported: disabled activation is not a claim that arbitrary content is
unreadable or that every built-in was removed.

Installed, discoverable, enabled, loaded and invoked are separate report states.
Without activation telemetry, loaded/invoked remain `unknown`. No skill-use or
usability improvement is inferred merely from enabling a skill.

## Setup, verification and cleanup

Setup, preflight and teardown default to `location = "environment"`. Verification
defaults to `location = "host"`, using trusted check code outside the container.
Shell checks can explicitly select either location. File and HTTP checks run on
the host; host file checks inspect collected/bind-mounted output.

Host shell checks use a separate private home and cache, with tools resolved
from the coordinator's absolute search paths outside worker directories. A
worker-installed tool cannot replace the host check's executable through its
`PATH`. Checks still read worker-controlled output; use trusted validation code
and an isolated interpreter mode when applicable, such as Python's `-I`.

A shell check inside the environment shares its potentially modified tools and
files. Its result has that trust limit. Specify the task's success checks,
credentials and stop conditions before running the matrix.

AJX records ownership before creating an environment and keeps it through
narration. The `release` stage then removes the environment before independent
reporting. Container timeout cancellation removes the entire owned container;
a root-owned bounded supervisor also expires it if the coordinator disappears.
Local processes have weaker cancellation guarantees, recorded in their report.

Resume never recreates a missing container or silently repeats a task. Changing
a recorded profile, cell or prompt requires a new output directory. A failed
preparation is preserved too; fix the configuration and start a new attempt
instead of silently replacing its environment. Failed
release remains an error with evidence preserved; it cannot become successful
cleanup merely because the task succeeded. Retry release with the unchanged
trial using `--stages release,render`.

`--keep-workspace` retains local files for inspection; a full run still releases
its container. Known copied credentials are removed during archival. Real
outputs can contain sensitive data and require review before sharing.

Inspect `environment.json`, `agent-configuration.json` and `run.json`. The report,
journey and matrix show profile identities, hashes, capabilities, extension
states and cleanup status alongside the asks.
