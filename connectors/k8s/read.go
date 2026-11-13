package k8s

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func (k *K8s) ctxVerb(e connector.Emit) error {
	rc := k.c.rc
	return e.Row(connector.Row{"context": rc.Context, "cluster": rc.Cluster, "server": rc.Server.Host,
		"namespace": k.ns, "user": rc.User, "auth": rc.AuthKind, "mode": string(k.t.Mode)})
}

func (k *K8s) health(ctx context.Context, e connector.Emit) error {
	start := time.Now()
	var v struct {
		GitVersion string `json:"gitVersion"`
		Platform   string `json:"platform"`
	}
	if err := k.c.get(ctx, "/version", nil, &v); err != nil {
		return err
	}
	r := connector.Row{"ok": true, "version": v.GitVersion, "platform": v.Platform}
	ready := "ok"
	if rc, err := k.c.stream(ctx, "GET", "/readyz", nil, nil, "", "text/plain"); err != nil {
		if errs.CodeOf(err) == errs.Auth {
			return err
		}
		ready = "unavailable"
		if errs.CodeOf(err) != errs.PolicyBlocked { // 403 on /readyz is common and not unhealthy
			r["ok"] = false
		}
	} else {
		b, _ := io.ReadAll(io.LimitReader(rc, 1024))
		rc.Close()
		ready = strings.TrimSpace(string(b))
	}
	r["readyz"] = ready
	var nodes struct {
		Items []struct {
			Status struct {
				Conditions []struct{ Type, Status string } `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.c.get(ctx, "/api/v1/nodes", url.Values{"limit": {"500"}}, &nodes); err == nil {
		n := 0
		for _, it := range nodes.Items {
			for _, c := range it.Status.Conditions {
				if c.Type == "Ready" && c.Status == "True" {
					n++
				}
			}
		}
		r["nodes_ready"] = strconv.Itoa(n) + "/" + strconv.Itoa(len(nodes.Items))
	}
	r["latency_ms"] = time.Since(start).Milliseconds()
	return e.Row(r)
}

const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json"

func (k *K8s) getVerb(ctx context.Context, c connector.Call, e connector.Emit) error {
	kind, name, _ := splitRef(c.Args)
	if kind == "" {
		return errs.New(errs.Usage, "get <kind> [name]")
	}
	res, err := k.c.resolve(ctx, kind)
	if err != nil {
		return err
	}
	ns := k.nsOf(c)
	if allNS(c) && name == "" {
		ns = ""
	}
	q := url.Values{}
	if s := selector(c); s != "" {
		q.Set("labelSelector", s)
	}
	if f := c.Flags["field-selector"]; f != "" {
		q.Set("fieldSelector", f)
	}
	if name == "" {
		q.Set("limit", strconv.Itoa(k.rowCap(c)))
	}
	if o := c.Flags["o"]; o == "yaml" || o == "json" {
		var obj map[string]any
		if name != "" {
			u, err := k.c.ri(res, ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return k.c.mapErr(ctx, err)
			}
			obj = u.Object
		} else {
			l, err := k.c.ri(res, ns).List(ctx, metav1.ListOptions{LabelSelector: selector(c), FieldSelector: c.Flags["field-selector"], Limit: int64(k.rowCap(c))})
			if err != nil {
				return k.c.mapErr(ctx, err)
			}
			obj = l.UnstructuredContent()
		}
		return k.emitObject(obj, o, c, e)
	}
	q.Set("includeObject", "Metadata")
	rc, err := k.c.stream(ctx, "GET", res.path(ns, name), q, nil, "", tableAccept)
	if err != nil {
		return err
	}
	defer rc.Close()
	var tbl struct {
		Kind    string `json:"kind"`
		Columns []struct {
			Name     string `json:"name"`
			Priority int    `json:"priority"`
		} `json:"columnDefinitions"`
		Rows []struct {
			Cells  []any `json:"cells"`
			Object struct {
				Metadata struct {
					Namespace string `json:"namespace"`
				} `json:"metadata"`
			} `json:"object"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(io.LimitReader(rc, 64<<20)).Decode(&tbl); err != nil {
		return errs.Wrap(errs.Runtime, err, "decode table")
	}
	if tbl.Kind != "Table" {
		return errs.New(errs.Unsupported, "API server did not return a table for %s", res.Name)
	}
	for _, row := range tbl.Rows {
		r := connector.Row{}
		if res.Namespaced {
			r["namespace"] = row.Object.Metadata.Namespace
		}
		for i, col := range tbl.Columns {
			if i < len(row.Cells) && (col.Priority == 0 || flagTrue(c, "wide")) {
				r[strings.ToLower(strings.ReplaceAll(col.Name, " ", "_"))] = jsonValue(row.Cells[i])
			}
		}
		if err := e.Row(r); err != nil {
			return err
		}
	}
	return nil
}

func (k *K8s) rowCap(c connector.Call) int {
	n := k.t.Limits.Rows
	if n <= 0 {
		n = connector.DefaultLimits.Rows
	}
	if c.Limit > 0 && c.Limit < n {
		n = c.Limit
	}
	return n
}

// emitObject prints an object as YAML/JSON text lines (managedFields dropped).
func (k *K8s) emitObject(obj map[string]any, format string, c connector.Call, e connector.Emit) error {
	stripManaged(obj)
	redactSecrets(obj)
	var b []byte
	if format == "json" {
		b, _ = json.MarshalIndent(obj, "", "  ")
	} else {
		b, _ = yaml.Marshal(obj)
	}
	max := k.lineCap(c)
	for i, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if i >= max {
			return e.Text("… truncated")
		}
		if err := e.Text(l); err != nil {
			return err
		}
	}
	return nil
}

// redactSecrets masks the values of Secret .data/.stringData (keys kept, value
// "***") in a single object or any list of objects, so get -o yaml|json and
// describe never print secret material (incl. service-account tokens).
func redactSecrets(obj map[string]any) { redactSecretsAs(obj, false) }

// API list items carry no kind; inSecretList marks items of a SecretList.
func redactSecretsAs(obj map[string]any, inSecretList bool) {
	kind, _ := obj["kind"].(string)
	if items, ok := obj["items"].([]any); ok {
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				redactSecretsAs(m, kind == "SecretList")
			}
		}
	}
	if kind != "Secret" && !(inSecretList && kind == "") {
		return
	}
	for _, f := range []string{"data", "stringData"} {
		if d, ok := obj[f].(map[string]any); ok {
			for key := range d {
				d[key] = "***"
			}
		}
	}
}

