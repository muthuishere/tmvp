// Package loki is the tmvp connector for Grafana Loki (logs). Read only,
// plain net/http through the target's via-aware dialer.
//
// Config fields (mode-aware): url (or base_url); token (bearer) or
// user/password (basic, e.g. Grafana Cloud); tenant (X-Scope-OrgID); headers;
// ca (${file:secrets.ca}); insecure; max_range (default 7d).
//
// Every query must carry a real label selector ({app="api"}); `{}` or only
// match-everything regexes are refused before Loki sees them.
package loki

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	httpc "github.com/muthuishere/tmvp/connectors/http"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() { connector.Register("loki", New) }

// New returns an unopened loki connector.
func New() connector.Connector { return &Loki{} }

// Loki is one API client.
type Loki struct {
	t        *connector.Target
	base     *url.URL
	client   *http.Client
	maxRange time.Duration
	now      func() time.Time
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls [substring] — label names (over --since, default 6h) + saved queries/*.logql"},
	{Name: "labels", Mode: connector.Read, Help: "labels — label names seen in --since (default 6h)"},
	{Name: "values", Mode: connector.Read, Help: "values <label> — values of a label in --since (default 6h)"},
	{Name: "query", Mode: connector.Read, Help: `query '<logql>' [-F time=RFC3339] — instant query (metric LogQL → one row per series)`},
	{Name: "range", Mode: connector.Read, Saved: true, Help: `range '<logql>'|@saved — log lines (newest first) or metric samples over --since (default 1h, ≤ max_range 7d); lines capped by --limit; -F step=1m -F direction=forward`},
	{Name: "tail", Mode: connector.Read, Help: `tail '<logql>'|@saved [-F follow=10s] — last lines of --since (default 15m) oldest→newest; follow polls for new lines, bounded by time and --limit`},
	{Name: "series", Mode: connector.Read, Help: `series '<selector>' — label sets of matching streams in --since (default 1h)`},
}

func (l *Loki) Type() string            { return "loki" }
func (l *Loki) Verbs() []connector.Verb { return verbs }

var matcherRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*(=~|!~|!=|=)\s*("(?:[^"\\]|\\.)*"|` + "`[^`]*`" + `)`)

// checkSelector refuses a query whose first stream selector matches everything.
func checkSelector(q string) error {
	q = strings.TrimSpace(q)
	if q == "" {
		return errs.New(errs.Usage, "missing LogQL").WithHint(`range '{app="api"} |= "error"'`)
	}
	if strings.HasPrefix(q, "@") {
		return nil // saved: checked after loading
	}
	i := strings.Index(q, "{")
	if i < 0 {
		return errs.New(errs.Usage, "LogQL needs a stream selector like {app=\"api\"}")
	}
	j := strings.Index(q[i:], "}")
	if j < 0 {
		return errs.New(errs.Usage, "unterminated stream selector")
	}
	for _, m := range matcherRe.FindAllStringSubmatch(q[i+1:i+j], -1) {
		val := m[3][1 : len(m[3])-1]
		switch m[2] {
		case "=":
			if val != "" {
				return nil
			}
		case "=~":
			if val != "" && val != ".*" && val != ".+" && val != ".*?" {
				return nil
			}
		}
	}
	return errs.New(errs.GuardRejected, "stream selector %s matches every stream", q[i:i+j+1]).
		WithHint(`narrow it with a label, e.g. {app="api", env="prod"} (tmvp … labels / values <label>)`)
}

// Guard checks arguments before Open.
func (l *Loki) Guard(c connector.Call) error {
	if _, ok := connector.FindVerb(verbs, c.Verb); !ok {
		return errs.New(errs.Usage, "loki has no verb %q", c.Verb)
	}
	switch c.Verb {
	case "values":
		if len(c.Args) == 0 {
			return errs.New(errs.Usage, "values needs a label name")
		}
	case "query", "range", "tail", "series":
		if err := checkSelector(strings.Join(c.Args, " ")); err != nil {
			return err
		}
	}
	if c.Since > 30*24*time.Hour {
		return errs.New(errs.GuardRejected, "--since %s exceeds 30d", c.Since)
	}
	return nil
}

// Open parses the URL and verifies the endpoint + credential.
func (l *Loki) Open(ctx context.Context, t *connector.Target) error {
	l.t, l.now = t, time.Now
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
		return errs.New(errs.Config, "%s.url must not carry credentials; use token or user/password", t.System)
	}
	l.base = u
	l.maxRange = 7 * 24 * time.Hour
	if mr, _ := t.StringOr(ctx, "max_range", ""); mr != "" {
		d, err := parseDur(mr)
		if err != nil || d <= 0 {
			return errs.New(errs.Config, "%s.max_range %q is not a duration", t.System, mr)
		}
		l.maxRange = d
	}
	insecure, _ := t.StringOr(ctx, "insecure", "false")
	l.client = httpc.NewClient(t, insecure == "true")
	if t.Has("ca") {
		pem, err := t.File(ctx, "ca")
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return errs.New(errs.Config, "%s.ca holds no PEM certificate", t.System)
		}
		tr := l.client.Transport.(*http.Transport)
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return l.Check(ctx)
}

// parseDur accepts Go durations plus "d" days.
func parseDur(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		return time.Duration(n) * 24 * time.Hour, err
	}
	return time.ParseDuration(s)
}

// Close drops idle connections.
func (l *Loki) Close() error {
	if l.client != nil {
		l.client.CloseIdleConnections()
	}
	return nil
}

// Check lists labels over the last 5 minutes (auth + tenant check).
func (l *Loki) Check(ctx context.Context) error {
	_, err := l.api(ctx, "/loki/api/v1/labels", l.window(5*time.Minute))
	return err
}

func (l *Loki) window(since time.Duration) url.Values {
	end := l.now()
	return url.Values{"start": {strconv.FormatInt(end.Add(-since).UnixNano(), 10)}, "end": {strconv.FormatInt(end.UnixNano(), 10)}}
}

// api GETs path and returns the "data" member.
func (l *Loki) api(ctx context.Context, path string, q url.Values) (json.RawMessage, error) {
	body, code, err := l.raw(ctx, path, q)
	if err != nil {
		return nil, err
	}
	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Status != "success" {
		return nil, errs.New(errs.Runtime, "HTTP %d: not a Loki API response", code)
	}
	return env.Data, nil
}

// raw GETs path and returns the 2xx body (error statuses mapped).
func (l *Loki) raw(ctx context.Context, path string, q url.Values) ([]byte, int, error) {
	u := *l.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, errs.New(errs.Usage, "cannot build request")
	}
	req.Header.Set("User-Agent", "tmvp")
	if err := httpc.ApplyAuth(ctx, l.t, req); err != nil {
		return nil, 0, err
	}
	if l.t.Has(string(l.t.Mode)+".tenant") || l.t.Has("tenant") {
		ten, err := l.t.ModeString(ctx, "tenant")
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("X-Scope-OrgID", ten)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, 0, httpc.NetErr(ctx, err)
	}
	defer resp.Body.Close()
	body, err := httpc.ReadBounded(resp.Body, l.t.Limits.Bytes)
	if err != nil {
		return nil, 0, err
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, 0, errs.AuthError(fmt.Errorf("HTTP 401"))
	case resp.StatusCode == http.StatusForbidden:
		return nil, 0, errs.New(errs.PolicyBlocked, "HTTP 403: %s", msg).WithHint("the credential/tenant may not read this")
	case resp.StatusCode == http.StatusBadRequest:
		return nil, 0, errs.New(errs.Usage, "loki: %s", msg)
	case resp.StatusCode == http.StatusGatewayTimeout || resp.StatusCode == http.StatusRequestTimeout:
		return nil, 0, errs.New(errs.Timeout, "loki: %s", msg)
	case resp.StatusCode == http.StatusNotFound:
		return nil, 0, errs.New(errs.NotFound, "HTTP 404 (is url the Loki root, without /loki/api?)")
	case resp.StatusCode >= 300:
		return nil, 0, errs.New(errs.Runtime, "HTTP %d: %s", resp.StatusCode, msg)
	}
	return body, resp.StatusCode, nil
}

func ignoreLimit(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}

func (l *Loki) lineCap(c connector.Call) int {
	if c.Limit > 0 {
		return c.Limit
	}
	return l.t.Limits.Lines
}

// logql resolves '<logql>' or @saved (queries/<name>.logql) and re-checks the selector.
func (l *Loki) logql(c connector.Call) (string, error) {
	q := strings.TrimSpace(strings.Join(c.Args, " "))
	if strings.HasPrefix(q, "@") && len(c.Args) == 1 {
		s, err := l.t.LoadSaved("queries", q, ".logql")
		if err != nil {
			return "", errs.Wrap(errs.NotFound, err, "")
		}
		q = strings.TrimSpace(s.Body)
	}
	return q, checkSelector(q)
}

// Do runs one verb.
func (l *Loki) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := l.Guard(c); err != nil {
		return err
	}
	return ignoreLimit(l.do(ctx, c, e))
}

func (l *Loki) since(c connector.Call, def time.Duration) (time.Duration, error) {
	s := c.Since
	if s <= 0 {
		s = def
	}
	if s > l.maxRange {
		return 0, errs.New(errs.GuardRejected, "--since %s exceeds max_range %s", s, l.maxRange)
	}
	return s, nil
}

func (l *Loki) do(ctx context.Context, c connector.Call, e connector.Emit) error {
	switch c.Verb {
	case "ls", "labels":
		since, err := l.since(c, 6*time.Hour)
		if err != nil {
			return err
		}
		names, err := l.strings(ctx, "/loki/api/v1/labels", l.window(since))
		if err != nil {
			return err
		}
		for _, n := range names {
			if c.Verb == "ls" && len(c.Args) > 0 && !strings.Contains(n, c.Args[0]) {
				continue
			}
			r := connector.Row{"label": n}
			if c.Verb == "ls" {
				r = connector.Row{"kind": "label", "name": n}
			}
			if err := e.Row(r); err != nil {
				return err
			}
		}
		if c.Verb == "ls" {
			return l.t.EmitSaved(e, "queries", ".logql")
		}
		return nil
	case "values":
		since, err := l.since(c, 6*time.Hour)
		if err != nil {
			return err
		}
		vals, err := l.strings(ctx, "/loki/api/v1/label/"+url.PathEscape(c.Args[0])+"/values", l.window(since))
		if err != nil {
			return err
		}
		for _, v := range vals {
			if err := e.Row(connector.Row{"label": c.Args[0], "value": v}); err != nil {
				return err
			}
		}
		return nil
	case "series":
		since, err := l.since(c, time.Hour)
		if err != nil {
			return err
		}
		q := l.window(since)
		q.Set("match[]", strings.Join(c.Args, " "))
		data, err := l.api(ctx, "/loki/api/v1/series", q)
		if err != nil {
			return err
		}
		var ss []map[string]string
		if err := json.Unmarshal(data, &ss); err != nil {
			return errs.Wrap(errs.Runtime, err, "decode series")
		}
		for _, s := range ss {
			if err := e.Row(connector.Row{"labels": labelString(s)}); err != nil {
				return err
			}
		}
		return nil
	case "query":
		q, err := l.logql(c)
		if err != nil {
			return err
		}
		v := url.Values{"query": {q}, "limit": {strconv.Itoa(l.lineCap(c))}}
		if ts := c.Flags["time"]; ts != "" {
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				return errs.New(errs.Usage, "time=%q is not RFC3339", ts)
			}
			v.Set("time", strconv.FormatInt(t.UnixNano(), 10))
		}
		data, err := l.api(ctx, "/loki/api/v1/query", v)
		if err != nil {
			return err
		}
		return emitResult(e, data, false)
	case "range":
		q, err := l.logql(c)
		if err != nil {
			return err
		}
		since, err := l.since(c, time.Hour)
		if err != nil {
			return err
		}
		v := l.window(since)
		v.Set("query", q)
		v.Set("limit", strconv.Itoa(l.lineCap(c)))
		dir := c.Flags["direction"]
		if dir == "" {
			dir = "backward"
		}
		if dir != "backward" && dir != "forward" {
			return errs.New(errs.Usage, "direction must be backward or forward")
		}
		v.Set("direction", dir)
		if st := c.Flags["step"]; st != "" {
			v.Set("step", st)
		}
		data, err := l.api(ctx, "/loki/api/v1/query_range", v)
		if err != nil {
			return err
		}
		return emitResult(e, data, dir == "forward")
	case "tail":
		return l.tail(ctx, c, e)
	}
	return errs.New(errs.Usage, "loki has no verb %q", c.Verb)
}

func (l *Loki) strings(ctx context.Context, path string, q url.Values) ([]string, error) {
	data, err := l.api(ctx, path, q)
	if err != nil {
		return nil, err
	}
	var out []string
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, errs.Wrap(errs.Runtime, err, "decode %s", path)
		}
	}
	sort.Strings(out)
	return out, nil
}

// tail emits the last lines oldest→newest, then optionally polls for new
// lines for -F follow (bounded by the call timeout and --limit).
func (l *Loki) tail(ctx context.Context, c connector.Call, e connector.Emit) error {
	q, err := l.logql(c)
	if err != nil {
		return err
	}
	since, err := l.since(c, 15*time.Minute)
	if err != nil {
		return err
	}
	var follow time.Duration
	if f := c.Flags["follow"]; f != "" {
		if follow, err = time.ParseDuration(f); err != nil || follow <= 0 {
			return errs.New(errs.Usage, "follow=%q is not a duration", f)
		}
		follow = min(follow, connector.CallTimeout(c, l.t.Limits)-2*time.Second)
	}
	capN := l.lineCap(c)
	v := l.window(since)
	v.Set("query", q)
	v.Set("limit", strconv.Itoa(min(capN, 100)))
	v.Set("direction", "backward")
	data, err := l.api(ctx, "/loki/api/v1/query_range", v)
	if err != nil {
		return err
	}
	lines, err := streamLines(data)
	if err != nil {
		return err
	}
	sortLines(lines, true)
	var last int64
	emitted := 0
	for _, ln := range lines {
		if err := e.Row(ln.row()); err != nil {
			return err
		}
		emitted++
		last = max(last, ln.ns)
	}
	if follow == 0 {
		return nil
	}
	deadline := l.now().Add(follow)
	if last == 0 {
		last = l.now().UnixNano()
	}
	for l.now().Before(deadline) && emitted < capN {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
		v := url.Values{"query": {q}, "start": {strconv.FormatInt(last+1, 10)}, "end": {strconv.FormatInt(l.now().UnixNano(), 10)},
			"limit": {strconv.Itoa(capN - emitted)}, "direction": {"forward"}}
		data, err := l.api(ctx, "/loki/api/v1/query_range", v)
		if err != nil {
			return err
		}
		lines, err := streamLines(data)
		if err != nil {
			return err
		}
		sortLines(lines, true)
		for _, ln := range lines {
			if err := e.Row(ln.row()); err != nil {
				return err
			}
			emitted++
			last = max(last, ln.ns)
		}
	}
	return nil
}

type line struct {
	ns     int64
	labels string
	text   string
}

func (x line) row() connector.Row {
	return connector.Row{"time": time.Unix(0, x.ns).UTC().Format(time.RFC3339Nano), "labels": x.labels, "line": x.text}
}

func sortLines(ls []line, asc bool) {
	sort.SliceStable(ls, func(i, j int) bool {
		if asc {
			return ls[i].ns < ls[j].ns
		}
		return ls[i].ns > ls[j].ns
	})
}

type result struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

func streamLines(data json.RawMessage) ([]line, error) {
	var res result
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, "decode result")
	}
	if res.ResultType != "streams" {
		return nil, errs.New(errs.Usage, "tail needs a log query, not a metric query (%s)", res.ResultType)
	}
	var ss []struct {
		Stream map[string]string `json:"stream"`
		Values [][2]string       `json:"values"`
	}
	if err := json.Unmarshal(res.Result, &ss); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, "decode streams")
	}
	var out []line
	for _, s := range ss {
		ls := labelString(s.Stream)
		for _, v := range s.Values {
			ns, _ := strconv.ParseInt(v[0], 10, 64)
			out = append(out, line{ns: ns, labels: ls, text: v[1]})
		}
	}
	return out, nil
}

// emitResult writes log lines (merged across streams, newest first unless
// forward) or metric samples.
func emitResult(e connector.Emit, data json.RawMessage, forward bool) error {
	var res result
	if err := json.Unmarshal(data, &res); err != nil {
		return errs.Wrap(errs.Runtime, err, "decode result")
	}
	switch res.ResultType {
	case "streams":
		lines, err := streamLines(data)
		if err != nil {
			return err
		}
		sortLines(lines, forward)
		for _, ln := range lines {
			if err := e.Row(ln.row()); err != nil {
				return err
			}
		}
		return nil
	case "vector", "matrix":
		var ss []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
			Values [][]any           `json:"values"`
		}
		if err := json.Unmarshal(res.Result, &ss); err != nil {
			return errs.Wrap(errs.Runtime, err, "decode samples")
		}
		for _, s := range ss {
			vals := s.Values
			if res.ResultType == "vector" {
				vals = [][]any{s.Value}
			}
			for _, v := range vals {
				if err := e.Row(sample(connector.Row{"metric": "{" + labelString(s.Metric) + "}"}, v)); err != nil {
					return err
				}
			}
		}
		return nil
	case "scalar":
		var v []any
		_ = json.Unmarshal(res.Result, &v)
		return e.Row(sample(connector.Row{}, v))
	}
	return errs.New(errs.Runtime, "unknown result type %q", res.ResultType)
}

func sample(r connector.Row, v []any) connector.Row {
	if len(v) == 2 {
		if ts, ok := v[0].(float64); ok {
			sec := int64(ts)
			r["time"] = time.Unix(sec, int64((ts-float64(sec))*1e9)).UTC().Format(time.RFC3339Nano)
		}
		if s, ok := v[1].(string); ok {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				r["value"] = f
			} else {
				r["value"] = s
			}
		}
	}
	return r
}

func labelString(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + strconv.Quote(m[k])
	}
	return strings.Join(parts, ",")
}
