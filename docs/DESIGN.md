# tmvp — Design

Status: draft v1 (skill model) · 2026-11-09 · Owner: Muthukumaran Navaneethakrishnan
Previous hub-model draft: `DESIGN-v0-hub.md` (kept for reference; superseded).

## 1. What it is

> **tmvp is a CLI that generates and runs a per-product agent skill.**
> `tmvp new skill reqsume` gives you `reqsume-tmvp`: one skill that knows reqsume's environments,
> systems and web apps. Anyone (any agent) using that skill works on reqsume, across its envs.

- **Not a platform, not SaaS, not enterprise.** No hosted server, no central registry, no multi-product hub.
  One local per-user server (section 4) keeps connections warm.
- **Install tmvp once per machine.** One binary + one warm server is shared by every product on the
  machine; connections, tunnels and logins are reused. **Each skill knows only its own product**
  (`reqsume-tmvp` sees reqsume and nothing else).
- The **skill does most of the work on its own**: it loads only the asset folder a task needs,
  batches reads across environments, and calls `tmvp` to execute.
- The skill lives **in the product's repo at `.agents/skills/<product>-tmvp/`**; its `config.yaml`
  holds only *commands* that fetch secrets, never values, so it can sit next to the skill.
- **tmvp is self-describing** (section 2.0): the generated skill is tiny and deterministic from
  config.yaml; an agent learns everything else from the CLI (`envs`, `systems`, `ls`, `verbs`,
  `llms`, `--llmhelp`). No generator skill, no AI-written guesses (owner, 2026-11-10).

### Trust model (owner decision)
tmvp trusts the agent; whoever holds access holds responsibility. Safety = read by default +
every mutation is an explicit `tmvp write --reason` + everything recorded, so any change can be
traced back. No approval gates, no trust prompts. Diagnosis is not a tmvp feature — the agent does it.

## 1.1 Functional requirements: performance, security, every OS
These are features with acceptance tests, not "non-functional" wishes. A release that misses
one is a broken release.

### FR-P Performance
| ID | Requirement | Acceptance test |
|---|---|---|
| P1 | Client overhead (call → server → first byte, warm) | p95 ≤ 20 ms on all OSes |
| P2 | Warm read, single env, `select 1` / k8s get pod | p95 ≤ 100 ms (+ remote latency) |
| P3 | Batch read across 3 envs | wall time ≈ slowest env + ≤ 30 ms (parallel, not summed) |
| P4 | Cold start (server boot + config + secrets + first connection) | p95 ≤ 3 s for 3 envs; secret commands run in parallel |
| P5 | Streaming | first env's results print before the last env finishes |
| P6 | Bounded output | default caps (1000 rows / 500 lines / 1 MB per env) so agent context stays small |
| P7 | Server footprint idle | ≤ 50 MB RSS, ~0% CPU; exits after 30 min idle |
| P8 | No query takes a system down | guard suite: unbounded scan/regex/consume is cut off by timeout/limit, system still healthy |

### FR-S Security
| ID | Requirement | Acceptance test |
|---|---|---|
| S1 | Secret values never in output, logs, audit, metrics, errors | canary secret test: value + base64/url/hex/split forms never appear in any byte tmvp emits |
| S2 | Secret values never on disk (except 0600/owner-ACL temp files, deleted after use) | filesystem scan after run + after kill -9 |
| S3 | Server reachable only by the same OS user | other-user connect to socket/pipe refused, on all OSes |
| S4 | Reads cannot mutate | mutation via read verb rejected; read credential + read-only session enforced per connector |
| S5 | Every mutation needs `tmvp write` + `--reason`; destructive only where config allows | policy test matrix |
| S6 | Every command audited (who/agent, what, why, outcome); write intent logged before execution | audit completeness test incl. crash mid-write |
| S7 | Config contains no literal secrets | `tmvp check` rejects literals in secret fields |
| S8 | Secrets dropped from memory on idle shutdown / `secrets clear` | server exit verified; no swap-to-disk files (best effort mlock where OS allows) |

### FR-X Every OS from day one
| ID | Requirement | Acceptance test |
|---|---|---|
| X1 | macOS, Linux, Windows — amd64 + arm64 — same features, same release | release blocked unless all 6 builds pass |
| X2 | Full test suite (incl. P and S tests) on all three OSes | CI matrix: macos, ubuntu, windows on every PR |
| X3 | Auto-start, idle shutdown, socket permissions, temp-file cleanup identical | per-OS lifecycle tests |
| X4 | Secret commands work with each OS's shells/CLIs | `$(…)` tests with sec/op/gcloud stubs on all OSes |
| X5 | Install in one step per OS | brew · winget/scoop · apt/rpm/curl script — smoke-tested in CI |

## 2. The generated skill

### 2.0 Self-describing CLI, tiny generated skill (owner decision 2026-11-10)
There is **no generator/author skill** and nothing is pre-written from guesses. `tmvp new skill`,
`tmvp skill add` and `tmvp skill sync` render `SKILL.md` **deterministically from config.yaml**:
product, envs (+ destructive), systems (name, type, labels, via, envs), the rules (batch reads,
writes only via `tmvp write … --reason`, `login_required` → show the user the command, never handle
secrets) and how to learn the rest:

