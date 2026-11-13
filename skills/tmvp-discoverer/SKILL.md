---
name: tmvp-discoverer
description: Create tmvp skills for a repo through a guided session — scan the codebase, ask the user to group projects, confirm environments, systems and variables, agree a plan, then write <project>-tmvp skills (config, references, databases, saved queries) into the repo. Run only when the user asks to create/set up a tmvp skill (/tmvp-discoverer).
disable-model-invocation: true
---
# tmvp-discoverer

A guided discovery session, run only when the user asks. You scan, **show what you found, ask, and stop** — every round ends with a question and waits. Nothing is written into the repo until the user approves the plan.

The user drives. You propose with evidence and a default; the user decides. Record every decision as theirs.

## Core rules
1. **Read-only everywhere.** tmvp read verbs, `check`, `validate` only. Never `tmvp write`; never raw `ssh`/`psql`/`kubectl`/`curl`/`redis-cli` to the product's hosts.
2. **Variables: names only.** Get env-file keys with `tmvp env-keys [dir]` (names + set/empty, never values) and sec names with `sec ls --json` / `sec env ls <env>`. Never open, cat or grep `.env*`, `vault/`, `~/.config/sec/**`, ssh key files, kubeconfig tokens. For ssh aliases use only `ssh -G <alias> | grep -iE '^(hostname|user|port|identityfile) '` (paths, never key contents). Evidence quotes key names and file paths, never a line from an env file, compose `environment:` or CI YAML (they can hold literals).
3. **Prod-like envs get `destructive: false`.**
4. **Never guess.** Unknown host/bucket/URL → `REPLACE_ME` + an open question.
5. **One topic per round, three questions at most** (one AskUserQuestion call, recommended option first), then stop. Start each round with a one-line header: `[step 3/6 Environments · project core · 4 open]`.
   - Mark every finding `✓` (2+ sources), `~` (one source) or `?` (inferred). Ask only about `~` and `?`; list `✓` items in one "confirming unless you object" line.
   - Question wording, evidence and defaults: `references/questions.md`. Aim for ≤ 12 questions for a single project.
   - Standing commands (tell the user once, in step 0): `defaults` (accept this round's defaults — never for the prod list, sensitive tables or the plan) · `why` (which config field this fills) · `skip` (→ open question + `REPLACE_ME`) · `back <step>` · `map` (progress) · `pause` (save and stop).
   - Record each answer as `decided` (by {USER}), `defaulted` or `skipped`; defaulted ones are shown again at the plan gate.
6. **Resumable.** Session state lives in `~/.tmvp/discovery/<repo>/session.md` (see workflow). Resume it if it exists.

Follow `references/workflow.md`.
