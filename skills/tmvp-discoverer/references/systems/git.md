# git

A plain git remote (any host): log, diffs, tags, what got deployed. Fields and verbs: `tmvp llms types/git` · `tmvp verbs git`.

## Find it in the codebase
- `git remote -v`, deploy tags pattern (`git tag`), release branches; use `gh` instead when the remote is GitHub and the user is logged in.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which branch per env is deployed?
- Key or token for a private remote (sec name)?

## Config
```yaml
envs:
  prod:
    secrets:
      git_key: "$(cat ~/.ssh/REPLACE_ME)"
    systems:
      repo:
        type: git
        url: git@git.example.com:acme/acme.git
        branch: main
        key: "${file:secrets.git_key}"
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod repo log --limit 5`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/repo.md`: branch/tag per env, tag naming for releases, how to see what is deployed (`deploys`, `tags`).
