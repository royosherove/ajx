# Working on AJX

Read README.md, CONTRIBUTING.md, and SECURITY.md before changing behavior.
The skill lives in `skills/ajx`. The native CLI is `cmd/ajx`; the Python
alternative is `skills/ajx/bin/ajx`. Release installations bundle the native
executable inside the skill and keep the Python launcher as `bin/ajx-python`.
AJX v1 is the first public version. Keep public docs focused on user needs and
shipped behavior; exclude private case studies, internal planning, and
unsupported performance or launch claims.

Name branches by purpose, such as `feature/<topic>`, `bug/<topic>`,
`docs/<topic>`, or `chore/<topic>`. Do not use `codex/` or other agent-name
prefixes.
Preserve the user's configured Git identity for agent-assisted commits.
Do not add agent authors or co-author trailers.

Run `python3 -I -m unittest discover -s skills/ajx/tests -p 'test_*.py'` for relevant code changes.
Use synthetic fixtures and fake harnesses. Do not run live agent or cloud trials
as part of ordinary tests.
Container integration uses a preloaded image selected by
`AJX_TEST_CONTAINER_IMAGE`; it runs synthetic processes without model credentials.
For Go changes, run `go test ./...`. For installation or packaging changes,
run `.github/scripts/test_install.py` and `.github/scripts/test_release.py`;
set `AJX_INSTALL_TEST_PACKAGE` to a native package directory to exercise the
installed console and headless demo.

Preserve evidence provenance, honest limitations, task/reporting separation,
resume semantics, and credential cleanup. Keep the runtime standard-library-only
unless a dependency is deliberately agreed.

Never commit real transcripts, reports, customer data, credentials, personal
paths, or local operations configuration. Scan the staged content and history.
Do not bypass the security hook to force a commit through.

Do not change repository visibility, publish packages, or create releases
without the user's explicit request.
