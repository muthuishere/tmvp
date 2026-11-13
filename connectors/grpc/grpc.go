// Package grpc is the tmvp connector for gRPC services. It needs no generated
// code: method and message types come from server reflection, or from a
// descriptor set (`protoset:`, e.g. `buf build -o api.binpb`) when the
// server has no reflection.
//
// Config fields (mode-aware: read.<key> / write.<key> win):
//
//	address       host:port (required); dialed through via: like every system
//	tls           true = TLS (default false = plaintext)
//	ca            secret/file PEM CA bundle (use ${file:secrets.x})
//	server_name   TLS SNI / authority override
//	insecure      true = skip TLS verification (test servers only)
//	token         secret; sent as metadata "authorization: Bearer <token>"
//	headers       map of metadata (values may be ${secrets.x})
//	protoset      descriptor set file, relative to the skill dir
//	read_methods  globs of methods that are safe to call as reads
//	              (default: Get*, List*, Search*, Find*, Count*, Describe*,
//	              Check, Watch, Health*). Matched against "pkg.Service/Method"
//	              and against the bare method name.
//
// A method that is not a read, or that streams from the client, runs only
// through `tmvp write … --reason` (--preview shows the request, sends nothing).
//
// Saved calls: <assets>/<system>/calls/<name>.json — front-matter
// {description, method, params}, body = the JSON request with {param}
// placeholders filled from k=v args.
package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"gopkg.in/yaml.v3"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() { connector.Register("grpc", New) }

// New returns an unopened gRPC connector.
func New() connector.Connector { return &GRPC{} }

// DefaultReadMethods are the method-name globs treated as reads.
var DefaultReadMethods = []string{"Get*", "List*", "Search*", "Find*", "Count*", "Describe*", "Check", "Watch", "Health*"}

// GRPC is one client connection for one env/system/mode.
type GRPC struct {
	t     *connector.Target
	cc    *grpc.ClientConn
	md    metadata.MD
	reads []string

	mu      sync.Mutex
	files   *protoregistry.Files // nil until loaded
	fromSet bool                 // descriptors came from protoset (no reflection needed)
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — saved calls (@name), then services and methods with kind (read/write) and streaming"},
	{Name: "describe", Mode: connector.Read, Help: "describe <service|pkg.Service/Method|message> — methods, or request/response fields"},
	{Name: "health", Mode: connector.Read, Help: "health [service] — grpc.health.v1 Check (status, latency)"},
	{Name: "call", Mode: connector.ReadWrite, Default: true, Saved: true,
		Help: "call <pkg.Service/Method> ['<json>'] | call @saved [k=v ...] — read methods only as reads; others via tmvp write (--preview)"},
}

func (g *GRPC) Type() string            { return "grpc" }
func (g *GRPC) Verbs() []connector.Verb { return verbs }
func (g *GRPC) Close() error {
	if g.cc != nil {
		return g.cc.Close()
	}
	return nil
}

// Guard refuses client/bidi-streaming-shaped misuse early: nothing else is
// knowable before the descriptors are loaded.
func (g *GRPC) Guard(c connector.Call) error {
	if c.Verb == "call" && len(c.Args) == 0 {
		return errs.New(errs.Usage, "call needs a method: call pkg.Service/Method ['<json>'] or call @saved")
	}
	return nil
}

