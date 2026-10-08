# Briefing details: use only the parts this trial needs

Start with the short flow in `SKILL.md`. This is a reference for choices that
need more detail, not a questionnaire to read to every new user.

## A user-facing plan

Present one compact plan before a first launch when scope is not yet established:

| Topic | Explain in user terms |
|---|---|
| Outcome and materials | What the worker will accomplish and what a new user would receive |
| Check | How a separate check will establish the actual outcome |
| Worker and reporter | Installed CLIs, model/default, auth route, one attempt unless requested otherwise |
| Starting environment | Initial tools, writable directories, network and install access, sandbox limits |
| Skills and extensions | Inherited or explicit selections, what is enforced, what cannot be controlled |
| Limits and cleanup | Task timeout, separate reporting time, possible charges, budget enforcement, external resources |
| Output | Local destination and the report to open first |

If the user already authorized this scope, proceed after readiness checks.
If they asked to review the plan before starting, wait for their go. Preserve
existing choices rather than replacing them with beginner defaults.

## When the task needs an execution environment

Read [environments.md](environments.md) for named profiles, supported controls,
authentication, tool staging, capability checks, and recovery. Ask only about
requirements not already supplied: initial tools, extra directory access,
network, installation permissions, limits, and cleanup ownership.

A legacy local run can use the existing CLI login, subject to adapter limits.
An explicit local profile uses fresh directories but does not enforce a
filesystem, network, or privilege sandbox. A qualified local Docker profile
can enforce its supported restrictions. It needs a preloaded pinned image
containing the harness/tools, and an explicit credential route. Do not promise
remote provisioning or substitute prompt wording for required enforcement.

Explicit skills support none, selected, and snapshots of declared directories.
Plugins/hooks support none only. Keep the reporter fixed. Explain that a skill
being enabled does not establish that it was used. Fresh-profile authentication
must be checked before proposing it: Codex supports staged login credentials;
Claude needs API/cloud-provider auth; Kiro needs an explicit KIRO_API_KEY and
its chat companion executable. Follow the reference for exact support.

## When the task acts in a cloud or SaaS account

- **Account the worker acts in** (when the task touches a cloud or SaaS account): profile/project/subscription and region. Before offering a profile as an option, check that it authenticates with the CLI the task uses (`aws sts get-caller-identity --profile <name>`, `gcloud auth list`, `az account show`), and offer only profiles that do, each with the account it resolves to. Never offer an unchecked profile.

- **Model credentials**, asked separately from the account above: which provider each harness uses for its model (`ajx plugins` lists auth types; trial `[auth.*]` profiles reference env vars as `${VAR}`; nothing secret is stored). When the task and the model use the same cloud (Claude Code on Bedrock testing an AWS product), give the model its own explicit auth profile and keep the task account out of shared `[env]`. Legacy local trials can use a distinct `AWS_PROFILE`; explicit environments require the credential routes in [environments.md](environments.md) and do not stage AWS profile files. Name the task identity and its explicit credential route separately.

Set `[task] identity_cmd` to show the account the way the worker will use it.
Add a `[[preflight]]` that authenticates with the credentials verification and
teardown use. No worker starts until required preflights pass. Tag external
resources with `$AJX_RUN_TOKEN` and tear down by that tag. Let cleanup errors
fail; do not hide them with `true`. A check intentionally testing access denial
can set `allow_auth_errors = true`. See `examples/aws-cloud/`.

Doctor shows model auth separately from the task identity in the worker's tool
shell and the verification/cleanup shell. Never present model auth as proof of
the worker's task account. Resolve environment collisions; if shell startup or
harness settings override the trial, state the effective identity and settings.
Do not launch while NOT READY or against a task account that the user has not
confirmed is a sandbox. For explicit profiles, environment-local preflights
check the actual runtime during preparation.

## When the user wants a comparison

- **Matrix**: which harnesses and, per harness, which models; configs (`clean` vs `user`); repetitions. "Claude Code with sonnet and opus, codex with example-model-a and example-model-b" is two `[[cells]]`, one with `models = ["sonnet", "opus"]` and one with `models = ["example-model-a", "example-model-b"]`: one cell per model, ids `<cell>-<model>`, and `--cells <cell>` selects the group. Pass model names exactly as the user says them; the harness resolves them and fails at run time on an unknown one. A cell without a model runs the harness default, which with `config = "user"` or `auth = "inherit"` can come from the user's own harness settings; say so. Run `ajx plugins` to show what is installed and which adapters are verified live. Prefer one clean cell using the current host's CLI when an adapter is available, with repetitions 1; otherwise choose an installed adapter with the user.

- **Reporter**: keep one reporter harness/model fixed across the matrix. Claude Code, Codex, and Kiro CLI have restricted reporter adapters. Set `[reporter]` explicitly when needed; otherwise the first report-capable worker harness is used. Other coordinator hosts can use those reporters or a custom reporter plugin. State tool and configuration restrictions honestly.

Explain a matrix cell as one agent setup. Keep the task, starting data, success
check, reporter, and limits comparable. Add repetitions or other setups only
within the requested scope; every additional run may incur costs. State intentional
differences in environments, product versions, skills, and measurement coverage.

## Configuration inheritance

Default `auth = "inherit"` may retain harness configuration. Claude Code uses
`--safe-mode` with the caller's config directory for its stored login and applies
`settings.json` environment values; the run records that limitation. Use the
supported auth profile for the intended isolation, not a fabricated
`isolates_config` declaration. Shell startup files can affect legacy local tool
shells. Use doctor and the adapter/profile references to explain what the worker
will actually receive. Explicit environments have stricter authentication rules
in [environments.md](environments.md).