```
tmvp envs | tmvp systems [env] | tmvp <env> <system> ls | tmvp verbs [type] | tmvp llms [topic|--full] | <any command> --llmhelp
```
- **`ls`** is a read verb every connector implements: what exists, live where possible, plus saved
  items with their source file (pg/mysql tables+views, mongo collections, redis prefixes from a
  bounded SCAN sample, nats streams/KV/object stores, ssh/script/cloud saved scripts, web pages,
  http APIs, prometheus metric names, git branches/tags, aws/gcp/az scope + whoami, portforward
  remote). k8s: pending its client-go rewrite.
- **`llms.txt`** (llmstxt.org) at the repo root and embedded in the binary, with `docs/llms/*.md`
  topics (overview, install, config, writes, errors, server, skills, troubleshooting, one per type).
  Per-type verb tables and `llms-full.txt` are generated from the connector registry
  (`go generate ./...`); a test fails on drift.
- **`--llmhelp`** on every command and verb: purpose, syntax, mode, flags, examples, related topic.
  Works with no config and no server.
- **`tmvp skill lint`**: structure only (SKILL.md front-matter, config validates incl. literal
  secrets, saved items have front-matter, asset paths exist, no secret values in any file).
- **Assets come from several repos**: `assets: [assets, ../ops-repo/tmvp-assets]` (relative to
  config.yaml; default `[assets]`). Saved items are looked up root by root, first match wins; `ls`
  shows the source path. Scaffolds create only empty folders people fill:
  `queries/ scripts/ pages/ apis/ manifests/ notes/`.

```
reqsume-tmvp/
├── SKILL.md        # generated from config.yaml (marker comment; hand-written SKILL.md is never overwritten)
├── config.yaml     # vars, secrets ($(…) — no values), assets roots, envs → named systems
└── assets/<system>/{queries,scripts,pages,apis,manifests,notes}/   # filled by people, front-matter first
```
http APIs: `apis/*.md` front-matter `{method, path, description, params, mutating}` and/or an
`openapi:` field (file, base_url path or URL); `call <api>` runs GET APIs as reads, mutating ones only
via `tmvp write`.

### 2.0.1 Web assets — executable front-matter
Prose is for the agent; the YAML front-matter is what the web connector runs. `base_url` and `users`
live in `config.yaml` (they differ per env); the asset holds what is the same everywhere.

`assets/admin/context.md`:
```yaml
---
login:
  page: login                          # pages/login.md
  steps:
    - goto: /login
    - fill: { el: email, value: "${user.user}" }
    - fill: { el: password, value: "${user.password}" }
    - click: submit
    - wait: { url: /dashboard }
  success: { el: dashboard.heading }   # login verified when visible
  session: { reuse: true, ttl: 7d }    # browser state per product/env/app/user, 0600, outside git
never: [users.delete, billing.refund]  # element refs blocked where destructive: false
---
```
`assets/admin/pages/users.md`:
```yaml
---
page: users
path: /users
elements:
  search: { testid: users-search }
  rows:   { testid: user-row }
  delete: { role: button, name: "Delete user", mutating: true }
actions:
  find-user: [ { fill: { el: search, value: "{email}" } }, { press: Enter }, { wait: { el: rows } } ]
checks:
  - visible: { role: heading, name: Users }
---
```
Locators: `testid` → `role`+`name` → `text` → `css` (last resort). `page.element` refers across pages.
When `login: { manual: browser }` is set in config (SSO, 2FA, consent screens), `steps` are skipped
and the user logs in by hand (section 8.1).

### 2.1 SKILL.md (generated, short)
Rendered from `internal/skill/templates/SKILL.md.tmpl`; see `internal/skill/testdata/*.golden` for
the exact output. It names the product, envs, systems, four rules and the discovery commands — no
references, no schemas.

## 3. Config

The config contains **no secret values** — only `$(…)` commands that fetch them — so it is safe to
keep beside the skill. Anyone who runs it still needs access to the secret manager.

**Lookup order** (first found wins; `--config <path>` overrides all):
1. `./.tmvp/config.yaml` — in the current repo
2. `.agents/skills/<product>-tmvp/config.yaml` — next to the skill
3. `~/.tmvp/<product>/config.yaml` — per product in home
4. `~/.tmvp/config.yaml` — home default

The model: **top-level `vars` + `secrets`, then `envs` → named `systems`.** Every system is just a
`type` plus its fields; a type can appear any number of times in an env. No `tools:`, no `files:`,
no separate `web:` block.

