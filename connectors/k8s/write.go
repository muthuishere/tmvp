package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func dryRun(c connector.Call) []string {
	if c.Preview {
		return []string{metav1.DryRunAll}
	}
	return nil
}

func resultWord(c connector.Call, s string) string {
	if c.Preview {
		return s + " (dry run)"
	}
	return s
}

// apply: server-side apply of one or more YAML/JSON documents.
func (k *K8s) apply(ctx context.Context, c connector.Call, e connector.Emit) error {
	if len(c.Args) == 0 {
		return errs.New(errs.Usage, "apply '<yaml>' | apply @manifest")
	}
	src := strings.Join(c.Args, " ")
	if strings.HasPrefix(c.Args[0], "@") {
		s, err := k.t.LoadSaved("manifests", c.Args[0], ".yaml")
		if err != nil {
			return errs.Wrap(errs.NotFound, err, "")
		}
		src = s.Body
	}
	dec := yaml.NewDecoder(strings.NewReader(src))
	var docs []map[string]any
	for {
		var d map[string]any
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errs.New(errs.Usage, "manifest is not valid YAML: %v", err)
		}
		if len(d) > 0 {
			docs = append(docs, d)
		}
	}
	if len(docs) == 0 {
		return errs.New(errs.Usage, "manifest is empty")
	}
	for _, d := range docs {
		apiv, _ := d["apiVersion"].(string)
		kind, _ := d["kind"].(string)
		md, _ := d["metadata"].(map[string]any)
		name, _ := md["name"].(string)
		if apiv == "" || kind == "" || name == "" {
			return errs.New(errs.Usage, "every document needs apiVersion, kind and metadata.name")
		}
		res, err := k.c.resolveGVK(ctx, apiv, kind)
		if err != nil {
			return err
		}
		ns := ""
		if res.Namespaced {
			ns, _ = md["namespace"].(string)
			if ns == "" {
				ns = k.nsOf(c)
			}
		}
		body, err := json.Marshal(d)
		if err != nil {
			return errs.Wrap(errs.Usage, err, "manifest")
		}
		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(body); err != nil {
			return errs.New(errs.Usage, "manifest: %v", err)
		}
		if res.Namespaced {
			obj.SetNamespace(ns)
		}
		if _, err := k.c.ri(res, ns).Apply(ctx, name, obj, metav1.ApplyOptions{FieldManager: "tmvp", Force: true, DryRun: dryRun(c)}); err != nil {
			return k.c.mapErr(ctx, err)
		}
		if err := e.Row(connector.Row{"kind": kind, "name": name, "namespace": ns, "result": resultWord(c, "applied")}); err != nil {
			return err
		}
	}
	return nil
}

func (k *K8s) scale(ctx context.Context, c connector.Call, e connector.Emit) error {
	kind, name, rest := splitRef(c.Args)
	if name == "" || len(rest) == 0 {
		return errs.New(errs.Usage, "scale <kind>/<name> <replicas>")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(rest[0], "--replicas="))
	if err != nil || n < 0 {
		return errs.New(errs.Usage, "replicas must be a non-negative number")
	}
	res, err := k.c.resolve(ctx, kind)
	if err != nil {
		return err
	}
	ns := k.nsOf(c)
	cur, err := k.c.ri(res, ns).Get(ctx, name, metav1.GetOptions{}, "scale")
	if err != nil {
		return k.c.mapErr(ctx, err)
	}
	from, _, _ := unstructured.NestedInt64(cur.Object, "spec", "replicas")
	body, _ := json.Marshal(map[string]any{"spec": map[string]any{"replicas": n}})
	if _, err := k.c.ri(res, ns).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{DryRun: dryRun(c)}, "scale"); err != nil {
		return k.c.mapErr(ctx, err)
	}
	return e.Row(connector.Row{"kind": res.Kind, "name": name, "namespace": ns, "from": int(from), "to": n, "result": resultWord(c, "scaled")})
}