// Open dials (lazily) and verifies reachability with a reflection or health probe.
func (g *GRPC) Open(ctx context.Context, t *connector.Target) error {
	g.t = t
	addr, err := t.ModeString(ctx, "address")
	if err != nil || addr == "" {
		return errs.New(errs.Config, "grpc: address (host:port) is required")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return errs.New(errs.Config, "grpc: address must be host:port")
	}
	var creds credentials.TransportCredentials = insecure.NewCredentials()
	if v, _ := t.StringOr(ctx, "tls", ""); v == "true" {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if sn, _ := t.StringOr(ctx, "server_name", ""); sn != "" {
			cfg.ServerName = sn
		}
		if v, _ := t.StringOr(ctx, "insecure", ""); v == "true" {
			cfg.InsecureSkipVerify = true
		}
		if t.Has("ca") {
			pem, err := t.File(ctx, "ca")
			if err != nil {
				return err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return errs.New(errs.Config, "grpc: ca holds no PEM certificate")
			}
			cfg.RootCAs = pool
		}
		creds = credentials.NewTLS(cfg)
	}
	g.md = metadata.MD{"user-agent": {"tmvp"}}
	if t.Has(string(t.Mode)+".token") || t.Has("token") {
		tok, err := t.ModeString(ctx, "token")
		if err != nil {
			return err
		}
		g.md.Set("authorization", "Bearer "+tok)
	}
	if raw, ok := t.Raw("headers"); ok {
		if m, ok := raw.(map[string]any); ok {
			for k := range m {
				v, err := t.String(ctx, "headers."+k)
				if err != nil {
					return err
				}
				g.md.Set(strings.ToLower(k), v)
			}
		}
	}
	g.reads = DefaultReadMethods
	if raw, ok := t.Raw("read_methods"); ok {
		g.reads = nil
		switch v := raw.(type) {
		case []any:
			for _, x := range v {
				g.reads = append(g.reads, fmt.Sprint(x))
			}
		case string:
			g.reads = strings.Split(v, ",")
		}
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithContextDialer(func(ctx context.Context, a string) (net.Conn, error) { return t.Dial(ctx, "tcp", a) }),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16 << 20)),
	}
	if sn, _ := t.StringOr(ctx, "server_name", ""); sn != "" {
		opts = append(opts, grpc.WithAuthority(sn))
	}
	cc, err := grpc.NewClient("passthrough:///"+addr, opts...)
	if err != nil {
		return errs.Wrap(errs.Config, err, "grpc: dial options")
	}
	g.cc = cc
	if p, _ := t.StringOr(ctx, "protoset", ""); p != "" {
		if err := g.loadProtoset(p); err != nil {
			return err
		}
	}
	return g.Check(ctx)
}

// Check: with a protoset, the health service (Unimplemented counts as
// reachable); otherwise listing services through reflection.
func (g *GRPC) Check(ctx context.Context) error {
	ctx, cancel := g.callCtx(ctx)
	defer cancel()
	if g.fromSet {
		_, err := healthpb.NewHealthClient(g.cc).Check(ctx, &healthpb.HealthCheckRequest{})
		if err != nil && status.Code(err) != codes.Unimplemented && status.Code(err) != codes.NotFound {
			return classify(err)
		}
		return nil
	}
	_, err := g.listServices(ctx)
	return err
}

func (g *GRPC) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx = metadata.NewOutgoingContext(ctx, g.md)
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	to := g.t.Limits.StatementTimeout
	if to <= 0 {
		to = 10 * time.Second
	}
	return context.WithTimeout(ctx, to)
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	if e := errs.As(err); e != nil {
		return e
	}
	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied:
		return errs.AuthError(err)
	case codes.Unavailable:
		return errs.Wrap(errs.Unreachable, err, "grpc unavailable")
	case codes.DeadlineExceeded:
		return errs.Wrap(errs.Timeout, err, "grpc deadline exceeded")
	case codes.NotFound:
		return errs.Wrap(errs.NotFound, err, "grpc")
	case codes.Unimplemented:
		return errs.Wrap(errs.Unsupported, err, "grpc method not implemented by the server")
	case codes.InvalidArgument:
		return errs.Wrap(errs.Usage, err, "grpc")
	}
	return errs.Wrap(errs.Runtime, err, "grpc")
}

// ---- descriptors -------------------------------------------------------

