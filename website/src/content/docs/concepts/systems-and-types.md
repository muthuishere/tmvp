---
title: Systems & types
description: A system is one named thing in one env, of one type. Types decide verbs and safety.
sidebar:
  order: 3
---

A **system** is a named entry under an env in `config.yaml` (`db`, `box`, `cluster`, `api` …) with a `type`. The same name in several envs is what makes `tmvp all db …` work.

Each type is a connector with a fixed verb list, each verb marked `read`, `write`, `destructive` or `read-write`:

```sh
tmvp verbs            # every type
tmvp verbs postgres   # one type
```

| Group | Types |
|---|---|
| Data | [postgres](/systems/postgres/), [mysql](/systems/mysql/), [mongo](/systems/mongo/), [redis](/systems/redis/), [elasticsearch](/systems/elasticsearch/), [opensearch](/systems/opensearch/) |
| Messaging | [kafka](/systems/kafka/), [nats](/systems/nats/) |
| Infrastructure | [ssh](/systems/ssh/) (incl. docker), [k8s](/systems/k8s/), [portforward](/systems/portforward/) |
| Cloud | [aws](/systems/aws/), [gcp](/systems/gcp/), [az](/systems/az/) |
| Observability | [prometheus](/systems/prometheus/), [loki](/systems/loki/), [grafana](/systems/grafana/) |
| Code & APIs | [gh](/systems/gh/), [git](/systems/git/), [http](/systems/http/) (REST + OpenAPI), [grpc](/systems/grpc/), [web](/systems/web/) |
| Escape hatch | [script](/systems/script/) |

Discover what a system holds from the live system, not from guesses: `tmvp <env> <system> ls`, then `describe`.
