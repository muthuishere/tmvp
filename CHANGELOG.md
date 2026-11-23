# Changelog

## 0.2.0 — code mode (2026-11-23)

- **Code mode:** `tmvp code run|check|operations|describe|scripts|docs` — an agent sends one program that calls tmvp operations (in parallel, in loops) and only the final value comes back. Every call still goes through the server: read-only, cost guard, env protection, audit. Writes are `write.<system>.<verb>`, need `--allow-writes` and a reason. Built on clicodemode v0.6.2. Limits per program: `--max-ops` (200), `--max-time` (60s), `--max-memory`, string and array caps; a per-call `timeout` above `--max-time` is refused.
- **Saved scripts:** `<skill dir>/scripts/<name>.js`, run by name with `tmvp code run <name>`, listed by `tmvp code scripts`. `tmvp new skill` scaffolds `scripts/example.js`.
- **Docs:** `tmvp llms code-mode`; the generated `SKILL.md` tells agents to use one program for multi-step investigations; `tmvp-discoverer` offers saved scripts for the investigations the user names; benchmark scripts and numbers in `examples/agent-benchmark`.

## 0.1.0 — first release (2026-11-13)

One CLI + local server that lets coding agents and engineers read — and, on purpose, write — every system of a product across all its environments.

- **23 system types:** postgres, mysql, mongo, redis, elasticsearch, opensearch, kafka, nats, ssh (incl. docker over ssh), k8s, portforward, aws, gcp, az, prometheus, loki, grafana, gh, git, http (+ OpenAPI), grpc (reflection or protoset), web, script.
- **Batch reads across envs:** `tmvp all <system> <verb> … --diff`; select envs by name, glob, list or `type:prod`.
- **Env types:** `type: prod|staging|dev|test|local` (normalised); protection follows type; prod can never be destructive; the first env is the default.
- **Read-only by default:** read-only transactions, method allowlists (gRPC), cost estimates (`too_expensive`), row caps, timeouts; writes only via `tmvp write … --reason`, `--preview`, `--force` for protected envs; audit log (`tmvp logs --writes`).
- **Secrets never reach the agent:** `$(cmd)`, `$file(cmd)` (e.g. sec), `$dotenv(file, KEY)` (built-in parser, repo/config-relative, absolute, `~`, Windows paths) and `${ENV_VAR}`; values live only in the local server's memory.
- **Skills:** `tmvp new skill` (`--from`), generated `SKILL.md` that lists `references/`; `tmvp-discoverer` guided discovery session (`tmvp skill install --discover`, `/tmvp-discoverer`); `tmvp env-keys` (env-file key names only).
- **Install:** `install.sh` / `install.cmd` with checksum verification; release workflow for 6 targets.
- **Docs site** (Astro Starlight) generated from `docs/llms`, plus `examples/agent-benchmark` (three incidents, with and without tmvp).
