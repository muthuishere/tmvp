# Code mode — one program instead of many calls

`tmvp code run` takes one small JavaScript program. The program calls tmvp operations
(in parallel, in loops), works on the results, and only its final value comes back.
Every call still goes through the local tmvp server, like a CLI call.

## When to use it

- **One question, one call:** use the plain CLI. `tmvp all db query "select …" --diff`.
- **Several reads, then a comparison or a drill-down:** write one program. The raw rows stay
  inside the program; the agent gets a short summary.
- **An investigation you will run again:** save it as a script in the skill (`scripts/<name>.js`).

Check saved scripts before writing a new one: `tmvp code scripts`.

## Commands

```
tmvp code operations              # every operation: db.query, db.describe, write.db.exec, ...
tmvp code describe db.query       # input schema of one operation
tmvp code check prog.js           # diagnostics only, nothing runs (exit 2 on errors)
tmvp code run prog.js --json      # {ok, value, error, ms}
tmvp code run stuck-orders        # <skill dir>/scripts/stuck-orders.js
tmvp code scripts                 # saved scripts + first-line description
tmvp code docs                    # the language reference (supported JS subset)
```

Flags: `--json`, `--input '<json>'` (read as the global `input`), `--max-steps`,
`--max-time` (default 60s), `--concurrency` (default 4), `--max-ops` (default 200 operation
calls per program; past it the program stops, uncatchable, exit 1), `--max-memory` (default 1024 MiB; a best-effort soft Go memory limit so a
runaway program fails instead of exhausting the machine; 0 = off), `--allow-writes`.
`-e/--env` is refused: put `env` in each operation call.
Exit codes: 0 ok, 1 the program failed, 2 nothing ran (check errors, usage).

## The API

Reads are `ops.<system>.<verb>(opts)`. Options:
`{env, args[], flags{}, labels, diff, limit, since, timeout, reason}`.
`env` takes any selector the CLI takes: `prod`, `dev,prod`, `all`, `type:prod`, `'prod*'`.
There is no `force` on reads.

A call returns `{rows, lines, targets, errors, exit}`. Every row and line carries its `env`.
A refused call throws, with the tmvp code first: `policy_blocked: …`, `too_expensive: …`.

The language is a subset of JavaScript run inside the CLI (no Node, no npm, no `new`, no
regular expressions). `tmvp code docs` lists what is supported.

## Example: compare envs, then drill into the odd one

```js
// which env has orders stuck in pending_payment, and why
const stuck = await ops.db.query({ env: "all", args: ["select count(*)::int as n from orders where status = 'pending_payment'"] })
const odd = stuck.rows.filter(r => r.n > 0).map(r => r.env)
const detail = await Promise.all(odd.map(env => Promise.all([
  ops.db.query({ env, args: ["select status_code, error, count(*)::int as n from webhook_events where status_code <> 200 group by 1, 2"] }),
  ops.app.run({ env, args: ["@logs", "1h"] }),
])))
export default odd.map((env, i) => ({
  env,
  stuck: stuck.rows.find(r => r.env === env).n,
  webhooks: detail[i][0].rows,
  log_errors: detail[i][1].lines.filter(l => l.text.indexOf(" 500") >= 0).length,
}))
```

```
tmvp code run prog.js --json
```

## Saved scripts in the repo

Saved scripts live in the skill: `<skill dir>/scripts/<name>.js`. The first `//` line is the
description shown by `tmvp code scripts`. Run one by name: `tmvp code run <name>`, with
`--input '{"env":"prod"}'` for parameters. `tmvp new skill` scaffolds `scripts/example.js`.

Commit them with the skill. They hold no secrets: credentials stay in `config.yaml` as secret
references, resolved by the server.

## Writes

Writes are `ops.write.<system>.<verb>({env, args, reason, preview, force})` (operation name `write.<system>.<verb>`).

- Refused unless the run has `--allow-writes`.
- `reason` is required.
- The server still enforces one env per write, `force` on protected envs, and the audit log
  (`tmvp logs --writes`).

```js
const r = await ops.write.db.exec({ env: "staging", args: ["update orders set status = 'paid' where id = 42"], reason: "support #812", preview: true })
```

## Safety: the server is still the gate

A program can do nothing a CLI call could not. Reads are read-only sessions, the cost guard
(`too_expensive`), timeouts, row caps, env-type protection and the audit log all apply to every
call. `--max-steps`, `--max-time`, `--max-ops` and `--max-memory` bound the program itself; a
per-call `timeout` above `--max-time` is refused (`policy_blocked`), so a program cannot raise
the cost-guard ceiling past its own budget. Strings are capped at 64 MiB and arrays at 10M
elements (the run stops). Imports are confined to the script's directory, symlinks resolved.

## Benchmark

acme demo (`examples/agent-benchmark`), warm server, median of 7, bytes of `--json` output:

| Investigation | One program | Separate calls |
|---|---|---|
| Stuck orders | 618 B, 83 ms | 3,245 B, 112 ms (3 calls) |
| Missed migration | 428 B, 167 ms | 70,554 B, 291 ms |
| Trailing space | 1,033 B, 172 ms | 172,499 B, 187 ms |
