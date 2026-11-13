// Package mysql is the tmvp MySQL/MariaDB connector.
//
// Reads run on the read: credential in a session marked
// SET SESSION TRANSACTION READ ONLY, inside START TRANSACTION READ ONLY, with
// max_execution_time and lock-wait guards and a row cap. Writes (tmvp write)
// run on the write: credential in a transaction; --preview rolls it back.
// DDL cannot be previewed (MySQL commits it implicitly) and is refused there.
// Destructive statements need destructive: true.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	driver "github.com/go-sql-driver/mysql"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() { connector.Register("mysql", New) }

// New returns an unopened mysql connector.
func New() connector.Connector { return &MySQL{} }

// MySQL is one pool for one env/system/mode.
type MySQL struct {
	t  *connector.Target
	db *sql.DB
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — saved queries (@name, all asset roots) then live tables and views of the current database"},
	{Name: "health", Mode: connector.Read, Help: "ping, server version, read_only, latency"},
	{Name: "describe", Mode: connector.Read, Help: "describe → tables of the current database; describe <[db.]table> → columns"},
	{Name: "query", Mode: connector.Read, Saved: true, Help: `query "<sql>" [params...] | query @saved [params...] (read-only tx, max_execution_time, row cap)`},
	{Name: "exec", Mode: connector.Write, Default: true, Help: `exec "<statement>"|@saved [params...] (tmvp write; --preview = rolled-back tx, not for DDL)`},
}

func (m *MySQL) Type() string            { return "mysql" }
func (m *MySQL) Verbs() []connector.Verb { return verbs }

// parseDSN accepts a go-sql-driver DSN (user:pw@tcp(host:3306)/db) or a
// mysql://user:pw@host:3306/db?param=v URL.
func parseDSN(dsn string) (*driver.Config, error) {
	if strings.HasPrefix(dsn, "mysql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return nil, errors.New("bad url")
		}
		cfg := driver.NewConfig()
		cfg.Net = "tcp"
		cfg.Addr = u.Host
		if u.Port() == "" {
			cfg.Addr = net.JoinHostPort(u.Hostname(), "3306")
		}
		if u.User != nil {
			cfg.User = u.User.Username()
			cfg.Passwd, _ = u.User.Password()
		}
		cfg.DBName = strings.TrimPrefix(u.Path, "/")
		q := u.Query()
		if q.Get("tls") != "" {
			cfg.TLSConfig = q.Get("tls")
		}
		return cfg, nil
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("bad dsn")
	}
	return cfg, nil
}

// Open builds the pool from <mode>.dsn (or dsn) and verifies it.
func (m *MySQL) Open(ctx context.Context, t *connector.Target) error {
	m.t = t
	dsn, err := t.ModeString(ctx, "dsn")
	if err != nil {
		return err
	}
	cfg, err := parseDSN(dsn)
	if err != nil {
		// never echo the DSN: it is a secret
		return errs.New(errs.Config, "%s.dsn is not a valid mysql DSN", t.Mode)
	}
	cfg.MultiStatements = false // one statement per call: no "select 1; drop ..."
	cfg.AllowAllFiles = false
	cfg.ParseTime = true
	cfg.Timeout = 10 * time.Second
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network == "unix" {
			return nil, errors.New("unix sockets are not supported through tmvp")
		}
		return t.Dial(ctx, "tcp", addr)
	}
	c, err := driver.NewConnector(cfg)
	if err != nil {
		return errs.New(errs.Config, "%s.dsn: invalid mysql settings", t.Mode)
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(10 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return classify(err)
	}
	m.db = db
	return nil
}

// Close closes the pool.
func (m *MySQL) Close() error {
	if m.db != nil {
		return m.db.Close()
	}
	return nil
}

// Check implements connector.Checker.
func (m *MySQL) Check(ctx context.Context) error { return classify(m.db.PingContext(ctx)) }

// Guard rejects destructive raw SQL and obvious read-verb mutations before
// any connection is opened.
func (m *MySQL) Guard(c connector.Call) error {
	if len(c.Args) == 0 || strings.HasPrefix(c.Args[0], "@") {
		return nil
	}
	switch c.Verb {
	case "exec":
		return destructiveErr(c.Args[0], c.Destructive)
	case "query":
		return readGate(c.Args[0])
	}
	return nil
}

func readGate(sql string) error {
	if !readOnlyStart(sql) {
		return errs.New(errs.PolicyBlocked, "query only runs read statements (SELECT/SHOW/DESCRIBE/EXPLAIN/WITH)").
			WithHint(`mutations go through: tmvp write <env> <system> "<sql>" --reason "why"`)
	}
	return nil
}

