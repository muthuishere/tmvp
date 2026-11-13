package grpc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

type svc struct {
	testpb.UnimplementedTestServiceServer
	calls int
	auth  string
}

func (s *svc) UnaryCall(ctx context.Context, r *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	s.calls++
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("authorization"); len(v) > 0 {
		s.auth = v[0]
	}
	return &testpb.SimpleResponse{Username: "u" + string(rune('0'+r.GetResponseSize()%10))}, nil
}

func (s *svc) StreamingOutputCall(r *testpb.StreamingOutputCallRequest, st testpb.TestService_StreamingOutputCallServer) error {
	for i := 0; i < 5; i++ {
		if err := st.Send(&testpb.StreamingOutputCallResponse{Payload: &testpb.Payload{Body: []byte{byte('a' + i)}}}); err != nil {
			return err
		}
	}
	return nil
}

func serve(t *testing.T) (string, *svc) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	impl := &svc{}
	testpb.RegisterTestServiceServer(s, impl)
	healthpb.RegisterHealthServer(s, health.NewServer())
	reflection.Register(s)
	go s.Serve(l)
	t.Cleanup(s.Stop)
	return l.Addr().String(), impl
}

func open(t *testing.T, mode connector.Mode, f connector.StaticFields) *GRPC {
	t.Helper()
	g := New().(*GRPC)
	tg := connector.NewTarget(connector.Target{System: "api", Mode: mode, Dir: t.TempDir(), Dialer: &net.Dialer{}}, f)
	if err := g.Open(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func TestReflectionLsDescribeHealth(t *testing.T) {
	addr, _ := serve(t)
	g := open(t, connector.ModeRead, connector.StaticFields{"address": addr})
	ctx := context.Background()
	var c connector.Collector
	if err := g.Do(ctx, connector.Call{Verb: "ls"}, &c); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range c.Rows {
		if m, ok := r["method"].(string); ok {
			got[r["service"].(string)+"/"+m] = r["kind"].(string) + "," + r["streaming"].(string)
		}
	}
	if got["grpc.testing.TestService/UnaryCall"] != "write,unary" || got["grpc.health.v1.Health/Check"] != "read,unary" ||
		got["grpc.testing.TestService/StreamingOutputCall"] != "write,server" || got["grpc.testing.TestService/FullDuplexCall"] != "write,bidi" {
		t.Fatalf("ls %v", got)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "describe", Args: []string{"grpc.testing.TestService/UnaryCall"}}, &c); err != nil || len(c.Rows) < 3 {
		t.Fatalf("describe %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "health"}, &c); err != nil || c.Rows[0]["status"] != "SERVING" {
		t.Fatalf("health %v %v", c.Rows, err)
	}
}

func TestReadsOnlyReadMethods(t *testing.T) {
	addr, impl := serve(t)
	ctx := context.Background()
	g := open(t, connector.ModeRead, connector.StaticFields{"address": addr})
	var c connector.Collector
	err := g.Do(ctx, connector.Call{Verb: "call", Args: []string{"grpc.testing.TestService/UnaryCall", `{"responseSize":3}`}}, &c)
	if errs.CodeOf(err) != errs.PolicyBlocked || impl.calls != 0 {
		t.Fatalf("write method as read: %v calls=%d", err, impl.calls)
	}
	// a read method is fine
	if err := g.Do(ctx, connector.Call{Verb: "call", Args: []string{"grpc.health.v1.Health/Check"}}, &c); err != nil || c.Rows[0]["status"] != "SERVING" {
		t.Fatalf("health call %v %v", c.Rows, err)
	}
	// declared a read in config → allowed; bearer token sent as metadata
	g2 := open(t, connector.ModeRead, connector.StaticFields{"address": addr, "read_methods": []any{"grpc.testing.TestService/Unary*", "StreamingOutputCall"}, "token": "FAKE_TOKEN"})
	c = connector.Collector{}
	if err := g2.Do(ctx, connector.Call{Verb: "call", Args: []string{"grpc.testing.TestService.UnaryCall", `{"responseSize":3}`}}, &c); err != nil || c.Rows[0]["username"] != "u3" {
		t.Fatalf("declared read %v %v", c.Rows, err)
	}
	if impl.auth != "Bearer FAKE_TOKEN" {
		t.Fatalf("auth metadata %q", impl.auth)
	}
	c = connector.Collector{}
	if err := g2.Do(ctx, connector.Call{Verb: "call", Args: []string{"grpc.testing.TestService/StreamingOutputCall", "{}"}, Limit: 3}, &c); err != nil || len(c.Rows) != 3 {
		t.Fatalf("stream cap %v %v", c.Rows, err)
	}
	// bidi never runs as a read even if declared
	g3 := open(t, connector.ModeRead, connector.StaticFields{"address": addr, "read_methods": []any{"*"}})
	if err := g3.Do(ctx, connector.Call{Verb: "call", Args: []string{"grpc.testing.TestService/FullDuplexCall"}}, &c); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("bidi as read: %v", err)
	}
}

func TestWritePreviewAndSaved(t *testing.T) {
	addr, impl := serve(t)
	ctx := context.Background()
	g := open(t, connector.ModeWrite, connector.StaticFields{"address": addr})
	var c connector.Collector
	if err := g.Do(ctx, connector.Call{Verb: "call", Write: true, Preview: true, Args: []string{"grpc.testing.TestService/UnaryCall", `{"responseSize":4}`}}, &c); err != nil || impl.calls != 0 || c.Rows[0]["would_call"] != "/grpc.testing.TestService/UnaryCall" {
		t.Fatalf("preview %v %v calls=%d", c.Rows, err, impl.calls)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "call", Write: true, Args: []string{"grpc.testing.TestService/UnaryCall", `{"responseSize":4}`}}, &c); err != nil || impl.calls != 1 {
		t.Fatalf("write %v %v", c.Rows, err)
	}
	// saved call with a param, run as a read
	dir := g.t.AssetsDir()
	os.MkdirAll(filepath.Join(dir, "calls"), 0o755)
	os.WriteFile(filepath.Join(dir, "calls", "status.json"), []byte("---\ndescription: health of one service\nmethod: grpc.health.v1.Health/Check\nparams: [svc]\n---\n{\"service\": \"{svc}\"}\n"), 0o644)
	r := open(t, connector.ModeRead, connector.StaticFields{"address": addr})
	r.t.Dir = g.t.Dir
	c = connector.Collector{}
	err := r.Do(ctx, connector.Call{Verb: "call", Args: []string{"@status", "svc="}}, &c)
	if err != nil || c.Rows[0]["status"] != "SERVING" {
		t.Fatalf("saved %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := r.Do(ctx, connector.Call{Verb: "ls"}, &c); err != nil || !strings.Contains(strings.ToLower(c.Rows[0]["name"].(string)), "status") {
		t.Fatalf("ls saved %v %v", c.Rows, err)
	}
}

func TestBadConfig(t *testing.T) {
	for _, a := range []string{"", "nohost"} {
		g := New().(*GRPC)
		err := g.Open(context.Background(), connector.NewTarget(connector.Target{System: "api", Mode: connector.ModeRead}, connector.StaticFields{"address": a}))
		if errs.CodeOf(err) != errs.Config {
			t.Errorf("%q: %v", a, err)
		}
	}
}

func TestProtosetWithoutReflection(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	s := grpc.NewServer()
	impl := &svc{}
	testpb.RegisterTestServiceServer(s, impl)
	go s.Serve(l)
	t.Cleanup(s.Stop)

	// descriptor set for test.proto and its imports
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var add func(fd protoreflect.FileDescriptor)
	add = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		for i := 0; i < fd.Imports().Len(); i++ {
			add(fd.Imports().Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
	}
	add(testpb.File_grpc_testing_test_proto)
	b, _ := proto.Marshal(set)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "api.binpb"), b, 0o644)

	g := New().(*GRPC)
	tg := connector.NewTarget(connector.Target{System: "api", Mode: connector.ModeRead, Dir: dir, Dialer: &net.Dialer{}},
		connector.StaticFields{"address": l.Addr().String(), "protoset": "api.binpb", "read_methods": []any{"UnaryCall"}})
	if err := g.Open(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	var c connector.Collector
	if err := g.Do(context.Background(), connector.Call{Verb: "call", Args: []string{"grpc.testing.TestService/UnaryCall", `{"responseSize":7}`}}, &c); err != nil || c.Rows[0]["username"] != "u7" {
		t.Fatalf("protoset call %v %v", c.Rows, err)
	}
}
