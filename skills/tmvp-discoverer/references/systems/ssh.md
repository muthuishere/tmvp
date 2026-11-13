# ssh

A server reached over ssh: health, docker containers and their logs, and saved read-only scripts. Also the `via:` hop for private databases/caches. Fields and verbs: `tmvp llms types/ssh` · `tmvp verbs ssh`.

## Find it in the codebase
- Deploy config: Kamal `config/deploy*.yml` (`servers:`, `ssh: user:`, `accessories:`, `service:` = container prefix), Capistrano `config/deploy/*.rb`, Ansible `inventory*`/`hosts`, `docker-compose*.yml` deployed with `DOCKER_HOST=ssh://`.
- CI: `.github/workflows/*.yml` steps using `appleboy/ssh-action`, `ssh `, `rsync`, `kamal deploy`, secret NAMES like `SSH_HOST`, `SSH_PRIVATE_KEY`, `DEPLOY_HOST`.
- Infra: terraform `hcloud_server`, `aws_instance`, `digitalocean_droplet`; `~/.ssh/config` Host aliases the owner points you to (never read key files).

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which host per env (if not literal in deploy config)? Which ssh user?
- Which key file path reaches it (e.g. `~/.ssh/acme-prod`)? You write the path, never open it.
- Is there a bastion/jump host? (then `via: bastion` on the box)

## Config
```yaml
secrets:
  known_hosts: "$(cat ~/.ssh/known_hosts)"
  prod_key: "$(cat ~/.ssh/REPLACE_ME)"   # path only; you never open it
envs:
  prod:
    destructive: false
    systems:
      box:
        type: ssh
        host: REPLACE_ME
        user: deploy
        key: "${secrets.prod_key}"
        known_hosts: "${secrets.known_hosts}"
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod box health` · `tmvp --skill <dir> all box docker ps`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/box.md`: per env the role of the host, the container name prefixes (`docker ps` names; Kamal adds a version suffix — tmvp matches a prefix, e.g. `docker logs acme-web`), which container is web/worker/cron, where app logs go.
Save read-only diagnostics as `assets/box/scripts/<name>.sh` with front-matter `read: true` (disk usage, `docker stats --no-stream`, tail of a known log file). Prove each: `tmvp --skill <dir> dev box @<name>`.
