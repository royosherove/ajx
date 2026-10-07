# Security and privacy

AJX is an early development project. Fixes target the current `main` branch.

Report a vulnerability through the repository's **Security → Report a
vulnerability** option when available. If it is unavailable, open a minimal
issue requesting a private reporting channel. Do not include credentials,
exploit details, real transcripts, personal information, or customer data in a
public issue.

Include the affected revision, operating system, harness version, expected and
actual behavior, and a synthetic reproduction in the private report.

## Running trials

AJX launches agent CLIs and executes setup, verification, and teardown commands.
Workers may run with tool approvals bypassed. Context isolation does not restrict
filesystem, network, or account permissions. Review the trial and use disposable
workspaces and credentials with limited permissions.

Model credentials and task credentials can resolve differently. Run `ajx2 doctor`
and confirm the account the worker's tool shell will use. Teardown attempts do
not guarantee that every resource was removed; investigate failed cleanup and
check the account after a cloud trial.

## Handling output

Reports, raw event streams, identity output, archived workspaces, and harness
configuration may contain sensitive information. AJX redacts selected credential
values and removes known credential files, but arbitrary command output and
task files are not guaranteed to be sanitized. Keep output private and review it
before sharing. Agent and reporter providers receive the prompts and evidence
needed for their work.

The repository's committed fixtures are synthetic. Gitleaks runs in CI and can
run locally before commits. It scans files and Git history without checking
credential validity against a provider.
