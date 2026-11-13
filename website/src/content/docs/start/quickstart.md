---
title: Set up your agent
description: Give Claude, Codex or any coding agent safe, batched access to every environment of your app — then let it diagnose.
sidebar:
  order: 3
---

In about five minutes your agent can compare every environment of your app in one call. You set up a skill once; the agent uses it from then on.

## 1. Install tmvp and the discoverer skill

```sh
tmvp version
tmvp skill install --discover     # tmvp-discoverer, for Claude Code / Codex
```

## 2. Let the agent build the skill

In your product repo, ask your agent:

```text
/tmvp-discoverer
```

It scans the repo (read-only; env-file **names** only, via `tmvp env-keys`), asks you which folders form a project, which environments you have (say `dev1, dev2, staging, prod-eu, prod-us`) and how each system is reached, shows a plan, and writes nothing until you approve. Then it writes `.agents/skills/<project>-tmvp/` and proves every system with `tmvp check`.

Prefer to do it by hand? The same result:

```sh
tmvp new skill acme --systems db:postgres,box:ssh --envs dev1,dev2,staging,prod-eu,prod-us
```

```yaml
# .agents/skills/acme-tmvp/config.yaml (excerpt) — references, never values
envs:
  prod-eu:
    type: production
    secrets:
      pg_url: "$dotenv(.env.prod-eu, DATABASE_URL)"     # your existing .env file — nothing to install
    systems:
      box: { type: ssh, host: REPLACE_ME, user: deploy, key: "${file:secrets.box_key}" }
      db:  { type: postgres, via: box, read: { dsn: "${secrets.pg_url}" } }
```

### Where the secrets come from — pick what you already use

tmvp's local server resolves each reference and keeps the value in memory; your agent only ever sees the name. Mix sources per secret.

| You keep secrets in… | Install (once) | In config.yaml |
|---|---|---|
| `.env` files in the repo | nothing | `"$dotenv(.env.prod-eu, DATABASE_URL)"` |
| Environment variables (CI, containers) | nothing | `"${PROD_DATABASE_URL}"` |
| sec (encrypted local vault) | the one-line installer in the sec README | `"$file(sec get acme@prod-eu:DATABASE_URL --out ${out})"` |
| LastPass | `brew install lastpass-cli` · `lpass login you@example.com` | `"$(lpass show --password acme/prod-eu-db)"` |
| 1Password | `brew install 1password-cli` · `op signin` | `"$(op read op://acme/prod-eu-db/url)"` |
| Bitwarden | `brew install bitwarden-cli` · `bw login`, then export `BW_SESSION` | `"$(bw get password acme-prod-eu-db)"` |
| HashiCorp Vault | `brew install hashicorp/tap/vault` · `vault login` | `"$(vault kv get -field=url secret/acme/prod-eu/db)"` |
| AWS Secrets Manager | AWS CLI · `aws sso login` | `"$(aws secretsmanager get-secret-value --secret-id acme/prod-eu/db --query SecretString --output text)"` |
| Google Secret Manager | gcloud · `gcloud auth login` | `"$(gcloud secrets versions access latest --secret acme-prod-eu-db)"` |
| Azure Key Vault | Azure CLI · `az login` | `"$(az keyvault secret show --vault-name acme --name prod-eu-db --query value -o tsv)"` |
| Doppler | `brew install dopplerhq/cli/doppler` · `doppler login` | `"$(doppler secrets get DATABASE_URL --plain --config prd)"` |

Any command that prints the value works (`$(…)`); use `$file(… ${out})` for tools that write to a file. If a CLI needs a login, tmvp tells you the exact command to run (`login_required`) instead of asking for the secret. More: [Secrets](/secrets/how-it-works/) · [Secret managers](/secrets/managers/).

```sh
tmvp --skill .agents/skills/acme-tmvp validate
tmvp --skill .agents/skills/acme-tmvp check
tmvp --skill .agents/skills/acme-tmvp skill sync   # SKILL.md your agent reads
```

## 3. Ask the agent a real question

```text
why are orders failing in prod-eu but not prod-us?
```

Using the generated skill, the agent runs reads like these — batched across envs, read-only, with only the differences coming back:

```sh
tmvp --skill .agents/skills/acme-tmvp prod-eu,prod-us db query \
  "select status, count(*) from orders where created_at > now() - interval '1 hour' group by status" --diff
tmvp --skill .agents/skills/acme-tmvp prod-eu box docker logs acme-web --since 15m
```

If it finds a fix, it proposes it; a change runs only when you ask, one env at a time:

```sh
tmvp write prod-eu db "update orders set status='retry' where id=42" \
  --reason "orders failing in prod-eu" --force --preview     # rolled back: rows affected only
```

Everything it did is in `tmvp logs --since 1d`.
