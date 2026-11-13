# Step 0 — preflight

1. Resolve `{repo}`, `{REPO}`, `{USER}`, `{session}` (workflow.md).
2. Tools: `tmvp version` — missing is a **hard stop** (give the install command: `curl -fsSL …/install.sh | sh`). `sec --version` — missing → variables come from env-file names only; note it. `gh auth status` only if the remote is GitHub.
3. If `{session}` exists: show the round header, the decisions in 3 lines, then ask the **unanswered questions verbatim** (plus "or start over?"). Stop.
4. New session: one line — "I'll scan read-only, then ask about projects, environments, systems and the questions agents will ask; I write nothing until you approve the plan." Then the standing commands in one line (`defaults · why · skip · back <step> · map · pause`). Go to step 1.
