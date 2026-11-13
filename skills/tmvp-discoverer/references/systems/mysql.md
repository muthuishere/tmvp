# mysql

MySQL / MariaDB (also PlanetScale, RDS/Aurora MySQL, Cloud SQL MySQL). Fields and verbs: `tmvp llms types/mysql` · `tmvp verbs mysql`.

## Find it in the codebase
- Drivers/ORMs: `grep -rlE 'mysql2|mysqlclient|pymysql|aiomysql|go-sql-driver/mysql|gorm.io/driver/mysql|com.mysql|mariadb|"mysql"|provider = "mysql"|knex.*mysql|typeorm.*mysql|doctrine|ActiveRecord'`
- Schema: `migrations/`, `db/migrate/`, `database/migrations/` (Laravel), `prisma/schema.prisma`, Flyway/Liquibase folders.
- Infra: compose images `mysql:*`, `mariadb:*`; Kamal `accessories:`; helm `bitnami/mysql`; terraform `aws_db_instance` engine mysql.
- Env var NAMES: `DATABASE_URL`, `MYSQL_*`, `DB_HOST`, `DB_DATABASE`.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which envs have this system (dev / staging / prod)? Confirm the list you derived from the code.
- Is it reachable directly from the engineer's machine, or only from a server (then `via: <ssh system>`)?
- Is there a read-only login for it? If not, reads use the same login (tmvp still forces read-only where it can).

## Config
```yaml
envs:
  prod:
    destructive: false
    secrets:
      mysql_dsn: "$file(sec get acme@production:MYSQL_DSN --out ${out})"      # go-sql-driver DSN or mysql:// URL
    systems:
      db:
        type: mysql
        via: box                          # only if private
        read: { dsn: "${secrets.mysql_dsn}" }
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod db health` · `tmvp --skill <dir> all db query "select 1"`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/db.md` in the product skill:
- every database and table per env (from migrations/ORM models AND the live `tmvp --skill <dir> all db ls` / `describe <table>`), one line each: name · purpose · owning code module (`path/to/file`).
- which code modules read/write which table (grep the names you found).
- env differences (a table only in prod, different names) and gotchas.
Save read-only queries the on-call engineer will need as `assets/db/queries/<name>.sql` — derived from real queries/repositories in the code, plus: latest rows, counts by status, recent errors/failures, stuck/pending jobs older than N minutes, rows for one user/order id.
Same `/*--- description: … params: [...] ---*/` front-matter as postgres; `select` only.
