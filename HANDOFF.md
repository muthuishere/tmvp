# tmvp — Handoff

2026-11-09 · Status: **v1 built (uncommitted) — see START HERE.** DESIGN.md updated 2026-11-10 with the config model below.

## START HERE (2026-11-10 evening) — v1 built, not committed

**State:** all of DESIGN.md is built. `task build test cross` is green: race checker on, 6 OS/arch targets, ~25k lines of Go, binaries 36–40 MB stripped. Nothing is committed (git repo exists, branch `main`).

**Use it now:**
```
task build                                   # → bin/tmvp
bin/tmvp llms                                # what tmvp is, every topic (llms.txt)
bin/tmvp new skill reqsume --envs dev,prod --systems db:postgres,cluster:k8s,admin:web
#   then fill the $(sec get REPLACE_ME… --print) slots in .agents/skills/reqsume-tmvp/config.yaml
bin/tmvp --skill .agents/skills/reqsume-tmvp validate | envs | systems | check
bin/tmvp --skill … prod db ls                # discover what exists, live
<any command> --llmhelp                      # agent-oriented help, no config needed
```

**Verified with fake secrets only.** Never run against a real system yet. Live tests, one per type, all skipped until a value is given through `sec`:
`sec run TMVP_TEST_PG_DSN -- task test:pg` · `TMVP_TEST_{MYSQL_DSN,MONGO_URL,REDIS_URL,NATS_URL,K8S_KUBECONFIG,WEB_URL}` etc.

**Next (in order):**
1. Owner OK → commit (no Co-authored-by).
2. reqsume as the first real user: `tmvp new skill reqsume …`, fill secrets with `sec get … --print`, run `tmvp check`, then the DESIGN §12 success check.
3. Confirm `sec get NAME --print` works when the tmvp server runs it. If not, switch the secret commands to `sec run`.
4. Live-test k8s (client-go; exec/port-forward behind `via:` use SPDY) and web (playwright-cli 0.1.13).

**Built 2026-11-11:** `kafka` (franz-go, embedded; consume never joins a group), `elasticsearch` (+ `opensearch` alias), `loki`, `grafana` (all stdlib net/http). Embedded drivers stay; external CLIs only for clouds and playwright. Live tests: `TMVP_TEST_{KAFKA_BROKERS,ES_URL,LOKI_URL,GRAFANA_URL}` (+ `_USER/_PASSWORD/_MECHANISM`, `_API_KEY`, `_TOKEN`, `_TENANT`). Topic: `tmvp llms logs-and-metrics`.

**Known gaps:**
- Windows: no peer-SID check on the socket (AF_UNIX has none; isolation is the owner+SYSTEM DACL from `internal/secure`, applied to the run dir, socket and every secret temp dir/file). Windows < 10 1803 (no AF_UNIX) unsupported. Verified on a real Windows 10 box (agentbus node, 2026-11-11): full `go test ./...` green; 6 concurrent `server start` → 1 server; run dir ACL = user + SYSTEM only. Re-run: build a zip with `git archive`, then `agentbus run --node <win> --shell powershell --script ... --file ...`.
- No s3 log sink and no `metrics.prom`.
- The az block list also blocks the harmless `ad app credential list`.

**Decided today (beyond the list below):** no skill-author skill. The generated SKILL.md is tiny and comes straight from the config. Agents learn from `envs`, `systems`, `ls`, `verbs`, `llms` and `--llmhelp`. `assets: [paths]` can span several repos. k8s runs on client-go. Writes run through `tmvp write` on ReadWrite verbs (web clicks, cloud `call`).

## Read in this order
1. `docs/DESIGN.md` — the v1 design (skill model). Source of truth.
2. `docs/ANALYSIS.md` — where we stand, leftover inconsistencies, simplifications, authoring flow, open decisions.
3. `docs/DESIGN-v0-hub.md` — superseded hub design. Only for the web `context.md`/`pages/*.md` front-matter spec and k8s verb list, which v1 still needs to bring back.

