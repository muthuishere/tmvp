package elasticsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// Default calibration (rough, per target override: Limits.RowsPerSecond).
const (
	// plainDocsPerSecond: a cluster scores/collects matched docs from the
	// inverted index at roughly 50M docs/s; plain searches are cheap.
	plainDocsPerSecond = 50e6
	// expensiveDocsPerSecond: leading wildcards, regexps and scripts visit
	// every term/doc of the index at roughly 1M docs/s.
	expensiveDocsPerSecond = 1e6
)

var _ connector.Estimator = (*ES)(nil)

// Estimate runs _count with the same query. When the query carries an
// expensive construct (leading wildcard, regexp, script) the cost is the
// whole index's doc count instead of the matches.
func (s *ES) Estimate(ctx context.Context, c connector.Call) (connector.Estimate, error) {
	if c.Verb != "search" && c.Verb != "count" {
		return connector.Estimate{}, connector.ErrNoEstimate
	}
	if err := s.Guard(c); err != nil {
		return connector.Estimate{}, err
	}
	index, m, q, err := s.resolve(c)
	if err != nil {
		return connector.Estimate{}, err
	}
	what := Expensive(m)
	if what == "" {
		what = expensiveLucene(q.Get("q"), "")
	}
	var body []byte
	if v, ok := m["query"]; ok {
		body, _ = json.Marshal(map[string]any{"query": v})
	}
	path := "/" + esc(index) + "/_count"
	if what != "" { // whole index: _count without the query
		body, q = nil, nil
	}
	rb, err := s.req(ctx, http.MethodPost, path, q, body)
	if err != nil {
		return connector.Estimate{}, err
	}
	var out struct {
		Count int64 `json:"count"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return connector.Estimate{}, errs.Wrap(errs.Runtime, err, "decode count")
	}
	return DocsEstimate(out.Count, index, what, s.t.Limits.RowsPerSecond), nil
}

// DocsEstimate: docs = matched docs (what == "") or the whole index (what
// names the expensive construct; it then runs at 1/50 of the plain rate).
func DocsEstimate(docs int64, index, what string, rate float64) connector.Estimate {
	if what != "" {
		return connector.Estimate{
			Duration: connector.PerSecond(float64(docs), connector.Rate(rate, plainDocsPerSecond)*expensiveDocsPerSecond/plainDocsPerSecond),
			Rows:     docs,
			Why:      fmt.Sprintf("%s, ~%s docs in %s", what, connector.Human(float64(docs)), index),
		}
	}
	return connector.Estimate{
		Duration:  connector.PerSecond(float64(docs), connector.Rate(rate, plainDocsPerSecond)),
		Rows:      docs,
		Why:       fmt.Sprintf("~%s matching docs in %s", connector.Human(float64(docs)), index),
		Confident: true,
	}
}

var scriptKeys = map[string]bool{"script": true, "script_score": true, "scripted_metric": true, "script_fields": true, "runtime_mappings": true}

// Expensive names the first expensive construct in a search body
// ("leading wildcard on message", "regexp on host", "script_score"), or "".
func Expensive(v any) string {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			val := x[k]
			switch {
			case scriptKeys[k]:
				return k
			case k == "regexp":
				return "regexp on " + firstField(val)
			case k == "wildcard":
				f := firstField(val)
				if p := wildcardPattern(val, f); strings.HasPrefix(p, "*") || strings.HasPrefix(p, "?") {
					return "leading wildcard on " + f
				}
			case k == "query_string" || k == "simple_query_string":
				if qm, ok := val.(map[string]any); ok {
					qs, _ := qm["query"].(string)
					df, _ := qm["default_field"].(string)
					if w := expensiveLucene(qs, df); w != "" {
						return w
					}
				}
			}
			if w := Expensive(val); w != "" {
				return w
			}
		}
	case []any:
		for _, e := range x {
			if w := Expensive(e); w != "" {
				return w
			}
		}
	}
	return ""
}

func firstField(v any) string {
	if m, ok := v.(map[string]any); ok {
		for k := range m {
			if k != "boost" && k != "rewrite" && k != "flags" {
				return k
			}
		}
	}
	return "?"
}

func wildcardPattern(v any, field string) string {
	m, _ := v.(map[string]any)
	switch p := m[field].(type) {
	case string:
		return p
	case map[string]any:
		for _, k := range []string{"value", "wildcard"} {
			if s, ok := p[k].(string); ok {
				return s
			}
		}
	}
	return ""
}

// luceneTerm: optional field:, then a leading */? followed by more chars, or a /regex/.
var luceneTerm = regexp.MustCompile(`(?:^|[\s(])(?:([\w.@-]+):)?(?:([*?])[^\s*)]|(/))`)

// expensiveLucene detects a leading wildcard or /regexp/ in a Lucene query string.
func expensiveLucene(qs, defField string) string {
	m := luceneTerm.FindStringSubmatch(qs)
	if m == nil {
		return ""
	}
	field := m[1]
	if field == "" {
		field = defField
	}
	if field == "" {
		field = "all fields"
	}
	if m[3] != "" {
		return "regexp on " + field
	}
	return "leading wildcard on " + field
}