```yaml
version: 1
product: reqsume

logging: { sink: file, path: ~/.tmvp/reqsume/logs }   # s3 later

vars:                          # plain values, reusable everywhere
  region: ap-south-1
  namespace: reqsume
secrets:                       # named $(command)s — fetched lazily, held in server memory only
  ssh_key:  "$(sec get REQSUME_SSH_KEY --print)"
  admin_pw: "$(sec get REQSUME_ADMIN_PW --print)"

envs:
  dev:
    destructive: true
    vars:    { domain: dev.reqsume.com, project: reqsume-dev }
    secrets: { db_ro: "$(sec get REQSUME_DEV_DB_RO --print)", db_rw: "$(sec get REQSUME_DEV_DB_RW --print)" }
    systems:
      db: { type: postgres, read: { dsn: "${secrets.db_ro}" }, write: { dsn: "${secrets.db_rw}" }, labels: { role: primary } }
  prod:
    destructive: false
    vars:    { domain: reqsume.com, project: reqsume-prod }
    secrets:
      db_ro:      "$(sec get REQSUME_PROD_DB_RO --print)"
      kubeconfig: "$(op read op://Reqsume/k8s-prod/kubeconfig)"
      gcp_sa:     "$file(node scripts/fetch-sa.js --out ${out})"   # command writes a file
    systems:
      bastion:   { type: ssh, host: 10.8.0.5, user: deploy, key: "${secrets.ssh_key}" }
      db-tunnel: { type: portforward, via: bastion, remote: db.internal:5432 }
      db:        { type: postgres, via: db-tunnel, read: { dsn: "${secrets.db_ro}" }, labels: { role: primary } }
      analytics: { type: postgres, read: { dsn: "$(sec get REQSUME_ANALYTICS_RO --print)" }, labels: { role: reporting } }
      cache:     { type: redis, url: "$(sec get REQSUME_PROD_REDIS --print)" }
      bus:       { type: nats, url: "${NATS_URL}", creds: "$(sec get REQSUME_NATS_CREDS --print)" }
      cluster:   { type: k8s, kubeconfig: "${file:secrets.kubeconfig}", context: reqsume-prod, namespace: "${vars.namespace}" }
      cloud:
        type: gcp
        project: "${vars.project}"
        credentials: "${file:secrets.gcp_sa}"
        login: { manual: "gcloud auth login", check: "gcloud auth print-access-token" }
      infra:     { type: git, url: git@github.com:deemwar/reqsume-infra }
      admin:
        type: web
        base_url: "https://admin.${vars.domain}"
        labels: { team: ops, exposure: internal }
        login: { manual: browser }            # SSO/2FA: user logs in by hand once, session reused
        users:
          ops:   { role: read,  user: "ops@${vars.domain}", password: "${secrets.admin_pw}" }
        config: { sso: google, vpn: wireguard }
```
**Fields tmvp itself parses** (connectors never see raw commands): `type`, `via`, `labels`,
`destructive`, `login: {manual, check}` (also handed to the connector as `Target.Login`), `limits:
{rows, lines, bytes, timeout, statement_timeout}`. Everything else is the type's own fields
(`read.*` / `write.*` sub-maps per credential mode); portforward adds `local_port:` (see §5 Ports).

**Types (v1 list):** `ssh`, `postgres`, `mysql`, `mongo`, `redis`, `nats`, `k8s`, `aws`, `gcp`, `az`,
`git`, `web`, `http`, `prometheus`, `script`, `portforward`, `kafka`, `elasticsearch` (alias
`opensearch`), `loki`, `grafana`. Storage (S3/GCS/blob) goes through the cloud types. Drivers are
embedded Go libraries (pgx, franz-go, …) or stdlib net/http for HTTP APIs; external CLIs only for the
clouds and playwright. OpenTelemetry has no query API — query where it lands (`tmvp llms logs-and-metrics`).

**Rules**
- **Interpolation:** `${vars.x}`, `${secrets.x}`, `${user.x}` (web login steps), `${NAME}` = process env,
  `${NAME:-default}` for a default. `${…}` and not `{{…}}`, so a config can also pass through
  `sec render` untouched. Substitution happens **after** YAML parsing, per field, so a value (a
  secret above all) can never change the config's structure. `$$` is a literal `$`.
- **Override:** an env's `vars`/`secrets` override top-level ones with the same name. One config
  file per product, no merging across lookup locations.
- **Secrets as files.** Some secrets are files (kubeconfig, GCP service-account JSON, ssh key, certs)
  and some sources only produce files (a vault CLI or a node script that writes one):
  - **in:** `$file(cmd)` — tmvp gives the command a private temp path as `${out}`; the command writes
    there; tmvp reads the content into memory and deletes the file at once. `$(cmd)` stays stdout.
  - **out:** `${file:secrets.x}` in a field means "give this a file path, not the content". Built-in
    connectors (k8s, ssh, gcp, aws, az) take the bytes in memory and never touch disk. Only when an
    external CLI needs a path is it materialised: Linux `memfd` (`/proc/self/fd/N`, never on
    disk), elsewhere a 0600/owner-ACL file in a private dir, alive only for that call, then deleted.
  - Either way the content is a secret: held in memory, redacted, never logged.
- **Expiry:** a secret command may print JSON `{"value": "…", "expires_at": "<RFC3339>"}` (the AWS
  `credential_process` shape) instead of a bare value; the server re-fetches before it expires.
- **Secrets are just commands.** `"$(any command)"` → tmvp runs it (no shell, argv parsed;
  `sh -c` only if the command needs pipes), captures stdout in memory. Inline `$(…)` in a field is
  allowed too. tmvp knows no provider names. Literal values in secret fields (`dsn`, `password`,
  `token`, `key`, `creds`, `kubeconfig`, `credentials`) are rejected.