## What tmvp is (one paragraph)
A Go CLI that **generates and runs a per-product agent skill** (e.g. `reqsume-tmvp` in the product repo's
`.agents/skills/`). The skill = `SKILL.md` + `config.yaml` (envs × systems; secrets as `$(command)`) +
`assets/<system>/` markdown and saved queries/pages. The agent uses `tmvp` to read postgres/k8s/logs/nats/
kafka/mongo/ssh/web across **all envs at once**; writes go one at a time via `tmvp write … --reason`.
The CLI is self-describing (`envs`, `systems`, `ls`, `verbs`, `llms`, `--llmhelp`); the generated SKILL.md is tiny and rendered from config.yaml.

## Owner decisions (do not re-litigate)
- Trust the agent: read-only default, `tmvp write --reason` mandatory for mutations, no grants/approval gates.
- Destructive statements = config switch per env/system. No tmvp-level batching of writes; native bulk ops (deleteMany, restore) allowed.
- Reads batch across envs by default. Diagnosis is the agent's job — no `diagnose` command.
- Every command audited (reason optional on reads) → JSONL, OTel log shape; sink `file` now, `s3` later; per product.
- **Secrets = `$(any command)`** in config (sec default; op, lpass, gcloud, aws…). Run on the **first command**, held in server memory only. **Re-fetched** when the config changes or a command hits an auth error (then retried once; same value → `secret_invalid`). No access → that slot fails `secret_missing` (exit 2), everything else works. Never print/log values (streaming redactor).
- Config lookup: `./.tmvp/config.yaml` → skill dir → `~/.tmvp/<product>/config.yaml` → `~/.tmvp/config.yaml` (first found wins).
- **Always-on local server**: one binary = client + server, auto-starts, unix socket on every OS (owner-only DACL on Windows), streamed JSONL, exits after 30 min idle.
- Performance, security, every OS (mac/linux/windows, amd64+arm64, `CGO_ENABLED=0`) are functional requirements from day 1.
- playwright CLI assumed installed (like aws); web = `context.md` (login) + `pages/*.md`.
- k8s: multiple clusters, usual verbs, extensible by scripts.
- **Config model (2026-11-10):** top-level `vars` + `secrets` (named `$(cmd)`, lazy), then `envs` → named `systems`, each `type` + fields + `labels` (key: value, `-l` selectors). Env overrides top-level vars/secrets. Interpolation `${vars.x}` / `${secrets.x}` / `${NAME}` (not `{{}}` — sec's syntax). No `tools:`, `files:`, `tmvp run`.
- **From research (adopted 2026-11-10):** `via:` = ProxyJump chains; selectors `prod.*` / `*.db`; secret JSON with `expires_at`; `${X:-default}` after YAML parse; `tmvp validate`, `config show --resolved`; no current env; `tmvp export pg-service|kubeconfig`. See `docs/RESEARCH.md`.
- **File secrets:** `$file(cmd)` (cmd writes `${out}`, read into memory, deleted) and `${file:secrets.x}` (field gets a path; connectors use bytes in memory; external CLIs get memfd on Linux, else 0600 temp for that call only).
- **Types:** ssh, postgres, mysql, mongo, redis, nats, k8s, aws, gcp, az, git, web, http, prometheus, script, portforward, kafka, elasticsearch/opensearch, loki, grafana (built 2026-11-11). Same type any number of times per env. `via:` chains tunnels.
- **Web:** `base_url` + named `users` (role + `$(…)` creds) + `config:` in config.yaml; login/pages front-matter in assets.
- **Manual login:** `login: {manual, check}`; failing check → `login_required` (exit 2) naming the command; user runs it (`! gcloud auth login`), agent retries. Web `manual: browser` → headed login once, session reused.
- **Install once, one skill = one product:** one binary + warm server per machine, keyed by product; a skill can never reach another product.
- **sec:** `sec get NAME --print` (owner-only) — verify it works when the server runs it.
- **Self-describing CLI (2026-11-10):** NO skill-author skill (`skills/`, `tmvp install-skill`, pre-filled reference.md removed — no AI-written guesses). SKILL.md is deterministic from config.yaml (`skill sync`). Discovery: `tmvp envs`, `tmvp systems`, per-connector `ls`, `llms.txt` + `docs/llms/` (embedded; verb tables generated from the registry, test fails on drift), `--llmhelp` everywhere (no config/server needed), `tmvp skill lint`. Assets from multiple repos: config `assets: [paths…]`, first match wins, `ls` shows source. http: `apis` / `call <api>` from `apis/*.md` or `openapi:`.
- **Environments + secret/cert health (2026-11-11):** supersedes "no current env". Env precedence: positional/`-e` > `TMVP_ENV` > `tmvp use` (per product + config path, `TMVP_HOME/state/env.json` 0600, any env, no --force) > `default_env:` > all envs. Output header + every JSONL frame + audit carry `env_source`. **One force rule:** a write to a protected env (`destructive: false`) needs `--force` + `--reason` however the env was chosen (`--preview` exempt); writes stay single-target. `tmvp secrets status|audit` and `tmvp certs` report names/states/dates/cert metadata only (value-change via salted in-memory hash). `use` and `certs` are reserved words.
- MCP: not now. Later hosted over HTTP via the existing **mcpgate** project (not stdio).

## Open decisions (ask the owner)
0. **Now:** more design + research — "boring" config conventions from prior art and a product-only competitor comparison. Don't build until that is done.
1. First release = reduced scope (server + secrets + batch reads + audit + postgres/k8s/loki + postgres write + author skill)?
2. Keep `tools:` / `tmvp run` in v1, or drop?
3. Multi-repo product: which repo holds the skill (first repo vs chosen ops repo)?
4. Accept FR-P performance numbers as targets?
5. Exact `sec` command that prints a value (`sec get NAME`?).
6. Validate secrets by real login at load (slower start) or only non-empty/parse?

## Next steps (when told to build)
1. Fix the DESIGN.md leftovers listed in ANALYSIS §2 (line 12 "No server", env/system name collision, web front-matter, key server by product).
2. `git init`; Go module; build order per DESIGN §12: server skeleton + config + secrets + redactor → postgres read + batch → audit → k8s/loki → `tmvp write` → `tmvp new skill` / `check`.
3. First real user: reqsume.

## Constraints
- Never put a secret value in context, files, logs or output. Use `sec`.
- No `Co-authored-by:` in commits. Commit/push only when asked.
- Competitor comparisons: product features only (closest: Teleport, HolmesGPT, MCP Toolbox).

## Owner decisions 2026-11-11 (do not re-litigate)
- Docker/containers: ssh→docker is mandatory; implemented as container hop (`via: [box, {container: pg}]`) + ssh docker verbs (`docker` read allow-list, `-F container=` on script/run, containers in `ls`).
- **Embed proper Go libraries** (pgx, mysql, mongo, go-redis, nats, client-go, x/crypto/ssh, franz-go, net/http). Use external CLIs only where the vendor CLI is the right tool: aws, gcloud, az, playwright-cli, git. tmvp is a profile switcher: config supplies every file, cert and token; the user logs in once only where that can't be avoided.
- **Types now:** above + kafka, elasticsearch/opensearch, loki, grafana. OpenTelemetry is queried where it lands. No Tempo.
- **Environments:** `default_env` in config → `tmvp use <env>` (no --force) → `TMVP_ENV` → explicit selector/`-e`. Batch reads (`all`, `dev,prod`, globs, labels) are first-class.
- **Writes:** one target, never batched, `--reason` always, and `--force` additionally for production (destructive:false).
- **Cost guard — BUILT 2026-11-11** (`internal/server/costguard.go`, `connector.Estimator`, `tmvp llms cost-and-timeouts`, DESIGN §7): every read is estimated first (EXPLAIN / explain / count / index stats / series / range). If the estimate is over the timeout, it doesn't run: `too_expensive` with the estimate and the hint "narrow, or --timeout N, or --force". Timeout precedence: `--timeout` > system `limits.timeout` > env > global (small default). The timeout is always a hard kill, even with --force. No estimate for kafka/redis/nats (fast; timeout only). **No separate plan command or plan IDs.**
- **Secrets/certs:** `secrets status` (exists, updated via hash, expiry, cert days), `secrets audit`, `certs` (TLS endpoint expiry), all summarised in `doctor`.
- **Postgres read/write model (owner decided 2026-11-12):** reads run read-only; writes only via `tmvp write`. A separate `write.dsn` user is optional — without it writes use the same `dsn`. No warning when the read login can write (the owner's call per system).
- **Skills/llms:** paused. CLI first.

## Done 2026-11-12 (from the fresh-agent Reqsume test)
- **Container prefix match:** `docker logs|inspect|top|stats`, `-F container=` and the `{container:}` hop take a prefix of a deploy-suffixed name (`reqsume-app-web` → `reqsume-app-web-<sha>_uncommitted_<x>`). Exact wins; else the single running `<prefix>-…`/`<prefix>_…` container; several → error listing them. `connectors/ssh/match.go`.
- **`tmvp new skill <p> --from <config.yaml>`** adopts an existing config verbatim (must load and name the product) and makes assets/ folders from its systems.
- **Claude discovery:** `tmvp new skill` links `.claude/skills/<p>-tmvp` → `.agents/skills/<p>-tmvp` (`--no-link` to skip; Windows without Developer Mode warns). Fresh-agent test: not found without the link, found with it.
- **SKILL.md rule 5:** reach the product only through tmvp (no raw ssh/psql/kubectl/curl); container names may be prefixes.
- **Stale server after upgrade:** requests carry the client's Go build ID; a server from another build answers `server_stale`, and the client stops it, waits for its lock and retries once on a fresh server. Found when the fresh agent's calls hit a server started by an older binary.
- **Installer:** `install.sh` (macOS/Linux) and `install.cmd` (Windows) download `tmvp_<os>_<arch>[.exe]` plus `checksums.txt` from GitHub releases, check the sha256, and install to `~/.local/bin` (`TMVP_BIN_DIR`, `TMVP_VERSION`, `TMVP_BASE_URL` overrides). `.github/workflows/release.yml` builds 6 targets on `v*` tags. Both scripts were tested against a local fake release (on macOS and the real Windows node), including the refusal of a tampered binary. Not live until the repo has a GitHub remote and a tag.

## Done 2026-11-13 — discoverer is opt-in and interactive
- `tmvp skill install` needs `--discover`; installers no longer install it (they print the hint). Already-installed copies are still refreshed by any tmvp run.
- tmvp-discoverer: `disable-model-invocation: true` (runs only as /tmvp-discoverer). Flow: scan → ask projects (monorepo grouping = blast radius) → ask envs → deeper scan → confirm system × env matrix → write `<project>-tmvp` skills with references/ (projects, environments, one per system, databases + tables + code users) and saved read-only queries from the code → prove every query → sync/lint.
- references/systems/<type>.md per tmvp type; references/output.md defines the product skill layout.
- Generated SKILL.md now lists `references/*.md` with their descriptions (`TestSyncListsReferences`).
- 2026-11-13 later: discoverer restructured huddle-style — SKILL.md + references/workflow.md + references/steps/step-00..06 (preflight → silent scan → projects → envs → systems & variables → plan approval gate → build). Session state `~/.tmvp/discovery/<repo>/session.md` (resumable, names only).
- New `tmvp env-keys [dir]` (internal/scan): key names + set/empty from every .env*/*.env file, never values (`TestEnvKeysNeverReturnsValues`).
- 2026-11-13: session upgraded from a Fable huddle — references/questions.md (question bank), step-04b brainstorm (questions agents ask → saved queries), confidence markers ✓~?, standing commands, decided/defaulted/skipped, sensitivity + prod budget, tmvp missing = hard stop, ssh via `ssh -G` only.

## Done 2026-11-13 — grpc connector (owner ask)
- connectors/grpc (native, no grpcurl): server reflection or `protoset:`; verbs ls / describe / health / call (+ saved calls `assets/<sys>/calls/*.json`). Reads only for methods matching `read_methods` (default Get*/List*/Search*/Find*/Count*/Describe*/Check/Watch/Health*); client/bidi streaming never a read; others via `tmvp write` (--preview). token → bearer metadata, headers map, tls/ca/server_name/insecure, via: honoured. Server-streaming capped by --limit.
- Tests: in-process server (grpc_testing TestService + health + reflection) incl. protoset-without-reflection; e2e through the binary verified (ls, read call, policy_blocked exit 3, write --preview, check).
- Docs: docs/llms/types/grpc.md, llms.txt; scaffold `skill add x grpc`; discoverer references/systems/grpc.md.
