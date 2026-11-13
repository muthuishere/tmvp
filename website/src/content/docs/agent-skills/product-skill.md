---
title: Your <product>-tmvp skill
description: What lives in .agents/skills/<project>-tmvp and how agents use it.
sidebar:
  order: 2
---

```
.agents/skills/<project>-tmvp/
  config.yaml                 # envs + systems; secrets as commands only
  SKILL.md                    # generated: tmvp --skill <dir> skill sync (lists references/)
  references/
    projects.md               # folders, other projects, blast radius
    environments.md           # envs, prod-like, shared stores
    <system>.md               # one per system: tables, who uses them, saved items
  assets/<system>/
    queries/<name>.sql        # saved read-only queries (front-matter: description, params)
    scripts/<name>.sh         # ssh read scripts (read: true)
    apis/<name>.md            # saved http calls
    calls/<name>.json         # saved grpc calls
    notes/<topic>.md
```

`.claude/skills/<project>-tmvp` links to it so Claude Code finds it; Codex and others read `.agents/skills`.

The generated SKILL.md tells agents: call tmvp as `tmvp --skill <dir> …`; read in batches with `all`; write only via `tmvp write … --reason`, one env, when the user asked; on `login_required` show the user the command; never handle secret values; never reach the product's hosts with raw ssh/psql/kubectl/curl.

```sh
tmvp --skill .agents/skills/acme-tmvp skill sync    # regenerate SKILL.md (hand-written ones are left alone)
tmvp --skill .agents/skills/acme-tmvp skill lint    # structure, front-matter, no secret values
tmvp prod db @unpaid-orders 7                       # run a saved query
```
