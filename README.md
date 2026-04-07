# claude-context-proxy

Lightweight local MITM proxy between Claude Code and `api.anthropic.com`.
Captures token usage from every API response and reports context consumption
in real time — without modifying Claude Code's behaviour.

Designed for **subscription users** (Claude Team, Claude.ai) who don't pay
per token but want to understand how much context window they're consuming
per session, per request, and per tool call.

## How it works

```
claude-ctx  (fish alias)
  ↓  HTTPS_PROXY=http://localhost:7474
  ↓  NODE_EXTRA_CA_CERTS=~/.config/claude-context-proxy/ca.crt
claude-context-proxy daemon  (localhost:7474)
  ↓  MITM: terminates TLS, inspects response, re-encrypts
api.anthropic.com
  ↓  SSE response with usage.input_tokens / cache tokens
proxy extracts token counts + writes stats
  ↓
~/.cache/claude-context-proxy/session.json   ← live session state
~/.cache/claude-context-proxy/history.jsonl  ← per-request log
~/.files/states/ctx.json                     ← statusline state (atomic write)
```

## Quick start

```bash
# 1. Install
make install

# 2. Generate CA cert and install to macOS keychain (one-time)
claude-context-proxy setup

# 3. Start the daemon
claude-context-proxy start

# 4. Run Claude Code through the proxy
#    (add HTTPS_PROXY + NODE_EXTRA_CA_CERTS, or use the claude-ctx alias)
HTTPS_PROXY=http://localhost:7474 \
NODE_EXTRA_CA_CERTS=~/.config/claude-context-proxy/ca.crt \
claude

# 5. Check usage
claude-context-proxy stats
```

### Fish alias (recommended)

```fish
# ~/.config/fish/functions/claude-ctx.fish
function claude-ctx
    set -lx HTTPS_PROXY http://localhost:7474
    set -lx NODE_EXTRA_CA_CERTS $HOME/.config/claude-context-proxy/ca.crt
    claude $argv
end
```

## Commands

| Command | Description |
|---------|-------------|
| `start` | Start proxy as background daemon |
| `stop` | Stop the running daemon |
| `restart` | Stop and restart the daemon |
| `setup` | Generate CA cert and install to macOS keychain |
| `log` | Tail the daemon log |
| `stats` | Current session — tokens, context windows consumed |
| `sessions` | All past sessions |
| `history` | Per-request log (`--last`, `--today`, `--since=DATE`) |
| `statusline` | Compact one-liner for shell prompt embedding |
| `config` | Show effective config |
| `version` | Print version |

## Stats output

```
Session: 2026-04-07 14:28 (47m)
─────────────────────────────────────
Requests:       38
Input tokens:   284,391  (1.4× windows)
Output tokens:   18,204
Context ratio:  15.6:1  (in:out)
─────────────────────────────────────
Top context spikes (last 10 req):
  req #3   82,341 tokens  (41% of window)
  req #12  61,204 tokens  (31% of window)
```

## Configuration

Config file: `~/.config/claude-context-proxy/config.json`

All fields can be overridden with environment variables:

| Env var | Default | Description |
|---------|---------|-------------|
| `CTX_PORT` | `7474` | Proxy listen port |
| `CTX_SESSION_GAP_MINUTES` | `30` | Inactivity gap before session resets |
| `CTX_MODE` | `context` | Display mode: `context` or `cost` |
| `CTX_INSPECT` | `0` | Set to `1` to enable tool call attribution |
| `CTX_DEBUG` | `0` | Set to `1` to enable debug logging |
| `CTX_STATUSLINE_PATH` | `~/.files/states/ctx.json` | Statusline output path (empty to disable) |

## Build

```bash
make build    # compile
make install  # install to ~/.local/bin + codesign (macOS)
make test     # run tests
make bench    # benchmarks
```

## Notes

- Token counts include prompt cache tokens (`cache_read_input_tokens` +
  `cache_creation_input_tokens`) — essential for accurate context tracking
  since Claude Code aggressively caches tool definitions and system prompts
- The proxy only MITMs `api.anthropic.com`; all other HTTPS traffic is
  tunnelled transparently
- CA private key never leaves `~/.config/claude-context-proxy/ca.key` (mode 0600)
