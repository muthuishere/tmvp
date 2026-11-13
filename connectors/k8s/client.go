package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/httpstream"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// client wraps client-go for one connection: a rest.Config whose Dial is the
// target's via-aware Dialer, a dynamic client, memory-cached discovery and a raw *http.Client (same auth/TLS/transport chain) for the few
// endpoints the typed/dynamic clients don't cover (tables, logs, metrics,
// readyz). A read-only client refuses every non-GET/HEAD request in a
// WrapTransport, i.e. before it reaches the network.
type client struct {
	rc       *restConfig
	cfg      *rest.Config
	hc       *http.Client
	dyn      dynamic.Interface
	dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	direct   bool // no via: chain (websocket transports may be used)
	readOnly bool

	discMu sync.Mutex
	res    []resource
}

// newClient builds the client. allowPortForward lets a read-only client make
// the (POST) SPDY port-forward upgrade — the k8s tunnel is opened in read mode.
func newClient(rc *restConfig, d connector.Dialer, readOnly, allowPortForward bool) (*client, error) {
	cfg := rest.CopyConfig(rc.cfg)
	cfg.Dial = d.DialContext
	cfg.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil } // never route cluster traffic through an env proxy
	cfg.UserAgent = "tmvp"
	cfg.WarningHandler = rest.NoWarnings{}
	cfg.QPS, cfg.Burst = 50, 100
	if readOnly {
		cfg.WrapTransport = readOnlyTransport(allowPortForward)
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, errs.New(errs.Config, "kubeconfig: %s", scrubHost(err.Error(), rc.Server))
	}
	dyn, err := dynamic.NewForConfigAndClient(cfg, hc)
	if err != nil {
		return nil, errs.New(errs.Config, "kubeconfig: %s", scrubHost(err.Error(), rc.Server))
	}
	_, direct := d.(*net.Dialer)
	return &client{rc: rc, cfg: cfg, hc: hc, dyn: dyn, dial: d.DialContext,
		direct: direct, readOnly: readOnly}, nil
}

const readOnlyMsg = "read connection cannot"

// readOnlyRT rejects mutating requests before they are sent. It exposes the
// next round tripper (utilnet.RoundTripperWrapper) so closing idle
// connections reaches the base transport.
type readOnlyRT struct {
	next             http.RoundTripper
	allowPortForward bool
}

func (t *readOnlyRT) WrappedRoundTripper() http.RoundTripper { return t.next }

func (t *readOnlyRT) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
	case t.allowPortForward && r.Method == http.MethodPost && isPortForward(r):
	default:
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, errs.New(errs.PolicyBlocked, readOnlyMsg+" %s", r.Method).WithHint(`use tmvp write <env> <system> ... --reason "why"`)
	}
	return t.next.RoundTrip(r)
}

// readOnlyTransport wraps the transport with the read guard.
func readOnlyTransport(allowPortForward bool) func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		return &readOnlyRT{next: next, allowPortForward: allowPortForward}
	}
}

// portForwardPath is .../api/v1/namespaces/<ns>/pods/<pod>/portforward.
var portForwardPath = regexp.MustCompile(`/api/v1/namespaces/[^/]+/pods/[^/]+/portforward$`)

// isPortForward: the pod portforward subresource, as a protocol upgrade —
// not any POST whose path happens to end in /portforward. (The SPDY round
// tripper adds Upgrade below this wrapper; the stream protocol header is set
// by the dialer before.)
func isPortForward(r *http.Request) bool {
	return portForwardPath.MatchString(r.URL.Path) && (r.Header.Get("Upgrade") != "" || r.Header.Get(httpstream.HeaderProtocolVersion) != "")
}

// close drops the client's idle keep-alive connections (through the
// client-go wrappers, which http.Client.CloseIdleConnections cannot see past).
func (c *client) close() { utilnet.CloseIdleConnectionsFor(c.hc.Transport) }

func (c *client) url(path string, q url.Values) *url.URL {
	u := *c.rc.Server
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = q.Encode()
	return &u
}

