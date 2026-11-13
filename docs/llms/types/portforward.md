# portforward

A fixed tunnel to one remote endpoint through its own `via:` (ssh or k8s); usable as a `via:` hop.

## Fields
remote (host:port) or target (svc/x:port) + namespace for k8s, via, local_port

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls — what this hop reaches: remote, via chain, local listener (no dial) |
| `status` | read | dial the remote through the tunnel; latency, listener |
| `open` | read | open a local 127.0.0.1 listener on local_port: or the tmvp port convention (= export pg-service) [-F port=N] (kept while the server keeps the system warm) |
| `close` | read | close the local listener |
<!-- END verbs -->

## Discovery
`ls` = remote, via chain, listener, default local port (no dial).

## Examples
```
tmvp prod db-tunnel ls
tmvp prod db-tunnel status
tmvp prod db-tunnel open   # then: psql service=acme-prod-db (tmvp export pg-service)
```
