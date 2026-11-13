package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/envsel"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

const envCfg = `product: envtest
default_env: dev
envs:
  dev:
    destructive: true
    systems:
      db: { type: postgres, read: { dsn: "postgres://u@127.0.0.1:1/x?sslmode=disable&connect_timeout=1" }, write: { dsn: "postgres://u@127.0.0.1:1/x?sslmode=disable&connect_timeout=1" } }
  staging:
    systems:
      db: { type: postgres, read: { dsn: "postgres://u@127.0.0.1:1/x?sslmode=disable&connect_timeout=1" } }
  prod:
    systems:
      db: { type: postgres, read: { dsn: "postgres://u@127.0.0.1:1/x?sslmode=disable&connect_timeout=1" }, write: { dsn: "postgres://u@127.0.0.1:1/x?sslmode=disable&connect_timeout=1" } }
`

// envSetup isolates state and writes cfg; returns the captured output.
func envSetup(t *testing.T, cfg string) *bytes.Buffer {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMVP_HOME", t.TempDir())
	t.Setenv("TMVP_NO_SERVER", "1")
	t.Setenv("TMVP_ENV", "")
	t.Setenv("TMVP_PRODUCT", "")
	t.Chdir(t.TempDir())
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	stdout, stderr = &out, &out
	old := globals
	t.Cleanup(func() { stdout, stderr, globals = os.Stdout, os.Stderr, old })
	globals = Globals{Config: p}
	return &out
}

// run executes one command line; returns exit code.
func run(t *testing.T, out *bytes.Buffer, args ...string) int {
	t.Helper()
	out.Reset()
	cfgPath := globals.Config
	globals = Globals{}
	root := newRoot()
	root.SetArgs(append([]string{"--config", cfgPath}, args...))
	err := root.Execute()
	globals.Config = cfgPath
	if err == nil {
		return 0
	}
	if c := doctorExit(err); c > 0 {
		return c
	}
	te := errs.As(err)
	printError(te)
	return errs.ExitCode(te.Code)
}

func TestEnvPrecedenceMatrix(t *testing.T) {
	envSetup(t, envCfg)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name             string
		positional, flag string
		tmvpEnv, use     string
		noDefault        bool
		wantEnv, wantSrc string
		wantErr          bool
	}{
		{name: "default only", wantEnv: "dev", wantSrc: "default"},
		{name: "use beats default", use: "staging", wantEnv: "staging", wantSrc: "use"},
		{name: "TMVP_ENV beats use", use: "staging", tmvpEnv: "prod", wantEnv: "prod", wantSrc: "TMVP_ENV"},
		{name: "--env beats TMVP_ENV", use: "staging", tmvpEnv: "prod", flag: "dev", wantEnv: "dev", wantSrc: "explicit"},
		{name: "positional beats all", use: "staging", tmvpEnv: "prod", positional: "dev,prod", wantEnv: "dev,prod", wantSrc: "explicit"},
		{name: "positional == flag", positional: "prod", flag: "prod", wantEnv: "prod", wantSrc: "explicit"},
		{name: "positional != flag", positional: "prod", flag: "dev", wantErr: true},
		{name: "batch glob", positional: "*", wantEnv: "*", wantSrc: "explicit"},
		{name: "no default_env → first env", noDefault: true, wantEnv: "dev", wantSrc: "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = envsel.Clear(cfg)
			if tc.use != "" {
				if _, err := envsel.Use(cfg, tc.use); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("TMVP_ENV", tc.tmvpEnv)
			globals.Env = tc.flag
			defer func() { globals.Env = "" }()
			c := *cfg
			if tc.noDefault {
				c.DefaultEnv = ""
			}
			sel, err := selectEnv(&c, tc.positional, tc.positional != "")
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil || sel.Env != tc.wantEnv || sel.Source != tc.wantSrc {
				t.Fatalf("got %+v %v", sel, err)
			}
		})
	}
}

func TestUseCommandAndProtectedWrites(t *testing.T) {
	out := envSetup(t, envCfg)
	if c := run(t, out, "use"); c != 0 || !strings.Contains(out.String(), "env dev") || !strings.Contains(out.String(), "source: default") {
		t.Fatalf("use: %d %s", c, out)
	}
	// owner decision: switching to a protected env needs NO --force
	if c := run(t, out, "use", "prod"); c != 0 || !strings.Contains(out.String(), "env prod [protected") {
		t.Fatalf("use prod: %d %s", c, out)
	}
	if st, err := os.Stat(envsel.StatePath()); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Fatalf("state file: %v %v", st, err)
	}
	if c := run(t, out, "use", "nope"); c != 2 {
		t.Fatalf("unknown env: %d %s", c, out)
	}
	// write to prod (from use) without --force → policy_blocked (3), never connects
	if c := run(t, out, "write", "db", "update x set y=1 where id=1", "--reason", "test"); c != 3 || !strings.Contains(out.String(), "--force") {
		t.Fatalf("write without force: %d %s", c, out)
	}
	// named prod: same rule
	if c := run(t, out, "write", "prod", "db", "update x set y=1 where id=1", "--reason", "test"); c != 3 {
		t.Fatalf("named prod without force: %d %s", c, out)
	}
	// with --force it passes the policy and reaches the (unreachable) system: exit 1
	if c := run(t, out, "write", "db", "update x set y=1 where id=1", "--reason", "test", "--force"); c != 1 || strings.Contains(out.String(), "protected") {
		t.Fatalf("write with force: %d %s", c, out)
	}
	if !strings.Contains(out.String(), "# env: prod (source: use") {
		t.Errorf("header missing: %s", out)
	}
	// dev is destructive: no --force needed
	if c := run(t, out, "use", "--clear"); c != 0 || !strings.Contains(out.String(), "source: default") {
		t.Fatalf("clear: %d %s", c, out)
	}
	if c := run(t, out, "write", "db", "update x set y=1 where id=1", "--reason", "test"); c != 1 || strings.Contains(out.String(), "protected") {
		t.Fatalf("dev write: %d %s", c, out)
	}
}

