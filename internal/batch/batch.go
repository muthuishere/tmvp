// Package batch runs one verb across many (env, system) targets in parallel,
// streaming rows tagged with env+system (DESIGN §6). Writes are never batched.
package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

// Unit is one target. Skip, when non-empty, is the reason it is skipped.
type Unit struct {
	Env    string
	System string
	Skip   string
	// Timeout overrides Options.Timeout for this unit (its effective
	// timeout: --timeout > system > env > global limits.timeout). It is the
	// hard kill — ctx deadline — even with --force.
	Timeout time.Duration
}

// Exec runs the verb on one unit.
type Exec func(ctx context.Context, u Unit, e connector.Emit) error

// Options control a batch.
type Options struct {
	Parallel int           // max concurrent units (default 8)
	Timeout  time.Duration // per unit (default 60s)
	Diff     bool          // only output rows/lines where units disagree
	Write    bool          // refuse when len(units) > 1
	Rows     int           // per-unit row cap (default connector.DefaultLimits.Rows)
	Lines    int           // per-unit line cap
	Bytes    int           // per-unit byte cap
}

// Result summarises a batch.
type Result struct {
	OK, Failed, Skipped int
	Exit                int
	Errors              []*errs.Error
}

// Run executes exec for every unit and streams frames to sink. It does not
// send the final "done" frame (the caller does, with Result.Exit).
func Run(ctx context.Context, units []Unit, exec Exec, o Options, sink protocol.Sink) Result {
	if o.Parallel <= 0 {
		o.Parallel = 8
	}
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	if o.Rows <= 0 {
		o.Rows = connector.DefaultLimits.Rows
	}
	if o.Lines <= 0 {
		o.Lines = connector.DefaultLimits.Lines
	}
	if o.Bytes <= 0 {
		o.Bytes = connector.DefaultLimits.Bytes
	}
	var res Result
	if o.Write && len(units) > 1 {
		e := errs.New(errs.PolicyBlocked, "writes run on exactly one env and one system (selector matched %d)", len(units)).
			WithHint("tmvp write <env> <system> ...")
		sink(protocol.Frame{Type: protocol.FrameError, Error: e})
		res.Failed, res.Exit, res.Errors = len(units), errs.ExitCode(e.Code), []*errs.Error{e}
		return res
	}
	type outcome struct {
		rows  []connector.Row
		lines []string
		err   error
	}
	outs := make([]outcome, len(units))
	var mu sync.Mutex
	sem := make(chan struct{}, o.Parallel)
	var wg sync.WaitGroup
	for i, u := range units {
		if u.Skip != "" {
			sink(protocol.Frame{Type: protocol.FrameStatus, Env: u.Env, System: u.System, Status: "skipped", Text: u.Skip})
			mu.Lock()
			res.Skipped++
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(i int, u Unit) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			to := o.Timeout
			if u.Timeout > 0 {
				to = u.Timeout
			}
			uctx, cancel := context.WithTimeout(ctx, to)
			defer cancel()
			em := &capEmit{u: u, o: o, sink: sink, diff: o.Diff}
			err := safeExec(uctx, exec, u, em)
			if err != nil && uctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
				err = errs.New(errs.Timeout, "timed out after %s (hard timeout)", to).
					WithHint(fmt.Sprintf("narrow the query, or raise it: --timeout %s", to*2))
			}
			el := float64(time.Since(start).Microseconds()) / 1000
			mu.Lock()
			defer mu.Unlock()
			outs[i] = outcome{rows: em.rows, lines: em.lines, err: err}
			if err != nil {
				e := errs.As(err).At(u.Env, u.System)
				res.Failed++
				res.Errors = append(res.Errors, e)
				sink(protocol.Frame{Type: protocol.FrameError, Env: u.Env, System: u.System, Error: e, Elapsed: el})
				return
			}
			res.OK++
			st := "ok"
			if em.truncated {
				st = "truncated"
			}
			sink(protocol.Frame{Type: protocol.FrameStatus, Env: u.Env, System: u.System, Status: st, Elapsed: el})
		}(i, u)
	}
	wg.Wait()
	if o.Diff {
		emitDiff(units, func(i int) ([]connector.Row, []string, bool) {
			return outs[i].rows, outs[i].lines, outs[i].err == nil && units[i].Skip == ""
		}, sink)
	}
	res.Exit = exitFor(res)
	return res
}

func safeExec(ctx context.Context, exec Exec, u Unit, e connector.Emit) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errs.New(errs.Runtime, "connector panic: %v", r)
		}
	}()
	return exec(ctx, u, e)
}

