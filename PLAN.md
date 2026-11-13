# tmvp — Build plan (2026-11-10)

Source of truth: `docs/DESIGN.md`. Decisions: `HANDOFF.md`. Go 1.26, pure Go (`CGO_ENABLED=0`), Taskfile, `sec` for secrets.

## Phase 0 — serial (lead)
Repo, go module, Taskfile, AGENTS.md/CLAUDE.md, .gitignore, this plan.

## Phase 1 — serial (agent: core)
The frozen contracts every other agent builds on:
- `internal/config` — load + lookup order, `vars`/`secrets`/`envs`/`systems`, `${…}` after parse,
  `$(cmd)` / `$file(cmd)` / `${file:secrets.x}`, labels, `via` chains, selectors (`prod.*`, `*.db`, `-l`),
  `validate` (names, collisions, cycles, literal secrets), `show --resolved` (masked).
- `internal/secret` — lazy runner (parallel, never interactive), expiry JSON, in-memory store keyed by
  product, file materialisation (memfd on Linux, else 0600 temp per call), streaming redactor.
- `internal/connector` — `Connector` interface + registry (`Register("postgres", New)`), `Request`/`Row` streaming.
- `internal/protocol`, `internal/server`, `internal/client` — unix socket (named-pipe fallback), auto-start
  detached, 30 min idle, product isolation, JSONL streaming, exit codes.
- `internal/batch` — parallel per env/system, `env`+`system` columns, `--diff`, partial = exit 4.
- `internal/audit` — JSONL (OTel shape), every command, write intent before / outcome after.
- `internal/login` — `login: {manual, check}` → `login_required` (exit 2).
- `cmd/tmvp` — cobra; each subcommand in its own file, self-registered (parallel agents never edit the same file).
- `connectors/postgres` — reference connector (read-only tx, guards, `tmvp write`, `--preview`).
Gate: `task build test` green; `task cross` builds 6 targets.

## Phase 2 — parallel (5 agents, separate packages only)
| Agent | Owns |
|---|---|
| A infra | `connectors/{ssh,portforward,k8s}` — via chains, warm tunnels, k8s verbs, kubeconfig from memory |
| B data | `connectors/{mysql,mongo,redis,nats}` |
| C cloud | `connectors/{aws,gcp,az,git,http,prometheus,script}` |
| D web | `connectors/web` — playwright-cli, context.md/pages front-matter, manual browser login, sessions |
| E skill | `tmvp new skill`, `skill add`, `export pg-service|kubeconfig`, `skill sync|lint`, `envs`, `systems`, `llms` + `--llmhelp` (no author skill — 2026-11-10) |
Rules: no edits outside owned dirs (+ own `cmd/tmvp/cmd_<name>.go`); deps were pre-fetched; tests with fakes/testcontainers-free.

## Phase 3 — serial (lead + checker agent)
Integration build/test on all targets, adversarial review (secret leak canary, read-only bypass,
product isolation), fix, docs sync. Success check = DESIGN §12.