- **Labels:** free-form `key: value` on any system (Teleport/k8s style). tmvp only selects by them
  (`-l role=primary,team=ops`, `-l 'role in (primary,reporting)'`); their meaning is the skill's.
- **`via:`** names a `portforward` (over `ssh` or `k8s`) or an `ssh` host — same meaning as ssh
  `ProxyJump`. A chain is allowed (`via: [bastion, jump2]`), resolved in order; cycles are rejected
  by `tmvp validate`. The server opens tunnels lazily, shares them, closes them on idle.
  A **container hop** `via: [box, {container: pg}]` (optional `network:`, `sudo: true`) must
  directly follow an ssh hop and be last: on each new connection tmvp runs
  `docker inspect` over that ssh client (cached ≤3s, names validated, no shell injection), picks
  the named or first network's IP and dials `<ip>:<port>` from the ssh host. No docker CLI or API
  library locally (owner decision 2026-11-11: ssh→docker is mandatory).
- **No current env** (superseded 2026-11-11 by §3.1 — kept for history). Unlike kubeconfig there is no selected env/context; every command names its
  envs or uses a selector.
- **Names:** system names, env names, `all` and verbs live in separate namespaces but must not
  collide — `tmvp check` rejects an env named like a system or verb, so `tmvp [envs] <system> …`
  is never ambiguous.
- Same system names across envs, so one `assets/<name>/` covers all envs.
- The config is a **one-time fill**: the generator writes `config.yaml` with `$(…)` slots and a
  comment per slot explaining which secret it needs; the engineer puts in their manager's command:
  ```yaml
  dsn: "$(sec get REQSUME_PROD_DB_RO --print)"
  dsn: "$(gcloud secrets versions access latest --secret=reqsume-prod-db-ro --project=reqsume)"
  dsn: "$(aws secretsmanager get-secret-value --secret-id reqsume/prod/db-ro --query SecretString --output text)"
  dsn: "$(op read op://Reqsume/prod-db/ro-dsn)"
  dsn: "$(lpass show --password reqsume-prod-db-ro)"
  ```
- Literal values in secret fields (`dsn`, `password`, `token`, `key`) are rejected.
- `tmvp check` validates the config and reports which refs resolve (names only).

### 3.1 Environment selection (owner decision 2026-11-11)

Top-level `default_env: dev` (must name an env). When a command does not name its envs, the
first of these wins: **positional selector / `--env, -e`** (`explicit`) > **`TMVP_ENV`** (one shell)
> **`tmvp use <env>`** (persisted per product + canonical config path in `TMVP_HOME/state/env.json`,
0600; `tmvp use` prints it and its source; `--clear` resets) > **`default_env`** > nothing → reads
run on all envs as before. Batch is unchanged: `all`, `dev,prod`, `prod*`, `*.db`, `-l` always run
across the named envs in parallel. The text header (stderr), every JSONL frame (`env_source`) and
every audit record say which env(s) ran and the source. `tmvp use` accepts any env. **One force
rule:** a write to a protected env (`destructive: false`) needs `--force` in addition to
`--reason`, however the env was chosen (named, `-e`, `TMVP_ENV`, `use`, `default_env`); `--preview`
is exempt. Writes stay one env + one system. `use` and `certs` are reserved words.

## 4. Runtime — thin client + always-on local server

Booting per call is too slow: not the Go binary, but **driver/connection setup** — TLS +
auth to Postgres, k8s API discovery, NATS/Kafka handshakes, launching a browser, running every
secret command. So tmvp is **one binary, two roles**:

```
agent / skill ──► tmvp (client, ~5 ms) ──unix socket──► tmvp server (always running)
                                                        ├─ config per product (watched, hot reload)
                                                        ├─ secrets: resolved once, memory only
                                                        ├─ connection pools per product/env/system
                                                        ├─ tunnels (portforward/ssh via) kept warm
                                                        ├─ web sessions (playwright-cli) per app/env/user
                                                        ├─ batch engine, guards, concurrency caps
                                                        └─ audit log + metrics (single writer)
```
- **Auto-start:** the first `tmvp …` call starts the server if it isn't running (detached), then
  forwards. No setup step for the engineer. `tmvp server start|stop|status|restart` exist too.
- **One server per user**, serving every product whose config it has loaded (by lookup order,
  section 3). Products load lazily on first use. **Everything is keyed by product**: config,
  secrets, pools, tunnels, sessions, audit. Each client call sends its product + resolved config
  path; a call can never name, list or reach another product's systems, and one product's
  failures or reloads never touch another.
- **Warm state:** pg pools (min 1 idle conn per env once used), k8s clients with cached discovery,
  NATS/Kafka connections, logged-in browser sessions. Typical warm read: tens of ms + query time.
- **Idle:** the server shuts itself down after **30 min with no request** (`server.idle`,
  default 30m; every call resets the timer). Shutdown closes all connections and browser
  sessions and drops every secret from memory. The next call boots it again (one cold start).
- **Config change** (file watch) → reload that product: re-run only changed `$(…)` commands, rebuild
  only affected pools. `tmvp secrets reload` forces a re-fetch after rotation.
