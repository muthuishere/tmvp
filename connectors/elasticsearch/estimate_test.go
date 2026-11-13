package elasticsearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
)

func TestExpensive(t *testing.T) {
	cases := map[string]string{
		`{"query":{"match":{"msg":"boom"}}}`:                                      "",
		`{"query":{"wildcard":{"message":"foo*"}}}`:                               "",
		`{"query":{"bool":{"must":[{"wildcard":{"message":{"value":"*foo"}}}]}}}`: "leading wildcard on message",
		`{"query":{"wildcard":{"host":"?x"}}}`:                                    "leading wildcard on host",
		`{"query":{"regexp":{"host":"web-.*"}}}`:                                  "regexp on host",
		`{"query":{"function_score":{"script_score":{"script":"1"}}}}`:            "script_score",
		`{"aggs":{"x":{"scripted_metric":{}}}}`:                                   "scripted_metric",
		`{"query":{"query_string":{"query":"level:error AND msg:*timeout"}}}`:     "leading wildcard on msg",
		`{"query":{"query_string":{"query":"/err.r/","default_field":"msg"}}}`:    "regexp on msg",
		`{"query":{"query_string":{"query":"status:500 AND *"}}}`:                 "",
	}
	for in, want := range cases {
		var m map[string]any
		if err := json.Unmarshal([]byte(in), &m); err != nil {
			t.Fatal(err)
		}
		if got := Expensive(m); got != want {
			t.Errorf("Expensive(%s) = %q, want %q", in, got, want)
		}
	}
	if expensiveLucene("*fail", "") != "leading wildcard on all fields" {
		t.Error("lucene")
	}
}

func TestDocsEstimate(t *testing.T) {
	e := DocsEstimate(12_000_000, "logs-*", "leading wildcard on message", 0)
	if e.Why != "leading wildcard on message, ~12.0M docs in logs-*" || e.Duration != 12*time.Second || e.Confident {
		t.Fatalf("%+v", e)
	}
	e = DocsEstimate(4200, "logs-*", "", 0)
	if e.Why != "~4.2K matching docs in logs-*" || e.Duration != 84*time.Microsecond || !e.Confident {
		t.Fatalf("%+v", e)
	}
}

func TestEstimate(t *testing.T) {
	var gotBody []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Write([]byte(`{"version":{"number":"8.15.0"}}`))
		case "/logs-1/_count":
			var b [512]byte
			n, _ := r.Body.Read(b[:])
			gotBody = append(gotBody, string(b[:n]))
			if n == 0 {
				w.Write([]byte(`{"count":5000000}`))
				return
			}
			w.Write([]byte(`{"count":42}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	s := New().(*ES)
	if err := s.Open(context.Background(), connector.NewTarget(connector.Target{System: "search", Mode: connector.ModeRead},
		connector.StaticFields{"url": srv.URL, "api_key": fakeKey})); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e, err := s.Estimate(ctx, connector.Call{Verb: "search", Args: []string{"logs-1", `{"query":{"match":{"msg":"boom"}},"size":5}`}})
	if err != nil || e.Rows != 42 || e.Why != "~42 matching docs in logs-1" || gotBody[0] != `{"query":{"match":{"msg":"boom"}}}` {
		t.Fatalf("%+v %v %q", e, err, gotBody)
	}
	e, err = s.Estimate(ctx, connector.Call{Verb: "search", Args: []string{"logs-1", `{"query":{"regexp":{"host":"w.*"}}}`}})
	if err != nil || e.Rows != 5000000 || e.Duration != 5*time.Second || e.Why != "regexp on host, ~5.0M docs in logs-1" {
		t.Fatalf("%+v %v", e, err)
	}
	for _, c := range []connector.Call{{Verb: "health"}, {Verb: "ls"}, {Verb: "mapping", Args: []string{"logs-1"}}} {
		if _, err := s.Estimate(ctx, c); !errors.Is(err, connector.ErrNoEstimate) {
			t.Errorf("%s: %v", c.Verb, err)
		}
	}
}
