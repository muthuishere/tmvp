# Secret managers

Keep secrets where your team already keeps them. In `config.yaml` a secret is a command that prints the value (`$(…)`), or writes it to a file (`$file(… ${out})`). tmvp's local server runs it, keeps the value in memory and redacts it from all output; the agent only sees the secret's name. If the tool needs a login, tmvp stops with `login_required` and names the exact command for you to run — it never asks for the secret itself.

| Tool | Install and log in (once) | In config.yaml |
|---|---|---|
| sec (encrypted local vault) | the one-line installer in the sec README; `sec set acme@production:DATABASE_URL` | `"$file(sec get acme@production:DATABASE_URL --out ${out})"` |
| LastPass | `brew install lastpass-cli` · `lpass login you@example.com` | `"$(lpass show --password acme/prod-db)"` |
| 1Password | `brew install 1password-cli` · `op signin` | `"$(op read op://acme/prod-db/url)"` |
| Bitwarden | `brew install bitwarden-cli` · `bw login`, then export `BW_SESSION` | `"$(bw get password acme-prod-db)"` |
| HashiCorp Vault | `brew install hashicorp/tap/vault` · `vault login` | `"$(vault kv get -field=url secret/acme/prod/db)"` |
| AWS Secrets Manager | AWS CLI · `aws sso login` | `"$(aws secretsmanager get-secret-value --secret-id acme/prod/db --query SecretString --output text)"` |
| Google Secret Manager | gcloud · `gcloud auth login` | `"$(gcloud secrets versions access latest --secret acme-prod-db)"` |
| Azure Key Vault | Azure CLI · `az login` | `"$(az keyvault secret show --vault-name acme --name prod-db --query value -o tsv)"` |
| Doppler | `brew install dopplerhq/cli/doppler` · `doppler login` | `"$(doppler secrets get DATABASE_URL --plain --config prd)"` |

Tips
- Prefer a read-only database login for `read.dsn`; give `write.dsn` its own secret if you allow writes at all.
- After rotating a secret: `tmvp secrets reload`.
- `tmvp secrets status` shows each slot (ok / missing / invalid) and its source, never a value.
- No secret manager yet? Start with your `.env` files: [`.env` files and environment variables](environment-variables.md).
