package mysql

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func TestDestructive(t *testing.T) {
	cases := map[string]string{
		"select * from users":                                  "",
		"DROP TABLE users":                                     "DROP",
		"truncate users":                                       "TRUNCATE",
		"delete from users":                                    "DELETE without WHERE",
		"DELETE FROM users WHERE id = 1":                       "",
		"update users set a = 1":                               "UPDATE without WHERE",
		"update users set a = 1 where id = 2":                  "",
		"alter table users drop column x":                      "ALTER ... DROP",
		"alter table users add column x int":                   "",
		"select 'drop table x; delete from y'":                 "",
		"select 1 -- delete from users\n":                      "",
		"select 1 # truncate t\n":                              "",
		"/* truncate x */ select 1":                            "",
		"/*!40000 truncate x */":                               "TRUNCATE",
		"select * from t for update":                           "",
		"insert into t values (1) on duplicate key update v=1": "",
		"update `where` set a = 1":                             "UPDATE without WHERE",
		`select "it\'s; drop table x"`:                         "",
		"WITH d AS (SELECT id FROM t) DELETE FROM t":           "DELETE without WHERE",
		"delete from t where id in (select id from (select id from u) x where 1=1)": "",
		"update t join (select id from u where a=1) s on s.id=t.id set t.v=1":       "UPDATE without WHERE",
		"call wipe()":                           "CALL",
		"prepare s from 'delete from users'":    "PREPARE",
		"execute s":                             "EXECUTE",
		"rename table users to old_users":       "RENAME",
		"alter table users rename to old_users": "ALTER ... RENAME",
		"revoke select on db.* from 'app'@'%'":  "REVOKE",
		"/*!50000 call wipe() */":               "CALL",
	}
	for sql, want := range cases {
		if got := Destructive(sql); got != want {
			t.Errorf("Destructive(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestReadGate(t *testing.T) {
	ok := []string{"select 1", "  SHOW TABLES", "describe t", "explain select 1", "with x as (select 1) select * from x", "/* c */ select 1"}
	bad := []string{"insert into t values (1)", "create table x(a int)", "update t set a=1 where id=1", "select * from t into outfile '/tmp/x'", "set global x=1", ""}
	for _, s := range ok {
		if err := readGate(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range bad {
		if errs.CodeOf(readGate(s)) != errs.PolicyBlocked {
			t.Errorf("%q not blocked", s)
		}
	}
}

func TestVerbsAndRegistry(t *testing.T) {
	f, ok := connector.Lookup("mysql")
	if !ok {
		t.Fatal("not registered")
	}
	vs := f().Verbs()
	for _, n := range []string{"health", "describe", "query", "exec"} {
		if _, ok := connector.FindVerb(vs, n); !ok {
			t.Errorf("missing verb %s", n)
		}
	}
	if dv, _ := connector.DefaultWriteVerb(vs); dv.Name != "exec" {
		t.Fatal("default write verb")
	}
}

func TestGuard(t *testing.T) {
	m := &MySQL{}
	if err := m.Guard(connector.Call{Verb: "exec", Args: []string{"drop table x"}}); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("%v", err)
	}
	if err := m.Guard(connector.Call{Verb: "exec", Args: []string{"drop table x"}, Destructive: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Guard(connector.Call{Verb: "query", Args: []string{"delete from x where id=1"}}); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("read verb mutation: %v", err)
	}
}

func TestSavedQuery(t *testing.T) {
	dir := t.TempDir()
	qd := filepath.Join(dir, "assets", "db", "queries")
	_ = os.MkdirAll(qd, 0o700)
	_ = os.WriteFile(filepath.Join(qd, "signups.sql"), []byte("---\nname: signups\n---\nselect count(*) from users where created_at > ?;\n"), 0o600)
	m := &MySQL{t: connector.NewTarget(connector.Target{System: "db", Dir: dir}, connector.StaticFields{})}
	q, args, err := m.statement([]string{"@signups", "2026-01-01"})
	if err != nil || q != "select count(*) from users where created_at > ?" || len(args) != 1 {
		t.Fatalf("%q %v %v", q, args, err)
	}
	if _, _, err := m.statement([]string{"@../../etc/passwd"}); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestParseDSN(t *testing.T) {
	c, err := parseDSN("mysql://u:FAKE_SECRET_123@db.example:3307/app")
	if err != nil || c.Addr != "db.example:3307" || c.User != "u" || c.DBName != "app" {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = parseDSN("mysql://u@db.example/app")
	if err != nil || c.Addr != "db.example:3306" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := parseDSN("u:FAKE_SECRET_123@tcp(h:3306)/app"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenBadDSNDoesNotEchoIt(t *testing.T) {
	m := &MySQL{}
	tg := connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{"read": map[string]any{"dsn": "FAKE_SECRET_123@@not(a dsn"}})
	err := m.Open(context.Background(), tg)
	if err == nil || strings.Contains(err.Error(), "FAKE_SECRET_123") {
		t.Fatalf("%v", err)
	}
}

func TestExecRefusedInReadMode(t *testing.T) {
	m := &MySQL{t: connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{})}
	err := m.exec(context.Background(), connector.Call{Verb: "exec", Args: []string{"update x set a=1 where id=1"}}, &connector.Collector{})
	if errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatal(err)
	}
}

func TestPreviewRefusesDDL(t *testing.T) {
	m := &MySQL{t: connector.NewTarget(connector.Target{Mode: connector.ModeWrite}, connector.StaticFields{})}
	err := m.exec(context.Background(), connector.Call{Verb: "exec", Preview: true, Args: []string{"create table x(a int)"}}, &connector.Collector{})
	if errs.CodeOf(err) != errs.Unsupported {
		t.Fatal(err)
	}
}

func TestClassify(t *testing.T) {
	cases := map[uint16]errs.Code{1045: errs.Auth, 1792: errs.PolicyBlocked, 3024: errs.GuardRejected, 1142: errs.PolicyBlocked, 1064: errs.Runtime}
	for n, want := range cases {
		err := classify(&driver.MySQLError{Number: n, Message: "Access denied for user 'u' (using password: YES)"})
		if errs.CodeOf(err) != want {
			t.Errorf("%d → %v, want %s", n, err, want)
		}
	}
	if errs.CodeOf(classify(context.DeadlineExceeded)) != errs.Timeout {
		t.Error("deadline")
	}
	if e := classify(errs.New(errs.Usage, "x")); errs.CodeOf(e) != errs.Usage {
		t.Error("tmvp errors pass through")
	}
}

// TestLive: TMVP_TEST_MYSQL_DSN (read-write test DB). sec run TMVP_TEST_MYSQL_DSN -- go test ./connectors/mysql -run Live
func TestLive(t *testing.T) {
	dsn := os.Getenv("TMVP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TMVP_TEST_MYSQL_DSN not set")
	}
	ctx := context.Background()
	open := func(mode connector.Mode) *MySQL {
		m := &MySQL{}
		tg := connector.NewTarget(connector.Target{Env: "test", System: "db", Mode: mode}, connector.StaticFields{string(mode): map[string]any{"dsn": dsn}})
		if err := m.Open(ctx, tg); err != nil {
			t.Fatal(err)
		}
		return m
	}
	r := open(connector.ModeRead)
	defer r.Close()
	var c connector.Collector
	if err := r.Do(ctx, connector.Call{Verb: "health"}, &c); err != nil || len(c.Rows) != 1 {
		t.Fatalf("health: %v", err)
	}
	c = connector.Collector{}
	if err := r.Do(ctx, connector.Call{Verb: "query", Args: []string{"select 1 as one, now() as t"}}, &c); err != nil || c.Rows[0]["one"] != int64(1) {
		t.Fatalf("%v %+v", err, c.Rows)
	}
	w := open(connector.ModeWrite)
	defer w.Close()
	if err := w.Do(ctx, connector.Call{Verb: "exec", Args: []string{"create table if not exists tmvp_live(x int)"}}, &c); err != nil {
		t.Fatal(err)
	}
	// read-only session: a write through a read transaction fails server-side
	err := r.readQuery(ctx, connector.Call{}, "insert into tmvp_live values (1)", nil, 10, &c)
	if errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("read-only not enforced: %v", err)
	}
	c = connector.Collector{}
	if err := w.Do(ctx, connector.Call{Verb: "exec", Preview: true, Args: []string{"insert into tmvp_live values (1)"}}, &c); err != nil || c.Rows[0]["committed"] != false {
		t.Fatalf("preview: %v %+v", err, c.Rows)
	}
	_ = w.Do(ctx, connector.Call{Verb: "exec", Destructive: true, Args: []string{"drop table tmvp_live"}}, &c)
}

const explainJoin = `{
  "query_block": {
    "select_id": 1,
    "cost_info": {"query_cost": "245123.40"},
    "ordering_operation": {
      "using_filesort": true,
      "grouping_operation": {
        "using_temporary_table": true,
        "nested_loop": [
          {"table": {"table_name": "orders", "access_type": "ALL", "rows_examined_per_scan": 1200000,
            "rows_produced_per_join": 120000, "filtered": "10.00",
            "cost_info": {"read_cost": "110000.00", "eval_cost": "12000.00", "prefix_cost": "122000.00", "data_read_per_join": "1G"},
            "used_columns": ["id", "customer_id", "total"], "attached_condition": "(orders.total > 100)"}},
          {"table": {"table_name": "customers", "access_type": "eq_ref", "possible_keys": ["PRIMARY"], "key": "PRIMARY",
            "used_key_parts": ["id"], "key_length": "4", "ref": ["shop.orders.customer_id"],
            "rows_examined_per_scan": 1, "rows_produced_per_join": 120000, "filtered": "100.00",
            "cost_info": {"read_cost": "30000.00", "eval_cost": "12000.00", "prefix_cost": "164000.00", "data_read_per_join": "90M"}}}
        ]
      }
    }
  }
}`

const explainPK = `{"query_block": {"select_id": 1, "cost_info": {"query_cost": "1.00"},
  "table": {"table_name": "users", "access_type": "const", "possible_keys": ["PRIMARY"], "key": "PRIMARY",
    "rows_examined_per_scan": 1, "rows_produced_per_join": 1, "filtered": "100.00"}}}`

func TestParseExplain(t *testing.T) {
	e, err := parseExplain([]byte(explainJoin), connector.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	// max(245123.4/1e5 = 2.45s, 1200001/1e6 = 1.2s)
	if e.Why != "full scan on orders, ~1.2M rows" || e.Rows != 1200001 || !e.Confident ||
		e.Duration < 2450*time.Millisecond || e.Duration > 2460*time.Millisecond {
		t.Fatalf("%+v", e)
	}
	// rows dominate when the cost rate is calibrated high
	e, _ = parseExplain([]byte(explainJoin), connector.Limits{CostPerSecond: 1e9, RowsPerSecond: 1e5})
	if e.Duration < 12*time.Second {
		t.Fatalf("%+v", e)
	}
	e, _ = parseExplain([]byte(explainPK), connector.Limits{})
	if e.Why != "query_cost 1" || e.Rows != 1 || e.Duration > time.Millisecond {
		t.Fatalf("%+v", e)
	}
	if _, err := parseExplain([]byte(`{"x":1}`), connector.Limits{}); err == nil {
		t.Fatal("bad explain accepted")
	}
}

func TestEstimateNotForOtherVerbs(t *testing.T) {
	m := &MySQL{}
	for _, v := range []string{"health", "ls", "describe", "exec"} {
		if _, err := m.Estimate(context.Background(), connector.Call{Verb: v}); !errors.Is(err, connector.ErrNoEstimate) {
			t.Fatalf("%s: %v", v, err)
		}
	}
}