func (g *GRPC) loadProtoset(p string) error {
	if !filepath.IsAbs(p) {
		p = filepath.Join(g.t.Dir, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return errs.Wrap(errs.Config, err, "grpc: protoset")
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(b, &set); err != nil {
		return errs.Wrap(errs.Config, err, "grpc: protoset is not a FileDescriptorSet (build with: buf build -o x.binpb, or protoc --include_imports --descriptor_set_out)")
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return errs.Wrap(errs.Config, err, "grpc: protoset")
	}
	g.files, g.fromSet = files, true
	return nil
}

func (g *GRPC) reflect(ctx context.Context, req *rpb.ServerReflectionRequest) (*rpb.ServerReflectionResponse, error) {
	st, err := rpb.NewServerReflectionClient(g.cc).ServerReflectionInfo(ctx)
	if err != nil {
		return nil, err
	}
	defer st.CloseSend()
	if err := st.Send(req); err != nil {
		return nil, err
	}
	resp, err := st.Recv()
	if err != nil {
		return nil, err
	}
	if e := resp.GetErrorResponse(); e != nil {
		return nil, status.Error(codes.Code(e.ErrorCode), e.ErrorMessage)
	}
	return resp, nil
}

func (g *GRPC) listServices(ctx context.Context) ([]string, error) {
	if g.fromSet {
		var out []string
		g.files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
			for i := 0; i < fd.Services().Len(); i++ {
				out = append(out, string(fd.Services().Get(i).FullName()))
			}
			return true
		})
		sort.Strings(out)
		return out, nil
	}
	resp, err := g.reflect(ctx, &rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_ListServices{ListServices: ""}})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return nil, errs.New(errs.Unsupported, "grpc: the server has no reflection").WithHint("set protoset: <file> (buf build -o api.binpb)")
		}
		return nil, classify(err)
	}
	var out []string
	for _, s := range resp.GetListServicesResponse().GetService() {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out, nil
}

