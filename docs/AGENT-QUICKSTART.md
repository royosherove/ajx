# Set up AJX for your user

This guide is for the agent receiving the README's copy-and-paste prompt.
Install AJX, load its released skill, and help the user reach a first useful
report. Execute the setup steps yourself when your tools and the user's
permissions allow it. Ask only for information or interaction you cannot
provide, such as the intended task, access boundaries, or signing in.

The user needs macOS or Linux on ARM64 or x86-64, Bash, `curl`, internet access,
and an agent with file and shell access. AJX installation needs no GitHub login,
Go, Python, Git checkout, administrator access, or model credentials. A real
trial additionally needs a supported worker CLI and reporter with usable model
authentication. A simulated demo does not.

## 1. Check the current setup

Check the operating system, architecture, shell tools, and whether
`$HOME/.local/bin/ajx` or another `ajx` executable is already installed.
Run `ajx version` when available. Check for an existing skill without replacing
custom files or reading credentials.

Use the user's current project and supplied goal. If neither is available, ask
for a project or offer the simulated demo. Do not audit the product, solve the
task, or search for defects before the worker's attempt; those findings could
contaminate its task prompt.

On an unsupported platform, explain the platform requirement and stop native
installation. Do not silently switch machines or provision cloud resources.
If shell or network access is unavailable, identify that specific blocker and
provide the next action; do not claim installation succeeded.

## 2. Install the stable CLI and matching skill

For a new installation, run this in Bash or Zsh:

```sh
bash -o pipefail -c 'curl -fsSL https://github.com/royosherove/ajx/releases/latest/download/install.sh | bash' && export PATH="$HOME/.local/bin:$PATH"
```

The installer selects the latest stable public release, verifies its package,
and installs both the native CLI and the complete matching skill. Do not clone
`main`, build from source, or copy only `SKILL.md` as an installation shortcut.
The native release needs no Python.

For an existing managed installation, run `ajx update --check --json`.
When a stable update is available, use `ajx update --with-skills` to keep the CLI
and skill together. If that command is unavailable, use the installer above.
Do not downgrade a newer development installation automatically.

If the installer reports conflicting unmanaged files, preserve them. Explain
the conflict and the printed replacement command, which backs up the existing
files. If the user chooses replacement, execute that command for them. Never
delete their existing skills as a workaround.

Confirm success with:

```sh
"$HOME/.local/bin/ajx" version
```

An export in one shell tool call may not persist into the next. Use the
absolute executable path when needed. Report the installed version and paths,
not just the installer's exit code.

## 3. Read the installed skill and continue here

For the default installation, read:

```text
~/.local/share/ajx/current/skill/SKILL.md
```

Expand `~` to the user's home directory using your file tools. Resolve the
skill's relative references from that same `skill/` directory and use its
bundled `bin/ajx` executable as instructed. The installer also registers this
skill under `~/.agents/skills/ajx`, `~/.claude/skills/ajx`, and
`~/.kiro/skills/ajx`. For a custom installation, use its reported location.

Follow the **installed release's skill**, including its briefing, environment,
and console references, for the trial. This web guide only bootstraps setup.
Do not substitute trial instructions from an unreleased source checkout.

A new slash command may not appear in a session opened before installation.
Read the local file directly and continue when the host permits it; the user
does not need to copy the same task into another chat just for discovery.
If host rules require a fresh session, give a short resume prompt containing
the skill path, project, task, decisions already made, and next step.
For a custom Kiro agent, explain the required skill resource entry without
overwriting unrelated configuration; see [harness support](HARNESS-SUPPORT.md).

## 4. Resolve prerequisites for one small trial

Reuse the installed skill's setup checks. Prefer the current host's CLI if its
worker and reporter adapters are available. Use an installed alternative only
after explaining the choice. Keep one worker, one attempt, a default model,
and one supported reporter unless the user asks for a comparison.

If a required CLI or task tool is missing, consult its official installation
instructions and install it within the authorized scope. Prefer a supported
user-local installation; do not install every harness. Explain any step that
needs administrator privileges or changes outside that scope. Do not ask the
user to type commands you can run.

Check authentication through the installed skill's supported readiness checks.
A desktop session is not proof that a separate worker CLI is authenticated.
When sign-in is missing, explain the exact next step and let the user complete
the provider's flow. Never request pasted secrets, inspect credential values,
or make paid model calls merely to probe readiness. Do not start an interactive
login process without the user's agreement.

Reuse the project's supplied task and materials; help choose one small,
observable outcome if needed. Prepare a neutral task, deterministic success
check, and a fresh disposable workspace. Explain its effective filesystem and
network permissions: a local directory is not a sandbox. Use the installed
skill's environment guidance when stronger isolation is required.

For the README prompt, show the plan and costs before launch. Write the trial
files, run `validate` and `doctor`, and resolve readiness problems. Open the
AJX Console so the user can review and start the run, or accept approval in
chat through the skill's documented console flow. Preserve the task and access
decisions they already supplied.

## 5. Show a useful result

After the authorized attempt, follow the installed skill's delivery steps.
Open the matrix `index.html`, the run's `report.html`, and
`pretty-journey.html`. Lead with whether the requested outcome was verified,
the top evidence-supported product asks, and one useful next change.
Explain any incomplete reporting, blocked execution, or failed cleanup.
Keep reports local unless the user asks to share them.

If the user requested the simulated demo, or chooses it while real-trial
prerequisites are unavailable, run:

```sh
"$HOME/.local/bin/ajx" console demo trials/console-demo
```

Use a new directory if that demo already exists. Give the user the console URL
and ask them to press **Check and start**. Clearly label it simulated: it
demonstrates the reports without worker credentials or model calls and does
not evaluate their project.

For static fictional examples, run:

```sh
"$HOME/.local/bin/ajx" example trials/example
```

Open `trials/example/index.html`. A demo is an optional way to explore AJX;
it must not be presented as a completed real trial.
