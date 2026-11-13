package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/muthuishere/tmvp/internal/audit"
	"github.com/muthuishere/tmvp/internal/certinfo"
	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/secret"
)

// Secret & certificate health (DESIGN §8.2). Only names, states, dates and
// public certificate metadata leave this file — never a value or key material.

// secretEvent writes one "secret" audit record (no value).
func secretEvent(aud *audit.Logger, ev secret.Event) {
	if aud == nil {
		return
	}
	a := map[string]any{"op": "secret", "secret": ev.Name, "key": ev.Key, "event": ev.Kind,
		"trigger": ev.Trigger, "updated": ev.Updated, "outcome": "ok"}
	if !ev.ExpiresAt.IsZero() {
		a["expires_at"] = ev.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if ev.Kind == "failure" {
		a["outcome"], a["code"], a["error"] = "failed", ev.Code, ev.Error
	}
	_ = aud.Event("secret", a)
}

// SecretReport is one secret slot's health.
type SecretReport struct {
	Name       string         `json:"name"`  // "pw" (top-level) or "prod.db_ro"
	Scope      string         `json:"scope"` // "" or env
	State      string         `json:"state"` // ok | missing | invalid | login_required
	Source     string         `json:"source"`
	File       bool           `json:"file,omitempty"`
	FetchedAt  time.Time      `json:"fetched_at,omitzero"`
	Updated    string         `json:"updated"` // yes | no | unknown
	ExpiresAt  time.Time      `json:"expires_at,omitzero"`
	Error      *errs.Error    `json:"error,omitempty"`
	Kind       string         `json:"kind,omitempty"` // pem | kubeconfig | json
	Cert       *certinfo.Cert `json:"cert,omitempty"` // earliest-expiring cert held
	CertCount  int            `json:"cert_count,omitempty"`
	CertStatus string         `json:"cert_status,omitempty"` // ok | expiring | expired
	PrivateKey bool           `json:"private_key,omitempty"`
}

// Problem reports whether the slot is not usable or its cert expired.
func (r SecretReport) Problem() bool { return r.State != "ok" || r.CertStatus == "expired" }

// Row is the flat output row.
func (r SecretReport) Row() map[string]any {
	row := map[string]any{"secret": r.Name, "state": r.State, "source": r.Source, "updated": r.Updated}
	if !r.FetchedAt.IsZero() {
		row["fetched_at"] = r.FetchedAt.UTC().Format(time.RFC3339)
	}
	if !r.ExpiresAt.IsZero() {
		row["expires_at"] = r.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if r.Error != nil {
		row["code"], row["reason"] = string(r.Error.Code), r.Error.Message
	}
	if r.Kind != "" {
		row["kind"] = r.Kind
	}
	if r.Cert != nil {
		row["cert_not_after"] = r.Cert.NotAfter.Format(time.RFC3339)
		row["cert_days_left"] = r.Cert.DaysLeft
		row["cert_subject"] = r.Cert.Subject
		row["cert_count"] = r.CertCount
		row["cert_status"] = r.CertStatus
	}
	if r.Kind != "" || r.PrivateKey {
		row["private_key"] = r.PrivateKey
	}
	return row
}

// secretsStatus is the server op behind `tmvp secrets status`.
func (e *Engine) secretsStatus(ctx context.Context, p *product, req protocol.Request, sink protocol.Sink) int {
	warn := 30
	if v := req.Flags["days"]; v != "" {
		fmt.Sscan(v, &warn)
	}
	reps, err := e.secretReport(ctx, p, p.config(), req.Env, warn, 15*time.Second)
	if err != nil {
		ee := errs.As(err)
		sink(protocol.Frame{Type: protocol.FrameError, Error: ee})
		return errs.ExitCode(ee.Code)
	}
	exit := 0
	for _, r := range reps {
		sink(protocol.Frame{Type: protocol.FrameRow, Row: r.Row()})
		switch {
		case r.State != "ok":
			exit = errs.ExitCode(errs.SecretMissing)
		case r.CertStatus == "expired" && exit == 0:
			exit = 1
		}
	}
	return exit
}

// SecretsReport runs the secret health check in-process (doctor).
func (e *Engine) SecretsReport(ctx context.Context, configPath, envSel string, warnDays int, timeout time.Duration) ([]SecretReport, error) {
	p, err := e.product(configPath)
	if err != nil {
		return nil, err
	}
	return e.secretReport(secret.WithTrigger(ctx, "doctor"), p, p.config(), envSel, warnDays, timeout)
}

func (e *Engine) secretReport(ctx context.Context, p *product, cfg *config.Config, envSel string, warnDays int, timeout time.Duration) ([]SecretReport, error) {
	envs, err := cfg.MatchEnvs(envSel)
	if err != nil {
		return nil, err
	}
	inScope := map[string]bool{"": true}
	for _, en := range envs {
		inScope[en] = true
	}
	var defs []*config.SecretDef
	for _, d := range cfg.AllSecrets() {
		if inScope[d.Scope] {
			defs = append(defs, d)
		}
	}
	out := make([]SecretReport, len(defs))
	var wg sync.WaitGroup
	for i, d := range defs {
		wg.Add(1)
		go func(i int, d *config.SecretDef) {
			defer wg.Done()
			out[i] = e.secretOne(ctx, p, cfg, d, envs, warnDays, timeout)
		}(i, d)
	}
	wg.Wait()
	return out, nil
}

func (e *Engine) secretOne(ctx context.Context, p *product, cfg *config.Config, d *config.SecretDef, envs []string, warnDays int, timeout time.Duration) SecretReport {
	r := SecretReport{Name: d.Name, Scope: d.Scope, File: d.File, Source: "env"}
	if d.Scope != "" {
		r.Name = d.Scope + "." + d.Name
	}
	if d.Command != "" {
		r.Source = "?"
		if txt, err := cfg.CommandText(d); err == nil {
			r.Source = FirstWord(txt)
		}
	}
	sd := toSecretDef(cfg, d)
	sctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		sctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	_, gerr := p.store.GetBytes(sctx, sd)
	in := p.store.Info(sd.Key)
	r.FetchedAt, r.Updated, r.ExpiresAt = in.FetchedAt, in.Updated, in.ExpiresAt
	if gerr != nil {
		ee := errs.As(gerr)
		ee.Message = e.Red.Redact(ee.Message)
		r.Error, r.State = ee, "missing"
		if ee.Code == errs.SecretInvalid {
			r.State = "invalid"
		}
		if e.needsLogin(sctx, p, cfg, d, envs) {
			r.State = "login_required"
		}
		return r
	}
	r.State = "ok"
	now := time.Now()
	p.store.Inspect(sd.Key, func(val []byte) {
		ci := certinfo.Inspect(val, now)
		r.Kind, r.PrivateKey, r.CertCount = ci.Kind, ci.PrivateKey, len(ci.Certs)
		if c, ok := ci.Earliest(); ok {
			r.Cert = &c
			r.CertStatus = certinfo.Status(c, warnDays)
		}
		if ci.Kind != "" && len(ci.Certs) == 0 && !ci.PrivateKey {
			r.Kind = "" // a JSON value with no cert/key: not a cert-ish secret
		}
	})
	return r
}

// needsLogin: a system using d has a login check that fails now.
func (e *Engine) needsLogin(ctx context.Context, p *product, cfg *config.Config, d *config.SecretDef, envs []string) bool {
	for _, en := range envs {
		if d.Scope != "" && d.Scope != en {
			continue
		}
		for _, sn := range cfg.Envs[en].SystemOrder {
			s := cfg.Envs[en].Systems[sn]
			if s.Login == nil || s.Login.Check == "" {
				continue
			}
			for _, n := range config.ReferencedSecrets(s) {
				if cfg.LookupSecret(en, n) == d {
					if e.Login.Ensure(ctx, p.name+"/"+en+"/"+sn, loginSpec(s), cfg.Dir, en, sn) != nil {
						return true
					}
				}
			}
		}
	}
	return false
}

// FirstWord is the program a command runs (base name, no args, no VAR= prefix).
func FirstWord(cmd string) string {
	for _, f := range strings.Fields(cmd) {
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			continue
		}
		return filepath.Base(strings.Trim(f, `"'`))
	}
	return ""
}

// CertReport is one certificate check: a TLS endpoint or a secret-held cert.
type CertReport struct {
	Env      string         `json:"env,omitempty"`
	System   string         `json:"system,omitempty"`
	Type     string         `json:"type,omitempty"`
	Source   string         `json:"source"`             // tls | secret
	Endpoint string         `json:"endpoint,omitempty"` // host:port (never credentials)
	Secret   string         `json:"secret,omitempty"`
	Cert     *certinfo.Cert `json:"cert,omitempty"` // leaf (tls) / earliest (secret)
	ChainMin int            `json:"chain_min_days"`
	Trusted  string         `json:"trusted,omitempty"` // yes | no: reason
	Status   string         `json:"status"`            // ok | expiring | expired | error | no_tls
	Error    *errs.Error    `json:"error,omitempty"`
}

// Row is the flat output row.
func (c CertReport) Row() map[string]any {
	r := map[string]any{"source": c.Source, "status": c.Status}
	for k, v := range map[string]string{"env": c.Env, "system": c.System, "type": c.Type, "endpoint": c.Endpoint, "secret": c.Secret, "trusted": c.Trusted} {
		if v != "" {
			r[k] = v
		}
	}
	if c.Cert != nil {
		r["not_after"] = c.Cert.NotAfter.Format(time.RFC3339)
		r["days_left"] = c.Cert.DaysLeft
		r["subject"] = c.Cert.Subject
		r["issuer"] = c.Cert.Issuer
		if len(c.Cert.SANs) > 0 {
			r["sans"] = strings.Join(c.Cert.SANs, ",")
		}
	}
	if c.Error != nil {
		r["code"], r["reason"] = string(c.Error.Code), c.Error.Message
	}
	return r
}

// CertsOptions bounds `tmvp certs`.
type CertsOptions struct {
	Env      string
	WarnDays int
	Timeout  time.Duration // per endpoint (default 5s)
	Secrets  bool          // include secret-held certs
}

// Certs checks every TLS endpoint of the selected systems (via-aware,
// read-only: a TLS handshake, nothing else) plus certs held in secrets.
func (e *Engine) Certs(ctx context.Context, configPath string, o CertsOptions) ([]CertReport, error) {
	p, err := e.product(configPath)
	if err != nil {
		return nil, err
	}
	cfg := p.config()
	ctx = secret.WithTrigger(ctx, "certs")
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Second
	}
	if o.WarnDays <= 0 {
		o.WarnDays = 30
	}
	envs, err := cfg.MatchEnvs(o.Env)
	if err != nil {
		return nil, err
	}
	type job struct{ env, sys string }
	var jobs []job
	for _, en := range envs {
		for _, sn := range cfg.Envs[en].SystemOrder {
			jobs = append(jobs, job{en, sn})
		}
	}
	res := make([]*CertReport, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			tctx, cancel := context.WithTimeout(ctx, o.Timeout)
			defer cancel()
			res[i] = e.certOne(tctx, p, cfg, j.env, j.sys, o.WarnDays)
		}(i, j)
	}
	wg.Wait()
	var out []CertReport
	for _, r := range res {
		if r != nil {
			out = append(out, *r)
		}
	}
	if o.Secrets {
		reps, err := e.secretReport(ctx, p, cfg, o.Env, o.WarnDays, o.Timeout)
		if err == nil {
			for _, s := range reps {
				if s.Cert != nil {
					out = append(out, CertReport{Source: "secret", Secret: s.Name, Env: s.Scope, Cert: s.Cert, Status: s.CertStatus})
				}
			}
		}
	}
	return out, nil
}

