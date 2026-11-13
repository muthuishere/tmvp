# aws

AWS through the aws CLI: S3, SES, CloudWatch logs, and other read calls. Fields and verbs: `tmvp llms types/aws` · `tmvp verbs aws`.

## Find it in the codebase
- `grep -rlE '@aws-sdk|aws-sdk|boto3|botocore|aws-sdk-go|software.amazon.awssdk'`; terraform `provider "aws"`, `aws_s3_bucket`, `aws_ses_*`; S3-compatible endpoints (R2, MinIO, Wasabi): `S3_ENDPOINT`, `AWS_ENDPOINT_URL`; env NAMES `AWS_REGION`, `AWS_PROFILE`, `S3_BUCKET`, `SES_*`.
- CI steps: `aws-actions/configure-aws-credentials`, `google-github-actions/auth`, `azure/login` show which account each env uses.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which profile/region per env? Is S3 really AWS, or R2/MinIO (then endpoint)?
- Any read-only role/profile for prod?

## Config
```yaml
envs:
  prod:
    destructive: false
    systems:
      files:
        type: aws
        region: eu-central-1
        profile: acme-prod                # or access_key/secret_key from sec:
        # access_key: "${secrets.aws_key_id}"
        # secret_key: "${secrets.aws_secret}"
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod files whoami`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/files.md`: buckets/log groups/services per env with what code uses them. Save read-only calls as `assets/<system>/scripts/<name>.sh` (`read: true`) — e.g. list recent objects under a prefix, recent log errors. tmvp's `call` only passes read subcommands; mutations go through `tmvp write`.
