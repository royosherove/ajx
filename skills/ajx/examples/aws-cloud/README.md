# AWS cloud task example

A task that creates real resources in an AWS account, run by Claude Code on Bedrock with two models.
It keeps the two sets of AWS credentials apart and makes cleanup checkable:

- **Model credentials**: `[auth.bedrock-model]`, an isolated `claude-bedrock` profile with its own
  `AWS_PROFILE`. Nothing in `[env]` touches `AWS_*`. The agent's tool shell still inherits the
  model's `AWS_PROFILE` as its default, so a command or SDK script that omits `--profile` acts in the
  model's account; doctor prints that default identity and warns when it differs from the task's.
- **Task credentials**: the profile is named in `task-prompt.md` and passed as `--profile` in
  `identity_cmd`, `[[preflight]]`, `[[verify]]` and `[[teardown]]`.
- **Preflight**: no worker starts unless the task profile authenticates and can list EC2 instances.
- **Teardown**: finds what the agent created by the tag the prompt asks for, and exits non-zero on
  any failure. ajx also flags credential errors in its output, so `ajx status` shows `AUTH`
  rather than `ok` when it could not have cleaned anything up.

Replace `ajx-task-sandbox` (an account you are willing to let an agent change) and `bedrock-model`
with real profile names in `trial.toml` and `task-prompt.md`, then:

```sh
python3 ../../bin/ajx doctor trial.toml
```

Before `ajx run`, doctor must show the task identity you expect in both the worker's tool shell
and the verify/teardown shell, with no FAILED lines. This example has not been run live; the
ajx tests only check that the trial file loads.
