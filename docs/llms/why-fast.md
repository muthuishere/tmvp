# Why tmvp is fast

| | Plain agent skill (calls `psql`, `kubectl`, `ssh` itself) | MCP server | tmvp |
|---|---|---|---|
| Process per call | a new `psql`/`ssh` every time: connect, TLS, login, fetch the password | one long-running server | one long-running local server (starts on first use, stops after `TMVP_IDLE`, default 30m) |
| Connections | rebuilt on every call | kept open | pools, ssh tunnels and container hops kept open per env/system |
| Secrets | fetched (or read from `.env`) on every call, often by the agent itself | held by the server | fetched once, held in the server's memory, never shown to the agent |
| Many environments | one call per env, usually one after another | depends on the server | one call, all envs in parallel, `--diff` returns only differences |
| Agent context cost | the skill text | every tool's schema loaded into context up front | the skill text; the agent learns verbs on demand (`tmvp verbs`, `--llmhelp`) |

So tmvp keeps the light, on-demand shape of a skill (a CLI the agent calls when it needs it) and gets the speed of an MCP server (a warm process that already holds connections and credentials). Local demo, one query on 4 environments: tmvp ≈ 17 ms per call, four separate `psql` runs ≈ 500 ms; behind ssh/jump hosts each fresh connection costs hundreds of ms to seconds, which tmvp pays once.
