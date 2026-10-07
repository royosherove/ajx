# jq smoke trial

A two-minute end-to-end check of the ajx2 pipeline on a CLI everyone has (`jq`), with one
Claude Code cell and one Kiro cell on Haiku. Verified live on 2026-10-06: both cells completed
all ten stages, self-narrated, and verified `succeeded`.

```sh
python3 ../../bin/ajx2 doctor trial.toml
python3 ../../bin/ajx2 run trial.toml --no-synthesis
open ajx-reports/jq/jq-live-01/index.html
```
