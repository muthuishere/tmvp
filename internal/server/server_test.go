package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/audit"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/secret"
)

// ---- fake connector + tunneler -------------------------------------------

type fake struct {
	t   *connector.Target
	dsn string
}

var (
	fakeMu    sync.Mutex
	fakeOpens = map[string]int{}
	fakeDo    = map[string]int{}
	fakeClose = map[*fake]int{}
)

func init() {
	connector.Register("fake", func() connector.Connector { return &fake{} })
	connector.RegisterTunneler("fakehop", hopper{})
}

func (f *fake) Type() string { return "fake" }
func (f *fake) Verbs() []connector.Verb {
	return []connector.Verb{
		{Name: "echo", Mode: connector.Read},
		{Name: "dial", Mode: connector.Read},
		{Name: "mut", Mode: connector.Write, Default: true},
		{Name: "nuke", Mode: connector.Destructive},
		{Name: "rw", Mode: connector.ReadWrite},
		{Name: "authr", Mode: connector.Read},
		{Name: "authw", Mode: connector.Write},
		{Name: "show", Mode: connector.Read, Saved: true},
	}
}
func (f *fake) Open(ctx context.Context, t *connector.Target) error {
	f.t = t
	dsn, err := t.ModeString(ctx, "dsn")
	if err != nil {
		return err
	}
	fakeMu.Lock()
	fakeOpens[t.Env+"/"+t.System]++
	fakeMu.Unlock()
	if strings.Contains(dsn, "BAD") {
		return errs.AuthError(fmt.Errorf("password rejected"))
	}
	f.dsn = dsn
	return nil
}
func (f *fake) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	switch c.Verb {
	case "echo":
		// deliberately leaks the secret: the redactor must catch it
		_ = e.Text("dsn is " + f.dsn)
		return e.Row(connector.Row{"mode": string(f.t.Mode), "args": strings.Join(c.Args, " "), "leak": f.dsn})
	case "dial":
		conn, err := f.t.Dial(ctx, "tcp", "x:1")
		if err != nil {
			return err
		}
		defer conn.Close()
		return e.Row(connector.Row{"via": conn.RemoteAddr().String()})
	case "authr", "authw":
		fakeMu.Lock()
		fakeDo[c.Verb]++
		fakeMu.Unlock()
		return errs.AuthError(fmt.Errorf("rejected"))
	case "rw", "show":
		manual := ""
		if f.t.Login != nil {
			manual = f.t.Login.Manual
		}
		return e.Row(connector.Row{"verb": c.Verb, "mode": string(f.t.Mode), "write": c.Write, "args": strings.Join(c.Args, " "), "login": manual})
	case "mut", "nuke":
		return e.Row(connector.Row{"done": c.Verb, "args": c.Args, "preview": c.Preview})
	}
	return errs.New(errs.Usage, "bad verb")
}
func (f *fake) Close() error { fakeMu.Lock(); fakeClose[f]++; fakeMu.Unlock(); return nil }

type hopper struct{}

type hopTunnel struct{ name string }

func (h hopTunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	a, b := net.Pipe()
	_ = b
	return fakeAddrConn{a, h.name}, nil
}
func (h hopTunnel) Close() error { return nil }

type fakeAddrConn struct {
	net.Conn
	name string
}

func (c fakeAddrConn) RemoteAddr() net.Addr { return fakeAddr(c.name) }

type fakeAddr string

func (a fakeAddr) Network() string { return "fake" }
func (a fakeAddr) String() string  { return string(a) }

func (hopper) OpenTunnel(ctx context.Context, hop *connector.Target, parent connector.Dialer) (connector.Tunnel, error) {
	return hopTunnel{name: "via-" + hop.System}, nil
}

// ---- helpers ---------------------------------------------------------------

type secrets struct {
	mu  sync.Mutex
	m   map[string]string
	ran map[string]int
}

func (s *secrets) runner() secret.Runner {
	return secret.RunnerFunc(func(ctx context.Context, cmd string, file bool, dir string) ([]byte, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.ran[cmd]++
		v, ok := s.m[cmd]
		if !ok {
			return nil, fmt.Errorf("permission denied")
		}
		return []byte(v), nil
	})
}

func (s *secrets) set(cmd, v string) { s.mu.Lock(); s.m[cmd] = v; s.mu.Unlock() }

