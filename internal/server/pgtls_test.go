package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakePG answers the SSLRequest with 'S' (then TLS) or 'N'.
func fakePG(t *testing.T, offerTLS bool) string {
	t.Helper()
	hs := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(hs.Close)
	tcfg := &tls.Config{Certificates: hs.TLS.Certificates}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, err := io.ReadFull(c, make([]byte, 8)); err != nil {
					return
				}
				if !offerTLS {
					_, _ = c.Write([]byte{'N'})
					return
				}
				_, _ = c.Write([]byte{'S'})
				tc := tls.Server(c, tcfg)
				_ = tc.Handshake()
				tc.Close()
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestPgEndpointModes(t *testing.T) {
	for dsn, want := range map[string]struct{ ok, opt bool }{
		"postgres://u@h/db":                 {true, true},
		"postgres://u@h/db?sslmode=prefer":  {true, true},
		"postgres://u@h/db?sslmode=allow":   {true, true},
		"postgres://u@h/db?sslmode=disable": {false, false},
		"postgres://u@h/db?sslmode=require": {true, false},
		"host=h sslmode=verify-full":        {true, false},
		"host=/var/run/postgresql":          {false, false},
	} {
		ep, ok, err := pgEndpoint(dsn)
		if err != nil || ok != want.ok || ep.optional != want.opt {
			t.Errorf("%s: ok=%v opt=%v err=%v", dsn, ok, ep.optional, err)
		}
	}
}

func TestCertsPostgresPreferProbesTLS(t *testing.T) {
	tlsAddr, plainAddr := fakePG(t, true), fakePG(t, false)
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf("product: pgc\nenvs:\n  dev:\n    systems:\n"+
		"      tlsdb: { type: postgres, dsn: \"postgres://u@%s/db\" }\n"+
		"      plaindb: { type: postgres, dsn: \"postgres://u@%s/db?sslmode=prefer\" }\n"+
		"      reqdb: { type: postgres, dsn: \"postgres://u@%s/db?sslmode=require\" }\n"+
		"      offdb: { type: postgres, dsn: \"postgres://u@%s/db?sslmode=disable\" }\n",
		tlsAddr, plainAddr, plainAddr, tlsAddr)
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	reps, err := NewEngine().Certs(context.Background(), p, CertsOptions{WarnDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]CertReport{}
	for _, r := range reps {
		got[r.System] = r
	}
	if r := got["tlsdb"]; r.Cert == nil || r.Status == "error" || r.Status == "no_tls" {
		t.Errorf("unset sslmode should check the cert: %+v", r)
	}
	if r := got["plaindb"]; r.Status != "no_tls" || r.Error != nil {
		t.Errorf("prefer + no TLS should be no_tls, not an error: %+v", r)
	}
	if r := got["reqdb"]; r.Status != "error" {
		t.Errorf("require + no TLS should be an error: %+v", r)
	}
	if _, ok := got["offdb"]; ok {
		t.Errorf("sslmode=disable must be skipped")
	}
}
