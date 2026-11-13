# Step 1 — scan (no questions)

Read `references/evidence.md`. Collect, with file:line evidence:

1. **Units** — every buildable/deployable folder: manifests (go.mod, package.json workspaces, pyproject, pom/gradle modules, Cargo members, Dockerfile*), and what deploys them (Kamal `config/deploy*.yml`, compose, helm/k8s, CI deploy jobs). Note which units share a data store.
2. **Environment names** — CI workflow names/`environment:` keys, Kamal destinations, k8s namespaces/contexts (names), terraform workspaces, `sec project ls`, and env-file names from `tmvp env-keys` (e.g. `.env.production`, `application-dev.env` → `dev`).
3. **Variables** — `tmvp env-keys {repo}` (names only) and, per env, `sec env ls <env>` / `sec ls --json`. Also names read in code (`os.Getenv`, `process.env.`, `os.environ`, `@Value("${…}")`, compose `environment:`). Build: variable → which env files define it (set/empty) → which code reads it → which system it implies (`DATABASE_URL`/`POSTGRES_*` → postgres, `REDIS_*` → redis, `S3_*`/`AWS_*` → aws, `*_API_KEY` of a SaaS → http …).
4. **Systems** — from deploy files, infra folders, drivers/clients in code and the variables above. Type hints: `references/systems/README.md` and `references/systems/<type>.md`.
5. **Data** — for each data store: migrations/models → tables; repositories/DAOs → the real queries (evidence.md "queries people actually run").

Write all of it under `## Findings` in `{session}`. Then go to step 2.
