// Scenario 3: emails silently fail in one env — retrying emails by region/error, config values with stray whitespace, config keys unique to the failing env
// Read-only. input (optional): { env: "all", focus: "prod" }. Credentials stay in config.yaml secret references.
const env = (input && input.env) || "all"
const focus = (input && input.focus) || "prod"
const [outbox, cfg, deploys] = await Promise.all([
  ops.db.query({ env, args: ["select region, error, count(*)::int as n, max(attempts)::int as attempts from email_outbox where status = 'retrying' group by 1, 2 order by n desc"] }),
  ops.db.query({ env, args: ["select key, value from app_config"], limit: 2000 }),
  ops.db.query({ env, args: ["select version, changed, at::text from deploys where at > now() - interval '2 days'"] }),
])
// value per key per env
const byKey = {}
for (const r of cfg.rows) { byKey[r.key] = byKey[r.key] || {}; byKey[r.key][r.env] = r.value }
const whitespace = cfg.rows.filter(r => typeof r.value === "string" && r.value !== r.value.trim())
  .map(r => ({ env: r.env, key: r.key, value: JSON.stringify(r.value) }))
// keys where the focus env differs from every other env, ignoring per-env values (urls, banners) that differ everywhere
const onlyFocus = []
for (const k of Object.keys(byKey)) {
  const vals = byKey[k], others = Object.keys(vals).filter(e => e !== focus).map(e => vals[e])
  const distinct = Object.keys(Object.fromEntries(others.map(v => [v, 1]))).length
  if (vals[focus] !== undefined && distinct === 1 && others[0] !== vals[focus]) onlyFocus.push({ key: k, [focus]: JSON.stringify(vals[focus]), others: JSON.stringify(others[0]) })
}
const sameDeployEverywhere = Object.keys(Object.fromEntries(deploys.rows.map(r => [r.version + " " + r.changed, 1])))
export default {
  retrying: outbox.rows.map(r => ({ env: r.env, region: r.region, error: r.error, n: r.n, attempts: r.attempts })),
  whitespace_values: whitespace,
  config_only_different_in_focus: onlyFocus,
  recent_deploys: sameDeployEverywhere,
  errors: [...outbox.errors, ...cfg.errors, ...deploys.errors],
}
