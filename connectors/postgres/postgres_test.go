package postgres

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func TestDestructive(t *testing.T) {
	cases := map[string]string{
		"select * from users":                                                        "",
		"DROP TABLE users":                                                           "DROP",
		"truncate users":                                                             "TRUNCATE",
		"delete from users":                                                          "DELETE without WHERE",
		"DELETE FROM users WHERE id = 1":                                             "",
		"update users set a = 1":                                                     "UPDATE without WHERE",
		"update users set a = 1 where id = 2":                                        "",
		"alter table users drop column x":                                            "ALTER ... DROP",
		"alter table users add column x int":                                         "",
		"select 'drop table x; delete from y'":                                       "",
		"select 1 -- delete from users\n":                                            "",
		"/* truncate x */ select 1":                                                  "",
		"select * from t for update":                                                 "",
		"insert into t values (1) on conflict (id) do update set v = 1":              "",
		"with x as (select 1) delete from users":                                     "DELETE without WHERE",
		"update t set a=1 where id=1; delete from t":                                 "DELETE without WHERE",
		`select $$ drop table x $$`:                                                  "",
		`select $tag$ truncate y $tag$`:                                              "",
		`update "where" set a = 1`:                                                   "UPDATE without WHERE",
		"WITH d AS (DELETE FROM users RETURNING id) SELECT * FROM d WHERE 1=1":       "DELETE without WHERE",
		"with d as (update t set a=1 returning id) select * from d where id = 1":     "UPDATE without WHERE",
		"with d as (delete from t where id = 1 returning id) select * from d":        "",
		"delete from t where id in (select id from u where x = 1)":                   "",
		"delete from t using (select id from u where x = 1) s":                       "DELETE without WHERE",
		"with d as (delete from t returning id) select ')' , $$)$$ from d where 1=1": "DELETE without WHERE",
		"DO $$ BEGIN DROP TABLE users; END $$":                                       "DO",
		"do language plpgsql $x$ begin delete from users; end $x$":                   "DO",
		"EXECUTE stmt(1)":                                                            "EXECUTE",
		"call wipe_everything()":                                                     "CALL",
		"alter table users rename to old_users":                                      "ALTER ... RENAME",
		"vacuum full users":                                                          "VACUUM FULL",
		"vacuum analyze users":                                                       "",
		"cluster users using users_pkey":                                             "CLUSTER",
		"revoke select on users from app":                                            "REVOKE",
	}
	for sql, want := range cases {
		if got := Destructive(sql); got != want {
			t.Errorf("Destructive(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestPoolResetsSessionOnRelease(t *testing.T) {
	cfg, err := poolConfig("postgres://u:FAKE_SECRET_123@127.0.0.1:1/db", &connector.Target{Mode: connector.ModeRead})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AfterRelease == nil {
		t.Fatal("AfterRelease hook not configured")
	}
	for _, want := range []string{"RESET ALL", "pg_advisory_unlock_all()", "DISCARD TEMP", "UNLISTEN *"} {
		if !strings.Contains(resetSQL, want) {
			t.Errorf("resetSQL missing %q", want)
		}
	}
	if strings.Contains(resetSQL, "DEALLOCATE") || strings.Contains(resetSQL, "DISCARD ALL") {
		t.Error("resetSQL must not deallocate pgx's cached prepared statements")
	}
}

func TestVerbsAndRegistry(t *testing.T) {
	f, ok := connector.Lookup("postgres")
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
	p := &PG{}
	if err := p.Guard(connector.Call{Verb: "exec", Args: []string{"drop table x"}}); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("%v", err)
	}
	if err := p.Guard(connector.Call{Verb: "exec", Args: []string{"drop table x"}, Destructive: true}); err != nil {
		t.Fatal(err)
	}
	if err := p.Guard(connector.Call{Verb: "exec", Args: []string{"update x set a=1 where id=2"}}); err != nil {
		t.Fatal(err)
	}
}

func TestSavedQuery(t *testing.T) {
	dir := t.TempDir()
	qd := filepath.Join(dir, "assets", "db", "queries")
	_ = os.MkdirAll(qd, 0o700)
	_ = os.WriteFile(filepath.Join(qd, "signups.sql"), []byte("---\nname: signups\nparams: [since]\n---\nselect count(*) from users where created_at > $1\n"), 0o600)
	p := &PG{t: connector.NewTarget(connector.Target{System: "db", Dir: dir}, connector.StaticFields{})}
	sql, args, err := p.statement([]string{"@signups", "2026-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if sql != "select count(*) from users where created_at > $1" || len(args) != 1 {
		t.Fatalf("%q %v", sql, args)
	}
	if _, _, err := p.statement([]string{"@../../etc/passwd"}); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestOpenBadDSNDoesNotEchoIt(t *testing.T) {
	p := &PG{}
	tg := connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{"read": map[string]any{"dsn": "::FAKE_SECRET_123::not a dsn"}})
	err := p.Open(context.Background(), tg)
	if err == nil || strings.Contains(err.Error(), "FAKE_SECRET_123") {
		t.Fatalf("%v", err)
	}
}

func TestExecRefusedInReadMode(t *testing.T) {
	p := &PG{t: connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{})}
	err := p.exec(context.Background(), connector.Call{Verb: "exec", Args: []string{"update x set a=1 where id=1"}}, &connector.Collector{})
	if errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatal(err)
	}
}

// TestLive runs against a real database: TMVP_TEST_PG_DSN (read-write test DB).
// Run with: sec run TMVP_TEST_PG_DSN -- task test:pg
func TestLive(t *testing.T) {
	dsn := os.Getenv("TMVP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TMVP_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	open := func(mode connector.Mode) *PG {
		p := &PG{}
		tg := connector.NewTarget(connector.Target{Env: "test", System: "db", Mode: mode},
			connector.StaticFields{string(mode): map[string]any{"dsn": dsn}})
		if err := p.Open(ctx, tg); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r := open(connector.ModeRead)
	defer r.Close()
	var c connector.Collector
	if err := r.Do(ctx, connector.Call{Verb: "health"}, &c); err != nil || len(c.Rows) != 1 {
		t.Fatalf("health: %v", err)
	}
	c = connector.Collector{}
	if err := r.Do(ctx, connector.Call{Verb: "query", Args: []string{"select 1 as one, now() as t, 1.5::numeric as n"}}, &c); err != nil {
		t.Fatal(err)
	}
	if c.Rows[0]["n"] != "1.5" {
		t.Fatalf("numeric: %#v", c.Rows[0]["n"])
	}
	// read-only: a mutation through a read verb is rejected
	err := r.Do(ctx, connector.Call{Verb: "query", Args: []string{"create table tmvp_live_should_fail(x int)"}}, &c)
	if errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("read-only not enforced: %v", err)
	}
	// statement timeout guard
	r.t.Limits.StatementTimeout = 200_000_000 // 200ms
	err = r.Do(ctx, connector.Call{Verb: "query", Args: []string{"select pg_sleep(2)"}}, &c)
	if errs.CodeOf(err) != errs.GuardRejected {
		t.Fatalf("timeout guard: %v", err)
	}
	w := open(connector.ModeWrite)
	defer w.Close()
	c = connector.Collector{}
	err = w.Do(ctx, connector.Call{Verb: "exec", Preview: true, Args: []string{"create temp table tmvp_preview(x int)"}}, &c)
	if err != nil || c.Rows[0]["committed"] != false {
		t.Fatalf("preview: %v %+v", err, c.Rows)
	}
}

const explainSeqScan = `[
  {
    "Plan": {
      "Node Type": "Aggregate", "Strategy": "Plain", "Parallel Aware": false,
      "Startup Cost": 98250.12, "Total Cost": 98250.13, "Plan Rows": 1, "Plan Width": 8,
      "Plans": [
        {
          "Node Type": "Hash Join", "Parent Relationship": "Outer", "Join Type": "Inner",
          "Startup Cost": 12.5, "Total Cost": 87750.10, "Plan Rows": 4200000, "Plan Width": 0,
          "Hash Cond": "(e.user_id = u.id)",
          "Plans": [
            {"Node Type": "Seq Scan", "Parent Relationship": "Outer", "Relation Name": "events", "Alias": "e",
             "Startup Cost": 0.00, "Total Cost": 72000.00, "Plan Rows": 4200000, "Plan Width": 8},
            {"Node Type": "Hash", "Parent Relationship": "Inner", "Startup Cost": 10.0, "Total Cost": 10.0, "Plan Rows": 200, "Plan Width": 4,
             "Plans": [
               {"Node Type": "Seq Scan", "Parent Relationship": "Outer", "Relation Name": "users", "Alias": "u",
                "Startup Cost": 0.00, "Total Cost": 10.0, "Plan Rows": 200, "Plan Width": 4}
             ]}
          ]
        }
      ]
    }
  }
]`

const explainIndex = `[{"Plan": {"Node Type": "Index Scan", "Scan Direction": "Forward", "Index Name": "users_pkey",
  "Relation Name": "users", "Alias": "users", "Startup Cost": 0.29, "Total Cost": 8.31, "Plan Rows": 1, "Plan Width": 40,
  "Index Cond": "(id = 1)"}}]`

func TestEstimateFromPlan(t *testing.T) {
	info, err := parsePlan([]byte(explainSeqScan))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.seqScans) != 2 || info.totalCost != 98250.13 || info.rows != 1 {
		t.Fatalf("%+v", info)
	}
	// no reltuples known: cost-based, not confident
	e := buildEstimate(info, nil, connector.Limits{})
	if e.Why != "Seq Scan on events, ~4.2M rows" || e.Confident || e.Duration < 980*time.Millisecond || e.Duration > time.Second {
		t.Fatalf("%+v", e)
	}
	// big table: rows/s floor kicks in (8.4M / 2M/s = 4.2s)
	tuples := map[relKey]float64{{"", "events"}: 8.4e6, {"", "users"}: 200}
	e = buildEstimate(info, tuples, connector.Limits{})
	if e.Why != "Seq Scan on events (~8.4M rows in table)" || !e.Confident || e.Duration != 4200*time.Millisecond {
		t.Fatalf("%+v", e)
	}
	// calibrated: higher threshold, no flag
	e = buildEstimate(info, tuples, connector.Limits{SeqScanRows: 1e7, CostPerSecond: 1e4})
	if e.Why != "Seq Scan on events, ~4.2M rows" || e.Duration < 9*time.Second {
		t.Fatalf("%+v", e)
	}
	// reltuples -1 (never analyzed) = not confident
	if e := buildEstimate(info, map[relKey]float64{{"", "events"}: -1, {"", "users"}: 200}, connector.Limits{}); e.Confident {
		t.Fatalf("%+v", e)
	}
	info, _ = parsePlan([]byte(explainIndex))
	e = buildEstimate(info, nil, connector.Limits{})
	if e.Why != "Index Scan on users" || !e.Confident || e.Rows != 1 {
		t.Fatalf("%+v", e)
	}
	if _, err := parsePlan([]byte("nope")); err == nil {
		t.Fatal("bad json accepted")
	}
}

func TestEstimateNotForOtherVerbs(t *testing.T) {
	p := &PG{}
	for _, v := range []string{"health", "ls", "describe", "exec"} {
		if _, err := p.Estimate(context.Background(), connector.Call{Verb: v}); !errors.Is(err, connector.ErrNoEstimate) {
			t.Fatalf("%s: %v", v, err)
		}
	}
}

// TestLiveEstimate: TMVP_TEST_PG_DSN (skips otherwise).
func TestLiveEstimate(t *testing.T) {
	dsn := os.Getenv("TMVP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TMVP_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	p := &PG{}
	tg := connector.NewTarget(connector.Target{Env: "test", System: "db", Mode: connector.ModeRead},
		connector.StaticFields{"read": map[string]any{"dsn": dsn}})
	if err := p.Open(ctx, tg); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	e, err := p.Estimate(ctx, connector.Call{Verb: "query", Timeout: 5 * time.Second,
		Args: []string{"select * from pg_class where relname = $1", "pg_class"}})
	if err != nil || e.Why == "" {
		t.Fatalf("%+v %v", e, err)
	}
}
