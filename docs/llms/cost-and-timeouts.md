# Cost guard & timeouts

Every READ is estimated before it runs. If the estimate is over the call's
effective timeout, tmvp does not run it and fails with `too_expensive`
(exit 3) — nothing reached the system except the cheap estimate itself.

```
error: [prod.db] too_expensive: prod.db est. ~42.0s (Seq Scan on events (~4.2M rows in table)) > timeout 10s
  (hint: narrow the query, or --timeout 63s, or --force)
```

What to do, in order of preference:
1. **Narrow the query** — add a WHERE on an indexed column, a shorter `--since`, a tighter label selector.
2. **`--timeout <dur>`** — raise the limit for this one call; it runs if the estimate is ≤ it.
3. **`--force`** — run regardless of the estimate. Ask the user first on prod.

The timeout is **always also a hard kill**, `--force` or not: the call's
context deadline plus the server-side limit (postgres `statement_timeout`,
mysql `max_execution_time`, mongo `maxTimeMS`, elasticsearch/prometheus
`timeout` param, loki/grafana request deadline, CLI process timeout).
A killed call fails with `timeout` (exit 1).

## Timeout precedence

`--timeout` > system `limits.timeout` > env `limits.timeout` > global `limits.timeout` > built-in **10s**.

```yaml
limits: { timeout: 20s }              # global (product-wide)
envs:
  prod:
    limits: { timeout: 15s }          # env
    systems:
      warehouse:
        type: postgres
        limits: { timeout: 2m, cost_per_second: 50000 }   # system
```

`tmvp config show` prints `effective_timeout: <dur> (<source>)` per system;
`tmvp doctor` shows a TIMEOUT column. A bare number is seconds.

## Examples

```
tmvp prod db query "select * from events where created_at > now() - interval '1h'"
tmvp prod db query "select count(*) from events" --timeout 2m     # estimate 42s ≤ 2m: runs
tmvp prod db query "select count(*) from events" --force          # skips the estimate; still killed at 10s
tmvp all db query @report                                          # batch: each target estimated on its own;
                                                                   # over-limit targets report too_expensive,
                                                                   # the rest run (exit 4 partial)
```

Writes are not estimated (`tmvp write … --preview` is the write dry-run);
their timeout follows the same precedence.

## Estimators per type

Estimates are **rough**: they turn a planner cost / doc count / byte count
into wall time with a calibration rate you can tune under `limits:`.
When estimation fails or the type has none, the read runs under the hard
timeout only (audit: `estimate: unavailable`).

| type | how it estimates | calibration (limits key, default) |
|---|---|---|
| postgres | `EXPLAIN (FORMAT JSON)` (no ANALYZE) in the read-only tx: total cost, plan rows, top node; Seq Scan on a table with `reltuples` > `seq_scan_rows` (1M) floors it at rows/`rows_per_second` | `cost_per_second` 100000, `rows_per_second` 2M |
| mysql | `EXPLAIN FORMAT=JSON`: `query_cost` + `rows_examined_per_scan`; names the biggest full scan | `cost_per_second` 100000, `rows_per_second` 1M |
| mongo | `explain` (queryPlanner) on find/aggregate/count: COLLSCAN → `estimatedDocumentCount` docs | `rows_per_second` 1M |
| elasticsearch / opensearch | `_count` for the query; leading wildcard / regexp / script → whole index doc count | `rows_per_second` (50M plain, 1M expensive) |
| loki | `/loki/api/v1/index/stats` for the selector + range → bytes | `bytes_per_second` 200MB/s |
| prometheus | `/api/v1/series` (bounded) × range/step points | `points_per_second` 10M |
| grafana | same as prometheus / loki when the datasource type is identifiable; else none | as above |
| aws / gcp / az logs | range + filter heuristic (no filter and range > 1h → over the 10s default) | — |
| k8s, ssh, script, git, http, web, kafka, redis, nats, portforward | none — timeout only | — |

## Audit

Every read's audit record carries `estimate` (`{duration_ms, rows, bytes, why, confident}`,
or `unavailable…` / `skipped (--force)`), `timeout_ms`, `timeout_source`
(flag|system|env|global|default), `force` and `blocked`. `tmvp logs --json` shows them.
