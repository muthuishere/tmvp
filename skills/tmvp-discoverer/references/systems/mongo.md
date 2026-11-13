# mongo

MongoDB (Atlas, self-hosted, DocumentDB). Fields and verbs: `tmvp llms types/mongo` · `tmvp verbs mongo`.

## Find it in the codebase
- Drivers/ODMs: `grep -rlE 'mongoose|mongodb\b|MongoClient|pymongo|motor|mongoengine|go.mongodb.org/mongo-driver|spring-data-mongodb|prisma.*mongodb'`
- Models: `models/*.ts|js` with `new Schema(`, `@Document(`, `collection=` / `.collection('…')` calls — these give collection names.
- Infra: compose `mongo:*`; Kamal accessories; helm `bitnami/mongodb`; terraform `mongodbatlas_*`.
- Env var NAMES: `MONGO_URL`, `MONGODB_URI`, `MONGO_*`.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which envs have this system (dev / staging / prod)? Confirm the list you derived from the code.
- Is it reachable directly from the engineer's machine, or only from a server (then `via: <ssh system>`)?
- Is there a read-only login for it? If not, reads use the same login (tmvp still forces read-only where it can).
- Which database name per env, if not in the URI path?

## Config
```yaml
envs:
  prod:
    destructive: false
    secrets:
      mongo_uri: "$file(sec get acme@production:MONGODB_URI --out ${out})"
    systems:
      mongo:
        type: mongo
        database: REPLACE_ME              # or in the URI path
        read: { uri: "${secrets.mongo_uri}" }
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod mongo ls` · `tmvp --skill <dir> all mongo count <collection>`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/mongo.md` in the product skill:
- every database and collection per env (from models/schemas AND the live `tmvp --skill <dir> all mongo ls`), one line each: name · purpose · owning code module (`path/to/file`).
- which code modules read/write which collection (grep the names you found).
- env differences (a collection only in prod, different names) and gotchas.
Save read-only queries the on-call engineer will need as `assets/<system>/queries/<name>.json` — derived from real queries/repositories in the code, plus: latest rows, counts by status, recent errors/failures, stuck/pending jobs older than N minutes, rows for one user/order id.
Saved items are find/aggregate filters (JSON) with the same description/params front-matter style as `tmvp llms types/mongo` shows; never `$out`/`$merge` stages.
