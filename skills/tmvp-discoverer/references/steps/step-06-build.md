# Step 6 — build (only after approval)

Track a checklist under `## Progress` in `{session}`; update it as you go.

Per project:
1. `tmvp new skill <project> --from <existing config>` if one exists, else `tmvp new skill <project> --envs <e1,e2,…>` (links `.claude/skills/`).
2. Write `config.yaml` per `references/wiring.md` and `references/systems/<type>.md`; secrets as `$file(sec get <project>@<env>:NAME --out ${out})`; prod envs `destructive: false`; unknowns `REPLACE_ME`.
3. `tmvp --skill <dir> validate`, then `check` (all envs). Fix config errors yourself. A blocker you can't fix (login, missing secret) → note it, keep going.
4. One "prove it" read per system (`references/wiring.md`). For data stores run `ls` and `describe` on the main tables.
5. Write `references/` and `assets/` per `references/output.md`: projects.md, environments.md, one `<system>.md` each (data stores: databases, tables from code confirmed live, table → code users), saved read-only queries from the code. Every command in them is a tmvp command.
6. Run every saved query in every env (`tmvp --skill <dir> all <db> @<name> …`); fix or drop the ones that fail.
7. `tmvp --skill <dir> skill sync` (SKILL.md lists the references) and `skill lint`.

8. Offer code-mode scripts: for each multi-step investigation {USER} named (step 4b, e.g. "orders stuck after payment"), offer to write `scripts/<name>.js` in the skill dir — one program that runs the reads across envs and returns only a summary (`tmvp llms code-mode`). First line `// <description>`; read-only; no secrets. Write only the ones {USER} picks, then `tmvp --skill <dir> code check scripts/<name>.js` and `tmvp --skill <dir> code run <name> --json`; fix or drop the ones that fail.

Report to {USER}: per project, env × system → ok / blocker; saved queries and scripts written; owner steps (exact commands); what was left out. Ask whether to commit — never commit unasked.
