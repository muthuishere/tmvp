# tmvp — agent rules
Read `docs/DESIGN.md` (truth), `HANDOFF.md`, `PLAN.md`. Go 1.26, `CGO_ENABLED=0`, `task build test cross`.
- Never put a secret value in code, tests, logs or output. Tests use fake values like `FAKE_SECRET_123`. Live creds only via `sec run NAME -- …`.
- Stay in the packages you own (PLAN.md). Subcommands live in their own `cmd/tmvp/cmd_<name>.go` and self-register.
- No `Co-authored-by:` in commits. Don't commit unless told.
