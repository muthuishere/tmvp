package grafana

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

const fakeTok = "FAKE_SA_TOKEN_123"

func server(t *testing.T) (*httptest.Server, *sync.Map) {
	got := &sync.Map{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeTok {
			w.WriteHeader(401)
			w.Write([]byte(`{"message":"invalid API key"}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		got.Store(r.Method+" "+r.URL.Path, string(b))
		got.Store("org", r.Header.Get("X-Grafana-Org-Id"))
		switch r.Method + " " + r.URL.Path {
		case "GET /api/org":
			w.Write([]byte(`{"id":2,"name":"acme"}`))
		case "GET /api/search":
			w.Write([]byte(`[{"uid":"abc","title":"API latency","folderTitle":"prod","tags":["api"],"url":"/d/abc"}]`))
		case "GET /api/datasources":
			w.Write([]byte(`[{"uid":"prom1","name":"Prometheus","type":"prometheus","isDefault":true}]`))
		case "GET /api/dashboards/uid/abc":
			w.Write([]byte(`{"dashboard":{"title":"API latency","panels":[
				{"id":1,"type":"timeseries","title":"p99","datasource":{"uid":"prom1","type":"prometheus"},"targets":[{"refId":"A","expr":"histogram_quantile(0.99, x)"}]},
				{"id":2,"type":"row","title":"logs","panels":[{"id":3,"type":"logs","title":"errors","datasource":"Loki","targets":[{"refId":"A","expr":"{app=\"api\"}"}]}]},
				{"id":4,"type":"text","title":"readme"}]}}`))
		case "GET /api/prometheus/grafana/api/v1/alerts":
			w.Write([]byte(`{"status":"success","data":{"alerts":[{"labels":{"alertname":"HighErr","severity":"page","team":"api"},"annotations":{"summary":"5xx high"},"state":"Alerting","activeAt":"2026-11-11T00:00:00Z","value":"3"}]}}`))
		case "GET /api/annotations":
			w.Write([]byte(`[{"id":9,"time":1700000000000,"text":"deploy v2","tags":["deploy"],"dashboardUID":"abc","login":"ci"}]`))
		case "POST /api/ds/query":
			w.Write([]byte(`{"results":{"A":{"status":200,"frames":[{"schema":{"name":"up","fields":[{"name":"Time","type":"time"},{"name":"Value","type":"number","labels":{"job":"api"}}]},"data":{"values":[[1700000000000,1700000060000],[1,0]]}}]}}}`))
		case "POST /api/annotations":
			w.Write([]byte(`{"id":10,"message":"Annotation added"}`))
		case "POST /api/alertmanager/grafana/api/v2/silences":
			w.Write([]byte(`{"silenceID":"s-1"}`))
		case "GET /api/alertmanager/grafana/api/v2/silences":
			w.Write([]byte(`[{"id":"s-1","status":{"state":"active"},"matchers":[{"name":"alertname","value":"HighErr","isRegex":false,"isEqual":true}],"endsAt":"2026-11-11T01:00:00Z","comment":"x","createdBy":"tmvp"}]`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"not found"}`))
		}
	}))
	t.Cleanup(s.Close)
	return s, got
}

func open(t *testing.T, url, tok string, mode connector.Mode, dir string) (*Grafana, error) {
	g := New().(*Grafana)
	err := g.Open(context.Background(), connector.NewTarget(connector.Target{System: "grafana", Mode: mode, Dir: dir},
		connector.StaticFields{"url": url, "token": tok, "org_id": "2"}))
	return g, err
}

func TestVerbTable(t *testing.T) {
	for _, v := range New().Verbs() {
		want := connector.Read
		if v.Name == "annotate" || v.Name == "silence" {
			want = connector.Write
		}
		if v.Mode != want || v.Mode == connector.Destructive {
			t.Errorf("%s: %v", v.Name, v.Mode)
		}
	}
}

