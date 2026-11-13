package connector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// Estimate is a connector's guess of what a read will cost, made BEFORE it
// runs (EXPLAIN, explain, _count, index stats, series × points, ...).
type Estimate struct {
	Duration  time.Duration // expected wall time
	Rows      int64         // rows/docs/series/lines it expects to scan (0 = unknown)
	Bytes     int64         // bytes it expects to scan (0 = unknown)
	Why       string        // short reason, e.g. "Seq Scan on events, ~4.2M rows"
	Confident bool          // false = heuristic only
}

// Estimator is optional: implemented by connectors that can estimate a read
// before running it. The server calls it (after Open, before Do) for every
// read verb unless --force is given; when Duration exceeds the call's
// effective timeout, Do is not called and the call fails with too_expensive.
// Return ErrNoEstimate for verbs it does not estimate; any error means
// "estimate unavailable" and the call runs under the hard timeout only.
type Estimator interface {
	Estimate(ctx context.Context, c Call) (Estimate, error)
}

// ErrNoEstimate: this call has no estimator (runs under the hard timeout).
var ErrNoEstimate = errors.New("no estimate for this call")

// ServerTimeout is the limit a connector passes to the system itself
// (statement_timeout, maxTimeMS, ...): the call's effective timeout, else
// the target's statement timeout, else the default.
func ServerTimeout(c Call, l Limits) time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	if l.StatementTimeout > 0 {
		return l.StatementTimeout
	}
	return DefaultLimits.StatementTimeout
}

// CallTimeout is the wall-time budget of a call: its effective timeout, else
// the target's configured timeout, else the default.
func CallTimeout(c Call, l Limits) time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	if l.Timeout > 0 {
		return l.Timeout
	}
	return DefaultLimits.Timeout
}

// Rate returns v, or def when v is unset.
func Rate(v, def float64) float64 {
	if v > 0 {
		return v
	}
	return def
}

// PerSecond converts units at rate units/second into a duration.
func PerSecond(units, rate float64) time.Duration {
	if units <= 0 || rate <= 0 {
		return 0
	}
	s := units / rate
	if s > float64(math.MaxInt64/int64(time.Second)) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(s * float64(time.Second))
}

// Human formats a count compactly: 950, 12.3K, 4.2M, 1.1B.
func Human(n float64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1fB", n/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", n/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fK", n/1e3)
	}
	return fmt.Sprintf("%.0f", n)
}

// HumanBytes formats a byte count: 512B, 3.4MB, 1.2GB.
func HumanBytes(n float64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.1fTB", n/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", n/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", n/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", n/(1<<10))
	}
	return fmt.Sprintf("%.0fB", n)
}