// endpoint is a TLS endpoint found in a system's fields.
type endpoint struct {
	addr, serverName string
	postgres         bool // needs the SSLRequest preamble
	optional         bool // TLS is optional (pg sslmode prefer/allow/unset): refusal = "no_tls", not an error
}

// errNoTLS: the server declined TLS on an endpoint where TLS is optional.
var errNoTLS = errors.New("server does not offer TLS")

func (e *Engine) certOne(ctx context.Context, p *product, cfg *config.Config, env, sys string, warn int) *CertReport {
	s := cfg.System(env, sys)
	if s == nil || s.Type == "portforward" || s.Type == "ssh" {
		return nil
	}
	if _, ok := connector.Lookup(s.Type); !ok && len(s.Via) > 0 {
		return nil
	}
	rep := &CertReport{Env: env, System: sys, Type: s.Type, Source: "tls"}
	errOut := func(err error) *CertReport {
		ee := errs.As(err)
		ee.Message = e.Red.Redact(ee.Message)
		rep.Status, rep.Error = "error", ee
		return rep
	}
	// field discovery needs no tunnel: build a direct target first
	ft := connector.NewTarget(e.baseTarget(p, cfg, s, connector.ModeRead, connector.DirectDialer()),
		&fields{cfg: cfg, env: env, sys: s, store: p.store, used: &usedSecrets{m: map[string]secret.Def{}}})
	ep, ok, err := findEndpoint(ctx, ft, s)
	if err != nil {
		return errOut(err)
	}
	if !ok {
		return nil
	}
	rep.Endpoint = ep.addr
	t := ft
	if len(s.Via) > 0 {
		vt, _, err := e.target(ctx, p, cfg, s, connector.ModeRead)
		if err != nil {
			return errOut(err)
		}
		t = vt
	}
	certs, err := handshake(ctx, t, ep)
	if errors.Is(err, errNoTLS) {
		rep.Status = "no_tls"
		return rep
	}
	if err != nil {
		return errOut(err)
	}
	now := time.Now()
	leaf := certinfo.FromX509(certs[0], now)
	rep.Cert = &leaf
	rep.ChainMin = leaf.DaysLeft
	expired := leaf.Expired
	pool := x509.NewCertPool()
	for _, c := range certs[1:] {
		pool.AddCert(c)
		if d := certinfo.DaysLeft(c.NotAfter, now); d < rep.ChainMin {
			rep.ChainMin = d
		}
		expired = expired || now.After(c.NotAfter)
	}
	if _, verr := certs[0].Verify(x509.VerifyOptions{DNSName: ep.serverName, Intermediates: pool}); verr != nil {
		rep.Trusted = "no: " + verr.Error()
	} else {
		rep.Trusted = "yes"
	}
	rep.Status = certinfo.Status(certinfo.Cert{DaysLeft: rep.ChainMin, Expired: expired}, warn)
	return rep
}

