# Building a skill on tmvp

A product skill is tiny and generated: `SKILL.md` is rendered from `config.yaml` by
`tmvp new skill` / `tmvp skill add` / `tmvp skill sync` (product, envs, systems, rules, and how to
learn the rest from tmvp). Everything else is discovered live or saved by people as assets.

## Layout
```
.agents/skills/acme-tmvp/
├── SKILL.md         # generated — do not hand-edit (sync rewrites it)
├── config.yaml      # envs → systems; $(…) secret commands; assets: [roots…]
└── assets/<system>/{queries,scripts,pages,apis,manifests,notes}/
```
Assets can come from several repos: `assets: [assets, ../platform/tmvp-assets]`. People keep adding
to them; first match wins; `ls` shows each item's source.

## Saved items — front-matter first, then the body
```sql
/*---
description: paid orders without an invoice (last N days)
params: [days]
---*/
select id, total from orders where status='paid' and invoice_id is null
  and created_at > now() - ($1 || ' days')::interval
```
```markdown
---
method: GET
path: /api/orders/{id}
description: one order with lines
params: [id]
mutating: false
---
Returns 404 for archived orders.
```
ssh scripts declare `read: true` in front-matter; `script` type uses a `# tmvp: read` header.

## Good practice
- Let the agent discover: `tmvp envs`, `tmvp systems`, `tmvp all db ls`, `tmvp prod db describe orders`.
- Save a query/script/api only after it proved useful; give it a precise `description`.
- Keep notes short and factual in `assets/<system>/notes/` (gotchas, env differences).
- Run `tmvp skill lint` before committing (structure, front-matter, no secret values).

## Bad practice
- Pre-writing table lists, schemas or endpoints from guesses — they rot. References must come from the code and be confirmed by live `ls`/`describe`; `ls` stays the source of truth.
- Putting values in config (`password: hunter2`) — use `$(command)`; lint/validate reject literals.
- Copying one product's systems into another skill — one skill = one product.
- Saved items that mutate without saying so (mark `mutating: true`, or omit `read: true`).
