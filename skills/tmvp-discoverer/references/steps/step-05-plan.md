# Step 5 — the plan (approval gate)

Write the plan into `{session}` under `## Plan` and show it in full:

```
Project <name>  →  .agents/skills/<name>-tmvp/   (linked: .claude/skills/<name>-tmvp)
  folders: …          users: on-call, support
  envs: dev (default), staging (shares dev DB), prod (protected)
  systems:
    box   ssh       dev prod   alias …, key ~/.ssh/… (path only)
    db    postgres  dev prod   via box · sec <name>@<env>:DATABASE_URL · app login (RO user = owner step)
    gh    gh        all        repo owner/name
  sensitivity: users.email, tokens → excluded; payments → counts only
  prod budget: limit 50, 10s, indexed filters
  references: projects.md, environments.md, db.md, box.md, gh.md
  saved queries: counts-by-status(days), stuck-jobs(minutes), user-by-id(id) …
  owner steps: sec import …, sec set …
  REPLACE_ME left: …
  defaulted (please glance): …
  left out (and why): …
Then: validate → check → prove each system → run every saved query in every env → sync/lint → report.
```

Ask: "{USER}, approve this plan, or change something?" Stop. Only explicit approval moves to step 6; a change reopens its step (`back <step>`), redoing only what depends on it, and shows the diff.