func stripManaged(obj map[string]any) {
	if md, ok := obj["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
		if an, ok := md["annotations"].(map[string]any); ok {
			delete(an, "kubectl.kubernetes.io/last-applied-configuration")
		}
	}
	if items, ok := obj["items"].([]any); ok {
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				stripManaged(m)
			}
		}
	}
}

func (k *K8s) describe(ctx context.Context, c connector.Call, e connector.Emit) error {
	kind, name, _ := splitRef(c.Args)
	if kind == "" || name == "" {
		return errs.New(errs.Usage, "describe <kind> <name>")
	}
	res, err := k.c.resolve(ctx, kind)
	if err != nil {
		return err
	}
	ns := k.nsOf(c)
	u, err := k.c.ri(res, ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return k.c.mapErr(ctx, err)
	}
	if err := k.emitObject(u.Object, "yaml", c, e); err != nil {
		return err
	}
	if err := e.Text("--- events"); err != nil {
		return err
	}
	fs := "involvedObject.name=" + name + ",involvedObject.kind=" + res.Kind
	evs, err := k.listEvents(ctx, nsIf(res.Namespaced, ns), fs, 0)
	if err != nil {
		return e.Text("(events unavailable: " + string(errs.CodeOf(err)) + ")")
	}
	for _, ev := range evs {
		if err := e.Text(ev["last_seen"].(string) + "  " + ev["type"].(string) + "  " + ev["reason"].(string) + "  " + ev["message"].(string)); err != nil {
			return err
		}
	}
	return nil
}

func nsIf(b bool, ns string) string {
	if b {
		return ns
	}
	return ""
}

