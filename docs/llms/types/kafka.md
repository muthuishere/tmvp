# kafka

Apache Kafka (also Redpanda, MSK, Confluent, Event Hubs' Kafka endpoint). Embedded franz-go — no CLI. Every broker connection goes through `via:`. `consume` reads partitions directly: it never joins a consumer group and never commits.

## Fields
brokers (`"b1:9092,b2:9092"`, or a YAML list of literal host:port), sasl `{mechanism: plain|scram-sha-256|scram-sha-512, user, password: ${secrets.x}}`, tls (`true`, or `{ca: ${file:secrets.ca}, cert: ${file:…}, key: ${file:…}, server_name, insecure}`); per mode (`read.sasl`, `write.sasl`). A rejected SASL credential → `auth_error` (secrets re-fetched, retried once).

```yaml
bus:
  type: kafka
  brokers: "${vars.kafka}"
  tls: true
  read:  {sasl: {mechanism: scram-sha-512, user: reader, password: "${secrets.kafka_ro}"}}
  write: {sasl: {mechanism: scram-sha-512, user: ops,    password: "${secrets.kafka_rw}"}}
```

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls [substring] — topics (partitions) + saved messages/*.json |
| `topics` | read | topics [substring] — topics with partition count, replication factor |
| `describe` | read | describe <topic> — per-partition leader/replicas/isr/start/end offsets, then non-default topic configs |
| `groups` | read | groups — consumer groups (state, protocol type) |
| `lag` | read | lag <group> — per topic/partition committed offset, end offset, lag, member |
| `consume` | read | consume <topic> [-F from=earliest\|latest-N\|<offset>\|<RFC3339>] [-F partition=N] [-F wait=10s] — bounded by --limit and time; never joins a group or commits (default from=latest-10 per partition) |
| `health` | read | health — brokers, controller, cluster id |
| `produce` | write, default | produce <topic> <value\|@saved> [-F key=k] [-F partition=N] (--preview shows the record) |
| `create-topic` | write | create-topic <topic> [-F partitions=1] [-F replication=-1] [-F config.<k>=<v>] |
| `delete-topic` | destructive | delete-topic <topic> |
| `delete-records` | destructive | delete-records <topic> -F before=<offset>\|latest [-F partition=N] — truncate partitions before an offset |
| `reset-offsets` | destructive | reset-offsets <group> <topic> -F to=earliest\|latest\|<RFC3339> — group must be empty (--preview shows old→new) |
<!-- END verbs -->

## Discovery
`ls [substring]` = topics + saved messages (`assets/<system>/messages/<name>.json`, used by `produce <topic> @name`).

## Examples
```
tmvp all bus ls orders
tmvp prod bus describe orders
tmvp prod bus lag billing-worker
tmvp prod bus consume orders -F from=latest-20 --limit 50
tmvp prod bus consume orders -F from=2026-11-11T09:00:00Z -F partition=3
tmvp write prod bus produce orders '{"id":1}' -F key=1 --reason "replay order 1"
tmvp write prod bus reset-offsets billing-worker orders -F to=2026-11-11T09:00:00Z --preview --reason "reprocess"
```