const cfgYAML = `
version: 1
product: %s
logging: { path: ./logs }
envs:
  dev:
    destructive: true
    secrets: { dsn: "$(get dev)" }
    systems:
      db: { type: fake, read: { dsn: "${secrets.dsn}" }, write: { dsn: "${secrets.dsn}" }, labels: { role: primary } }
  prod:
    secrets: { dsn: "$(get prod)" }
    systems:
      hop: { type: fakehop }
      db: { type: fake, via: hop, read: { dsn: "${secrets.dsn}" }, write: { dsn: "${secrets.dsn}" } }
      locked: { type: fake, dsn: "x", login: { manual: "acme login", check: "acme status" } }
      nohop: { type: fake, via: plain, dsn: "x" }
      plain: { type: fake, dsn: "x" }
      browsered: { type: fake, dsn: "x", login: { manual: browser } }
`

func writeCfg(t *testing.T, product string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(fmt.Sprintf(cfgYAML, product)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newEngine(s *secrets) *Engine {
	e := NewEngine()
	e.Runner = s.runner()
	e.Login.Run = func(ctx context.Context, cmd, dir string) error { return fmt.Errorf("not logged in") }
	return e
}

type result struct {
	exit   int
	frames []protocol.Frame
	raw    string
}

func call(t *testing.T, e *Engine, req protocol.Request) result {
	t.Helper()
	var buf bytes.Buffer
	Serve(context.Background(), e, req, &buf)
	var r result
	r.raw = buf.String()
	for _, line := range strings.Split(strings.TrimSpace(r.raw), "\n") {
		var f protocol.Frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("bad frame %q: %v", line, err)
		}
		if f.Type == protocol.FrameDone {
			r.exit = f.Exit
		} else {
			r.frames = append(r.frames, f)
		}
	}
	return r
}

func (r result) of(typ string) []protocol.Frame {
	var out []protocol.Frame
	for _, f := range r.frames {
		if f.Type == typ {
			out = append(out, f)
		}
	}
	return out
}

func newSecrets() *secrets {
	return &secrets{m: map[string]string{"get dev": "FAKE_DEV_DSN_123", "get prod": "FAKE_PROD_DSN_456"}, ran: map[string]int{}}
}

// ---- tests -----------------------------------------------------------------

func TestReadBatchRedactsAndTags(t *testing.T) {
	s := newSecrets()
	e := newEngine(s)
	p := writeCfg(t, "alpha")
	r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "all", System: "db", Verb: "echo", Args: []string{"hi"}})
	if r.exit != 0 {
		t.Fatalf("exit %d: %s", r.exit, r.raw)
	}
	if strings.Contains(r.raw, "FAKE_DEV_DSN_123") || strings.Contains(r.raw, "FAKE_PROD_DSN_456") {
		t.Fatalf("secret leaked: %s", r.raw)
	}
	rows := r.of(protocol.FrameRow)
	if len(rows) != 2 {
		t.Fatalf("rows: %s", r.raw)
	}
	for _, f := range rows {
		if f.Row["env"] != f.Env || f.Row["mode"] != "read" || f.Row["leak"] != secret.Mask {
			t.Fatalf("%+v", f.Row)
		}
	}
	// warm: second call does not reopen or re-run secrets
	call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "db", Verb: "echo"})
	if s.ran["get dev"] != 1 {
		t.Fatalf("secret re-run: %v", s.ran)
	}
	// audit has every command, redacted
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "logs", audit.FileName))
	if strings.Count(string(b), `"body":"command"`) != 3 || strings.Contains(string(b), "FAKE_DEV") {
		t.Fatalf("audit: %s", b)
	}
}

func TestMissingSecretFailsOnlyItsSlot(t *testing.T) {
	s := newSecrets()
	delete(s.m, "get prod")
	e := newEngine(s)
	p := writeCfg(t, "alpha")
	r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev,prod", System: "db", Verb: "echo"})
	if r.exit != 4 {
		t.Fatalf("want partial exit 4, got %d: %s", r.exit, r.raw)
	}
	errsF := r.of(protocol.FrameError)
	if len(errsF) != 1 || errsF[0].Env != "prod" || errsF[0].Error.Code != errs.SecretMissing {
		t.Fatalf("%s", r.raw)
	}
	r = call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "prod", System: "db", Verb: "echo"})
	if r.exit != 2 {
		t.Fatalf("secret_missing exit 2, got %d", r.exit)
	}
}

