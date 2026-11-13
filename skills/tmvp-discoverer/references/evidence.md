# Where systems hide (read names and structure, never secret values)

| Look at | Tells you | tmvp type |
|---|---|---|
| `git remote -v` | GitHub/GitLab repo | `gh` (repo: owner/name) · `git` for history |
| `.github/workflows/*.yml` | deploy targets per env, registry, cloud logins, which secrets CI uses (names) | `gh`; also hints for ssh/k8s/aws |
| Kamal `config/deploy*.yml` (`servers:`, `accessories:`, `registry:`, `proxy:`) | app hosts per destination, container names (`<service>-web-<sha>` → use prefix `<service>-web`), db/redis accessories | `ssh` per host; `postgres`/`redis` `via: [box, {container: <name>}]` |
| `docker-compose*.yml` | services, ports, networks | `ssh` + container hop |
| `k8s/`, `helm/`, `kustomization.yaml`, `kubectl config get-contexts` (names only) | clusters, namespaces per env | `k8s` (context, namespace) |
| `terraform/`, `pulumi/`, `infra/*/` folder names | cloud accounts, buckets, DNS, queues | `aws`/`gcp`/`az`, `http` for SaaS APIs |
| `infra/observability`, `prometheus.yml`, `loki`, grafana provisioning | metrics/log/dashboard URLs and which host runs them | `prometheus`, `loki`, `grafana` (`via:` the ssh host when bound to localhost) |
| Variable *names*: `tmvp env-keys` (every .env* in the repo, names only), `sec ls --json`, `sec env ls <env>`, `sec project ls` | which services exist per env: `POSTGRES_*`, `REDIS_*`, `S3_*`, `SES_*`, `RAZORPAY_*`, `OPENAI_*`… | the matching type; SaaS keys → `http` with saved read-only APIs |
| `ssh -G <alias>` (hostname/user/port/identityfile lines only; never key files) | which alias/key reaches which host | `ssh` user/host/key path |
| README / docs/ / runbooks | prod topology, "db lives on X" | confirms the above |

## Mapping hints
- One ssh system per real host; name them by role (`box`, `dbhost`, `obs`), same names across envs when they play the same role.
- Two prod app hosts sharing one DB → either envs `prod1`/`prod2` (same systems, different `box`) or one `prod` env with `app1`/`app2`; follow how the team talks about them.
- A service reachable only from inside a host (localhost-bound Prometheus, a db in a container) → `via:` that host, never open its port.
- Prod DB login that can write: fine — tmvp forces read-only sessions on reads; writes need `tmvp write` (optional `write: { dsn }` for a separate user).
- CI-only secrets (registry tokens, deploy keys) are evidence of systems, not things the skill needs, unless a read uses them.

## Finding units in a monorepo
- Build/manifest files mark units: `go.mod`, `package.json` (+ `workspaces`/`pnpm-workspace.yaml`/`turbo.json`/`nx.json`), `pyproject.toml`, `pom.xml`/`build.gradle` modules, `Cargo.toml` workspace members, `Dockerfile*`.
- Deploy files say which units ship together: one Kamal file / helm release / CI deploy job that builds several folders = one deploy unit.
- Shared data is the strongest grouping signal: folders that open the same DB/queue usually belong in one project (same blast radius). Folders with their own stores can be separate projects.
- `migrations/`, `prisma/`, `db/`, `schema.sql`, `alembic/`, `flyway`, `liquibase`, `ent/schema`, ORM model folders → the tables of that unit's database.

## Finding the queries people actually run
- Repositories/DAOs/`*_repo.*`/`queries/*.sql`/sqlc `query.sql`/raw `db.Query(`/`.raw(`/`$queryRaw`/knex builders.
- Admin pages and dashboards (`admin/`, Grafana SQL panels) — the reads humans already care about.
- Runbooks/README snippets with `select`.
- Background jobs and their status columns (stuck/failed jobs are the first on-call question).
