# http

Any HTTP API: third-party SaaS (payments, email, LLM providers) and the product's own admin/health endpoints. Fields and verbs: `tmvp llms types/http` · `tmvp verbs http`.

## Find it in the codebase
- SDKs/clients: `grep -rlE 'razorpay|stripe|paypal|brevo|sendgrid|mailgun|twilio|openai|anthropic|@google/genai|groq|telegram|slack|axios.create|fetch\(|requests\.(get|post)|http.NewRequest|RestTemplate|WebClient'`
- Base URLs: string literals `https://api.…`, OpenAPI files (`openapi*.yaml|json`, `swagger*`), the product's own `/health`, `/admin` routes.
- Env var NAMES: `*_API_KEY`, `*_SECRET`, `*_TOKEN`, `*_BASE_URL`.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which of these APIs should agents read from (only read endpoints are useful, e.g. payment status lookups)?
- Is there a test-mode key for dev and a live key for prod?

## Config
```yaml
envs:
  prod:
    destructive: false
    secrets:
      pay_key: "$file(sec get acme@production:RAZORPAY_KEY_ID --out ${out})"
      pay_secret: "$file(sec get acme@production:RAZORPAY_KEY_SECRET --out ${out})"
    systems:
      pay:
        type: http
        base_url: https://api.payments.example.com
        user: "${secrets.pay_key}"
        password: "${secrets.pay_secret}"
        # token: "${secrets.api_token}"     # bearer instead
        health_path: /v1/health
        # openapi: assets/pay/openapi.yaml
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod pay health` or a saved read API `tmvp --skill <dir> prod pay call @<name>`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Save the GET endpoints the code actually calls as `assets/<system>/apis/<name>.md` (front-matter `method: GET`, `path: /v1/orders/{id}`, `description:`), e.g. payment by id, subscription status, email delivery status. Write `references/<system>.md`: which code module calls which endpoint and why, test vs live mode per env. Never save mutating calls as reads.
