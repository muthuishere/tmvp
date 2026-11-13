package az

import (
	"context"
	"fmt"
	"github.com/muthuishere/tmvp/connectors/internal/cli"
	"github.com/muthuishere/tmvp/internal/connector"
	"regexp"
	"strings"
	"time"
)

var _ connector.Estimator = (*AZ)(nil)

// Estimate is a no-network heuristic for the logs verb: scanning Log Analytics logs
// costs ~60s per hour of range unfiltered, ~0.25s per hour filtered.
func (a *AZ) Estimate(ctx context.Context, c connector.Call) (connector.Estimate, error) {
	if c.Verb != "logs" {
		return connector.Estimate{}, connector.ErrNoEstimate
	}
	if strings.TrimSpace(strings.Join(c.Args, " ")) == "" {
		return connector.Estimate{}, connector.ErrNoEstimate // activity log: bounded by --max-events
	}
	rng, err := cli.SinceOr(c, time.Hour, 7*24*time.Hour)
	if err != nil {
		return connector.Estimate{}, err
	}
	return logEstimate(rng, kqlFilter.MatchString(strings.Join(c.Args, " ")), "Log Analytics logs"), nil
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

// kqlFilter: the KQL narrows rows server-side (where/search).
var kqlFilter = regexp.MustCompile(`(?i)\b(where|search)\b`)
