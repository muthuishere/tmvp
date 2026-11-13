# Examples: an agent diagnosing across environments

Three reproducible incidents on a demo app with four environments (dev, dev2, staging, prod), each tried by Claude Haiku **with tmvp** and by Haiku, Opus and Fable with only plain `.env` files. Full setup, prompts and numbers: `examples/agent-benchmark/README.md` in the repo.

| Scenario | Planted fault | Haiku + tmvp | Haiku `.env` | Fable `.env` | Opus `.env` |
|---|---|---|---|---|---|
| Orders stuck after payment (prod only) | webhook secret version differs in prod | 32s · 0 leaks | 44s · 8 leaks | 47s · 0 | 159s · 0 |
| Refunds page fails (staging only) | staging missed a migration | 30s · 0 | 33s · 6 | 163s · 0 | 229s · 0 |
| EU emails silently fail (prod only) | trailing space in 1 of ~300 config values | 47s · 0 | 72s · 9 | 53s · 2 | 41s · 0 |

"Leaks" = times the database password appeared in the agent's session log. Every agent found every cause.

What it shows:
- **A process, not a smarter model.** The skill gives the agent the map of environments; one read-only call reads all of them and `--diff` returns only what differs. That gets even a small model to the root cause.
- **Secrets stay out of the agent.** Without tmvp, Haiku leaked the password in every scenario and Fable once (it dumped the whole repo, `.env` included). With tmvp the config says `$dotenv(.env.prod, PGPASSWORD)`; the tmvp server reads the value, the agent never does.
- **Writes stay deliberate.** Asked to "fix it now", the `.env` agent could have run `UPDATE` on prod with the plain password; the tmvp agent can only write through `tmvp write … --reason` (`--force` for protected envs, `--preview` first), recorded in `tmvp logs --writes`.

The typical agent session with tmvp:
```
tmvp envs                                                  # what exists
tmvp all db query "select count(*) from orders where status='pending_payment'" --diff
tmvp all db query "select key, value, length(value) from app_config" --diff
tmvp prod app @logs 1h                                      # saved read-only script
```
