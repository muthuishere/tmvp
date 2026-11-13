# tmvp

**One safe connection for your coding agent to every system of your app, across every environment.**

Claude, Codex or any coding agent — or you — can look into logs, run database queries and inspect queues across dev, staging and prod in one call, under a policy that keeps your systems up and your credentials untouched.

📖 Docs: **https://muthuishere.github.io/tmvp/**

```sh
tmvp all db query "select status, count(*) from orders where created_at > now() - interval '1 hour' group by 1" --diff
```
One call, every environment, only the rows where environments disagree.

## Why

- **Nothing goes down.** Reads run read-only (read-only transactions, method allowlists), with row caps and timeouts. A query estimated to run too long is refused before it starts (`too_expensive`).
- **No silent writes.** A read can never write (`policy_blocked`). Writes go through `tmvp write <env> <system> … --reason "…"` — one target, `--preview` first, `--force` on protected envs — and every write lands in the audit log (`tmvp logs --writes`).
- **Credentials untouched.** `config.yaml` holds references, never values: `$dotenv(.env.prod, DATABASE_URL)`, `${ENV_VAR}`, or any secret manager command (sec, 1Password, LastPass, Bitwarden, Vault, AWS, GCP, Azure, Doppler). tmvp's local server resolves them; the agent only sees names.
- **Batch across environments.** `tmvp all …`, `tmvp type:prod …`, `--diff`.
- **Fast.** A background server keeps connection pools, ssh tunnels and secrets warm between calls and runs environments in parallel.

## Install

```sh
# macOS / Linux
curl -fsSL https://raw.githubusercontent.com/muthuishere/tmvp/main/install.sh | sh
# Windows (cmd)
curl -fsSLO https://raw.githubusercontent.com/muthuishere/tmvp/main/install.cmd && install.cmd
# from source (Go 1.26)
go install github.com/muthuishere/tmvp/cmd/tmvp@latest
```
The installers download the release binary for your OS, verify its checksum and never use sudo.

## Quick start

Let your agent build the setup with you:
```sh
tmvp skill install --discover     # once
```
then, in your product repo, ask your agent: `/tmvp-discoverer`. It scans the repo (env-file *key names* only, never values), asks which projects and environments you have and how each system is reached, shows a plan, and writes `.agents/skills/<project>-tmvp/` only after you approve.

Or by hand:
```sh
tmvp new skill acme --envs dev,staging,prod
```
```yaml
# .agents/skills/acme-tmvp/config.yaml
envs:
  prod:
    type: production                  # always protected
    secrets:
      pg_url: "$dotenv(.env.prod, DATABASE_URL)"
    systems:
      db: { type: postgres, read: { dsn: "${secrets.pg_url}" } }
```
```sh
tmvp --skill .agents/skills/acme-tmvp check
tmvp --skill .agents/skills/acme-tmvp all db query "select 1"
```

## Systems

postgres · mysql · mongo · redis · elasticsearch · opensearch · kafka · nats · ssh (incl. docker over ssh) · k8s · portforward · aws · gcp · az · prometheus · loki · grafana · gh · git · http (+ OpenAPI) · grpc · web · script — chained through jump hosts and containers with `via:`.

## Does it help? A small benchmark

Three planted incidents in a four-environment demo app, the same prompt for every agent ([examples/agent-benchmark](examples/agent-benchmark)):

| Scenario | Haiku + tmvp | Haiku, `.env` only | Fable, `.env` only | Opus, `.env` only |
|---|---|---|---|---|
| Orders stuck after payment (prod) | 32s · 0 leaks | 44s · 8 leaks | 47s · 0 | 159s · 0 |
| Missed migration (staging) | 30s · 0 | 33s · 6 | 163s · 0 | 229s · 0 |
| Trailing space in 1 of ~300 config values (prod) | 47s · 0 | 72s · 9 | 53s · 2 | 41s · 0 |

Every agent found every root cause — including Haiku with tmvp. Without tmvp, the database password reached the model (Haiku every time, Fable once by reading the repo). With tmvp: zero.

## Learn more

- `tmvp verbs [type]` · `tmvp llms [topic]` · any command `--llmhelp` — docs built into the binary
- [llms.txt](llms.txt) — docs index for agents
- [CHANGELOG.md](CHANGELOG.md)
