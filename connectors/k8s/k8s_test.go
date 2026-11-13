package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	spdystream "k8s.io/apimachinery/pkg/util/httpstream/spdy"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

const fakeToken = "FAKE_TOKEN_123"

type fakeAPI struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string // "METHOD path?query"
	body map[string]string
}

func (f *fakeAPI) seen(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{body: map[string]string{}}
	mux := http.NewServeMux()
	res := func(name, kind string, ns bool, short ...string) map[string]any {
		return map[string]any{"name": name, "kind": kind, "namespaced": ns, "shortNames": short, "singularName": strings.ToLower(kind), "verbs": []string{"get", "list"}}
	}
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"gitVersion": "v1.31.0", "platform": "linux/amd64"})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"kind": "APIVersions", "versions": []string{"v1"}})
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"groupVersion": "v1", "resources": []any{res("pods", "Pod", true, "po"), res("pods/log", "Pod", true),
			res("services", "Service", true, "svc"), res("secrets", "Secret", true), res("namespaces", "Namespace", false, "ns"), res("events", "Event", true, "ev"), res("nodes", "Node", false, "no")}})
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"kind": "APIGroupList", "groups": []any{map[string]any{"name": "apps",
			"versions": []any{map[string]any{"groupVersion": "apps/v1", "version": "v1"}}, "preferredVersion": map[string]any{"groupVersion": "apps/v1", "version": "v1"}}}})
	})
	mux.HandleFunc("/apis/apps/v1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"groupVersion": "apps/v1", "resources": []any{res("deployments", "Deployment", true, "deploy"), res("replicasets", "ReplicaSet", true, "rs")}})
	})
	mux.HandleFunc("/api/v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"items": []any{map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}}})
	})
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "web-1", "namespace": "app", "labels": map[string]any{"app": "web"}},
		"spec":   map[string]any{"containers": []any{map[string]any{"name": "c", "ports": []any{map[string]any{"name": "pg", "containerPort": 5432}}}}},
		"status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}
	mux.HandleFunc("/api/v1/namespaces/app/pods", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "as=Table") {
			writeJSON(w, map[string]any{"kind": "Table", "columnDefinitions": []any{map[string]any{"name": "Name"}, map[string]any{"name": "Status"}, map[string]any{"name": "IP", "priority": 1}},
				"rows": []any{map[string]any{"cells": []any{"web-1", "Running", "10.0.0.1"}, "object": map[string]any{"metadata": map[string]any{"namespace": "app"}}}}})
			return
		}
		writeJSON(w, map[string]any{"items": []any{pod}})
	})
	mux.HandleFunc("/api/v1/namespaces/app/pods/web-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			writeJSON(w, map[string]any{"kind": "Status", "status": "Success"})
			return
		}
		cp := map[string]any{}
		for k, v := range pod {
			cp[k] = v
		}
		cp["metadata"] = map[string]any{"name": "web-1", "managedFields": []any{"x"}}
		writeJSON(w, cp)
	})
	mux.HandleFunc("/api/v1/namespaces/app/secrets/s1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s1"},
			"data": map[string]any{"token": "RkFLRV9TRUNSRVRfMTIz"}, "stringData": map[string]any{"pw": "FAKE_SECRET_123"}})
	})
	mux.HandleFunc("/api/v1/namespaces/app/secrets", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "Forbidden", "code": 403,
			"message": "secrets is forbidden: User \"u1\" cannot list resource \"secrets\""})
	})
	mux.HandleFunc("/api/v1/namespaces/app/pods/web-1/log", func(w http.ResponseWriter, r *http.Request) {
		for i := 1; i <= 10; i++ {
			fmt.Fprintf(w, "line %d\n", i)
		}
	})
	mux.HandleFunc("/api/v1/namespaces/app/services/db", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"spec": map[string]any{"selector": map[string]any{"app": "web"}, "ports": []any{map[string]any{"port": 5432, "targetPort": "pg"}}}})
	})
	mux.HandleFunc("/api/v1/namespaces/app/events", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"items": []any{map[string]any{"type": "Warning", "reason": "BackOff", "message": "restarting", "count": 3,
			"lastTimestamp": "2026-11-10T10:00:00Z", "involvedObject": map[string]any{"kind": "Pod", "name": "web-1"}, "metadata": map[string]any{"namespace": "app"}}}})
	})
	mux.HandleFunc("/apis/apps/v1/namespaces/app/deployments/web", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.body[r.Method+" deploy "+r.Header.Get("Content-Type")] = string(b)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "web", "uid": "u1", "annotations": map[string]any{"deployment.kubernetes.io/revision": "3"}},
			"spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "web"}}}})
	})
	mux.HandleFunc("/apis/apps/v1/namespaces/app/deployments/web/scale", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.body[r.Method+" scale "+r.Header.Get("Content-Type")] = "x"
		f.mu.Unlock()
		writeJSON(w, map[string]any{"apiVersion": "autoscaling/v1", "kind": "Scale", "metadata": map[string]any{"name": "web"}, "spec": map[string]any{"replicas": 2}})
	})
	mux.HandleFunc("/apis/apps/v1/namespaces/app/replicasets", func(w http.ResponseWriter, _ *http.Request) {
		rs := func(name, rev string) map[string]any {
			return map[string]any{"metadata": map[string]any{"name": name, "annotations": map[string]any{"deployment.kubernetes.io/revision": rev},
				"ownerReferences": []any{map[string]any{"uid": "u1"}}},
				"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "web", "pod-template-hash": "h" + rev}}}}}
		}
		writeJSON(w, map[string]any{"items": []any{rs("web-a", "1"), rs("web-b", "2"), rs("web-c", "3")}})
	})
	mux.HandleFunc("/api/v1/namespaces/app/pods/web-1/exec", func(w http.ResponseWriter, r *http.Request) {
		st := `{"status":"Success"}`
		if r.URL.Query()["command"][0] == "false" {
			st = `{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"7"}]}}`
		}
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") { // SPDY (used behind a via: dialer)
			if _, err := httpstream.Handshake(r, w, []string{"v4.channel.k8s.io"}); err != nil {
				return
			}
			got := map[string]httpstream.Stream{}
			var mu sync.Mutex
			ready := make(chan struct{})
			conn := spdystream.NewResponseUpgrader().UpgradeResponse(w, r, func(s httpstream.Stream, _ <-chan struct{}) error {
				mu.Lock()
				defer mu.Unlock()
				got[s.Headers().Get("streamType")] = s
				if len(got) == 3 {
					close(ready)
				}
				return nil
			})
			if conn == nil {
				return
			}
			defer conn.Close()
			<-ready
			io.WriteString(got["stdout"], "hello\nwor")
			io.WriteString(got["stderr"], "ld\n")
			io.WriteString(got["stdout"], "ld\n")
			got["stdout"].Close()
			got["stderr"].Close()
			io.WriteString(got["error"], st)
			got["error"].Close()
			<-conn.CloseChan()
			return
		}
		up := websocket.Upgrader{Subprotocols: []string{"v5.channel.k8s.io", "v4.channel.k8s.io"}}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		send := func(id byte, s string) { _ = ws.WriteMessage(websocket.BinaryMessage, append([]byte{id}, s...)) }
		send(1, "hello\nwor")
		send(2, "ld\n")
		send(1, "ld\n")
		send(3, st)
		_ = ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	mux.HandleFunc("/api/v1/namespaces/app/pods/web-1/portforward", func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "websocket not supported here", http.StatusBadRequest) // exercise the SPDY fallback
			return
		}
		if _, err := httpstream.Handshake(r, w, []string{"portforward.k8s.io"}); err != nil {
			return
		}
		streams := make(chan httpstream.Stream, 4)
		conn := spdystream.NewResponseUpgrader().UpgradeResponse(w, r, func(s httpstream.Stream, _ <-chan struct{}) error {
			streams <- s
			return nil
		})
		if conn == nil {
			return
		}
		defer conn.Close()
		for {
			select {
			case s := <-streams:
				if s.Headers().Get("streamType") != "data" {
					continue
				}
				if s.Headers().Get("port") != "5432" {
					s.Reset()
					continue
				}
				go func() {
					b := make([]byte, 64)
					for {
						n, err := s.Read(b)
						if n > 0 {
							s.Write([]byte(strings.ToUpper(string(b[:n]))))
						}
						if err != nil {
							s.Close()
							return
						}
					}
				}()
			case <-conn.CloseChan():
				return
			}
		}
	})
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			w.WriteHeader(401)
			writeJSON(w, map[string]any{"kind": "Status", "message": "Unauthorized"})
			return
		}
		if r.Method == "PATCH" && strings.Contains(r.URL.Path, "/pods/") {
			writeJSON(w, map[string]any{})
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) kubeconfig(token string) string {
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	return fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: other
clusters:
- name: c1
  cluster: {server: %s, certificate-authority-data: %s}
users:
- name: u1
  user: {token: %s}
contexts:
- name: prod
  context: {cluster: c1, user: u1, namespace: app}
- name: other
  context: {cluster: c1, user: u1, namespace: nope}
`, f.srv.URL, base64.StdEncoding.EncodeToString(ca), token)
}

func open(t *testing.T, f *fakeAPI, mode connector.Mode, token string) (*K8s, error) {
	tg := connector.NewTarget(connector.Target{System: "cluster", Mode: mode, Dir: t.TempDir()},
		connector.StaticFields{"kubeconfig": f.kubeconfig(token), "context": "prod"})
	k := New().(*K8s)
	err := k.Open(context.Background(), tg)
	return k, err
}

func TestVerbTable(t *testing.T) {
	read := map[string]bool{"ls": true, "ctx": true, "get": true, "describe": true, "logs": true, "events": true, "top": true, "health": true}
	write := map[string]bool{"apply": true, "scale": true, "restart": true, "rollout": true, "delete": true, "exec": true}
	for _, v := range New().Verbs() {
		if read[v.Name] != (v.Mode == connector.Read) || (!read[v.Name] && !write[v.Name]) {
			t.Errorf("verb %s mode %v", v.Name, v.Mode)
		}
	}
	if len(New().Verbs()) != len(read)+len(write) {
		t.Fatal("verb count")
	}
}

func TestReadVerbs(t *testing.T) {
	f := newFakeAPI(t)
	k, err := open(t, f, connector.ModeRead, fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	ctx := context.Background()
	do := func(c connector.Call) *connector.Collector {
		t.Helper()
		var col connector.Collector
		if err := k.Do(ctx, c, &col); err != nil {
			t.Fatalf("%s: %v", c.Verb, err)
		}
		return &col
	}
	if r := do(connector.Call{Verb: "ctx"}).Rows[0]; r["namespace"] != "app" || r["context"] != "prod" || r["auth"] != "token" {
		t.Fatalf("ctx %v", r)
	}
	if r := do(connector.Call{Verb: "health"}).Rows[0]; r["ok"] != true || r["nodes_ready"] != "1/1" || r["version"] != "v1.31.0" {
		t.Fatalf("health %v", r)
	}
	if r := do(connector.Call{Verb: "get", Args: []string{"po"}}).Rows; len(r) != 1 || r[0]["name"] != "web-1" || r[0]["status"] != "Running" || r[0]["ip"] != nil || r[0]["namespace"] != "app" {
		t.Fatalf("get %v", r)
	}
	if l := do(connector.Call{Verb: "get", Args: []string{"pod/web-1"}, Flags: map[string]string{"o": "yaml"}}).Lines; strings.Contains(strings.Join(l, "\n"), "managedFields") || len(l) == 0 {
		t.Fatalf("get -o yaml %v", l)
	}
	if l := do(connector.Call{Verb: "describe", Args: []string{"pods", "web-1"}}).Lines; !strings.Contains(strings.Join(l, "\n"), "BackOff") {
		t.Fatalf("describe %v", l)
	}
	if l := do(connector.Call{Verb: "logs", Args: []string{"deploy/web"}, Limit: 3}).Lines; len(l) != 3 {
		t.Fatalf("logs %v", l)
	}
	if !f.seen("GET /api/v1/namespaces/app/pods/web-1/log?limitBytes=1048576&tailLines=3") {
		t.Fatalf("log query: %v", f.reqs)
	}
	if r := do(connector.Call{Verb: "events"}).Rows; len(r) != 1 || r[0]["object"] != "pod/web-1" {
		t.Fatalf("events %v", r)
	}
	var col connector.Collector
	if err := k.Do(ctx, connector.Call{Verb: "top", Args: []string{"pods"}}, &col); errs.CodeOf(err) != errs.Unsupported {
		t.Fatalf("top without metrics: %v", err)
	}
	if err := k.Do(ctx, connector.Call{Verb: "get", Args: []string{"widgets"}}, &col); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("unknown kind: %v", err)
	}
	if err := k.Do(ctx, connector.Call{Verb: "get", Args: []string{"pod/missing"}, Flags: map[string]string{"o": "yaml"}}, &col); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("404: %v", err)
	}
	err = k.Do(ctx, connector.Call{Verb: "get", Args: []string{"secrets"}, Flags: map[string]string{"o": "json"}}, &col)
	if errs.CodeOf(err) != errs.PolicyBlocked || strings.Contains(err.Error(), f.srv.URL) {
		t.Fatalf("403: %v", err)
	}
	for _, o := range []string{"yaml", "json"} {
		out := strings.Join(do(connector.Call{Verb: "get", Args: []string{"secret", "s1"}, Flags: map[string]string{"o": o}}).Lines, "\n")
		if strings.Contains(out, "RkFLRV9TRUNSRVRfMTIz") || strings.Contains(out, "FAKE_SECRET_123") || !strings.Contains(out, "token") {
			t.Fatalf("secret not masked (%s): %s", o, out)
		}
	}
	if out := strings.Join(do(connector.Call{Verb: "describe", Args: []string{"secret/s1"}}).Lines, "\n"); strings.Contains(out, "FAKE_SECRET_123") {
		t.Fatalf("describe leaked secret: %s", out)
	}
}

// Read connections cannot mutate: write verbs are refused and non-GET never reaches the API.
func TestReadCannotMutate(t *testing.T) {
	f := newFakeAPI(t)
	k, err := open(t, f, connector.ModeRead, fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var col connector.Collector
	for _, c := range []connector.Call{{Verb: "delete", Args: []string{"pod", "web-1"}}, {Verb: "scale", Args: []string{"deploy/web", "0"}},
		{Verb: "exec", Args: []string{"web-1", "--", "id"}}} {
		if err := k.Do(ctx, c, &col); errs.CodeOf(err) != errs.PolicyBlocked {
			t.Fatalf("%s via read: %v", c.Verb, err)
		}
	}
	if err := k.c.do(ctx, "DELETE", "/api/v1/namespaces/app/pods/web-1", nil, nil, "", nil); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("raw delete on read client: %v", err)
	}
	// the dynamic client shares the guarded transport
	pods, err := k.c.resolve(ctx, "pods")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.c.mapErr(ctx, k.c.ri(pods, "app").Delete(ctx, "web-1", metav1.DeleteOptions{})); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("dynamic delete on read client: %v", err)
	}
	if _, err := k.c.portForward(ctx, "app", "web-1", 5432); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("port-forward on a plain read client: %v", err)
	}
	if f.seen("DELETE") || f.seen("PATCH") || f.seen("POST") {
		t.Fatal("mutation reached the API")
	}
}

func TestWriteVerbs(t *testing.T) {
	f := newFakeAPI(t)
	k, err := open(t, f, connector.ModeWrite, fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	do := func(c connector.Call) *connector.Collector {
		t.Helper()
		var col connector.Collector
		if err := k.Do(ctx, c, &col); err != nil {
			t.Fatalf("%s: %v", c.Verb, err)
		}
		return &col
	}
	if r := do(connector.Call{Verb: "scale", Args: []string{"deploy/web", "5"}, Preview: true}).Rows[0]; r["from"] != 2 || r["to"] != 5 || r["result"] != "scaled (dry run)" {
		t.Fatalf("scale %v", r)
	}
	if !f.seen("PATCH /apis/apps/v1/namespaces/app/deployments/web/scale?dryRun=All") {
		t.Fatalf("scale dry run: %v", f.reqs)
	}
	do(connector.Call{Verb: "restart", Args: []string{"deploy", "web"}})
	if !strings.Contains(f.body["PATCH deploy application/merge-patch+json"], "restartedAt") {
		t.Fatalf("restart body %v", f.body)
	}
	if r := do(connector.Call{Verb: "rollout", Args: []string{"undo", "deploy/web"}}).Rows[0]; r["to_revision"] != 2 {
		t.Fatalf("undo %v", r)
	}
	if b := f.body["PATCH deploy application/json-patch+json"]; !strings.Contains(b, "/spec/template") || strings.Contains(b, "pod-template-hash") {
		t.Fatalf("undo body %s", b)
	}
	do(connector.Call{Verb: "apply", Args: []string{"apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\nspec: {replicas: 1}\n"}})
	if !f.seen("PATCH /apis/apps/v1/namespaces/app/deployments/web?fieldManager=tmvp&force=true") || f.body["PATCH deploy application/apply-patch+yaml"] == "" {
		t.Fatalf("apply: %v", f.reqs)
	}
	do(connector.Call{Verb: "delete", Args: []string{"pod/web-1"}})
	if !f.seen("DELETE /api/v1/namespaces/app/pods/web-1") {
		t.Fatal("delete")
	}
	l := do(connector.Call{Verb: "exec", Args: []string{"web-1", "--", "echo"}}).Lines
	sort.Strings(l) // stdout and stderr are separate streams: order across them is not fixed
	if strings.Join(l, "|") != "hello|ld|world" {
		t.Fatalf("exec %q", l)
	}
	var col connector.Collector
	if err := k.Do(ctx, connector.Call{Verb: "exec", Args: []string{"web-1", "false"}}, &col); errs.CodeOf(err) != errs.Runtime || !strings.Contains(err.Error(), "7") {
		t.Fatalf("exec exit: %v", err)
	}
}

// Behind a via: dialer exec uses SPDY over that dialer (client-go's websocket
// transport ignores rest.Config.Dial).
func TestExecViaDialer(t *testing.T) {
	f := newFakeAPI(t)
	var dials atomic.Int32
	d := connector.DialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	})
	tg := connector.NewTarget(connector.Target{System: "cluster", Mode: connector.ModeWrite, Dialer: d},
		connector.StaticFields{"kubeconfig": f.kubeconfig(fakeToken), "context": "prod"})
	k := New().(*K8s)
	if err := k.Open(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	var col connector.Collector
	if err := k.Do(context.Background(), connector.Call{Verb: "exec", Args: []string{"web-1", "--", "echo"}}, &col); err != nil {
		t.Fatal(err)
	}
	sort.Strings(col.Lines)
	if strings.Join(col.Lines, "|") != "hello|ld|world" {
		t.Fatalf("exec %q", col.Lines)
	}
	col = connector.Collector{}
	if err := k.Do(context.Background(), connector.Call{Verb: "exec", Args: []string{"web-1", "false"}}, &col); errs.CodeOf(err) != errs.Runtime || !strings.Contains(err.Error(), "7") {
		t.Fatalf("exec exit: %v", err)
	}
	if dials.Load() == 0 || !f.seen("POST /api/v1/namespaces/app/pods/web-1/exec") {
		t.Fatalf("exec did not go through the via dialer over SPDY: %d %v", dials.Load(), f.reqs)
	}
}

func TestDestructiveDelete(t *testing.T) {
	k := New().(*K8s)
	cases := []struct {
		c    connector.Call
		dest bool
	}{
		{connector.Call{Verb: "delete", Args: []string{"pod", "web-1"}}, false},
		{connector.Call{Verb: "delete", Args: []string{"ns/app"}}, true},
		{connector.Call{Verb: "delete", Args: []string{"namespace", "app"}}, true},
		{connector.Call{Verb: "delete", Args: []string{"pods"}, Flags: map[string]string{"selector": "app=web"}}, true},
		{connector.Call{Verb: "delete", Args: []string{"pods"}}, true},
		{connector.Call{Verb: "delete", Args: []string{"pv", "x"}}, true},
	}
	for _, tc := range cases {
		err := k.Guard(tc.c)
		if (errs.CodeOf(err) == errs.PolicyBlocked) != tc.dest {
			t.Errorf("%v: %v", tc.c.Args, err)
		}
		tc.c.Destructive = true
		if err := k.Guard(tc.c); err != nil {
			t.Errorf("allowed %v: %v", tc.c.Args, err)
		}
	}
}

func TestAuthErrorNoSecret(t *testing.T) {
	f := newFakeAPI(t)
	_, err := open(t, f, connector.ModeRead, "FAKE_WRONG_456")
	if errs.CodeOf(err) != errs.Auth {
		t.Fatalf("want auth_error, got %v", err)
	}
	if strings.Contains(err.Error(), "FAKE_WRONG_456") || strings.Contains(err.Error(), f.srv.URL) {
		t.Fatalf("secret/url in error: %v", err)
	}
	_, err = parseKubeconfig([]byte(f.kubeconfig(fakeToken)), "missing")
	if errs.CodeOf(err) != errs.Config || strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("missing ctx: %v", err)
	}
	_, err = parseKubeconfig([]byte("FAKE_SECRET_123: [\n"), "")
	if errs.CodeOf(err) != errs.Config || strings.Contains(err.Error(), "FAKE_SECRET_123") {
		t.Fatalf("bad yaml: %v", err)
	}
}

func TestTunnelPortForward(t *testing.T) {
	f := newFakeAPI(t)
	hop := connector.NewTarget(connector.Target{System: "cluster", Mode: connector.ModeRead},
		connector.StaticFields{"kubeconfig": f.kubeconfig(fakeToken), "context": "prod"})
	var dials atomic.Int32
	parent := connector.DialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	})
	tun, err := Tunneler{}.OpenTunnel(context.Background(), hop, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	for _, addr := range []string{"app/svc/db:5432", "pod/web-1:5432", "deploy/web:pg"} {
		c, err := tun.DialContext(context.Background(), Network, addr)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		c.Write([]byte("ping"))
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil || string(b) != "PING" {
			t.Fatalf("%s: %q %v", addr, b, err)
		}
		c.Close()
	}
	if _, err := tun.DialContext(context.Background(), "tcp", "db.internal:5432"); errs.CodeOf(err) != errs.Unsupported {
		t.Fatalf("plain addr: %v", err)
	}
	// API + SPDY port-forward traffic went through the via dialer (not direct)
	if dials.Load() < 4 || f.seen("GET /api/v1/namespaces/app/pods/web-1/portforward") {
		t.Fatalf("via dialer used %d times; reqs %v", dials.Load(), f.reqs)
	}
}

func TestSavedManifest(t *testing.T) {
	f := newFakeAPI(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "assets", "cluster", "manifests"), 0o755)
	os.WriteFile(filepath.Join(dir, "assets", "cluster", "manifests", "web.yaml"), []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\n---\n"), 0o644)
	tg := connector.NewTarget(connector.Target{System: "cluster", Mode: connector.ModeWrite, Dir: dir},
		connector.StaticFields{"kubeconfig": f.kubeconfig(fakeToken), "context": "prod"})
	k := New().(*K8s)
	if err := k.Open(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	var col connector.Collector
	if err := k.Do(context.Background(), connector.Call{Verb: "apply", Args: []string{"@web"}, Preview: true}, &col); err != nil {
		t.Fatal(err)
	}
	if len(col.Rows) != 1 || col.Rows[0]["result"] != "applied (dry run)" {
		t.Fatalf("%v", col.Rows)
	}
}

func TestLs(t *testing.T) {
	f := newFakeAPI(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "assets", "cluster", "manifests"), 0o755)
	os.WriteFile(filepath.Join(dir, "assets", "cluster", "manifests", "web.yaml"), []byte("# web deployment\napiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\n"), 0o644)
	tg := connector.NewTarget(connector.Target{System: "cluster", Mode: connector.ModeRead, Dir: dir},
		connector.StaticFields{"kubeconfig": f.kubeconfig(fakeToken), "context": "prod"})
	k := New().(*K8s)
	if err := k.Open(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	var col connector.Collector
	if err := k.Do(context.Background(), connector.Call{Verb: "ls"}, &col); err != nil {
		t.Fatal(err)
	}
	kinds := map[any]bool{}
	for _, r := range col.Rows {
		kinds[r["kind"]] = true
	}
	// the fake API serves no list routes for these kinds: each 404 must be
	// skipped, not fail ls, and the saved manifest must still be listed.
	if !kinds["manifests"] {
		t.Fatalf("ls rows: %v", col.Rows)
	}
}

// TestLiveK8s runs read verbs against a real cluster. TMVP_TEST_K8S_KUBECONFIG
// is kubeconfig content or a path to one (TMVP_TEST_K8S_CONTEXT optional).
func TestLiveK8s(t *testing.T) {
	kc := os.Getenv("TMVP_TEST_K8S_KUBECONFIG")
	if kc == "" {
		t.Skip("TMVP_TEST_K8S_KUBECONFIG not set")
	}
	if b, err := os.ReadFile(kc); err == nil {
		kc = string(b)
	}
	fields := connector.StaticFields{"kubeconfig": kc}
	if c := os.Getenv("TMVP_TEST_K8S_CONTEXT"); c != "" {
		fields["context"] = c
	}
	tg := connector.NewTarget(connector.Target{Mode: connector.ModeRead}, fields)
	k := New()
	if err := k.Open(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	for _, c := range []connector.Call{{Verb: "ctx"}, {Verb: "health"}, {Verb: "get", Args: []string{"ns"}},
		{Verb: "get", Args: []string{"po"}, Flags: map[string]string{"A": "true"}, Limit: 5}, {Verb: "events", Limit: 5}} {
		var col connector.Collector
		if err := k.Do(context.Background(), c, &col); err != nil {
			t.Fatalf("%s %v: %v", c.Verb, c.Args, err)
		}
		t.Logf("%s %v: %d rows", c.Verb, c.Args, len(col.Rows))
	}
	var col connector.Collector
	if err := k.Do(context.Background(), connector.Call{Verb: "delete", Args: []string{"pod", "tmvp-nonexistent"}}, &col); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("read connection must refuse delete: %v", err)
	}
}