func handshake(ctx context.Context, t *connector.Target, ep endpoint) ([]*x509.Certificate, error) {
	raw, err := t.Dial(ctx, "tcp", ep.addr)
	if err != nil {
		return nil, errs.Wrap(errs.Unreachable, err, "dial %s", ep.addr)
	}
	defer raw.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}
	if ep.postgres {
		// SSLRequest: length 8, code 80877103
		if _, err := raw.Write([]byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}); err != nil {
			return nil, errs.Wrap(errs.Unreachable, err, "postgres ssl request")
		}
		b := make([]byte, 1)
		if _, err := raw.Read(b); err != nil {
			return nil, errs.Wrap(errs.Unreachable, err, "postgres ssl request")
		}
		if b[0] != 'S' {
			if ep.optional {
				return nil, errNoTLS
			}
			return nil, errs.New(errs.Unsupported, "server at %s does not offer TLS", ep.addr)
		}
	}
	// InsecureSkipVerify: we read the chain to REPORT on it (expired or
	// untrusted certs must still be shown); trust is verified separately.
	tc := tls.Client(raw, &tls.Config{ServerName: ep.serverName, InsecureSkipVerify: true}) //nolint:gosec
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, errs.Wrap(errs.Unreachable, err, "tls handshake with %s", ep.addr)
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errs.New(errs.Runtime, "%s presented no certificate", ep.addr)
	}
	return certs, nil
}

