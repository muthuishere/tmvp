package gcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/connectors/internal/cli"
	"github.com/muthuishere/tmvp/internal/connector"
)

var _ connector.Estimator = (*GCP)(nil)

// Estimate is a no-network heuristic for the logs verb: scanning Cloud Logging
// costs ~60s per hour of range unfiltered, ~0.25s per hour filtered.
func (g *GCP) Estimate(ctx context.Context, c connector.Call) (connector.Estimate, error) {
	if c.Verb != "logs" {
		return connector.Estimate{}, connector.ErrNoEstimate
	}

	rng, err := cli.SinceOr(c, time.Hour, 7*24*time.Hour)
	if err != nil {
		return connector.Estimate{}, err
	}
	return logEstimate(rng, strings.TrimSpace(strings.Join(c.Args, " ")) != "", "Cloud Logging"), nil
}

// logEstimate is the pure range × filter heuristic shared by the logs verb.
func logEstimate(rng time.Duration, filtered bool, src string) connector.Estimate {
	h := rng.Hours()
	if filtered {
		return connector.Estimate{Duration: connector.PerSecond(h, 4),
			Why: fmt.Sprintf("filtered scan over %s of %s", rng, src)}
	}
	return connector.Estimate{Duration: connector.PerSecond(h*60, 1),
		Why: fmt.Sprintf("no filter over %s of %s", rng, src)}
}
