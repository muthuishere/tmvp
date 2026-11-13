// Package prometheus is the tmvp connector for the Prometheus HTTP API (also
// Thanos / Mimir / VictoriaMetrics query frontends). Read only.
//
// Config fields: url (or base_url), token (bearer) or user/password (basic),
// headers (map), insecure. Mode-aware like every connector.
//
// Verbs: query <promql> [-F time=RFC3339], range <promql> (window --since,
// default 1h, ≤ 7d; -F step=30s), alerts.
package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	httpc "github.com/muthuishere/tmvp/connectors/http"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() { connector.Register("prometheus", New) }

// New returns an unopened prometheus connector.
func New() connector.Connector { return &Prom{} }

// Prom is one API client.
type Prom struct {
	t      *connector.Target
	base   *url.URL
	client *http.Client
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls [substring] — metric names (label __name__ values), filtered by substring, capped by --limit"},
	{Name: "query", Mode: connector.Read, Help: `query "<promql>" [-F time=RFC3339] — instant vector → one row per series`},
	{Name: "range", Mode: connector.Read, Help: `range "<promql>" [-F step=30s] — over --since (default 1h, ≤7d), one row per sample`},
	{Name: "alerts", Mode: connector.Read, Help: "alerts — active alerts (name, state, severity, since)"},
}

func (p *Prom) Type() string            { return "prometheus" }
func (p *Prom) Verbs() []connector.Verb { return verbs }

// Guard checks arguments before Open.
func (p *Prom) Guard(c connector.Call) error {
	if (c.Verb == "query" || c.Verb == "range") && len(c.Args) == 0 {
		return errs.New(errs.Usage, "%s needs a PromQL expression", c.Verb).WithHint(`query "up"`)
	}
	if c.Since > 7*24*time.Hour {
		return errs.New(errs.GuardRejected, "--since %s exceeds the 7d cap", c.Since)
	}
	return nil
}

// Open parses the URL and builds the client.
func (p *Prom) Open(ctx context.Context, t *connector.Target) error {
	p.t = t
	key := "url"
	if !t.Has("url") && !t.Has(string(t.Mode)+".url") {
		key = "base_url"
	}
	raw, err := t.ModeString(ctx, key)
	if err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errs.New(errs.Config, "%s.url is not an http(s) URL", t.System)
	}
	if u.User != nil {
		return errs.New(errs.Config, "%s.url must not carry credentials; use user/password or token", t.System)
	}
	p.base = u
	insecure, _ := t.StringOr(ctx, "insecure", "false")
	p.client = httpc.NewClient(t, insecure == "true")
	return nil
}

func (p *Prom) Close() error {
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	return nil
}

// Check implements connector.Checker.
func (p *Prom) Check(ctx context.Context) error {
	_, err := p.api(ctx, "/api/v1/status/buildinfo", nil)
	return err
}

// Do runs one verb.
func (p *Prom) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := p.Guard(c); err != nil {
		return err
	}
	switch c.Verb {
	case "ls":
		data, err := p.api(ctx, "/api/v1/label/__name__/values", nil)
		if err != nil {
			return err
		}
		var names []string
		if err := json.Unmarshal(data, &names); err != nil {
			return errs.Wrap(errs.Runtime, err, "prometheus: decode metric names")
		}
		for _, n := range names {
			if len(c.Args) > 0 && !strings.Contains(n, c.Args[0]) {
				continue
			}
			if err := e.Row(connector.Row{"kind": "metric", "name": n}); err != nil {
				if errors.Is(err, connector.ErrLimit) {
					return nil
				}
				return err
			}
		}
		return nil
	case "query":
		q := url.Values{"query": {strings.Join(c.Args, " ")}, "timeout": {serverTimeout(c, p.t.Limits)}}
		if ts := c.Flags["time"]; ts != "" {
			q.Set("time", ts)
		}
		data, err := p.api(ctx, "/api/v1/query", q)
		if err != nil {
			return err
		}
		return emitResult(e, data, false)
	case "range":
		since := rangeSince(c)
		end := time.Now()
		step := c.Flags["step"]
		if step == "" {
			step = strconv.Itoa(int(defaultStep(since).Seconds())) + "s"
		}
		q := url.Values{"query": {strings.Join(c.Args, " ")}, "start": {end.Add(-since).UTC().Format(time.RFC3339)},
			"end": {end.UTC().Format(time.RFC3339)}, "step": {step}, "timeout": {serverTimeout(c, p.t.Limits)}}
		data, err := p.api(ctx, "/api/v1/query_range", q)
		if err != nil {
			return err
		}
		return emitResult(e, data, true)
	case "alerts":
		data, err := p.api(ctx, "/api/v1/alerts", nil)
		if err != nil {
			return err
		}
		var res struct {
			Alerts []struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				State       string            `json:"state"`
				ActiveAt    string            `json:"activeAt"`
				Value       string            `json:"value"`
			} `json:"alerts"`
		}
		if err := json.Unmarshal(data, &res); err != nil {
			return errs.Wrap(errs.Runtime, err, "parse alerts")
		}
		for _, a := range res.Alerts {
			summary := a.Annotations["summary"]
			if summary == "" {
				summary = a.Annotations["description"]
			}
			r := connector.Row{"alert": a.Labels["alertname"], "state": a.State, "severity": a.Labels["severity"],
				"since": a.ActiveAt, "value": a.Value, "labels": labelString(a.Labels, "alertname", "severity"), "summary": summary}
			if err := e.Row(r); err != nil {
				return ignoreLimit(err)
			}
		}
		return nil
	}
	return errs.New(errs.Usage, "prometheus has no verb %q", c.Verb)
}

