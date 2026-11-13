// Package httpc is the tmvp connector for plain HTTP APIs (type: http).
//
// Config fields (mode-aware: read.<key> / write.<key> win):
//
//	base_url     every path is resolved against it; absolute URLs to other hosts are refused
//	token        bearer token (secret) → Authorization: Bearer …
//	user         ┐ basic auth (password is a secret)
//	password     ┘
//	headers      map of extra headers (values may interpolate secrets)
//	health_path  path for `health` (default /health)
//	insecure     true to skip TLS verification (dev only)
//
// Verbs: get <path>, health; post/put/patch <path> [body], delete <path> are
// writes (tmvp write; --preview shows the request without sending it).
// Bodies are capped at the target's byte limit; every request has the
// target's timeout and dials through via:.
package httpc

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() { connector.Register("http", New) }

// New returns an unopened http connector.
func New() connector.Connector { return &HTTP{} }

// HTTP is one client for one env/system/mode.
type HTTP struct {
	t      *connector.Target
	base   *url.URL
	client *http.Client
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — defined APIs (assets apis/*.md + openapi:): name, method, path, mutating, source; offline unless openapi is a URL"},
	{Name: "apis", Mode: connector.Read, Help: "apis [filter] — same as ls, filtered by name/path substring"},
	{Name: "call", Mode: connector.ReadWrite, Saved: true, Help: "call <api|@api> [k=v ...] — run a defined API; {k} fills path params; mutating APIs only via tmvp write (--preview)"},
	{Name: "get", Mode: connector.Read, Help: "get <path> [-F q.key=value] [-F h.Header=value] — JSON → rows, else text"},
	{Name: "health", Mode: connector.Read, Help: "health [path] — status, latency (default health_path or /health)"},
	{Name: "post", Mode: connector.Write, Default: true, Help: "post <path> [body] (tmvp write)"},
	{Name: "put", Mode: connector.Write, Help: "put <path> [body] (tmvp write)"},
	{Name: "patch", Mode: connector.Write, Help: "patch <path> [body] (tmvp write)"},
	{Name: "delete", Mode: connector.Write, Help: "delete <path> (tmvp write)"},
}

func (h *HTTP) Type() string            { return "http" }
func (h *HTTP) Verbs() []connector.Verb { return verbs }

// Guard checks the path shape before Open.
func (h *HTTP) Guard(c connector.Call) error {
	switch c.Verb {
	case "health", "ls", "apis":
		return nil
	case "call":
		if len(c.Args) == 0 {
			return errs.New(errs.Usage, "call needs an api name").WithHint("ls lists them")
		}
		return nil
	}
	if len(c.Args) == 0 {
		return errs.New(errs.Usage, "%s needs a path", c.Verb).WithHint(c.Verb + " /api/status")
	}
	if strings.Contains(c.Args[0], "://") || strings.HasPrefix(c.Args[0], "//") {
		return errs.New(errs.PolicyBlocked, "absolute URLs are not allowed; paths are resolved against base_url").
			WithHint("pass a path such as /api/v1/items")
	}
	return nil
}

// Open parses base_url and builds the client.
func (h *HTTP) Open(ctx context.Context, t *connector.Target) error {
	h.t = t
	raw, err := t.ModeString(ctx, "base_url")
	if err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errs.New(errs.Config, "%s.base_url is not an http(s) URL", t.System)
	}
	if u.User != nil {
		return errs.New(errs.Config, "%s.base_url must not carry credentials; use user/password or token", t.System)
	}
	h.base = u
	insecure, _ := t.StringOr(ctx, "insecure", "false")
	h.client = NewClient(t, insecure == "true")
	return nil
}

// NewClient builds an http.Client that dials through the target's via chain.
func NewClient(t *connector.Target, insecure bool) *http.Client {
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, n, a string) (net.Conn, error) { return t.Dial(ctx, n, a) },
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        4,
		IdleConnTimeout:     5 * time.Minute,
		Proxy:               http.ProxyFromEnvironment,
	}
	if len(t.Via) > 0 {
		tr.Proxy = nil
	}
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in per system
	}
	// no client-wide Timeout: the server's per-call ctx deadline (the
	// effective --timeout / limits.timeout) is the hard kill, so --timeout can
	// raise it above the configured value for one call.
	return &http.Client{Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				return http.ErrUseLastResponse // never forward credentials to another host
			}
			return nil
		}}
}

func (h *HTTP) Close() error {
	if h.client != nil {
		h.client.CloseIdleConnections()
	}
	return nil
}

// Check implements connector.Checker.
func (h *HTTP) Check(ctx context.Context) error {
	return h.Do(ctx, connector.Call{Verb: "health"}, connector.EmitFuncs{})
}

// Do runs one verb.
func (h *HTTP) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := h.Guard(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, connector.CallTimeout(c, h.t.Limits))
	defer cancel()
	switch c.Verb {
	case "ls", "apis":
		f := ""
		if len(c.Args) > 0 {
			f = c.Args[0]
		}
		return h.emitAPIs(ctx, f, e)
	case "call":
		return h.call(ctx, c, e)
	case "health":
		p := "/health"
		if len(c.Args) > 0 {
			p = c.Args[0]
		} else if hp, err := h.t.StringOr(ctx, "health_path", ""); err != nil {
			return err
		} else if hp != "" {
			p = hp
		}
		start := time.Now()
		resp, body, err := h.do(ctx, http.MethodGet, p, c.Flags, nil)
		if err != nil {
			return err
		}
		_ = body
		return ignoreLimit(e.Row(connector.Row{"ok": resp.StatusCode < 400, "status": resp.StatusCode,
			"latency_ms": time.Since(start).Milliseconds()}))
	case "get":
		resp, body, err := h.do(ctx, http.MethodGet, c.Args[0], c.Flags, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			return statusErr(resp.StatusCode, body)
		}
		return EmitBody(e, resp.Header.Get("Content-Type"), body)
	case "post", "put", "patch", "delete":
		method := strings.ToUpper(c.Verb)
		var payload []byte
		if len(c.Args) > 1 {
			payload = []byte(strings.Join(c.Args[1:], " "))
		}
		if c.Preview {
			u, _ := h.resolve(c.Args[0], c.Flags)
			return ignoreLimit(e.Row(connector.Row{"preview": true, "method": method, "url": u.Redacted(), "body_bytes": len(payload)}))
		}
		resp, body, err := h.do(ctx, method, c.Args[0], c.Flags, payload)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			return statusErr(resp.StatusCode, body)
		}
		if err := e.Row(connector.Row{"status": resp.StatusCode}); err != nil {
			return ignoreLimit(err)
		}
		return EmitBody(e, resp.Header.Get("Content-Type"), body)
	}
	return errs.New(errs.Usage, "http has no verb %q", c.Verb)
}

func (h *HTTP) resolve(p string, flags map[string]string) (*url.URL, error) {
	ref, err := url.Parse(p)
	if err != nil {
		return nil, errs.New(errs.Usage, "bad path")
	}
	if ref.IsAbs() || ref.Host != "" {
		return nil, errs.New(errs.PolicyBlocked, "absolute URLs are not allowed")
	}
	u := *h.base
	if strings.HasPrefix(ref.Path, "/") && u.Path != "" && u.Path != "/" {
		ref.Path = strings.TrimSuffix(u.Path, "/") + ref.Path
	} else if !strings.HasPrefix(ref.Path, "/") {
		ref.Path = strings.TrimSuffix(u.Path, "/") + "/" + ref.Path
	}
	out := u.ResolveReference(ref)
	q := out.Query()
	for k, v := range flags {
		if name, ok := strings.CutPrefix(k, "q."); ok {
			q.Set(name, v)
		}
	}
	out.RawQuery = q.Encode()
	return out, nil
}

// do sends one request with auth/headers and reads a bounded body.
func (h *HTTP) do(ctx context.Context, method, p string, flags map[string]string, payload []byte) (*http.Response, []byte, error) {
	u, err := h.resolve(p, flags)
	if err != nil {
		return nil, nil, err
	}
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, nil, errs.New(errs.Usage, "cannot build request")
	}
	req.Header.Set("User-Agent", "tmvp")
	if payload != nil {
		ct := "text/plain"
		if json.Valid(payload) {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}
	if err := ApplyAuth(ctx, h.t, req); err != nil {
		return nil, nil, err
	}
	for k, v := range flags {
		if name, ok := strings.CutPrefix(k, "h."); ok {
			req.Header.Set(name, v)
		}
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, nil, NetErr(ctx, err)
	}
	defer resp.Body.Close()
	body, err := ReadBounded(resp.Body, h.t.Limits.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, nil, errs.AuthError(fmt.Errorf("HTTP %d", resp.StatusCode))
	}
	return resp, body, nil
}

// ApplyAuth sets headers, bearer token or basic auth from the target's fields.
func ApplyAuth(ctx context.Context, t *connector.Target, req *http.Request) error {
	for _, base := range []string{"headers", string(t.Mode) + ".headers"} {
		raw, ok := t.Raw(base)
		m, isMap := raw.(map[string]any)
		if !ok || !isMap {
			continue
		}
		for k := range m {
			v, err := t.String(ctx, base+"."+k)
			if err != nil {
				return err
			}
			req.Header.Set(k, v)
		}
	}
	if t.Has(string(t.Mode)+".token") || t.Has("token") {
		tok, err := t.ModeString(ctx, "token")
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	} else if t.Has(string(t.Mode)+".user") || t.Has("user") {
		user, err := t.ModeString(ctx, "user")
		if err != nil {
			return err
		}
		pw := ""
		if t.Has(string(t.Mode)+".password") || t.Has("password") {
			if pw, err = t.ModeString(ctx, "password"); err != nil {
				return err
			}
		}
		if t.Redactor != nil {
			t.Redactor.AddBasicAuth(user, pw)
		}
		req.SetBasicAuth(user, pw)
	}
	return nil
}

// ReadBounded reads at most max bytes; more → guard_rejected.
func ReadBounded(r io.Reader, max int) ([]byte, error) {
	if max <= 0 {
		max = 1 << 20
	}
	b, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, errs.Wrap(errs.Unreachable, err, "reading response")
	}
	if len(b) > max {
		return nil, errs.New(errs.GuardRejected, "response body exceeded %d bytes", max).WithHint("narrow the request (paging, filters)")
	}
	return b, nil
}

// NetErr maps a transport error without echoing the URL (it may carry query secrets).
func NetErr(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return errs.New(errs.Timeout, "request timed out")
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
		if ue.Timeout() {
			return errs.New(errs.Timeout, "request timed out")
		}
	}
	return errs.New(errs.Unreachable, "request failed: %v", err)
}

func statusErr(code int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	c := errs.Runtime
	if code == http.StatusNotFound {
		c = errs.NotFound
	}
	return errs.New(c, "HTTP %d: %s", code, msg)
}

// EmitBody emits JSON bodies as rows (array of objects / object) and
// everything else as text lines.
func EmitBody(e connector.Emit, ctype string, body []byte) error {
	t := bytes.TrimSpace(body)
	if len(t) > 0 && (strings.Contains(ctype, "json") || t[0] == '{' || t[0] == '[') {
		var v any
		if json.Unmarshal(t, &v) == nil {
			switch x := v.(type) {
			case map[string]any:
				return ignoreLimit(e.Row(x))
			case []any:
				for _, it := range x {
					var err error
					if m, ok := it.(map[string]any); ok {
						err = e.Row(m)
					} else {
						err = e.Row(connector.Row{"value": it})
					}
					if err != nil {
						return ignoreLimit(err)
					}
				}
				return nil
			}
		}
	}
	for _, l := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if err := e.Text(l); err != nil {
			return ignoreLimit(err)
		}
	}
	return nil
}

func ignoreLimit(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}
