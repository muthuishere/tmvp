---
title: Via chains
description: Reach systems behind bastions, inside containers and pods, with ProxyJump semantics.
sidebar:
  order: 4
---

`via:` names the hops to a system, in order — ssh, portforward or k8s systems in the same env. tmvp opens the chain and dials through it; nothing is exposed on a public port.

```yaml
systems:
  bastion:   { type: ssh, host: REPLACE_ME, user: deploy, key: "${file:secrets.ssh_key}" }
  db-tunnel: { type: portforward, via: bastion, remote: db.internal:5432 }
  db:        { type: postgres, via: db-tunnel, read: { dsn: "${secrets.pg_url}" } }
  cache:     { type: redis, via: [bastion, {container: acme-redis}], url: "${secrets.redis_url}" }
```

- `via: bastion` or a list: `via: [jump1, db-tunnel]`.
- **ssh → docker:** `{container: name}` must follow an ssh hop and come last (`network:`, `sudo: true` optional). Container names may be prefixes (`acme-web` matches `acme-web-<sha>`).
- **ssh → k8s → pod:** a k8s system can itself sit behind ssh; pods are reached through it.
- A service bound to `localhost` on a host (Prometheus, a db in a container) → `via:` that host.

`tmvp systems` shows each system's chain; `tmvp validate` rejects cycles. Full field reference: [config.yaml](/reference/config/).
