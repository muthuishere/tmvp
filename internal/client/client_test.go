package client

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

func TestInProcessPing(t *testing.T) {
	t.Setenv("TMVP_NO_SERVER", "1")
	var got []protocol.Frame
	code, err := Do(context.Background(), protocol.Request{Op: protocol.OpPing}, func(f protocol.Frame) { got = append(got, f) })
	if err != nil || code != 0 || len(got) != 1 || got[0].Info["proto"] != float64(protocol.Version) {
		t.Fatalf("%d %v %+v", code, err, got)
	}
	code, _ = Do(context.Background(), protocol.Request{Op: protocol.OpRun}, func(protocol.Frame) {})
	if code != 2 {
		t.Fatalf("no config → exit 2, got %d", code)
	}
}

func TestDialNoAutostart(t *testing.T) {
	t.Setenv("TMVP_SOCKET", t.TempDir()+"/none.sock")
	if _, err := Dial(context.Background(), false); err != ErrNotRunning {
		t.Fatal(err)
	}
}

func TestLongSocketPathIsConfigError(t *testing.T) {
	t.Setenv("TMVP_SOCKET", "/"+strings.Repeat("x", 120)+".sock")
	_, err := Dial(context.Background(), true)
	if errs.CodeOf(err) != errs.Config && runtime.GOOS != "windows" {
		t.Fatalf("got %v", err)
	}
}
