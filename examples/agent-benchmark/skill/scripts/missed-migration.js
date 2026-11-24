// Scenario 2: a page fails in one env — schema migration level, refund_reason column and app 500s per env
// Read-only. input (optional): { env: "all", since: "1h" }. Credentials stay in config.yaml secret references.
import { laggingEnvs, gather, logHits } from "tmvp:evidence"

const env = (input && input.env) || "all"
const since = (input && input.since) || "1h"
const [mig, ev, logs] = await Promise.all([
  laggingEnvs("db.query", { env, args: ["select max(version)::int as version from schema_migrations"] }, { column: "version" }),
  gather({ cols: { op: "db.query", args: ["select count(*)::int as n from information_schema.columns where table_name = 'orders' and column_name = 'refund_reason'"] } }, { env }),
  logHits("app.run", { env, args: ["@logs", since] }, { match: " 500" }),
])
const hits = e => (logs.hits[e] || { count: 0 })
export default {
  latest_migration: mig.latest,
  behind: mig.behind.map(b => ({ env: b.env, migration: b.value, missing: b.missing,
    has_refund_reason: ((ev.envs[b.env] || {}).cols || [{ n: 0 }])[0].n > 0, errors_500: hits(b.env).count, sample: hits(b.env).sample })),
  ok_envs: mig.current.map(e => e + (hits(e).count ? " (" + hits(e).count + " 500s)" : "")),
  errors: [...mig.errors, ...ev.errors, ...logs.errors],
}
