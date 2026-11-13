# tmvp — Where we stand (analysis of DESIGN.md)

2026-11-09 · reads with `docs/DESIGN.md` (v1, skill model). Superseded hub draft: `docs/DESIGN-v0-hub.md`.

## 1. Where we stand

**Decided (owner):**
| # | Decision |
|---|---|
| 1 | tmvp = CLI that **generates and runs a per-product skill** (`reqsume-tmvp` in `.agents/skills/`). Not a platform/SaaS. |
| 2 | Trust the agent. Safety = read default + `tmvp write --reason` + everything audited. No gates. |
| 3 | Diagnosis is the agent's job, not a tmvp command. |
| 4 | Reads batch across all envs by default (`--diff`); writes one env/system at a time; native bulk ops allowed. |
| 5 | Destructive = config switch per env/system. |
| 6 | Secrets = `$(any command)` in config; loaded on the first command, held in memory, re-fetched when the config changes or a command hits an auth error (then retried once). No access → that slot errors (`secret_missing`, exit 2); everything else works. |
| 7 | Config holds commands, not values → can live beside the skill; lookup `./.tmvp` → skill dir → `~/.tmvp/<product>` → `~/.tmvp`. |
| 8 | Always-on local server (auto-start, 30 min idle exit), thin client. |
| 9 | Performance, security, every OS (mac/linux/windows, amd64+arm64) are functional requirements from day 1. |
| 10 | `tmvp-skill-author` generator skill analyzes one or many repos (ctx-optimize when present). |
| 11 | Hosted MCP later via **mcpgate**; tmvp only stays gateway-friendly. |
| 12 | Audit every command (reads too) with `--reason`; logs to file or S3, per product. |

**Not built:** nothing. `~/muthu/gitworkspace/tmvp` is a folder with docs, no git, no code.

## 2. Inconsistencies left in DESIGN.md (fix in one pass)
| Line | Problem | Fix |
|---|---|---|
| 12 | "No server" — contradicts §4 local server | "No hosted server / registry; one local per-user server" |
| 346, 348 | `config.example.yaml`, "memory cache process" — both superseded | `config.yaml`; secrets live in the server |
| 116–117 vs 2.0 | `--systems` flag vs generator decides | keep both: generator calls the flag |
| 272 | `tmvp [envs] <system>` — ambiguous if an env and a system share a name | reserve: env names validated not to collide with system names/verbs |
| 274, 168–170 | `tools:` + `tmvp run` — any CLI is unclassifiable → always a write per §7, so `run` needs `--reason` always | state it; or split `run` (read identity) vs `write run` |
| §5 web | v0 had executable `context.md`/`pages/*.md` front-matter (login steps, elements, actions); v1 lost it | bring the front-matter spec back into §2 assets/web |
| §4 | One server for "every product" but config is found per repo/cwd | key everything by `product`; client sends resolved config path |
| P4 | 3 s cold start assumes secret CLIs are fast; `op`/`gcloud` can take 1–2 s each and may prompt | run in parallel; never prompt (fail → warn); measure per command in `tmvp check` |
| S8 | "no swap" isn't achievable everywhere | mark best-effort (already) — accept or drop |

## 3. How to make it simpler

1. **Cut the first release to the loop that proves value.** Server + secrets + batch + read-only
   + audit + **postgres, k8s, loki** + `tmvp write` for postgres + the author skill. Web, nats,
   kafka, mongo, ssh, `run`, s3 sink come after one product (reqsume) uses it daily.
2. **One config file, no layering.** Lookup order picks one file; no merging across the four places.
3. **Drop `tools:` / `tmvp run` from v1.** It overlaps with the agent's own shell; it's the
   least safe and least classifiable part. Revisit when someone needs it.
4. **Two asset kinds only:** `reference.md` (meaning) and saved items (`queries/`, `scripts/`,
   `pages/`). No per-system custom formats beyond front-matter.
5. **No metrics file in v1** — the audit JSONL already has every number; `.prom` is a derived view
   we can add in a day.
6. **Fewer verbs:** per system start with `describe`, `query` (or `get`/`logs`), `health`. Add
   verbs when the author skill or users hit a wall.
7. **Server is an implementation detail.** No `server` subcommands in docs/skill — only
   `tmvp server status` for debugging.

## 4. How authoring should work (`tmvp-skill-author`)

**Goal:** an engineer says "make a tmvp skill for reqsume" and ends with a working skill + one
config to fill — no manual scaffolding.

```
1 locate    → which repos make up the product (cwd, sibling folders, or user-named paths/URLs)
2 analyze   → ctx-optimize per repo if present (query/card), else direct code reading
3 inventory → systems × envs × web apps, each with file:line evidence; ask the user ONCE to confirm
4 generate  → tmvp new skill <product> --systems … --envs …  (CLI writes the scaffold)
             then the agent fills: SKILL.md, config.yaml slots, assets/<system>/reference.md,
             starter queries (from migrations), web pages (from router), subjects/topics (from code)
5 verify    → tmvp check (config + which $(…) resolve) → one read per system across envs
6 handover  → list of config slots still to fill + which secret each needs
```

**Evidence sources by system** (what the author looks for):
| System | Evidence in code |
|---|---|
| postgres/mysql | driver imports, DSN env vars, `migrations/`, ORM models |
| mongo | client imports, collection names |
| nats/kafka | publish/subscribe calls → subjects/topics, stream configs |
| k8s | Helm charts/values per env, kustomize overlays, `kubectl` in Taskfiles/CI |
| logs/otel | logger/exporter config, Loki/OTLP endpoints |
| ssh | deploy scripts, inventory files, hosts in Taskfile/CI |
| web | frontend router (pages), auth/login code, `data-testid`s |
| environments | `.env.*`, values-<env>.yaml, CI deploy jobs, terraform workspaces |

**Update mode:** re-run analysis → diff against the existing skill → propose additions/removals →
apply only additions automatically; removals and changed facts are flagged, never deleted;
hand-written sections (marked `<!-- manual -->`) are never touched.

**Multi-repo:** the inventory merges repos (api + web + infra); envs come mostly from infra repos,
systems from api, pages from web. The skill is written into the repo the user picks (default: the
first repo, or a dedicated ops repo).

**What the author must not do:** invent systems without evidence, put secret values anywhere,
generate folders for systems that don't exist.

## 5. Open decisions
1. First-release scope: the cut in §3.1, or full?
2. Keep `tools:` / `tmvp run` in v1?
3. Where the author writes the skill for multi-repo products (first repo vs a chosen ops repo)?
4. Performance numbers in FR-P — accept as targets?
5. `sec` command that prints a value (`sec get NAME`?).
