# tmvp — Design

Status: draft · 2026-11-09 · Owner: Muthukumaran Navaneethakrishnan

## 1. What it is

> **tmvp = multiple products × multiple environments, usable by any agent through one skill.**
> A profile is a product. A product has many environments. Every environment has systems, CLIs
> and web UIs. Any agent (Claude, Codex, OpenCode, Copilot, wfnexus) reads the same definitions
> and runs the same verbs — read-only by default, batched across products and envs.

One CLI that reaches **any system you operate** — ssh hosts, SQL DBs, Mongo, k8s, logs, OTel traces,
NATS, Kafka, AWS, GCP — through **profiles**. A profile describes one product: its environments,
how to connect to each system (config), what each system *means* (references), and reusable actions
(assets). An agent skill teaches any agent (Claude, Codex, a wfnexus step) to use it.

It is a **separate application**: a CLI plus an agent skill, own repo, own binary, used directly
by humans and **any agent** (Claude Code, Codex, OpenCode, Copilot, a wfnexus step).
Profiles are **importable from git**: a team keeps its estate definitions (config + references +
assets + skill.md) in a git repo, `tmvp import`s them, and every agent on every machine gets the
same knowledge. Credentials never travel in git — only `{{NAME}}` / `${NAME}` placeholders.

**The point: diagnose extremely fast — done by the agent.** When something breaks, one agent turn
should go from "something is wrong in reqsume prod" to "here is what is broken and where" in
seconds — because every system, credential, meaning and web page is already defined.

### Trust model (owner decision 2026-11-09)
tmvp **trusts the agent**. Whoever holds access holds responsibility. Safety comes from three
things only: read is the default, every mutation is an explicit `tmvp write` with a `--reason`,
and **everything is recorded** (who, what, why, outcome) so any change can be traced back and
undone. No approval gates, no trust prompts, no sandboxing of agents. Diagnosis is out of scope —
it happens in the agent, outside tmvp.

### Goals
- Every verb is **batch by default**: one call runs across all envs in parallel, env-keyed results.
- One command shape for every system: `tmvp <profile>[,profile] [envs|all] <system> <verb> [args]`.
- Credentials never stored as literals and never printed: `{{NAME}}` via `sec`, `${NAME}` via env.
- Agents understand the estate from markdown, not from the user re-explaining it.
- Safe by default: prod read-only unless explicitly confirmed; every command audited.
- Reuse existing tools/libraries (pgx, client-go, ssh, Loki/Tempo HTTP APIs); do not rebuild drivers.

### Non-goals (first release)
- No server, UI, or daemon (MCP serve comes after MVP).
- No credential storage of its own — `sec` and env are the only sources.
- No write workflows beyond a gated `exec`; no migrations, no deploys.

## 2. First-release scope

| Area | In first release | Later |
|---|---|---|
| Core | profiles, multi-env config, `sec`/env resolution, policy, audit log, JSON/table output | `extends`, profile import/export |
| Connectors | **postgres, mongo, nats, kafka, k8s (full), ssh, loki, http** | mysql, redis, tempo/jaeger, prometheus, native aws/gcp/azure APIs |
| Any-CLI profiles | **`tmvp use` / `tmvp run -- <cli>`** for aws, gcloud, az, gh, kubectl, helm, terraform, docker, **playwright**, any tool | per-tool shell prompt integration |
| Web UIs | **`webui/<app>/context.md` + `pages/*.md`** knowledge, driven via the Playwright CLI tool | page-def drift check |
| Extensibility | **custom verbs + scripts + exec plugins from the profile git repo** | Go plugin SDK |
| Knowledge | `references/`, `assets/queries`, `skill.md`, `tmvp profile new` scaffold | `tmvp learn` (auto-draft references from live systems) |
| Agent | global skill + `--json` everywhere | `tmvp serve --mcp`, `tmvp trace`, `tmvp health` |

Success check for first release: with a deliberately broken pod + failing web page on prod only,
an agent using just the skill pinpoints both (via batched, `--diff`ed calls) in one or two turns.
And from a fresh shell an agent, given only the skill, can answer
"how many signups failed in reqsume prod in the last hour and what do the api logs say" using
`tmvp` alone — no credentials in its context, no write possible except via `tmvp write … --reason`.

## 3. On-disk layout

### 3.1 Repo (code)
```
tmvp/
├── cmd/tmvp/main.go
├── internal/
│   ├── cli/          # cobra commands, arg parsing, output rendering
│   ├── profile/      # load, validate, resolve env, list
│   ├── secret/       # {{SEC}} via sec, ${ENV} via os env; redaction
│   ├── policy/       # read vs write, classification, guards
│   ├── audit/        # append-only JSONL audit log
│   └── output/       # table / json / raw renderers
├── connectors/
│   ├── connector.go  # interface + registry
│   ├── postgres/
│   ├── ssh/
│   ├── k8s/
│   ├── loki/
│   └── http/
├── skills/tmvp/SKILL.md   # the global agent skill (installed to ~/.claude/skills)
├── docs/DESIGN.md
├── Taskfile.yml
└── .goreleaser.yaml
```

### 3.2 Runtime data (no code here)
```
~/data/tmvp/              # override with $TMVP_HOME
├── sources.yaml                      # imported git sources (url, ref, path, last sync sha)
├── sources/<source-name>/            # git clones (read-only working copies)
├── profiles/                         # local profiles + symlinks into sources/
│   └── {profile}/                    # one product, e.g. reqsume
│       ├── skill.md                  # entry point for agents
│       ├── config.yaml               # all environments + systems
│       ├── webui/                    # one folder per web app (section 10.1)
│       │   └── {app}/
│       │       ├── context.md        # base urls per env, login flow, which creds, roles, quirks
│       │       ├── pages/*.md        # one per page: url, purpose, elements, actions, checks
│       │       └── specs/*.spec.ts   # optional Playwright tests generated/kept from pages
│       ├── references/               # one .md per system name
│       │   ├── db.md
│       │   ├── k8s.md
│       │   └── logs.md
│       └── assets/
│           ├── queries/*.sql         # saved queries, runnable by name
│           ├── scripts/*.sh          # diagnostics run over ssh/exec
│           └── templates/
├── spool/*.jsonl                    # buffered events awaiting commit to sinks
└── events/                          # local Iceberg/Parquet warehouse (file sink)
```

