package httpc

import (
	"context"
	"io"
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
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("/api/items", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer FAKE_TOKEN_123" || r.Header.Get("X-Team") != "ops" {
			w.WriteHeader(401)
			return
		}
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"created":` + string(b) + `}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":1,"q":"` + r.URL.Query().Get("x") + `"},{"id":2}]`))
	})
	mux.HandleFunc("/api/big", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("a", 5000))) })
	mux.HandleFunc("/api/slow", func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) })
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func open(t *testing.T, mode connector.Mode, f connector.StaticFields, lim connector.Limits) *HTTP {
	t.Helper()
	h := New().(*HTTP)
	if err := h.Open(context.Background(), connector.NewTarget(connector.Target{System: "api", Mode: mode, Limits: lim}, f)); err != nil {
		t.Fatal(err)
	}
	return h
}

func fields(base string) connector.StaticFields {
	return connector.StaticFields{"base_url": base + "/api", "token": "FAKE_TOKEN_123", "headers": map[string]any{"X-Team": "ops"}, "health_path": "health"}
}

func TestVerbs(t *testing.T) {
	for _, v := range New().Verbs() {
		read := v.Name == "get" || v.Name == "health" || v.Name == "ls" || v.Name == "apis"
		if v.Name == "call" {
			if v.Mode != connector.ReadWrite || !v.Saved {
				t.Errorf("call: %v", v.Mode)
			}
			continue
		}
		if read != (v.Mode == connector.Read) {
			t.Errorf("%s: %v", v.Name, v.Mode)
		}
	}
}

