package server

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// costGuard is the per-target read cost guard (DESIGN §7): estimate first,
// refuse with too_expensive when the estimate exceeds the effective timeout.
// The timeout itself is always also the hard kill (batch ctx deadline +
// server-side limits via Call.Timeout), --force or not.
type costGuard struct {
	read    bool
	force   bool
	timeout time.Duration
	source  string // flag | system | env | global | default

	// results (for the audit)
	est         *connector.Estimate
	unavailable string // why there is no estimate ("" when est != nil or not attempted)
	blocked     bool
}

// check runs the estimator when the call is a read without --force. A nil
// guard (writes in tests) checks nothing.
func (g *costGuard) check(ctx context.Context, c connector.Connector, call connector.Call, env, sys string) error {
	if g == nil || !g.read {
		return nil
	}
	if g.force {
		g.unavailable = "skipped (--force)"
		return nil
	}
	est, ok := c.(connector.Estimator)
	if !ok {
		g.unavailable = "unavailable (type has no estimator)"
		return nil
	}
	// the estimate itself must stay cheap: at most a quarter of the budget, capped
	ectx, cancel := context.WithTimeout(ctx, min(max(g.timeout/4, time.Second), 5*time.Second))
	defer cancel()
	e, err := est.Estimate(ectx, call)
	if err != nil {
		if err == connector.ErrNoEstimate {
			g.unavailable = "unavailable (verb not estimated)"
		} else {
			g.unavailable = "unavailable: " + errs.As(err).Message
		}
		return nil
	}
	g.est = &e
	if e.Duration <= g.timeout {
		return nil
	}
	g.blocked = true
	why := e.Why
	if why == "" {
		why = "estimate"
	}
	return errs.New(errs.TooExpensive, "%s.%s est. ~%s (%s) > timeout %s", env, sys, fmtDur(e.Duration), why, g.timeout).
		WithHint(fmt.Sprintf("narrow the query, or --timeout %s, or --force", suggest(e.Duration))).At(env, sys)
}

// audit records the guard's decision on a command event.
func (g *costGuard) audit(a map[string]any) {
	if g == nil {
		return
	}
	a["timeout_ms"] = g.timeout.Milliseconds()
	a["timeout_source"] = g.source
	if !g.read {
		return
	}
	a["blocked"] = g.blocked
	switch {
	case g.est != nil:
		a["estimate"] = map[string]any{"duration_ms": g.est.Duration.Milliseconds(), "rows": g.est.Rows,
			"bytes": g.est.Bytes, "why": g.est.Why, "confident": g.est.Confident}
	case g.unavailable != "":
		a["estimate"] = g.unavailable
	}
}

// fmtDur renders an estimate compactly: 850ms, 4.2s, 3m10s, 2h5m.
func fmtDur(d time.Duration) string {
	switch {
	case d >= 1000*time.Hour:
		return ">1000h"
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}

// suggest is a --timeout value comfortably above the estimate (×1.5,
// rounded up to a whole second / minute).
func suggest(d time.Duration) string {
	s := d.Seconds() * 1.5
	if s >= 3.6e6 {
		return "1000h"
	}
	if s < 120 {
		return fmt.Sprintf("%ds", int(math.Ceil(s)))
	}
	return fmt.Sprintf("%dm", int(math.Ceil(s/60)))
}
