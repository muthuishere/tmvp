# web

A web app driven in a browser as a declared user: pages, text, screenshots, checks. Fields and verbs: `tmvp llms types/web` · `tmvp verbs web`.

## Find it in the codebase
- Frontend routes: `app/**/page.tsx`, `pages/`, `src/routes/`, router files; login pages; e2e tests (`playwright/`, `cypress/`) show page paths and selectors.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which test/admin user per env can agents log in as (password stored in sec)?
- Which pages matter for checks (login, dashboard, checkout)?

## Config
```yaml
envs:
  prod:
    secrets:
      viewer_pw: "$file(sec get acme@production:E2E_VIEWER_PASSWORD --out ${out})"
    systems:
      site:
        type: web
        base_url: https://app.example.com
        users:
          viewer: { role: viewer, user: viewer@example.com, password: "${secrets.viewer_pw}" }
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod site ls` then `tmvp --skill <dir> prod site check <page>`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Declare key pages in `assets/site/pages/<name>.md` (path, elements, actions) from the routes and e2e selectors; write `references/site.md` with the user journeys worth checking.
