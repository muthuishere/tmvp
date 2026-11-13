package ssh

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func TestDockerArgvAllowList(t *testing.T) {
	ok := []struct {
		args  []string
		flags map[string]string
		since time.Duration
		limit int
		want  string
	}{
		{[]string{"ps"}, nil, 0, 0, "docker ps"},
		{[]string{"ps", "-a"}, nil, 0, 0, "docker ps -a"},
		{[]string{"ps"}, map[string]string{"all": "true"}, 0, 0, "docker ps -a"},
		{[]string{"images"}, nil, 0, 0, "docker images"},
		{[]string{"inspect", "api"}, nil, 0, 0, "docker inspect api"},
		{[]string{"top", "api"}, nil, 0, 0, "docker top api"},
		{[]string{"stats"}, nil, 0, 0, "docker stats --no-stream"},
		{[]string{"stats", "--no-stream", "api"}, nil, 0, 0, "docker stats --no-stream api"},
		{[]string{"logs", "api", "--tail", "50", "--since", "10m", "--timestamps"}, nil, 0, 0, "docker logs --tail 50 --since 10m --timestamps api"},
		{[]string{"logs", "api"}, nil, 5 * time.Minute, 200, "docker logs --tail 200 --since 300s api"},
		{[]string{"compose", "ps"}, map[string]string{"project": "/srv/app"}, 0, 0, "docker compose --project-directory /srv/app ps"},
		{[]string{"compose", "logs", "web", "--tail=10"}, map[string]string{"file": "/srv/app/compose.yml"}, 0, 0, "docker compose -f /srv/app/compose.yml logs --no-color --tail 10 web"},
	}
	for _, c := range ok {
		got, err := dockerArgv(c.args, c.flags, c.since, c.limit)
		if err != nil || strings.Join(got, " ") != c.want {
			t.Errorf("%v: got %q, %v; want %q", c.args, got, err, c.want)
		}
	}
	bad := [][]string{
		nil, {"rm", "api"}, {"exec", "api", "sh"}, {"run", "alpine"}, {"ps", "-q"}, {"ps", "--format", "x"},
		{"logs", "api", "-f"}, {"logs", "api", "--follow"}, {"logs", "api", "--details"}, {"logs"},
		{"logs", "a", "b"}, {"inspect", "api", "--format", "{{.Config.Env}}"}, {"inspect"}, {"inspect", "a;rm -rf /"},
		{"inspect", "$(id)"}, {"top", "api", "aux"}, {"images", "-a"}, {"compose", "up"}, {"compose", "down"},
		{"compose", "ps", "-q"}, {"compose", "logs", "web", "-f"}, {"logs", "api", "--since", "1m;id"},
		{"logs", "api", "--tail", "x"}, {"stats", "a", "b"}, {"system", "prune"},
	}
	for _, a := range bad {
		if _, err := dockerArgv(a, nil, 0, 0); err == nil {
			t.Errorf("%q accepted", a)
		}
	}
	if _, err := dockerArgv([]string{"compose", "ps"}, map[string]string{"project": "--evil"}, 0, 0); err == nil {
		t.Error("flag-looking project accepted")
	}
	if err := New().(*SSH).Guard(connector.Call{Verb: "docker", Args: []string{"rm", "api"}}); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Errorf("guard: %v", err)
	}
	if err := New().(*SSH).Guard(connector.Call{Verb: "run", Args: []string{"ls"}, Flags: map[string]string{"container": "a b"}}); err == nil {
		t.Error("bad container flag accepted")
	}
}

