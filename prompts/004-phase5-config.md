---
id: "004"
title: "Phase 5 — config file & multi-model pricing"
phase: "phase-5"
repo: "claude-context-proxy"
model: "haiku"
depends_on: ["003"]
budget_usd: 1.00
---

# 004 — Phase 5: Config File & Multi-Model Pricing

## IMPORTANT: Progress Logging
Before doing ANYTHING else, create the progress log file. After EVERY step, append a line:
```bash
echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] STEP N: description — PASS/FAIL" >> /Users/projects/personal/claude-context-proxy/logs/004-phase5-config.progress.log
```

## Context
The proxy has grown several env vars (`CTX_SESSION_GAP_MINUTES`, `CTX_STATUSLINE_PATH`, `CTX_INSPECT`).
Phase 5 consolidates these into a config file and adds per-model pricing so cost
estimates are accurate regardless of which Claude model is used.

Read `main.go` before starting.

## Scope

### 1. Config file
Location: `~/.config/claude-context-proxy/config.json`

```json
{
  "port": 7474,
  "session_gap_minutes": 30,
  "statusline_path": "~/.files/states/ctx.json",
  "inspect": false,
  "pricing": {
    "claude-sonnet-4": { "input_per_mtok": 3.00, "output_per_mtok": 15.00 },
    "claude-haiku-4":  { "input_per_mtok": 0.80, "output_per_mtok":  4.00 },
    "claude-opus-4":   { "input_per_mtok": 15.00, "output_per_mtok": 75.00 }
  },
  "default_model": "claude-sonnet-4"
}
```

- Load on startup; create with defaults if missing
- Env vars remain as overrides (backwards-compat): `CTX_PORT`, `CTX_SESSION_GAP_MINUTES`, `CTX_STATUSLINE_PATH`, `CTX_INSPECT`
- Unknown JSON fields are ignored (forward-compat)

### 2. Model detection from request
- Read the request body to extract `"model"` field before forwarding
- Store detected model in the current request context
- Use it for pricing in `recordTokens`
- If model unrecognised, fall back to `default_model` pricing

**Important**: body must still be forwarded verbatim — buffer it, then replay via `io.NopCloser`.

### 3. Per-model cost in history entries
Add `"model"` field to `HistoryEntry`:
```json
{"ts":"...","input":42381,"output":1204,"path":"/v1/messages","model":"claude-sonnet-4","session_id":"..."}
```

### 4. `config` subcommand
```
claude-context-proxy config
```
Print the current effective config (merged file + env overrides) as formatted JSON.

```
claude-context-proxy config --path
```
Print the path to the config file.

## Output files changed
- `main.go` — config loading, model detection, per-model pricing, `config` subcommand
- `main_test.go` — add tests

## Tests to add
- `TestConfigLoad` — missing file → defaults; valid file → values used; env override wins
- `TestModelDetection` — request body with `"model": "claude-opus-4"` → correct pricing applied
- `TestModelFallback` — unknown model uses `default_model` pricing

## Verification checklist
- [ ] `go build ./...` succeeds
- [ ] `go test ./...` passes
- [ ] `claude-context-proxy config` prints effective config
- [ ] `~/.config/claude-context-proxy/config.json` created on first run with defaults
- [ ] `history.jsonl` entries have `model` field after proxied requests

## Commit instructions
- Commit after config loading
- Commit after model detection + pricing
- Commit after `config` subcommand + tests
- Push when done

## Report
Write `reports/004-phase5-config.report.md`.