- **Transport:** unix socket `~/.tmvp/run/tmvp.sock`, 0600, peer-UID check (same user only); no
  TCP port. Protocol: JSON request → streamed JSONL results (rows/lines per env), so batches
  print as each env finishes. Client exit code comes from the server's final status.
- **Robustness:** a crashed server is restarted by the next client call; in-flight reads are
  reported failed (no silent retry of writes). One wedged system never blocks others
  (per-system workers + timeouts). `tmvp server status` shows loaded products, pools, open
  sessions, degraded envs (missing secrets) — names only.
- **Single writer** for audit/metrics removes concurrent-append races between agents.
- Why not stay CLI-only: no warm connections, secrets re-fetched per call, and browser sessions
  can't persist. Why not a network service: no need — local, same user, no auth surface.

### 4.1 Every OS — macOS, Linux, Windows (amd64 + arm64)
- **Pure Go, `CGO_ENABLED=0`**: one static binary per OS/arch via goreleaser (brew, scoop/winget,
  apt/rpm, plain download). No cgo deps — so no pg_query parser or embedded DuckDB; SQL safety
  relies on read-only credentials + `BEGIN READ ONLY` + timeouts.
- **Socket:** unix domain socket on all three (Windows 10 1803+ supports AF_UNIX; Go handles it).
  No named pipe: Windows older than 10 1803 is unsupported. Unix: run dir 0700, socket 0600,
  peer uid checked (`SO_PEERCRED` / `LOCAL_PEERCRED`). Windows: run dir and socket get a protected
  DACL granting only the current user SID + SYSTEM (`golang.org/x/sys/windows`, no cgo); AF_UNIX
  has no peer-credential call there, so the DACL (connect needs write access to the socket file)
  is the isolation boundary.
- **Auto-start detached:** `setsid` on Unix, `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP` on
  Windows; idle shutdown the same everywhere.
- **Single-flight start:** the client takes an exclusive lock (`<sock>.start.lock`; flock on Unix,
  `LockFileEx` on Windows) before spawning and re-dials after acquiring it. The server holds
  `<sock>.lock` for its lifetime (non-blocking; held → "already running"), and only unlinks an
  existing socket after a probe-connect fails. OS locks die with the process, so a crash never
  leaves a stale lock.
- **Paths:** `~/.tmvp` = `$HOME/.tmvp` on Unix, `%USERPROFILE%\.tmvp` on Windows (`os.UserHomeDir`).
- **Secret commands `$(…)`:** parsed to argv and executed directly (no shell) on every OS; if a
  command needs pipes/redirection it runs via `sh -c` on Unix and `pwsh -NoProfile -Command`
  (fallback `cmd /c`) on Windows.
- **Private files** (temp kubeconfigs/keys, socket): 0600 on Unix; owner-only ACL on Windows.
- **External CLIs** (kubectl, aws, gcloud, az, git, playwright-cli, op, lpass, sec) resolved from PATH with
  `.exe`/`.cmd` lookup on Windows.
- **Repo language doesn't matter:** the generator skill analyzes any stack (Go, Java/Kotlin,
  Node/TS, Python, .NET, Ruby, PHP, Rust …) because the agent reads the code; ctx-optimize helps
  where available.
- CI matrix runs the test suite on macOS, Linux and Windows from day 1.

### 4.2 Hosted MCP — via mcpgate (later, not designed here)
Hosting is **not tmvp's job**. Our existing **mcpgate** project
(`~/muthu/deemwarworkspace/mcpgate`) turns a CLI + agent skill into a hosted MCP server (HTTP),
so people can add one URL and use the product skill with no install. tmvp only has to be
gateway-friendly:
- every command non-interactive, `--json` output, stable exit codes and error codes;
- `--reason` and all inputs as plain flags/args;
- the generated skill self-contained in its folder (SKILL.md + assets) so mcpgate can serve it.

## 5. Commands

Run from inside the product repo (skill auto-found) or with `--skill <path>`.

```
tmvp new skill <product> [--systems …]          # scaffold (SKILL.md generated from config)
tmvp skill add <name> <type>                    # add a system + its empty asset folders, re-sync SKILL.md
tmvp skill sync | tmvp skill lint [dir]         # regenerate SKILL.md · structure check
tmvp envs | tmvp systems [env] [--check]        # what is available (config only, never secrets)
tmvp [envs] <system> ls                         # what exists in the system + saved items (source path)
tmvp llms [topic] [--full]                      # agent docs (llms.txt); --llmhelp on any command
tmvp check                                      # config + refs + connectivity per env
tmvp doctor [--offline] [--env E] [--system S]  # machine, external tools, types, per-system secrets/login/connect, top fixes (no config needed)

tmvp [envs|all] <system> <verb> [args]          # reads; envs omitted → all
tmvp <selector> <verb> [args]                   # prod.* · *.db · prod.db · -l role=primary
tmvp validate                                   # config only: schema, refs, name collisions, via cycles
tmvp config show --resolved                     # merged config, every secret masked
tmvp export pg-service|kubeconfig [env]         # snippets so plain psql/kubectl use tmvp's tunnels
tmvp write <env> <system> <verb|stmt> --reason "…" [--preview]
tmvp [envs] <system> @name [args]               # bare saved item → the type's @item verb (ssh script, script run, pg/mysql query)
tmvp [envs] <web-system> <verb> [args]          # login, goto, text, snapshot, screenshot, check, pages, elements, click, fill, press, select, do
tmvp verbs [type...]                            # every connector's real verbs + mode (agents discover them at runtime)
tmvp login <env> <system> [--user ops]          # status / opens the manual login (section 8.1)
tmvp logs [--since 1d] [--writes]               # read back the event log / write audit
```

