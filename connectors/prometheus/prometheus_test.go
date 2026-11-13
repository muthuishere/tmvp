package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "ops" || p != "FAKE_PW_123" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/prom/api/v1/query":
			if r.URL.Query().Get("query") == "bad(" {
				w.WriteHeader(400)
				w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
				return
			}
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up","job":"api"},"value":[1700000000.5,"1"]}]}}`))
		case "/prom/api/v1/query_range":
			if r.URL.Query().Get("step") == "" || r.URL.Query().Get("start") == "" {
				w.WriteHeader(400)
				return
			}
			w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"__name__":"up"},"values":[[1700000000,"1"],[1700000015,"0"],[1700000030,"NaN"]]}]}}`))
		case "/prom/api/v1/alerts":
			w.Write([]byte(`{"status":"success","data":{"alerts":[{"labels":{"alertname":"HighLatency","severity":"page","job":"api"},"annotations":{"summary":"slow"},"state":"firing","activeAt":"2026-01-01T00:00:00Z","value":"2"}]}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func open(t *testing.T, url, pw string) *Prom {
	t.Helper()
	p := New().(*Prom)
	err := p.Open(context.Background(), connector.NewTarget(connector.Target{System: "metrics", Mode: connector.ModeRead},
		connector.StaticFields{"url": url + "/prom", "user": "ops", "password": pw}))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVerbsReadOnly(t *testing.T) {
	for _, v := range New().Verbs() {
		if v.Mode != connector.Read {
			t.Errorf("%s not read", v.Name)
		}
	}
}

func TestQueryRangeAlerts(t *testing.T) {
	s := server(t)
	p := open(t, s.URL, "FAKE_PW_123")
	ctx := context.Background()
	var c connector.Collector
	if err := p.Do(ctx, connector.Call{Verb: "query", Args: []string{"up"}}, &c); err != nil {
		t.Fatal(err)
	}
	if c.Rows[0]["metric"] != `up{job="api"}` || c.Rows[0]["value"] != 1.0 {
		t.Fatalf("query %v", c.Rows)
	}
	c = connector.Collector{}
	if err := p.Do(ctx, connector.Call{Verb: "range", Args: []string{"up"}, Since: time.Hour}, &c); err != nil || len(c.Rows) != 3 || c.Rows[2]["value"] != "NaN" {
		t.Fatalf("range %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := p.Do(ctx, connector.Call{Verb: "alerts"}, &c); err != nil || c.Rows[0]["alert"] != "HighLatency" || c.Rows[0]["labels"] != `job="api"` {
		t.Fatalf("alerts %v %v", c.Rows, err)
	}
	if err := p.Do(ctx, connector.Call{Verb: "query", Args: []string{"bad("}}, &c); errs.CodeOf(err) != errs.Usage {
		t.Fatalf("bad query: %v", err)
	}
	if err := p.Do(ctx, connector.Call{Verb: "range", Args: []string{"up"}, Since: 30 * 24 * time.Hour}, &c); errs.CodeOf(err) != errs.GuardRejected {
		t.Fatalf("since cap: %v", err)
	}
}

func TestAuthErrorNoSecret(t *testing.T) {
	s := server(t)
	p := open(t, s.URL, "FAKE_WRONG_456")
	err := p.Do(context.Background(), connector.Call{Verb: "alerts"}, &connector.Collector{})
	if errs.CodeOf(err) != errs.Auth || strings.Contains(err.Error(), "FAKE_WRONG_456") {
		t.Fatalf("got %v", err)
	}
}

func TestLive(t *testing.T) {
	u := os.Getenv("TMVP_TEST_PROMETHEUS_URL")
	if u == "" {
		t.Skip("TMVP_TEST_PROMETHEUS_URL not set")
	}
	p := New()
	if err := p.Open(context.Background(), connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{"url": u})); err != nil {
		t.Fatal(err)
	}
	if err := p.Do(context.Background(), connector.Call{Verb: "query", Args: []string{"up"}}, &connector.Collector{}); err != nil {
		t.Fatal(err)
	}
}
