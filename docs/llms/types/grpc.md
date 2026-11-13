# grpc

gRPC services, with no generated code. Methods and messages come from server reflection, or from a descriptor set (`protoset:`) when the server has no reflection. Requests and responses are JSON (protojson). A method runs as a read only if it matches `read_methods`, and never if it streams from the client. Everything else runs only through `tmvp write` (`--preview` shows the request and sends nothing).

## Fields
address (`host:port`, required) · tls (`true` = TLS; default plaintext) · ca (`${file:secrets.ca}`) · server_name · insecure (skip TLS verify, test servers only) · token (secret, sent as `authorization: Bearer …` metadata) · headers (map of metadata; values may be secrets) · protoset (descriptor set file relative to the skill: `buf build -o api.binpb` or `protoc --include_imports --descriptor_set_out=api.binpb …`) · read_methods (globs on `pkg.Service/Method` or the bare method name; default `Get* List* Search* Find* Count* Describe* Check Watch Health*`) · `via:` for tunnels

## Verbs
<!-- BEGIN verbs (generated from the connector registry: go generate ./... ; do not edit) -->
| Verb | Mode | Help |
|---|---|---|
| `ls` | read | ls — saved calls (@name), then services and methods with kind (read/write) and streaming |
| `describe` | read | describe <service\|pkg.Service/Method\|message> — methods, or request/response fields |
| `health` | read | health [service] — grpc.health.v1 Check (status, latency) |
| `call` | read-write, default, @item | call <pkg.Service/Method> ['<json>'] \| call @saved [k=v ...] — read methods only as reads; others via tmvp write (--preview) |
<!-- END verbs -->

## Discovery
`ls` lists saved calls (`assets/<system>/calls/*.json`), then every service method with kind (read/write), streaming (unary/server/client/bidi) and request/response types. `describe <service|pkg.Service/Method|message>` shows the fields.

## Saved calls
```json
---
description: one order by id
method: shop.v1.OrderService/GetOrder
params: [id]
---
{"id": "{id}"}
```
Run with `tmvp prod api @get-order id=42`, or with positional values in `params` order (`@get-order 42`).

## Examples
```
tmvp prod api ls
tmvp prod api describe shop.v1.OrderService/GetOrder
tmvp all api health
tmvp all api call shop.v1.OrderService/GetOrder '{"id":"42"}' --diff
tmvp prod api call shop.v1.OrderService/ListOrders '{"status":"PAID"}' --limit 20
tmvp write prod api call shop.v1.OrderService/CancelOrder '{"id":"42"}' --reason "support #812" --preview
```
