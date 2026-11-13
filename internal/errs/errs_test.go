package errs

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestFromAndIs(t *testing.T) {
	plain := errors.New("boom")
	if From(plain) != nil || Is(plain) || Is(plain, Runtime) {
		t.Fatal("a plain error must not count as a tmvp error")
	}
	if As(plain) == nil || As(plain).Code != Runtime {
		t.Fatal("As still converts plain errors to runtime")
	}
	if As(context.DeadlineExceeded).Code != Timeout {
		t.Fatal("deadline → timeout")
	}
	if From(nil) != nil || Is(nil) || As(nil) != nil {
		t.Fatal("nil stays nil")
	}
	te := New(Auth, "rejected")
	wrapped := fmt.Errorf("ctx: %w", te)
	if From(wrapped) != te || !Is(wrapped) || !Is(wrapped, Config, Auth) || Is(wrapped, Config) {
		t.Fatal("From/Is must find a wrapped tmvp error and match its code")
	}
	if !IsAuth(wrapped) || CodeOf(wrapped) != Auth {
		t.Fatal("IsAuth/CodeOf")
	}
}
