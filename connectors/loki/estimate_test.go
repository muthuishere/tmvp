package loki

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

func TestStreamSelectors(t *testing.T) {
	got := StreamSelectors(`sum(rate({app="api", x="a}b"} | json | line_format "{{.msg}}" [5m])) / sum(rate({app="web"}[5m]))`)
	want := []string{`{app="api", x="a}b"}`, `{app="web"}`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	if MaxRange(`rate({a="b"}[5m]) + rate({a="b"}[1h])`, 0) != time.Hour || MaxRange(`{a="b"}`, time.Minute) != time.Minute {
		t.Fatal("MaxRange")
	}
	for d, w := range map[time.Duration]string{24 * time.Hour: "24h", 7 * 24 * time.Hour: "7d", 90 * time.Minute: "1h30m", 15 * time.Minute: "15m"} {
		if ShortDur(d) != w {
			t.Errorf("ShortDur(%s)=%s", d, ShortDur(d))
		}
	}
}

func TestBytesEstimate(t *testing.T) {
	e := BytesEstimate(IndexStats{Entries: 10, Bytes: 3650722202}, 24*time.Hour, `{app="api"}`, 0)
	if e.Why != `~3.4GB over 24h in {app="api"}` || e.Bytes != 3650722202 || e.Rows != 10 ||
		e.Duration != connector.PerSecond(3650722202, 200e6) {
		t.Fatalf("%+v", e)
	}
}

func TestEstimate(t *testing.T) {
	var gotQ []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loki/api/v1/labels":
			w.Write([]byte(`{"status":"success","data":[]}`))
		case "/loki/api/v1/index/stats":
			gotQ = append(gotQ, r.URL.Query().Get("query"))
			if r.URL.Query().Get("start") == "" {
				w.WriteHeader(400)
				return
			}
			w.Write([]byte(`{"streams":2,"chunks":30,"entries":1000,"bytes":1048576}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	l := New().(*Loki)
	if err := l.Open(context.Background(), connector.NewTarget(connector.Target{System: "logs", Mode: connector.ModeRead,
		Limits: connector.Limits{Bytes: 1 << 20, BytesPerSecond: 1 << 20}}, connector.StaticFields{"url": s.URL})); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e, err := l.Estimate(ctx, connector.Call{Verb: "range", Args: []string{`{app="api"} |= "error"`}, Since: 6 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if e.Duration != time.Second || e.Bytes != 1<<20 || e.Rows != 1000 || e.Why != `~1.0MB over 6h in {app="api"}` {
		t.Fatalf("%+v", e)
	}
	gotQ = nil
	e, err = l.Estimate(ctx, connector.Call{Verb: "query", Args: []string{`count_over_time({app="api"}[5m]) / count_over_time({app="web"}[5m])`}})
	if err != nil || e.Bytes != 2<<20 || len(gotQ) != 2 || e.Why != `~2.0MB over 5m in {app="api"} + {app="web"}` {
		t.Fatalf("%+v %v %q", e, err, gotQ)
	}
	for _, c := range []connector.Call{{Verb: "labels"}, {Verb: "ls"}, {Verb: "values", Args: []string{"app"}}} {
		if _, err := l.Estimate(ctx, c); !errors.Is(err, connector.ErrNoEstimate) {
			t.Errorf("%s: %v", c.Verb, err)
		}
	}
}
