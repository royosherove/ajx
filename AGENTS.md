# Working on AJX

Read README.md, CONTRIBUTING.md, and SECURITY.md before changing behavior.
The skill lives in `skills/ajx`; the CLI is `skills/ajx/bin/ajx`.
AJX v1 is the first public version. Keep public docs focused on user needs and
shipped behavior; exclude private case studies, internal planning, and
unsupported performance or launch claims.

Run `python3 -I skills/ajx/tests/test_ajx.py` for relevant code changes.
Use synthetic fixtures and fake harnesses. Do not run live agent or cloud trials
as part of ordinary tests.

Preserve evidence provenance, honest limitations, task/reporting separation,
resume semantics, and credential cleanup. Keep the runtime standard-library-only
unless a dependency is deliberately agreed.

Never commit real transcripts, reports, customer data, credentials, personal
paths, or local operations configuration. Scan the staged content and history.
Do not bypass the security hook to force a commit through.

Do not change repository visibility, publish packages, or create releases
without the user's explicit request.
