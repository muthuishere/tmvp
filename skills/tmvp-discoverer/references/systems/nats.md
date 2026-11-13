# nats

NATS / JetStream: streams, consumers, KV buckets. Fields and verbs: `tmvp llms types/nats` · `tmvp verbs nats`.

## Find it in the codebase
- Clients: `grep -rlE 'nats.go|nats-io|"nats"|nats-py|nats.ws|jnats'`; subjects/stream names in code; compose `nats:*`; env NAMES `NATS_URL`, `NATS_CREDS`.

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
      nats_creds: "$file(sec get acme@production:NATS_CREDS --out ${out})"
    systems:
      nats:
        type: nats
        url: nats://REPLACE_ME:4222
        creds: "${file:secrets.nats_creds}"
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod nats ls`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/nats.md`: streams, subjects and consumers → owning code module; KV buckets and their key shapes.