func exitFor(r Result) int {
	switch {
	case r.Failed == 0:
		return 0
	case r.OK > 0:
		return errs.ExitCode(errs.Partial)
	default:
		// all failed: the most severe-looking code of the first error
		return errs.ExitCode(r.Errors[0].Code)
	}
}

// capEmit tags rows with env/system, enforces caps, and buffers for --diff.
type capEmit struct {
	u         Unit
	o         Options
	sink      protocol.Sink
	diff      bool
	mu        sync.Mutex
	nrows     int
	nlines    int
	nbytes    int
	truncated bool
	rows      []connector.Row
	lines     []string
}

func (c *capEmit) Row(r connector.Row) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return connector.ErrLimit
	}
	b, _ := json.Marshal(r)
	if c.nrows >= c.o.Rows || c.nbytes+len(b) > c.o.Bytes {
		c.truncate(fmt.Sprintf("row limit reached (%d rows); narrow the query or pass --limit", c.nrows))
		return connector.ErrLimit
	}
	c.nrows++
	c.nbytes += len(b)
	if c.diff {
		c.rows = append(c.rows, r)
		return nil
	}
	c.sink(protocol.Frame{Type: protocol.FrameRow, Env: c.u.Env, System: c.u.System, Row: tag(r, c.u)})
	return nil
}

func (c *capEmit) Text(s string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return connector.ErrLimit
	}
	if c.nlines >= c.o.Lines || c.nbytes+len(s) > c.o.Bytes {
		c.truncate(fmt.Sprintf("line limit reached (%d lines)", c.nlines))
		return connector.ErrLimit
	}
	c.nlines++
	c.nbytes += len(s)
	if c.diff {
		c.lines = append(c.lines, s)
		return nil
	}
	c.sink(protocol.Frame{Type: protocol.FrameText, Env: c.u.Env, System: c.u.System, Text: s})
	return nil
}

func (c *capEmit) truncate(msg string) {
	c.truncated = true
	c.sink(protocol.Frame{Type: protocol.FrameStatus, Env: c.u.Env, System: c.u.System, Status: "truncated", Text: msg})
}

func tag(r connector.Row, u Unit) connector.Row {
	out := make(connector.Row, len(r)+2)
	for k, v := range r {
		out[k] = v
	}
	out["env"], out["system"] = u.Env, u.System
	return out
}

// emitDiff outputs only rows/lines that are not identical across every
// successful unit.
func emitDiff(units []Unit, get func(i int) ([]connector.Row, []string, bool), sink protocol.Sink) {
	var idx []int
	for i := range units {
		if _, _, ok := get(i); ok {
			idx = append(idx, i)
		}
	}
	if len(idx) < 2 {
		for _, i := range idx {
			rows, lines, _ := get(i)
			for _, r := range rows {
				sink(protocol.Frame{Type: protocol.FrameRow, Env: units[i].Env, System: units[i].System, Row: tag(r, units[i])})
			}
			for _, l := range lines {
				sink(protocol.Frame{Type: protocol.FrameText, Env: units[i].Env, System: units[i].System, Text: l})
			}
		}
		return
	}
	rowCount := map[string]int{}
	lineCount := map[string]int{}
	for _, i := range idx {
		rows, lines, _ := get(i)
		seenR, seenL := map[string]bool{}, map[string]bool{}
		for _, r := range rows {
			k := rowKey(r)
			if !seenR[k] {
				seenR[k] = true
				rowCount[k]++
			}
		}
		for _, l := range lines {
			if !seenL[l] {
				seenL[l] = true
				lineCount[l]++
			}
		}
	}
	same := 0
	for _, i := range idx {
		rows, lines, _ := get(i)
		for _, r := range rows {
			if rowCount[rowKey(r)] == len(idx) {
				same++
				continue
			}
			sink(protocol.Frame{Type: protocol.FrameRow, Env: units[i].Env, System: units[i].System, Row: tag(r, units[i])})
		}
		for _, l := range lines {
			if lineCount[l] == len(idx) {
				same++
				continue
			}
			sink(protocol.Frame{Type: protocol.FrameText, Env: units[i].Env, System: units[i].System, Text: l})
		}
	}
	sink(protocol.Frame{Type: protocol.FrameInfo, Info: map[string]any{"diff_identical": same, "diff_targets": len(idx)}})
}

func rowKey(r connector.Row) string {
	keys := make([]string, 0, len(r))
	for k := range r {
		if k != "env" && k != "system" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	m := make([]any, 0, 2*len(keys))
	for _, k := range keys {
		m = append(m, k, r[k])
	}
	b, _ := json.Marshal(m)
	return string(b)
}