**Environment & health commands (2026-11-11, §3.1 / §8.2):**
```
tmvp use [env] [--clear]                        # active env per product (state 0600); prints env + source
tmvp -e <envsel> …  ·  TMVP_ENV=<envsel> tmvp … # override use/default_env for one command / shell
tmvp write [env] <system> … --reason "…" [--force]   # --force required on protected envs (destructive: false)
tmvp secrets status [-e envsel] [--days 30]     # per secret: state, source tool, fetched, updated, expiry, cert days, key present
tmvp secrets audit [--since 1d]                 # fetch/refresh/invalidate/failure per name + triggering command
tmvp certs [-e envsel] [--days 30]              # TLS endpoint + secret-held cert expiry (via-aware, read-only)
```
Flags: `--json`, `--diff`, `--limit N`, `--since DUR`, `--reason "…"` (optional on reads, required on writes).

### Verbs per system (as implemented — `tmvp verbs` is the live source)
Modes: **read** (plain call) · **write** (only `tmvp write … --reason`) · **destructive** (write +
`destructive: true`) · **read-write** (runs both ways; the connector refuses mutation unless the call
came through `tmvp write`, decided per call from `Call.Write`). **default** = write verb used when
`tmvp write` gets a non-verb (raw SQL, `@name`). **@item** = verb a bare `@name` runs on reads.

| Type | Read | Read-write | Write (via `tmvp write`) |
|---|---|---|---|
| (every type) | ls (k8s: pending) | | |
| postgres | health, describe, query `<sql\|@saved>` [@item] | — | exec [default] |
| mysql | health, describe, query [@item] | — | exec [default] |
| mongo | collections, find, aggregate, count | — | insert, update, delete, aggregate-out; drop* |
| redis | info, get, scan, ttl, type | — | set, del, expire; flushdb*, flushall* |
| nats | streams, consumers, sub, kv-get | — | pub [default], kv-put; purge*, stream-delete* |
| k8s | ctx, health, get, describe, logs, events, top | — | apply [default], scale, restart, rollout, delete, exec |
| ssh | health, script `@name` [@item] (`-F container=`), docker (allow-list: ps, logs, inspect (env masked), stats, top, images, compose ps/logs) | — | run [default] (`-F container=` → docker exec) |
| script | list, run `@name` [@item] | — | exec [default] |
| web | login, goto, text, snapshot, screenshot, check, pages, elements | click, fill, press, select, do [default] | (the read-write verbs) |
| aws / gcp / az | whoami, logs | call | cli [default] (alias of `call` through write) |
| git | log, diff, show, tags, deploys, fetch | — | — (never pushes) |
| http | get, health, apis | call [@item] | post [default], put, patch, delete |
| prometheus | query, range, alerts | — | — |
| kafka | topics, describe, groups, lag, consume (no group, no commit), health | — | produce [default], create-topic; delete-topic*, delete-records*, reset-offsets* |
| elasticsearch / opensearch | health, indices, mapping, search [@item], count, cat, get | — | index [default], update, delete, update-by-query; delete-index*, delete-by-query*, reindex* |
| loki | labels, values, query, range [@item], tail, series (selector required) | — | — |
| grafana | dashboards, dashboard, datasources, alerts, silences, annotations, query [@item] | — | annotate [default], silence create/expire |
| portforward | status, open, close | — | — |

`*` destructive — only where `destructive: true`.
Examples: `tmvp write prod admin click users.ban --reason "…"`, `tmvp write prod cloud call ec2
start-instances --instance-ids i-1 --reason "…"`, `tmvp prod bastion @disk`.

**Ports:** `tmvp <env> <portforward> open` listens on `127.0.0.1:<local_port>` (system field) or the
convention `15432 + fnv32(product/env/system) % 1000`; `tmvp export pg-service` prints the same
port for a postgres behind that portforward, so `psql service=<product>-<env>-<db>` works after `open`.

## 6. Batch across environments (reads)
- Envs omitted → **all envs**, in parallel, each with its own creds and timeout.
- Output is env-keyed (every merged row carries `env` and `system` columns; `--json` → `results[]` per env); `--diff` shows only where envs disagree.
- Env lacking the system → `skipped`. Some envs fail → exit 4 `partial`, results still returned.
- **Writes are never batched** by tmvp: one env, one system per `tmvp write`. Native bulk ops
  (deleteMany, multi-row SQL, `pg_restore`, `kubectl delete -l`) are fine inside that one target.

## 7. Read vs write, destructive, guards
- Reads run on the `read:` credential, in read-only sessions (`BEGIN READ ONLY`, k8s view context).
  A mutation through a read verb fails with a hint to use `tmvp write`.