// stream performs a raw request and returns the open body on 2xx.
func (c *client) stream(ctx context.Context, method, path string, q url.Values, body io.Reader, ctype, accept string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url(path, q).String(), body)
	if err != nil {
		return nil, errs.Wrap(errs.Runtime, err, "")
	}
	req.Header.Set("Accept", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, c.mapErr(ctx, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var st struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &st)
		return nil, c.statusErr(resp.StatusCode, st.Message)
	}
	return resp.Body, nil
}

// do performs a raw request and decodes JSON into out (if non-nil).
func (c *client) do(ctx context.Context, method, path string, q url.Values, body []byte, ctype string, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	rc, err := c.stream(ctx, method, path, q, rd, ctype, "")
	if err != nil {
		return err
	}
	defer rc.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(rc, 64<<20)).Decode(out); err != nil {
		return errs.Wrap(errs.Runtime, err, "decode %s", path)
	}
	return nil
}

func (c *client) get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, q, nil, "", out)
}

// mapErr turns a client-go / net/http error into a tmvp error. Messages never
// carry the server URL or credentials.
func (c *client) mapErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if e := errs.From(err); e != nil {
		return e
	}
	var st apierrors.APIStatus
	if errors.As(err, &st) {
		s := st.Status()
		return c.statusErr(int(s.Code), s.Message)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (ctx != nil && ctx.Err() != nil) {
		return errs.New(errs.Timeout, "kubernetes API: request timed out")
	}
	var ue *url.Error
	isNet := errors.As(err, &ue)
	if isNet {
		err = ue.Err
	}
	msg := scrubHost(err.Error(), c.rc.Server)
	if i := strings.Index(msg, readOnlyMsg); i >= 0 { // the read guard, flattened by a streaming dialer
		m := strings.TrimSpace(strings.SplitN(msg[i:], "(", 2)[0])
		return errs.New(errs.PolicyBlocked, "%s", m).WithHint(`use tmvp write <env> <system> ... --reason "why"`)
	}
	if strings.Contains(msg, "getting credentials") || strings.Contains(msg, "exec plugin") {
		return errs.New(errs.Auth, "kubeconfig exec credential plugin failed").
			WithHint("check the plugin is installed, on PATH and logged in")
	}
	var ne net.Error
	if isNet || errors.As(err, &ne) {
		return errs.New(errs.Unreachable, "kubernetes API: %s", msg)
	}
	return errs.New(errs.Runtime, "kubernetes API: %s", msg)
}

// statusErr maps a Kubernetes API status code to a tmvp error.
func (c *client) statusErr(code int, msg string) error {
	msg = scrubHost(msg, c.rc.Server)
	if msg == "" {
		msg = http.StatusText(code)
	}
	switch code {
	case http.StatusUnauthorized:
		return errs.AuthError(fmt.Errorf("kubernetes API: %s", http.StatusText(code)))
	case http.StatusForbidden:
		return errs.New(errs.PolicyBlocked, "forbidden: %s", msg).WithHint("the credential for this mode lacks RBAC permission")
	case http.StatusNotFound:
		return errs.New(errs.NotFound, "%s", msg)
	case http.StatusConflict, http.StatusUnprocessableEntity, http.StatusBadRequest:
		return errs.New(errs.Usage, "%s", msg)
	case http.StatusGatewayTimeout, http.StatusRequestTimeout:
		return errs.New(errs.Timeout, "%s", msg)
	case http.StatusTooManyRequests:
		return errs.New(errs.GuardRejected, "%s", msg)
	}
	return errs.New(errs.Runtime, "kubernetes API %d: %s", code, msg)
}

// ---- discovery ----

type resource struct {
	Group, Version, Name, Kind, Singular string
	Namespaced                           bool
	Short                                []string
	Verbs                                []string
}

func (r resource) gvr() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: r.Group, Version: r.Version, Resource: r.Name}
}

