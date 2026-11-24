// Scenario 1: orders stuck after payment — stuck orders, rejected webhooks and payfast config per env, plus which env is odd
// Read-only. input (optional): { env: "all" }. Credentials stay in config.yaml secret references.
import { gather } from "tmvp:evidence"
import { oddOneOut } from "tmvp:drift"

const env = (input && input.env) || "all"
const [ev, cfg] = await Promise.all([
  gather({
    stuck: { op: "db.query", args: ["select count(*)::int as stuck, min(created_at)::text as oldest from orders where status = 'pending_payment'"] },
    hooks: { op: "db.query", args: ["select status_code as status, error, count(*)::int as n from webhook_events where status_code <> 200 group by 1, 2 order by n desc"] },
  }, { env }),
  oddOneOut("db.query", { env, args: ["select key, value from app_config where key like 'payfast.%' order by key"] }),
])
const envs = Object.entries(ev.envs).map(([e, v]) => ({ env: e, ...((v.stuck || [])[0] || { stuck: 0 }), rejected_webhooks: v.hooks || [] }))
export default {
  affected: envs.filter(v => v.stuck > 0),
  odd_config: cfg.odd,
  clean_envs: envs.filter(v => !v.stuck).map(v => v.env),
  errors: [...ev.errors, ...cfg.errors],
}
