package batch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

type rec struct {
	mu     sync.Mutex
	frames []protocol.Frame
}

func (r *rec) sink(f protocol.Frame) { r.mu.Lock(); r.frames = append(r.frames, f); r.mu.Unlock() }
func (r *rec) of(typ string) []protocol.Frame {
	var out []protocol.Frame
	for _, f := range r.frames {
		if f.Type == typ {
			out = append(out, f)
		}
	}
	return out
}

func TestParallelPartialAndTags(t *testing.T) {
	units := []Unit{{Env: "dev", System: "db"}, {Env: "staging", System: "db"}, {Env: "prod", System: "db"}, {Env: "qa", System: "db", Skip: "no db"}}
	var r rec
	start := time.Now()
	res := Run(context.Background(), units, func(ctx context.Context, u Unit, e connector.Emit) error {
		time.Sleep(100 * time.Millisecond)
		if u.Env == "staging" {
			return errs.New(errs.Unreachable, "down")
		}
		return e.Row(connector.Row{"n": 1})
	}, Options{}, r.sink)
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("not parallel")
	}
	if res.OK != 2 || res.Failed != 1 || res.Skipped != 1 || res.Exit != 4 {
		t.Fatalf("%+v", res)
	}
	for _, f := range r.of(protocol.FrameRow) {
		if f.Row["env"] != f.Env || f.Row["system"] != "db" {
			t.Fatalf("row not tagged: %+v", f)
		}
	}
	if len(r.of(protocol.FrameError)) != 1 {
		t.Fatal("expected 1 error frame")
	}
}

func TestAllFailedUsesCode(t *testing.T) {
	var r rec
	res := Run(context.Background(), []Unit{{Env: "a", System: "s"}}, func(context.Context, Unit, connector.Emit) error {
		return errs.New(errs.SecretMissing, "x")
	}, Options{}, r.sink)
	if res.Exit != 2 {
		t.Fatal(res.Exit)
	}
}

func TestWriteRefusesMultiTarget(t *testing.T) {
	var r rec
	ran := false
	res := Run(context.Background(), []Unit{{Env: "a"}, {Env: "b"}}, func(context.Context, Unit, connector.Emit) error { ran = true; return nil }, Options{Write: true}, r.sink)
	if ran || res.Exit != 3 {
		t.Fatalf("ran=%v exit=%d", ran, res.Exit)
	}
}

func TestCapsAndTimeout(t *testing.T) {
	var r rec
	res := Run(context.Background(), []Unit{{Env: "a", System: "s"}, {Env: "b", System: "s"}}, func(ctx context.Context, u Unit, e connector.Emit) error {
		if u.Env == "b" {
			<-ctx.Done()
			return ctx.Err()
		}
		for i := 0; ; i++ {
			if err := e.Row(connector.Row{"i": i}); errors.Is(err, connector.ErrLimit) {
				return nil
			}
		}
	}, Options{Rows: 5, Timeout: 100 * time.Millisecond}, r.sink)
	if n := len(r.of(protocol.FrameRow)); n != 5 {
		t.Fatalf("rows %d", n)
	}
	if res.Exit != 4 || errs.CodeOf(res.Errors[0]) != errs.Timeout {
		t.Fatalf("%+v %v", res, res.Errors)
	}
}

func TestDiff(t *testing.T) {
	var r rec
	Run(context.Background(), []Unit{{Env: "dev", System: "db"}, {Env: "prod", System: "db"}}, func(ctx context.Context, u Unit, e connector.Emit) error {
		_ = e.Row(connector.Row{"k": "same"})
		_ = e.Row(connector.Row{"k": "only-" + u.Env})
		return nil
	}, Options{Diff: true}, r.sink)
	rows := r.of(protocol.FrameRow)
	if len(rows) != 2 {
		t.Fatalf("%+v", rows)
	}
	for _, f := range rows {
		if f.Row["k"] == "same" {
			t.Fatal("identical row shown in diff")
		}
	}
}
