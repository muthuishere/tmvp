// Package k8s is tmvp's Kubernetes connector and k8s port-forward tunneler.
//
// Config (system fields):
//
//	kubeconfig: "${file:secrets.kubeconfig}"   (bytes in memory; read.kubeconfig / write.kubeconfig override)
//	context:    reqsume-prod                    (read.context / write.context override — e.g. a view-only context for reads)
//	namespace:  app                             (default namespace; -F ns=x overrides per call)
//
// It talks to the API through client-go (rest.Config from the in-memory
// kubeconfig, dynamic client + memory-cached discovery, SPDY/websocket
// exec and port-forward; no kubectl), dialing through the target's via:
// chain. Read connections refuse every non-GET request in the transport,
// before it reaches the network.
//
// Read verbs: ctx, get, describe, logs, events, top, health.
// Write verbs (tmvp write): apply, scale, restart, rollout undo, delete, exec.
// delete of a namespace, a label-selector delete or --all is destructive.
package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() {
	connector.Register("k8s", New)
	connector.RegisterTunneler("k8s", Tunneler{})
}

// New returns an unopened k8s connector.
func New() connector.Connector { return &K8s{} }

// K8s is one API client for one env/system/mode.
type K8s struct {
	t  *connector.Target
	c  *client
	ns string

	allowPortForward bool // set by the tunneler: port-forward is a POST upgrade
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "what exists: saved manifests, then namespaces and workloads/services in the namespace (-F A=true for all)"},
	{Name: "ctx", Mode: connector.Read, Help: "context, cluster, namespace, auth kind"},
	{Name: "health", Mode: connector.Read, Help: "version, readyz, ready nodes, latency"},
	{Name: "get", Mode: connector.Read, Help: "get <kind> [name] [-F selector=app=x] [-F ns=x|-F A=true] [-F o=yaml]"},
	{Name: "describe", Mode: connector.Read, Help: "describe <kind> <name> | <kind>/<name> — object + its events"},
	{Name: "logs", Mode: connector.Read, Help: "logs <pod>|deploy/<name> [-F container=c] [-F previous=true] (bounded tail, --since)"},
	{Name: "events", Mode: connector.Read, Help: "events [name] [-F type=Warning] (--since filters)"},
	{Name: "top", Mode: connector.Read, Help: "top pods|nodes (metrics API)"},
	{Name: "apply", Mode: connector.Write, Default: true, Help: "apply '<yaml>'|@manifest (server-side apply; --preview = dry-run)"},
	{Name: "scale", Mode: connector.Write, Help: "scale <kind>/<name> <replicas>"},
	{Name: "restart", Mode: connector.Write, Help: "restart deploy|sts|ds/<name> (rollout restart)"},
	{Name: "rollout", Mode: connector.Write, Help: "rollout undo deploy/<name> [-F to-revision=N]"},
	{Name: "delete", Mode: connector.Write, Help: "delete <kind> <name> | <kind> -F selector=… (destructive) | ns <name> (destructive)"},
	{Name: "exec", Mode: connector.Write, Help: "exec <pod> [-F container=c] -- <cmd> [args...]"},
}

func (k *K8s) Type() string            { return "k8s" }
func (k *K8s) Verbs() []connector.Verb { return verbs }

// Open parses the kubeconfig from memory and verifies the API answers.
func (k *K8s) Open(ctx context.Context, t *connector.Target) error {
	k.t = t
	rc, err := loadRestConfig(ctx, t)
	if err != nil {
		return err
	}
	if k.c, err = newClient(rc, t.Dialer, t.Mode != connector.ModeWrite, k.allowPortForward); err != nil {
		return err
	}
	k.ns = rc.Namespace
	if ns, _ := t.StringOr(ctx, "namespace", ""); ns != "" {
		k.ns = ns
	}
	if k.ns == "" {
		k.ns = "default"
	}
	var v struct{}
	return k.c.get(ctx, "/version", nil, &v)
}

// loadRestConfig resolves <mode>.kubeconfig|kubeconfig and <mode>.context|context.
func loadRestConfig(ctx context.Context, t *connector.Target) (*restConfig, error) {
	kp := "kubeconfig"
	if t.Has(string(t.Mode) + ".kubeconfig") {
		kp = string(t.Mode) + ".kubeconfig"
	}
	var kb []byte
	raw, ok := t.Raw(kp)
	if !ok {
		return nil, errs.New(errs.Config, "k8s: kubeconfig is required")
	}
	if s, _ := raw.(string); strings.HasPrefix(strings.TrimSpace(s), "${file:") {
		b, err := t.File(ctx, kp)
		if err != nil {
			return nil, err
		}
		kb = b
	} else {
		s, err := t.String(ctx, kp)
		if err != nil {
			return nil, err
		}
		kb = []byte(s)
	}
	cname := ""
	if t.Has(string(t.Mode)+".context") || t.Has("context") {
		s, err := t.ModeString(ctx, "context")
		if err != nil {
			return nil, err
		}
		cname = s
	}
	return parseKubeconfig(kb, cname)
}

// Close drops idle connections.
func (k *K8s) Close() error {
	if k.c != nil {
		k.c.close()
	}
	return nil
}

// Check implements connector.Checker.
func (k *K8s) Check(ctx context.Context) error {
	return k.c.get(ctx, "/version", nil, &struct{}{})
}