// resolve returns the descriptor for a fully-qualified symbol, fetching the
// defining files (and their imports) through reflection on first use.
func (g *GRPC) resolve(ctx context.Context, sym string) (protoreflect.Descriptor, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.files == nil {
		g.files = new(protoregistry.Files)
	}
	if d, err := g.files.FindDescriptorByName(protoreflect.FullName(sym)); err == nil {
		return d, nil
	}
	if g.fromSet {
		return nil, errs.New(errs.NotFound, "grpc: %q is not in the protoset", sym)
	}
	resp, err := g.reflect(ctx, &rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: sym}})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, errs.New(errs.NotFound, "grpc: the server does not know %q", sym).WithHint("ls")
		}
		return nil, classify(err)
	}
	pending := map[string]*descriptorpb.FileDescriptorProto{}
	if err := g.collect(ctx, resp.GetFileDescriptorResponse().GetFileDescriptorProto(), pending); err != nil {
		return nil, err
	}
	for len(pending) > 0 {
		progress := false
		for name, fdp := range pending {
			ready := true
			for _, dep := range fdp.GetDependency() {
				if _, err := g.files.FindFileByPath(dep); err != nil {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			fd, err := protodesc.NewFile(fdp, g.files)
			if err != nil {
				return nil, errs.Wrap(errs.Runtime, err, "grpc: descriptor %s", name)
			}
			if err := g.files.RegisterFile(fd); err != nil {
				return nil, errs.Wrap(errs.Runtime, err, "grpc: descriptor %s", name)
			}
			delete(pending, name)
			progress = true
		}
		if !progress {
			return nil, errs.New(errs.Runtime, "grpc: unresolvable descriptor imports")
		}
	}
	return g.files.FindDescriptorByName(protoreflect.FullName(sym))
}

func (g *GRPC) collect(ctx context.Context, raw [][]byte, pending map[string]*descriptorpb.FileDescriptorProto) error {
	for _, b := range raw {
		fdp := new(descriptorpb.FileDescriptorProto)
		if err := proto.Unmarshal(b, fdp); err != nil {
			return errs.Wrap(errs.Runtime, err, "grpc: bad descriptor from server")
		}
		if _, err := g.files.FindFileByPath(fdp.GetName()); err == nil || pending[fdp.GetName()] != nil {
			continue
		}
		pending[fdp.GetName()] = fdp
		for _, dep := range fdp.GetDependency() {
			if _, err := g.files.FindFileByPath(dep); err == nil || pending[dep] != nil {
				continue
			}
			if gd, err := protoregistry.GlobalFiles.FindFileByPath(dep); err == nil { // well-known types
				_ = g.files.RegisterFile(gd)
				continue
			}
			resp, err := g.reflect(ctx, &rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_FileByFilename{FileByFilename: dep}})
			if err != nil {
				return classify(err)
			}
			if err := g.collect(ctx, resp.GetFileDescriptorResponse().GetFileDescriptorProto(), pending); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *GRPC) method(ctx context.Context, name string) (protoreflect.MethodDescriptor, error) {
	name = strings.TrimPrefix(name, "/")
	i := strings.LastIndexAny(name, "/.")
	if i <= 0 {
		return nil, errs.New(errs.Usage, "method must be pkg.Service/Method, got %q", name)
	}
	svc, m := name[:i], name[i+1:]
	d, err := g.resolve(ctx, svc)
	if err != nil {
		return nil, err
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, errs.New(errs.Usage, "%q is not a service", svc)
	}
	md := sd.Methods().ByName(protoreflect.Name(m))
	if md == nil {
		return nil, errs.New(errs.NotFound, "%s has no method %s", svc, m).WithHint("describe " + svc)
	}
	return md, nil
}

// IsRead reports whether the method may run as a plain read.
func IsRead(globs []string, md protoreflect.MethodDescriptor) bool {
	if md.IsStreamingClient() {
		return false
	}
	full := string(md.Parent().FullName()) + "/" + string(md.Name())
	for _, gl := range globs {
		gl = strings.TrimSpace(gl)
		if gl == "" {
			continue
		}
		if ok, _ := path.Match(gl, full); ok {
			return true
		}
		if !strings.Contains(gl, "/") {
			if ok, _ := path.Match(gl, string(md.Name())); ok {
				return true
			}
		}
	}
	return false
}

func kind(globs []string, md protoreflect.MethodDescriptor) string {
	k := "write"
	if IsRead(globs, md) {
		k = "read"
	}
	return k
}

func streaming(md protoreflect.MethodDescriptor) string {
	switch {
	case md.IsStreamingClient() && md.IsStreamingServer():
		return "bidi"
	case md.IsStreamingClient():
		return "client"
	case md.IsStreamingServer():
		return "server"
	}
	return "unary"
}

// ---- verbs -------------------------------------------------------------

func (g *GRPC) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	switch c.Verb {
	case "ls":
		return g.ls(ctx, e)
	case "describe":
		return g.describe(ctx, c.Args, e)
	case "health":
		return g.health(ctx, c.Args, e)
	case "call":
		return g.call(ctx, c, e)
	}
	return errs.New(errs.Usage, "grpc has no verb %q", c.Verb)
}

func stop(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}

func (g *GRPC) ls(ctx context.Context, e connector.Emit) error {
	if err := g.t.EmitSaved(e, "calls", ".json"); err != nil {
		return stop(err)
	}
	cctx, cancel := g.callCtx(ctx)
	defer cancel()
	svcs, err := g.listServices(cctx)
	if err != nil {
		return err
	}
	for _, s := range svcs {
		if strings.HasPrefix(s, "grpc.reflection.") {
			continue
		}
		d, err := g.resolve(cctx, s)
		if err != nil {
			if err := e.Row(connector.Row{"service": s, "error": err.Error()}); err != nil {
				return stop(err)
			}
			continue
		}
		sd := d.(protoreflect.ServiceDescriptor)
		for i := 0; i < sd.Methods().Len(); i++ {
			md := sd.Methods().Get(i)
			if err := e.Row(connector.Row{"service": s, "method": string(md.Name()), "kind": kind(g.reads, md),
				"streaming": streaming(md), "request": string(md.Input().FullName()), "response": string(md.Output().FullName())}); err != nil {
				return stop(err)
			}
		}
	}
	return nil
}

func (g *GRPC) describe(ctx context.Context, args []string, e connector.Emit) error {
	cctx, cancel := g.callCtx(ctx)
	defer cancel()
	if len(args) == 0 {
		return g.ls(ctx, e)
	}
	sym := strings.TrimPrefix(args[0], "/")
	if strings.Contains(sym, "/") {
		md, err := g.method(cctx, sym)
		if err != nil {
			return err
		}
		if err := e.Row(connector.Row{"method": sym, "kind": kind(g.reads, md), "streaming": streaming(md),
			"request": string(md.Input().FullName()), "response": string(md.Output().FullName())}); err != nil {
			return stop(err)
		}
		for _, side := range []struct {
			n string
			m protoreflect.MessageDescriptor
		}{{"request", md.Input()}, {"response", md.Output()}} {
			if err := fields(e, side.n, side.m); err != nil {
				return stop(err)
			}
		}
		return nil
	}
	d, err := g.resolve(cctx, sym)
	if err != nil {
		return err
	}
	switch d := d.(type) {
	case protoreflect.ServiceDescriptor:
		for i := 0; i < d.Methods().Len(); i++ {
			md := d.Methods().Get(i)
			if err := e.Row(connector.Row{"method": string(md.Name()), "kind": kind(g.reads, md), "streaming": streaming(md),
				"request": string(md.Input().FullName()), "response": string(md.Output().FullName())}); err != nil {
				return stop(err)
			}
		}
		return nil
	case protoreflect.MessageDescriptor:
		return stop(fields(e, "", d))
	}
	return errs.New(errs.Usage, "%q is not a service, method or message", sym)
}

func fields(e connector.Emit, side string, m protoreflect.MessageDescriptor) error {
	for i := 0; i < m.Fields().Len(); i++ {
		f := m.Fields().Get(i)
		t := f.Kind().String()
		if f.Message() != nil {
			t = string(f.Message().FullName())
		} else if f.Enum() != nil {
			t = string(f.Enum().FullName())
		}
		card := ""
		if f.IsList() {
			card = "repeated"
		} else if f.IsMap() {
			card = "map"
		}
		r := connector.Row{"message": string(m.FullName()), "field": f.JSONName(), "number": int(f.Number()), "type": t, "cardinality": card}
		if side != "" {
			r["side"] = side
		}
		if err := e.Row(r); err != nil {
			return err
		}
	}
	return nil
}

func (g *GRPC) health(ctx context.Context, args []string, e connector.Emit) error {
	cctx, cancel := g.callCtx(ctx)
	defer cancel()
	svc := ""
	if len(args) > 0 {
		svc = args[0]
	}
	start := time.Now()
	resp, err := healthpb.NewHealthClient(g.cc).Check(cctx, &healthpb.HealthCheckRequest{Service: svc})
	lat := time.Since(start).Milliseconds()
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return stop(e.Row(connector.Row{"service": svc, "status": "no health service", "latency_ms": lat}))
		}
		return classify(err)
	}
	return stop(e.Row(connector.Row{"service": svc, "status": resp.GetStatus().String(), "latency_ms": lat}))
}