## 4. Profile config (`config.yaml`)

```yaml
version: 1
name: reqsume
description: Resume SaaS — Go API, Postgres, k8s on Hetzner
default_env: dev

shared:                       # merged into every env's systems (env wins)
  box:
    type: ssh
    user: deploy
    key: "{{REQSUME_SSH_KEY}}"

environments:
  dev:
    systems:
      db:
        type: postgres
        dsn: "{{REQSUME_DEV_DB_URL}}"
      k8s:
        type: k8s
        kubeconfig: ${KUBECONFIG}
        context: reqsume-dev
        namespace: reqsume
      logs:
        type: loki
        url: ${LOKI_DEV_URL}
        labels: { app: reqsume }

  prod:
    tools:                    # any-CLI profile switch (section 5.3)
      aws:   { env: { AWS_PROFILE: reqsume-prod, AWS_REGION: eu-central-1 } }
      gh:    { env: { GH_TOKEN: "{{REQSUME_GH_TOKEN}}", GH_REPO: deemwar/reqsume } }
      gcloud: { env: { CLOUDSDK_ACTIVE_CONFIG_NAME: reqsume-prod } }
      kubectl: { env: { KUBECONFIG: "${HOME}/.kube/reqsume-prod" } }
      playwright:
        env: { BASE_URL: https://reqsume.com, ADMIN_USER: "{{REQSUME_ADMIN_USER}}", ADMIN_PW: "{{REQSUME_ADMIN_PW}}" }
        files: { .auth/admin.json: "{{REQSUME_ADMIN_SESSION}}" }   # storage_state, temp 0600
    webui:                    # which web apps exist in this env (section 10.1)
      admin:  { base_url: https://admin.reqsume.com }
      site:   { base_url: https://reqsume.com }
    systems:
      db:
        type: postgres
        dsn: "{{REQSUME_PROD_DB_URL}}"
        statement_timeout: 15s
        max_rows: 1000
      box:
        host: 10.8.0.5          # merged with shared.box
      k8s:
        type: k8s
        context: reqsume-prod
        namespace: reqsume
      logs:
        type: loki
        url: ${LOKI_PROD_URL}
        token: "{{LOKI_PROD_TOKEN}}"
      k8s-edge:               # several k8s clusters per env = several systems
        type: k8s
        kubeconfig: "{{EDGE_KUBECONFIG}}"   # kubeconfig content from sec, memory only
        context: edge-prod
      docs:
        type: mongo
        uri: "{{REQSUME_PROD_MONGO_URI}}"
        database: reqsume
      bus:
        type: nats
        url: ${NATS_URL}
        creds: "{{REQSUME_NATS_CREDS}}"
      events:
        type: kafka
        brokers: ["${KAFKA_BROKERS}"]
        sasl: { mechanism: SCRAM-SHA-512, user: reqsume, password: "{{REQSUME_KAFKA_PW}}" }
      ops:                    # custom system defined entirely in the profile repo
        type: exec
        plugin: assets/plugins/reqsume-ops   # executable implementing the plugin protocol
```

### Rules
- **Same system names across envs** so one `references/<system>.md` covers all envs.
- Value syntax: `{{NAME}}` → resolved by `sec`; `${NAME}` → process env; anything else is a literal.
- Fields marked `secret` in a connector's schema (`dsn`, `password`, `token`, `key`) **must** be
  `{{…}}` or `${…}`; a literal fails validation (`tmvp profile check`).
- Unknown `type` or unknown field → validation error, not a silent ignore.
- Everything is read-only; writes only via `tmvp write … --reason` (section 8).

## 5. Command surface

```
tmvp profile import <git-url> [--ref main] [--path profiles/] [--name team]  # git clone + link
tmvp sources                                   # list imported sources + sync state
tmvp profile sync [source]                     # git pull, re-validate, report changed profiles
tmvp source remove <name>
tmvp skill install [--agent claude|codex|opencode|copilot|all]       # install the skill for any agent

tmvp profiles                                  # list profiles + envs
tmvp profile new <name>                        # scaffold folder
tmvp profile check <name>                      # validate config, report missing secrets (names only)
tmvp profile show <name> [env]                 # resolved structure, secrets shown as {{NAME}}

tmvp <profile> [envs] systems                   # list systems in env
tmvp <profile> [envs] <system> describe         # what is it: schema/namespaces/labels
tmvp <profile> [envs] <system> health           # can we connect, latency
tmvp <profile> [envs] <system> query <q|@name>  # postgres SQL, loki LogQL, http GET
tmvp <profile> [envs] <system> tail  [filter]   # loki stream, k8s logs -f
tmvp <profile> [envs] <system> get   <kind> [name]   # k8s
tmvp <profile> [envs] <system> logs  <workload> [--since 1h]  # k8s
tmvp <profile> <env>  <system> run   <cmd|@script>   # ssh (gated: write verb)
tmvp <profile> <env>  <system> exec  <stmt>     # postgres write / k8s exec (gated)

Global flags: --json  --raw  --limit N  --since DUR  --diff  --preview  --reason "…" (required for write, optional for read)
```

