---
id: "005"
title: "Phase 6 — refactor & production hardening"
phase: "phase-6"
repo: "claude-context-proxy"
model: "sonnet"
depends_on: ["004"]
budget_usd: 2.00
---

# 005 — Phase 6: Refactor & Production Hardening

## IMPORTANT: Progress Logging
Before doing ANYTHING else, create the progress log file. After EVERY step, append a line:
```bash
echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] STEP N: description — PASS/FAIL" >> /Users/projects/personal/claude-context-proxy/logs/005-phase6-refactor.progress.log
```

## Context
`main.go` has grown across 5 phases into a ~500+ line single file.
This phase splits it into a proper package structure, hardens error handling,
and adds benchmarks — without changing any external behaviour.

Read ALL of `main.go` and `main_test.go` before starting.
All existing tests must still pass unchanged after the refactor.

## Scope

### 1. Package split
Reorganise into:
```
main.go                     ← entry point only, arg dispatch
internal/
  proxy/
    handler.go              ← proxyHandler, SSE tee-parser
  stats/
    session.go              ← Session, HistoryEntry, recordTokens, loadSession, saveSession
    history.go              ← appendHistory, readHistory, filtering
    statusline.go           ← statusline writer
  cli/
    stats.go                ← cmdStats, cmdSessions, cmdHistory, cmdStatusline, cmdConfig
  config/
    config.go               ← Config struct, Load, defaults, env overrides
```

Rules:
- No circular imports
- `internal/` packages are not exported (Go convention enforced by tooling)
- `main.go` stays small: parse args, call into `cli`

### 2. Error handling audit
Go through every `_ =` and silent error discard. For each:
- If it's I/O on the stats path (disk writes): log with `log.Printf` and continue — never crash
- If it's in the proxy hot path: return a proper HTTP error to the client
- Remove any remaining `log.Fatal` outside of `main()`

### 3. Graceful shutdown
Handle `SIGINT` / `SIGTERM`:
- Flush any pending `recordTokens` goroutines (add a `sync.WaitGroup`)
- Write final `session.json` and `ctx.json` before exit
- Exit cleanly within 5 seconds

### 4. Benchmark tests
Add to `*_test.go`:
- `BenchmarkProxyHandler` — measure overhead of the proxy layer (mock upstream)
- `BenchmarkRecordTokens` — measure stats write throughput under concurrent load
- `BenchmarkSSETeeParser` — measure tee-parser overhead vs raw copy

Run with `go test -bench=. -benchtime=3s ./...` and include results in the report.

### 5. Makefile improvements
```makefile
build:    go build -ldflags="-s -w" -o claude-context-proxy .
install:  build + cp to ~/.local/bin
test:     go test ./...
bench:    go test -bench=. -benchtime=3s ./...
lint:     go vet ./... (no external linters required)
clean:    rm -f claude-context-proxy
```

## Tests
All existing tests must pass. No test logic should change — only import paths.
New benchmark functions added alongside existing tests.

## Verification checklist
- [ ] `go build ./...` succeeds
- [ ] `go test ./...` passes — same count as before refactor
- [ ] `go vet ./...` clean
- [ ] `go test -bench=. ./...` runs and prints results
- [ ] `make install` still works
- [ ] `claude-context-proxy stats` / `sessions` / `history` / `statusline` / `config` all work
- [ ] Proxy handles SIGTERM gracefully (kill -TERM <pid> → clean exit)

## Commit instructions
- Commit after package scaffold (empty files, imports wired)
- Commit after each internal package filled in (proxy, stats, cli, config)
- Commit after error handling audit
- Commit after graceful shutdown
- Commit after benchmarks + Makefile
- Push when done

## Report
Write `reports/005-phase6-refactor.report.md` including:
- Final package structure (tree)
- Benchmark results table
- Any surprising findings from the error handling audit
