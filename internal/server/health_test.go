package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muthuishere/tmvp/internal/audit"
	"github.com/muthuishere/tmvp/internal/protocol"
)

func TestForceRuleOnProtectedEnvWrites(t *testing.T) {
	e := newEngine(newSecrets())
	p := writeCfg(t, "alpha")
	cases := []struct {
		name string
		req  protocol.Request
		exit int
	}{
		{"prod named, no force", protocol.Request{Env: "prod", System: "plain", Verb: "mut", EnvSource: protocol.EnvExplicit}, 3},
		{"prod from use, no force", protocol.Request{Env: "prod", System: "plain", Verb: "mut", EnvSource: protocol.EnvFromUse}, 3},
		{"prod from default, no force", protocol.Request{Env: "prod", System: "plain", Verb: "mut", EnvSource: protocol.EnvDefault}, 3},
		{"prod from TMVP_ENV, no force", protocol.Request{Env: "prod", System: "plain", Verb: "mut", EnvSource: protocol.EnvFromVar}, 3},
		{"prod from use, force", protocol.Request{Env: "prod", System: "plain", Verb: "mut", EnvSource: protocol.EnvFromUse, Force: true}, 0},
		{"prod preview needs no force", protocol.Request{Env: "prod", System: "plain", Verb: "mut", EnvSource: protocol.EnvFromUse, Preview: true}, 0},
		{"dev (destructive) from use", protocol.Request{Env: "dev", System: "db", Verb: "mut", EnvSource: protocol.EnvFromUse}, 0},
		{"still single target", protocol.Request{Env: "dev,prod", System: "db", Verb: "mut", Force: true}, 3},
	}
	for _, tc := range cases {
		tc.req.Op, tc.req.ConfigPath, tc.req.Reason = protocol.OpWrite, p, "r"
		r := call(t, e, tc.req)
		if r.exit != tc.exit {
			t.Errorf("%s: exit %d want %d: %s", tc.name, r.exit, tc.exit, r.raw)
		}
		if tc.exit == 3 && tc.req.Env == "prod" && !strings.Contains(r.raw, "--force") {
			t.Errorf("%s: error should name --force: %s", tc.name, r.raw)
		}
	}
}

func TestFramesAndAuditCarryEnvSource(t *testing.T) {
	e := newEngine(newSecrets())
	p := writeCfg(t, "alpha")
	r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev,prod", System: "db", Verb: "echo", EnvSource: protocol.EnvFromVar})
	rows := r.of(protocol.FrameRow)
	if r.exit != 0 || len(rows) != 2 {
		t.Fatalf("%s", r.raw)
	}
	for _, f := range r.frames {
		if f.EnvSource != protocol.EnvFromVar {
			t.Fatalf("frame without env_source: %+v", f)
		}
	}
	for _, f := range rows {
		if f.Env != "dev" && f.Env != "prod" {
			t.Fatalf("row env %q", f.Env)
		}
	}
	recs, _ := audit.Read(filepath.Join(filepath.Dir(p), "logs"), audit.ReadOptions{})
	n := 0
	for _, rc := range recs {
		if rc.Body == "command" && rc.Attributes["env_source"] == protocol.EnvFromVar {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("audit records with env_source: %d", n)
	}
}

func TestSecretsStatusUpdatedAndAudit(t *testing.T) {
	s := newSecrets()
	e := newEngine(s)
	p := writeCfg(t, "alpha")
	status := func() map[string]map[string]any {
		r := call(t, e, protocol.Request{Op: protocol.OpSecretsStatus, ConfigPath: p})
		if strings.Contains(r.raw, "FAKE_DEV_DSN") || strings.Contains(r.raw, "FAKE_PROD_DSN") {
			t.Fatalf("value leaked: %s", r.raw)
		}
		out := map[string]map[string]any{}
		for _, f := range r.of(protocol.FrameRow) {
			out[f.Row["secret"].(string)] = f.Row
		}
		return out
	}
	st := status()
	if st["dev.dsn"]["state"] != "ok" || st["dev.dsn"]["updated"] != "unknown" || st["dev.dsn"]["source"] != "get" {
		t.Fatalf("%+v", st["dev.dsn"])
	}
	// reload with the same value → updated: no; rotated → yes
	call(t, e, protocol.Request{Op: protocol.OpSecretsReload, ConfigPath: p})
	if st = status(); st["dev.dsn"]["updated"] != "no" {
		t.Fatalf("same value: %+v", st["dev.dsn"])
	}
	s.set("get dev", "FAKE_DEV_DSN_ROTATED_789")
	call(t, e, protocol.Request{Op: protocol.OpSecretsReload, ConfigPath: p})
	if st = status(); st["dev.dsn"]["updated"] != "yes" {
		t.Fatalf("rotated: %+v", st["dev.dsn"])
	}
	// prod scope filter + missing state
	delete(s.m, "get prod")
	call(t, e, protocol.Request{Op: protocol.OpSecretsClear, ConfigPath: p})
	r := call(t, e, protocol.Request{Op: protocol.OpSecretsStatus, ConfigPath: p, Env: "prod"})
	rows := r.of(protocol.FrameRow)
	if len(rows) != 1 || rows[0].Row["state"] != "missing" || r.exit != 2 {
		t.Fatalf("prod: %s", r.raw)
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "logs", audit.FileName))
	for _, w := range []string{`"event":"fetch"`, `"event":"refresh"`, `"event":"failure"`, `"trigger":"secrets.status prod"`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("audit missing %s", w)
		}
	}
	if strings.Contains(string(b), "FAKE_DEV_DSN") {
		t.Fatal("audit leaked a value")
	}
}

func TestCertsGenericURLField(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf("product: certs\nenvs:\n  dev:\n    systems:\n      api: { type: fake, url: %q, dsn: x }\n      plain: { type: fake, url: \"http://127.0.0.1:1\", dsn: x }\n", srv.URL)
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	reps, err := NewEngine().Certs(context.Background(), p, CertsOptions{WarnDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 1 || reps[0].System != "api" || reps[0].Cert == nil || reps[0].Status == "error" {
		t.Fatalf("%+v", reps)
	}
	if !strings.HasPrefix(reps[0].Trusted, "no") {
		t.Errorf("httptest cert must be untrusted: %q", reps[0].Trusted)
	}
}
