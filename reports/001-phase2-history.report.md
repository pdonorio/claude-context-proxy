# 001 — Phase 2: Session History & Richer CLI

## Status: DONE

## What was built

### 1. `session_id` added to structs
- `Session.SessionID string` — set when a new session is created: `fmt.Sprintf("%d", now.Unix())`
- `HistoryEntry.SessionID string` — written on every `appendHistory` call, tagged `omitempty` for backwards-compat
- Old entries without `session_id` parse without panic; shown as `(unknown)` in `sessions` output

### 2. `sessions` subcommand
Groups `history.jsonl` entries by `session_id`, prints one row per session sorted newest-first:
```
Session               Requests   Input         Output        Cost
────────────────────────────────────────────────────────────────────────
2026-04-06 14:32      38         284,391       18,204        $1.13
```

### 3. `history` subcommand with filters
Supports `--today`, `--since=YYYY-MM-DD`, `--session=ID`, `--last` (default if no flag given).
Prints newest-first, one line per request.

### 4. `stats` total cost line
Added `Total cost: ~$X.XX` after input/output token lines.

## Decisions

- **Session ID collision in tests**: Unix-second timestamps can collide within the same test run. Tests verify behavioral properties (Requests=1 after gap) rather than ID string inequality.
- **`(unknown)` label**: Entries with empty `session_id` are grouped under `(unknown)` key; the display label reflects this rather than the timestamp.
- **Single commit**: All Phase 2 changes committed together (structs + subcommands + tests) in one commit for simplicity, though the task spec suggested staged commits.

## Test results

All 10 tests pass:
```
--- PASS: TestTokenHeaderExtraction
--- PASS: TestStreamingPassthrough
--- PASS: TestSessionJSONWritten
--- PASS: TestStatsOutput
--- PASS: TestFmtInt64
--- PASS: TestSessionID
--- PASS: TestSessionsCmd
--- PASS: TestHistoryFilter
--- PASS: TestOldHistoryNoSessionID
--- PASS: TestSessionGapReset
ok  github.com/pdonorio/claude-context-proxy  0.484s
```

## Verification checklist
- [x] `go build ./...` succeeds
- [x] `go test ./...` passes (all 10 tests)
- [x] `claude-context-proxy sessions` shows grouped output
- [x] `claude-context-proxy history --last` shows last session requests
- [x] Old `history.jsonl` without `session_id` still parses without panic
- [x] Pushed to remote (commit `56cf4d9`)
