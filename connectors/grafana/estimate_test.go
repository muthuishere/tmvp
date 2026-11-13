package grafana

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
)

func TestSelectorHelpers(t *testing.T) {
	if got := promSelectors(`sum by (job) (rate(http_requests_total{code="500"}[5m]))`); !reflect.DeepEqual(got, []string{`http_requests_total{code="500"}`}) {
		t.Fatalf("%q", got)
	}
	if got := streamSelectors(`sum(count_over_time({app="api"} | line_format "{{.x}}" [5m]))`); !reflect.DeepEqual(got, []string{`{app="api"}`}) {
		t.Fatalf("%q", got)
	}
	c := connector.Call{Flags: map[string]string{}}
	if grafanaSteps(c, time.Hour, 1000) != 241 || grafanaSteps(c, 24*time.Hour, 1000) != 1000 {
		t.Fatal("default steps")
	}
	if grafanaSteps(connector.Call{Flags: map[string]string{"step": "60s"}}, time.Hour, 1000) != 61 ||
		grafanaSteps(connector.Call{Flags: map[string]string{"queryType": "instant"}}, time.Hour, 1000) != 1 {
		t.Fatal("flag steps")
	}
}

func TestEstimate(t *testing.T) {
	var gotMatch, gotStats string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/org":
			w.Write([]byte(`{"id":2}`))
		case "/api/datasources/uid/prom1":
			w.Write([]byte(`{"uid":"prom1","type":"prometheus"}`))
		case "/api/datasources/uid/loki1":
			w.Write([]byte(`{"uid":"loki1","type":"loki"}`))
		case "/api/datasources/uid/pg1":
			w.Write([]byte(`{"uid":"pg1","type":"grafana-postgresql-datasource"}`))
		case "/api/datasources/proxy/uid/prom1/api/v1/series":
			gotMatch = r.URL.Query().Get("match[]")
			w.Write([]byte(`{"status":"success","data":[{"__name__":"up","job":"a"},{"__name__":"up","job":"b"}]}`))
		case "/api/datasources/proxy/uid/loki1/loki/api/v1/index/stats":
			gotStats = r.URL.Query().Get("query")
			w.Write([]byte(`{"streams":1,"chunks":3,"entries":500,"bytes":2097152}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	g := New().(*Grafana)
	if err := g.Open(context.Background(), connector.NewTarget(connector.Target{System: "grafana", Mode: connector.ModeRead,
		Limits: connector.Limits{Rows: 1000, Bytes: 1 << 20, BytesPerSecond: 1 << 20, PointsPerSecond: 482}},
		connector.StaticFields{"url": srv.URL, "token": fakeTok})); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e, err := g.Estimate(ctx, connector.Call{Verb: "query", Args: []string{"prom1", `rate(up{job=~".+"}[5m])`}, Since: time.Hour, Flags: map[string]string{}})
	if err != nil || gotMatch != `up{job=~".+"}` || e.Rows != 482 || e.Duration != time.Second ||
		e.Why != "~2 series × 241 steps = 482 points over 1h0m0s" {
		t.Fatalf("%+v %v %q", e, err, gotMatch)
	}
	e, err = g.Estimate(ctx, connector.Call{Verb: "query", Args: []string{"loki1", `{app="api"} |= "x"`}, Since: 2 * time.Hour})
	if err != nil || gotStats != `{app="api"}` || e.Bytes != 2<<20 || e.Duration != 2*time.Second || e.Why != `~2.0MB over 2h0m0s in {app="api"}` {
		t.Fatalf("%+v %v", e, err)
	}
	for _, c := range []connector.Call{{Verb: "query", Args: []string{"pg1", "select 1"}}, {Verb: "dashboards"}, {Verb: "datasources"}} {
		if _, err := g.Estimate(ctx, c); !errors.Is(err, connector.ErrNoEstimate) {
			t.Errorf("%s: %v", c.Verb, err)
		}
	}
}
