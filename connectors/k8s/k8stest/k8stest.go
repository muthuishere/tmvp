// Package k8stest is an in-process fake Kubernetes API server for tests: a
// TLS httptest server with discovery, pods/services/deployments and the SPDY
// port-forward upgrade (the protocol client-go speaks behind a via: dialer).
// It is imported only by tests.
package k8stest

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/util/httpstream"
	spdystream "k8s.io/apimachinery/pkg/util/httpstream/spdy"
)

// Token is the bearer token the fake accepts (a fake value).
const Token = "FAKE_K8S_TOKEN_123"

// Pod is one fake pod in namespace "app".
type Pod struct {
	Name        string
	Labels      map[string]string
	Running     bool
	Ready       bool
	Terminating bool
	Ports       map[string]int // named container ports
}

// Handler serves one forwarded connection to pod:port. It returns when done;
// the data stream is then closed.
type Handler func(pod string, port int, rw io.ReadWriter)

// API is the fake server.
type API struct {
	Srv *httptest.Server

	mu       sync.Mutex
	pods     []Pod
	services map[string]map[string]string // name -> selector
	svcPorts map[string][2]any            // name -> {port, targetPort}
	deploys  map[string]map[string]string // name -> matchLabels
	reqs     []string
	handler  Handler
	fwdPods  []string

	Active atomic.Int32 // forwarded data streams currently open
	Conns  atomic.Int32 // SPDY port-forward connections currently open
}

// Upper echoes upper-cased bytes back until EOF.
func Upper(_ string, _ int, rw io.ReadWriter) {
	b := make([]byte, 4096)
	for {
		n, err := rw.Read(b)
		if n > 0 {
			if _, werr := rw.Write([]byte(strings.ToUpper(string(b[:n])))); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// New starts the fake API with no objects and the Upper handler.
func New(t testing.TB) *API {
	a := &API{services: map[string]map[string]string{}, svcPorts: map[string][2]any{}, deploys: map[string]map[string]string{}, handler: Upper}
	a.Srv = httptest.NewTLSServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.Srv.Close)
	return a
}

// AddPod adds a pod.
func (a *API) AddPod(p Pod) { a.mu.Lock(); a.pods = append(a.pods, p); a.mu.Unlock() }

// AddService adds a service selecting sel; port maps to targetPort (int or name).
func (a *API) AddService(name string, sel map[string]string, port int, targetPort any) {
	a.mu.Lock()
	a.services[name] = sel
	a.svcPorts[name] = [2]any{port, targetPort}
	a.mu.Unlock()
}

// AddDeployment adds a deployment whose selector is matchLabels.
func (a *API) AddDeployment(name string, matchLabels map[string]string) {
	a.mu.Lock()
	a.deploys[name] = matchLabels
	a.mu.Unlock()
}

// SetHandler replaces the per-connection handler.
func (a *API) SetHandler(h Handler) { a.mu.Lock(); a.handler = h; a.mu.Unlock() }

// Requests returns "METHOD path" of every request seen.
func (a *API) Requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.reqs...)
}

// Forwarded returns the pod name of every forwarded data stream, in order.
func (a *API) Forwarded() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.fwdPods...)
}

// Kubeconfig returns a kubeconfig (context "prod", namespace "app") for the fake.
func (a *API) Kubeconfig() string {
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.Srv.Certificate().Raw})
	return fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: prod
clusters:
- name: c1
  cluster: {server: %s, certificate-authority-data: %s}
users:
- name: u1
  user: {token: %s}
contexts:
- name: prod
  context: {cluster: c1, user: u1, namespace: app}
