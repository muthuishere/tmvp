package az

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/connectors/internal/cli/clitest"
	"github.com/muthuishere/tmvp/internal/connector"
)

func TestLogEstimate(t *testing.T) {
	for _, tc := range []struct {
		rng      time.Duration
		filtered bool
		want     time.Duration
		over10s  bool
	}{
		{30 * time.Minute, false, 30 * time.Second, true},
		{time.Hour, false, time.Minute, true},
		{2 * time.Hour, false, 2 * time.Minute, true},
		{24 * time.Hour, false, 24 * time.Minute, true},
		{time.Hour, true, 250 * time.Millisecond, false},
		{24 * time.Hour, true, 6 * time.Second, false},
		{7 * 24 * time.Hour, true, 42 * time.Second, true},
	} {
		e := logEstimate(tc.rng, tc.filtered, "Log Analytics logs")
		if e.Duration != tc.want || (e.Duration > 10*time.Second) != tc.over10s || e.Confident || e.Why == "" {
			t.Errorf("%s filtered=%v: %+v, want %s", tc.rng, tc.filtered, e, tc.want)
		}
	}
}

func TestEstimateVerbs(t *testing.T) {
	clitest.Fake(t, "az", "echo '{}'")
	x := open(t, connector.StaticFields{})
	ctx := context.Background()
	if _, err := x.Estimate(ctx, connector.Call{Verb: "whoami"}); !errors.Is(err, connector.ErrNoEstimate) {
		t.Fatalf("whoami: %v", err)
	}
	e, err := x.Estimate(ctx, connector.Call{Verb: "logs", Args: []string{"FAKE_SCOPE"}, Since: 24 * time.Hour})
	if err != nil || e.Duration != 24*time.Minute {
		t.Fatalf("unfiltered 24h: %+v %v", e, err)
	}
	e, err = x.Estimate(ctx, connector.Call{Verb: "logs", Args: []string{"AppTraces | where SeverityLevel > 2"}, Since: 4 * time.Hour})
	if err != nil || e.Duration != time.Second {
		t.Fatalf("filtered 4h: %+v %v", e, err)
	}
}

func TestCallTimeoutOverridesLimits(t *testing.T) {
	clitest.Fake(t, "az", "exec sleep 5")
	x := open(t, connector.StaticFields{})
	x.t.Limits.Timeout = time.Hour
	start := time.Now()
	err := x.Do(context.Background(), connector.Call{Verb: "whoami", Timeout: 200 * time.Millisecond}, &connector.Collector{})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("want timeout from Call.Timeout, got %v after %s", err, time.Since(start))
	}
}