- `envs` omitted → all envs for reads; mutating verbs need one explicit env. Ambiguity (env name equals a system name) is a config error.
- `@name` runs `assets/queries/name.sql` or `assets/scripts/name.sh`; `--param k=v` fills `:k`.
- Exit codes: 0 ok, 1 runtime error, 2 usage/config error, 3 blocked by policy.

### 5.1 Import from git

`tmvp profile import <ssh-or-https-url>` — uses the existing `git` (your ssh keys / gh auth) to
clone the repo locally into `~/data/tmvp/sources/` and link its profiles. That's all: no trust
step, no code runs on import. `tmvp profile sync` = `git pull`.


- A source repo holds `profiles/<name>/{config.yaml,skill.md,references/,assets/}` (or a single
  profile at repo root). Private repos use the user's existing git auth (ssh agent / gh) — tmvp
  stores no git credentials.
- On import/sync: `profile check` runs on every profile; **a literal secret in any config fails
  the import** (nothing gets linked). Missing `{{NAME}}`s are reported by name so the user can
  add them to `sec`.
- Imported profiles are **read-only** locally; edits go back through git (PR). A local profile
  with the same name shadows an imported one, with a warning.
- Namespacing: collisions across sources → `team/reqsume`; unique names stay bare.
- The source repo can also carry `skills/` overrides; `tmvp skill install` merges profile
  `skill.md` pointers into the installed skill index so any agent discovers them.

### 5.2 Any agent

The CLI is the contract; the skill is a thin adapter. `tmvp skill install --agent X` writes the
same SKILL.md to the agent's skill location (`~/.claude/skills/`, `~/.codex/`, `~/.agents/`, …)
plus a `tmvp agent-context` command that prints a compact index (profiles → envs → systems →
reference paths) for agents without a skill system — paste into any prompt or AGENTS.md.

### 5.3 Profile switch for any CLI

Every env can declare `tools:` — the env vars / flags / config files a third-party CLI needs.
tmvp never reimplements those CLIs; it **injects the right identity** and runs them.

```
tmvp use reqsume prod                 # print `export ...` lines → eval "$(tmvp use reqsume prod)"
tmvp shell reqsume prod               # subshell with all tools switched; prompt shows [reqsume:prod]
tmvp run reqsume prod -- aws s3 ls    # one-off, scoped to this command only
tmvp run reqsume prod -- gh pr list
tmvp run reqsume prod -- terraform plan
tmvp tools reqsume prod               # which tools are configured, which secrets are missing (names)
```

- Tool entry fields: `env:` (vars), `args:` (prepended flags, e.g. `--context`), `files:` (temp
  0600 files materialised from `{{…}}`, e.g. a kubeconfig or gcloud SA JSON; deleted on exit).
- tmvp fetches each secret from its provider and injects it into the child process it runs.
  Holding the value is unavoidable; **tmvp's job is never to log or print it** (redaction on all
  output, logs and audit). `tmvp use` (eval in your shell) exports only non-secret vars; for
  secret-bearing tools use `tmvp shell` / `tmvp run`.
- Modes (section 8) apply: tools declare `read:`/`write:` identities (e.g. `AWS_PROFILE` pointing
  at a read-only role, `kubectl` blocked verbs list). tmvp can't police arbitrary CLIs fully — the
  guard is the read-only *identity*, not argument parsing.
- Child output passes through the redactor like every other command; audited as verb `run`.

### 5.4 Extensibility via the profile repo

Three levels, all shipped in the same git repo as the profile:

1. **Custom verbs** (zero code) — in `config.yaml`, bind a name to a saved query/script/CLI:
   ```yaml
   systems:
     db:
       type: postgres
       verbs:
         failed-signups: { query: "@failed_signups", params: [since] }
     k8s:
       type: k8s
       verbs:
         restart-api: { cli: "kubectl rollout restart deploy/api", mutating: true }
   ```
   → `tmvp reqsume prod db failed-signups --since 1h`
2. **Scripts** — `assets/scripts/*.sh|*.py` run locally with the env's tools switched and system
   connection info exported (`TMVP_SYS_DB_DSN` etc. as lazy secret handles), or remotely via ssh.
   Front-matter declares `mutating`, `params`, `description` so the skill lists them.
3. **Exec plugins** — `type: exec` system pointing at an executable in the repo (any language).
   Protocol: `plugin describe` → JSON `{verbs:[{name,mutating,args}]}`; `plugin do <verb>` with
   resolved config on stdin (JSON) → JSONL rows/lines on stdout. Same policy, audit, redaction.


### 5.5 Batch across environments (default for every verb)

Every verb takes an **env set**, not one env, and runs it in parallel:
```
tmvp reqsume db query "select count(*) from signups where status='failed'"   # env omitted → ALL envs
tmvp reqsume dev,prod k8s get pods -l app=api
tmvp reqsume all logs query '{app="reqsume"} |= "error"' --since 30m
tmvp reqsume prod db query @failed_signups                                    # single env = batch of one
tmvp reqsume all web admin check --all
tmvp run reqsume dev,prod -- aws s3 ls                                        # any CLI too
```
- **Reads default to all envs** of the profile (`default_env` only applies to mutating verbs).
- **Mutating verbs require exactly one explicit env** — no batch writes (exit 3 otherwise).
- Envs where the system/page doesn't exist are reported `skipped`, not failed.
- Each env runs in parallel with its own creds/policy; per-env timeout; one slow env never blocks
  the others' results.
- Output is always **env-keyed**:
  - table: an `env` column first (rows) or one block per env (streams/text);
  - `--json`: `{"results":[{"env":"dev","status":"ok","ms":41,"columns":[...],"rows":[...]},
    {"env":"prod","status":"error","error":"..."}]}`;
  - `--diff`: show only where envs disagree (row counts, values, pass/fail) — the fast way to
    see "works on dev, broken on prod".