// api GETs path with q and returns the "data" member of a success envelope.
func (p *Prom) api(ctx context.Context, path string, q url.Values) (json.RawMessage, error) {
	return p.apiN(ctx, path, q, p.t.Limits.Bytes)
}

// apiN is api with an explicit response-size bound.
func (p *Prom) apiN(ctx context.Context, path string, q url.Values, maxBytes int) (json.RawMessage, error) {
	u := *p.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errs.New(errs.Usage, "cannot build request")
	}
	req.Header.Set("User-Agent", "tmvp")
	if err := httpc.ApplyAuth(ctx, p.t, req); err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, httpc.NetErr(ctx, err)
	}
	defer resp.Body.Close()
	body, err := httpc.ReadBounded(resp.Body, maxBytes)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errs.AuthError(fmt.Errorf("HTTP %d", resp.StatusCode))
	}
	var env struct {
		Status    string          `json:"status"`
		Data      json.RawMessage `json:"data"`
		ErrorType string          `json:"errorType"`
		Error     string          `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, errs.New(errs.Runtime, "HTTP %d: not a Prometheus API response", resp.StatusCode)
	}
	if env.Status != "success" {
		code := errs.Runtime
		if env.ErrorType == "bad_data" {
			code = errs.Usage
		} else if env.ErrorType == "timeout" {
			code = errs.Timeout
		}
		return nil, errs.New(code, "prometheus %s: %s", env.ErrorType, env.Error)
	}
	return env.Data, nil
}

type series struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
	Values [][]any           `json:"values"`
}

func emitResult(e connector.Emit, data json.RawMessage, ranged bool) error {
	var res struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return errs.Wrap(errs.Runtime, err, "parse result")
	}
	switch res.ResultType {
	case "scalar", "string":
		var v []any
		_ = json.Unmarshal(res.Result, &v)
		return ignoreLimit(e.Row(sample(connector.Row{}, v)))
	}
	var ss []series
	if err := json.Unmarshal(res.Result, &ss); err != nil {
		return errs.Wrap(errs.Runtime, err, "parse result")
	}
	for _, s := range ss {
		base := connector.Row{"metric": seriesName(s.Metric)}
		if !ranged {
			if err := e.Row(sample(base, s.Value)); err != nil {
				return ignoreLimit(err)
			}
			continue
		}
		for _, v := range s.Values {
			r := connector.Row{"metric": base["metric"]}
			if err := e.Row(sample(r, v)); err != nil {
				return ignoreLimit(err)
			}
		}
	}
	return nil
}

func sample(r connector.Row, v []any) connector.Row {
	if len(v) == 2 {
		if ts, ok := v[0].(float64); ok {
			sec := int64(ts)
			r["time"] = time.Unix(sec, int64((ts-float64(sec))*1e9)).UTC().Format(time.RFC3339Nano)
		}
		if s, ok := v[1].(string); ok {
			if f, err := strconv.ParseFloat(s, 64); err == nil && !isSpecial(s) {
				r["value"] = f
			} else {
				r["value"] = s
			}
		}
	}
	return r
}

func isSpecial(s string) bool {
	switch strings.ToLower(s) {
	case "nan", "+inf", "-inf", "inf":
		return true
	}
	return false
}

func seriesName(m map[string]string) string {
	return m["__name__"] + "{" + labelString(m, "__name__") + "}"
}

func labelString(m map[string]string, skip ...string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		skipped := false
		for _, s := range skip {
			skipped = skipped || k == s
		}
		if !skipped {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + strconv.Quote(m[k])
	}
	return strings.Join(parts, ",")
}

func ignoreLimit(err error) error {
	if err == connector.ErrLimit {
		return nil
	}
	return err
}