// Guard rejects destructive deletes and malformed args before any connection.
func (k *K8s) Guard(c connector.Call) error {
	switch c.Verb {
	case "delete":
		if why := destructiveDelete(c); why != "" && !c.Destructive {
			return errs.New(errs.PolicyBlocked, "destructive delete (%s) blocked: destructive: false here", why).
				WithHint("delete one named object, or set destructive: true on the env/system if this is intended")
		}
		if len(c.Args) == 0 {
			return errs.New(errs.Usage, "delete needs <kind> <name>")
		}
	case "rollout":
		if len(c.Args) < 2 || c.Args[0] != "undo" {
			return errs.New(errs.Usage, "only `rollout undo <kind>/<name>` is supported")
		}
	case "scale":
		if len(c.Args) < 2 {
			return errs.New(errs.Usage, "scale <kind>/<name> <replicas>")
		}
	case "exec":
		if len(c.Args) < 2 {
			return errs.New(errs.Usage, "exec <pod> -- <cmd> [args...]")
		}
	}
	return nil
}

// destructiveDelete explains why a delete call is destructive ("" if not).
func destructiveDelete(c connector.Call) string {
	if len(c.Args) == 0 {
		return ""
	}
	kind, name, _ := splitRef(c.Args)
	switch strings.ToLower(strings.SplitN(kind, ".", 2)[0]) {
	case "ns", "namespace", "namespaces":
		return "namespace"
	case "pv", "persistentvolume", "persistentvolumes", "crd", "crds", "customresourcedefinition", "customresourcedefinitions", "node", "nodes", "no":
		return "cluster-wide " + kind
	}
	if selector(c) != "" {
		return "label-selector delete"
	}
	if name == "" || flagTrue(c, "all") {
		return "delete all " + kind
	}
	return ""
}

// Do runs one verb.
func (k *K8s) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := k.Guard(c); err != nil {
		return err
	}
	if v, ok := connector.FindVerb(verbs, c.Verb); ok && v.Mode != connector.Read && k.t.Mode != connector.ModeWrite {
		return errs.New(errs.PolicyBlocked, "%s needs tmvp write", c.Verb).WithHint(`tmvp write <env> <system> ` + c.Verb + ` ... --reason "why"`)
	}
	tmo := connector.CallTimeout(c, k.t.Limits)
	ctx, cancel := context.WithTimeout(ctx, tmo)
	defer cancel()
	var err error
	switch c.Verb {
	case "ls":
		err = k.ls(ctx, c, e)
	case "ctx":
		err = k.ctxVerb(e)
	case "health":
		err = k.health(ctx, e)
	case "get":
		err = k.getVerb(ctx, c, e)
	case "describe":
		err = k.describe(ctx, c, e)
	case "logs":
		err = k.logs(ctx, c, e)
	case "events":
		err = k.events(ctx, c, e)
	case "top":
		err = k.top(ctx, c, e)
	case "apply":
		err = k.apply(ctx, c, e)
	case "scale":
		err = k.scale(ctx, c, e)
	case "restart":
		err = k.restart(ctx, c, e)
	case "rollout":
		err = k.rolloutUndo(ctx, c, e)
	case "delete":
		err = k.delete(ctx, c, e)
	case "exec":
		err = k.exec(ctx, c, e)
	default:
		return errs.New(errs.Usage, "k8s has no verb %q", c.Verb)
	}
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	if err != nil && ctx.Err() != nil && errs.CodeOf(err) != errs.Auth {
		return errs.New(errs.Timeout, "k8s %s exceeded %s", c.Verb, tmo)
	}
	return err
}

// ---- helpers ----

// splitRef parses "kind/name ..." or "kind name ...".
func splitRef(a []string) (kind, name string, rest []string) {
	if len(a) == 0 {
		return "", "", nil
	}
	if k, n, ok := strings.Cut(a[0], "/"); ok {
		return k, n, a[1:]
	}
	if len(a) > 1 && !strings.HasPrefix(a[1], "-") {
		return a[0], a[1], a[2:]
	}
	return a[0], "", a[1:]
}

func selector(c connector.Call) string {
	if s := c.Flags["selector"]; s != "" {
		return s
	}
	return c.Flags["l"]
}

func flagTrue(c connector.Call, k string) bool {
	v := strings.ToLower(c.Flags[k])
	return v == "true" || v == "1" || v == "yes"
}

func (k *K8s) nsOf(c connector.Call) string {
	if v := c.Flags["ns"]; v != "" {
		return v
	}
	if v := c.Flags["namespace"]; v != "" {
		return v
	}
	return k.ns
}

func allNS(c connector.Call) bool { return flagTrue(c, "A") || flagTrue(c, "all-namespaces") }

func (k *K8s) lineCap(c connector.Call) int {
	n := k.t.Limits.Lines
	if n <= 0 {
		n = connector.DefaultLimits.Lines
	}
	if c.Limit > 0 && c.Limit < n {
		n = c.Limit
	}
	return n
}

func age(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// jsonValue flattens a table cell to a JSON-friendly scalar.
func jsonValue(v any) any {
	switch x := v.(type) {
	case nil, string, bool, float64, int, int64:
		return x
	}
	b, _ := json.Marshal(v)
	return string(b)
}
