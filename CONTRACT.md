# tmvp — Contract for connector / subcommand authors

Read this before touching code in Phase 2. `docs/DESIGN.md` is the source of truth for behaviour;
this file is the source of truth for **interfaces**. Phase 1 packages (`internal/*`, the files in
`cmd/tmvp` listed below, `connectors/postgres`) are frozen: do not edit them. If you need a change,
say so in your report — do not work around it.

## 1. Rules

- Stay in your own directories (PLAN.md). Your files in `cmd/tmvp` are only `cmd_<name>.go` and
  `conn_<type>.go`.
- `CGO_ENABLED=0`, Go 1.26. No `go mod tidy` (it would remove other agents' deps). `go get` only
  when you really need a new module, and say so in your report.
- Never put a secret value in code, tests, logs, errors or output. Tests use fake values like
  `FAKE_SECRET_123`. Live tests read their credentials from env vars (`TMVP_TEST_<TYPE>_*`) and
  `t.Skip` when those are not set; run them with `sec run NAME -- go test ... -run Live`.
- Gate: `CGO_ENABLED=0 go build ./... && go vet ./... && go test ./...` passes, and `task cross` passes.

## 2. Writing a connector

One package per type: `connectors/<type>/`. Register it in `init()` and link it into the binary
with one file, `cmd/tmvp/conn_<type>.go`:

```go
// cmd/tmvp/conn_redis.go
package main

import _ "github.com/muthuishere/tmvp/connectors/redis"
```

### 2.1 The interface (`internal/connector`)

```go
type Connector interface {
	Type() string
	Verbs() []Verb
	Open(ctx context.Context, t *Target) error   // once per product/env/system/mode; verify auth here
	Do(ctx context.Context, c Call, e Emit) error // per call; may run concurrently; honour ctx
	Close() error
}
type Factory func() Connector
func Register(typ string, f Factory)            // from init(); panics on duplicates

type VerbMode int // Read | Write | Destructive | ReadWrite
// ReadWrite: runs as a plain read AND through `tmvp write`; the connector refuses mutation
// unless Call.Write (web click/fill/do, cloud call). Mode.Readable() / Mode.Writable().
type Verb struct {
	Name    string
	Mode    VerbMode
	Help    string
	Default bool // the write verb used when `tmvp write env sys "<stmt>"` gets a non-verb
	Saved   bool // the read verb a bare `tmvp env sys @name` routes to (ssh script, pg query ...)
}

type Call struct {
	Verb        string
	Args        []string
	Flags       map[string]string // -F key=value from the CLI, connector-specific
	Reason      string
	Preview     bool          // write: show blast radius, change nothing
	Limit       int           // --limit (0 = default caps)
	Since       time.Duration // --since (0 = unset)
	Destructive bool          // env/system allows destructive ops
	Write       bool          // came through `tmvp write` (connector opened with ModeWrite)
	Timeout     time.Duration // effective hard timeout (--timeout > system > env > global limits.timeout > 10s);
	                          // pass it server-side via connector.ServerTimeout(c, t.Limits) / CallTimeout
}

type Row = map[string]any
type Emit interface {
	Row(r Row) error      // returns ErrLimit when the cap is reached → stop, return nil
	Text(line string) error
}
var ErrLimit error
```

Optional interfaces:

| Interface | When it is called |
|---|---|
| `Guard(c Call) error` | on a fresh **unopened** instance before Open — reject policy problems (destructive SQL, bad args) without touching the system or resolving secrets |
| `Check(ctx) error` | by `tmvp check` after Open |
| `Estimate(ctx, Call) (Estimate, error)` | **`Estimator`** — after Open, before Do, for every READ (unless `--force`). Return `Estimate{Duration, Rows, Bytes, Why, Confident}` from a cheap pre-run probe (EXPLAIN, explain, `_count`, index stats, series). `Duration` > `Call.Timeout` → the server refuses with `too_expensive` and Do is never called. Return `connector.ErrNoEstimate` for verbs you don't estimate; any error = "estimate unavailable" → the read runs under the hard timeout. Must be cheap and read-only; the server bounds it to ≤ 5s. Calibration rates come from `t.Limits` (`CostPerSecond`, `RowsPerSecond`, `BytesPerSecond`, `PointsPerSecond`, `SeqScanRows`; 0 = your documented default). `Why` must name the cost driver (`Seq Scan on events, ~4.2M rows`) and never a secret |
| `Login(ctx, Call, Emit) error` | by `tmvp login <env> <system>` (web manual browser login). The Call carries `Flags["user"]` |

### 2.2 The Target (what Open receives)

```go
type Target struct {
	Product, Env, System, Type string
	Mode        Mode   // ModeRead | ModeWrite — which credential to use
	Destructive bool
	Labels      map[string]string
	Via         []string
	Login       *LoginSpec // config `login: {manual, check}`; nil when absent (tmvp runs Check itself)
	Redactor    Redactor   // AddBasicAuth(user, pass): register derived secret forms (base64 basic auth)
	Limits      Limits // Rows 1000, Lines 500, Bytes 1 MiB, Timeout 10s, StatementTimeout 15s + cost calibration (config `limits:` global < env < system)
	Dir         string // skill dir; assets in Dir/assets/<System>
	Dialer      Dialer // honours via:. ALWAYS dial through it (t.Dial)
}
func (t *Target) String(ctx, path) (string, error)     // "host", "read.dsn", "users.ops.password"; secrets resolved lazily
func (t *Target) StringOr(ctx, path, def) (string, error)
func (t *Target) ModeString(ctx, key) (string, error)   // "<mode>.<key>" then "<key>"; write mode never falls back to read.*
func (t *Target) Raw(path) (any, bool)                  // unresolved value (maps, lists, numbers)
func (t *Target) Has(path) bool
func (t *Target) File(ctx, path) ([]byte, error)        // "${file:secrets.x}" → bytes in memory (kubeconfig, key, SA json)
func (t *Target) FilePath(ctx, path) (string, func(), error) // only for external CLIs: memfd on Linux, else 0600 temp; ALWAYS call cleanup
func (t *Target) Dial(ctx, network, addr) (net.Conn, error)
func (t *Target) AssetDirs() []string                              // <root>/<system> per asset root (config `assets:`), lookup order
func (t *Target) AssetsDir() string                                // first of AssetDirs
func (t *Target) LoadSaved(kind, name, ext string) (*Saved, error) // first <root>/<system>/<kind>/<name><ext>, front-matter split
func (t *Target) ListSaved(kind, ext string) ([]*Saved, error)     // all items across roots (first wins), sorted
func (t *Target) EmitSaved(e Emit, kind, ext string) error         // ls rows: kind, name (@x), description, source
```

Fields are whatever the user wrote under the system in `config.yaml` (except `type`, `via`,
`labels`, `login`, `destructive`, which are parsed by tmvp). Read sub-maps are `read.*`, write
sub-maps are `write.*`. Built-in connectors use `File` (bytes in memory) — use `FilePath` only when
handing a path to an external CLI, and only for the duration of that call.

### 2.3 Behaviour every connector must have

- **`ls` (Read)** — "what is available": list what exists, live where possible (tables, collections,
  prefixes, streams, metrics, branches …) plus saved items via `t.EmitSaved(e, kind, ext)` /
  `t.ListSaved` (all asset roots, first match wins, rows carry `source`). Bounded like any read.
  Prefer rows with `kind` and `name`. `cmd/tmvp` tests fail for a type without it (k8s: pending).
- Saved items: always `t.LoadSaved` / `t.ListSaved` / `t.AssetDirs()` — never build
  `Dir/assets/<system>` paths yourself (config `assets:` may list several roots).

- **Read verbs cannot mutate.** Use the `read:` credential and a read-only session where the
  system has one (pg `BEGIN READ ONLY`, k8s view context, mongo secondary-preferred reads ...).
  A mutation attempted through a read verb → `errs.PolicyBlocked` with a hint naming `tmvp write`.
- **Write verbs** run only via `tmvp write` (the server enforces mode + `--reason` + single target).
  Implement `Preview` where it is cheap (rolled-back tx, dry-run, server-side `--dry-run=server`).
- **Destructive**: mark verbs `Destructive` when the verb itself is destructive (purge, flush,
  delete ns). When destructiveness depends on the arguments (SQL, mongo `deleteMany({})`), detect it
  in `Guard` and again in `Do`, refusing unless `Call.Destructive`.
- **Guards**: bound everything (per-call budget `connector.CallTimeout(c, t.Limits)`, server-side
  limit `connector.ServerTimeout(c, t.Limits)` — never bake `t.Limits.Timeout` into a client, since
  `--timeout` may raise it for one call; row/line caps, bounded consume/tail).
  Stop when `Emit` returns `ErrLimit`. Honour `ctx`.
- **Errors**: return `*errs.Error` (`internal/errs`). Credential rejected → `errs.AuthError(err)`;
  the server then re-runs the secrets the target used and retries **once** (same value →
  `secret_invalid`). Never put a DSN/URL/token in an error message (the redactor is a backstop,
  not a licence).
- **Output**: rows are flat `map[string]any` with JSON-friendly values (string, number, bool, nil,
  RFC3339 strings for time). The batch adds `env` and `system` — do not set them.

### 2.4 Error codes (`internal/errs`) and exit codes

| Code | Exit | Use |
|---|---|---|
| `runtime` | 1 | anything unexpected |
| `timeout` | 1 | deadline hit |
| `unreachable` | 1 | cannot connect |
| `auth_error` | 1 | credential rejected (`errs.AuthError`) → server invalidates + retries once |
| `unsupported` | 1 | type/tunneler/feature not available |
| `config_error`, `usage`, `not_found` | 2 | bad config / args |
| `secret_missing`, `secret_invalid` | 2 | secret slot failed / rejected after refetch |
| `login_required` | 2 | manual login needed (hint names the command) |
| `policy_blocked` | 3 | read-only violation, missing `--reason`, destructive not allowed, multi-target write |
| `partial` | 4 | some targets failed (set by the batch, not by connectors) |
| `guard_rejected` | 5 | guard cut it off (statement timeout, scan limit) |

Constructors: `errs.New(code, fmt, ...)`, `errs.Wrap(code, err, fmt, ...)`, `.WithHint(h)`,
`errs.AuthError(err)`, `errs.As(err)` (converts: never nil for a non-nil err — use it for
reporting), `errs.From(err)` (the *Error in the chain or nil), `errs.Is(err, codes...)` ("is already
a tmvp error [with one of these codes]"), `errs.CodeOf(err)`.

**Auth retry:** the server re-fetches secrets and retries once only when the auth error came from
Open or the call is a read — a write that reached the system is never re-run.

**Never put a secret on a child process's argv.** Pass it via a 0600 file the CLI reads itself
(az `--password=@file`, playwright-cli `run-code --filename`), env, or stdin.

CLI-backed connectors share `connectors/internal/cli` (+ `cli/clitest` fakes): Find, Output/Lines
(bounded), Policy/Classify (read/mutating/reveals-secret), `WriteCall` (call→cli in write).

## 3. Registering a tunneler (`via:`)

ssh / portforward / k8s port-forward hops implement:

```go
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}
type Tunnel interface {
	Dialer
	Close() error
}
type Tunneler interface {
	OpenTunnel(ctx context.Context, hop *Target, parent Dialer) (Tunnel, error)
}
func RegisterTunneler(typ string, t Tunneler) // from init(), e.g. RegisterTunneler("ssh", sshTunneler{})
```

- `hop` is the hop system's Target (mode read; resolve `host`, `user`, `key` via `hop.String/File`).
- Dial the hop itself through `parent` (that is how chains work).
- ssh tunnel: `DialContext(addr)` dials `addr` from the ssh host (ProxyJump semantics).
  portforward tunnel: ignore `addr`, connect to its fixed `remote:` through its own via.
- The server resolves `via: [a, b]` in order (a's own via first), opens each hop once per
  product/env/chain, shares it between systems, and closes it on reload/idle. Do not cache tunnels yourself.
- A type may be both a connector and a tunneler (ssh: verbs `health`, `@script`; tunnel for `via`).
  Register both from the same package.

## 4. Adding a subcommand

Create `cmd/tmvp/cmd_<name>.go`; never edit another `cmd_*.go`:

```go
package main

func init() {
	addCommand(func() *cobra.Command {
		return &cobra.Command{
			Use: "export pg-service|kubeconfig [env]", Short: "...",
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig() // lookup order + --skill/--config
				if err != nil {
					return err // *errs.Error → printed + exit code
				}
				// local work, or forward to the server:
				req := baseRequest(protocol.OpRun, cfg)
				req.Env, req.System, req.Verb, req.Args = ...
				return send(cmd.Context(), req) // streams frames, returns exit code
			},
		}
	})
}
```

Helpers in `main.go`: `globals` (flags `--skill --config --json --diff --limit --since --reason -l
--timeout -F`; `--llmhelp` is intercepted before cobra — add your command's examples to `llmDocs` in `cmd_llms.go`), `loadConfig()`, `baseRequest(op, cfg)`, `send(ctx, req)`, `exitCode(n)`, `stdout`,
`stderr`. Top-level words used by Phase 1: `write validate config check logs login secrets server
version`. Also reserved: `new skill export verbs envs systems llms`. If you add another top-level word,
add it to `config.DefaultReserved` via your report (lead edits it), so no env/system can shadow it.

Web verbs (`goto`, `click`, ...) do not need a subcommand: they are connector verbs and go through
the normal read dispatch `tmvp [envs] <web-system> <verb>` / `tmvp write <env> <web> click users.ban`.

## 5. Read dispatch grammar (for reference)

```
tmvp <envsel> <system> <verb> args     prod | prod,staging | all | prod*
tmvp <env.system> <verb> args          prod.db | prod.* | *.db
tmvp <system> <verb> args              envs omitted = all
tmvp <envsel> <verb> args -l k=v       every system matching the labels
tmvp write <env> <system> <verb|stmt> args --reason "..." [--preview]
tmvp <envsel> <system> @name args      bare saved item → the type's Saved verb (reads);
                                       in write → the Default write verb
tmvp verbs [type...]                   list real verbs + modes
```

## 6. Config facts you can rely on

- Interpolation happens after YAML parsing: `${vars.x}`, `${secrets.x}`, `${file:secrets.x}`
  (whole field only), `${user.x}` (left verbatim for web), `${NAME}`, `${NAME:-default}`, `$$` = `$`.
- Inline `"$(cmd)"` / `"$file(cmd)"` in any field becomes an anonymous env-scoped secret
  (`${secrets._<system>.<path>}`); connectors never see commands.
- `tmvp validate` rejects literal values in secret-ish fields: `dsn password passwd token key creds
  kubeconfig credentials secret uri api_key apikey private_key client_secret access_key secret_key`
  and any URL with `user:password@`. Name your secret fields accordingly so the check covers them.
- `labels`, `via`, `login: {manual, check}`, `destructive` are handled by tmvp, not by connectors.
- Login checks run before Open (cached 5 min). A connector never prompts.

## 7. Tests

- Unit tests with fakes; no network, no containers. `connector.StaticFields{...}` builds a Target
  from literal values: `connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{"read": map[string]any{"dsn": "FAKE"}})`.
  `connector.Collector` records emitted rows/lines.
- A `TestLive...` per connector reading `TMVP_TEST_<TYPE>_*` from the environment, skipped when absent.
- Required tests per connector: verb table, read-cannot-mutate path, destructive detection (if
  argument-dependent), error mapping to codes (auth → `auth_error`), no secret in any error string.
- End-to-end engine tests can register a fake type in a `_test.go` (see `internal/server/server_test.go`).
- `TMVP_NO_SERVER=1` runs the engine in-process (no socket, no auto-start) — use it for CLI tests.
  `TMVP_HOME`/`TMVP_SOCKET` relocate state (unix socket paths must stay under 104 bytes on macOS).
