// Health sweep: a cheap probe per system across envs, returning only what is failing.
// Operations are passed by name because their names come from the product config.

// call() throws synchronously for an unknown operation name; an async wrapper turns that into a rejection.
async function run(op, args) { return call(op, args) }

/** Run every probe on the selected envs and return only the failing env/system pairs.
 * failing({ db: { op: "db.query", args: ["select 1"] }, app: { op: "app.run", args: ["@status"] } }, { env: "all" })
 * -> { failing: [{ probe, env, system, code, message }], ok: ["db@dev", ...], checked: 6 }.
 * A probe that throws (every target failed, refused, too expensive) is reported as failing, never thrown. */
export async function failing(probes, opts = {}) {
  const names = Object.keys(probes)
  const out = { failing: [], ok: [], checked: 0 }
  const rs = await Promise.all(names.map(n => {
    const { op, ...args } = probes[n]
    const env = args.env || opts.env || "all"
    return run(op, { ...args, env }).then(r => ({ r, env }), e => ({ e, env }))
  }))
  names.forEach((n, i) => {
    const { r, e, env } = rs[i]
    if (e) {
      out.checked++
      out.failing.push({ probe: n, env, code: "error", message: String(e.message || e) })
      return
    }
    const bad = {}
    for (const x of r.errors) {
      bad[(x.env || "") + "/" + (x.system || "")] = 1
      out.failing.push({ probe: n, env: x.env, system: x.system, code: x.code, message: x.message })
    }
    for (const t of r.targets) {
      out.checked++
      if (bad[t.env + "/" + t.system]) continue
      if (t.status === "ok" || t.status === "truncated") out.ok.push(n + "@" + t.env)
      else out.failing.push({ probe: n, env: t.env, system: t.system, code: t.status, message: t.note || "" })
    }
  })
  out.ok.sort()
  return out
}
