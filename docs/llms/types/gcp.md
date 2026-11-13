# gcp

Google Cloud through `gcloud`; properties per call via env (never `gcloud config set`).

## Fields
project, region, zone, account, credentials (`${file:secrets.x}` SA JSON); login: {manual: "gcloud auth login", check: "gcloud auth print-access-token"}

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls — configured scope (project, region, zone, account), whoami, saved scripts |
| `whoami` | read | active account and project |
| `logs` | read | logs ["<logging filter>"] — Cloud Logging, newest first, bounded by --since (≤7d) / --limit |
| `call` | read-write | call <gcloud args> — read verbs only (list/describe/read ...); through tmvp write any call (--preview shows it) |
| `cli` | write, default | alias of `call` for tmvp write (kept for compatibility) |
<!-- END verbs -->

## Discovery
`ls` = configured scope (project, region, zone, account) + saved scripts + whoami.

## Examples
```
tmvp all cloud ls
tmvp prod cloud logs "severity>=ERROR" --since 1h
tmvp prod cloud call compute instances list
tmvp write prod cloud call run services update api --region=x --reason "…" --preview
```
