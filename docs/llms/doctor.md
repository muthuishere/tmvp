# tmvp doctor

One screen answering: what is installed, what can I connect to, what is broken. Works without a
config (machine, tools and types only). Runs in-process — it never starts the server — and reports
secret **names and states only**, never values or the commands that produce them.

```
tmvp doctor                                   # everything; config by lookup order if one exists
tmvp doctor --offline                         # no secret commands, login checks or connections
tmvp --skill .agents/skills/acme-tmvp doctor --env prod --system db
tmvp doctor --json                            # JSONL rows, each with "section"
```

Sections:

| Section | Shows |
|---|---|
| machine | tmvp version, OS/arch, `TMVP_HOME`, socket path + length check, server (pid, uptime, product names) |
| tools | kubectl (optional), aws, gcloud, az, git, playwright-cli, and secret sources (sec, op, lpass, vault): found?, `--version` first line, which types need it; binaries the config's `$(…)` commands run are flagged when missing |
| types | every registered type with read / write verb counts and whether it is built-in |
| config / systems | per env × system: type known, secrets (ok / missing / invalid / unknown), login (ok / required + the command), via chain, connect (ok / failed + code) |
| summary | problem count and the top 3 fixes with exact commands |

Exit codes: `0` all good · `1` something failing (missing tool the config needs, login required,
secret or connection failed) · `2` config problem (no/invalid config given, validation errors,
unknown type).

Fixes it suggests: `install playwright-cli: npm i -g @playwright/cli`, `install aws`,
`run: gcloud auth login` (the system's `login.manual`), `set TMVP_SOCKET …`, `tmvp validate`.
Use `tmvp check` for the server-side view (warm pools, cached secrets).
