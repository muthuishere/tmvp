---
title: Security model
description: What tmvp enforces, where secrets live, and what it will never do.
sidebar:
  order: 1
---

## Secrets never enter the agent's context
- Config holds **commands** (`$(…)`, `$file(… --out ${out})`, `${file:secrets.x}`), never values; `tmvp validate` and `skill lint` reject literals.
- The local per-user server runs them, keeps values in memory only, and redacts them (and derived forms such as basic-auth base64) from every output and audit line.
- Errors never contain secret values. `tmvp secrets status`, `secrets audit` and `certs` report names, states and dates only.
- `tmvp env-keys` lists env-file key names, never values.

## Reads cannot write
- Every verb has a declared mode (`tmvp verbs`); the server refuses a write verb outside `tmvp write` (`policy_blocked`, exit 3).
- SQL reads run in read-only transactions with statement and lock timeouts and a row cap; Redis, Mongo, Kafka and others allow only read commands as reads.
- HTTP: GET as reads; mutating APIs only through `tmvp write`. gRPC: only methods matching `read_methods`, never client streaming.
- Cost guard: reads estimated above their timeout are refused (`too_expensive`) unless `--force`; the timeout still kills.

## Writes are deliberate
- `tmvp write <env> <system> … --reason "…"` — exactly one env and one system, never batched.
- Prod-like envs are `destructive: false`: writes there also need `--force`; destructive statements (DROP, TRUNCATE, unfiltered DELETE/UPDATE, flushdb, namespace delete …) are refused outright.
- `--preview` shows the blast radius where cheap (rolled-back transaction, dry run, the request it would send).
- Every write leaves an intent and an outcome record in the audit log.

## What tmvp will never do
- Print, log or return a secret value.
- Turn a read into a write, or batch a write across envs.
- Log in for you: on `login_required` it names the command and you run it.
- Send your data or credentials to a shared service — the server is local and per-user.
