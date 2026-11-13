# k8s

Kubernetes via client-go (no kubectl), dialing through `via:`. Read connections refuse every non-GET request. (Being rewritten; `ls` is pending.)

## Fields
kubeconfig (`${file:secrets.x}`, read./write. override), context, namespace (`-F ns=x` per call)

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | what exists: saved manifests, then namespaces and workloads/services in the namespace (-F A=true for all) |
| `ctx` | read | context, cluster, namespace, auth kind |
| `health` | read | version, readyz, ready nodes, latency |
| `get` | read | get <kind> [name] [-F selector=app=x] [-F ns=x\|-F A=true] [-F o=yaml] |
| `describe` | read | describe <kind> <name> \| <kind>/<name> — object + its events |
| `logs` | read | logs <pod>\|deploy/<name> [-F container=c] [-F previous=true] (bounded tail, --since) |
| `events` | read | events [name] [-F type=Warning] (--since filters) |
| `top` | read | top pods\|nodes (metrics API) |
| `apply` | write, default | apply '<yaml>'\|@manifest (server-side apply; --preview = dry-run) |
| `scale` | write | scale <kind>/<name> <replicas> |
| `restart` | write | restart deploy\|sts\|ds/<name> (rollout restart) |
| `rollout` | write | rollout undo deploy/<name> [-F to-revision=N] |
| `delete` | write | delete <kind> <name> \| <kind> -F selector=… (destructive) \| ns <name> (destructive) |
| `exec` | write | exec <pod> [-F container=c] -- <cmd> [args...] |
<!-- END verbs -->

## Discovery
TODO: `ls` (namespaces/workloads) pending the client-go rewrite; until then `get ns`, `get deploy`.

## Port-forward (k8s as a `via:` hop)
A k8s system is also a tunnel: a `portforward` system with `via: <k8s system>` and
`target: kind/name:port` port-forwards (like `kubectl port-forward`) through the API server, and
any system can then use that portforward as its `via:`. `kind` is `pod`, `svc`, `deploy`, `sts`, …;
`svc/` and workload targets resolve to a Running+Ready, non-terminating pod (error `not_found` if
none); a service port maps to its `targetPort`, named ports resolve from the pod spec. The tunnel
connection is read-only except the port-forward upgrade itself. Chains work: the k8s system can
itself sit behind `via: <ssh>`.
```yaml
bastion:  { type: ssh, host: bastion.example.com, user: deploy, key: "${file:secrets.ssh_key}" }
cluster:  { type: k8s, via: bastion, kubeconfig: "${file:secrets.kubeconfig}", context: prod }
pg-fwd:   { type: portforward, via: cluster, target: svc/postgres:5432, namespace: data }
db:       { type: postgres, via: pg-fwd, read: { dsn: "${secrets.db_ro}" } }
api-fwd:  { type: portforward, via: cluster, target: deploy/api:http }
api:      { type: http, via: api-fwd, base_url: "http://api.internal" }
```
`tmvp prod db …` and `tmvp prod api get /health` then run through ssh → API server → pod.
`tmvp prod pg-fwd open` keeps a local 127.0.0.1 listener for plain psql.

## Examples
```
tmvp all cluster get pods
tmvp prod cluster logs deploy/api --since 15m --limit 200
tmvp prod cluster events
tmvp write prod cluster restart deploy/api --reason "stuck workers"
```