func TestReads(t *testing.T) {
	srv, got := server(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "assets", "grafana", "queries"), 0o755)
	os.WriteFile(filepath.Join(dir, "assets", "grafana", "queries", "up.expr"), []byte("---\ndatasource: prom1\ndescription: up\n---\nup{job=\"api\"}\n"), 0o644)
	g, err := open(t, srv.URL, fakeTok, connector.ModeRead, dir)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := got.Load("org"); v != "2" {
		t.Fatalf("org header %v", v)
	}
	ctx := context.Background()
	var c connector.Collector
	if err := g.Do(ctx, connector.Call{Verb: "ls"}, &c); err != nil || len(c.Rows) != 3 || c.Rows[1]["kind"] != "datasource" || c.Rows[2]["name"] != "@up" {
		t.Fatalf("ls %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "dashboard", Args: []string{"abc"}}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 3 || c.Rows[0]["datasource"] != "prom1" || c.Rows[1]["expr"] != `{app="api"}` || c.Rows[2]["title"] != "readme" {
		t.Fatalf("dashboard %v", c.Rows)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "alerts", Flags: map[string]string{"state": "firing"}}, &c); err != nil || len(c.Rows) != 1 || c.Rows[0]["labels"] != `team="api"` {
		t.Fatalf("alerts %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "annotations"}, &c); err != nil || c.Rows[0]["text"] != "deploy v2" {
		t.Fatalf("annotations %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "query", Args: []string{"@up"}, Limit: 5}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 2 || c.Rows[0]["Time"] != "2023-11-14T22:13:20Z" || c.Rows[0]["series"] != `job="api"` {
		t.Fatalf("query %v", c.Rows)
	}
	b, _ := got.Load("POST /api/ds/query")
	var in struct {
		Queries []map[string]any `json:"queries"`
	}
	json.Unmarshal([]byte(b.(string)), &in)
	if in.Queries[0]["expr"] != `up{job="api"}` || in.Queries[0]["maxLines"] != 5.0 {
		t.Fatalf("ds/query body %s", b)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "silences"}, &c); err != nil || c.Rows[0]["matchers"] != `alertname="HighErr"` {
		t.Fatalf("silences %v %v", c.Rows, err)
	}
}

func TestWrites(t *testing.T) {
	srv, got := server(t)
	g, err := open(t, srv.URL, fakeTok, connector.ModeWrite, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var c connector.Collector
	if err := g.Do(ctx, connector.Call{Verb: "annotate", Args: []string{"deploy", "v3"}}, &c); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("read call must not annotate: %v", err)
	}
	if err := g.Do(ctx, connector.Call{Verb: "annotate", Args: []string{"deploy v3"}, Flags: map[string]string{"tags": "deploy,api"}, Write: true}, &c); err != nil || c.Rows[0]["id"] != int64(10) {
		t.Fatalf("annotate %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "silence", Args: []string{"create", "alertname=HighErr,env=~prod.*"}, Flags: map[string]string{"duration": "30m"}, Reason: "deploy", Write: true}, &c); err != nil || c.Rows[0]["id"] != "s-1" {
		t.Fatalf("silence %v %v", c.Rows, err)
	}
	b, _ := got.Load("POST /api/alertmanager/grafana/api/v2/silences")
	if !strings.Contains(b.(string), `"comment":"deploy"`) || !strings.Contains(b.(string), `"isRegex":true`) {
		t.Fatalf("silence body %s", b)
	}
	if err := g.Guard(connector.Call{Verb: "silence", Args: []string{"create", "env=~.*"}, Write: true}); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("match-all silence: %v", err)
	}
}

func TestAuthErrorNoSecret(t *testing.T) {
	srv, _ := server(t)
	_, err := open(t, srv.URL, "FAKE_WRONG_456", connector.ModeRead, "")
	if errs.CodeOf(err) != errs.Auth || strings.Contains(err.Error(), "FAKE_WRONG_456") {
		t.Fatalf("got %v", err)
	}
}

func TestLive(t *testing.T) {
	u := os.Getenv("TMVP_TEST_GRAFANA_URL")
	if u == "" {
		t.Skip("TMVP_TEST_GRAFANA_URL not set")
	}
	g := New().(*Grafana)
	if err := g.Open(context.Background(), connector.NewTarget(connector.Target{System: "grafana", Mode: connector.ModeRead},
		connector.StaticFields{"url": u, "token": os.Getenv("TMVP_TEST_GRAFANA_TOKEN")})); err != nil {
		t.Fatal(err)
	}
	var c connector.Collector
	if err := g.Do(context.Background(), connector.Call{Verb: "dashboards"}, &c); err != nil {
		t.Fatal(err)
	}
	t.Log(len(c.Rows), "dashboards")
}
