# Writes & safety

tmvp trusts the agent, and makes every change explicit and traceable.

- **Reads** run on the `read:` credential in read-only sessions (pg/mysql `READ ONLY` tx, mongo
  secondary reads, k8s GET-only transport, cloud read-op allow lists). A read verb that would mutate
  → `policy_blocked` with a hint naming `tmvp write`. Make the read role unprivileged anyway: the
  session flags are defence in depth.
- **Writes**: only `tmvp write <env> <system> <verb|statement> [args] --reason "why"`.
  One env, one system, never batched. `--reason` is mandatory. `--preview` shows the blast radius
  where cheap (rolled-back transaction with rows affected, dry run, the request it would send).
- A non-verb after the system goes to the type's **default write verb**: `tmvp write prod db
  "update users set …"` → `exec`; `tmvp write prod bastion @rotate` → ssh `run @rotate`.
- **Destructive** (DROP/TRUNCATE, DELETE/UPDATE without WHERE, empty mongo filters, flushdb, purge,
  namespace delete, …) runs only where `destructive: true` (env or system).
- **read-write verbs** (web click/fill/press/select/do, cloud `call`, http `call`) run as plain
  reads only when they cannot mutate; through `tmvp write` they may.
- **Cost guard** on every read: estimated first, refused with `too_expensive` when over its timeout (`tmvp llms cost-and-timeouts`).
- **Guards** on every call: per-target timeout (always a hard kill, even with `--force`), statement timeout, row/line/byte caps, bounded
  scans/consumes/tails. Cut-offs → `guard_rejected` (exit 5) or a `truncated` status.
- **Audit**: every command is recorded (JSONL, OTel log shape) under the product's log dir;
  `tmvp logs --writes` reads the write audit back.

## Good
```
tmvp write prod db "update orders set status='paid' where id=42" --reason "support #812" --preview
tmvp write prod db "update orders set status='paid' where id=42" --reason "support #812"
tmvp write prod cluster scale deploy/api 4 --reason "traffic spike"
tmvp write prod api call refund-order id=42 --reason "support #812"
```
## Bad
- writing without the user asking; looping `tmvp write` over envs to "batch" a change
- `--reason "fix"` — say what and why; it is the audit trail
- putting the mutation in a read verb, or a secret value in an argument
