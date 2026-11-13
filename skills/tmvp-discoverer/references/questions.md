# Question bank

Ask only what evidence leaves `~` or `?`. Always show the evidence (file:line, key names) and offer the default. Third column = what the answer fills.

## Step 2 — projects
| question | show | default | fills |
|---|---|---|---|
| How many projects, which folders each? | units table + shared stores | grouping by shared data store | projects, skill dirs |
| `api` and `worker` both use `DATABASE_URL` — one project? | the two code refs | yes | projects |
| Who uses this skill, for what (on-call triage, support lookups, release checks, data questions)? | — | on-call + support | saved queries, references focus |
| `libs/shared` is used by both — list under both or neither? | import refs | neither; mentioned in projects.md | projects.md |
| Anything deployed from another repo that these folders touch? | — | no | projects.md |

## Step 3 — environments (two rounds)
Round A
| question | show | default | fills |
|---|---|---|---|
| How many envs, what names? | names + evidence, merge suggestions | merged list | `envs:` |
| Which are production-like? | — | names with prod/production/live | `destructive: false` |
| Default env for agents? | — | first non-prod | `default_env` |

Round B
| question | show | default | fills |
|---|---|---|---|
| Does staging have its own DB/redis, or share dev's (or prod's)? | same key names in both env files | separate | environments.md; a shared store gets the stricter env's protection |
| Include `local`? | compose file | no | `envs:` |
| Several deploy targets per env (prod1/prod2, eu/us, tenants)? | deploy files | one env per target | `envs:` / systems |
| What do you call it — `prod` or `production`? sec scope may differ | sec scopes | tmvp `prod`, sec scope as found | secret commands |

## Step 4 — systems & variables (rounds A–E)
| round | question | default | fills |
|---|---|---|---|
| A matrix | Anything missing or wrong in this system × env matrix? Do the names (`box`, `db`, `cache`) match your team's words? | as shown | systems |
| B reach | Prod DB reached directly, via bastion/app host, or only with VPN up? Which ssh alias/key path per env? | `via` the app host | `via:`, ssh fields |
| B login | No read-only DB user in sec — use the app login for reads, or create an RO user first? | app login now; RO user as owner step | `read.dsn` |
| C variables | Unmapped names in 3 buckets: CI-only · app config/flags · unknown. What are the unknown ones? | ignore first two | secrets map |
| C gaps | `REDIS_URL` in `.env.production` but not in sec — import it (command shown) or leave a blocker? | owner step `sec import …` | owner steps |
| D sensitivity | These tables look like PII/secrets (`users.email`, `tokens`, `payments`) — exclude from saved queries, or counts only? | counts/ids only, never `select *` | sensitivity, saved queries |
| D budget | Prod reads: replica or primary? row limit, timeout? | primary, `limit 50`, indexed filters, 10s | `limits:`, queries |
| E SaaS | Found keys for Razorpay, SES, OpenAI… — which matter to agents? | payments + email in, LLM out | http systems, apis |
| E observability | Where do you look first when prod breaks (Grafana, Loki, docker logs, Sentry)? | what infra/ shows | obs systems |

## Step 4b — questions agents will ask
| question | default |
|---|---|
| Which 3 questions did you last answer by hand on prod? | — |
| Pick/rename from these candidates (from code) | top 5 candidates |