type savedCall struct {
	Description string   `yaml:"description"`
	Method      string   `yaml:"method"`
	Params      []string `yaml:"params"`
}

// request turns call args into (method, JSON body).
func (g *GRPC) request(c connector.Call) (string, string, error) {
	a := c.Args
	if strings.HasPrefix(a[0], "@") {
		s, err := g.t.LoadSaved("calls", a[0], ".json")
		if err != nil {
			return "", "", errs.Wrap(errs.NotFound, err, "grpc")
		}
		var sc savedCall
		if err := yaml.Unmarshal([]byte(s.FrontMatter), &sc); err != nil || sc.Method == "" {
			return "", "", errs.New(errs.Config, "%s: front-matter needs method: pkg.Service/Method", s.Path)
		}
		body := strings.TrimSpace(s.Body)
		vals := map[string]string{}
		for i, kv := range a[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				if i < len(sc.Params) {
					k, v = sc.Params[i], kv
				} else {
					return "", "", errs.New(errs.Usage, "argument %q: use k=v", kv)
				}
			}
			vals[k] = v
		}
		for _, p := range sc.Params {
			v, ok := vals[p]
			if !ok {
				return "", "", errs.New(errs.Usage, "@%s needs %s=<value>", s.Name, p)
			}
			q, _ := json.Marshal(v)
			raw := string(q[1 : len(q)-1]) // JSON-escaped, unquoted: works inside "…{p}…" and for bare numbers
			body = strings.ReplaceAll(body, "{"+p+"}", raw)
		}
		if body == "" {
			body = "{}"
		}
		return sc.Method, body, nil
	}
	body := "{}"
	if len(a) > 1 {
		body = strings.Join(a[1:], " ")
	}
	return a[0], body, nil
}

