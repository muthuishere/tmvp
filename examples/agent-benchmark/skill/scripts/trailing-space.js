// Scenario 3: emails silently fail in one env — retrying emails by region/error, config values with stray whitespace, config keys unique to the failing env
// Read-only. input (optional): { env: "all", focus: "prod" }. Credentials stay in config.yaml secret references.
import { gather } from "tmvp:evidence"
import { oddOneOut, strayWhitespace } from "tmvp:drift"

const env = (input && input.env) || "all"
const focus = (input && input.focus) || "prod"
const config = { env, args: ["select key, value from app_config"], limit: 2000 }
const [ev, ws, odd] = await Promise.all([
  gather({
    retrying: { op: "db.query", args: ["select region, error, count(*)::int as n, max(attempts)::int as attempts from email_outbox where status = 'retrying' group by 1, 2 order by n desc"] },
    deploys: { op: "db.query", args: ["select version, changed, at::text from deploys where at > now() - interval '2 days'"] },
  }, { env }),
  strayWhitespace("db.query", config, { key: "key", value: "value" }),
  oddOneOut("db.query", config, { focus }),
])
const all = k => Object.entries(ev.envs).flatMap(([e, v]) => (v[k] || []).map(r => ({ env: e, ...r })))
export default {
  retrying: all("retrying"),
  whitespace_values: ws.found.map(w => ({ env: w.env, key: w.key, value: w.value })),
  config_only_different_in_focus: odd.odd.map(o => ({ key: o.key, [focus]: JSON.stringify(o.value), others: JSON.stringify(o.others) })),
  recent_deploys: [...new Set(all("deploys").map(r => r.version + " " + r.changed))],
  errors: [...ev.errors, ...ws.errors, ...odd.errors],
}
