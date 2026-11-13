# prometheus

Prometheus (or any PromQL API: Mimir, Thanos, VictoriaMetrics). Fields and verbs: `tmvp llms types/prometheus` · `tmvp verbs prometheus`.

## Find it in the codebase
- `prometheus.yml`, `prom/prometheus` images in compose/Kamal accessories, `kube-prometheus-stack`, `/metrics` handlers (`prom-client`, `prometheus_client`, `promhttp`), env NAMES `PROMETHEUS_URL`.
- Often only reachable on the observability host → `via: <ssh system>` with a 127.0.0.1 url.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which host runs it per env, and on which port?
- Auth (token/basic, tenant id) — which sec name?

## Config
```yaml
envs:
  prod:
    systems:
      metrics:
        type: prometheus
        via: obs                          # only if private
        url: "http://127.0.0.1:9090"
        # token / user+password if protected
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod metrics query "count(up)"`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/metrics.md`: the product's own metric names (grep the code's counters/histograms) and the PromQL that answers on-call questions (error rate, p95 latency, queue depth). `ls <substring>` lists live metric names.