func TestQuotingAndMasking(t *testing.T) {
	if got := shJoin([]string{"docker", "logs", "it's"}); got != `'docker' 'logs' 'it'\''s'` {
		t.Fatalf("shJoin %s", got)
	}
	if got := inContainer("api", "echo 'hi' && ls $HOME", false); got != `docker exec 'api' sh -c 'echo '\''hi'\'' && ls $HOME'` {
		t.Fatalf("inContainer %s", got)
	}
	if got := inContainer("api", "id", true); !strings.HasPrefix(got, "sudo -n docker exec") {
		t.Fatalf("sudo %s", got)
	}
	got := scriptInContainer("api", "echo TMVP_SCRIPT_EOF\n", []string{"a b"}, false)
	want := "docker exec -i 'api' sh -s -- 'a b' <<'TMVP_SCRIPT_EOF_X'\necho TMVP_SCRIPT_EOF\nTMVP_SCRIPT_EOF_X\n"
	if got != want {
		t.Fatalf("script %q", got)
	}
	m, err := maskInspect(`[{"Name":"/api","Config":{"Env":["DB_PASSWORD=FAKE_SECRET_123","PATH=/usr/bin","EMPTY"]}}]`)
	if err != nil || strings.Contains(m, "FAKE_SECRET_123") || !strings.Contains(m, `"DB_PASSWORD=***"`) || !strings.Contains(m, `"EMPTY=***"`) {
		t.Fatalf("mask %v %s", err, m)
	}
	if _, err := maskInspect("Error: FAKE_SECRET_123 not json"); err == nil || strings.Contains(err.Error(), "FAKE_SECRET_123") {
		t.Fatalf("unparsable inspect: %v", err)
	}
	for _, n := range []string{"pg", "my_app-1.web"} {
		if validName(n) != nil {
			t.Errorf("%s rejected", n)
		}
	}
	for _, n := range []string{"", "-x", ".x", "a b", "a;b", "a/b", "$(x)", "a'b"} {
		if validName(n) == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestPickIP(t *testing.T) {
	js := `true|{"zeta":{"IPAddress":"172.20.0.9"},"alpha":{"IPAddress":"172.18.0.4"}}`
	if ip, err := pickIP(js, "", "c"); err != nil || ip != "172.18.0.4" {
		t.Fatalf("first: %s %v", ip, err)
	}
	if ip, err := pickIP(js, "zeta", "c"); err != nil || ip != "172.20.0.9" {
		t.Fatalf("named: %s %v", ip, err)
	}
	if _, err := pickIP(js, "nope", "c"); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("missing net: %v", err)
	}
	if _, err := pickIP(`false|{}`, "", "c"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("stopped: %v", err)
	}
	if _, err := pickIP(`true|{"host":{"IPAddress":""}}`, "", "c"); err == nil {
		t.Fatal("no ip accepted")
	}
}

// setStub installs a fake `docker` on the fake ssh server for one test.
const psAll = "docker ps -a --format '{{.Names}}|{{.State}}'"

func TestMatchContainer(t *testing.T) {
	all := []psEntry{{"api", "running"}, {"app-web-41068e_uncommitted_dc56", "running"}, {"app-web-0aa1", "exited"},
		{"app-webhook", "running"}, {"ui-web-1", "exited"}, {"ui-web-2", "exited"}, {"db-1", "running"}, {"db-2", "running"}}
	for want, exp := range map[string]string{
		"api":          "api",                             // exact
		"app-web":      "app-web-41068e_uncommitted_dc56", // running beats exited; app-webhook is not app-web-…
		"app-web-0aa1": "app-web-0aa1",                    // exact, even stopped
		"db":           "!several", "ui-web": "!several", "nope": "!not found", "ap": "!not found",
	} {
		got, err := matchContainer(all, want)
		if strings.HasPrefix(exp, "!") {
			if err == nil || !strings.Contains(err.Error(), exp[1:]) {
				t.Errorf("%s: want error %q, got %q %v", want, exp[1:], got, err)
			}
		} else if err != nil || got != exp {
			t.Errorf("%s: got %q %v, want %q", want, got, err, exp)
		}
	}
}

func setStub(t *testing.T, f func(string) (string, uint32, bool)) {
	dockerStub = f
	t.Cleanup(func() { dockerStub = nil })
}

// TestContainerHopE2E dials an echo "database" through ssh + {container: pg}.
func TestContainerHopE2E(t *testing.T) {
	cs, keyPEM := genKey(t)
	srv := startServer(t, cs.PublicKey())
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	_, port, _ := net.SplitHostPort(echo.Addr().String())
	var inspects atomic.Int32
	setStub(t, func(l string) (string, uint32, bool) {
		if strings.HasPrefix(l, psAll) {
			return "pg|running\nstopped|exited\ndenied|running\n", 0, true
		}
		if !strings.Contains(l, "docker inspect -f") {
			return "", 0, false
		}
		inspects.Add(1)
		switch {
		case strings.Contains(l, "'pg'"):
			return `true|{"backend":{"IPAddress":"127.0.0.1"},"bridge":{"IPAddress":"10.255.255.1"}}` + "\n", 0, true
		case strings.Contains(l, "'stopped'"):
			return "false|{}\n", 0, true
		case strings.Contains(l, "'denied'"):
			return "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock\n", 1, true
		}
		return "Error: No such object: gone\n", 1, true
	})
	ctx := context.Background()
	st, err := Tunneler{}.OpenTunnel(ctx, target(t, srv, keyPEM, connector.ModeRead, nil), connector.DirectDialer())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hop := func(f connector.StaticFields) *ContainerTunnel {
		ct, err := ContainerTunneler{}.OpenTunnel(ctx, connector.NewTarget(connector.Target{System: "container:x"}, f), st)
		if err != nil {
			t.Fatal(err)
		}
		return ct.(*ContainerTunnel)
	}
	ct := hop(connector.StaticFields{"container": "pg", "network": "backend"})
	now := time.Now()
	ct.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		c, err := ct.DialContext(ctx, "tcp", "pg.ignored:"+port)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("ping\n"))
		got, _ := bufio.NewReader(c).ReadString('\n')
		c.Close()
		if got != "ping\n" {
			t.Fatalf("echo %q", got)
		}
	}
	if inspects.Load() != 1 {
		t.Fatalf("cached resolve: %d inspects", inspects.Load())
	}
	now = now.Add(resolveTTL + time.Second)
	if c, err := ct.DialContext(ctx, "tcp", "x:"+port); err != nil {
		t.Fatal(err)
	} else {
		c.Close()
	}
	if inspects.Load() != 2 {
		t.Fatalf("re-resolve after ttl: %d inspects", inspects.Load())
	}
	for name, want := range map[string]string{"gone": "not found", "stopped": "not running", "denied": "docker group"} {
		_, err := hop(connector.StaticFields{"container": name}).DialContext(ctx, "tcp", "x:5432")
		if err == nil || !strings.Contains(err.Error()+errs.As(err).Hint, want) {
			t.Errorf("%s: want %q, got %v (hint %q)", name, want, err, errs.As(err).Hint)
		}
	}
	if _, err := (ContainerTunneler{}).OpenTunnel(ctx, connector.NewTarget(connector.Target{}, connector.StaticFields{"container": "pg"}), connector.DirectDialer()); errs.CodeOf(err) != errs.Config {
		t.Fatalf("non-ssh parent: %v", err)
	}
	if _, ok := connector.LookupTunneler("container"); !ok {
		t.Fatal("container tunneler not registered")
	}
}

