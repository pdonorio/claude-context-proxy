---
id: "002"
title: "Phase 3 — fish statusline integration"
phase: "phase-3"
repo: "claude-context-proxy"
model: "sonnet"
depends_on: ["001"]
budget_usd: 1.00
---

# 002 — Phase 3: Fish Statusline Integration

## IMPORTANT: Progress Logging
Before doing ANYTHING else, create the progress log file. After EVERY step, append a line:
```bash
echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] STEP N: description — PASS/FAIL" >> /Users/projects/personal/claude-context-proxy/logs/002-phase3-statusline.progress.log
```

## Context
Phase 2 added session history. Now we want the proxy to write a small JSON file
after every request so the fish shell statusline can display live token counts
without calling any subprocess.

Read `main.go` before starting. Do not rewrite what already works.

## Scope

### 1. Statusline state file
After every `saveSession` call, also write `~/.files/states/ctx.json`:
```json
{
  "input_tokens": 284391,
  "output_tokens": 18204,
  "requests": 38,
  "cost_usd": 1.13,
  "session_id": "1744048320",
  "updated_at": "2026-04-06T15:19:00Z"
}
```

- Create `~/.files/states/` if it doesn't exist
- Write atomically: write to `.ctx.json.tmp` then `os.Rename` to `ctx.json`
- If the directory doesn't exist and can't be created, log a warning and continue — never crash the proxy over this

### 2. Configurable path
- Default: `~/.files/states/ctx.json`
- Override via env var: `CTX_STATUSLINE_PATH=/path/to/file.json`
- If set to empty string `""`, disable statusline writes entirely

### 3. `statusline` subcommand
```
claude-context-proxy statusline
```
Reads `~/.files/states/ctx.json` and prints a compact one-liner suitable
for embedding in a fish prompt or status bar:
```
⬡ 284k in · 18k out · $1.13
```
- Numbers ≥ 1000 shown as `Nk` (rounded); ≥ 1,000,000 as `NM`
- If file missing or stale (updated_at > 35 min ago): print nothing and exit 0
- Exit code 0 always (statusline callers ignore errors)

### 4. `statusline --json` flag
Print the raw `ctx.json` contents for scripting.

## Output files changed
- `main.go` — add statusline writer + subcommand
- `main_test.go` — add tests

## Tests to add
- `TestStatuslineWrite` — after `recordTokens`, verify `ctx.json` written with correct fields
- `TestStatuslineAtomic` — temp file cleaned up; no partial writes visible
- `TestStatuslineCmd` — known `ctx.json`; verify compact output format
- `TestStatuslineDisabled` — `CTX_STATUSLINE_PATH=""` skips write

## Verification checklist
- [ ] `go build ./...` succeeds
- [ ] `go test ./...` passes
- [ ] After a proxied request, `~/.files/states/ctx.json` exists with correct data
- [ ] `claude-context-proxy statusline` prints compact summary
- [ ] `CTX_STATUSLINE_PATH=/tmp/test.json` writes to the override path
- [ ] `CTX_STATUSLINE_PATH=""` writes nothing (no crash)

## Commit instructions
- Commit after statusline writer
- Commit after subcommand + tests
- Push when done

## Report
Write `reports/002-phase3-statusline.report.md` with what was built, decisions, test results.
