# grafana

Grafana: dashboards, datasources, alerts, and queries through its datasources. Fields and verbs: `tmvp llms types/grafana` · `tmvp verbs grafana`.

## Find it in the codebase
- `grafana/grafana` images, `provisioning/dashboards/*.json`, `grafana.ini`, env NAMES `GRAFANA_URL`, `GRAFANA_TOKEN`.
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
    secrets:
      grafana_token: "$file(sec get acme@production:GRAFANA_TOKEN --out ${out})"
    systems:
      grafana:
        type: grafana
        via: obs                          # only if private
        url: "https://grafana.example.com"
        token: "${secrets.grafana_token}"   # Viewer service-account token
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod grafana dashboards`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/grafana.md`: dashboard uids per service and the panel queries worth reusing (`dashboard <uid>` shows them); save expressions as `assets/<system>/queries/<name>.expr` with front-matter `datasource: <uid>`.
