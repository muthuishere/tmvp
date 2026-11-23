# What each project skill must contain

```
.agents/skills/<project>-tmvp/
  config.yaml                      # envs + systems; secrets as commands only
  SKILL.md                         # generated: tmvp --skill <dir> skill sync (lists references/)
  references/
    projects.md                    # this project's folders, the other projects, blast radius
    environments.md                # envs, which are prod-like, default env, how they differ
    <system>.md                    # one per system (db.md, cache.md, gh.md, box.md …)
  assets/<system>/
    queries/<name>.sql             # saved read-only queries (data stores)
    scripts/<name>.sh              # ssh read scripts, front-matter read: true
    apis/<name>.md                 # saved read-only http calls
    notes/<topic>.md               # short gotchas
  scripts/<name>.js                # code-mode investigations (tmvp code run <name>), only ones the user picked
```
`.claude/skills/<project>-tmvp` is a relative symlink to it (made by `tmvp new skill`).

Every reference starts with front-matter `description:` (one line — `skill sync` shows it in SKILL.md) and every command in it is a tmvp command: `tmvp --skill .agents/skills/<project>-tmvp <env> <system> <verb> …`.

## references/projects.md
Table: project · folders · deploys as · systems. Mark this project. One line: "This skill can reach only the systems below; for <other> use `<other>-tmvp`."

## references/environments.md
Table: env · prod-like? · destructive · what runs where (roles, not IPs) · shared stores (e.g. "staging shares dev's DB"; a store shared with prod is protected everywhere) · differences. The env selection rules: `tmvp all …`, `tmvp <env> …`.

## references/<system>.md (data stores: postgres, mysql, mongo, redis, elasticsearch/opensearch, kafka, nats)
- What it is per env (type, via which host — roles only).
- **Databases / schemas** and **tables** (or keyspaces, indexes, topics, subjects): from migrations/models, confirmed by live `ls` / `describe`. Note tables present in code but missing live (pending migration) and vice versa.
- **Who uses what**: table → code modules (path) that read/write it.
- **Excluded / counts-only tables** and the prod budget (limit, timeout) — saved queries obey them.
- **Saved queries**: list each `@name` with its description and an example call.
- How to explore further: `ls`, `describe <table>`, `query "<sql>"`.

## Saved queries (assets/<system>/queries/*.sql)
Take them from the code and from on-call questions — never invented tables. Read-only SQL only, with front-matter:
```sql
/*---
description: signups per day, last N days (from internal/users/repo.go:42)
params: [days]
---*/
select date_trunc('day', created_at) d, count(*) from users
where created_at > now() - ($1 || ' days')::interval group by 1 order by 1 desc
```
Never select PII columns (emails, names, tokens) — ids and counts. Good first set per DB: row counts of the main tables, latest N rows of the core entity, counts by status (orders, payments, jobs), stuck/failed jobs, recent errors table, the lookups the code does most (`by email`, `by id`). Keep each one cheap (indexed filter or `limit`). Each must run ok in every env before you finish.

## references/<system>.md (other systems)
- ssh: role, containers/services and their name prefixes, log locations, saved read scripts.
- k8s: context/namespace per env, deployments.
- gh: repo, workflows per env (deploy-dev → dev …), how to read a failed run.
- prometheus/loki/grafana: key metrics, log labels/selectors the code emits, dashboards.
- aws/gcp/az: account/profile per env, buckets/queues/services used and by which code.
- http (SaaS): what the product uses it for, saved read-only APIs.