- `tmvp write` uses the `write:` credential; requires `--reason`; `--preview` shows blast radius
  (e.g. rows affected in a rolled-back tx).
- The read role must be **unprivileged** (SELECT only): read-only transactions are defence in depth,
  not the boundary. SQL guards treat as destructive: DROP/TRUNCATE, DELETE/UPDATE without a WHERE at
  their own paren level (CTEs included), DO/EXECUTE/CALL, ALTER … RENAME, VACUUM FULL, CLUSTER, REVOKE
  (mysql: CALL/PREPARE/EXECUTE, RENAME TABLE, REVOKE). Pooled pg sessions are reset on release.
- Destructive (DROP/TRUNCATE/DELETE without WHERE, delete ns/topic/stream, purge, offset reset,
  restore over data) runs only where `destructive: true` (env or system level).
- **Cost guard (built 2026-11-11).** Every READ is estimated first by the connector's optional
  `Estimator` (EXPLAIN / explain / `_count` / index stats / series / range heuristic) after Open and
  before Do. Estimate > the call's effective timeout → not run: `too_expensive` (exit 3, like
  `policy_blocked`: refused before the system does work, the fix is the caller's decision), message
  `too_expensive: <env>.<system> est. ~<dur> (<why>) > timeout <t>`, hint
  `narrow the query, or --timeout <suggested>, or --force`. Estimation unsupported or failing → run
  with the hard timeout (audit `estimate: unavailable`). No plan command, no plan IDs.
  - Effective timeout: `--timeout` > system `limits.timeout` > env `limits.timeout` > global
    `limits.timeout` > 10s. `config show` / `doctor` print it (and its source) per system.
  - The timeout is ALWAYS also a hard kill, even with `--force`: ctx deadline per target (batch) +
    `Call.Timeout` passed server-side (pg statement_timeout, mysql max_execution_time, mongo maxTimeMS,
    es/prom `timeout`, loki/grafana request deadline, CLI process timeout).
  - `--force` (global flag): reads skip the estimate; writes keep its old meaning (protected env).
  - Batch: each target is estimated independently; over-limit targets report `too_expensive`, the
    rest run (exit 4 partial).
  - Calibration is rough and per system under `limits:` (`cost_per_second`, `rows_per_second`,
    `bytes_per_second`, `points_per_second`, `seq_scan_rows`). Per-type table:
    `tmvp llms cost-and-timeouts`. No estimator: k8s, ssh, script, git, http, web, kafka, redis, nats.
  - Audit: each read records `estimate` (or unavailable / skipped), `timeout_ms`, `timeout_source`,
    `force`, `blocked`.
- Guards so a read can't take a system down: statement/lock timeouts (pg), `maxTimeMS` (mongo),
  max time range + label selector (loki), bounded consume/sub, log tail caps, ssh `nice`/timeout,
  row/line limits, per-system concurrency cap. Defaults stricter on prod; overridable via `limits:`.

## 8. Secrets — load once, keep in memory

- **Load on the first command.** The first command after the server boots runs every `$(…)` in the
  config (in parallel, never interactive) and holds the values **in memory only**. Later calls reuse them.
- **Reload when changed.** Secrets rotate, so tmvp reloads instead of trusting the cache forever:
  - config file changed (mtime/hash checked on each command) → re-run the commands that changed;
  - a command fails with an auth error (bad password, expired token, HTTP 401) → re-run that one
    `$(…)`; if the value differs, rebuild that pool and **retry the command once**. Same value →
    fail with `secret_invalid`. The retry happens only when the failure was in connect/Open or the
    call is a read — **a write that reached the system is never re-run** (its error says so).
  - HTTP 403 / RBAC denial is **not** an auth error: k8s 403 → `policy_blocked` (the credential is
    valid, the role lacks the right); k8s 401 → `auth_error` → re-fetch (exec credentials too).
  - Secret values shorter than 4 characters are rejected (`secret_invalid`): too short to redact.
  - `tmvp secrets reload` forces it manually.
- The memory is the **tmvp server's** (section 4): values never written to disk, dropped when the
  server exits or on `tmvp secrets clear`; `tmvp secrets reload` re-runs the commands.
- **Validate after load.** Each resolved secret is checked (non-empty; for a DSN, parses and the
  connection authenticates). Result per key: `ok` / `failed` with a reason — names only, never values.
- **Missing access = error for that slot, not for the server.** If someone has no access to a secret,
  the server still starts; any command that needs that env/system fails with `secret_missing`
  (exit 2) naming the config key and the reason. Other envs/systems keep working. `tmvp check`
  shows the full validation table up front.
- File-shaped secrets (kubeconfigs, keys, GCP credentials JSON) are materialised 0600 in a temp dir
  (Linux: memfd, else /dev/shm, else the OS temp dir) only for the call that needs them, removed
  after; the server deletes stale `tmvp-sec-*` dirs (>15 min) at start.
- **Never on argv.** Secrets never go on a child process's command line (visible in `ps`): az SP
  login uses `--password=@<0600 file>`; web fill/select values go into a 0600 `run-code` script
  (`playwright-cli run-code --filename=…`), deleted after the call.