type eventList struct {
	Items []struct {
		Metadata struct {
			Namespace         string `json:"namespace"`
			CreationTimestamp string `json:"creationTimestamp"`
		} `json:"metadata"`
		Type           string `json:"type"`
		Reason         string `json:"reason"`
		Message        string `json:"message"`
		Count          int    `json:"count"`
		LastTimestamp  string `json:"lastTimestamp"`
		EventTime      string `json:"eventTime"`
		InvolvedObject struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"involvedObject"`
	} `json:"items"`
}

func (k *K8s) listEvents(ctx context.Context, ns, fieldSel string, since time.Duration) ([]connector.Row, error) {
	q := url.Values{"limit": {"1000"}}
	if fieldSel != "" {
		q.Set("fieldSelector", fieldSel)
	}
	p := "/api/v1/events"
	if ns != "" {
		p = "/api/v1/namespaces/" + url.PathEscape(ns) + "/events"
	}
	var l eventList
	if err := k.c.get(ctx, p, q, &l); err != nil {
		return nil, err
	}
	var out []connector.Row
	cut := time.Time{}
	if since > 0 {
		cut = time.Now().Add(-since)
	}
	for _, ev := range l.Items {
		ts := ev.LastTimestamp
		if ts == "" {
			ts = ev.EventTime
		}
		if ts == "" {
			ts = ev.Metadata.CreationTimestamp
		}
		if t, err := time.Parse(time.RFC3339, ts); err == nil && !cut.IsZero() && t.Before(cut) {
			continue
		} else if err == nil {
			ts = t.UTC().Format(time.RFC3339)
		}
		out = append(out, connector.Row{"last_seen": ts, "namespace": ev.Metadata.Namespace, "type": ev.Type, "reason": ev.Reason,
			"object": strings.ToLower(ev.InvolvedObject.Kind) + "/" + ev.InvolvedObject.Name, "count": ev.Count, "message": ev.Message})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["last_seen"].(string) < out[j]["last_seen"].(string) })
	return out, nil
}

func (k *K8s) events(ctx context.Context, c connector.Call, e connector.Emit) error {
	var fs []string
	if len(c.Args) > 0 {
		_, name, _ := splitRef(c.Args)
		if name == "" {
			name = c.Args[0]
		}
		fs = append(fs, "involvedObject.name="+name)
	}
	if t := c.Flags["type"]; t != "" {
		fs = append(fs, "type="+t)
	}
	ns := k.nsOf(c)
	if allNS(c) {
		ns = ""
	}
	evs, err := k.listEvents(ctx, ns, strings.Join(fs, ","), c.Since)
	if err != nil {
		return err
	}
	if n := k.rowCap(c); len(evs) > n { // newest n
		evs = evs[len(evs)-n:]
	}
	for _, r := range evs {
		if err := e.Row(r); err != nil {
			return err
		}
	}
	return nil
}

func (k *K8s) top(ctx context.Context, c connector.Call, e connector.Emit) error {
	what := "pods"
	if len(c.Args) > 0 {
		what = c.Args[0]
	}
	var p string
	switch what {
	case "pod", "pods", "po":
		ns := k.nsOf(c)
		if allNS(c) {
			p = "/apis/metrics.k8s.io/v1beta1/pods"
		} else {
			p = "/apis/metrics.k8s.io/v1beta1/namespaces/" + url.PathEscape(ns) + "/pods"
		}
	case "node", "nodes", "no":
		p = "/apis/metrics.k8s.io/v1beta1/nodes"
	default:
		return errs.New(errs.Usage, "top pods|nodes")
	}
	q := url.Values{}
	if s := selector(c); s != "" {
		q.Set("labelSelector", s)
	}
	var l struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Usage      map[string]string `json:"usage"`
			Containers []struct {
				Usage map[string]string `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	if err := k.c.get(ctx, p, q, &l); err != nil {
		if errs.CodeOf(err) == errs.NotFound {
			return errs.New(errs.Unsupported, "metrics API (metrics.k8s.io) is not available on this cluster")
		}
		return err
	}
	for _, it := range l.Items {
		r := connector.Row{"name": it.Metadata.Name}
		if it.Metadata.Namespace != "" {
			r["namespace"] = it.Metadata.Namespace
		}
		if it.Usage != nil {
			r["cpu"], r["memory"] = it.Usage["cpu"], it.Usage["memory"]
		} else {
			var cpu, mem []string
			for _, ct := range it.Containers {
				cpu, mem = append(cpu, ct.Usage["cpu"]), append(mem, ct.Usage["memory"])
			}
			r["cpu"], r["memory"] = strings.Join(cpu, "+"), strings.Join(mem, "+")
		}
		if err := e.Row(r); err != nil {
			return err
		}
	}
	return nil
}

// ---- logs ----

func (k *K8s) logs(ctx context.Context, c connector.Call, e connector.Emit) error {
	if len(c.Args) == 0 {
		return errs.New(errs.Usage, "logs <pod>|<kind>/<name>")
	}
	ns := k.nsOf(c)
	pod := c.Args[0]
	if strings.Contains(pod, "/") {
		kind, name, _ := splitRef(c.Args[:1])
		p, err := k.podFor(ctx, ns, kind, name, false)
		if err != nil {
			return err
		}
		pod = p
	}
	max := k.lineCap(c)
	q := url.Values{"tailLines": {strconv.Itoa(max)}}
	lb := k.t.Limits.Bytes
	if lb <= 0 {
		lb = connector.DefaultLimits.Bytes
	}
	q.Set("limitBytes", strconv.Itoa(lb))
	if c.Since > 0 {
		q.Set("sinceSeconds", strconv.Itoa(max64(1, int(c.Since.Seconds()))))
	}
	if ct := c.Flags["container"]; ct != "" {
		q.Set("container", ct)
	} else if ct := c.Flags["c"]; ct != "" {
		q.Set("container", ct)
	}
	if flagTrue(c, "previous") {
		q.Set("previous", "true")
	}
	if flagTrue(c, "timestamps") {
		q.Set("timestamps", "true")
	}
	rc, err := k.c.stream(ctx, "GET", "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods/"+url.PathEscape(pod)+"/log", q, nil, "", "text/plain")
	if err != nil {
		return err
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		if n >= max {
			return nil
		}
		n++
		if err := e.Text(sc.Text()); err != nil {
			return err
		}
	}
	return nil
}

func max64(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// podFor picks a pod for kind/name (deploy, sts, ds, rs, job, svc, pod),
// preferring running+ready pods.
func (k *K8s) podFor(ctx context.Context, ns, kind, name string, needReady bool) (string, error) {
	res, err := k.c.resolve(ctx, kind)
	if err != nil {
		return "", err
	}
	if res.Name == "pods" {
		return name, nil
	}
	sel, err := k.selectorOf(ctx, ns, res, name)
	if err != nil {
		return "", err
	}
	return k.firstPod(ctx, ns, sel)
}

// selectorOf returns the pod label selector of workload res/name.
func (k *K8s) selectorOf(ctx context.Context, ns string, res resource, name string) (string, error) {
	var obj struct {
		Spec struct {
			Selector json.RawMessage `json:"selector"`
		} `json:"spec"`
	}
	if err := k.c.get(ctx, res.path(ns, name), nil, &obj); err != nil {
		return "", err
	}
	return selectorString(res.Name, obj.Spec.Selector)
}

func selectorString(resName string, raw json.RawMessage) (string, error) {
	labels := map[string]string{}
	if resName == "services" {
		_ = json.Unmarshal(raw, &labels)
	} else {
		var ls struct {
			MatchLabels map[string]string `json:"matchLabels"`
		}
		_ = json.Unmarshal(raw, &ls)
		labels = ls.MatchLabels
	}
	if len(labels) == 0 {
		return "", errs.New(errs.Unsupported, "%s has no label selector to find pods", resName)
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return strings.Join(parts, ","), nil
}

type podList struct {
	Items []podObj `json:"items"`
}

type podObj struct {
	Metadata struct {
		Name              string  `json:"name"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		Containers []struct {
			Ports []struct {
				Name          string `json:"name"`
				ContainerPort int    `json:"containerPort"`
			} `json:"ports"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase      string                          `json:"phase"`
		Conditions []struct{ Type, Status string } `json:"conditions"`
	} `json:"status"`
}

func (p podObj) ready() bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			return true
		}
	}
	return false
}

