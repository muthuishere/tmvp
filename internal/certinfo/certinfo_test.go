package certinfo

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func selfSigned(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake.test"},
		NotBefore: notAfter.Add(-365 * 24 * time.Hour), NotAfter: notAfter, DNSNames: []string{"fake.test"}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func TestInspectShapes(t *testing.T) {
	now := time.Now()
	c, k := selfSigned(t, now.Add(10*24*time.Hour+time.Hour))
	old, _ := selfSigned(t, now.Add(-3*24*time.Hour))
	b64 := base64.StdEncoding.EncodeToString
	kube := "apiVersion: v1\nkind: Config\nusers:\n- name: u\n  user:\n    client-certificate-data: " + b64(c) +
		"\n    client-key-data: " + b64(k) + "\nclusters:\n- name: c\n  cluster:\n    certificate-authority-data: " + b64(old) + "\n"
	sa := `{"type":"service_account","private_key":"` + strings.ReplaceAll(string(k), "\n", `\n`) + `"}`
	cases := []struct {
		name  string
		in    []byte
		certs int
		key   bool
		days  int
		kind  string
	}{
		{"pem", append(append([]byte{}, c...), k...), 1, true, 10, "pem"},
		{"b64", []byte(b64(c)), 1, false, 10, "pem"},
		{"kubeconfig", []byte(kube), 2, true, -3, "kubeconfig"},
		{"sa json", []byte(sa), 0, true, 0, "json"},
		{"plain", []byte("FAKE_SECRET_123"), 0, false, 0, ""},
	}
	for _, tc := range cases {
		in := Inspect(tc.in, now)
		if len(in.Certs) != tc.certs || in.PrivateKey != tc.key || in.Kind != tc.kind {
			t.Errorf("%s: %+v", tc.name, in)
			continue
		}
		if e, ok := in.Earliest(); ok && e.DaysLeft != tc.days {
			t.Errorf("%s: days %d want %d", tc.name, e.DaysLeft, tc.days)
		}
	}
	if Status(Cert{DaysLeft: -1}, 30) != "expired" || Status(Cert{DaysLeft: 5}, 30) != "expiring" || Status(Cert{DaysLeft: 90}, 30) != "ok" {
		t.Error("status")
	}
}
