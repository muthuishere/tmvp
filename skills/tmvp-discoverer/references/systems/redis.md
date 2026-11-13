# redis

Redis / Valkey / KeyDB (also ElastiCache, Upstash, Memorystore) — caches, queues, sessions, rate limits. Fields and verbs: `tmvp llms types/redis` · `tmvp verbs redis`.

## Find it in the codebase
- Clients: `grep -rlE 'ioredis|"redis"|redis-py|import redis|aioredis|go-redis|redis/go-redis|lettuce|jedis|StackExchange.Redis|bullmq|\bbull\b|sidekiq|rq\b|celery.*redis|resque'`
- Key prefixes: grep string literals passed to `get/set/hset/lpush/xadd` and queue names (`new Queue('…')`) — they become the prefix list.
- Infra: compose `redis:*`, `valkey/*`; Kamal `accessories: redis`; helm `bitnami/redis`; terraform `aws_elasticache_*`.
- Env var NAMES: `REDIS_URL`, `REDIS_HOST`, `REDIS_PASSWORD`, `CACHE_URL`, `QUEUE_URL`.

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
      redis_url: "$file(sec get acme@production:REDIS_URL --out ${out})"
    systems:
      cache:
        type: redis
        via: box                          # private URL (10.x / 172.x / internal DNS) → via the app host
        url: "${secrets.redis_url}"
      # in a container: via: [box, {container: acme-redis}]
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod cache ls` (works under restricted ACLs; `info` may be NOPERM)

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/cache.md`: key prefixes per env from `ls` (count + example key shape, never values) mapped to the code that writes them, queue names and which worker consumes them, expected TTLs. Note which prefixes are safe to inspect with `get` (no personal data).
No saved queries; list useful commands instead: `scan 'queue:*'`, `ttl <key>`, `type <key>`.