- **Lazy per named secret:** a secret runs on the first command that needs a system using it, not all
  at boot — so a missing secret fails only the systems that use it.

### 8.1 Manual login (owner decision)
Some logins can't be automated: `gcloud auth login`, `az login`, `aws sso login`, SSO/2FA web apps,
VPN. A system declares `login: { manual: "<cmd>", check: "<cmd>" }` (or `manual: browser` for web).
- Before a call, the server runs `check` (cached briefly). Failing → error `login_required`, exit 2,
  naming the exact command. tmvp never attempts the login itself and never prompts.
- The agent shows it to the user; in Claude Code they type `! gcloud auth login`, accept in the
  browser, and the agent retries.
- The system's `login:` reaches connectors as `Target.Login` (web reads `manual: browser` from it;
  `context.md` `login.manual` is the fallback).
- Web `manual: browser`: `tmvp login <env> <system>` opens a headed browser; the user logs in by hand
  (SSO, 2FA, consent); the session is saved (0600, per product/env/app/user) and later calls run
  headless until it expires, then `login_required` again.
- **tmvp never logs a secret**: a streaming redactor (plain/base64/url/hex, values split across
  chunks) sits in front of stdout, stderr, the audit log and metrics.

### 8.2 Secret & certificate health (2026-11-11)
`tmvp secrets status` fetches each in-scope secret and reports `ok|missing|invalid|login_required`,
the source tool (first argv word only), last fetch, `updated: yes|no|unknown` (an HMAC of the value
with a random per-server salt, memory only — never the value), `expires_at`, and for PEM /
kubeconfig / service-account secrets the earliest x509 `NotAfter`, days left and private-key
presence (no key material). The store emits `secret` audit records (fetch/refresh/invalidate/failure
+ triggering command) read back by `tmvp secrets audit`. `tmvp certs` handshakes (5s, via-aware,
`InsecureSkipVerify` only to read the chain; trust verified separately) with every TLS endpoint —
postgres via SSLRequest (any sslmode but `disable`; `prefer`/`allow`/unset report `no_tls` when the server declines), `https` url/base_url, the k8s API server, `rediss`/`tls`
/`?tls=true`, `host`/`brokers` with `tls: true` — warns under `--days`, exit 1 when expired or
unreachable. `tmvp doctor` adds `secrets:` and `certificates:` summary lines.

## 9. Logging
Per product (from the config), sink `file` (default, `~/.tmvp/<product>/logs/`) or `s3`.
**Every tmvp command is audited** — reads and writes — with its `--reason`.
- Event log (JSONL, OTel log shape): every command — env, system, verb, mode, outcome, duration,
  rows, caller agent, `--reason`.
- Write audit: every `tmvp write` attempt — statement (redacted), reason, preview/blast radius,
  outcome; intent record before, outcome record after.
- `metrics.prom` (Prometheus text) next to the log.
- Start on file; `tmvp logs push --to s3` copies history when switching. Parquet/Iceberg later if needed.

## 10. Errors
`code`, `message`, `hint`, env/system. Exit: 0 ok · 1 runtime/timeout/unreachable/auth ·
2 config/secret_missing · 3 policy_blocked · 4 partial · 5 guard_rejected.
Retries: reads only, transient only, once.

## 11. Code layout (tmvp repo)
```
cmd/tmvp/            # one binary: client + `server` role
internal/server/     # unix socket, request routing, lifecycle, auto-start, idle; keyed by product
internal/client/     # thin forwarder, streaming output, exit codes
internal/pool/       # per product/env/system connection pools + health
internal/skill/      # scaffold templates (SKILL.md, config.yaml, assets/*)
internal/config/     # lookup order, validate, env resolution
internal/secret/     # $(…) runner, in-server secret store, redactor
internal/batch/      # parallel per-env execution, diff
internal/mode/       # read/write/destructive classification, guards
internal/eventlog/   # file/s3 sinks, audit, prom
connectors/{ssh,postgres,mysql,mongo,redis,nats,k8s,aws,gcp,az,git,web,http,prometheus,script,portforward}/
```

## 12. Build order
1. Client/server skeleton (auto-start, socket, streaming) + config load + `$(…)` secrets in server memory + redactor.
1a. `tmvp new skill` scaffold.
1b. Self-describing CLI: `envs`, `systems`, `ls` per connector, `llms`, `--llmhelp`, `skill lint` (no generator skill).
2. Batch engine + read/write/destructive + guards + event log/audit.
3. postgres, k8s.
4. `tmvp write`, manual login.
5. ssh + portforward, web (playwright-cli), mongo, redis, nats, mysql, aws/gcp/az, git, http,
   prometheus, script. Then (built 2026-11-11): kafka, elasticsearch/opensearch, loki, grafana.
6. s3 sink.

Success check: an agent given only the generated reqsume skill discovers reqsume's real systems and envs via `tmvp envs/systems/ls`; fill config once, then an agent with only
`reqsume-tmvp` answers "failed signups last hour + api errors, dev vs prod" in one turn,
with no secret in its context.

## 13. Open questions
1. ~~Exact `sec` command~~ — `sec get NAME --print` (owner-only). Verify it works when the tmvp
   server (started from an agent session) runs it; else use `sec run`/`sec exec`.
