# gh

GitHub through `gh`: workflow runs and logs, deployments, PRs, releases for one repo. tmvp sets the repo (`-R` / `GH_REPO`); a `token` goes only into the child's `GH_TOKEN`.

## Fields
repo (owner/name, required), host (GitHub Enterprise), token (secret, optional; default is the user's `gh auth login`); login: {manual: "gh auth login", check: "gh auth status"}

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls — configured repo/host, whoami, saved scripts |
| `whoami` | read | the GitHub account gh is using (login, name) |
| `runs` | read | runs [--workflow W] [--branch B] [--status S] — recent workflow runs (bounded by --limit) |
| `run` | read | run <id> — one run and its jobs (status, conclusion, times) |
| `run-log` | read | run-log <id> [--failed] — the run's log (--failed: failed steps only), line-capped |
| `workflows` | read | workflows — the repo's workflows (name, path, state) |
| `prs` | read | prs [--state open\|closed\|merged\|all] — pull requests |
| `pr` | read | pr <n> — one pull request with checks and review state |
| `releases` | read | releases — tags, names, latest/draft/prerelease, dates |
| `deployments` | read | deployments [--environment E] — GitHub deployments (environment, ref, sha, creator, time) |
| `branches` | read | branches — branch names, head sha, protected |
| `call` | read-write | call <gh args> — reads only (run/pr/issue/release/workflow/label/cache list\|view\|status\|diff\|checks, api GET); through tmvp write any allowed call |
| `rerun` | write | rerun <run-id> [--failed] — re-run a workflow run (tmvp write; --preview shows it) |
| `dispatch` | write | dispatch <workflow> [--ref R] [-f k=v ...] — trigger a workflow_dispatch run (tmvp write) |
| `cancel` | write | cancel <run-id> — cancel a running workflow run (tmvp write) |
| `cli` | write, default | cli <gh args> — any gh call that is not blocked (tmvp write; --preview shows it) |
<!-- END verbs -->

## Policy
- `call` runs reads only: `run|pr|issue|release|workflow|label|cache|variable|secret|repo … list|view|status|diff|checks`, and `api` GET (`-X POST`, `-f`/`-F`/`--input` without `-X GET` are writes).
- Never through tmvp, not even `tmvp write`: `auth`, `config`, `secret` (except `secret list`, names only), `ssh-key`, `gpg-key`, `extension`, `codespace`, `alias`, `gist`, `run download`, `repo delete|clone|fork|sync`, and `-R`/`--repo`/`--hostname` (set from config).
- `rerun`, `dispatch`, `cancel`, `cli` are writes: `tmvp write … --reason`, `--preview` first.

## Discovery
`ls` = configured repo/host + saved scripts + whoami. Then `workflows`, `runs`, `deployments`.

## Examples
```
tmvp prod gh runs --workflow deploy-prod-new.yml --limit 5
tmvp prod gh run 123456789
tmvp prod gh run-log 123456789 --failed
tmvp all gh deployments --environment production
tmvp prod gh call api repos/{owner}/{repo}/commits/main/status
tmvp write prod gh rerun 123456789 --failed --reason "flaky test" --preview
tmvp write dev gh dispatch deploy-dev-new.yml --ref main --reason "redeploy dev"
```
