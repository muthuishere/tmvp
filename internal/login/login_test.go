package login

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/errs"
)

func TestEnsure(t *testing.T) {
	c := New()
	calls := 0
	ok := false
	c.Run = func(context.Context, string, string) error {
		calls++
		if ok {
			return nil
		}
		return errors.New("not logged in")
	}
	now := time.Now()
	c.now = func() time.Time { return now }
	s := &Spec{Manual: "gcloud auth login", Check: "gcloud auth print-access-token"}
	err := c.Ensure(context.Background(), "p/prod/cloud", s, "", "prod", "cloud")
	if errs.CodeOf(err) != errs.LoginRequired || errs.ExitCode(errs.CodeOf(err)) != 2 || !strings.Contains(errs.As(err).Hint, "gcloud auth login") {
		t.Fatalf("%v", err)
	}
	ok = true
	for i := 0; i < 3; i++ {
		if err := c.Ensure(context.Background(), "p/prod/cloud", s, "", "prod", "cloud"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("cache not used: %d", calls)
	}
	now = now.Add(TTL + time.Second)
	_ = c.Ensure(context.Background(), "p/prod/cloud", s, "", "prod", "cloud")
	if calls != 3 {
		t.Fatal("cache not expired")
	}
	if c.Ensure(context.Background(), "k", nil, "", "", "") != nil {
		t.Fatal("no spec → nil")
	}
}
