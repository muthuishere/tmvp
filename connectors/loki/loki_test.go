package loki

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

const fakeTok = "FAKE_TOKEN_123"

func server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeTok {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("X-Scope-OrgID") != "acme" {
			w.WriteHeader(403)
			w.Write([]byte("no org id"))
			return
		}
		switch r.URL.Path {
		case "/loki/api/v1/labels":
			w.Write([]byte(`{"status":"success","data":["job","app"]}`))
		case "/loki/api/v1/label/app/values":
			w.Write([]byte(`{"status":"success","data":["api","web"]}`))
		case "/loki/api/v1/series":
			w.Write([]byte(`{"status":"success","data":[{"app":"api","env":"prod"}]}`))
		case "/loki/api/v1/query_range", "/loki/api/v1/query":
			q := r.URL.Query().Get("query")
			if strings.HasPrefix(q, "rate(") || strings.HasPrefix(q, "sum") {
				w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"app":"api"},"value":[1700000000,"2.5"]}]}}`))
				return
			}
			if q == `{app="bad"` {
				w.WriteHeader(400)
				w.Write([]byte("parse error"))
				return
			}
			w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
				{"stream":{"app":"api"},"values":[["1700000002000000000","b"],["1700000000000000000","a"]]},
				{"stream":{"app":"web"},"values":[["1700000001000000000","w"]]}]}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func open(t *testing.T, url, tok, dir string) (*Loki, error) {
	l := New().(*Loki)
	err := l.Open(context.Background(), connector.NewTarget(connector.Target{System: "logs", Mode: connector.ModeRead, Dir: dir},
		connector.StaticFields{"url": url, "token": tok, "tenant": "acme"}))
	return l, err
}

func TestVerbsReadOnly(t *testing.T) {
	for _, v := range New().Verbs() {
		if v.Mode != connector.Read {
			t.Errorf("%s not read", v.Name)
		}
	}
}

func TestSelectorGuard(t *testing.T) {
	bad := []string{`{}`, `{app=""}`, `{app=~".*"}`, `{app!="x"}`, `sum(rate({}[5m]))`, `no selector`, ""}
	for _, q := range bad {
		if checkSelector(q) == nil {
			t.Errorf("%q accepted", q)
		}
	}
	good := []string{`{app="api"}`, `{app=~"api|web"} |= "error"`, "sum(rate({job=`x`}[5m]))", `{app!="x", env="prod"}`}
	for _, q := range good {
		if err := checkSelector(q); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
	l := New().(*Loki)
	if err := l.Guard(connector.Call{Verb: "range", Args: []string{"{}"}}); errs.CodeOf(err) != errs.GuardRejected {
		t.Fatalf("guard {}: %v", err)
	}
}

func TestVerbs(t *testing.T) {
	srv := server(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "assets", "logs", "queries"), 0o755)
	os.WriteFile(filepath.Join(dir, "assets", "logs", "queries", "errors.logql"), []byte("---\ndescription: api errors\n---\n{app=\"api\"} |= \"error\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "assets", "logs", "queries", "all.logql"), []byte("{}\n"), 0o644)
	l, err := open(t, srv.URL, fakeTok, dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var c connector.Collector
	if err := l.Do(ctx, connector.Call{Verb: "ls"}, &c); err != nil || len(c.Rows) != 4 || c.Rows[0]["name"] != "app" {
		t.Fatalf("ls %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := l.Do(ctx, connector.Call{Verb: "range", Args: []string{"@errors"}, Since: time.Hour}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 3 || c.Rows[0]["line"] != "b" || c.Rows[2]["line"] != "a" {
		t.Fatalf("range newest first %v", c.Rows)
	}
	if err := l.Do(ctx, connector.Call{Verb: "range", Args: []string{"@all"}}, &c); errs.CodeOf(err) != errs.GuardRejected {
		t.Fatalf("saved {} must be refused: %v", err)
	}
	if err := l.Do(ctx, connector.Call{Verb: "range", Args: []string{`{app="api"}`}, Since: 8 * 24 * time.Hour}, &c); errs.CodeOf(err) != errs.GuardRejected {
		t.Fatalf("max range: %v", err)
	}
	c = connector.Collector{}
	if err := l.Do(ctx, connector.Call{Verb: "tail", Args: []string{`{app="api"}`}}, &c); err != nil || c.Rows[0]["line"] != "a" {
		t.Fatalf("tail oldest first %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := l.Do(ctx, connector.Call{Verb: "query", Args: []string{`sum(rate({app="api"}[5m]))`}}, &c); err != nil || c.Rows[0]["value"] != 2.5 {
		t.Fatalf("query %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := l.Do(ctx, connector.Call{Verb: "values", Args: []string{"app"}}, &c); err != nil || len(c.Rows) != 2 {
		t.Fatalf("values %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := l.Do(ctx, connector.Call{Verb: "series", Args: []string{`{app="api"}`}}, &c); err != nil || c.Rows[0]["labels"] != `app="api",env="prod"` {
		t.Fatalf("series %v %v", c.Rows, err)
	}
	if err := l.Do(ctx, connector.Call{Verb: "query", Args: []string{`{app="bad"`}}, &c); errs.CodeOf(err) != errs.Usage {
		t.Fatalf("400: %v", err)
	}
}

func TestAuthErrorNoSecret(t *testing.T) {
	srv := server(t)
	_, err := open(t, srv.URL, "FAKE_WRONG_456", "")
	if errs.CodeOf(err) != errs.Auth || strings.Contains(err.Error(), "FAKE_WRONG_456") {
		t.Fatalf("got %v", err)
	}
}

func TestLive(t *testing.T) {
	u := os.Getenv("TMVP_TEST_LOKI_URL")
	if u == "" {
		t.Skip("TMVP_TEST_LOKI_URL not set")
	}
	f := connector.StaticFields{"url": u}
	if tok := os.Getenv("TMVP_TEST_LOKI_TOKEN"); tok != "" {
		f["token"] = tok
	}
	if ten := os.Getenv("TMVP_TEST_LOKI_TENANT"); ten != "" {
		f["tenant"] = ten
	}
	l := New().(*Loki)
	if err := l.Open(context.Background(), connector.NewTarget(connector.Target{System: "logs", Mode: connector.ModeRead}, f)); err != nil {
		t.Fatal(err)
	}
	var c connector.Collector
	if err := l.Do(context.Background(), connector.Call{Verb: "labels"}, &c); err != nil {
		t.Fatal(err)
	}
	t.Log(c.Rows)
}