func (k *K8s) restart(ctx context.Context, c connector.Call, e connector.Emit) error {
	kind, name, _ := splitRef(c.Args)
	if name == "" {
		return errs.New(errs.Usage, "restart deploy/<name>")
	}
	res, err := k.c.resolve(ctx, kind)
	if err != nil {
		return err
	}
	switch res.Name {
	case "deployments", "statefulsets", "daemonsets":
	default:
		return errs.New(errs.Usage, "restart supports deployments, statefulsets, daemonsets (got %s)", res.Name)
	}
	ns := k.nsOf(c)
	patch := map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": map[string]any{"kubectl.kubernetes.io/restartedAt": time.Now().UTC().Format(time.RFC3339)}}}}}
	body, _ := json.Marshal(patch)
	if _, err := k.c.ri(res, ns).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{DryRun: dryRun(c)}); err != nil {
		return k.c.mapErr(ctx, err)
	}
	return e.Row(connector.Row{"kind": res.Kind, "name": name, "namespace": ns, "result": resultWord(c, "restarted")})
}

// rolloutUndo rolls a deployment back to the previous (or -F to-revision) ReplicaSet template.
func (k *K8s) rolloutUndo(ctx context.Context, c connector.Call, e connector.Emit) error {
	kind, name, _ := splitRef(c.Args[1:])
	if name == "" {
		return errs.New(errs.Usage, "rollout undo deploy/<name>")
	}
	res, err := k.c.resolve(ctx, kind)
	if err != nil {
		return err
	}
	if res.Name != "deployments" {
		return errs.New(errs.Unsupported, "rollout undo supports deployments only")
	}
	ns := k.nsOf(c)
	dep, err := k.c.ri(res, ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return k.c.mapErr(ctx, err)
	}
	rawSel, _, _ := unstructured.NestedFieldNoCopy(dep.Object, "spec", "selector")
	selJSON, _ := json.Marshal(rawSel)
	sel, err := selectorString("deployments", selJSON)
	if err != nil {
		return err
	}
	var rsl struct {
		Items []struct {
			Metadata struct {
				Name            string            `json:"name"`
				Annotations     map[string]string `json:"annotations"`
				OwnerReferences []struct {
					UID string `json:"uid"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				Template map[string]any `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := k.c.get(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(ns)+"/replicasets", url.Values{"labelSelector": {sel}}, &rsl); err != nil {
		return err
	}
	cur, _ := strconv.Atoi(dep.GetAnnotations()["deployment.kubernetes.io/revision"])
	want, _ := strconv.Atoi(c.Flags["to-revision"])
	type cand struct {
		rev  int
		name string
		tmpl map[string]any
	}
	var cands []cand
	for _, rs := range rsl.Items {
		owned := false
		for _, o := range rs.Metadata.OwnerReferences {
			owned = owned || o.UID == string(dep.GetUID())
		}
		rev, err := strconv.Atoi(rs.Metadata.Annotations["deployment.kubernetes.io/revision"])
		if !owned || err != nil {
			continue
		}
		cands = append(cands, cand{rev, rs.Metadata.Name, rs.Spec.Template})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].rev > cands[j].rev })
	var pick *cand
	for i := range cands {
		if (want > 0 && cands[i].rev == want) || (want == 0 && cands[i].rev < cur) {
			pick = &cands[i]
			break
		}
	}
	if pick == nil {
		return errs.New(errs.NotFound, "no previous revision to roll back to")
	}
	if md, ok := pick.tmpl["metadata"].(map[string]any); ok {
		if l, ok := md["labels"].(map[string]any); ok {
			delete(l, "pod-template-hash")
		}
	}
	body, _ := json.Marshal([]map[string]any{{"op": "replace", "path": "/spec/template", "value": pick.tmpl}})
	if _, err := k.c.ri(res, ns).Patch(ctx, name, types.JSONPatchType, body, metav1.PatchOptions{DryRun: dryRun(c)}); err != nil {
		return k.c.mapErr(ctx, err)
	}
	return e.Row(connector.Row{"kind": "Deployment", "name": name, "namespace": ns, "from_revision": cur, "to_revision": pick.rev,
		"replicaset": pick.name, "result": resultWord(c, "rolled back")})
}

func (k *K8s) delete(ctx context.Context, c connector.Call, e connector.Emit) error {
	if why := destructiveDelete(c); why != "" && !(c.Destructive || k.t.Destructive) {
		return errs.New(errs.PolicyBlocked, "destructive delete (%s) blocked: destructive: false here", why)
	}
	kind, name, _ := splitRef(c.Args)
	res, err := k.c.resolve(ctx, kind)
	if err != nil {
		return err
	}
	ns := k.nsOf(c)
	bg := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &bg, DryRun: dryRun(c)}
	ri := k.c.ri(res, ns)
	if name != "" {
		if err := ri.Delete(ctx, name, opts); err != nil {
			return k.c.mapErr(ctx, err)
		}
		return e.Row(connector.Row{"kind": res.Kind, "name": name, "namespace": nsIf(res.Namespaced, ns), "result": resultWord(c, "deleted")})
	}
	// collection delete (destructive, already allowed above): list first so the output names what went
	l, err := ri.List(ctx, metav1.ListOptions{LabelSelector: selector(c)})
	if err != nil {
		return k.c.mapErr(ctx, err)
	}
	for _, it := range l.Items {
		if err := ri.Delete(ctx, it.GetName(), opts); err != nil {
			return k.c.mapErr(ctx, err)
		}
		if err := e.Row(connector.Row{"kind": res.Kind, "name": it.GetName(), "namespace": nsIf(res.Namespaced, ns), "result": resultWord(c, "deleted")}); err != nil {
			return err
		}
	}
	return nil
}

func (k *K8s) exec(ctx context.Context, c connector.Call, e connector.Emit) error {
	ns := k.nsOf(c)
	target := c.Args[0]
	pod := target
	if strings.Contains(target, "/") {
		kind, name, _ := splitRef([]string{target})
		p, err := k.podFor(ctx, ns, kind, name, true)
		if err != nil {
			return err
		}
		pod = p
	}
	cmd := c.Args[1:]
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 {
		return errs.New(errs.Usage, "exec <pod> -- <cmd>")
	}
	if c.Preview {
		return e.Row(connector.Row{"pod": pod, "namespace": ns, "command": strings.Join(cmd, " "), "preview": true, "note": "not executed"})
	}
	ct := c.Flags["container"]
	if ct == "" {
		ct = c.Flags["c"]
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sink := &lineSink{e: e, max: k.lineCap(c), stop: cancel}
	code, err := k.c.execStream(sctx, ns, pod, ct, cmd, sink.writer(1), sink.writer(2))
	sink.mu.Lock()
	emitErr, truncated := sink.emitErr, sink.truncated
	sink.mu.Unlock()
	if emitErr != nil {
		return emitErr
	}
	if truncated {
		return nil
	}
	if err != nil {
		return err
	}
	sink.final()
	if sink.emitErr != nil {
		return sink.emitErr
	}
	if code > 0 {
		return errs.New(errs.Runtime, "command exited with status %d", code)
	}
	return nil
}

// lineSink turns exec stdout/stderr into capped text lines. When the cap is
// hit (or emitting fails) it cancels the stream.
type lineSink struct {
	mu        sync.Mutex
	e         connector.Emit
	buf       [3]bytes.Buffer
	n, max    int
	stop      func()
	emitErr   error
	truncated bool
}

type sinkWriter struct {
	s  *lineSink
	id int
}

func (s *lineSink) writer(id int) io.Writer { return sinkWriter{s, id} }

func (w sinkWriter) Write(b []byte) (int, error) {
	s := w.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.truncated || s.emitErr != nil {
		return len(b), nil // discard; the stream is being cancelled
	}
	s.buf[w.id].Write(b)
	s.flush(w.id, false)
	return len(b), nil
}

// flush emits complete lines of stream id (and the remainder when final). Caller holds mu.
func (s *lineSink) flush(id int, final bool) {
	for s.emitErr == nil && !s.truncated {
		b := s.buf[id].Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if !final || len(b) == 0 {
				return
			}
			i = len(b)
		}
		line := string(b[:i])
		s.buf[id].Next(min(i+1, len(b)))
		if s.n >= s.max {
			_ = s.e.Text("… output truncated")
			s.truncated = true
			s.stop()
			return
		}
		s.n++
		if s.emitErr = s.e.Text(line); s.emitErr != nil {
			s.stop()
		}
	}
}

func (s *lineSink) final() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flush(1, true)
	s.flush(2, true)
}