func destructiveErr(sql string, allowed bool) error {
	if why := Destructive(sql); why != "" && !allowed {
		return errs.New(errs.PolicyBlocked, "destructive statement (%s) blocked: destructive: false here", why).
			WithHint("add a WHERE clause, or set destructive: true on the env/system if this is intended")
	}
	return nil
}

// Do runs one verb.
func (m *MySQL) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	switch c.Verb {
	case "health":
		return m.health(ctx, e)
	case "ls":
		if err := m.t.EmitSaved(e, "queries", ".sql"); err != nil {
			return errs.Wrap(errs.Runtime, err, "")
		}
		return m.readQuery(ctx, c, listTables, nil, m.limit(c), e)
	case "describe":
		return m.describe(ctx, c, e)
	case "query":
		q, args, err := m.statement(c.Args)
		if err != nil {
			return err
		}
		if err := readGate(q); err != nil {
			return err
		}
		return m.readQuery(ctx, c, q, args, m.limit(c), e)
	case "exec":
		return m.exec(ctx, c, e)
	}
	return errs.New(errs.Usage, "mysql has no verb %q", c.Verb)
}

func (m *MySQL) limit(c connector.Call) int {
	if c.Limit > 0 {
		return c.Limit
	}
	if m.t != nil && m.t.Limits.Rows > 0 {
		return m.t.Limits.Rows
	}
	return connector.DefaultLimits.Rows
}

// statement resolves "<sql>" or "@saved" plus positional params.
func (m *MySQL) statement(a []string) (string, []any, error) {
	if len(a) == 0 {
		return "", nil, errs.New(errs.Usage, "missing SQL").WithHint(`query "select ..." or query @saved`)
	}
	q := a[0]
	if strings.HasPrefix(q, "@") {
		s, err := m.t.LoadSaved("queries", q, ".sql")
		if err != nil {
			return "", nil, errs.Wrap(errs.NotFound, err, "")
		}
		q = s.Body
	}
	args := make([]any, 0, len(a)-1)
	for _, x := range a[1:] {
		args = append(args, x)
	}
	return strings.TrimRight(strings.TrimSpace(q), ";"), args, nil
}

// session sets guards on a dedicated connection. MySQL uses
// max_execution_time (ms, SELECT only); MariaDB max_statement_time (s).
// The limit follows the call (connector.ServerTimeout), so --timeout moves it.
func (m *MySQL) session(ctx context.Context, conn *sql.Conn, readOnly bool, c connector.Call) error {
	st := connector.ServerTimeout(c, m.t.Limits)
	if readOnly {
		if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
			return classify(err)
		}
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET SESSION max_execution_time = %d", st.Milliseconds())); err != nil {
		_, _ = conn.ExecContext(ctx, fmt.Sprintf("SET SESSION max_statement_time = %g", st.Seconds()))
	}
	_, _ = conn.ExecContext(ctx, fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d", max(1, int(min(st, 5*time.Second).Seconds()))))
	return nil
}

func (m *MySQL) health(ctx context.Context, e connector.Emit) error {
	start := time.Now()
	var version string
	var ro int64
	var now time.Time
	if err := m.db.QueryRowContext(ctx, "SELECT VERSION(), @@global.read_only, NOW()").Scan(&version, &ro, &now); err != nil {
		return classify(err)
	}
	return ignoreLimit(e.Row(connector.Row{"ok": true, "version": version, "read_only": ro != 0,
		"server_time": now.UTC().Format(time.RFC3339), "latency_ms": time.Since(start).Milliseconds()}))
}

const listTables = `SELECT table_schema AS ` + "`schema`" + `, table_name AS name, LOWER(table_type) AS kind, table_rows AS est_rows
FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY 1, 2`

const listColumns = `SELECT column_name AS ` + "`column`" + `, column_type AS type, is_nullable = 'YES' AS nullable, column_default AS ` + "`default`" + `, column_key AS ` + "`key`" + `
FROM information_schema.columns WHERE table_schema = COALESCE(?, DATABASE()) AND table_name = ? ORDER BY ordinal_position`

func (m *MySQL) describe(ctx context.Context, c connector.Call, e connector.Emit) error {
	if len(c.Args) == 0 {
		return m.readQuery(ctx, c, listTables, nil, m.limit(c), e)
	}
	var schema any
	table := c.Args[0]
	if i := strings.IndexByte(table, '.'); i >= 0 {
		schema, table = table[:i], table[i+1:]
	}
	return m.readQuery(ctx, c, listColumns, []any{schema, table}, m.limit(c), e)
}

