package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// Default calibration (rough): Loki queriers chew through on the order of
// 200 MB/s of chunk data for a filtered query across a modest cluster.
// Override per target with Limits.BytesPerSecond.
const defaultBytesPerSecond = 200e6

var _ connector.Estimator = (*Loki)(nil)

// IndexStats is the /loki/api/v1/index/stats response.
type IndexStats struct {
	Streams int64 `json:"streams"`
	Chunks  int64 `json:"chunks"`
	Entries int64 `json:"entries"`
	Bytes   int64 `json:"bytes"`
}

// Estimate asks index/stats how many bytes the query's stream selectors
// cover over its window.
func (l *Loki) Estimate(ctx context.Context, c connector.Call) (connector.Estimate, error) {
	var def time.Duration
	switch c.Verb {
	case "range", "series", "query":
		def = time.Hour
	case "tail":
		def = 15 * time.Minute
	default:
		return connector.Estimate{}, connector.ErrNoEstimate
	}
	q, err := l.logql(c)
	if c.Verb == "series" {
		q, err = strings.Join(c.Args, " "), nil
	}
	if err != nil {
		return connector.Estimate{}, err
	}
	since, err := l.since(c, def)
	if err != nil {
		return connector.Estimate{}, err
	}
	if c.Verb == "query" {
		since = MaxRange(q, 5*time.Minute) // instant: the [range] it aggregates
	}
	sels := StreamSelectors(q)
	if len(sels) == 0 {
		return connector.Estimate{}, connector.ErrNoEstimate
	}
	var total IndexStats
	for _, sel := range sels {
		v := l.window(since)
		v.Set("query", sel)
		body, _, err := l.raw(ctx, "/loki/api/v1/index/stats", v)
		if err != nil {
			return connector.Estimate{}, err
		}
		var st IndexStats
		if err := json.Unmarshal(body, &st); err != nil {
			return connector.Estimate{}, errs.Wrap(errs.Runtime, err, "decode index stats")
		}
		total.Streams += st.Streams
		total.Chunks += st.Chunks
		total.Entries += st.Entries
		total.Bytes += st.Bytes
	}
	return BytesEstimate(total, since, strings.Join(sels, " + "), l.t.Limits.BytesPerSecond), nil
}

// BytesEstimate turns index stats into an Estimate:
// "~3.4GB over 24h in {app="api"}".
func BytesEstimate(st IndexStats, window time.Duration, selector string, rate float64) connector.Estimate {
	return connector.Estimate{
		Duration:  connector.PerSecond(float64(st.Bytes), connector.Rate(rate, defaultBytesPerSecond)),
		Rows:      st.Entries,
		Bytes:     st.Bytes,
		Why:       fmt.Sprintf("~%s over %s in %s", connector.HumanBytes(float64(st.Bytes)), ShortDur(window), selector),
		Confident: true,
	}
}

// ShortDur renders 24h → "24h", 7d → "7d", 90m → "1h30m".
func ShortDur(d time.Duration) string {
	switch {
	case d >= 48*time.Hour && d%(24*time.Hour) == 0:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

var rangeRe = regexp.MustCompile(`\[([0-9]+[smhdwy]+(?:[0-9]+[smhdwy]+)*)\]`)

// MaxRange is the largest [range] in a LogQL expression, else def.
func MaxRange(q string, def time.Duration) time.Duration {
	best := time.Duration(0)
	for _, m := range rangeRe.FindAllStringSubmatch(q, -1) {
		if d, err := parseDur(m[1]); err == nil && d > best {
			best = d
		} else if d, err := time.ParseDuration(m[1]); err == nil && d > best {
			best = d
		}
	}
	if best == 0 {
		return def
	}
	return best
}

// StreamSelectors returns the distinct {…} stream selectors of a LogQL
// expression, skipping string literals (so line_format "{{.x}}" is ignored).
func StreamSelectors(q string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(q); {
		switch q[i] {
		case '"', '`':
			i = skipString(q, i)
		case '{':
			j := i + 1
			for j < len(q) && q[j] != '}' {
				if q[j] == '"' || q[j] == '`' {
					j = skipString(q, j)
					continue
				}
				j++
			}
			if j >= len(q) {
				return out
			}
			sel := q[i : j+1]
			if !seen[sel] {
				seen[sel] = true
				out = append(out, sel)
			}
			i = j + 1
		default:
			i++
		}
	}
	return out
}

func skipString(s string, i int) int {
	ch := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\\' && ch != '`' {
			j++
			continue
		}
		if s[j] == ch {
			return j + 1
		}
	}
	return len(s)
}
