# claude-context-proxy

Lightweight local proxy between Claude Code and api.anthropic.com.
Reads token usage headers from responses and reports per-session consumption.

## What it does
- Listens on localhost:7474, forwards all traffic to api.anthropic.com
- Extracts x-anthropic-input-tokens / x-anthropic-output-tokens from responses
- Writes stats to ~/.cache/claude-context-proxy/session.json + history.jsonl
- CLI: `claude-context-proxy stats` prints a formatted summary

## Usage
```bash
claude-context-proxy &
ANTHROPIC_BASE_URL=http://localhost:7474 claude
claude-context-proxy stats
```

## Running prompts
```bash
cd /Users/projects/personal/claude-context-proxy
pp run        # run next pending prompt
pp status     # show all prompts
```

## Phases
- Phase 1 (000): minimal proxy + stats CLI — Go, stdlib only
- Phase 2 (001): session tracking + `ctx-proxy stats` history query
- Phase 3 (002): statusline integration (fish + .files/states/ctx.json)
- Phase 4 (003): token breakdown by tool call (nice to have)

## Git
GPG signing is disabled for this repo (`commit.gpgsign=false` in `.git/config`).
Do not use `--no-gpg-sign` flag — the config already handles it.

## Language
Go — single binary, no deps, fast startup.