func (r resource) prefix() string {
	if r.Group == "" {
		return "/api/" + r.Version
	}
	return "/apis/" + r.Group + "/" + r.Version
}

// path builds .../[namespaces/ns/]<resource>[/name] (raw requests).
func (r resource) path(ns, name string) string {
	p := r.prefix()
	if r.Namespaced && ns != "" {
		p += "/namespaces/" + url.PathEscape(ns)
	}
	p += "/" + r.Name
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

// ri is the dynamic client for r in ns ("" = all namespaces / cluster-scoped).
func (c *client) ri(r resource, ns string) dynamic.ResourceInterface {
	nri := c.dyn.Resource(r.gvr())
	if r.Namespaced && ns != "" {
		return nri.Namespace(ns)
	}
	return nri
}

// discover lists API resources (preferred versions), cached in memory for
// the connector's lifetime (warm state). It is a small legacy-discovery walk
// over the client-go transport rather than client-go/discovery: that package
// imports client-go/kubernetes/scheme (every k8s.io/api group + openapi),
// which roughly doubled the tmvp binary.
func (c *client) discover(ctx context.Context) ([]resource, error) {
	c.discMu.Lock()
	defer c.discMu.Unlock()
	if c.res != nil {
		return c.res, nil
	}
	var groups metav1.APIGroupList
	if err := c.get(ctx, "/apis", nil, &groups); err != nil {
		return nil, err
	}
	gvs := []string{"v1"}
	for _, g := range groups.Groups {
		gvs = append(gvs, g.PreferredVersion.GroupVersion)
	}
	lists := make([]*metav1.APIResourceList, len(gvs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	var firstErr error
	var errMu sync.Mutex
	for i, gv := range gvs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p := "/apis/" + gv
			if gv == "v1" {
				p = "/api/v1"
			}
			var l metav1.APIResourceList
			if err := c.get(ctx, p, nil, &l); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				return
			}
			if l.GroupVersion == "" {
				l.GroupVersion = gv
			}
			lists[i] = &l
		}()
	}
	wg.Wait()
	var out []resource
	for _, l := range lists {
		if l == nil {
			continue
		}
		gv, perr := schema.ParseGroupVersion(l.GroupVersion)
		if perr != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") {
				continue
			}
			out = append(out, resource{Group: gv.Group, Version: gv.Version, Name: r.Name, Kind: r.Kind, Singular: r.SingularName,
				Namespaced: r.Namespaced, Short: r.ShortNames, Verbs: r.Verbs})
		}
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, errs.New(errs.Runtime, "kubernetes API discovery returned nothing")
	}
	c.res = out
	return out, nil
}

// resolve maps "po", "pods", "pod", "Deployment", "deploy.apps", "ingresses.networking.k8s.io".
func (c *client) resolve(ctx context.Context, s string) (resource, error) {
	rs, err := c.discover(ctx)
	if err != nil {
		return resource{}, err
	}
	s = strings.ToLower(strings.TrimSpace(s))
	name, group, _ := strings.Cut(s, ".")
	for _, r := range rs {
		if group != "" && r.Group != group {
			continue
		}
		if r.Name == name || r.Singular == name || strings.ToLower(r.Kind) == name {
			return r, nil
		}
		for _, sh := range r.Short {
			if sh == name {
				return r, nil
			}
		}
	}
	return resource{}, errs.New(errs.NotFound, "the server has no resource type %q", s)
}

// resolveGVK finds the resource for apiVersion+kind (apply).
func (c *client) resolveGVK(ctx context.Context, apiVersion, kind string) (resource, error) {
	rs, err := c.discover(ctx)
	if err != nil {
		return resource{}, err
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return resource{}, errs.New(errs.Usage, "bad apiVersion %q", apiVersion)
	}
	for _, r := range rs {
		if r.Group == gv.Group && r.Kind == kind {
			r.Version = gv.Version
			return r, nil
		}
	}
	return resource{}, errs.New(errs.NotFound, "the server has no kind %s in %s", kind, apiVersion)
}
