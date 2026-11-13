# grpc

gRPC services (the product's own or a partner's). No generated code needed: tmvp uses server reflection, or a descriptor set.

## Find it in the codebase
- `.proto` files: `find . -name '*.proto' -not -path '*/node_modules/*'`; `buf.yaml`/`buf.gen.yaml`; generated code `*_grpc.pb.go`, `*_pb2_grpc.py`, `*.grpc-client.ts`, `@grpc/grpc-js`, `grpcio`, `io.grpc`, `tonic`.
- Servers: `grpc.NewServer(`, `reflection.Register(` (reflection on → no protoset needed), `health.NewServer(`; listen ports in code, compose `ports:`, k8s Service `port: 50051`/`grpc` names, Kamal `proxy:`.
- Clients: `grpc.Dial(`/`grpc.NewClient("host:port")`, env NAMES like `*_GRPC_ADDR`, `*_GRPC_URL`, `*_SERVICE_HOST`.
- Auth: interceptors that read `authorization` metadata → a token variable NAME.

## Questions to ask the user
- Is reflection enabled in each env (often off in prod)? If not, can we build a descriptor set from the repo's protos (`buf build -o api.binpb`)?
- Address per env, TLS or plaintext, reachable directly or only from inside a host (`via:`)?
- Which methods are safe reads? Show the `ls` list: anything named Get/List/Search/Find/Count/Describe is read by default. Ask about other read-only methods (e.g. `Lookup`, `Quote`) to add to `read_methods`, and about any Get*-named method that changes state, which must be dropped.

## Config
```yaml
api:
  type: grpc
  address: REPLACE_ME:443            # host:port
  tls: true
  token: "${secrets.api_token}"      # $file(sec get acme@production:API_TOKEN --out ${out})
  # protoset: assets/api/api.binpb   # when the server has no reflection
  read_methods: ["Get*", "List*", "Search*", "acme.v1.PriceService/Quote"]
```
Internal service bound to localhost or a private network: `via: box` (`via: [box, {container: acme-api}]` for a container).

## Prove it
`tmvp --skill <dir> <env> api health`, then `<env> api ls` (lists methods with read/write kind).

## What to write into the product skill
- `references/api.md`: services and methods with their kind (read/write), which code calls them, auth, per-env differences (reflection on/off, TLS).
- Saved calls `assets/<system>/calls/<name>.json`, taken from the reads the code makes most and from on-call questions (an order by id, a user's status, recent items). Front-matter: `description`, `method`, `params`; the body is JSON with `{param}` placeholders. Read methods only.
- If a protoset was built, commit it under `assets/<system>/` and set `protoset:`.
