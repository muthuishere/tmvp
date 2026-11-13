# aws

AWS through the `aws` CLI with a per-call environment (your ~/.aws is never modified).

## Fields
region, profile, access_key/secret_key/session_token (secrets), credentials/config (`${file:secrets.x}`); login: {manual: "aws sso login --profile p", check: "aws sts get-caller-identity --profile p"}

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls — configured scope (profile, region), whoami, saved scripts |
| `whoami` | read | sts get-caller-identity (account, arn) |
| `logs` | read | logs <log-group> [filter-pattern] — CloudWatch, bounded by --since (≤7d) / --limit |
| `call` | read-write | call <service> <op> [args] — read-only ops; through tmvp write any op (--preview shows it) |
| `cli` | write, default | alias of `call` for tmvp write (kept for compatibility) |
<!-- END verbs -->

## Discovery
`ls` = configured scope (profile, region) + saved scripts + whoami.

## Examples
```
tmvp all aws ls
tmvp prod aws logs /ecs/api "ERROR" --since 1h
tmvp prod aws call ec2 describe-instances
tmvp write prod aws call ec2 start-instances --instance-ids i-0abc --reason "restore box"
```
