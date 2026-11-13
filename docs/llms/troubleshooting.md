# Troubleshooting

| Symptom | Do |
|---|---|
| `no config found` | pass `--skill <dir>` or `--config <file>`; `tmvp config path` |
| `"x" is neither an env nor a system` | `tmvp envs`, `tmvp systems` |
| `unreachable` | VPN/tunnel? `tmvp <env> <tunnel> status`; `tmvp check <env> <system>` |
| `secret_missing` | the `$(…)` command failed for you (no access/not logged in to the vault); fix access, `tmvp secrets reload` |
| `login_required` | show the user the hint's command (e.g. `! gcloud auth login`, `tmvp login prod admin`), then retry |
| `unsupported … not on PATH` | install the CLI (aws, gcloud, az, git, playwright-cli) |
| `policy_blocked` | it mutates: use `tmvp write <env> <system> … --reason "…"` if the user asked |
| `guard_rejected` / truncated | narrow the query, add filters, `--limit`, or raise `limits:` |
| stale results after editing config | the server reloads on the next call; force with `tmvp server stop` |
| saved item not found | `tmvp <env> <system> ls` shows which asset roots/items exist |
| everything slow on first call | cold start: secrets run + connections open once, then warm |

`tmvp check` validates the config, resolves secrets (names only) and connects to every selected
system. `tmvp validate` and `tmvp skill lint` never run a secret command.
