package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
)

// Default calibration (rough), same as the prometheus / loki connectors;
// override per target with Limits.PointsPerSecond / Limits.BytesPerSecond.
const (
	defaultPointsPerSecond = 10e6  // samples evaluated per second (prometheus)
	defaultBytesPerSecond  = 200e6 // chunk bytes scanned per second (loki)
	seriesCap              = 10000 // series probe bound: past it, "≥10.0K series"
	seriesProbeBytes       = 64 << 20
)

var _ connector.Estimator = (*Grafana)(nil)

// Estimate handles `query` against prometheus / loki datasources by probing
// them through Grafana's datasource proxy (series count / index stats).
// Other datasource types (and other verbs) have no estimate.
func (g *Grafana) Estimate(ctx context.Context, c connector.Call) (connector.Estimate, error) {
	if c.Verb != "query" {
		return connector.Estimate{}, connector.ErrNoEstimate
	}
	if err := g.Guard(c); err != nil {
		return connector.Estimate{}, err
	}
	uid, expr, err := g.target(c)
	if err != nil {
		return connector.Estimate{}, err
	}
	since := c.Since
	if since <= 0 {
		since = time.Hour
	}
	var ds datasource
	if err := g.call(ctx, http.MethodGet, "/api/datasources/uid/"+url.PathEscape(uid), nil, nil, &ds); err != nil {
		return connector.Estimate{}, err
	}
	proxy := "/api/datasources/proxy/uid/" + url.PathEscape(uid)
	now := g.now()
	switch {
	case strings.Contains(ds.Type, "prometheus"):
		sels := promSelectors(expr)
		if len(sels) == 0 {
			return connector.Estimate{}, connector.ErrNoEstimate
		}
		q := url.Values{"match[]": sels, "start": {strconv.FormatInt(now.Add(-since).Unix(), 10)},
			"end": {strconv.FormatInt(now.Unix(), 10)}, "limit": {strconv.Itoa(seriesCap + 1)}}
		var res struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := g.callN(ctx, http.MethodGet, proxy+"/api/v1/series", q, nil, &res, seriesProbeBytes); err != nil {
			return connector.Estimate{}, err
		}
		steps := grafanaSteps(c, since, min(g.limit(c), 1000))
		return pointsEstimate(int64(len(res.Data)), steps, since, g.t.Limits.PointsPerSecond), nil
	case ds.Type == "loki":
		sels := streamSelectors(expr)
		if len(sels) == 0 {
			return connector.Estimate{}, connector.ErrNoEstimate
		}
		var total lokiStats
		for _, sel := range sels {
			q := url.Values{"query": {sel}, "start": {strconv.FormatInt(now.Add(-since).UnixNano(), 10)},
				"end": {strconv.FormatInt(now.UnixNano(), 10)}}
			var st lokiStats
			if err := g.call(ctx, http.MethodGet, proxy+"/loki/api/v1/index/stats", q, nil, &st); err != nil {
				return connector.Estimate{}, err
			}
			total.Entries += st.Entries
			total.Bytes += st.Bytes
		}
		return bytesEstimate(total, since, strings.Join(sels, " + "), g.t.Limits.BytesPerSecond), nil
	}
	return connector.Estimate{}, connector.ErrNoEstimate
}

type lokiStats struct {
	Entries int64 `json:"entries"`
	Bytes   int64 `json:"bytes"`
}

// grafanaSteps: an instant query is 1 point per series; a range query has
// window/step+1 points, or (no -F step) Grafana's maxDataPoints at most,
// with a 15s floor on the interval.
func grafanaSteps(c connector.Call, window time.Duration, maxPoints int) int64 {
	if c.Flags["instant"] == "true" || c.Flags["queryType"] == "instant" {
		return 1
	}
	if st := c.Flags["step"]; st != "" {
		if d, err := time.ParseDuration(st); err == nil && d > 0 {
			return int64(window/d) + 1
		}
	}
	return min(int64(window/(15*time.Second))+1, int64(maxPoints))
}

// pointsEstimate mirrors prometheus.PointsEstimate.
func pointsEstimate(series, steps int64, window time.Duration, rate float64) connector.Estimate {
	capped := series > seriesCap
	if capped {
		series = seriesCap
	}
	points := series * steps
	sc := "~" + connector.Human(float64(series))
	if capped {
		sc = "≥" + connector.Human(float64(series))
	}
	return connector.Estimate{
		Duration:  connector.PerSecond(float64(points), connector.Rate(rate, defaultPointsPerSecond)),
		Rows:      points,
		Why:       fmt.Sprintf("%s series × %d steps = %s points over %s", sc, steps, connector.Human(float64(points)), window),
		Confident: !capped,
	}
}

// bytesEstimate mirrors loki.BytesEstimate.
func bytesEstimate(st lokiStats, window time.Duration, selector string, rate float64) connector.Estimate {
	return connector.Estimate{
		Duration:  connector.PerSecond(float64(st.Bytes), connector.Rate(rate, defaultBytesPerSecond)),
		Rows:      st.Entries,
		Bytes:     st.Bytes,
		Why:       fmt.Sprintf("~%s over %s in %s", connector.HumanBytes(float64(st.Bytes)), window, selector),
		Confident: true,
	}
}

// streamSelectors mirrors loki.StreamSelectors.
func streamSelectors(q string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(q); {
		switch q[i] {
		case '"', '`':
			i = skipString(q, i)
		case '{':
			j := braceEnd(q, i)
			if q[j-1] != '}' {
				return out
			}
			if sel := q[i:j]; !seen[sel] {
				seen[sel] = true
				out = append(out, sel)
			}
			i = j
		default:
			i++
		}
	}
	return out
}

// promSelectors mirrors prometheus.Selectors.
var (
	identRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*`)
	// clauses whose parenthesised contents are label names, not selectors.
	labelListRe = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\(([^)]*)\)`)
)

var promKeywords = map[string]bool{"and": true, "or": true, "unless": true, "by": true, "without": true, "on": true,
	"ignoring": true, "group_left": true, "group_right": true, "offset": true, "bool": true, "inf": true, "nan": true, "atan2": true}

// (prometheus.Selectors) extracts the vector selectors of a PromQL expression
// ("name", "name{…}" or "{…}"), deduplicated, in order. Rough by design:
// identifiers followed by "(" are functions, string literals, [ranges],
// numbers and by/without(...) label lists are skipped.
func promSelectors(expr string) []string {
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
