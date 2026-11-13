# Config patterns (copy, then replace names)

Secret slots run a command; tmvp's server runs it and keeps the value in memory:
- from sec (preferred): `"$file(sec get <product>@<env>:NAME --out ${out})"` — sec writes a 0600 file tmvp reads; nothing on stdout.
- a key file already on disk: `"$(cat ~/.ssh/<keyfile>)"` (the path is fine to write; never open the file yourself).
- other managers: `"$(op read op://Vault/item/field)"`, `"$(gcloud secrets versions access latest --secret=NAME)"`.

```yaml
version: 1
product: acme
default_env: dev
secrets:                                   # shared by all envs
  known_hosts: "$(cat ~/.ssh/known_hosts)"
  prod_key: "$(cat ~/.ssh/acme-prod)"
envs:
  prod:
    destructive: false                     # always for prod-like envs
    secrets:
      pg_url: "$file(sec get acme@production:DATABASE_URL --out ${out})"
      redis_url: "$file(sec get acme@production:REDIS_URL --out ${out})"
    systems:
      box:     { type: ssh, host: REPLACE_ME, user: deploy, key: "${secrets.prod_key}", known_hosts: "${secrets.known_hosts}" }
      db:      { type: postgres, via: box, read: { dsn: "${secrets.pg_url}" } }
      cache:   { type: redis, via: [box, {container: acme-redis}], url: "${secrets.redis_url}" }
      gh:      { type: gh, repo: owner/acme }            # uses `gh auth login`; or token: "${secrets.gh_token}"
      metrics: { type: prometheus, via: box, url: "http://127.0.0.1:9090" }
      logs_q:  { type: loki, via: box, url: "http://127.0.0.1:3100" }
      files:   { type: aws, region: eu-central-1, profile: acme-prod }   # S3/SES/CloudWatch via aws CLI
      pay:     { type: http, base_url: "https://api.razorpay.com", user: "${secrets.rzp_id}", password: "${secrets.rzp_secret}" }
```

| type | prove it (one cheap read) |
|---|---|
| ssh | `<env> box health` · `<env> box docker ps` |
| postgres / mysql | `<env> db query "select 1"` |
| redis | `<env> cache ls` (works under restricted ACLs; `info server` may be NOPERM) |
| gh | `<env> gh runs --limit 3` |
| prometheus | `<env> metrics query "count(up)"` |
| loki | `<env> logs_q labels` |
| k8s | `<env> cluster health` |
| aws | `<env> files whoami` |
| http | `<env> pay health` or a saved read API `@name` |

A URL from sec that times out when dialed directly usually points at a private/VPN address: add `via: <ssh host>` (e.g. `cache: { type: redis, via: box, url: … }`). In step 6 only (after plan approval): try direct first, then via the app host.

Names like `logs`, `write`, `all`, `check` collide with tmvp commands; `tmvp validate` lists reserved names.
Saved read-only API calls for SaaS live in `assets/<system>/apis/<name>.md` (see `tmvp llms types/http`).
