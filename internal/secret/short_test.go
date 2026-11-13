package secret

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/muthuishere/tmvp/internal/errs"
)

func TestShortSecretRejected(t *testing.T) {
	red := NewRedactor()
	s := NewStore("demo", nil, red)
	d := Def{Name: "pw", Key: "pw", Fetch: func(context.Context) ([]byte, error) { return []byte("abc\n"), nil }}
	_, err := s.Get(context.Background(), d)
	if errs.CodeOf(err) != errs.SecretInvalid || !strings.Contains(errs.As(err).Hint, "too short") {
		t.Fatalf("want secret_invalid with hint, got %v", err)
	}
	ok := Def{Name: "ok", Key: "ok", Fetch: func(context.Context) ([]byte, error) { return []byte("FAKE_SECRET_123"), nil }}
	if v, err := s.Get(context.Background(), ok); err != nil || v != "FAKE_SECRET_123" {
		t.Fatalf("got %q %v", v, err)
	}
}

func TestAddBasicAuth(t *testing.T) {
	red := NewRedactor()
	red.AddBasicAuth("alice", "FAKE_SECRET_123")
	hdr := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("alice:FAKE_SECRET_123"))
	out := red.Redact(hdr + " pw=FAKE_SECRET_123 user=alice")
	if strings.Contains(out, "FAKE_SECRET_123") || strings.Contains(out, "YWxpY2U6") || !strings.Contains(out, "user=alice") {
		t.Fatalf("not redacted correctly: %s", out)
	}
	var nilR *Redactor
	nilR.AddBasicAuth("a", "b") // must not panic
}
