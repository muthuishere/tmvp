// Scenario 1: orders stuck after payment — stuck orders, rejected webhooks and payfast config per env, plus which env is odd
// Read-only. input (optional): { env: "all" }. Credentials stay in config.yaml secret references.
const env = (input && input.env) || "all"
const [stuck, hooks, cfg] = await Promise.all([
  ops.db.query({ env, args: ["select count(*)::int as stuck, min(created_at)::text as oldest from orders where status = 'pending_payment'"] }),
  ops.db.query({ env, args: ["select status_code, error, count(*)::int as n from webhook_events where status_code <> 200 group by 1, 2 order by n desc"] }),
  ops.db.query({ env, args: ["select key, value from app_config where key like 'payfast.%' order by key"] }),
])
const envs = {}
const at = e => (envs[e] = envs[e] || { stuck: 0, rejected_webhooks: [], payfast: {} })
for (const r of stuck.rows) { at(r.env).stuck = r.stuck; at(r.env).oldest = r.oldest }
for (const r of hooks.rows) at(r.env).rejected_webhooks.push({ status: r.status_code, error: r.error, n: r.n })
for (const r of cfg.rows) at(r.env).payfast[r.key] = r.value
// a config key is "odd" in an env when its value differs from every other env
const odd = []
const keys = Object.keys(Object.fromEntries(cfg.rows.map(r => [r.key, 1])))
for (const k of keys) {
  const vals = Object.keys(envs).map(e => [e, envs[e].payfast[k]])
  for (const [e, v] of vals) {
    if (vals.filter(([, w]) => w === v).length === 1 && vals.length > 2) odd.push({ env: e, key: k, value: v, others: vals.filter(([f]) => f !== e).map(([, w]) => w)[0] })
  }
}
export default {
  affected: Object.entries(envs).filter(([, v]) => v.stuck > 0).map(([e, v]) => ({ env: e, stuck: v.stuck, oldest: v.oldest, rejected_webhooks: v.rejected_webhooks })),
  odd_config: odd,
  clean_envs: Object.entries(envs).filter(([, v]) => v.stuck === 0).map(([e]) => e),
  errors: [...stuck.errors, ...hooks.errors, ...cfg.errors],
}