func TestWritePolicy(t *testing.T) {
	e := newEngine(newSecrets())
	p := writeCfg(t, "alpha")
	cases := []struct {
		name string
		req  protocol.Request
		exit int
	}{
		{"no reason", protocol.Request{Op: protocol.OpWrite, Env: "dev", System: "db", Verb: "mut"}, 3},
		{"multi env", protocol.Request{Op: protocol.OpWrite, Env: "dev,prod", System: "db", Verb: "mut", Reason: "r"}, 3},
		{"write verb via read", protocol.Request{Op: protocol.OpRun, Env: "dev", System: "db", Verb: "mut"}, 3},
		{"destructive blocked", protocol.Request{Op: protocol.OpWrite, Env: "prod", System: "plain", Verb: "nuke", Reason: "r"}, 3},
		{"destructive allowed", protocol.Request{Op: protocol.OpWrite, Env: "dev", System: "db", Verb: "nuke", Reason: "r"}, 0},
		{"read verb via write", protocol.Request{Op: protocol.OpWrite, Env: "dev", System: "db", Verb: "echo", Reason: "r"}, 2},
		{"default write verb", protocol.Request{Op: protocol.OpWrite, Env: "dev", System: "db", Verb: "UPDATE x", Reason: "r", Preview: true}, 0},
	}
	for _, tc := range cases {
		tc.req.ConfigPath = p
		r := call(t, e, tc.req)
		if r.exit != tc.exit {
			t.Errorf("%s: exit %d want %d: %s", tc.name, r.exit, tc.exit, r.raw)
		}
	}
	r := call(t, e, protocol.Request{Op: protocol.OpWrite, ConfigPath: p, Env: "dev", System: "db", Verb: "UPDATE x", Reason: "r"})
	if row := r.of(protocol.FrameRow)[0].Row; row["done"] != "mut" || row["args"].([]any)[0] != "UPDATE x" {
		t.Fatalf("%+v", row)
	}
	recs, _ := audit.Read(filepath.Join(filepath.Dir(p), "logs"), audit.ReadOptions{WritesOnly: true})
	var intents, outcomes int
	for _, rc := range recs {
		switch rc.Attributes["phase"] {
		case "intent":
			intents++
		case "outcome":
			outcomes++
		}
	}
	if intents == 0 || intents != outcomes {
		t.Fatalf("intents %d outcomes %d", intents, outcomes)
	}
}

func TestAuthErrorRefetchesAndRetriesOnce(t *testing.T) {
	s := newSecrets()
	s.set("get dev", "FAKE_BAD_1")
	e := newEngine(s)
	p := writeCfg(t, "alpha")
	// same value after re-fetch → secret_invalid
	r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "db", Verb: "echo"})
	if r.exit != 2 || r.of(protocol.FrameError)[0].Error.Code != errs.SecretInvalid {
		t.Fatalf("%d %s", r.exit, r.raw)
	}
	// rotated: first fetch bad, re-fetch good → retry succeeds
	s.set("get dev", "FAKE_BAD_2")
	e2 := newEngine(s)
	rotating := secret.RunnerFunc(func(ctx context.Context, cmd string, file bool, dir string) ([]byte, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.ran[cmd]++
		if s.ran[cmd] == 1 {
			return []byte("FAKE_BAD_3"), nil
		}
		return []byte("FAKE_GOOD_3"), nil
	})
	s.ran = map[string]int{}
	e2.Runner = rotating
	r = call(t, e2, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "db", Verb: "echo"})
	if r.exit != 0 || s.ran["get dev"] != 2 {
		t.Fatalf("exit %d ran %v: %s", r.exit, s.ran, r.raw)
	}
	if strings.Contains(r.raw, "FAKE_GOOD_3") || strings.Contains(r.raw, "FAKE_BAD_3") {
		t.Fatal("leak")
	}
}

func TestViaTunnelAndMissingTunneler(t *testing.T) {
	e := newEngine(newSecrets())
	p := writeCfg(t, "alpha")
	r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "prod", System: "db", Verb: "dial"})
	if r.exit != 0 || r.of(protocol.FrameRow)[0].Row["via"] != "via-hop" {
		t.Fatalf("%s", r.raw)
	}
	r = call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "prod", System: "nohop", Verb: "dial"})
	if r.exit == 0 || r.of(protocol.FrameError)[0].Error.Code != errs.Unsupported {
		t.Fatalf("%s", r.raw)
	}
}

func TestLoginRequired(t *testing.T) {
	e := newEngine(newSecrets())
	p := writeCfg(t, "alpha")
	r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "prod", System: "locked", Verb: "echo"})
	if r.exit != 2 {
		t.Fatalf("%s", r.raw)
	}
	er := r.of(protocol.FrameError)[0].Error
	if er.Code != errs.LoginRequired || !strings.Contains(er.Hint, "acme login") {
		t.Fatalf("%+v", er)
	}
}

