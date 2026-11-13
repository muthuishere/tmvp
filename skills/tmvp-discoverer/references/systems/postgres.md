# postgres

PostgreSQL databases (also Supabase, Neon, RDS/Aurora Postgres, Cloud SQL). Fields and verbs: `tmvp llms types/postgres` · `tmvp verbs postgres`.

## Find it in the codebase
- Drivers/ORMs: `grep -rlE 'psycopg|asyncpg|sqlalchemy|django.db|"pg"|from .pg.|pg-promise|postgres\(|prisma|typeorm|knex|sequelize|drizzle-orm/pg|jackc/pgx|lib/pq|gorm.io/driver/postgres|org.postgresql|r2dbc-postgresql|Npgsql|sqlx'`
- Schema: `prisma/schema.prisma` (`provider = "postgresql"`), `migrations/**/*.sql`, `db/migrate/`, `alembic/versions/`, `src/main/resources/db/migration/` (Flyway), `liquibase`, `drizzle/`, `*.entity.ts`.
- Infra: `docker-compose*.yml` / `compose*.yaml` images `postgres:*`, `postgis/*`; Kamal `config/deploy*.yml` `accessories:` with a postgres image; k8s/helm `bitnami/postgresql`, `cloudnative-pg`, `StatefulSet` with postgres; terraform `aws_db_instance` (engine postgres), `google_sql_database_instance`.
- Env var NAMES (never open `.env*`): `DATABASE_URL`, `POSTGRES_*`, `PG*`, `DB_HOST`, `SPRING_DATASOURCE_URL`. Use `sec ls --json` / `sec env ls <env>` for which names exist.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which envs have this system (dev / staging / prod)? Confirm the list you derived from the code.
- Is it reachable directly from the engineer's machine, or only from a server (then `via: <ssh system>`)?
- Is there a read-only login for it? If not, reads use the same login (tmvp still forces read-only where it can).
- Which database names does each env have (one per service in a monorepo)?

## Config
```yaml
envs:
  prod:
    destructive: false                    # prod-like: always
    secrets:
      pg_url: "$file(sec get acme@production:DATABASE_URL --out ${out})"
      pg_ro_url: "$file(sec get acme@production:DATABASE_URL_READONLY --out ${out})"   # only if it exists in sec
    systems:
      db:
        type: postgres
        via: box                          # only when the DB is private to a server; else omit
        read:  { dsn: "${secrets.pg_ro_url}" }
        write: { dsn: "${secrets.pg_url}" }   # omit → writes use the read dsn via tmvp write
      # DB in a container on the box: via: [box, {container: acme-postgres}]
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod db health` then `tmvp --skill <dir> all db query "select 1"`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/db.md` in the product skill:
- every schema and table per env (from migrations/ORM models AND the live `tmvp --skill <dir> all db ls` / `describe <table>`), one line each: name · purpose · owning code module (`path/to/file`).
- which code modules read/write which table (grep the names you found).
- env differences (a table only in prod, different names) and gotchas.
Save read-only queries the on-call engineer will need as `assets/db/queries/<name>.sql` — derived from real queries/repositories in the code, plus: latest rows, counts by status, recent errors/failures, stuck/pending jobs older than N minutes, rows for one user/order id.
```sql
/*---
description: orders stuck in pending for more than N minutes
params: [minutes]
---*/
select id, status, created_at from orders
where status = 'pending' and created_at < now() - ($1 || ' minutes')::interval
order by created_at limit 50
```
Only `select`/`with … select`. Prove each saved query once: `tmvp --skill <dir> dev db @<name> <args>`.
