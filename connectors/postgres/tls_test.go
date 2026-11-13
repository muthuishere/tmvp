package postgres

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
)

// fakeTLSConn stands in for the *tls.Conn pgx hands back when the server
// negotiated TLS (as it may under sslmode=prefer/allow or the default).
type fakeTLSConn struct {
	net.Conn
	st tls.ConnectionState
}

func (f fakeTLSConn) ConnectionState() tls.ConnectionState { return f.st }

func fakeCert(t *testing.T, notAfter time.Time) *x509.Certificate {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "db.fake.test"},
		NotBefore: notAfter.Add(-365 * 24 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAddTLSCert(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plainA, plainB := net.Pipe()
	defer plainA.Close()
	defer plainB.Close()

	row := connector.Row{}
	addTLSCert(row, plainA, now) // sslmode=disable / prefer fell back to plain
	if row["tls"] != false || row["cert_status"] != nil {
		t.Fatalf("plain conn: %v", row)
	}

	for _, tc := range []struct {
		after time.Time
		want  string
	}{{now.Add(-48 * time.Hour), "expired"}, {now.Add(10 * 24 * time.Hour), "expiring"}, {now.Add(200 * 24 * time.Hour), "ok"}} {
		row = connector.Row{}
		addTLSCert(row, fakeTLSConn{Conn: plainA, st: tls.ConnectionState{HandshakeComplete: true,
			PeerCertificates: []*x509.Certificate{fakeCert(t, tc.after)}}}, now)
		if row["tls"] != true || row["cert_status"] != tc.want || row["cert_subject"] == nil {
			t.Errorf("notAfter %s: want %s, got %v", tc.after, tc.want, row)
		}
	}

	row = connector.Row{}
	addTLSCert(row, fakeTLSConn{Conn: plainA}, now) // handshake not done
	if row["tls"] != false || row["cert_status"] != nil {
		t.Fatalf("incomplete handshake: %v", row)
	}
}
