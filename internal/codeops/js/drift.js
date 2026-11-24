// Drift across envs: run one read everywhere and keep only what differs (config, flags, versions, stray whitespace).
// Operations are passed by name ("db.query") because their names come from the product config.

const META = { env: 1, system: 1 }

function strip(row) {
  const o = {}
  for (const k of Object.keys(row)) if (!META[k]) o[k] = row[k]
  return o
}

const byEnvName = (x, y) => (x.env < y.env ? -1 : x.env > y.env ? 1 : 0)

function same(a, b) { return JSON.stringify(a) === JSON.stringify(b) }

async function readAll(op, args, envs) {
  const list = envs && envs.length ? envs : [args.env || "all"]
  const rows = [], errors = []
  const rs = await Promise.all(list.map(env => call(op, { ...args, env })
    .catch(e => ({ rows: [], errors: [{ env, code: "error", message: String(e.message || e) }] }))))
  for (const r of rs) { rows.push(...r.rows); errors.push(...r.errors) }
  rows.sort(byEnvName) // parallel targets arrive in any order; keep output stable
  return { rows, errors }
}

// per env: { key: value } when key is given, else the env's rows (env/system dropped)
function byEnv(rows, key, value) {
  const out = {}
  for (const r of rows) {
    out[r.env] = out[r.env] || (key ? {} : [])
    if (key) out[r.env][r[key]] = value ? r[value] : strip(r)
    else out[r.env].push(strip(r))
  }
  return out
}

/** Run one read on many envs; return each env's result side by side plus only what differs.
 * compareEnvs("db.query", { args: ["select key, value from app_config"] }, { key: "key", value: "value", envs: ["dev", "prod"] })
 * -> { envs: { dev: { k: v }, prod: { ... } }, differs: [{ key, values: { dev, prod } }], errors }.
 * envs defaults to args.env or "all". Without `key`, each env's rows are compared whole and
 * differs lists the envs whose rows are not shared by a majority: [{ env, rows }]. */
export async function compareEnvs(op, args = {}, opts = {}) {
  const { rows, errors } = await readAll(op, args, opts.envs)
  const envs = byEnv(rows, opts.key, opts.value)
  const names = Object.keys(envs)
  const differs = []
  if (opts.key) {
    const keys = {}
    for (const e of names) for (const k of Object.keys(envs[e])) keys[k] = 1
    for (const k of Object.keys(keys)) {
      const values = {}
      for (const e of names) values[e] = envs[e][k]
      const vs = names.map(e => values[e])
      if (vs.some(v => !same(v, vs[0]))) differs.push({ key: k, values })
    }
  } else {
    for (const e of names) {
      const agree = names.filter(f => same(envs[f], envs[e])).length
      if (agree * 2 <= names.length) differs.push({ env: e, rows: envs[e] })
    }
  }
  return { envs, differs, errors }
}

/** The odd one out: keys where one env's value differs while every other env agrees.
 * oddOneOut("db.query", { args: ["select key, value from app_config"] }, { key: "key", value: "value", focus: "prod" })
 * -> { odd: [{ key, env, value, others }], envs: ["dev", ...], errors }. key/value default to "key"/"value".
 * Keys that differ everywhere (urls, banners) are ignored. With `focus`, only keys where that env is the
 * odd one are kept (works with 2 envs); without it a key needs 3+ envs. */
export async function oddOneOut(op, args = {}, opts = {}) {
  const key = opts.key || "key", value = opts.value || "value"
  const { envs, differs, errors } = await compareEnvs(op, args, { ...opts, key, value })
  const odd = []
  for (const d of differs) {
    const es = Object.keys(d.values).filter(e => d.values[e] !== undefined)
    for (const e of es) {
      if (opts.focus && e !== opts.focus) continue
      const others = es.filter(f => f !== e).map(f => d.values[f])
      if (!others.length || (!opts.focus && es.length < 3)) continue
      if (others.every(v => same(v, others[0])) && !same(d.values[e], others[0])) {
        odd.push({ key: d.key, env: e, value: d.values[e], others: others[0] })
      }
    }
  }
  return { odd, envs: Object.keys(envs), errors }
}

/** Values with leading or trailing whitespace: a stray space or newline in config is a classic silent failure.
 * strayWhitespace("db.query", { args: ["select key, value from app_config"] }, { key: "key", value: "value" })
 * -> { found: [{ env, key, column, value }], errors }, value JSON-quoted so the whitespace is visible.
 * Without `value`, every string column of every row is checked (key then names the column). */
export async function strayWhitespace(op, args = {}, opts = {}) {
  const { rows, errors } = await readAll(op, args, opts.envs)
  const found = []
  for (const r of rows) {
    const cols = opts.value ? [opts.value] : Object.keys(strip(r))
    for (const c of cols) {
      const v = r[c]
      if (typeof v === "string" && v !== v.trim()) {
        found.push({ env: r.env, key: opts.key ? r[opts.key] : c, column: c, value: JSON.stringify(v) })
      }
    }
  }
  return { found, errors }
}
