# jq smoke trial

A small trial of the AJX pipeline using `jq`, with one Claude Code worker and one
Kiro CLI worker. Install `jq` and the selected agent CLIs first. Review the model
identifiers and credentials in `trial.toml`; the workers and reporter consume model
usage. This is a live trial, separate from the offline test suite.

From the repository root, after adding `skills/ajx/bin` to `PATH`:

```sh
mkdir -p trials
cp -R skills/ajx/examples/jq-smoke trials/jq-smoke
ajx doctor trials/jq-smoke/trial.toml
# Review the plan, permissions, and costs before running:
ajx run trials/jq-smoke/trial.toml --no-synthesis
open trials/jq-smoke/ajx-reports/jq/jq-live-01/index.html
```
