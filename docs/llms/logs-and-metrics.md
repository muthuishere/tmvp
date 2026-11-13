# logs and metrics — where to query

OpenTelemetry has no query endpoint of its own: the collector *exports* to a backend, and you query
the backend. Find where your logs/metrics/traces land (the collector's `exporters:`), then use that type.

| Where it lands | tmvp type | Verb |
|---|---|---|
| Loki (logs) | `loki` | `range '{app="api"} \|= "error"' --since 30m`, `tail`, `query` (metric LogQL) |
| Prometheus / Mimir / Thanos / VictoriaMetrics (metrics) | `prometheus` | `query`, `range`, `alerts` |
| Elasticsearch / OpenSearch (logs, APM) | `elasticsearch` / `opensearch` | `search <index> '<json>'`, `count` |
| Anything Grafana can see (incl. Tempo-less OTel backends) | `grafana` | `query <datasource-uid> '<expr>'`, `dashboard <uid>` to copy a panel's query |
| GCP Cloud Logging | `gcp` | `logs` |
| AWS CloudWatch Logs | `aws` | `logs` |
| Azure Monitor / Log Analytics | `az` | `logs` |
| Pod stdout (no backend) | `k8s` | `logs` |
| Plain files on a VM | `ssh` | `@script` (e.g. `journalctl`/`tail` in a saved script) |

Tips:
- Always narrow first: `loki values app`, `prometheus ls http_`, `elasticsearch mapping <index>`, `grafana ls`.
- Every read is bounded (`--since`, `--limit`, byte cap); ask for less and repeat rather than one huge pull.
- Multi-env: `tmvp all logs range '{app="api"} |= "panic"' --since 1h` runs per env in parallel.
