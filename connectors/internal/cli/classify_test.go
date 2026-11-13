package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/errs"
)

// A CLI killed by the context deadline surfaces from exec as an *ExitError
// ("signal: killed"); Classify must still call it a timeout.
func TestClassifyDeadlineKill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := helper(ctx, "sleep").Run()
	if err == nil {
		t.Fatal("sleep should have been killed")
	}
	if got := errs.From(Classify(ctx, err, "", 50*time.Millisecond)); got == nil || got.Code != errs.Timeout {
		t.Fatalf("want timeout, got %v", got)
	}
	// Same kill error but classified with a fresh ctx and the deadline only in the chain.
	wrapped := errors.Join(err, context.DeadlineExceeded)
	if got := errs.From(Classify(context.Background(), wrapped, "", 0)); got == nil || got.Code != errs.Timeout {
		t.Fatalf("want timeout from chain, got %v", got)
	}
}

func TestClassifyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := helper(ctx, "sleep")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	runErr := cmd.Wait()
	got := Classify(ctx, runErr, "", time.Second)
	if e := errs.From(got); e == nil || e.Code == errs.Timeout {
		t.Fatalf("cancel should not be timeout: %v", got)
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("want context.Canceled in chain, got %v", got)
	}
}

func TestClassifyExitStaysRuntime(t *testing.T) {
	err := helper(context.Background(), "exit3").Run()
	if e := errs.From(Classify(context.Background(), err, "boom", time.Second)); e == nil || e.Code != errs.Runtime {
		t.Fatalf("want runtime, got %v", e)
	}
}

// helper re-runs this test binary as a portable stand-in for sleep / sh -c
// "exit 3" (neither exists on Windows).
func helper(ctx context.Context, mode string) *exec.Cmd {
	c := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcess$")
	c.Env = append(os.Environ(), "TMVP_CLI_HELPER="+mode)
	return c
}

func TestHelperProcess(t *testing.T) {
	switch os.Getenv("TMVP_CLI_HELPER") {
	case "sleep":
		time.Sleep(5 * time.Second)
		os.Exit(0)
	case "exit3":
		os.Exit(3)
	}
}