func TestProductIsolationAndReload(t *testing.T) {
	s := newSecrets()
	e := newEngine(s)
	pa, pb := writeCfg(t, "alpha"), writeCfg(t, "beta")
	call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: pa, Env: "dev", System: "db", Verb: "echo"})
	call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: pb, Env: "dev", System: "db", Verb: "echo"})
	st := call(t, e, protocol.Request{Op: protocol.OpStatus})
	info := st.of(protocol.FrameInfo)[0].Info
	if n := len(info["products"].([]any)); n != 2 {
		t.Fatalf("products %d", n)
	}
	// hot reload: change dev's secret command in alpha only
	b, _ := os.ReadFile(pa)
	_ = os.WriteFile(pa, bytes.Replace(b, []byte("$(get dev)"), []byte("$(get dev2)"), 1), 0o600)
	s.set("get dev2", "FAKE_DEV2_789")
	r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: pa, Env: "dev", System: "db", Verb: "echo"})
	if r.exit != 0 || s.ran["get dev2"] != 1 {
		t.Fatalf("reload: %v %s", s.ran, r.raw)
	}
	if strings.Contains(r.raw, "FAKE_DEV2_789") {
		t.Fatal("leak after reload")
	}
	// a request for alpha can only name alpha's systems (selector is per config)
	r = call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: pa, Env: "dev", System: "nonexistent", Verb: "echo"})
	if r.exit != 2 {
		t.Fatal(r.raw)
	}
	// secrets clear drops values; next call re-runs
	call(t, e, protocol.Request{Op: protocol.OpSecretsClear, ConfigPath: pa})
	call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: pa, Env: "dev", System: "db", Verb: "echo"})
	if s.ran["get dev2"] != 2 {
		t.Fatalf("clear: %v", s.ran)
	}
}

func TestSocketServer(t *testing.T) {
	dir, err := os.MkdirTemp("", "tmv")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	// a user-chosen socket dir must not be chmod'ed by the server
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s.sock")
	e := newEngine(newSecrets())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, e, Options{Socket: sock, Idle: 300 * time.Millisecond}) }()
	var c net.Conn
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(sock); st != nil && os.PathSeparator == '/' && st.Mode().Perm() != 0o600 {
		t.Fatalf("socket perm %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(dir); os.PathSeparator == '/' && st.Mode().Perm() != 0o755 {
		t.Fatalf("server changed the user's dir perms to %v", st.Mode().Perm())
	}
	p := writeCfg(t, "alpha")
	b, _ := json.Marshal(protocol.Request{Proto: protocol.Version, Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "db", Verb: "echo"})
	_, _ = c.Write(append(b, '\n'))
	var out bytes.Buffer
	_, _ = out.ReadFrom(c)
	c.Close()
	if !strings.Contains(out.String(), `"type":"done"`) || strings.Contains(out.String(), "FAKE_DEV_DSN_123") {
		t.Fatalf("%s", out.String())
	}
	// second server refuses
	if err := Run(context.Background(), NewEngine(), Options{Socket: sock}); err != ErrAlreadyRunning {
		t.Fatalf("want ErrAlreadyRunning, got %v", err)
	}
	// idle exit
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit on idle")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket not removed")
	}
}

func TestReadWriteVerbSavedRouteAndLoginSpec(t *testing.T) {
	e := newEngine(newSecrets())
	p := writeCfg(t, "alpha")
	row := func(req protocol.Request) map[string]any {
		t.Helper()
		req.ConfigPath = p
		r := call(t, e, req)
		if r.exit != 0 || len(r.of(protocol.FrameRow)) != 1 {
			t.Fatalf("%+v: %s", req, r.raw)
		}
		return r.of(protocol.FrameRow)[0].Row
	}
	// ReadWrite verb runs as a read (read creds, Write=false) ...
	if r := row(protocol.Request{Op: protocol.OpRun, Env: "dev", System: "db", Verb: "rw", Args: []string{"a"}}); r["mode"] != "read" || r["write"] != false {
		t.Fatalf("read: %+v", r)
	}
	// ... and through tmvp write (write creds, Write=true), not re-routed to the default verb
	if r := row(protocol.Request{Op: protocol.OpWrite, Env: "dev", System: "db", Verb: "rw", Args: []string{"a"}, Reason: "r"}); r["verb"] != "rw" || r["mode"] != "write" || r["write"] != true || r["args"] != "a" {
		t.Fatalf("write: %+v", r)
	}
	// bare @item goes to the Saved verb on reads
	if r := row(protocol.Request{Op: protocol.OpRun, Env: "dev", System: "db", Verb: "@disk", Args: []string{"x"}}); r["verb"] != "show" || r["args"] != "@disk x" {
		t.Fatalf("saved: %+v", r)
	}
	// ... and to the Default write verb on writes
	if r := row(protocol.Request{Op: protocol.OpWrite, Env: "dev", System: "db", Verb: "@disk", Reason: "r"}); r["done"] != "mut" {
		t.Fatalf("saved write: %+v", r)
	}
	// login: {manual: browser} reaches the connector
	if r := row(protocol.Request{Op: protocol.OpRun, Env: "prod", System: "browsered", Verb: "rw"}); r["login"] != "browser" {
		t.Fatalf("login: %+v", r)
	}
}

