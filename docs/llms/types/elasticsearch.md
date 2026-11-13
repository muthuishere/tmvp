# elasticsearch

Elasticsearch and OpenSearch (type `elasticsearch` or its alias `opensearch`; same verbs). Plain HTTP, no SDK. A read connection only sends GET, plus POST to `_search`, `_count`, `_msearch`, `_mapping`, `_field_caps`, `_validate/query`.

## Fields
url (or base_url), one of api_key (`Authorization: ApiKey …`) | token (bearer) | user/password, headers, ca (`${file:secrets.ca}`), insecure; per mode. 401 → `auth_error`; 403 → `policy_blocked` (the credential lacks the privilege).

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls [substring] — indices (health, docs), aliases, saved queries/*.json |
| `health` | read | health — cluster health (status, nodes, shards) |
| `indices` | read | indices [pattern] — _cat/indices (health, status, docs, size) |
| `mapping` | read | mapping <index> — one row per field: index, field (dotted), type |
| `search` | read, @item | search <index> '<json>'\|@saved [-F q=<lucene>] — hits as rows; size ≤ --limit, server timeout; @saved = queries/<name>.json (front-matter index:) |
| `count` | read | count <index> ['<json>'\|@saved] [-F q=<lucene>] |
| `cat` | read | cat <endpoint> [pattern] — allow-listed _cat endpoints: health indices aliases nodes shards allocation count segments thread_pool pending_tasks recovery templates master plugins fielddata tasks |
| `get` | read | get <index> <id> — one document |
| `index` | write, default | index <index> '<json>'\|@saved [-F id=<id>] — create/replace a document |
| `update` | write | update <index> <id> '<partial json>' — partial update (body without "doc"/"script" is wrapped in {"doc": …}) |
| `delete` | write | delete <index> <id> — delete one document |
| `update-by-query` | write | update-by-query <index> '<json with query + script>' (--preview = count; no query = every doc → needs destructive) |
| `delete-index` | destructive | delete-index <index> — no wildcards, no _all |
| `delete-by-query` | destructive | delete-by-query <index> '<json with query>' (--preview = count) |
| `reindex` | destructive | reindex '<json {"source":…,"dest":…}>' |
<!-- END verbs -->

## Discovery
`ls [substring]` = indices (non-dot), aliases and saved queries `assets/<system>/queries/<name>.json` (front-matter `index:`; `tmvp prod search @errors` runs `search`). `search` sets `size` ≤ `--limit` and a server-side `timeout`; responses are capped by `limits.bytes`.

Destructive detection: `update-by-query` without a query (or `match_all`) needs `destructive: true`; `delete-index` refuses wildcards and `_all`.

## Examples
```
tmvp prod search ls
tmvp prod search mapping logs-2026.11
tmvp all search search 'logs-*' '{"query":{"term":{"http.status":500}}}' --limit 20
tmvp prod search count orders -F q='status:failed'
tmvp prod search cat shards
tmvp write prod search index orders '{"id":1,"status":"ok"}' -F id=1 --reason "fix doc"
tmvp write prod search delete-by-query orders '{"query":{"term":{"test":true}}}' --preview --reason "drop test docs"
```
