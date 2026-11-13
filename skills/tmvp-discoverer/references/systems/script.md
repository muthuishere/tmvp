# script

Local read-only scripts run with secrets in their environment (anything without a connector: a vendor CLI, a one-off checker). Fields and verbs: `tmvp llms types/script` · `tmvp verbs script`.

## Find it in the codebase
- Existing ops scripts: `scripts/`, `bin/`, `ops/`, Taskfile/Makefile targets that only read (status, report, export).

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which existing scripts are safe to run against prod (read-only)?

## Config
```yaml
envs:
  prod:
    secrets:
      vendor_key: "$file(sec get acme@production:VENDOR_API_KEY --out ${out})"
    systems:
      ops:
        type: script
        read: { env: { VENDOR_API_KEY: "${secrets.vendor_key}" } }
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod ops ls`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Copy/wrap read-only scripts into `assets/ops/scripts/<name>.sh` with the `# tmvp: read` header; anything that mutates stays write-only (no header). Describe each in `references/ops.md`.