func TestAuthRetryNeverRerunsAWrite(t *testing.T) {
	e := newEngine(newSecrets())
	var n int
	var mu sync.Mutex
	// every fetch yields a new value, so a refresh always "changes" the secret
	e.Runner = secret.RunnerFunc(func(ctx context.Context, cmd string, file bool, dir string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		return []byte(fmt.Sprintf("FAKE_ROTATING_%d", n)), nil
	})
	p := writeCfg(t, "alpha")
	call(t, e, protocol.Request{Op: protocol.OpWrite, ConfigPath: p, Env: "dev", System: "db", Verb: "authw", Reason: "r"})
	call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "db", Verb: "authr"})
	fakeMu.Lock()
	w, r := fakeDo["authw"], fakeDo["authr"]
	fakeMu.Unlock()
	if w != 1 {
		t.Fatalf("write ran %d times; an auth error after Open must not retry a write", w)
	}
	if r != 2 {
		t.Fatalf("read ran %d times; want one retry", r)
	}
}

func TestSameProductNameTwoConfigsIsolated(t *testing.T) {
	s := newSecrets()
	e := newEngine(s)
	p1 := writeCfg(t, "twin")
	dir := t.TempDir()
	p2 := filepath.Join(dir, "config.yaml")
	b, _ := os.ReadFile(p1)
	_ = os.WriteFile(p2, []byte(strings.ReplaceAll(string(b), "$(get dev)", "$(get other)")), 0o600)
	s.set("get other", "FAKE_OTHER_DSN_789")
	r1 := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p1, Env: "dev", System: "db", Verb: "echo"})
	r2 := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p2, Env: "dev", System: "db", Verb: "echo"})
	if r1.exit != 0 || r2.exit != 0 {
		t.Fatal(r1.raw, r2.raw)
	}
	s.mu.Lock()
	ranOther := s.ran["get other"]
	s.mu.Unlock()
	if ranOther != 1 {
		t.Fatalf("second config reused the first config's product state (get other ran %d)", ranOther)
	}
	if n := len(e.status()["products"].([]map[string]any)); n != 2 {
		t.Fatalf("want 2 products, got %d", n)
	}
}

func TestRetireWaitsForInFlightCalls(t *testing.T) {
	p := &product{conns: map[string]*conn{}}
	f := &fake{}
	c := &conn{c: f, refs: 1}
	p.conns["k"] = c
	p.mu.Lock()
	p.retire("k", c)
	p.mu.Unlock()
	fakeMu.Lock()
	n := fakeClose[f]
	fakeMu.Unlock()
	if n != 0 || p.conns["k"] != nil {
		t.Fatal("in-use conn closed on retire")
	}
	p.release(c)
	fakeMu.Lock()
	n = fakeClose[f]
	fakeMu.Unlock()
	if n != 1 {
		t.Fatal("retired conn not closed by last release")
	}
}

// A client from another build gets server_stale (and nothing runs); the same
// build, no build (old clients) and stop/status/ping are served normally.
func TestServeRejectsOtherBuild(t *testing.T) {
	e := newEngine(nil)
	stale := func(r result) bool {
		for _, f := range r.of(protocol.FrameError) {
			if f.Error.Code == "server_stale" {
				return true
			}
		}
		return false
	}
	if r := call(t, e, protocol.Request{Proto: protocol.Version, Op: protocol.OpPing, Build: "other"}); r.exit != 0 || stale(r) {
		t.Fatalf("ping from another build: %s", r.raw)
	}
	if r := call(t, e, protocol.Request{Proto: protocol.Version, Op: protocol.OpRun, Build: "other"}); r.exit != 1 || !stale(r) {
		t.Fatalf("want server_stale: %s", r.raw)
	}
	for _, b := range []string{"", protocol.SelfBuild()} {
		if r := call(t, e, protocol.Request{Proto: protocol.Version, Op: protocol.OpRun, Build: b}); stale(r) {
			t.Fatalf("build %q treated as stale: %s", b, r.raw)
		}
	}
	if protocol.SelfBuild() == "" {
		t.Fatal("SelfBuild empty")
	}
}