`, a.Srv.URL, base64.StdEncoding.EncodeToString(ca), Token)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func res(name, kind string, short ...string) map[string]any {
	return map[string]any{"name": name, "kind": kind, "namespaced": true, "shortNames": short,
		"singularName": strings.ToLower(kind), "verbs": []string{"get", "list"}}
}

func (a *API) podJSON(p Pod) map[string]any {
	var ports []any
	for n, v := range p.Ports {
		ports = append(ports, map[string]any{"name": n, "containerPort": v})
	}
	phase, ready := "Pending", "False"
	if p.Running {
		phase = "Running"
	}
	if p.Ready {
		ready = "True"
	}
	md := map[string]any{"name": p.Name, "namespace": "app", "labels": p.Labels}
	if p.Terminating {
		md["deletionTimestamp"] = "2026-11-11T00:00:00Z"
	}
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": md,
		"spec":   map[string]any{"containers": []any{map[string]any{"name": "c", "ports": ports}}},
		"status": map[string]any{"phase": phase, "conditions": []any{map[string]any{"type": "Ready", "status": ready}}}}
}

func matches(labels map[string]string, sel string) bool {
	if sel == "" {
		return true
	}
	for _, kv := range strings.Split(sel, ",") {
		k, v, _ := strings.Cut(kv, "=")
		if labels[k] != v {
			return false
		}
	}
	return true
}

func (a *API) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.reqs = append(a.reqs, r.Method+" "+r.URL.Path)
	a.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+Token {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"kind": "Status", "message": "Unauthorized"})
		return
	}
	p := r.URL.Path
	const nsp = "/api/v1/namespaces/app/"
	switch {
	case p == "/version":
		writeJSON(w, map[string]any{"gitVersion": "v1.31.0"})
	case p == "/api/v1":
		writeJSON(w, map[string]any{"groupVersion": "v1", "resources": []any{res("pods", "Pod", "po"), res("services", "Service", "svc")}})
	case p == "/apis":
		writeJSON(w, map[string]any{"kind": "APIGroupList", "groups": []any{map[string]any{"name": "apps",
			"versions":         []any{map[string]any{"groupVersion": "apps/v1", "version": "v1"}},
			"preferredVersion": map[string]any{"groupVersion": "apps/v1", "version": "v1"}}}})
	case p == "/apis/apps/v1":
		writeJSON(w, map[string]any{"groupVersion": "apps/v1", "resources": []any{res("deployments", "Deployment", "deploy")}})
	case p == nsp+"pods":
		sel := r.URL.Query().Get("labelSelector")
		a.mu.Lock()
		var items, rows []any
		for _, pd := range a.pods {
			if matches(pd.Labels, sel) {
				items = append(items, a.podJSON(pd))
				rows = append(rows, map[string]any{"cells": []any{pd.Name, map[bool]string{true: "True", false: "False"}[pd.Ready]},
					"object": map[string]any{"metadata": map[string]any{"namespace": "app"}}})
			}
		}
		a.mu.Unlock()
		if strings.Contains(r.Header.Get("Accept"), "as=Table") {
			writeJSON(w, map[string]any{"kind": "Table", "columnDefinitions": []any{map[string]any{"name": "Name"}, map[string]any{"name": "Ready"}}, "rows": rows})
			return
		}
		writeJSON(w, map[string]any{"items": items})
	case strings.HasPrefix(p, nsp+"pods/") && strings.HasSuffix(p, "/portforward"):
		a.portForward(w, r, strings.TrimSuffix(strings.TrimPrefix(p, nsp+"pods/"), "/portforward"))
	case strings.HasPrefix(p, nsp+"pods/"):
		name := strings.TrimPrefix(p, nsp+"pods/")
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, pd := range a.pods {
			if pd.Name == name {
				writeJSON(w, a.podJSON(pd))
				return
			}
		}
		a.notFound(w, "pods \""+name+"\" not found")
	case strings.HasPrefix(p, nsp+"services/"):
		name := strings.TrimPrefix(p, nsp+"services/")
		a.mu.Lock()
		sel, ok := a.services[name]
		sp := a.svcPorts[name]
		a.mu.Unlock()
		if !ok {
			a.notFound(w, "services \""+name+"\" not found")
			return
		}
		writeJSON(w, map[string]any{"spec": map[string]any{"selector": sel, "ports": []any{map[string]any{"port": sp[0], "targetPort": sp[1]}}}})
	case strings.HasPrefix(p, "/apis/apps/v1/namespaces/app/deployments/"):
		name := strings.TrimPrefix(p, "/apis/apps/v1/namespaces/app/deployments/")
		a.mu.Lock()
		ml, ok := a.deploys[name]
		a.mu.Unlock()
		if !ok {
			a.notFound(w, "deployments.apps \""+name+"\" not found")
			return
		}
		writeJSON(w, map[string]any{"spec": map[string]any{"selector": map[string]any{"matchLabels": ml}}})
	default:
		a.notFound(w, "not found")
	}
}

func (a *API) notFound(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusNotFound)
	writeJSON(w, map[string]any{"kind": "Status", "status": "Failure", "code": 404, "message": msg})
}

// portForward implements the server side of the SPDY port-forward protocol
// (portforward.k8s.io): an error + data stream pair per connection.
func (a *API) portForward(w http.ResponseWriter, r *http.Request, pod string) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "websocket not supported by the fake", http.StatusBadRequest) // client-go falls back to SPDY
		return
	}
	if _, err := httpstream.Handshake(r, w, []string{"portforward.k8s.io"}); err != nil {
		return
	}
	streams := make(chan httpstream.Stream, 16)
	conn := spdystream.NewResponseUpgrader().UpgradeResponse(w, r, func(s httpstream.Stream, _ <-chan struct{}) error {
		streams <- s
		return nil
	})
	if conn == nil {
		return
	}
	a.Conns.Add(1)
	defer a.Conns.Add(-1)
	defer conn.Close()
	errStreams := map[string]httpstream.Stream{}
	for {
		select {
		case s := <-streams:
			h := s.Headers()
			if h.Get("streamType") == "error" {
				errStreams[h.Get("requestID")] = s
				continue
			}
			port, _ := strconv.Atoi(h.Get("port"))
			a.mu.Lock()
			hd := a.handler
			a.fwdPods = append(a.fwdPods, pod)
			a.mu.Unlock()
			if port == 9 { // "nothing listening": report on the error stream like the kubelet
				if es := errStreams[h.Get("requestID")]; es != nil {
					_, _ = io.WriteString(es, fmt.Sprintf("error forwarding port %d to pod %s: connection refused", port, pod))
					_ = es.Close()
				}
				_ = s.Reset()
				continue
			}
			a.Active.Add(1)
			go func() {
				defer a.Active.Add(-1)
				hd(pod, port, s)
				_ = s.Close()
			}()
		case <-conn.CloseChan():
			return
		}
	}
}
