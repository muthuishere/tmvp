// Package postgres is the reference tmvp connector (see CONTRACT.md).
//
// Reads run on the read: credential inside BEGIN READ ONLY with
// statement/lock timeouts and a row cap. Writes (tmvp write) run on the
// write: credential; --preview executes in a transaction that is rolled back
// and reports rows affected. Destructive statements (DROP, TRUNCATE,
// DELETE/UPDATE without WHERE, ALTER ... DROP/RENAME, DO/EXECUTE/CALL,
// VACUUM FULL, CLUSTER, REVOKE) need destructive: true.
//
// Security: the read: credential MUST be an unprivileged role (SELECT only,
// no ownership, no write grants). BEGIN READ ONLY and
// default_transaction_read_only are defence in depth, not a security
// boundary: any session can SET TRANSACTION READ WRITE or call functions with
// side effects, so the database role is the only real protection.
package postgres

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muthuishere/tmvp/internal/certinfo"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() { connector.Register("postgres", New) }

// New returns an unopened postgres connector.
func New() connector.Connector { return &PG{} }

// PG is one pool for one env/system/mode.
type PG struct {
	t    *connector.Target
	pool *pgxpool.Pool
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — saved queries (@name, all asset roots) then live tables, views, matviews"},
	{Name: "health", Mode: connector.Read, Help: "ping, server version, recovery state, latency"},
	{Name: "describe", Mode: connector.Read, Help: "describe            → tables; describe <table> → columns"},
	{Name: "query", Mode: connector.Read, Saved: true, Help: `query "<sql>" [params...] | query @saved [params...] (read-only tx, row cap)`},
	{Name: "exec", Mode: connector.Write, Default: true, Help: `exec "<sql>"|@saved [params...] (tmvp write; --preview = rolled-back tx)`},
}

func (p *PG) Type() string            { return "postgres" }
func (p *PG) Verbs() []connector.Verb { return verbs }

// Open builds the pool from <mode>.dsn (or dsn) and verifies it.
func (p *PG) Open(ctx context.Context, t *connector.Target) error {
	p.t = t
	dsn, err := t.ModeString(ctx, "dsn")
	if err != nil {
		return err
	}
	cfg, err := poolConfig(dsn, t)
	if err != nil {
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return classify(err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return classify(err)
	}
	p.pool = pool
	return nil
}

// poolConfig builds the pgxpool config (DSN, tunnel dial, session reset).
func poolConfig(dsn string, t *connector.Target) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// never echo the DSN: it is a secret
		return nil, errs.New(errs.Config, "%s.dsn is not a valid postgres DSN", t.Mode)
	}
	cfg.MaxConns = 4
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 10 * time.Minute
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return t.Dial(ctx, network, addr)
	}
	cfg.ConnConfig.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
		if len(t.Via) > 0 {
			return []string{host}, nil // resolve on the far side of the tunnel
		}
		return net.DefaultResolver.LookupHost(ctx, host)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "tmvp"
	if t.Mode == connector.ModeRead {
		cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}
	cfg.AfterRelease = resetSession
	return cfg, nil
}

// resetSQL is DISCARD ALL minus DEALLOCATE ALL (pgx caches prepared
// statements per connection; deallocating them behind its back breaks it).
const resetSQL = "CLOSE ALL; SET SESSION AUTHORIZATION DEFAULT; RESET ALL; UNLISTEN *; " +
	"SELECT pg_advisory_unlock_all(); DISCARD PLANS; DISCARD SEQUENCES; DISCARD TEMP"

// resetSession is the pool's AfterRelease hook: it clears session state
// (GUCs, advisory locks, temp tables, listeners) so nothing leaks into the
// next borrower. A connection that cannot be reset is destroyed.
func resetSession(c *pgx.Conn) bool {
	if c.PgConn().TxStatus() != 'I' { // still in a transaction: don't reuse
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Exec(ctx, resetSQL)
	return err == nil
}

// Close closes the pool.
func (p *PG) Close() error {
	if p.pool != nil {
		p.pool.Close()
	}
	return nil
}

// Check implements connector.Checker.
func (p *PG) Check(ctx context.Context) error { return classify(p.pool.Ping(ctx)) }

// Do runs one verb.
func (p *PG) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	switch c.Verb {
	case "health":
		return p.health(ctx, e)
	case "ls":
		if err := p.t.EmitSaved(e, "queries", ".sql"); err != nil {
			return errs.Wrap(errs.Runtime, err, "")
		}
		return p.readQuery(ctx, c, listTables, nil, e)
	case "describe":
		return p.describe(ctx, c, e)
	case "query":
		sql, args, err := p.statement(c.Args)
		if err != nil {
			return err
		}
		return p.readQuery(ctx, c, sql, args, e)
	case "exec":
		return p.exec(ctx, c, e)
	}
	return errs.New(errs.Usage, "postgres has no verb %q", c.Verb)
}