- Batching also works across profiles: `tmvp reqsume,wfnexus prod k8s get pods`.

### 5.6 Diagnosis is the agent's job, not a command

tmvp has **no `diagnose` command**. It gives the agent fast, safe, batched verbs plus the
knowledge (skill.md, references, webui pages); the **agent** does the reasoning. Diagnosis is
fast because:
1. no setup — every system, credential and page is already defined in the profile;
2. one call fans out across all envs/systems in parallel (5.5), so the agent compares
   dev vs prod in a single turn;
3. output is bounded and env-keyed, so the agent spends tokens on the problem, not noise.

The skill carries a **diagnosis playbook** (not code): read `skill.md` → batch health/errors
across all envs → `--diff` to spot the env that differs → drill into that env with references →
web `check` on affected pages → report cause + evidence + next step. Profiles can add their own
playbook steps in `skill.md` ("for signup failures, check X then Y").

## 6. Connector interface

```go
type Verb string // describe, health, query, tail, get, logs, run, exec

type Connector interface {
    Type() string
    Schema() FieldSchema                 // fields, which are secret, defaults
    Verbs() map[Verb]VerbSpec            // VerbSpec{Mutating bool, Args ...}
    Open(ctx context.Context, cfg ResolvedConfig) (Session, error)
}

type Session interface {
    Do(ctx context.Context, v Verb, args Args, out Sink) error
    Close() error
}

// Sink receives rows/lines/events; renderers turn them into table/json/raw.
type Sink interface {
    Columns([]string)
    Row([]any)
    Line(ts time.Time, labels map[string]string, msg string)
}
```

### 6.1 k8s verbs (kubectl parity for the daily set)

`ctx` · `ns` · `get <kind> [name] [-l sel] [-A]` · `describe <kind> <name>` · `logs <pod|deploy> [-c] [-f] [--since] [--previous]` ·
`events [--for obj]` · `top pods|nodes` · `exec <pod> -- cmd` · `port-forward <svc|pod> <port>` ·
`apply -f <file|@manifest>` · `delete <kind> <name>` · `scale <deploy> <n>` · `restart <deploy>` ·
`rollout status|history|undo <deploy>` · `cordon|uncordon|drain <node>` · `yaml <kind> <name>` ·
`health` (nodes ready, crashlooping pods, pending pods). Anything else: `tmvp run <p> <env> -- kubectl …`
with the cluster's kubeconfig/context injected. Each k8s system = one cluster/context; envs may hold many.

Connectors register in `init()`; the registry is compile-time (no dynamic plugins in first release).

### First-release connectors

| Type | Library | Verbs | Mutating | Notes |
|---|---|---|---|---|
| postgres | pgx v5 | describe, health, query, exec | exec | `query` runs in `BEGIN READ ONLY` + `statement_timeout`; non-SELECT in `query` is rejected by the DB itself |
| ssh | x/crypto/ssh | health, run | run | known_hosts verified; key from `{{…}}` written to memory only |
| loki | HTTP API | describe (labels), health, query, tail | — | LogQL; `--since` maps to start |
| http | net/http | health, query (GET) | — | base_url + headers; for internal admin/status endpoints |
| mongo | mongo-go-driver v2 | describe (dbs/collections/indexes), health, find, aggregate, count, exec (insert/update/delete) | exec | `find`/`aggregate` take JSON/EJSON; `$out`/`$merge` treated as mutating |
| nats | nats.go | describe (streams/consumers/KV buckets), health, sub, pub, req, stream info, kv get/put | pub, req, kv put | `sub` bounded by `--limit/--since`; JetStream replay by seq/time |
| kafka | franz-go | describe (topics/partitions/groups/lag), health, consume, produce, offsets | produce, offsets reset | `consume` never commits offsets (uses an ephemeral group) |
| k8s | client-go | see 6.1 | apply, delete, scale, restart, rollout undo, exec, cordon/drain, port-forward(readonly-safe) | multiple clusters = multiple systems; namespace override `-n` |
| exec | external plugin binary | whatever the plugin declares | as declared | see 5.4 |

## 7. Secrets

### 7.0 Secret sources — sec by default, any CLI by config
Three sources for now, all through **their own CLIs** (no SDKs, no provider framework):
**sec** (default), **1Password** (`op`), **LastPass** (`lpass`). A source is just a command
template in config that prints the value to stdout; tmvp runs it, captures the value in memory,
uses it, and never logs it.
```yaml
# profiles/<project>/config.yaml (global ~/data/tmvp/config.yaml is the fallback)
secrets:
  default: sec
  sources:
    sec:  { cmd: "sec get {name}" }                       # exact sec subcommand TBD (open question)
    op:   { cmd: "op read op://{name}" }                  # {{op:Reqsume/DB/password}}
    lpass: { cmd: "lpass show --password {name}" }        # {{lpass:reqsume-prod-db}}
    # any other manager = add a line: vault, aws, doppler … all just CLIs
    # vault: { cmd: "vault kv get -field={field} {path}" }
```
| Placeholder | Resolves |
|---|---|
| `{{NAME}}` | default source (sec) |
| `{{op:Vault/Item/field}}` · `{{lpass:item}}` · `{{<source>:ref}}` | that source's `cmd` |
| `${NAME}` | process environment |

**Files from the vault too.** Kubeconfigs, ssh keys, service-account JSON, `.pem`s are stored in
the secret manager and declared per env as files; tmvp writes them to a private temp file (0600)
for the command and removes it after:
```yaml
environments:
  prod:
    files:
      kubeconfig: "{{op:Reqsume/k8s-prod/kubeconfig}}"
      ssh_key:    "{{sec:REQSUME_PROD_SSH_KEY}}"
    systems:
      k8s: { type: k8s, kubeconfig: "@files.kubeconfig", context: reqsume-prod }
      box: { type: ssh, host: 10.8.0.5, key: "@files.ssh_key" }
```
- Values live in memory for one command; each distinct ref resolved once per batch.
- Errors name the ref and source, never the value (`secret_missing`, `secret_source_failed`).
- `tmvp secrets check <profile> [envs]` → which refs resolve (names only).

