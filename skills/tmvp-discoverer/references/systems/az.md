# az

Azure through the az CLI: Log Analytics, storage, App Service and other read calls. Fields and verbs: `tmvp llms types/az` · `tmvp verbs az`.

## Find it in the codebase
- `grep -rlE '@azure/|azure-|Azure.'`; `azure-pipelines.yml`, bicep `*.bicep`, terraform `provider "azurerm"`; env NAMES `AZURE_*`.
- CI steps: `aws-actions/configure-aws-credentials`, `google-github-actions/auth`, `azure/login` show which account each env uses.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which subscription (and Log Analytics workspace) per env?
- Any read-only role/profile for prod?

## Config
```yaml
envs:
  prod:
    destructive: false
    systems:
      cloud:
        type: az
        subscription: REPLACE_ME
        # workspace: REPLACE_ME        # for logs
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod cloud whoami`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/cloud.md`: buckets/log groups/services per env with what code uses them. Save read-only calls as `assets/<system>/scripts/<name>.sh` (`read: true`) — e.g. list recent objects under a prefix, recent log errors. tmvp's `call` only passes read subcommands; mutations go through `tmvp write`.