// Guard rejects destructive raw SQL before any connection is opened.
// (@saved statements are checked again in exec once the file is read.)
func (p *PG) Guard(c connector.Call) error {
	if c.Verb != "exec" || len(c.Args) == 0 || strings.HasPrefix(c.Args[0], "@") {
		return nil
	}
	return destructiveErr(c.Args[0], c.Destructive)
}

func destructiveErr(sql string, allowed bool) error {
	if why := Destructive(sql); why != "" && !allowed {
		return errs.New(errs.PolicyBlocked, "destructive statement (%s) blocked: destructive: false here", why).
			WithHint("add a WHERE clause, or set destructive: true on the env/system if this is intended")
	}
	return nil
}

// statement resolves "<sql>" or "@saved" plus positional params.
func (p *PG) statement(a []string) (string, []any, error) {
	if len(a) == 0 {
		return "", nil, errs.New(errs.Usage, "missing SQL").WithHint(`query "select ..." or query @saved`)
	}
	sql := a[0]
	if strings.HasPrefix(sql, "@") {
		s, err := p.t.LoadSaved("queries", sql, ".sql")
		if err != nil {
			return "", nil, errs.Wrap(errs.NotFound, err, "")
		}
		sql = s.Body
	}
	args := make([]any, 0, len(a)-1)
	for _, x := range a[1:] {
		args = append(args, x)
	}
	return strings.TrimSpace(sql), args, nil
}

func (p *PG) health(ctx context.Context, e connector.Emit) error {
	start := time.Now()
	var version string
	var rec bool
	var now time.Time
	err := p.pool.QueryRow(ctx, "select version(), pg_is_in_recovery(), now()").Scan(&version, &rec, &now)
	if err != nil {
		return classify(err)
	}
	row := connector.Row{"ok": true, "version": version, "in_recovery": rec,
		"server_time": now.UTC().Format(time.RFC3339), "latency_ms": time.Since(start).Milliseconds()}
	// Report the server cert from the TLS the connection actually negotiated,
	// whatever the sslmode (prefer/allow/default included); a plain
	// connection (sslmode=disable, or prefer falling back) reports tls=false.
	if c, err := p.pool.Acquire(ctx); err == nil {
		addTLSCert(row, c.Conn().PgConn().Conn(), time.Now())
		c.Release()
	}
	return ignoreLimit(e.Row(row))
}

// certWarnDays matches the server's cert check default.
const certWarnDays = 30

// addTLSCert adds tls and, when TLS was negotiated, the leaf server cert's
// expiry to row. Only public cert metadata is read.
func addTLSCert(row connector.Row, conn net.Conn, now time.Time) {
	tc, ok := conn.(interface{ ConnectionState() tls.ConnectionState })
	if !ok {
		row["tls"] = false
		return
	}
	st := tc.ConnectionState()
	row["tls"] = st.HandshakeComplete
	if !st.HandshakeComplete || len(st.PeerCertificates) == 0 {
		return
	}
	c := certinfo.FromX509(st.PeerCertificates[0], now)
	row["cert_not_after"] = c.NotAfter.UTC().Format(time.RFC3339)
	row["cert_days_left"] = c.DaysLeft
	row["cert_subject"] = c.Subject
	row["cert_status"] = certinfo.Status(c, certWarnDays)
}

const listTables = `select n.nspname as schema, c.relname as name,
  case c.relkind when 'r' then 'table' when 'v' then 'view' when 'm' then 'matview' when 'p' then 'partitioned' when 'f' then 'foreign' end as kind,
  greatest(c.reltuples, 0)::bigint as est_rows
from pg_class c join pg_namespace n on n.oid = c.relnamespace
where c.relkind in ('r','v','m','p','f') and n.nspname not in ('pg_catalog','information_schema') and n.nspname not like 'pg_toast%'
order by 1, 2`

const listColumns = `select column_name as column, data_type as type, is_nullable = 'YES' as nullable, column_default as default
from information_schema.columns where table_schema = $1 and table_name = $2 order by ordinal_position`

func (p *PG) describe(ctx context.Context, c connector.Call, e connector.Emit) error {
	if len(c.Args) == 0 {
		return p.readQuery(ctx, c, listTables, nil, e)
	}
	schema, table := "public", c.Args[0]
	if i := strings.IndexByte(table, '.'); i >= 0 {
		schema, table = table[:i], table[i+1:]
	}
	return p.readQuery(ctx, c, listColumns, []any{schema, table}, e)
}

