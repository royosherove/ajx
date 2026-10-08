# Contributing to AJX

Use Python 3.11 or newer on macOS or Linux. The runtime and offline tests use
only the Python standard library.

```sh
python3 -I -m unittest discover -s skills/ajx/tests -p 'test_*.py'
python3 skills/ajx/bin/ajx --help
```

Keep changes focused. Explain the problem, the resulting behavior, and how you
validated it in your pull request. Add regression tests when behavior changes.
Use the existing fake harness for lifecycle tests. Live trials require a
separate, explicit decision about credentials, permissions, and cost.

The suite skips real-container checks unless `AJX_TEST_CONTAINER_IMAGE` names a
preloaded immutable Linux image ID or digest with Python and standard shell
utilities. CI resolves a synthetic test image and exercises those checks on
Linux. They use no agent/model credentials. Keep assertions about enforced
boundaries covered by these integration tests.

Preserve the separation between worker, narrator, and reporter; evidence IDs;
measured versus inferred results; credential cleanup; and the rule that a
resumed trial must not silently execute its task again. See
[the architecture](docs/ARCHITECTURE.md).

## Preventing accidental disclosure

Use synthetic fixtures. Do not commit real agent transcripts, customer content,
cloud identities, private endpoints, credentials, reports, or workstation
configuration. Keep local trials under the ignored `trials/` directory.
Review `git diff --cached` before each commit.

Install Gitleaks 8.30.1, then enable the repository's staged-content hook:

```sh
git config --local core.hooksPath .githooks
python3 -I .github/scripts/test_security.py
gitleaks git --redact --log-opts=--all
```

If you already use Git hooks, integrate `.githooks/pre-commit` into them instead
of replacing your hooks configuration. Gitleaks must be on `PATH`, or you can
set `git config --local ajx.gitleaksPath /absolute/path/to/gitleaks`.
The hook scans staged blobs, including content that differs from the working
tree. It fails if the scanner is missing. GitHub Actions also scans the complete
reachable history with the repository's `.gitleaks.toml` rules.

The additional rules detect personal email addresses, home paths, internal
hostnames, cloud account identifiers, and captured agent environment metadata.
Reserved example email domains, GitHub's no-reply attribution domain, and the
public `noreply@github.com` and `support@github.com` bot addresses are allowed.
Automated checks are a safeguard; review new fixtures and attachments manually
too. Do not add broad exclusions to silence a finding.

Use GitHub's no-reply email for commits if you do not want your personal email
in repository history, and select it as the author email when merging pull
requests. Pull request privacy checks scan the contribution history; runtime
checks exercise GitHub's temporary merge. Report vulnerabilities as described in
[SECURITY.md](SECURITY.md). Contributions are provided under the
[MIT license](LICENSE).
