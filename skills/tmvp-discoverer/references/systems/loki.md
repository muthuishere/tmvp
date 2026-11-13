# loki

Loki log queries (LogQL), self-hosted or Grafana Cloud. Fields and verbs: `tmvp llms types/loki` · `tmvp verbs loki`.

## Find it in the codebase
- `loki` / `grafana/loki` / `promtail` / `alloy` images, `loki-config.yaml`, OTEL exporters, env NAMES `LOKI_URL`, `OTEL_EXPORTER_*`.
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
      logs_q:
        type: loki
        via: obs                          # only if private
        url: "http://127.0.0.1:3100"
        # token / user+password if protected
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod logs_q labels`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/logs_q.md`: label sets per service (`labels`, `values <label>`) and save LogQL as `assets/logs_q/queries/<name>.logql` (errors per service, a request id). Note: `logs` is a reserved name — call the system `logs_q` or `loki`.
