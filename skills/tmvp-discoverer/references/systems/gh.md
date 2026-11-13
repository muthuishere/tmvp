# gh

A GitHub repository: workflow runs and failure logs, deployments, PRs, releases. Fields and verbs: `tmvp llms types/gh` · `tmvp verbs gh`.

## Find it in the codebase
- `git remote -v` (owner/name), `.github/workflows/*.yml` (`name:`, `on:`, `environment:`), GitHub Environments named in workflows (`environment: production`).
- Monorepo: one repo can serve several projects — note which workflows belong to which project (`paths:` filters).

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Is the repo on GitHub Enterprise (then `host:`)?
- Use the user's `gh auth login` (default) or a token from sec?

## Config
```yaml
envs:
  prod:
    systems:
      gh:
        type: gh
        repo: acme-org/acme
        # host: github.example.com
        # token: "${secrets.gh_token}"   # only if not using gh auth login
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod gh runs --limit 3`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/gh.md`: workflows → what they deploy, to which env, triggered how (push to branch / tag / manual dispatch with which inputs). Map GitHub Environments to tmvp envs. Note how to find the last deploy of each env (`deployments`, or `runs --workflow <file>`).
