# The local server

One binary, two roles. Every `tmvp` call is a thin client that talks over a unix socket to a
per-user server which it auto-starts (detached) when needed. The server keeps per-product state:
secrets (memory only), connection pools, tunnels, browser sessions, the audit writer.

- `tmvp server status` — loaded products, connections, tunnels, secret slots (names only)
- `tmvp server stop` — drop everything (all secrets and connections); `start` / `run` (foreground)
- `tmvp secrets reload` — rerun the secret commands after a rotation; `tmvp secrets clear`
- Idle shutdown after 30 min without a request (`server: { idle: 30m }` or `TMVP_IDLE`).
- Config file changes are picked up on the next call (changed secrets refetched, affected pools rebuilt).
- Socket `~/.tmvp/run/tmvp.sock` (0600, same-UID peers only; named-pipe fallback on old Windows).

Environment: `TMVP_HOME` (state dir, default `~/.tmvp`), `TMVP_SOCKET` (socket path; keep it
under 104 bytes on macOS), `TMVP_NO_SERVER=1` (run in-process, no socket — tests/CI),
`TMVP_PRODUCT` (product for the lookup order), `TMVP_AGENT` (agent name in the audit).
Commands that never need the server or a config: `--llmhelp`, `llms`, `verbs`, `version`,
`new skill`, `skill lint`.
