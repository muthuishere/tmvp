package elasticsearch

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

const fakeKey = "FAKE_APIKEY_123"

type seen struct {
	mu   sync.Mutex
	reqs []string
	body map[string]string
}

func server(t *testing.T) (*httptest.Server, *seen) {
	sn := &seen{body: map[string]string{}}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sn.mu.Lock()
		sn.reqs = append(sn.reqs, r.Method+" "+r.URL.Path)
		sn.body[r.URL.Path] = string(b)
		sn.mu.Unlock()
		if r.Header.Get("Authorization") != "ApiKey "+fakeKey {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"type":"security_exception","reason":"unable to authenticate"}}`))
			return
		}
		switch {
		case r.URL.Path == "/":
			w.Write([]byte(`{"version":{"number":"8.15.0"}}`))
		case r.URL.Path == "/_cat/indices":
			w.Write([]byte(`[{"index":"logs-1","health":"green","docs.count":"5","store.size":"1kb"},{"index":".kibana","health":"green"}]`))
		case r.URL.Path == "/_cat/aliases":
			w.Write([]byte(`[{"alias":"logs","index":"logs-1"}]`))
		case r.URL.Path == "/_cluster/health":
			w.Write([]byte(`{"status":"green","number_of_nodes":3}`))
		case r.URL.Path == "/logs-1/_mapping":
			w.Write([]byte(`{"logs-1":{"mappings":{"properties":{"msg":{"type":"text"},"http":{"properties":{"status":{"type":"integer"}}}}}}}`))
		case r.URL.Path == "/logs-1/_search":
			w.Write([]byte(`{"timed_out":false,"hits":{"hits":[{"_index":"logs-1","_id":"1","_score":1.0,"_source":{"msg":"boom","http":{"status":500}}}]},
			"aggregations":{"by_status":{"buckets":[{"key":500,"doc_count":1}]}}}`))
		case r.URL.Path == "/logs-1/_count":
			w.Write([]byte(`{"count":42}`))
		case r.URL.Path == "/secret/_search":
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"root_cause":[{"type":"security_exception","reason":"action unauthorized"}]}}`))
		case r.URL.Path == "/logs-1/_doc/1" && r.Method == http.MethodGet:
			w.Write([]byte(`{"_index":"logs-1","_id":"1","found":true,"_source":{"msg":"boom"}}`))
		case r.URL.Path == "/logs-1/_doc/404":
			w.WriteHeader(404)
			w.Write([]byte(`{"_index":"logs-1","_id":"404","found":false}`))
		case r.URL.Path == "/logs-1/_doc" && r.Method == http.MethodPost:
			w.WriteHeader(201)
			w.Write([]byte(`{"_index":"logs-1","_id":"new","result":"created","_shards":{"total":2}}`))
		case r.URL.Path == "/logs-1/_delete_by_query":
			w.Write([]byte(`{"deleted":3,"failures":[]}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no handler"}`))
		}
	}))
	t.Cleanup(s.Close)
	return s, sn
}

func open(t *testing.T, url, key string, mode connector.Mode, dir string) (*ES, error) {
	t.Helper()
	s := New().(*ES)
	err := s.Open(context.Background(), connector.NewTarget(connector.Target{System: "search", Mode: mode, Dir: dir},
		connector.StaticFields{"url": url, "api_key": key}))
	return s, err
}

func TestVerbTableAndAlias(t *testing.T) {
	if f, ok := connector.Lookup("opensearch"); !ok || f().Type() != "opensearch" {
		t.Fatal("opensearch alias missing")
	}
	for _, v := range New().Verbs() {
		want := connector.Read
		switch v.Name {
		case "index", "update", "delete", "update-by-query":
			want = connector.Write
		case "delete-index", "delete-by-query", "reindex":
			want = connector.Destructive
		}
		if v.Mode != want {
			t.Errorf("%s: %v", v.Name, v.Mode)
		}
	}
}

func TestReadVerbs(t *testing.T) {
	srv, sn := server(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "assets", "search", "queries"), 0o755)
	os.WriteFile(filepath.Join(dir, "assets", "search", "queries", "errors.json"),
		[]byte("---\ndescription: 5xx\nindex: logs-1\n---\n{\"query\":{\"term\":{\"http.status\":500}},\"size\":5000}\n"), 0o644)
	s, err := open(t, srv.URL, fakeKey, connector.ModeRead, dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var c connector.Collector
	if err := s.Do(ctx, connector.Call{Verb: "ls"}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 3 || c.Rows[0]["name"] != "logs-1" || c.Rows[1]["kind"] != "alias" || c.Rows[2]["name"] != "@errors" {
		t.Fatalf("ls %v", c.Rows)
	}
	c = connector.Collector{}
	if err := s.Do(ctx, connector.Call{Verb: "search", Args: []string{"@errors"}, Limit: 50}, &c); err != nil {
		t.Fatal(err)
	}
	if c.Rows[0]["msg"] != "boom" || c.Rows[0]["http.status"] != 500.0 || c.Rows[1]["aggregation"] != "by_status" {
		t.Fatalf("search %v", c.Rows)
	}
	var sent map[string]any
	json.Unmarshal([]byte(sn.body["/logs-1/_search"]), &sent)
	if sent["size"] != 50.0 || sent["timeout"] != "15000ms" {
		t.Fatalf("size/timeout not capped: %v", sent)
	}
	c = connector.Collector{}
	if err := s.Do(ctx, connector.Call{Verb: "mapping", Args: []string{"logs-1"}}, &c); err != nil || len(c.Rows) != 3 || c.Rows[1]["field"] != "http.status" {
		t.Fatalf("mapping %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := s.Do(ctx, connector.Call{Verb: "count", Args: []string{"logs-1"}}, &c); err != nil || c.Rows[0]["count"] != int64(42) {
		t.Fatalf("count %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := s.Do(ctx, connector.Call{Verb: "get", Args: []string{"logs-1", "1"}}, &c); err != nil || c.Rows[0]["msg"] != "boom" {
		t.Fatalf("get %v %v", c.Rows, err)
	}
	if err := s.Do(ctx, connector.Call{Verb: "get", Args: []string{"logs-1", "404"}}, &c); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("get 404: %v", err)
	}
	if err := s.Do(ctx, connector.Call{Verb: "search", Args: []string{"secret", "{}"}}, &c); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("403: %v", err)
	}
	if err := s.Do(ctx, connector.Call{Verb: "cat", Args: []string{"snapshots"}}, &c); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("cat allow-list: %v", err)
	}
}