func (g *GRPC) call(ctx context.Context, c connector.Call, e connector.Emit) error {
	if len(c.Args) == 0 {
		return errs.New(errs.Usage, "call needs a method")
	}
	name, body, err := g.request(c)
	if err != nil {
		return err
	}
	cctx, cancel := g.callCtx(ctx)
	defer cancel()
	md, err := g.method(cctx, name)
	if err != nil {
		return err
	}
	if !c.Write && !IsRead(g.reads, md) {
		return errs.New(errs.PolicyBlocked, "%s/%s is not a read method (read_methods: %s)", md.Parent().FullName(), md.Name(), strings.Join(g.reads, ",")).
			WithHint("tmvp write <env> <system> call … --reason \"…\" --preview, or add it to read_methods if it really only reads")
	}
	if md.IsStreamingClient() {
		return errs.New(errs.Unsupported, "client/bidi streaming methods are not supported")
	}
	req := dynamicpb.NewMessage(md.Input())
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal([]byte(body), req); err != nil {
		return errs.Wrap(errs.Usage, err, "request JSON does not match %s", md.Input().FullName())
	}
	full := "/" + string(md.Parent().FullName()) + "/" + string(md.Name())
	if c.Preview {
		j, _ := protojson.Marshal(req)
		return stop(e.Row(connector.Row{"would_call": full, "request": string(j), "kind": kind(g.reads, md)}))
	}
	out := protojson.MarshalOptions{EmitUnpopulated: false, UseProtoNames: false}
	emit := func(m proto.Message) error {
		b, err := out.Marshal(m)
		if err != nil {
			return err
		}
		var row map[string]any
		if err := json.Unmarshal(b, &row); err != nil || row == nil {
			return e.Text(string(b))
		}
		return e.Row(row)
	}
	if !md.IsStreamingServer() {
		resp := dynamicpb.NewMessage(md.Output())
		if err := g.cc.Invoke(cctx, full, req, resp); err != nil {
			return classify(err)
		}
		return stop(emit(resp))
	}
	st, err := g.cc.NewStream(cctx, &grpc.StreamDesc{StreamName: string(md.Name()), ServerStreams: true}, full)
	if err != nil {
		return classify(err)
	}
	if err := st.SendMsg(req); err != nil {
		return classify(err)
	}
	if err := st.CloseSend(); err != nil {
		return classify(err)
	}
	for n := 0; ; n++ {
		if c.Limit > 0 && n >= c.Limit {
			return nil // cancel() ends the stream
		}
		resp := dynamicpb.NewMessage(md.Output())
		if err := st.RecvMsg(resp); err != nil {
			if err == io.EOF {
				return nil
			}
			return classify(err)
		}
		if err := emit(resp); err != nil {
			return stop(err)
		}
	}
}