### 7.1 Never log a secret (tmvp's responsibility)
tmvp necessarily holds secret values to run commands. Every resolved value is registered with a
**streaming redactor** (handles values split across output chunks; plain, base64, url-encoded,
hex forms) that sits in front of stdout, stderr, the event log, the write audit and the UI.
Values are never written to disk except the 0600 temp files above, removed after the command.

## 8. Modes, guards & error handling (built first)

### 8.1 Read by default; writes are one explicit command with a reason
Everything is **read** unless the command is the **write** command. Nothing to enable in config,
no grants, no approval step — just say it is a write and why (`--reason`):
```
tmvp write reqsume prod db "update signups set status='retry' where id=4812" \
  --reason "retry stuck signup #4812 (RS-311)"
tmvp write reqsume prod k8s delete pod api-7f9c --reason "pod wedged, force reschedule"
tmvp write reqsume prod db @restore_users --reason "restore users from 2026-11-08 backup"
```
- Read verbs (`query`, `get`, `logs`, …) are **always read**: they run on read credentials and in
  read-only sessions; a mutating statement through a read verb fails (`policy_blocked`, hint: use
  `tmvp write`).
- `--reason` is **mandatory on every mutation** (`tmvp write`; missing → `policy_blocked`) and
  **optional on reads** (`tmvp reqsume all db query … --reason "checking RS-311"`) — when given it
  is logged with the read too, so the UI shows *why* someone/some agent looked.
- `tmvp write` covers inserts, updates, deletes, drops, restores, apply/scale/delete in k8s, produce/pub, mutating web actions, and any
  `tmvp run -- <cli>` that changes things.
- **No tmvp batching for writes**: one product, one env, one system per `write` — each its own
  audit record. Envs/products lists are rejected for `write`.
- **Native bulk ops are fine** inside that one target — `deleteMany`/`updateMany`, multi-row SQL,
  `kubectl delete -l`, bulk produce, `pg_restore`/`mongorestore`/snapshot restore. tmvp doesn't
  split or throttle them; it records the blast radius (rows/docs/resources affected).
- **Destructive is a config switch.** Statements tmvp classifies as destructive (DROP/TRUNCATE/
  DELETE without WHERE, `kubectl delete ns`, topic/stream delete, purge, offset reset, restore over
  existing data) run only where config says they are available:
  ```yaml
  environments:
    dev:  { destructive: true }
    prod: { destructive: false }          # default false; per system: systems.db.destructive: true
  ```
  Not available → `policy_blocked` with hint naming the config key. Available → same as any write
  (`--reason`, audited).
- `--preview` shows what it would touch (e.g. rows affected inside a rolled-back tx) without doing it.

### 8.2 Least-privilege credentials per mode (the real guarantee)
```yaml
db:
  type: postgres
  read:  { dsn: "{{RS_PROD_DB_RO}}", prefer: replica }
  write: { dsn: "{{RS_PROD_DB_RW}}" }     # resolved from sec ONLY for `tmvp write`
```
Same for k8s (`view` vs `edit` kubeconfig), mongo/kafka/nats users, and `tools:` (aws/gh/gcloud
read role vs write role). **Protected envs require a `read:` credential**; `tmvp profile check`
fails otherwise. Classification (below) is the second layer, never the only one.

### 8.3 Classification
- postgres: SQL parsed (pg_query) → read/write; reads also run in `BEGIN READ ONLY`.
- mongo: verb-based; aggregate with `$out`/`$merge` = write.
- k8s: fixed verb table (get/describe/logs/events/top = read; apply/scale/restart/exec = write;/delete/drain = write).
- kafka/nats: consume/sub/info = read; produce/pub/kv put/purge/offset reset = write.
- web: element/action `mutating:` flag; unknown elements on protected envs = write.
- **Unclassifiable** (ad-hoc ssh `run`, `tmvp run -- <any cli>`) = **write**, unless it is a
  declared script with `mode: read` front-matter, or the tool runs under its read identity.

### 8.4 Guards — no query or process may take a system down
Defaults per env, stricter on protected envs, overridable in `limits:`.

| System | Guards |
|---|---|
| postgres | `statement_timeout` 10s (prod 5s), `lock_timeout` 1s, idle-in-tx timeout; **EXPLAIN pre-check** — reject if cost > `max_cost` or seq scan on table > `max_scan_rows`; auto-LIMIT; reject `FOR UPDATE`/advisory locks in read; replica preferred |
| mongo | `maxTimeMS`; explain → reject COLLSCAN over `max_scan_docs`; capped limit; `secondaryPreferred` |
| loki | max range (24h / prod 6h), label selector required, line cap, query timeout |
| k8s | paginated lists, log tail cap, no unbounded watch/follow (time cap), exec time cap |
| kafka / nats | bounded by count + time; ephemeral consumers; never commit/alter real groups |
| ssh | timeout, `nice -n 19 ionice -c3`, output cap |
| web | one browser per app/env, nav timeouts, no parallel sessions on prod |

Global: per-system concurrency cap + rate limit (token bucket); **circuit breaker** (system slow or
erroring → stop sending for a cool-off); batch concurrency cap; `tmvp stop` kills all in-flight
commands; `TMVP_DISABLED=1` refuses everything.
```yaml
limits:                              # env- or system-level
  timeout: 10s
  max_rows: 1000
  max_cost: 1e6
  max_scan_rows: 5e6
  concurrency: 2
  rate: 5/s
```

