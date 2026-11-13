package prometheus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
)

func TestSelectors(t *testing.T) {
	cases := map[string][]string{
		`up`: {"up"},
		`sum by (job) (rate(http_requests_total{code=~"5.."}[5m])) / ignoring(code) sum(rate(http_requests_total[5m]))`: {`http_requests_total{code=~"5.."}`, "http_requests_total"},
		`label_replace(up, "dst", "$1", "src", "(.*)")`:                                                                 {"up"},
		`{__name__="x", job="a}b"} > 0.5 and on(job) foo offset 5m`:                                                     {`{__name__="x", job="a}b"}`, "foo"},
		`time() - 3`: nil,
	}
	for in, want := range cases {
		if got := Selectors(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Selectors(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPointsEstimate(t *testing.T) {
	e := PointsEstimate(1000, Steps(time.Hour, 15*time.Second), time.Hour, 0)
	if e.Rows != 241000 || e.Duration != 24100*time.Microsecond || !e.Confident ||
		e.Why != "~1.0K series × 241 steps = 241.0K points over 1h0m0s" {
		t.Fatalf("%+v", e)
	}
	e = PointsEstimate(10001, 1, 0, 1000)
	if e.Confident || e.Rows != 10000 || e.Duration != 10*time.Second || !strings.HasPrefix(e.Why, "≥10.0K series") {
		t.Fatalf("%+v", e)
	}
}

func TestEstimate(t *testing.T) {
	var gotMatch []string
	var gotLimit, gotTimeout string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/series":
			gotMatch, gotLimit = r.URL.Query()["match[]"], r.URL.Query().Get("limit")
			var b strings.Builder
			for i := 0; i < 500; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"__name__":"up","i":"%d"}`, i)
			}
			w.Write([]byte(`{"status":"success","data":[` + b.String() + `]}`))
		case "/api/v1/query_range":
			gotTimeout = r.URL.Query().Get("timeout")
			w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		}
	}))
	defer s.Close()
	p := New().(*Prom)
	if err := p.Open(context.Background(), connector.NewTarget(connector.Target{System: "m", Mode: connector.ModeRead,
		Limits: connector.Limits{Bytes: 1 << 20, PointsPerSecond: 1e5}}, connector.StaticFields{"url": s.URL})); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := connector.Call{Verb: "range", Args: []string{`rate(up{job="api"}[5m])`}, Since: time.Hour, Flags: map[string]string{"step": "60s"}, Timeout: 7 * time.Second}
	e, err := p.Estimate(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotMatch) != 1 || gotMatch[0] != `up{job="api"}` || gotLimit != "10001" {
		t.Fatalf("probe %q %q", gotMatch, gotLimit)
	}
	if e.Rows != 500*61 || e.Duration != 305*time.Millisecond || e.Why != "~500 series × 61 steps = 30.5K points over 1h0m0s" {
		t.Fatalf("%+v", e)
	}
	if err := p.Do(ctx, c, &connector.Collector{}); err != nil || gotTimeout != "7000ms" {
		t.Fatalf("timeout %q %v", gotTimeout, err)
	}
	for _, c := range []connector.Call{{Verb: "alerts"}, {Verb: "ls"}, {Verb: "query", Args: []string{"vector(1)"}}} {
		if _, err := p.Estimate(ctx, c); !errors.Is(err, connector.ErrNoEstimate) {
			t.Errorf("%s: %v", c.Verb, err)
		}
	}
}
