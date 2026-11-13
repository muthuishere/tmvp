# ssh

SSH host and `via:` tunneler (also the `{container: name}` hop and read-only docker). Commands run under `nice`, timeout and output caps; the key is parsed in memory.

## Fields
host, port (22), user, key (`${file:secrets.x}`), passphrase, password, host_key / known_hosts (or insecure: true); read.*/write.* per mode

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls — saved scripts across all asset roots: @name, read\|write, description, source; plus running containers (name, image, status, ports) |
| `health` | read | connect, uname, uptime, latency |
| `docker` | read | docker ps [-a] \| logs <c> [--tail N] [--since X] [--timestamps] \| inspect <c> (env masked) \| stats [c] \| top <c> \| images \| compose ps \| compose logs <svc> (-F project=<dir> \| -F file=<path>) — allow-list only |
| `script` | read, @item | script @name [args...] [-F container=<c>] — saved script declared read (assets/<system>/scripts/<name>.sh); in a container via docker exec -i |
| `run` | write, default | run "<command>"\|@name [args...] [-F container=<c>] (tmvp write; nice + timeout + output caps; in a container via docker exec) |
<!-- END verbs -->

## Discovery
`ls` = saved scripts (`scripts/*.sh`) with read/write mode (reads need front-matter `read: true`),
plus running containers (kind `containers`: name, image, status, ports) when the ssh user can use docker.

## Docker (no docker CLI or API needed locally — everything runs over ssh)
- `docker` (read) is an allow-list; anything else, and any flag not listed, is rejected:
  `ps [-a]`, `logs <c> [--tail N] [--since X] [--timestamps]` (`--limit`/`--since` of the call fill in),
  `inspect <c>` (Config.Env values masked as `KEY=***`), `stats [c]` (always `--no-stream`), `top <c>`,
  `images`, `compose ps`, `compose logs <svc>` (with `-F project=<dir>` or `-F file=<compose.yml>`).
- Inside a container: `script @name -F container=api` (read; script must declare read) pipes the saved
  script into `docker exec -i api sh -s -- args`; `tmvp write ... "<cmd>" -F container=api --reason ...`
  runs `docker exec api sh -c '<cmd>'` (same write policy: --reason, --force on protected envs).
- Container hop: a system reaches a container's port through ssh with
  `via: [box, {container: pg}]` (or `{container: pg, network: backend}`, optional `sudo: true` → `sudo -n docker`).
  The hop must directly follow an ssh system and be last. On each new connection tmvp runs
  `docker inspect` over ssh to get the container IP (cached ≤3s; compose recreate changes it) and dials
  `<ip>:<port of the target's host:port>` from the ssh host; the target's host name is ignored.
  Errors: container not found, not running, docker not permitted (add the ssh user to the `docker` group).
- Container/service/network names must match `^[A-Za-z0-9][A-Za-z0-9_.-]*$`.

## Examples
```
tmvp all bastion ls
tmvp all bastion health
tmvp prod bastion @disk
tmvp write prod bastion "systemctl restart api" --reason "deploy stuck"
tmvp prod box docker ps -a
tmvp prod box docker logs api --tail 200 --since 15m
tmvp prod box docker compose logs web -F project=/srv/app
tmvp prod box @disk -F container=api
tmvp write prod box "rm -f /tmp/cache/*" -F container=api --reason "stale cache" --force
```
Container hop config:
```yaml
box: { type: ssh, host: db.internal.example, user: deploy, key: "${file:secrets.box_key}", host_key: "ssh-ed25519 AAAA..." }
db:  { type: postgres, via: [box, {container: pg}], read: { dsn: "${secrets.db_ro}" } }   # dsn host ignored, port used
```
