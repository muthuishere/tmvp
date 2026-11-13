package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// Default calibration (rough): a Prometheus-compatible engine evaluates on
// the order of 10M samples/second for a simple selector. Override per target
// with Limits.PointsPerSecond.
const defaultPointsPerSecond = 10e6

// seriesCap bounds the /api/v1/series probe: past it we only know "≥ cap".
const seriesCap = 10000

// seriesProbeBytes bounds the probe response (10K label sets fit easily).
const seriesProbeBytes = 64 << 20

var _ connector.Estimator = (*Prom)(nil)

// Estimate counts the series the query's selectors touch (/api/v1/series,
// bounded) and multiplies by the steps evaluated.
func (p *Prom) Estimate(ctx context.Context, c connector.Call) (connector.Estimate, error) {
	if (c.Verb != "query" && c.Verb != "range") || len(c.Args) == 0 {
		return connector.Estimate{}, connector.ErrNoEstimate
	}
	sels := Selectors(strings.Join(c.Args, " "))
	if len(sels) == 0 {
		return connector.Estimate{}, connector.ErrNoEstimate
	}
	end := time.Now()
	window, steps := time.Duration(0), int64(1)
	if c.Verb == "range" {
		window = rangeSince(c)
		steps = Steps(window, parseStep(c.Flags["step"], defaultStep(window)))
	}
	start := end.Add(-window)
	if window == 0 {
		start = end.Add(-5 * time.Minute) // instant query: default lookback
	}
	q := url.Values{"match[]": sels, "start": {strconv.FormatInt(start.Unix(), 10)},
		"end": {strconv.FormatInt(end.Unix(), 10)}, "limit": {strconv.Itoa(seriesCap + 1)}}
	data, err := p.apiN(ctx, "/api/v1/series", q, seriesProbeBytes)
	if err != nil {
		return connector.Estimate{}, err
	}
	var ss []json.RawMessage
	if err := json.Unmarshal(data, &ss); err != nil {
		return connector.Estimate{}, errs.Wrap(errs.Runtime, err, "decode series")
	}
	return PointsEstimate(int64(len(ss)), steps, window, p.t.Limits.PointsPerSecond), nil
}

// PointsEstimate turns a series count and steps into an Estimate. A count
// above seriesCap is reported as "≥10.0K series" and is not confident.
func PointsEstimate(series, steps int64, window time.Duration, rate float64) connector.Estimate {
	capped := series > seriesCap
	if capped {
		series = seriesCap
	}
	points := series * steps
	sc := "~" + connector.Human(float64(series))
	if capped {
		sc = "≥" + connector.Human(float64(series))
	}
	why := fmt.Sprintf("%s series × 1 step = %s points (instant)", sc, connector.Human(float64(points)))
	if window > 0 {
		why = fmt.Sprintf("%s series × %d steps = %s points over %s", sc, steps, connector.Human(float64(points)), window)
	}
	return connector.Estimate{
		Duration:  connector.PerSecond(float64(points), connector.Rate(rate, defaultPointsPerSecond)),
		Rows:      points,
		Why:       why,
		Confident: !capped,
	}
}

// Steps is the number of evaluation points in a window: window/step + 1.
func Steps(window, step time.Duration) int64 {
	if step <= 0 || window <= 0 {
		return 1
	}
	return int64(window/step) + 1
}

func rangeSince(c connector.Call) time.Duration {
	if c.Since > 0 {
		return c.Since
	}
	return time.Hour
}

// defaultStep is what range uses without -F step: ~250 points, ≥ 15s.
func defaultStep(since time.Duration) time.Duration {
	s := since / 250
	if s < 15*time.Second {
		s = 15 * time.Second
	}
	return time.Duration(int(s.Seconds())) * time.Second
}

// parseStep accepts "30s" or a number of seconds; def when unparsable.
func parseStep(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
		return time.Duration(f * float64(time.Second))
	}
	return def
}

func serverTimeout(c connector.Call, l connector.Limits) string {
	return strconv.FormatInt(connector.ServerTimeout(c, l).Milliseconds(), 10) + "ms"
}

var (
	identRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*`)
	// clauses whose parenthesised contents are label names, not selectors.
	labelListRe = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\(([^)]*)\)`)
)

var promKeywords = map[string]bool{"and": true, "or": true, "unless": true, "by": true, "without": true, "on": true,
	"ignoring": true, "group_left": true, "group_right": true, "offset": true, "bool": true, "inf": true, "nan": true, "atan2": true}

// Selectors extracts the vector selectors of a PromQL expression
// ("name", "name{…}" or "{…}"), deduplicated, in order. Rough by design:
// identifiers followed by "(" are functions, string literals, [ranges],
// numbers and by/without(...) label lists are skipped.
func Selectors(expr string) []string {
	expr = labelListRe.ReplaceAllString(expr, " ")
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for i := 0; i < len(expr); {
		ch := expr[i]
		switch {
		case ch == '"' || ch == '\'' || ch == '`':
			i = skipString(expr, i)
		case ch == '[':
			if j := strings.IndexByte(expr[i:], ']'); j >= 0 {
				i += j + 1
			} else {
				i = len(expr)
			}
		case ch == '{':
			j := braceEnd(expr, i)
			add(expr[i:j])
			i = j
		case ch >= '0' && ch <= '9' || ch == '.':
			for i < len(expr) && (isWord(expr[i]) || expr[i] == '.') {
				i++
			}
		case isWord(ch):
			id := identRe.FindString(expr[i:])
			if id == "" {
				i++
				continue
			}
			j := i + len(id)
			k := j
			for k < len(expr) && (expr[k] == ' ' || expr[k] == '\t' || expr[k] == '\n') {
				k++
			}
			switch {
			case k < len(expr) && expr[k] == '(':
				// function / aggregation call
			case promKeywords[strings.ToLower(id)]:
			case k < len(expr) && expr[k] == '{':
				e := braceEnd(expr, k)
				add(id + expr[k:e])
				j = e
			default:
				add(id)
			}
			i = j
		default:
			i++
		}
	}
	return out
}

func isWord(b byte) bool {
	return b == '_' || b == ':' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// skipString returns the index after the string literal starting at i.
func skipString(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\\' && q != '`' {
			j++
			continue
		}
		if s[j] == q {
			return j + 1
		}
	}
	return len(s)
}

// braceEnd returns the index after the "}" closing the "{" at i (strings skipped).
func braceEnd(s string, i int) int {
	for j := i + 1; j < len(s); {
		switch s[j] {
		case '"', '\'', '`':
			j = skipString(s, j)
		case '}':
			return j + 1
		default:
			j++
		}
	}
	return len(s)
}
