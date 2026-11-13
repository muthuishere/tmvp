# portforward

A local port forwarded to a remote host:port (through ssh/k8s) for tools tmvp has no connector for. Fields and verbs: `tmvp llms types/portforward` · `tmvp verbs portforward`.

## Find it in the codebase
- Use only when the user needs a raw client (psql, a GUI) against a private service; prefer a real connector (postgres, redis, …) with `via:`.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which remote host:port (or k8s service) and through which hop?
- Which local port?

## Config
```yaml
envs:
  dev:
    systems:
      pg_tunnel:
        type: portforward
        via: box
        remote: "127.0.0.1:5432"
        local_port: 15432
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> dev pg_tunnel ls` (no dial) — `open` only when the user asks

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Mention the tunnel in the product skill's notes; nothing else to generate.