func TestDockerVerbsE2E(t *testing.T) {
	cs, keyPEM := genKey(t)
	srv := startServer(t, cs.PublicKey())
	setStub(t, func(l string) (string, uint32, bool) {
		switch {
		case strings.HasPrefix(l, psAll):
			return "api|running\napp-web-abc_uncommitted_1|running\napp-web-old|exited\n", 0, true
		case l == "'docker' 'logs' '--tail' '1' 'app-web-abc_uncommitted_1'":
			return "hello\n", 0, true
		case strings.HasPrefix(l, "docker ps --format"):
			return `{"Names":"api","Image":"app:1","Status":"Up 2 hours","Ports":"0.0.0.0:8080->8080/tcp"}` + "\n", 0, true
		case l == "'docker' 'ps' '-a'":
			return "CONTAINER ID   IMAGE   NAMES\nabc   app:1   api\n", 0, true
		case l == "'docker' 'inspect' 'api'":
			return `[{"Name":"/api","Config":{"Env":["DB_PASSWORD=FAKE_SECRET_123"]}}]` + "\n", 0, true
		case l == "'docker' 'logs' '--tail' '2' 'gone'":
			return "Error: No such container: gone\n", 1, true
		}
		return "", 0, false
	})
	ctx := context.Background()
	s := New().(*SSH)
	tg := target(t, srv, keyPEM, connector.ModeRead, nil)
	if err := s.Open(ctx, tg); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var c connector.Collector
	if err := s.Do(ctx, connector.Call{Verb: "docker", Args: []string{"ps", "-a"}}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Lines) != 2 || !strings.Contains(c.Lines[1], "api") {
		t.Fatalf("ps %q", c.Lines)
	}
	c = connector.Collector{}
	if err := s.Do(ctx, connector.Call{Verb: "docker", Args: []string{"inspect", "api"}}, &c); err != nil {
		t.Fatal(err)
	}
	if out := strings.Join(c.Lines, "\n"); strings.Contains(out, "FAKE_SECRET_123") || !strings.Contains(out, "DB_PASSWORD=***") {
		t.Fatalf("inspect %s", out)
	}
	if err := s.Do(ctx, connector.Call{Verb: "docker", Args: []string{"logs", "gone"}, Limit: 2}, &c); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("logs gone: %v", err)
	}
	c = connector.Collector{}
	if err := s.Do(ctx, connector.Call{Verb: "docker", Args: []string{"logs", "app-web"}, Limit: 1}, &c); err != nil || strings.Join(c.Lines, "") != "hello" {
		t.Fatalf("logs by prefix: %q %v", c.Lines, err)
	}
	if err := s.Do(ctx, connector.Call{Verb: "docker", Args: []string{"exec", "api", "sh"}}, &c); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("exec via read: %v", err)
	}
	c = connector.Collector{}
	if err := s.Do(ctx, connector.Call{Verb: "ls"}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 1 || c.Rows[0]["kind"] != "containers" || c.Rows[0]["name"] != "api" || c.Rows[0]["image"] != "app:1" {
		t.Fatalf("ls %v", c.Rows)
	}

	// script @name -F container=api (read) pipes the script into docker exec -i
	writeScript(t, tg, "disk.sh", "---\nread: true\n---\ndf -h\n")
	c = connector.Collector{}
	if err := s.Do(ctx, connector.Call{Verb: "script", Args: []string{"@disk", "a b"}, Flags: map[string]string{"container": "api"}}, &c); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Lines, "|") != "out: docker exec -i 'api' sh -s -- 'a b' <<'TMVP_SCRIPT_EOF'|out: df -h|out: TMVP_SCRIPT_EOF" {
		t.Fatalf("script %q", c.Lines)
	}

	w := New().(*SSH)
	if err := w.Open(ctx, target(t, srv, keyPEM, connector.ModeWrite, nil)); err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	c = connector.Collector{}
	if err := w.Do(ctx, connector.Call{Verb: "run", Args: []string{"rm -f /tmp/it's"}, Flags: map[string]string{"container": "api"}}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Lines) != 1 || c.Lines[0] != `out: docker exec 'api' sh -c 'rm -f /tmp/it'\''s'` {
		t.Fatalf("run %q", c.Lines)
	}
}

func writeScript(t *testing.T, tg *connector.Target, name, body string) {
	t.Helper()
	d := filepath.Join(tg.AssetsDir(), "scripts")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
