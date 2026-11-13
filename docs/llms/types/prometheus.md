# prometheus

Prometheus HTTP API (also Thanos/Mimir/VictoriaMetrics). Read only.

## Fields
url (or base_url), token or user/password, headers, insecure

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls [substring] — metric names (label __name__ values), filtered by substring, capped by --limit |
| `query` | read | query "<promql>" [-F time=RFC3339] — instant vector → one row per series |
| `range` | read | range "<promql>" [-F step=30s] — over --since (default 1h, ≤7d), one row per sample |
| `alerts` | read | alerts — active alerts (name, state, severity, since) |
<!-- END verbs -->

## Discovery
`ls [substring]` = metric names.

## Examples
```
tmvp prod prom ls http_requests
tmvp all prom query "sum(rate(http_requests_total{code=~\"5..\"}[5m]))"
tmvp prod prom range "up" --since 6h -F step=1m
tmvp prod prom alerts
```