// readQuery runs sql in a READ ONLY transaction with timeouts, streaming rows.
func (p *PG) readQuery(ctx context.Context, c connector.Call, sql string, args []any, e connector.Emit) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return classify(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // read-only: always roll back
	if err := p.setTimeouts(ctx, tx, c); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return classify(err)
	}
	defer rows.Close()
	fds := rows.FieldDescriptions()
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return classify(err)
		}
		r := make(connector.Row, len(vals))
		for i, v := range vals {
			r[string(fds[i].Name)] = normalize(v)
		}
		if err := e.Row(r); err != nil {
			return ignoreLimit(err)
		}
	}
	return classify(rows.Err())
}

// setTimeouts sets the server-side kill to the call's effective timeout
// (connector.ServerTimeout), so --timeout raises or lowers it.
func (p *PG) setTimeouts(ctx context.Context, tx pgx.Tx, c connector.Call) error {
	st := connector.ServerTimeout(c, p.t.Limits)
	_, err := tx.Exec(ctx, fmt.Sprintf("set local statement_timeout = %d; set local lock_timeout = %d",
		st.Milliseconds(), min(st, 5*time.Second).Milliseconds()))
	return classify(err)
}

// exec runs a write: one statement (or @saved) in a transaction.
func (p *PG) exec(ctx context.Context, c connector.Call, e connector.Emit) error {
	if p.t.Mode != connector.ModeWrite {
		return errs.New(errs.PolicyBlocked, "exec needs tmvp write").WithHint(`tmvp write <env> <system> "<sql>" --reason "why"`)
	}
	sql, args, err := p.statement(c.Args)
	if err != nil {
		return err
	}
	if err := destructiveErr(sql, c.Destructive || p.t.Destructive); err != nil {
		return err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return classify(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	if err := p.setTimeouts(ctx, tx, c); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return classify(err)
	}
	row := connector.Row{"rows_affected": tag.RowsAffected(), "command": tag.String(), "preview": c.Preview}
	if c.Preview {
		if err := tx.Rollback(ctx); err != nil {
			return classify(err)
		}
		row["committed"] = false
		return ignoreLimit(e.Row(row))
	}
	if err := tx.Commit(ctx); err != nil {
		return classify(err)
	}
	row["committed"] = true
	return ignoreLimit(e.Row(row))
}

func ignoreLimit(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}

// classify maps driver errors to tmvp codes.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "28P01", "28000":
			return errs.AuthError(errors.New(pe.Message))
		case "25006":
			return errs.New(errs.PolicyBlocked, "read-only transaction: %s", pe.Message).
				WithHint(`mutations go through: tmvp write <env> <system> "<sql>" --reason "why"`)
		case "57014":
			return errs.New(errs.GuardRejected, "statement cancelled by timeout guard: %s", pe.Message).
				WithHint("narrow the query or raise limits.statement_timeout")
		case "55P03":
			return errs.New(errs.GuardRejected, "lock timeout: %s", pe.Message)
		}
		return errs.New(errs.Runtime, "postgres %s: %s", pe.Code, pe.Message).WithHint(pe.Hint)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(errs.Timeout, err, "")
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		msg, _, _ := strings.Cut(ce.Unwrap().Error(), "\n")
		return errs.New(errs.Unreachable, "cannot connect to postgres: %s", msg)
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return errs.Wrap(errs.Unreachable, err, "")
	}
	return errs.Wrap(errs.Runtime, err, "")
}

// normalize turns driver values into JSON-friendly ones.
func normalize(v any) any {
	switch x := v.(type) {
	case nil, bool, string, int16, int32, int64, int, float32, float64:
		return x
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case [16]byte:
		return fmt.Sprintf("%x-%x-%x-%x-%x", x[0:4], x[4:6], x[6:8], x[8:10], x[10:16])
	case []byte:
		if utf8.Valid(x) {
			return string(x)
		}
		return "\\x" + hex.EncodeToString(x)
	case pgtype.Numeric:
		if !x.Valid {
			return nil
		}
		if x.NaN {
			return "NaN"
		}
		if x.Int == nil {
			return "0"
		}
		f := new(big.Float).SetInt(x.Int)
		if x.Exp != 0 {
			scale := new(big.Float).SetFloat64(1)
			ten := big.NewFloat(10)
			e := x.Exp
			for ; e > 0; e-- {
				scale.Mul(scale, ten)
			}
			for ; e < 0; e++ {
				scale.Quo(scale, ten)
			}
			f.Mul(f, scale)
		}
		return f.Text('f', -1)
	case map[string]any, []any:
		return x
	case fmt.Stringer:
		return x.String()
	}
	return fmt.Sprint(v)
}