### 8.5 Errors
Every error: `code`, `message`, `hint` (next action), plus env/system. JSON on `--json`.
| code | exit | example |
|---|---|---|
| `ok` | 0 | |
| `runtime` / `timeout` / `unreachable` / `auth` | 1 | |
| `config_invalid` / `secret_missing` (name only) | 2 | |
| `policy_blocked` | 3 | mutation via a read verb, or `tmvp write` without `--reason` |
| `partial` | 4 | batch: some envs ok, some failed — results still returned per env |
| `guard_rejected` | 5 | "EXPLAIN cost 4e8 > 1e6 (seq scan on events)" hint: filter on created_at |
| `circuit_open` | 6 | system cooling off after repeated failures |

### 8.6 Write audit
Every `tmvp write` attempt — allowed, blocked, previewed or failed — writes one record to
the `tmvp.writes` table in the logging sink (section 9; never expired by `keep`):
```json
{"ts":"2026-11-09T10:20:11Z","trace_id":"…","profile":"reqsume","env":"prod","system":"db",
 "mode":"write","verb":"write","statement":"update signups set status='retry' where id=4812",
 "target":"signups#4812","reason":"retry stuck signup #4812 (RS-311)",
 "caller":"claude-code","session":"…","user":"muthu",
 "preview":{"rows_affected":1},"outcome":"ok","rows_affected":1,"ms":18}
```
`tmvp audit [--profile p] [--env e] [--since 7d]` lists them, and
`tmvp_writes_total{profile,env,system,mode,outcome}` goes into `tmvp.prom`.

Retries: **reads only**, transient errors only (connect/reset), once, jittered. Writes never retried.

## 9. Telemetry, logging sinks & the activity UI

Every command emits OTel-shaped events (one per command and per env in a batch, shared TraceId):
command, profile, env, system, verb, mode, outcome, guard decision, duration, rows, caller agent,
session, user — and for writes the `reason`, statement (redacted) and blast radius.

### 9.1 Where events go — `logging:` in config
**Per project.** Each profile's `config.yaml` has its own `logging:` — different products can
belong to different teams/clients and their activity must land in *their* bucket. A global
`~/data/tmvp/config.yaml` `logging:` is only the fallback for profiles that don't set one
(default: `file` under `~/data/tmvp/events/<project>/`). `tmvp ui` reads every project's sink and
shows them together (or `--profile` to filter).

One sink per project: **file** (default — everything stays on this machine) **or s3** (shared across
machines/agents/teams). Same table format either way, so the UI works identically on both.
```yaml
# profiles/<project>/config.yaml — logging belongs to the project
logging:
  sink: file                          # file | s3
  path: ~/data/tmvp/events            # file sink: local warehouse right here
  # sink: s3
  # bucket: deemwar-tmvp-events
  # endpoint: ${S3_ENDPOINT}          # AWS S3, Cloudflare R2, MinIO, GCS (S3 API)
  # access_key: "{{TMVP_S3_KEY}}"
  # secret_key: "{{TMVP_S3_SECRET}}"
  format: parquet                     # parquet (first cut) | iceberg
  flush: { every: 30s, max_events: 500 }
  keep: 90d
```
**Start on files, move to S3 any time — nothing lost.** Day 1 is `sink: file`. The file layout
is exactly the S3 layout (same partitions, same Parquet/Iceberg files), so moving is a copy:
```
tmvp logs reqsume bucket create --bucket deemwar-tmvp-events --endpoint $S3_ENDPOINT   # creates bucket (+ lifecycle for keep:)
tmvp logs reqsume import --to s3 [--bucket …]      # uploads all local events + writes audit, verifies counts/checksums
tmvp logs reqsume sink s3 --bucket …       # switches this project's config to s3 (local spool still buffers)
tmvp logs reqsume import --from file:/other/machine/events   # merge another machine's local logs in
```
- `import` is idempotent (event ids dedupe), resumable, and keeps the local copy until
  `--prune-local` is passed.
- After the switch, the UI shows old (imported) and new events together.