var tlsSchemes = map[string]string{"https": "443", "wss": "443", "rediss": "6379", "tls": "4222", "ssl": "9093",
	"kafka+ssl": "9093", "ldaps": "636", "grpcs": "443"}

var defaultPorts = map[string]string{"postgres": "5432", "redis": "6379", "nats": "4222", "kafka": "9093",
	"mongo": "27017", "mysql": "3306", "elasticsearch": "9200", "loki": "3100", "grafana": "3000"}

// findEndpoint locates the system's TLS endpoint from its fields; generic for
// types it does not know (url / base_url / host / brokers).
func findEndpoint(ctx context.Context, t *connector.Target, s *config.System) (endpoint, bool, error) {
	str := func(path string) (string, error) {
		if !t.Has(path) {
			return "", nil
		}
		v, _ := t.Raw(path)
		switch v.(type) {
		case string, int, float64:
		default:
			return "", nil
		}
		if sv, ok := v.(string); ok {
			if _, isFile := config.FileRef(sv); isFile {
				return "", nil
			}
		}
		return t.String(ctx, path)
	}
	truthy := func(path string) bool {
		v, ok := t.Raw(path)
		if !ok {
			return false
		}
		switch x := v.(type) {
		case bool:
			return x
		case string:
			x = strings.ToLower(x)
			return x == "true" || x == "ssl" || x == "sasl_ssl" || x == "require" || x == "yes"
		case map[string]any:
			return true // tls: {ca: ..}
		}
		return false
	}
	tlsOn := truthy("tls") || truthy("ssl") || truthy("security_protocol") || truthy("read.tls")
	if s.Type == "k8s" && t.Has("kubeconfig") {
		b, err := t.File(ctx, "kubeconfig")
		if err != nil {
			return endpoint{}, false, err
		}
		ctxName, _ := str("context")
		if srv := kubeServer(b, ctxName); srv != "" {
			return fromURL(srv, true)
		}
		return endpoint{}, false, nil
	}
	if s.Type == "postgres" {
		for _, f := range []string{"read.dsn", "dsn", "read.url", "url"} {
			v, err := str(f)
			if err != nil {
				return endpoint{}, false, err
			}
			if v != "" {
				return pgEndpoint(v)
			}
		}
	}
	for _, f := range []string{"url", "base_url", "endpoint", "read.url", "uri", "read.uri", "address", "addr", "server"} {
		v, err := str(f)
		if err != nil {
			return endpoint{}, false, err
		}
		if v == "" {
			continue
		}
		if strings.Contains(v, "://") {
			ep, ok, err := fromURL(v, tlsOn)
			if ok || err != nil {
				return ep, ok, err
			}
			continue
		}
		if tlsOn {
			return hostPort(v, defaultPorts[s.Type])
		}
	}
	if raw, ok := t.Raw("brokers"); ok {
		var first string
		switch b := raw.(type) {
		case []any:
			if len(b) > 0 {
				first = fmt.Sprint(b[0])
			}
		case string:
			first = strings.TrimSpace(strings.Split(b, ",")[0])
		}
		if first != "" {
			if strings.Contains(first, "://") {
				return fromURL(first, tlsOn)
			}
			if tlsOn {
				return hostPort(first, "9093")
			}
		}
	}
	if tlsOn {
		h, err := str("host")
		if err != nil || h == "" {
			return endpoint{}, false, err
		}
		port, _ := str("port")
		if port != "" && !strings.Contains(h, ":") {
			h = net.JoinHostPort(h, port)
		}
		return hostPort(h, defaultPorts[s.Type])
	}
	return endpoint{}, false, nil
}

