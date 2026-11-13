# tmvp — Research: config prior art + competitors

2026-11-10 · Product-only comparison. Links marked † were not re-fetched; complaints are from model knowledge, not cited issues.

## A) Prior art: config shapes

| Tool | Config shape | What tmvp should copy / avoid |
|---|---|---|
| **Steampipe** ([connections](https://steampipe.io/docs/managing/connections), [aggregator](https://steampipe.io/blog/spc-aggregator), [search_path](https://steampipe.io/docs/guides/search-path)) | `connection "aws_prod" { plugin="aws" profile="prod" }` and `connection "aws_all" { type="aggregator" plugin="aws" connections=["aws_*"] }` | **Copy:** aggregators as first-class virtual systems, glob selection, and a `_ctx`/connection column on every merged row so you know which env it came from. **Avoid:** a restart after every config edit; tmvp's server should hot-reload. Third-party generators that build this from AWS Orgs ([unicrons](https://unicrons.cloud/en/2024/10/18/automate-your-steampipe-aws-configuration-with-aws-organizations/)) show how painful writing it by hand is, so offer `tmvp import`. |
| **kubeconfig** † (kubernetes.io/docs/concepts/configuration/organize-cluster-access-kubeconfig) | `clusters[]`, `users[]`, `contexts[]{cluster,user,namespace}`; the `users.exec:` plugin runs a command to get credentials | **Copy:** `exec:` credential plugins (= your `$(cmd)` secrets), and a *context* being a named tuple. **Avoid:** one global mutable `current-context`, which is the classic "ran it in prod" footgun. tmvp should have no current env at all, only explicit selection. |
| **~/.ssh/config** † | `Host prod-db` / `ProxyJump bastion-prod` / `LocalForward 5432 db:5432` / `Match exec "…"` | **Copy:** `via:` with the same meaning as ProxyJump: a chain `via: [bastion, jump2]`, resolved recursively, with cycle detection. Also copy `Match exec` for conditions. **Avoid:** first-match-wins wildcard precedence, which confuses people. |
| **dbt profiles.yml** † (docs.getdbt.com/docs/core/connect-data-platform/profiles.yml) | `profile: { target: dev, outputs: { dev: {...}, prod: {...} } }` plus `"{{ env_var('PG_PASS') }}"` | **Copy:** env → outputs nesting is exactly your env → systems. Copy `env_var('X','default')` defaults. **Avoid:** Jinja in config, which gives people a Turing-complete footgun. |
| **MCP Toolbox tools.yaml** ([configure](https://googleapis.github.io/genai-toolbox/getting-started/configure), [sources](https://googleapis.github.io/genai-toolbox/resources/sources)) | `sources: my-pg: {kind: postgres, host: ..., password: ${PG_PASS}}`, `tools: q: {kind: postgres-sql, source: my-pg, statement: ...}`, `toolsets:` | **Copy:** `${ENV}` / `${ENV:default}` substitution done *before* YAML parsing, `kind:` as a discriminator, and `toolbox validate`. **Avoid:** being env-only (no lazy command secrets) and having no env axis. |
| **Teleport labels** † (goteleport.com/docs/reference/access-controls/roles) | resources carry `labels: {env: prod, team: x}`; roles use `db_labels: {env: [staging]}`; `tsh db ls env=prod` | **Copy:** label selectors (`-l env=prod,tier=db`) for both querying and write policy. Your free-form `tags` should be key=value, not bare strings. |
| **Terraform workspaces / Pulumi stacks** † | `Pulumi.prod.yaml` has `config: {app:dbHost: x, app:dbPass: {secure: ...}}`; `pulumi config set --secret` | **Copy:** per-env overlay files (`tmvp.prod.yaml` merged over the base), and secrets as a typed value (`{secure:}`) rather than a convention. **Avoid:** TF workspaces' implicit selected state. |
| **AWS config** † (docs.aws.amazon.com/cli/latest/userguide/cli-configure-sourcing-external.html) | `[profile prod]` / `source_profile=base` / `role_arn=...` / `credential_process = op read ...` | **Copy:** `credential_process` is the closest prior art to your secrets: a JSON contract plus an `Expiration` field. Have secret commands optionally return `{value, expires_at}` so the server knows when to re-fetch. `source_profile` chaining is also a good model for `login:`. |
| **docker context** † | `docker context create prod --docker host=ssh://prod` | **Copy:** `ssh://` URLs as transport. That is all. |
| **pg_service.conf** † (postgresql.org/docs/current/libpq-pgservice.html) | `[prod-ro]` / `host=... dbname=... user=ro` paired with `.pgpass` | **Copy:** split service definitions from secrets (a separate file or command). Also emit a generated `pg_service.conf` so plain `psql service=prod-ro` works through the tunnel. usql's `~/.usqlconfig`/DSN aliases are similar. |

### Recommended conventions for tmvp

1. **Naming:** address systems as `env.system`, e.g. `prod.orders-pg`, in lowercase kebab-case. Globs (`*.orders-pg`, `prod.*`) select across envs, the same way Steampipe's `aws_*` does.
2. **Interpolation:** keep `${vars.x}` / `${secrets.x}` / `${env.X}`, plus `${env.X:-default}` borrowed from shell and Toolbox. Substitute after parsing, per field, so a secret value can never inject YAML. `$$` escapes a literal `$`. Never interpolate secrets into the `tags` or `type` fields.
3. **Secrets:** `secrets: {pg_prod: {cmd: "sec get X --print", ttl: 1h}}`. Fetch lazily, keep in server memory only, and redact the value and its base64/url forms from all output. Optionally accept the AWS `credential_process` JSON shape so the command can return an expiry.
4. **Overrides:** precedence is base `tmvp.yaml` < `tmvp.<env>.yaml` < `TMVP_<ENV>_<SYSTEM>_<FIELD>` env vars < CLI `--set`. Show the merged result with `tmvp config show --resolved`, with secrets masked.
5. **Tags:** use key=value labels, with selectors like `-l env=prod,type=postgres` and set-based `tier in (db,cache)`, as in Teleport and k8s. Write policy is keyed on labels too.
6. **Tunnels:** `via: bastion-prod` or `via: [a, b]`, with the same meaning as ProxyJump. A tunnel is itself a system, so it can have its own `login:`. Bring tunnels up lazily, share them per server, and tear them down on idle.
7. **Login detection:** `login: {manual: "gcloud auth login", check: "gcloud auth print-access-token -q", every: 10m}`. On a failed check, return a structured `NEEDS_LOGIN prod.gke: run 'gcloud auth login'` to the agent and never attempt the login itself.
8. **No current env**, unlike kubeconfig. Every command names its target or selector. `tmvp validate` and `tmvp doctor` cover the job of `toolbox validate`.

## B) Competitors (products only)

| Product | What it does / setup | Multi-env | Secrets | Read/write safety + audit | Common complaints (from my knowledge) |
|---|---|---|---|---|---|
| **Teleport + MCP** ([DB over MCP](https://goteleport.com/docs/connect-your-client/model-context-protocol/database-access.md), [MCP access](https://goteleport.com/docs/ver/19.x/connect-your-client/model-context-protocol/mcp-access.md)) | Identity-aware proxy. MCP gives `teleport_list_databases` and `teleport_postgres_query` (**Postgres only**). Needs a cluster at v18.1 or later. | Yes, through labels across clusters | Short-lived certs; no static secrets | RBAC on labels, DB auto-user provisioning, full session audit. This is the strongest safety story here. | Heavy to self-host (auth/proxy/agents per resource). Some features are Enterprise-only. Overkill for one developer. |
| **HolmesGPT** ([toolsets](https://docs.robusta.dev/improve_holmes_docs/configuration/holmesgpt/custom_toolsets.html)) | An AI SRE agent with its own LLM loop. YAML "toolsets" (k8s, prometheus, github, etc.) live in `~/.holmes/config.yaml` or Helm. | One cluster/context per run, mostly | Env vars and k8s secrets | Mostly read-only toolsets; little audit | Brings its own agent rather than feeding Claude Code. Toolset config churn ([issue index](https://issues.ecosyste.ms/hosts/GitHub/repositories/robusta-dev%2Fholmesgpt/issues)). Token-heavy. |
| **Google MCP Toolbox** ([sources](https://googleapis.github.io/genai-toolbox/resources/sources)) | Go server; `tools.yaml` holds sources and parameterised SQL tools; MCP endpoint. | Only by duplicating sources | `${ENV}` only | Read-safety is whatever SQL you pre-write; OTel tracing | Databases only. No env axis, no tunnels, and every query has to be written in advance. |
| **Steampipe / Powerpipe** ([AWS orgs](https://steampipe.io/docs/guides/aws-orgs)) | SQL over more than 140 APIs through an embedded Postgres; aggregators; an MCP server exists. | **Best in class** for cloud accounts | Plugin config / cloud profiles | Read-only by design; no write path | Restart after config changes, cold-cache slowness and API throttling. Does not query your *app* databases or queues. |
| **k8sgpt** † (github.com/k8sgpt-ai/k8sgpt) | Analyzers scan the cluster and an LLM explains the findings; operator and MCP mode. | One kubeconfig context at a time | Anonymize flag masks names | Read-only | Shallow: mostly restates events. k8s only. |
| **Robusta** † | Prometheus alert enrichment plus Holmes; Helm installed per cluster | Per cluster, with a SaaS UI aggregating | k8s secrets | Playbooks can act (writes) | Needs the SaaS UI for the good parts; heavy Helm install. |
| **Komodor / Klaudia** † | k8s platform with an AI root-cause agent | Multi-cluster | Agent in the cluster | RBAC plus audit | Closed SaaS, k8s-centric, no local CLI or agent workflow. |
| **Kubiya** † | Agent platform with "teammates", runners and tools; approval flows | Through runners | Integration store | Approval gates on actions (worth borrowing) | Platform lock-in, complex setup, drifting away from developer tooling. |
| **Cleric / Resolve AI** † | Autonomous AI SRE investigating alerts across observability tools | Yes | Vendor-held integrations | Read-mostly with human approval | Closed; self-host is limited or VPC-only; you can't inspect its tools. |
| **Warp agent mode** † | Terminal agent runs your shell commands; "Warp Drive" holds env vars and workflows | Whatever your shell can reach | Can pull from 1Password/LastPass into env | Per-command approval; secret redaction | No system model or batching; credentials do end up in the shell the agent drives. |
| **Postgres/k8s MCPs** † (crystaldba/postgres-mcp, the reference `@modelcontextprotocol/server-postgres`, Flux159/mcp-server-kubernetes, containers/kubernetes-mcp-server) | One DSN or one kubeconfig per server | No; you register N servers, one per env | DSN in `.mcp.json`, which is often committed | Some have a `--read-only` mode; the reference PG server's read-only transaction was bypassable (archived) | Tool-list bloat with many servers, DSNs leaking into config, no cross-env view. |

### Where tmvp wins
- **One product across every env and every system type** (databases, queues, k8s, cloud, web UIs), queried in batches. Everything else here covers either one env or one system type.
- **Secrets as lazy commands held only in server memory**, so values stay out of the model's context and out of `.mcp.json`. Only Teleport does better, and it needs a cluster.
- **A generated skill for the coding agent you already use**, rather than another agent with its own LLM loop (HolmesGPT, Cleric, Kubiya).
- **Tunnels (`via:`) and login checks are part of the config.** None of the MCP servers model these.

### Where tmvp lags
- **Write and read enforcement.** Teleport enforces RBAC with short-lived certs on the server side. tmvp's `--reason` gate only works if read paths are truly read-only: use read-only DB roles and `SET TRANSACTION READ ONLY`, not trust in the agent.
- **Coverage.** Steampipe's breadth of cloud API tables is years of plugin work; for cloud APIs, consider wrapping Steampipe instead of competing with it.
- **No team or RBAC story yet.** The audit log is per machine.

### What to steal
1. Steampipe's aggregators, glob selectors and a source column on every merged row.
2. Teleport's label selectors, applied to both reads and the write policy.
3. Toolbox's `validate` command and `${X:default}` syntax.
4. AWS `credential_process`'s JSON contract with an expiry.
5. SSH ProxyJump's meaning for `via:`.
6. Kubiya-style approval for writes: `tmvp write` stages the change and you approve it from a phone through `notify`.
7. Emit `pg_service.conf` and kubeconfig fragments, so plain `psql` and `kubectl` also go through tmvp's tunnels.