// readQuery runs q in a READ ONLY transaction with guards, streaming rows.
func (m *MySQL) readQuery(ctx context.Context, c connector.Call, q string, args []any, limit int, e connector.Emit) error {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return classify(err)
	}
	defer conn.Close()
	if err := m.session(ctx, conn, true, c); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}) // START TRANSACTION READ ONLY
	if err != nil {
		return classify(err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only: always roll back
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return classify(err)
	}
	defer rows.Close()
	return emitRows(rows, limit, e)
}

func emitRows(rows *sql.Rows, limit int, e connector.Emit) error {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return classify(err)
	}
	n := 0
	for rows.Next() {
		vals := make([]any, len(cts))
		ptrs := make([]any, len(cts))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return classify(err)
		}
		r := make(connector.Row, len(vals))
		for i, v := range vals {
			r[cts[i].Name()] = normalize(v, cts[i].DatabaseTypeName())
		}
		if err := e.Row(r); err != nil {
			return ignoreLimit(err)
		}
		if n++; limit > 0 && n >= limit {
			return nil
		}
	}
	return classify(rows.Err())
}

// exec runs a write: one statement (or @saved) in a transaction.
func (m *MySQL) exec(ctx context.Context, c connector.Call, e connector.Emit) error {
	if m.t.Mode != connector.ModeWrite {
		return errs.New(errs.PolicyBlocked, "exec needs tmvp write").WithHint(`tmvp write <env> <system> "<sql>" --reason "why"`)
	}
	q, args, err := m.statement(c.Args)
	if err != nil {
		return err
	}
	if err := destructiveErr(q, c.Destructive || m.t.Destructive); err != nil {
		return err
	}
	if c.Preview && isDDL(q) {
		return errs.New(errs.Unsupported, "--preview cannot roll back DDL (MySQL commits it implicitly)").
			WithHint("run without --preview once reviewed")
	}
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return classify(err)
	}
	defer conn.Close()
	if err := m.session(ctx, conn, false, c); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return classify(err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return classify(err)
	}
	n, _ := res.RowsAffected()
	row := connector.Row{"rows_affected": n, "preview": c.Preview}
	if c.Preview {
		if err := tx.Rollback(); err != nil {
			return classify(err)
		}
		row["committed"] = false
		return ignoreLimit(e.Row(row))
	}
	if err := tx.Commit(); err != nil {
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
	var te *errs.Error
	if errors.As(err, &te) {
		return err
	}
	var me *driver.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1045, 1044, 1698, 1862, 2049:
			return errs.AuthError(fmt.Errorf("mysql %d: access denied", me.Number))
		case 1792, 1290, 1836:
			return errs.New(errs.PolicyBlocked, "read-only: %s", me.Message).
				WithHint(`mutations go through: tmvp write <env> <system> "<sql>" --reason "why"`)
		case 1142, 1143, 1227:
			return errs.New(errs.PolicyBlocked, "permission denied: %s", me.Message).
				WithHint(`the read credential cannot do this; writes go through tmvp write`)
		case 3024, 1969, 1317:
			return errs.New(errs.GuardRejected, "statement cancelled by timeout guard: %s", me.Message).
				WithHint("narrow the query or raise limits.statement_timeout")
		case 1205:
			return errs.New(errs.GuardRejected, "lock wait timeout: %s", me.Message)
		}
		return errs.New(errs.Runtime, "mysql %d: %s", me.Number, me.Message)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(errs.Timeout, err, "")
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return errs.Wrap(errs.Unreachable, err, "")
	}
	var oe *net.OpError
	if errors.As(err, &oe) || errors.Is(err, driver.ErrInvalidConn) {
		return errs.Wrap(errs.Unreachable, err, "")
	}
	return errs.Wrap(errs.Runtime, err, "")
}

// normalize turns driver values into JSON-friendly ones.
func normalize(v any, dbType string) any {
	switch x := v.(type) {
	case nil, bool, string, int64, int32, int, uint64, float32, float64:
		return x
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case []byte:
		s := string(x)
		switch dbType {
		case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT", "YEAR":
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return n
			}
			if n, err := strconv.ParseUint(s, 10, 64); err == nil {
				return n
			}
		case "UNSIGNED TINYINT", "UNSIGNED SMALLINT", "UNSIGNED MEDIUMINT", "UNSIGNED INT", "UNSIGNED BIGINT":
			if n, err := strconv.ParseUint(s, 10, 64); err == nil {
				return n
			}
		case "FLOAT", "DOUBLE":
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
		}
		if utf8.Valid(x) {
			return s
		}
		return fmt.Sprintf("0x%x", x)
	case fmt.Stringer:
		return x.String()
	}
	return fmt.Sprint(v)
}
