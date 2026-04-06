---
id: "003"
title: "Phase 4 — token breakdown by tool call"
phase: "phase-4"
repo: "claude-context-proxy"
model: "sonnet"
depends_on: ["002"]
budget_usd: 2.00
---

# 003 — Phase 4: Token Breakdown by Tool Call

## IMPORTANT: Progress Logging
Before doing ANYTHING else, create the progress log file. After EVERY step, append a line:
```bash
echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] STEP N: description — PASS/FAIL" >> /Users/projects/personal/claude-context-proxy/logs/003-phase4-tool-breakdown.progress.log
```

## Context
Phase 3 added statusline integration. Now we want to attribute token usage
to specific tool calls by parsing the SSE stream inline without buffering.

Read `main.go` fully before starting. The SSE passthrough is in `proxyHandler`.

## Background: Anthropic SSE format
Each SSE event is:
```
event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01...","name":"Read","input":{}}}

event: message_delta
data: {"type":"message_delta","delta":{},"usage":{"output_tokens":42}}
```
The `message_delta` event with `usage` field appears at the end of a streaming response
and gives final output token count. `content_block_start` with `type: tool_use` names the tool.

## Scope

### 1. SSE tee-parser (inspect mode only)
Add a `CTX_INSPECT=1` env var (or `--inspect` flag to the server start) that enables
inline SSE parsing. When disabled (default), proxy is unaffected — zero overhead.

When enabled:
- Tee the SSE body through a parser as it streams to the client
- Parse `content_block_start` events to capture tool names in order
- Parse `message_delta` events to capture final output tokens per response
- Do NOT buffer; the tee must not delay chunks reaching the client

### 2. Extend `HistoryEntry` with tool attribution
```json
{
  "ts": "...",
  "input": 82341,
  "output": 1204,
  "path": "/v1/messages",
  "session_id": "...",
  "tools": ["Read", "Glob", "Bash", "Read"]
}
```
`tools` is the ordered list of tool names called in that request (may repeat).
Only populated when `CTX_INSPECT=1`.

### 3. `stats --tools` flag
When passed, show a tool call frequency table below the existing stats output:
```
Tool call breakdown (current session):
  Bash       34 calls
  Read       28 calls
  Glob       11 calls
  Edit        9 calls
  Agent       3 calls
```
Computed by reading `history.jsonl` for current session entries that have `tools` set.

### 4. Update `statusline` output when inspect data available
If the last history entry has tools, append the most-called tool to the statusline:
```
⬡ 284k in · 18k out · $1.13 · Bash×34
```

## Output files changed
- `main.go` — tee-parser, extended HistoryEntry, `stats --tools`, statusline update
- `main_test.go` — add tests

## Tests to add
- `TestSSETeeParser` — feed a real-looking SSE stream through the parser; verify tool names and output tokens extracted correctly without modifying byte output
- `TestHistoryToolField` — verify `tools` field written and parsed round-trip
- `TestStatsToolsFlag` — known history with tools; verify breakdown output

## Verification checklist
- [ ] `go build ./...` succeeds
- [ ] `go test ./...` passes
- [ ] `CTX_INSPECT=0` (default): no SSE parsing, benchmark shows zero overhead path
- [ ] `CTX_INSPECT=1`: after a Claude session, `history.jsonl` entries have `tools` field
- [ ] `claude-context-proxy stats --tools` shows tool frequency table

## Commit instructions
- Commit after SSE tee-parser (no history changes yet)
- Commit after HistoryEntry extension + `stats --tools`
- Commit after statusline update + all tests
- Push when done

## Report
Write `reports/003-phase4-tool-breakdown.report.md` with what was built, design decisions (especially around zero-overhead default path), test results.
