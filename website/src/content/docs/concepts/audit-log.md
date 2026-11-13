---
title: Audit log
description: Every command, one JSONL line in OpenTelemetry log shape, per product.
sidebar:
  order: 9
---

- File: `~/.tmvp/<product>/logs/events.jsonl` (override with `logging.path` in config).
- One line per command; for writes an **intent** record before execution and an **outcome** record after.
- Carries env, system, verb, `env_source`, the cost estimate, `--force`, the reason and the calling agent (`TMVP_AGENT`). Every line passes the redactor: no secret values.

```sh
tmvp logs --since 1d          # recent events
tmvp logs --writes            # write intent/outcome only
tmvp secrets audit --since 1d # secret fetch / refresh / invalidate events (names only)
```
