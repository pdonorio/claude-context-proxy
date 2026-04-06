---
id: "001"
title: "Phase 2 — session history & richer CLI"
phase: "phase-2"
repo: "claude-context-proxy"
model: "sonnet"
depends_on: ["000"]
budget_usd: 1.50
---

# 001 — Phase 2: Session History & Richer CLI

## IMPORTANT: Progress Logging
Before doing ANYTHING else, create the progress log file. After EVERY step, append a line:
```bash
echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] STEP N: description — PASS/FAIL" >> /Users/projects/personal/claude-context-proxy/logs/001-phase2-history.progress.log
```

## Context
Phase 1 built a working proxy that writes `session.json` and `history.jsonl`.
The history file has no session boundaries — all requests are in one flat file.

Read `main.go` before starting. Do not rewrite what already works.

## Scope

### 1. Add `session_id` to history entries
- Generate a session ID when a new session starts: `fmt.Sprintf("%d", startedAt.Unix())`
- Add `"session_id"` field to `HistoryEntry` struct and to `Session` struct
- Write it on every `appendHistory` call
- Backwards-compatible: old entries without the field still parse fine

### 2. `sessions` subcommand
```
claude-context-proxy sessions
```
Reads `history.jsonl`, groups entries by `session_id`, prints one row per session:
```
Session             Requests  Input        Output       Cost
2026-04-06 14:32    38        284,391      18,204       $1.13
2026-04-05 09:15    12        91,204        4,011       $0.33
```
Sort newest-first.

### 3. `history` subcommand with filters
```
claude-context-proxy history [--today] [--since=YYYY-MM-DD] [--session=SESSION_ID] [--last]
```
- `--today`: entries from today (local time)
- `--since=DATE`: entries on or after DATE
- `--session=ID`: entries for a specific session_id
- `--last`: entries from the most recent session only (default if no filter given)
- Without flags, defaults to `--last`

Output: one line per request, newest first:
```
2026-04-06 15:19  input=82,341  output=1,204  path=/v1/messages
```

### 4. Update `stats` to show session cost total
Add a `Total cost:` line at the bottom of `stats` output:
```
Total cost:     ~$1.13
```

## Output files changed
- `main.go` — extend existing structs and add subcommands
- `main_test.go` — add tests for sessions grouping, history filters, session_id persistence

## Tests to add
- `TestSessionID` — two requests in same session share ID; new session after gap gets new ID
- `TestSessionsCmd` — known history.jsonl; verifies sessions output groups correctly
- `TestHistoryFilter` — `--today`, `--since`, `--last` return correct subsets

## Verification checklist
- [ ] `go build ./...` succeeds
- [ ] `go test ./...` passes (all existing + new)
- [ ] `claude-context-proxy sessions` shows grouped output
- [ ] `claude-context-proxy history --last` shows last session requests
- [ ] Old `history.jsonl` without `session_id` still parses without panic

## Commit instructions
- Commit after struct changes + session_id generation
- Commit after `sessions` subcommand
- Commit after `history` subcommand + tests
- Push when done

## Report
Write `reports/001-phase2-history.report.md` with what was built, decisions, test results.
