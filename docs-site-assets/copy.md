# T-mVp: Copy Deck

Tone: calm, cool, unbothered. The hero doesn't shout and doesn't look back. Every claim below is a real feature (source: `llms.txt`, `docs/llms/*.md`).

---

## 1. Taglines

1. **One connection. Every system. Every env.**
2. **Cool head. Read-only hands.**
3. **One move. A dozen problems handled.**
4. **Walk into prod. Touch nothing.**
5. **Every system answers. Nothing breaks.**

**Recommended:** **One connection. Every system. Every env.** It says what the product does, and the rhythm carries the swagger without overselling.

---

## 2. Homepage hero

**Headline:** One connection to every system you run.

**Subline:** T-mVp gives coding agents and engineers a single CLI into postgres, kafka, k8s, clouds, logs, metrics, APIs and more, across dev, staging and prod. Reads are enforced read-only. Writes need a reason. Secrets never reach the agent.

**CTAs:**
- Primary: **Install T-mVp**
- Secondary: **Read the 5-minute overview**

### Feature cards

**One connection**
Postgres, MySQL, Mongo, Redis, Kafka, NATS, SSH, k8s, AWS/GCP/Azure, Prometheus, Loki, Grafana, Elasticsearch, git, gh, HTTP/OpenAPI, gRPC, web pages and scripts. One grammar: `tmvp <env> <system> <verb>`.

**Every environment at once**
Reads run across every env in parallel. Add `--diff` and see where staging and prod disagree, in one command.

**Read prod without fear**
Read-only transactions, method allowlists, timeouts, cost estimates before the run, and row caps. An expensive query gets stopped before it starts.

**Writes on purpose**
Changes go through `tmvp write <env> <system> … --reason`, one target at a time. `--preview` first, and protected envs need an explicit `--force`.

**Secrets never seen**
Credentials are `$(commands)`, for example from the `sec` broker. They resolve inside the local server's memory and never show up in the agent's context, output or logs.

**Agents onboard themselves**
The `tmvp-discoverer` skill scans your repo, asks about projects, environments and systems, and writes a per-project skill. `tmvp envs`, `systems`, `ls` and `--llmhelp` cover the rest.

---

## 3. The many issues

Every problem here gets the same calm move.

- **Twelve CLIs, twelve auth flows** → one CLI, one grammar, every system type.
- **"Is staging the same as prod?"** → one read across all envs, with `--diff` showing the gap.
- **An agent ran an UPDATE on prod** → reads run in read-only transactions, and writes need `tmvp write … --reason`.
- **A runaway query takes down the database** → a cost estimate before the run, timeouts, row caps, and `too_expensive` stops it first.
- **An API key pasted into the chat** → secrets are `$(commands)`, resolved in server memory and never printed.
- **"Who changed that, and why?"** → every command lands in a JSONL audit log with the reason attached.
- **The DB sits behind a bastion behind a VPN** → `via` chains handle the tunnels, and you just name the system.
- **Agents guessing at your schema** → `tmvp <env> <system> ls` and `describe` ask the live system instead.
- **"Where are the logs, again?"** → Loki, Prometheus, Grafana, Elasticsearch and cloud logs, all behind the same verbs.
- **One env down, whole answer lost** → partial results come back from the healthy envs (exit 4) and the failed ones are named.

---

## 4. Foundations

Swagger is earned, and this is what it rests on.

**Read-only, enforced.**
Read-only isn't a polite request to the agent. Every verb is declared `read`, `write` or `destructive` in the connector registry. Database reads run inside read-only transactions, and HTTP and gRPC calls go through method allowlists. A read physically can't become a write.

**Audit everything.**
Every command is recorded as JSONL in OpenTelemetry log shape: who, which env, which system, the cost estimate, the reason, and whether `--force` was used. `tmvp audit --since 1d` shows the record.

**Limits before damage.**
Each read gets an estimate (duration, rows, bytes) before it runs. Timeouts follow a clear precedence and row caps bound the output. Anything too expensive is refused unless you deliberately `--force` it.

**A broker for secrets.**
Config holds commands, not values: `$(sec …)`, `$file(…)`. The local server runs them, keeps the result in memory, and hands back data, never credentials. `tmvp secrets status` and `tmvp certs` report health and expiry without exposing a value.

**Via chains.**
Real infrastructure is layered. A system can declare `via`, a chain of SSH hops, port-forwards and tunnels, and T-mVp walks it for you. The agent asks for `prod.db` and the route is handled.

**A local, per-user server.**
A small server starts automatically on your machine and belongs only to you. It holds connections, logins and secrets so the CLI stays fast and stateless, and nothing sensitive goes to a shared service.

---

## 5. Microcopy

**404:** This page walked off without looking back. Try `tmvp llms` for the docs.

**Install success:** T-mVp is in. Run `tmvp envs` and let every system report.

**Footer:** T-mVp: one connection, solid foundations. Built so prod stays calm.
