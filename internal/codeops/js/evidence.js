// Incident evidence in one call: several reads in parallel folded per env, log hit counts, envs lagging behind.
// Operations are passed by name ("db.query", "app.run") because their names come from the product config.

// call() throws synchronously for an unknown operation name; an async wrapper turns that into a rejection.
async function run(op, args) { return call(op, args) }

function strip(row) {
  const o = {}
  for (const k of Object.keys(row)) if (k !== "env" && k !== "system") o[k] = row[k]
  return o
}

const byEnvName = (x, y) => (x.env < y.env ? -1 : x.env > y.env ? 1 : 0)

function caught(env, e) {
  return { rows: [], lines: [], errors: [{ env, code: "error", message: String(e.message || e) }] }
}

function read(op, args, opts) {
  const env = args.env || opts.env || "all"
  // parallel targets arrive in any order; sort by env (stable) so output is repeatable
  return run(op, { ...args, env }).then(r => ({ ...r, rows: r.rows.toSorted(byEnvName), lines: r.lines.toSorted(byEnvName) }), e => caught(env, e))
}

/** Run named reads in parallel and fold them per env, so you read one object instead of N result sets.
 * gather({ stuck: { op: "db.query", args: ["select count(*)::int as n from orders where status = 'pending'"] },
 *          logs:  { op: "app.run", args: ["@logs", "1h"] } }, { env: "all" })
 * -> { envs: { dev: { stuck: [{ n: 0 }], logs: ["line", ...] }, ... }, errors: [{ check, env, code, message }] }.
 * Each check takes the op's own args (env, args, flags, since, limit); opts.env (default "all") fills a missing env.
 * A check that fails everywhere becomes an error entry; the program keeps going. */
export async function gather(checks, opts = {}) {
  const names = Object.keys(checks)
  const rs = await Promise.all(names.map(n => {
    const { op, ...args } = checks[n]
    return read(op, args, opts)
  }))
  const envs = {}, errors = []
  const at = e => (envs[e] = envs[e] || {})
  names.forEach((n, i) => {
    const r = rs[i]
    for (const row of r.rows) { const o = at(row.env); (o[n] = o[n] || []).push(strip(row)) }
    for (const l of r.lines || []) { const o = at(l.env); (o[n] = o[n] || []).push(l.text) }
    for (const e of r.errors) errors.push({ check: n, ...e })
  })
  return { envs, errors }
}

/** Count log lines containing a substring per env, with the first matching line as a sample (e.g. HTTP 500s).
 * logHits("app.run", { args: ["@logs", "1h"] }, { match: " 500" })
 * -> { hits: { prod: { count: 12, sample: "..." } }, quiet: ["dev", "staging"], errors }.
 * quiet lists envs that returned lines but none matched. */
export async function logHits(op, args = {}, opts = {}) {
  const r = await read(op, args, opts)
  const hits = {}, seen = {}
  for (const l of r.lines || []) {
    seen[l.env] = 1
    if (opts.match && l.text.indexOf(opts.match) < 0) continue
    const h = hits[l.env] = hits[l.env] || { count: 0, sample: l.text }
    h.count++
  }
  return { hits, quiet: Object.keys(seen).filter(e => !hits[e]), errors: r.errors }
}

/** Envs that lag behind the others on a numeric column (migration version, schema level, release number).
 * laggingEnvs("db.query", { args: ["select max(version)::int as version from schema_migrations"] }, { column: "version" })
 * -> { latest: 42, behind: [{ env, value, missing }], current: ["dev", "staging"], errors }. column defaults to "version". */
export async function laggingEnvs(op, args = {}, opts = {}) {
  const col = opts.column || "version"
  const r = await read(op, args, opts)
  const v = {}
  for (const row of r.rows) {
    const n = Number(row[col])
    if (row[col] !== null && row[col] !== undefined && !Number.isNaN(n)) v[row.env] = Math.max(v[row.env] ?? n, n)
  }
  const es = Object.keys(v)
  const latest = es.length ? Math.max(...es.map(e => v[e])) : null
  return {
    latest,
    behind: es.filter(e => v[e] < latest).map(e => ({ env: e, value: v[e], missing: latest - v[e] })),
    current: es.filter(e => v[e] === latest),
    errors: r.errors,
  }
}