func TestJSONLCarriesEnvAndSource(t *testing.T) {
	out := envSetup(t, envCfg)
	t.Setenv("TMVP_ENV", "staging,prod")
	run(t, out, "db", "ls", "--json")
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	envs := map[string]bool{}
	for _, l := range lines {
		var f protocol.Frame
		if err := json.Unmarshal([]byte(l), &f); err != nil {
			t.Fatalf("bad line %q", l)
		}
		if f.Type == protocol.FrameDone {
			continue
		}
		if f.EnvSource != "TMVP_ENV" {
			t.Fatalf("frame without env_source: %s", l)
		}
		envs[f.Env] = true
	}
	if !envs["staging"] || !envs["prod"] || envs["dev"] {
		t.Fatalf("envs %v\n%s", envs, out)
	}
}

func certPair(t *testing.T, notAfter time.Time) (string, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "fake.test"},
		NotBefore: notAfter.Add(-400 * 24 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

func TestSecretsStatusCertsNoValues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses echo/cp")
	}
	dir := t.TempDir()
	now := time.Now()
	vc, vk := certPair(t, now.Add(45*24*time.Hour+time.Hour))
	ec, _ := certPair(t, now.Add(-2*24*time.Hour-time.Hour))
	valid, expired := filepath.Join(dir, "valid.pem"), filepath.Join(dir, "expired.pem")
	_ = os.WriteFile(valid, []byte(vc+vk), 0o600)
	_ = os.WriteFile(expired, []byte(ec), 0o600)
	cfg := fmt.Sprintf(`product: sectest
logging: { path: %s }
secrets:
  pw: "$(echo FAKE_SECRET_123)"
  tls_ok: "$file(cp %s ${out})"
  tls_old: "$file(cp %s ${out})"
  gone: "$(false)"
envs:
  dev:
    destructive: true
    systems:
      db: { type: postgres, read: { dsn: "postgres://u:${secrets.pw}@127.0.0.1:1/x?sslmode=disable" } }
`, filepath.Join(dir, "logs"), valid, expired)
	out := envSetup(t, cfg)
	for _, js := range []bool{false, true} {
		args := []string{"secrets", "status"}
		if js {
			args = append(args, "--json")
		}
		c := run(t, out, args...)
		s := out.String()
		if c != 2 {
			t.Fatalf("exit %d (gone is missing → 2)\n%s", c, s)
		}
		for _, bad := range []string{"FAKE_SECRET_123", "BEGIN", "PRIVATE KEY-----"} {
			if strings.Contains(s, bad) {
				t.Fatalf("leaked %q:\n%s", bad, s)
			}
		}
		want := []string{"expired: -2 days", "ok: 45 days", "private key: present", "gone", "missing", "echo"}
		if js {
			want = []string{`"cert_days_left":45`, `"cert_days_left":-2`, `"cert_status":"expired"`, `"private_key":true`, `"state":"missing"`, `"source":"echo"`}
		}
		for _, w := range want {
			if !strings.Contains(s, w) {
				t.Errorf("json=%v missing %q in\n%s", js, w, s)
			}
		}
	}
	if c := run(t, out, "secrets", "audit"); c != 0 || !strings.Contains(out.String(), "fetch") || !strings.Contains(out.String(), "failure") ||
		!strings.Contains(out.String(), "secrets.status") || strings.Contains(out.String(), "FAKE_SECRET_123") {
		t.Fatalf("audit: %d %s", c, out)
	}
	// certs also lists secret-held certs; expired → exit 1
	if c := run(t, out, "certs"); c != 1 || !strings.Contains(out.String(), "expired") || strings.Contains(out.String(), "BEGIN") {
		t.Fatalf("certs: %d %s", c, out)
	}
}

func TestCertsAgainstTLSServer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	out := envSetup(t, fmt.Sprintf("product: tlstest\nenvs:\n  dev:\n    systems:\n      api: { type: http, base_url: %q }\n", srv.URL))
	if c := run(t, out, "certs", "--days", "30"); c != 0 {
		t.Fatalf("exit %d\n%s", c, out)
	}
	host := strings.TrimPrefix(srv.URL, "https://")
	if s := out.String(); !strings.Contains(s, "dev.api") || !strings.Contains(s, host) || !strings.Contains(s, "trusted: no") {
		t.Fatalf("%s", s)
	}
	// a window larger than the cert's life → expiring warning, still exit 0
	if c := run(t, out, "certs", "--days", "1000000", "--json"); c != 0 || !strings.Contains(out.String(), `"status":"expiring"`) {
		t.Fatalf("%d %s", c, out)
	}
}