func TestGetAndHealth(t *testing.T) {
	s := server(t)
	h := open(t, connector.ModeRead, fields(s.URL), connector.Limits{})
	var c connector.Collector
	if err := h.Do(context.Background(), connector.Call{Verb: "get", Args: []string{"/items"}, Flags: map[string]string{"q.x": "y"}}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 2 || c.Rows[0]["q"] != "y" {
		t.Fatalf("rows %v", c.Rows)
	}
	c = connector.Collector{}
	if err := h.Do(context.Background(), connector.Call{Verb: "health"}, &c); err != nil || c.Rows[0]["status"] != 204 {
		t.Fatalf("health %v %v", c.Rows, err)
	}
}

func TestAbsoluteURLRefused(t *testing.T) {
	g := New().(connector.Guard)
	for _, p := range []string{"https://evil.example/x", "//evil.example/x"} {
		if err := g.Guard(connector.Call{Verb: "get", Args: []string{p}}); errs.CodeOf(err) != errs.PolicyBlocked {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestAuthErrorNoSecret(t *testing.T) {
	s := server(t)
	f := fields(s.URL)
	f["token"] = "FAKE_WRONG_456"
	h := open(t, connector.ModeRead, f, connector.Limits{})
	err := h.Do(context.Background(), connector.Call{Verb: "get", Args: []string{"items"}}, &connector.Collector{})
	if errs.CodeOf(err) != errs.Auth || strings.Contains(err.Error(), "FAKE_WRONG_456") {
		t.Fatalf("got %v", err)
	}
}

func TestBoundsAndTimeout(t *testing.T) {
	s := server(t)
	h := open(t, connector.ModeRead, fields(s.URL), connector.Limits{Bytes: 1000, Timeout: 500 * time.Millisecond, Rows: 10, Lines: 10})
	if err := h.Do(context.Background(), connector.Call{Verb: "get", Args: []string{"big"}}, &connector.Collector{}); errs.CodeOf(err) != errs.GuardRejected {
		t.Fatalf("big: %v", err)
	}
	if err := h.Do(context.Background(), connector.Call{Verb: "get", Args: []string{"slow"}}, &connector.Collector{}); errs.CodeOf(err) != errs.Timeout {
		t.Fatalf("slow: %v", err)
	}
}

func TestWriteAndPreview(t *testing.T) {
	s := server(t)
	h := open(t, connector.ModeWrite, fields(s.URL), connector.Limits{})
	var c connector.Collector
	if err := h.Do(context.Background(), connector.Call{Verb: "post", Args: []string{"items", `{"a":1}`}, Preview: true}, &c); err != nil || c.Rows[0]["preview"] != true {
		t.Fatalf("preview %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := h.Do(context.Background(), connector.Call{Verb: "post", Args: []string{"items", `{"a":1}`}}, &c); err != nil {
		t.Fatal(err)
	}
	if c.Rows[0]["status"] != 200 || c.Rows[1]["created"] == nil {
		t.Fatalf("rows %v", c.Rows)
	}
}

func TestBaseURLWithCredentialsRejected(t *testing.T) {
	h := New()
	err := h.Open(context.Background(), connector.NewTarget(connector.Target{System: "api"}, connector.StaticFields{"base_url": "https://u:FAKE_PW_1@x.example"}))
	if errs.CodeOf(err) != errs.Config || strings.Contains(err.Error(), "FAKE_PW_1") {
		t.Fatalf("got %v", err)
	}
}

func TestLive(t *testing.T) {
	u := os.Getenv("TMVP_TEST_HTTP_URL")
	if u == "" {
		t.Skip("TMVP_TEST_HTTP_URL not set")
	}
	h := open(t, connector.ModeRead, connector.StaticFields{"base_url": u}, connector.Limits{})
	if err := h.Do(context.Background(), connector.Call{Verb: "health"}, &connector.Collector{}); err != nil {
		t.Fatal(err)
	}
}

type basicRec struct{ user, pass string }

func (b *basicRec) AddBasicAuth(u, p string) { b.user, b.pass = u, p }

func TestBasicAuthRegistersRedactorForm(t *testing.T) {
	rec := &basicRec{}
	tg := connector.NewTarget(connector.Target{Mode: connector.ModeRead, Redactor: rec},
		connector.StaticFields{"user": "ops", "password": "FAKE_SECRET_123"})
	req, _ := http.NewRequest("GET", "http://x.test/", nil)
	if err := ApplyAuth(context.Background(), tg, req); err != nil {
		t.Fatal(err)
	}
	if rec.user != "ops" || rec.pass != "FAKE_SECRET_123" {
		t.Fatalf("basic-auth pair not registered with the redactor: %+v", rec)
	}
}

func TestAPIsFromAssetsAndOpenAPI(t *testing.T) {
	s := server(t)
	dir := t.TempDir()
	root1, root2 := dir+"/a1", dir+"/a2"
	for p, body := range map[string]string{
		root1 + "/api/apis/list-items.md":  "---\nmethod: GET\npath: /items\ndescription: all items\nparams: [x]\n---\nnotes\n",
		root2 + "/api/apis/list-items.md":  "---\nmethod: GET\npath: /shadowed\n---\n",
		root2 + "/api/apis/create-item.md": "---\nmethod: POST\npath: /items\n---\n",
		dir + "/openapi.yaml":              "paths:\n  /items/{id}:\n    get: {operationId: get-item, parameters: [{name: id, in: path}]}\n",
	} {
		_ = os.MkdirAll(p[:strings.LastIndex(p, "/")], 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := fields(s.URL)
	f["openapi"] = "openapi.yaml"
	h := New().(*HTTP)
	tg := connector.NewTarget(connector.Target{System: "api", Mode: connector.ModeRead, Dir: dir, AssetRoots: []string{root1, root2}}, f)
	if err := h.Open(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	var c connector.Collector
	if err := h.Do(context.Background(), connector.Call{Verb: "ls"}, &c); err != nil {
		t.Fatal(err)
	}
	got := map[string]connector.Row{}
	for _, r := range c.Rows {
		got[r["name"].(string)] = r
	}
	if len(got) != 3 || got["@list-items"]["path"] != "/items" || got["@create-item"]["mutating"] != true || got["@get-item"]["source"] != "openapi" {
		t.Fatalf("rows %v", c.Rows)
	}
	c = connector.Collector{}
	if err := h.Do(context.Background(), connector.Call{Verb: "call", Args: []string{"@list-items", "x=z"}}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 2 || c.Rows[0]["q"] != "z" {
		t.Fatalf("call rows %v", c.Rows)
	}
	err := h.Do(context.Background(), connector.Call{Verb: "call", Args: []string{"create-item", "a=1"}}, &connector.Collector{})
	if errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("mutating api as read: %v", err)
	}
	c = connector.Collector{}
	if err := h.Do(context.Background(), connector.Call{Verb: "call", Args: []string{"create-item", "a=1"}, Write: true, Preview: true}, &c); err != nil || c.Rows[0]["method"] != "POST" {
		t.Fatalf("preview %v %v", err, c.Rows)
	}
	if err := h.Do(context.Background(), connector.Call{Verb: "call", Args: []string{"get-item"}}, &connector.Collector{}); errs.CodeOf(err) != errs.Usage {
		t.Fatalf("missing path param: %v", err)
	}
}
