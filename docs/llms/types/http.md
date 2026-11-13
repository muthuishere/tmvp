# http

Plain HTTP APIs. Paths resolve against base_url (absolute URLs refused). Named APIs come from `apis/*.md` front-matter {method, path, description, params, mutating} and/or an `openapi:` file/path/URL.

## Fields
base_url, token (secret bearer) or user/password, headers, health_path, insecure, openapi (file relative to the skill, path on base_url, or URL)

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls — defined APIs (assets apis/*.md + openapi:): name, method, path, mutating, source; offline unless openapi is a URL |
| `apis` | read | apis [filter] — same as ls, filtered by name/path substring |
| `call` | read-write, @item | call <api\|@api> [k=v ...] — run a defined API; {k} fills path params; mutating APIs only via tmvp write (--preview) |
| `get` | read | get <path> [-F q.key=value] [-F h.Header=value] — JSON → rows, else text |
| `health` | read | health [path] — status, latency (default health_path or /health) |
| `post` | write, default | post <path> [body] (tmvp write) |
| `put` | write | put <path> [body] (tmvp write) |
| `patch` | write | patch <path> [body] (tmvp write) |
| `delete` | write | delete <path> (tmvp write) |
<!-- END verbs -->

## Discovery
`ls` / `apis [filter]` = defined APIs (offline unless openapi is a URL); `call <api>` runs GET APIs as reads, mutating ones only via tmvp write.

## Examples
```
tmvp prod api ls
tmvp all api call @get-order id=42
tmvp prod api get /v1/status -F q.verbose=1
tmvp write prod api call refund-order id=42 --reason "support #812" --preview
```
