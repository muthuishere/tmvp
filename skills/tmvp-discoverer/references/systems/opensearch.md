# opensearch

Opensearch clusters: indices, mappings, searches, counts. Fields and verbs: `tmvp llms types/opensearch` · `tmvp verbs opensearch`.

## Find it in the codebase
- Clients/infra: `opensearchproject/*` images, `@opensearch-project/opensearch`, `opensearch-py`, terraform `aws_opensearch_domain`.
- Index names: string literals passed to index/search calls, index templates, ILM policies.
- Env var NAMES: OPENSEARCH_URL / OPENSEARCH_PASSWORD.

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
      os_pw: "$file(sec get acme@production:OPENSEARCH_PASSWORD --out ${out})"
    systems:
      search:
        type: opensearch
        url: https://search.example.com
        user: reader
        password: "${secrets.os_pw}"
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod search health`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/search.md`: indices/aliases per env (from `ls` and the code), which service writes each, key mapping fields. Save searches as `assets/search/queries/<name>.json` with front-matter `index:` (recent errors, a document by id, counts per status).