func TestReadCannotMutate(t *testing.T) {
	srv, sn := server(t)
	s, err := open(t, srv.URL, fakeKey, connector.ModeRead, "")
	if err != nil {
		t.Fatal(err)
	}
	var c connector.Collector
	// write verb on a read call → blocked before any request
	if err := s.Do(context.Background(), connector.Call{Verb: "index", Args: []string{"logs-1", "{}"}}, &c); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("index via read: %v", err)
	}
	// a read connection refuses non-GET except allow-listed POSTs
	for _, mp := range [][2]string{{"POST", "/logs-1/_doc"}, {"DELETE", "/logs-1"}, {"PUT", "/x"}, {"POST", "/_reindex"}, {"POST", "/x/_delete_by_query"}} {
		if _, err := s.req(context.Background(), mp[0], mp[1], nil, []byte("{}")); errs.CodeOf(err) != errs.PolicyBlocked {
			t.Errorf("%v: %v", mp, err)
		}
	}
	for _, mp := range [][2]string{{"POST", "/x/_search"}, {"POST", "/_msearch"}, {"POST", "/x/_count"}, {"POST", "/x/_validate/query"}, {"GET", "/x"}} {
		if !readAllowed(mp[0], mp[1]) {
			t.Errorf("%v should be allowed", mp)
		}
	}
	for _, r := range sn.reqs {
		if !strings.HasPrefix(r, "GET") {
			t.Errorf("read connection sent %s", r)
		}
	}
}

func TestWritesAndDestructive(t *testing.T) {
	srv, _ := server(t)
	s, err := open(t, srv.URL, fakeKey, connector.ModeWrite, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var c connector.Collector
	if err := s.Do(ctx, connector.Call{Verb: "index", Args: []string{"logs-1", `{"msg":"hi"}`}, Write: true}, &c); err != nil || c.Rows[0]["result"] != "created" {
		t.Fatalf("index %v %v", c.Rows, err)
	}
	bad := []connector.Call{
		{Verb: "delete-by-query", Args: []string{"logs-1", `{"query":{"term":{"a":1}}}`}, Write: true},              // destructive off
		{Verb: "update-by-query", Args: []string{"logs-1", `{"script":{"source":"ctx._source.x=1"}}`}, Write: true}, // no query
		{Verb: "update-by-query", Args: []string{"logs-1", `{"query":{"match_all":{}}}`}, Write: true},              // match_all
		{Verb: "delete-index", Args: []string{"logs-*"}, Write: true, Destructive: true},                            // wildcard
		{Verb: "delete-index", Args: []string{"_all"}, Write: true, Destructive: true},
	}
	for _, b := range bad {
		if err := s.Do(ctx, b, &c); errs.CodeOf(err) != errs.PolicyBlocked {
			t.Errorf("%v: %v", b, err)
		}
	}
	c = connector.Collector{}
	if err := s.Do(ctx, connector.Call{Verb: "delete-by-query", Args: []string{"logs-1", `{"query":{"term":{"a":1}}}`}, Write: true, Destructive: true, Preview: true}, &c); err != nil || c.Rows[0]["would_affect"] != int64(42) {
		t.Fatalf("preview %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := s.Do(ctx, connector.Call{Verb: "delete-by-query", Args: []string{"logs-1", `{"query":{"term":{"a":1}}}`}, Write: true, Destructive: true}, &c); err != nil || c.Rows[0]["deleted"] != 3.0 {
		t.Fatalf("dbq %v %v", c.Rows, err)
	}
}

func TestAuthErrorNoSecret(t *testing.T) {
	srv, _ := server(t)
	_, err := open(t, srv.URL, "FAKE_WRONG_456", connector.ModeRead, "")
	if errs.CodeOf(err) != errs.Auth {
		t.Fatalf("want auth_error: %v", err)
	}
	if strings.Contains(err.Error(), "FAKE_WRONG_456") {
		t.Fatal("secret in error")
	}
	if _, err := open(t, "http://u:FAKE_PW_1@h", fakeKey, connector.ModeRead, ""); errs.CodeOf(err) != errs.Config || strings.Contains(err.Error(), "FAKE_PW_1") {
		t.Fatalf("creds in url: %v", err)
	}
}

func TestLive(t *testing.T) {
	u := os.Getenv("TMVP_TEST_ES_URL")
	if u == "" {
		t.Skip("TMVP_TEST_ES_URL not set")
	}
	f := connector.StaticFields{"url": u}
	if k := os.Getenv("TMVP_TEST_ES_API_KEY"); k != "" {
		f["api_key"] = k
	}
	s := New().(*ES)
	if err := s.Open(context.Background(), connector.NewTarget(connector.Target{System: "es", Mode: connector.ModeRead}, f)); err != nil {
		t.Fatal(err)
	}
	var c connector.Collector
	if err := s.Do(context.Background(), connector.Call{Verb: "health"}, &c); err != nil {
		t.Fatal(err)
	}
	t.Log(c.Rows)
}
