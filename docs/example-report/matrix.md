# AJX matrix: Demo Export CLI / csv-export-example

Task prompt sha256 `b10b4173c2cc0185…`; 3 run(s) across 3 cell(s); order seed 0; parallel 1.

## Runs

| Run | Harness | Model (observed) | Config | Verified | Declared (excerpt) | Stop | Wall clock | Tool calls | Failed | Retries | Output tokens | Asks (defects) | Gates | Journey | Report |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| baseline-codex-r1 | codex synthetic fixture | example-model | clean | **succeeded** | I exported the records and verified that the CSV contains 100 data rows. | completed | 1m 48s | 5 (partial) | 1 | 1 | 2862 (partial) | 3 (0) | 0 | reconstruction | [report](runs/baseline-codex-r1/report.html) |
| headless-kiro-cli-r1 | kiro-cli synthetic fixture | example-model | clean | **failed** | The export is blocked by required approval. I did not create the CSV. | completed | 0m 25s | 2 (partial) | 1 | 0 | n/a (unavailable) | 1 (0) | 1 | reconstruction | [report](runs/headless-kiro-cli-r1/report.html) |
| revised-codex-r1 | codex synthetic fixture | example-model | clean | **succeeded** | I created the CSV and verified all 100 data rows. | completed | 0m 34s | 3 (partial) | 0 | 0 | 1120 (partial) | 1 (0) | 0 | reconstruction | [report](runs/revised-codex-r1/report.html) |

## Comparability

- prompt_sha256: b10b4173c2cc01852b1db682fbfbb91ce20ceedc033cf0633f7686e38630ee76
- product_versions: ['1.2.0', '1.3.0']
- harness_versions: ['codex synthetic fixture', 'kiro-cli synthetic fixture']
- wall_clock_comparable: True
- token_note: output tokens use each model's tokenizer; compare across models as resource usage, not reasoning; per-cell token variance includes only runs with complete (reconciled) usage
- cost_note: cost_estimate_usd (Claude Code) and credits (Kiro) are harness estimates in different units; never summed or sorted across harnesses

## Asks per run

- **baseline-codex-r1** (3): ASK-001 Make the quick-start export command executable; ASK-002 Expose progress while a CSV export is running; ASK-003 Explain when to choose export instead of dump
- **headless-kiro-cli-r1** (1): ASK-001 Check headless authorization before starting an export
- **revised-codex-r1** (1): ASK-001 Expose progress while a CSV export is running