func hostPort(h, defPort string) (endpoint, bool, error) {
	host, port, err := net.SplitHostPort(h)
	if err != nil {
		if defPort == "" {
			defPort = "443"
		}
		host, port = h, defPort
	}
	return endpoint{addr: net.JoinHostPort(host, port), serverName: host}, true, nil
}

func fromURL(raw string, tlsOn bool) (endpoint, bool, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return endpoint{}, false, nil // never echo the (maybe credentialed) URL
	}
	sch := strings.ToLower(u.Scheme)
	q := u.Query()
	def, isTLS := tlsSchemes[sch]
	if !isTLS && (tlsOn || q.Get("tls") == "true" || q.Get("ssl") == "true") {
		isTLS, def = true, map[string]string{"http": "80", "redis": "6379", "nats": "4222",
			"mongodb": "27017", "mongodb+srv": "27017"}[sch]
	}
	if !isTLS {
		return endpoint{}, false, nil
	}
	host := strings.Split(u.Host, ",")[0] // mongo seed lists
	return hostPort(host, def)
}

func pgEndpoint(dsn string) (endpoint, bool, error) {
	host, port, mode := "", "5432", "prefer"
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return endpoint{}, false, nil
		}
		host = u.Hostname()
		if u.Port() != "" {
			port = u.Port()
		}
		if m := u.Query().Get("sslmode"); m != "" {
			mode = m
		}
	} else {
		for _, kv := range strings.Fields(dsn) {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "host":
				host = v
			case "port":
				port = v
			case "sslmode":
				mode = v
			}
		}
	}
	// disable never speaks TLS; prefer/allow (and unset = prefer) may, so
	// probe and report "no_tls" if the server declines.
	if host == "" || strings.HasPrefix(host, "/") || mode == "disable" {
		return endpoint{}, false, nil
	}
	optional := mode == "allow" || mode == "prefer"
	return endpoint{addr: net.JoinHostPort(host, port), serverName: host, postgres: true, optional: optional}, true, nil
}

