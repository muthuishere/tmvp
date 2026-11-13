# gcp

Google Cloud through gcloud: Cloud Logging, GCS, Cloud Run, and other read calls. Fields and verbs: `tmvp llms types/gcp` · `tmvp verbs gcp`.

## Find it in the codebase
- `grep -rlE '@google-cloud/|google-cloud-|cloud.google.com/go|firebase-admin'`; `app.yaml`, `cloudbuild.yaml`, `service.yaml` (Cloud Run); terraform `provider "google"`; env NAMES `GOOGLE_CLOUD_PROJECT`, `GOOGLE_APPLICATION_CREDENTIALS`.
- CI steps: `aws-actions/configure-aws-credentials`, `google-github-actions/auth`, `azure/login` show which account each env uses.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which project per env? User login or a service-account JSON in sec (`sec file ls`)?
- Any read-only role/profile for prod?

## Config
```yaml
envs:
  prod:
    destructive: false
    systems:
      cloud:
        type: gcp
        project: acme-prod
        region: europe-west1
        # credentials: "${file:secrets.gcp_sa}"
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod cloud whoami`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/cloud.md`: buckets/log groups/services per env with what code uses them. Save read-only calls as `assets/<system>/scripts/<name>.sh` (`read: true`) — e.g. list recent objects under a prefix, recent log errors. tmvp's `call` only passes read subcommands; mutations go through `tmvp write`.
