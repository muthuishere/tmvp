# Step 4 — systems and variables (per project; later projects start "same as <first> except…?")

Rounds in order, one topic each, ≤ 3 questions (rows in questions.md step 4). Skip a round whose findings are all ✓.

- **A matrix** — systems (name · type) × envs: `✓/~/? evidence` or `missing`, and how it is reached if known.
- **B reach & login** — via which host/bastion/VPN, ssh alias and key *path*, read-only DB user or app login. Never dial anything to find out.
- **C variables & gaps** — every variable: envs/files defining it (set/empty) · in sec as `<project>@<env>:NAME`? · config slot. Triage unmapped names into CI-only / app config / unknown; ask only about unknown. Gaps → owner command (`sec import <file> --project <p> --env <e>` or `sec set …`).
- **D sensitivity & budget** — PII/secret-looking tables (from migrations) → excluded or counts-only; prod limits.
- **E SaaS & observability** — which third-party APIs matter; where they look first when prod breaks.

Record systems, reach, variable map, `## Sensitivity` in `{session}`. Then step 4b.
