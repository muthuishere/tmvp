---
title: Why T-mVp
description: Let your coding agent diagnose issues faster across every environment, safely — and the foundations underneath.
sidebar:
  order: 1
---

Something breaks in prod-eu but not prod-us. Your coding agent — Claude, Codex or any other — could find out in a minute, if it could look. Today it either can't reach the systems, or it reaches them the way a human does: `ssh`, `psql`, `kubectl`, `aws`, three dashboards and a password manager, one environment at a time, with a prod login it should never have held.

T-mVp lets the agent diagnose faster across every environment, safely. It runs the same read across dev1, dev2, staging, prod-eu and prod-us in one call and gets back only the differences (`--diff`). Reads are enforced read-only, expensive ones are refused before they run, writes happen only when you ask, and secrets never enter the agent's context.

The CLI is the mechanism — `tmvp <env> <system> <verb>` — so the same commands work for the agent and for you.

## Common operational problems, and how tmvp handles them

<ul class="tm-issues">
<li><b>Twelve CLIs, twelve auth flows</b> → one CLI, one grammar, every system type.</li>
<li><b>"Is staging the same as prod?"</b> → one read across all envs, with <code>--diff</code> showing the gap.</li>
<li><b>An agent ran an UPDATE on prod</b> → reads run in read-only transactions; writes need <code>tmvp write … --reason</code>.</li>
<li><b>A runaway query takes down the database</b> → a cost estimate before the run, timeouts and row caps; <code>too_expensive</code> stops it first.</li>
<li><b>An API key pasted into the chat</b> → secrets are <code>$(commands)</code>, resolved in server memory and never printed.</li>
<li><b>"Who changed that, and why?"</b> → every command lands in a JSONL audit log with the reason attached.</li>
<li><b>The DB sits behind a bastion</b> → <code>via</code> chains handle the tunnels; you just name the system.</li>
<li><b>Agents guessing at your schema</b> → <code>tmvp &lt;env&gt; &lt;system&gt; ls</code> and <code>describe</code> ask the live system.</li>
<li><b>"Where are the logs, again?"</b> → Loki, Prometheus, Grafana, Elasticsearch and cloud logs, behind the same verbs.</li>
<li><b>One env down, whole answer lost</b> → healthy envs still answer (exit 4, <code>partial</code>) and the failed ones are named.</li>
</ul>

## Foundations

### Read-only, enforced
Not a polite request to the agent. Every verb is declared `read`, `write`, `destructive` or `read-write` in the connector registry (`tmvp verbs`). Database reads run inside read-only transactions; HTTP and gRPC calls go through method rules. A read cannot become a write.

### Audit everything
Every command is one JSONL line in OpenTelemetry log shape — env, system, verb, estimate, reason, whether `--force` was used — in `~/.tmvp/<product>/logs/events.jsonl`. Writes get an intent record before and an outcome record after. Read it back with `tmvp logs --since 1d` (`--writes` for writes only).

### Limits before damage
Each read gets an estimate (duration, rows, bytes) before it runs. Timeouts follow one precedence (flag › system › env › global › 10s), and row caps bound the output. Too expensive is refused unless you deliberately `--force` it.

### A broker for secrets
Config holds commands, not values: `$(sec …)`, `$file(…)`. The local server runs them, keeps results in memory and hands back data, never credentials. `tmvp secrets status` and `tmvp certs` report health and expiry without showing a value.

### Via chains
Real infrastructure is layered. A system declares `via:` — ssh hops, port-forwards, k8s, a docker container — and tmvp walks it. The agent asks for `prod.db`; the route is handled.

### A local, per-user server
A small server starts automatically and belongs only to you. It holds connections, logins and secrets so the CLI stays fast and stateless; nothing sensitive goes to a shared service.
