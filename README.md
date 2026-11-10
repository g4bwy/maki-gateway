# maki-gateway

A web gateway for [maki](../maki): browse, attach, and control maki sessions from a real
web UI (no terminal-in-browser), mobile friendly. Go backend, zero maki Rust changes.

Two engines, one merged session list:

- **web** sessions: gateway spawns `maki acp` per conversation, bridges WebSocket-less SSE out,
  JSON-RPC over stdio.
- **live** sessions: a Lua plugin inside a running maki TUI serves a unix socket; the gateway
  attaches to it (`list`, `prompt`, steer, events, permission bridge).
- **stored** sessions: read from `~/.local/state/maki/sessions/*.jsonl` (never offset-tail across
  rewrites; only newline-terminated records).

Wire contracts: `docs/CONTRACTS.md`. Everything else is implementation detail.

## Run

    go build ./cmd/gateway
    ./gateway -maki maki -addr 127.0.0.1:8765

Run it from the repo root, because the UI files sit in `web/` under it. From any other directory
pass the path: `-web /path/to/maki-gateway/web`. When neither the `-web` value nor a `web`
directory beside the binary holds `index.html`, the gateway says so and exits instead of serving a
page of 404s.

Open http://127.0.0.1:8765. Installing the attach plugin: see `lua/gateway/README.md`.

`MAKI_GATEWAY_TOKEN` gates non-loopback binds only. A loopback bind (127.0.0.1, ::1, localhost)
serves without auth even when that variable is set, so port-forward with care.

## Status

Prototype. Loopback-only unless `MAKI_GATEWAY_TOKEN` is set (non-loopback bind refuses without it).