// kubeServer returns the API server URL of ctxName (or current-context).
func kubeServer(b []byte, ctxName string) string {
	var kc struct {
		Current  string `yaml:"current-context"`
		Contexts []struct {
			Name    string `yaml:"name"`
			Context struct {
				Cluster string `yaml:"cluster"`
			} `yaml:"context"`
		} `yaml:"contexts"`
		Clusters []struct {
			Name    string `yaml:"name"`
			Cluster struct {
				Server string `yaml:"server"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if yaml.Unmarshal(b, &kc) != nil {
		return ""
	}
	if ctxName == "" {
		ctxName = kc.Current
	}
	cluster := ""
	for _, c := range kc.Contexts {
		if c.Name == ctxName {
			cluster = c.Context.Cluster
		}
	}
	for _, c := range kc.Clusters {
		if c.Name == cluster || (cluster == "" && len(kc.Clusters) == 1) {
			return c.Cluster.Server
		}
	}
	return ""
}

// sortCerts orders reports: worst first, then env/system.
func SortCerts(rs []CertReport) {
	rank := map[string]int{"expired": 0, "error": 1, "expiring": 2, "ok": 3, "no_tls": 4}
	sort.SliceStable(rs, func(i, j int) bool {
		if rank[rs[i].Status] != rank[rs[j].Status] {
			return rank[rs[i].Status] < rank[rs[j].Status]
		}
		return rs[i].Env+rs[i].System+rs[i].Secret < rs[j].Env+rs[j].System+rs[j].Secret
	})
}
