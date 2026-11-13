# loki

Grafana Loki (logs). Read only. Every query needs a real label selector: `{}`, `{app=""}` or `{app=~".*"}` alone are refused (`guard_rejected`) before Loki sees them.

## Fields
url (or base_url, the Loki root — no `/loki/api`), token (bearer) or user/password (basic; Grafana Cloud = instance id + API token), tenant (`X-Scope-OrgID`), headers, ca, insecure, max_range (default `7d`).

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls [substring] — label names (over --since, default 6h) + saved queries/*.logql |
| `labels` | read | labels — label names seen in --since (default 6h) |
| `values` | read | values <label> — values of a label in --since (default 6h) |
| `query` | read | query '<logql>' [-F time=RFC3339] — instant query (metric LogQL → one row per series) |
| `range` | read, @item | range '<logql>'\|@saved — log lines (newest first) or metric samples over --since (default 1h, ≤ max_range 7d); lines capped by --limit; -F step=1m -F direction=forward |
| `tail` | read | tail '<logql>'\|@saved [-F follow=10s] — last lines of --since (default 15m) oldest→newest; follow polls for new lines, bounded by time and --limit |
| `series` | read | series '<selector>' — label sets of matching streams in --since (default 1h) |
<!-- END verbs -->

## Discovery
`ls` = label names + saved LogQL `assets/<system>/queries/<name>.logql` (`tmvp prod logs @errors` runs `range`). Then `values <label>` to build a selector.

## Examples
```
tmvp prod logs ls
tmvp prod logs values app
tmvp all logs range '{app="api"} |= "error"' --since 30m --limit 100
tmvp prod logs query 'sum by (app) (rate({env="prod"} |= "error" [5m]))'
tmvp prod logs tail '{app="api"}' -F follow=15s
tmvp prod logs @errors --since 2h
```
