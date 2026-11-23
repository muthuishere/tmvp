// Scenario 2: a page fails in one env — schema migration level, refund_reason column and app 500s per env
// Read-only. input (optional): { env: "all", since: "1h" }. Credentials stay in config.yaml secret references.
const env = (input && input.env) || "all"
const since = (input && input.since) || "1h"
const [mig, cols, logs] = await Promise.all([
  ops.db.query({ env, args: ["select max(version)::int as version, count(*)::int as applied from schema_migrations"] }),
  ops.db.query({ env, args: ["select count(*)::int as has_refund_reason from information_schema.columns where table_name = 'orders' and column_name = 'refund_reason'"] }),
  ops.app.run({ env, args: ["@logs", since] }),
])
const envs = {}
const at = e => (envs[e] = envs[e] || { errors_500: 0 })
for (const r of mig.rows) at(r.env).migration = r.version
for (const r of cols.rows) at(r.env).has_refund_reason = r.has_refund_reason > 0
const samples = {}
for (const l of logs.lines) {
  if (l.text.indexOf(" 500") >= 0) {
    at(l.env).errors_500++
    samples[l.env] = samples[l.env] || l.text.slice(l.text.indexOf(" ") + 1)
  }
}
const latest = Math.max(...Object.values(envs).map(v => v.migration || 0))
export default {
  latest_migration: latest,
  behind: Object.entries(envs).filter(([, v]) => v.migration < latest)
    .map(([e, v]) => ({ env: e, migration: v.migration, missing: latest - v.migration, has_refund_reason: v.has_refund_reason, errors_500: v.errors_500, sample: samples[e] })),
  ok_envs: Object.entries(envs).filter(([, v]) => v.migration === latest).map(([e, v]) => e + (v.errors_500 ? " (" + v.errors_500 + " 500s)" : "")),
  errors: [...mig.errors, ...cols.errors, ...logs.errors],
}
