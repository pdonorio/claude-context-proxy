# claude-context-proxy

Lightweight local proxy between Claude Code and `api.anthropic.com`.
Reads token usage headers from every response and reports per-session consumption.

## How it works

```
Claude Code
  ↓  ANTHROPIC_BASE_URL=http://localhost:7474
claude-context-proxy (this process)
  ↓  forwards to api.anthropic.com
Anthropic API
  ↓  response with x-anthropic-*-tokens headers
proxy writes stats
  ↓
~/.cache/claude-context-proxy/session.json   ← live state
~/.cache/claude-context-proxy/history.jsonl  ← per-request log
```

## Usage

```bash
# Install
make install

# Start the proxy
claude-context-proxy &

# Point Claude Code at it
export ANTHROPIC_BASE_URL=http://localhost:7474
claude

# View stats
claude-context-proxy stats
```

## Stats output

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

## Configuration

| Env var                  | Default | Description                              |
|--------------------------|---------|------------------------------------------|
| `CTX_SESSION_GAP_MINUTES`| `30`    | Minutes of inactivity before session resets |

## Build

```bash
make build    # compile binary
make install  # install to ~/.local/bin/claude-context-proxy
make test     # run tests
```

## Pricing (hardcoded, claude-sonnet-4)

- Input: $3.00 / million tokens
- Output: $15.00 / million tokens
