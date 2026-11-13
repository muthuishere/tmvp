// Package certinfo extracts certificate metadata (NotAfter, issuer, SANs) and
// private-key presence from secret bytes or a TLS peer — never key material.
//
// Understood shapes: PEM (any number of blocks), base64 of PEM, kubeconfig
// YAML (client-certificate-data, certificate-authority-data,
// client-key-data, embedded PEM strings), and JSON documents such as GCP
// service-account files (private_key) — any string value holding PEM or
// base64 PEM is inspected.
package certinfo

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Cert is one certificate's public metadata.
type Cert struct {
	Subject  string    `json:"subject"`
	Issuer   string    `json:"issuer"`
	NotAfter time.Time `json:"not_after"`
	DaysLeft int       `json:"days_left"`
	SANs     []string  `json:"sans,omitempty"`
	IsCA     bool      `json:"is_ca,omitempty"`
	Expired  bool      `json:"expired,omitempty"`
}

// Info is what a blob contains.
type Info struct {
	Certs      []Cert `json:"certs,omitempty"`
	PrivateKey bool   `json:"private_key"`
	Kind       string `json:"kind,omitempty"` // pem | kubeconfig | json | ""
}

// Earliest returns the certificate expiring first (ok=false when none).
func (i Info) Earliest() (Cert, bool) {
	if len(i.Certs) == 0 {
		return Cert{}, false
	}
	c := i.Certs[0]
	for _, x := range i.Certs[1:] {
		if x.NotAfter.Before(c.NotAfter) {
			c = x
		}
	}
	return c, true
}

// Status is ok | expiring | expired for a cert given the warning window.
func Status(c Cert, warnDays int) string {
	switch {
	case c.Expired || c.DaysLeft < 0:
		return "expired"
	case c.DaysLeft < warnDays:
		return "expiring"
	}
	return "ok"
}

// DaysLeft is whole days until t, truncated toward zero (-3 = expired 3+
// days ago; 0 can be either side: use Cert.Expired).
func DaysLeft(t, now time.Time) int {
	return int(math.Trunc(t.Sub(now).Hours() / 24))
}

// FromX509 converts a parsed certificate.
func FromX509(c *x509.Certificate, now time.Time) Cert {
	out := Cert{Subject: name(c.Subject.CommonName, c.Subject.String()), Issuer: name(c.Issuer.CommonName, c.Issuer.String()),
		NotAfter: c.NotAfter.UTC(), DaysLeft: DaysLeft(c.NotAfter, now), IsCA: c.IsCA,
		Expired: now.After(c.NotAfter)}
	out.SANs = append(out.SANs, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		out.SANs = append(out.SANs, ip.String())
	}
	return out
}

func name(cn, full string) string {
	if cn != "" {
		return cn
	}
	return full
}

// Inspect looks for certificates and private keys in b.
func Inspect(b []byte, now time.Time) Info {
	var in Info
	seen := map[string]bool{}
	var scan func(blob []byte, depth int)
	scan = func(blob []byte, depth int) {
		if depth > 3 || len(blob) == 0 {
			return
		}
		if bytes.Contains(blob, []byte("-----BEGIN")) {
			if in.Kind == "" {
				in.Kind = "pem"
			}
			rest := blob
			for {
				var blk *pem.Block
				blk, rest = pem.Decode(rest)
				if blk == nil {
					break
				}
				switch {
				case blk.Type == "CERTIFICATE":
					if seen[string(blk.Bytes)] {
						continue
					}
					seen[string(blk.Bytes)] = true
					if c, err := x509.ParseCertificate(blk.Bytes); err == nil {
						in.Certs = append(in.Certs, FromX509(c, now))
					}
				case strings.Contains(blk.Type, "PRIVATE KEY"):
					in.PrivateKey = true
				}
			}
			// PEM may also sit inside a JSON/YAML string with literal \n
			if !bytes.Contains(blob, []byte("\n-----END")) && bytes.Contains(blob, []byte(`\n`)) {
				scan(bytes.ReplaceAll(blob, []byte(`\n`), []byte("\n")), depth+1)
			}
		}
		t := bytes.TrimSpace(blob)
		if len(t) == 0 {
			return
		}
		// structured documents: walk every string value
		var doc any
		if t[0] == '{' || t[0] == '[' {
			if json.Unmarshal(t, &doc) == nil {
				if in.Kind == "" || in.Kind == "pem" {
					in.Kind = "json"
				}
				walk(doc, func(k, v string) { scanValue(k, v, depth, scan) })
				return
			}
		}
		if bytes.Contains(t, []byte("apiVersion")) || bytes.Contains(t, []byte("-data:")) {
			if yaml.Unmarshal(t, &doc) == nil {
				if _, ok := doc.(map[string]any); ok {
					in.Kind = "kubeconfig"
					walk(doc, func(k, v string) { scanValue(k, v, depth, scan) })
					return
				}
			}
		}
		// whole blob base64 of PEM
		if !bytes.Contains(t, []byte("-----BEGIN")) {
			if d, ok := b64(string(t)); ok && bytes.Contains(d, []byte("-----BEGIN")) {
				scan(d, depth+1)
			}
		}
	}
	scan(b, 0)
	sort.SliceStable(in.Certs, func(i, j int) bool { return in.Certs[i].NotAfter.Before(in.Certs[j].NotAfter) })
	return in
}

func scanValue(k, v string, depth int, scan func([]byte, int)) {
	switch {
	case strings.Contains(v, "-----BEGIN"):
		scan([]byte(v), depth+1)
	case strings.HasSuffix(k, "-data") || strings.HasSuffix(k, "_data") || len(v) > 64:
		if d, ok := b64(v); ok && bytes.Contains(d, []byte("-----BEGIN")) {
			scan(d, depth+1)
		}
	}
}

func b64(s string) ([]byte, bool) {
	s = strings.Join(strings.Fields(s), "")
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding} {
		if d, err := enc.DecodeString(s); err == nil {
			return d, true
		}
	}
	return nil, false
}

func walk(x any, f func(k, v string)) {
	var rec func(k string, x any)
	rec = func(k string, x any) {
		switch t := x.(type) {
		case string:
			f(k, t)
		case map[string]any:
			for kk, v := range t {
				rec(kk, v)
			}
		case []any:
			for _, v := range t {
				rec(k, v)
			}
		}
	}
	rec("", x)
}
