---
title: tmvp-discoverer
description: An opt-in agent skill that scans your repo, asks you in rounds, agrees a plan, then writes a tmvp skill per project.
sidebar:
  order: 1
---

```sh
tmvp skill install --discover    # into ~/.claude/skills, ~/.agents/skills (and $CLAUDE_CONFIG_DIR/skills)
```

Then, in your product repo, ask your agent: **`/tmvp-discoverer`**. It never starts on its own (`disable-model-invocation`), and it writes nothing into the repo until you approve its plan. Later tmvp runs keep the installed copy current.

## The session

| Step | What happens | Ends with |
|---|---|---|
| 0 Preflight | tools present; resume an earlier session | a question, or the scan |
| 1 Scan | units, env names, variables (`tmvp env-keys`, names only), systems, tables and real queries — each with file:line | findings |
| 2 Projects | monorepo grouping: which folders form one project (its blast radius) | your grouping |
| 3 Environments | names, which are prod-like, default env, shared stores | your env list |
| 4 Systems & variables | system × env matrix, reach (`via`), variable → secret map, sensitive tables, prod budget, SaaS, observability | a settled matrix |
| 4b Questions agents ask | brainstorm saved queries from real needs | the saved-query list |
| 5 Plan | everything above, plus owner steps and leftovers | **your approval** |
| 6 Build | write `.agents/skills/<project>-tmvp/`, prove each system and every saved query, sync, lint | a report |

Every finding is marked `✓` (2+ sources), `~` (one) or `?` (inferred); only `~` and `?` are asked. At most three questions per round. Progress lives in `~/.tmvp/discovery/<repo>/session.md` — names only, resumable.

**Standing commands:** `defaults` (accept this round's defaults — never for the prod list, sensitive tables or the plan) · `why` · `skip` · `back <step>` · `map` · `pause`.

Full text: [steps](/agent-skills/discoverer-steps/) · [question bank](/agent-skills/discoverer-questions/).
