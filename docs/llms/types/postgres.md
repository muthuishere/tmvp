# postgres

PostgreSQL. Reads run on `read.dsn` inside `BEGIN READ ONLY` with statement/lock timeouts and a row cap; writes on `write.dsn` in a transaction (`--preview` rolls back and reports rows affected).

## Fields
read.dsn / write.dsn (secret: `postgres://user:pass@host:5432/db`), or host/port/dbname/user + password; `via:` for tunnels; `limits:`

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls — saved queries (@name, all asset roots) then live tables, views, matviews |
| `health` | read | ping, server version, recovery state, latency |
| `describe` | read | describe            → tables; describe <table> → columns |
| `query` | read, @item | query "<sql>" [params...] \| query @saved [params...] (read-only tx, row cap) |
| `exec` | write, default | exec "<sql>"\|@saved [params...] (tmvp write; --preview = rolled-back tx) |
<!-- END verbs -->

## Discovery
`ls` = saved queries (assets `queries/*.sql`) + live tables/views/matviews; `describe <table>` = columns.

## Examples
```
tmvp all db ls
tmvp prod db describe orders
tmvp all db query "select count(*) from orders where created_at > now() - interval '1 day'" --diff
tmvp prod db @unpaid-orders 7
tmvp write prod db "update orders set status='paid' where id=42" --reason "support #812" --preview
```