- **Storage format: Apache Iceberg table** (`tmvp.events`, `tmvp.writes`) of Parquet files,
  partitioned by `day(ts)` and `profile`. Schema evolution, time-travel ("what did the audit
  look like yesterday"), and any engine reads it: DuckDB, Spark, Trino, Athena, ClickHouse.
- Catalog: Iceberg **SQL catalog** (SQLite locally, Postgres when shared) or a **REST catalog**
  (e.g. R2 Data Catalog / Polaris / Glue). Go: `apache/iceberg-go`.
- CLI runs are short and many agents write at once, so events go to a **local buffer**
  (`~/data/tmvp/spool/*.jsonl`) and are committed in batches (optimistic Iceberg commit with
  retry). A crash loses nothing — the spool is replayed on the next run. `tmvp logs flush` forces it;
  `tmvp logs compact` merges small files.
- **Write audit goes to the sink synchronously** (spool fsync before the write executes), so a
  write without an audit record cannot happen.
- Fallback / first cut: `format: parquet` = Hive-partitioned Parquet (`dt=…/profile=…/`) with the
  same schema; upgrading to Iceberg later is a metadata-only migration.

### 9.2 Prometheus
`~/data/tmvp/metrics/tmvp.prom` (Prometheus text format) rewritten after each command —
`tmvp_commands_total`, `tmvp_command_duration_seconds`, `tmvp_guard_rejections_total`,
`tmvp_writes_total`, `tmvp_rows_returned_total`, `tmvp_system_up`. node_exporter textfile
collector reads it as-is. The same counters are derivable from the Iceberg table for the UI.

### 9.3 Activity UI — `tmvp ui`
Local web UI (single binary, embedded), reads the configured sinks through embedded **DuckDB**
(iceberg + httpfs extensions) — so it shows activity **across all machines, agents and teams**
writing to the same bucket.
- **Live feed**: every command across products × envs, filter by product/env/system/agent/user/outcome.
- **Writes**: every write with its description, statement, blast radius, who/which agent, outcome.
- **Matrix**: products × envs grid — activity, errors, guard rejections, last seen.
- **Agents**: which agents/sessions ran what, error rates, top queries.
- **Guards & errors**: what got rejected and why (cost, timeout, circuit open).
- **Trace view**: one batched command → its per-env spans.
- Read-only UI; `tmvp ui --addr 127.0.0.1:7788`. Can be deployed as a small service pointed at the
  bucket for a team view (same binary: `tmvp ui --serve`).

## 10. Knowledge layer

### `profiles/{profile}/skill.md` (entry point)
```markdown
# reqsume
Resume SaaS. Go API + Postgres + k8s. Envs: dev (write allowed), prod (read only).

## Systems
- db — Postgres, users/resumes/signups. See references/db.md
- k8s — api, worker, web deployments. See references/k8s.md
- logs — Loki, label app=reqsume. See references/logs.md
- box — ssh to app host. See references/box.md

## Common tasks
- Failed signups last hour → `tmvp reqsume prod db query @failed_signups --param since=1h`
- API errors → `tmvp reqsume prod logs query '{app="reqsume",svc="api"} |= "error"' --since 1h`

## Rules
- Never write to prod. Ask the owner.
```

### `references/<system>.md`
Free-form, but the scaffold suggests: **What it is · What lives here (tables/namespaces/labels) ·
Useful queries · Gotchas · Env differences · Safe / unsafe actions.**

### 10.1 Web UIs — `webui/<app>/`

Playwright is **not** a connector; it is a CLI in `tools:` exactly like `aws`. `tmvp run reqsume prod -- npx playwright test …`
runs it with the env's base url, credentials and storage_state injected. What makes it useful
to an agent is the knowledge folder:

**`webui/<app>/context.md`** — everything needed to *infer* how to drive the app:
```markdown
# admin (reqsume)
Base URL: dev https://admin.dev.reqsume.com · prod https://admin.reqsume.com
## Login
- Google SSO for humans; for automation use email+password form at /login
- Credentials: env ADMIN_USER / ADMIN_PW (sec: REQSUME_ADMIN_USER / REQSUME_ADMIN_PW)
- After login a session cookie `rs_sess`; reuse storage_state .auth/admin.json (valid ~7 days)
- 2FA: none on admin role "ops"; prod requires VPN (WireGuard 10.8.0.x)
## Roles
- ops: read-only dashboards · admin: user management (never in prod automation)
## Conventions
- Elements carry data-testid; prefer getByTestId > role > text
- Toasts confirm saves; tables paginate at 50
## Don'ts
- Never click "Delete user" / "Refund" in prod
```

**`webui/<app>/pages/<page>.md`** — one per page:
```markdown
# Users list
URL: /users?q={query}
Purpose: find a user, see plan + signup status
Requires: login (ops)
## Elements
- search: getByTestId('users-search')
- rows: getByTestId('user-row') — columns email, plan, status, created
- open user: click row → /users/{id}  (see user-detail.md)
## Actions
- search by email: fill search, press Enter, wait rows
## Checks
- page loaded: heading "Users" visible
- empty state: text "No users found"
## Mutating
- none on this page
```

**Assumption:** the Playwright CLI (`playwright-cli`, persistent-session browser CLI for agents)
is installed like `aws` — tmvp never installs or bundles it. tmvp adds the **verbs**, resolves
names from `context.md` / `pages/*.md`, injects env (base url, creds, session) and calls it.

#### `context.md` front-matter — the executable part
Prose stays for humans/agents; the YAML front-matter is what `login` runs:
```yaml
---
app: admin
base_url: { dev: https://admin.dev.reqsume.com, prod: https://admin.reqsume.com }
login:
  page: login                       # pages/login.md
  steps:
    - goto: /login
    - fill: { el: email, value: "${ADMIN_USER}" }
    - fill: { el: password, value: "${ADMIN_PW}" }
    - click: submit
    - wait: { url: /dashboard }
  success: { el: dashboard.heading }   # login verified when visible
  session: { reuse: true, ttl: 7d }    # storage_state kept per profile/env/app (0600, outside git)
roles: { ops: [ADMIN_USER, ADMIN_PW] }
never: [users.delete, billing.refund]  # element refs blocked on protected envs, always
---
```

#### `pages/<page>.md` front-matter — named elements + actions
```yaml
---
page: users
path: /users
elements:
  search:  { testid: users-search }
  rows:    { testid: user-row }
  delete:  { role: button, name: "Delete user", mutating: true }
actions:
  find-user: [ { fill: { el: search, value: "{email}" } }, { press: Enter }, { wait: { el: rows } } ]
checks:
  - visible: { role: heading, name: Users }
envs: [dev, staging, prod]
---
```
Locators: `testid` → `role`+`name` → `text` → `css` (last resort). Pages can reference other
pages' elements as `page.element` (e.g. `dashboard.heading`).

#### Web verbs
```
tmvp web reqsume prod admin login [--role ops]          # run context.md login, cache session
tmvp web reqsume prod admin goto users                   # by page name (or a raw /path)
tmvp web reqsume prod admin click users.delete           # named element; raw locator also ok
tmvp web reqsume prod admin fill users.search "a@b.com"
tmvp web reqsume prod admin press Enter
tmvp web reqsume prod admin select <el> <value>
tmvp web reqsume prod admin hover|check|uncheck|upload <el> [file]
tmvp web reqsume prod admin wait <el|--url /x|--text "...">
tmvp web reqsume prod admin text <el>                    # read text / table rows (--json)
tmvp web reqsume prod admin snapshot                     # accessibility snapshot for agents
tmvp web reqsume prod admin screenshot [<el>] [--full]
tmvp web reqsume prod admin do users.find-user --email a@b.com   # run a page action
tmvp web reqsume prod admin check [users|--all]          # run page checks
tmvp web reqsume prod admin pages | elements users       # list what is defined
tmvp web reqsume prod admin new-page <name>              # scaffold; agent fills by exploring
tmvp web reqsume prod admin close
```
- One **persistent browser session** per profile/env/app, so verbs chain like a human session
  (login once, then goto/click/fill…). Session state lives under `~/data/tmvp/.sessions/` (0600).
- Not logged in / session expired → verbs auto-run `login` first.
- Policy: elements/actions marked `mutating` follow modes (section 8): only run via `tmvp write … web <action> --reason`;
  `never:` refs are blocked on protected envs always.
- Every verb is audited; typed values from `${…}`/`{{…}}` are redacted in output and audit.
- Anything the verbs don't cover: `tmvp run reqsume prod -- playwright-cli …` with env injected.

**Multi-environment testing is built in.** Pages and specs never hold a URL host or a credential —
only paths, elements and checks. The env supplies base url, creds and session. So the same
definitions run against any env:
```
tmvp web reqsume dev  admin check --all          # every page's Checks on dev
tmvp web reqsume prod admin check users          # same page def, prod
tmvp web reqsume dev,staging,prod admin check --all --matrix   # side-by-side pass/fail per env
tmvp web reqsume prod admin test                 # run specs/*.spec.ts with prod injected
```
`--matrix` reports which pages pass on dev but fail on prod (drift / bad deploy). Per-env
differences (a page only on dev, a different login on prod) are declared in `context.md` /
the page's `Envs:` line, and skipped elsewhere instead of failing. Prod runs only non-mutating
checks; mutating checks need write mode (section 8).

Pages flagged `Mutating` follow modes (section 8). The skill tells
agents: read `context.md` first, then only the pages the task touches; when they explore a new
page, write its `pages/*.md` and propose it back to the profile repo.

### `assets/queries/*.sql`
```sql
-- name: failed_signups
-- description: Signups that errored in the window
-- params: since (interval)
select id, email_domain, error, created_at from signups
where status = 'failed' and created_at > now() - :since::interval
order by created_at desc;
```

## 11. Global agent skill (`skills/tmvp/SKILL.md`)

Teaches the agent to:
1. `tmvp profiles` → pick the profile matching the product in question.
2. Read `profiles/<p>/skill.md`; read only the `references/*.md` the task needs.
3. Prefer `@saved` queries/scripts from `assets/` over ad-hoc ones.
4. Always use `--json`; respect `--limit`; use `tmvp write` only when the user asked for the change, always with a truthful `--reason`; pass `--reason` on reads too (the task you are working on).
5. Never ask for, print, or reconstruct credentials — refer to `{{NAME}}` only.
6. When a useful ad-hoc query works, propose saving it to `assets/queries/` and a fact to the reference.

## 12. Output

- TTY → table; piped or `--json` → JSON (`{"columns":[...],"rows":[...]}` or JSONL for streams).
- `--raw` → connector-native text (e.g. log lines only).
- Errors to stderr as `{"error":"...","code":3}` when `--json`.

## 13. Build plan (first release)

0. **Import** — `tmvp profile import/sync` (git clone/pull + validate; lands with step 1).
(Batch-across-envs is part of the core in step 1; every connector gets it for free.)
1. **Core** — cobra CLI, profile loader + validation, env/shared merge, `profiles`/`profile new|check|show`.
2. **Read/write, guards, errors, telemetry + sink (file or s3; parquet → iceberg) (section 8–9)** — built before any connector.
2b. **Secrets + redaction** — `sec`/env resolution, redactor wired into all output and audit.
4. **postgres** connector (describe/health/query/exec, `@saved` queries).
5. **k8s** connector (get/logs/tail/exec).
6. **ssh** connector (run, `@scripts`).
7. **loki** + **http** connectors.
7a. **Any-CLI switch** — `tools:`, `tmvp use|shell|run|tools`.
7b. **mongo, nats, kafka** connectors; **k8s full verb set** (6.1).
7c. **Web UI** — `playwright` as a `tools:` entry (like aws) + `webui/` knowledge (10.1) + `tmvp web` verbs.
7d. **Extensibility** — custom verbs, scripts with front-matter, exec plugin protocol.
7e. **Activity UI** (`tmvp ui`, DuckDB over the sinks).
8. **Skill** + `skill install --agent` + `agent-context`; scaffold templates; dogfood a real
   profile (reqsume dev/prod) imported from a private git repo.
9. goreleaser + `brew` tap.

Each step lands with tests: unit tests for loader/policy/redactor; connector tests against
throwaway containers (postgres, k3s/kind optional, loki) behind a build tag.

## 14. After first release

`tmvp learn` (draft references from live schema/namespaces/labels) · redis, mysql,
tempo/jaeger, prometheus, native aws/gcp APIs · `extends` / `_base` profiles ·
`tmvp health <profile> <env>` (all systems) · `tmvp trace <id>` (logs → traces → db) ·
`tmvp serve --mcp` · wfnexus step type that calls tmvp.

## 15. Open questions

1. `sec` integration: direct value fetch vs re-exec under `sec exec` (§7).
2. Check `tmvp` isn't taken on brew/PATH before first release.
3. First real profile to dogfood: reqsume?
4. Profiles git repo: one shared `deemwar-estate` repo, or a `.tmvp/` folder inside each product repo
   (import supports both via `--path`)?
