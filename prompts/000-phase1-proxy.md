---
id: "000"
title: "Phase 1 — minimal token-counting proxy"
repo: "claude-context-proxy"
model: "sonnet"
depends_on: []
budget_usd: 2.00
---

# 000 — Phase 1: Minimal Token-Counting Proxy

## IMPORTANT: Progress Logging
Before doing ANYTHING else, create the progress log file. After EVERY step, append a line:
```bash
echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] STEP N: description — PASS/FAIL" >> /Users/projects/personal/claude-context-proxy/logs/000-phase1-proxy.progress.log
```

## Context — what this tool is
A lightweight local proxy that sits between Claude Code and `api.anthropic.com`.
It reads `x-anthropic-input-tokens` / `x-anthropic-output-tokens` response headers,
accumulates per-session totals, and writes a live summary to disk.

```
Claude Code
  ↓  ANTHROPIC_BASE_URL=http://localhost:7474
claude-context-proxy (this tool)
  ↓  forwards to api.anthropic.com
Anthropic API
  ↓  response with x-anthropic-*-tokens headers
proxy logs + writes stats
  ↓
~/.cache/claude-context-proxy/session.json   ← live state
~/.cache/claude-context-proxy/history.jsonl  ← per-request log
```

Read `~/.local/projects/claude-context-proxy.md` for the full spec before starting.

## Phase 1 scope (implement all of this)

### Core proxy
- Single Go file `main.go` — HTTP server on `:7474`
- Forward all requests to `https://api.anthropic.com` verbatim
  - Preserve all request headers (including `Authorization`) unchanged
  - Preserve request body unchanged
  - **Critical**: streaming responses (`text/event-stream`) must be forwarded as SSE,
    not buffered — use `http.Flusher` to flush each chunk immediately
- Extract `x-anthropic-input-tokens` and `x-anthropic-output-tokens` from responses
  (present on both streaming and non-streaming; on SSE they appear as response headers)

### Stats tracking
- `~/.cache/claude-context-proxy/session.json` — written after every request:
  ```json
  {
    "started_at": "2026-04-06T14:32:00Z",
    "requests": 38,
    "input_tokens": 284391,
    "output_tokens": 18204,
    "last_request_at": "2026-04-06T15:19:00Z"
  }
  ```
- `~/.cache/claude-context-proxy/history.jsonl` — one line per request:
  ```json
  {"ts":"2026-04-06T14:32:01Z","input":42381,"output":1204,"path":"/v1/messages"}
  ```
- Session resets when gap between requests exceeds 30 minutes (configurable via `CTX_SESSION_GAP_MINUTES` env)

### Stats CLI (`ctx-proxy` or subcommand of the binary)
Print a formatted summary to stdout:
```
Session: 2026-04-06 14:32 (47m)
─────────────────────────────────────
Requests:       38
Input tokens:   284,391  (~$0.85)
Output tokens:    18,204  (~$0.27)
─────────────────────────────────────
Top input spikes (last 10 req):
  req #3   82,341 tokens
  req #12  61,204 tokens
```

Cost estimate: input $3.00/Mtok, output $15.00/Mtok (claude-sonnet-4 pricing — hardcode for now).

Invocation: `claude-context-proxy stats` (subcommand) or a separate `ctx-proxy` binary — your choice.

### Go module setup
- `go mod init github.com/pdonorio/claude-context-proxy`
- No external dependencies for Phase 1 — stdlib only

## Output files
```
main.go          ← proxy server + stats CLI
go.mod
go.sum           ← (empty if no external deps)
Makefile         ← build, run, install targets
README.md        ← how to start, ANTHROPIC_BASE_URL usage
```

## Startup / install
The binary should install to `~/.local/bin/claude-context-proxy`.
`make install` should build and copy it there.

## Tests
Write `main_test.go` with at least:
- Token header extraction works (mock response with headers)
- Streaming passthrough doesn't buffer (mock SSE response)
- Session JSON is written correctly
- `stats` output is correct for known history

Run: `go test ./...`

## Verification checklist
- [ ] `go build ./...` succeeds
- [ ] `go test ./...` passes
- [ ] `claude-context-proxy &` starts without error
- [ ] `ANTHROPIC_BASE_URL=http://localhost:7474 claude --version` works (proxied successfully)
- [ ] After a real Claude Code session: `claude-context-proxy stats` shows non-zero token counts
- [ ] `~/.cache/claude-context-proxy/history.jsonl` has entries

## Commit instructions
- Commit after scaffold (go.mod + empty main.go)
- Commit after proxy core works
- Commit after stats CLI works
- Commit after tests pass
- Push after all commits

## Report
Write `reports/000-phase1-proxy.report.md` with:
- What was built
- Any design decisions made
- Test results
- Notes for phases 2–4