func (k *K8s) pods(ctx context.Context, ns, sel string) ([]podObj, error) {
	var l podList
	err := k.c.get(ctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods", url.Values{"labelSelector": {sel}, "limit": {"200"}}, &l)
	return l.Items, err
}

func (k *K8s) firstPod(ctx context.Context, ns, sel string) (string, error) {
	return k.pickPod(ctx, ns, sel, false)
}

// readyPod is firstPod for port-forward: only a Running+Ready pod that is
// not terminating will do (kubectl port-forward semantics); otherwise a
// NotFound error that says why.
func (k *K8s) readyPod(ctx context.Context, ns, sel string) (string, error) {
	return k.pickPod(ctx, ns, sel, true)
}

func (k *K8s) pickPod(ctx context.Context, ns, sel string, strict bool) (string, error) {
	ps, err := k.pods(ctx, ns, sel)
	if err != nil {
		return "", err
	}
	best := ""
	for _, p := range ps {
		if p.Status.Phase == "Running" && p.ready() && p.Metadata.DeletionTimestamp == nil {
			return p.Metadata.Name, nil
		}
		if strict {
			continue
		}
		if best == "" {
			best = p.Metadata.Name
		}
	}
	if strict && len(ps) > 0 {
		return "", errs.New(errs.NotFound, "no ready pod matches %s in %s (%d match, none Running+Ready)", sel, ns, len(ps)).
			WithHint("check the workload: tmvp <env> <k8s system> get pods -F selector=" + sel)
	}
	if best == "" {
		return "", errs.New(errs.NotFound, "no pods match %s in %s", sel, ns)
	}
	return best, nil
}
