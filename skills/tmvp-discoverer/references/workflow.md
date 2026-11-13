# Discovery workflow

## Variables
- `{repo}` — repo root (`git rev-parse --show-toplevel`, else cwd); `{REPO}` its name (from `git remote get-url origin`, else the folder).
- `{USER}` — `git config user.name`; address them by it.
- `{session}` — `~/.tmvp/discovery/{REPO}/session.md`: the running record (findings, decisions, open questions, plan, progress). Write it after every decision, so a new session can resume.
- `{skill-root}` — this skill's folder (references live here).

## Steps
| step | file | ends with |
|---|---|---|
| 0 | `steps/step-00-preflight.md` | resume or start; tools present |
| 1 | `steps/step-01-scan.md` | silent scan → findings in `{session}` |
| 2 | `steps/step-02-projects.md` | user's project grouping (blast radius) |
| 3 | `steps/step-03-environments.md` | user's env list per project |
| 4 | `steps/step-04-systems-and-variables.md` | user-confirmed system × env matrix, reach, variable → secret map, sensitivity |
| 4b | `steps/step-04b-questions-agents-ask.md` | saved-query list from real needs |
| 5 | `steps/step-05-plan.md` | **user approves the plan** |
| 6 | `steps/step-06-build.md` | skills written, proven, reported |

Question bank: `references/questions.md`. Never skip ahead. Going back is fine: if the user changes an earlier answer, update `{session}` and redo the later steps that depend on it.

## Session file shape
```markdown
# tmvp discovery — {REPO}  (started YYYY-MM-DD, step N)
## Findings        (units, envs, systems, variables — with evidence file:line)
## Decisions       (- YYYY-MM-DD decided|defaulted|skipped: projects = core[api,worker,migrations], site[web])
## Depends         (projects → envs → matrix → saved queries → plan: a changed answer redoes only what follows it)
## Sensitivity     (excluded / counts-only tables, prod limits)
## Saved queries   (name · params · system · envs)
## Open questions  (verbatim, so resume starts with them)
## Plan            (step 5, verbatim as approved)
## Progress        (step 6 checklist)
```
No secret values ever go in it — names only.
