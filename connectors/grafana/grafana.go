// Package grafana is the tmvp connector for the Grafana HTTP API: find
// dashboards and their queries, list datasources, read alert state and
// annotations, and run a query against any datasource through /api/ds/query
// (Prometheus, Loki, Elasticsearch, OTel backends … behind Grafana). Plain
// net/http through the target's via-aware dialer.
//
// Config fields (mode-aware): url (or base_url); token (service-account
// token, Bearer); org_id (X-Grafana-Org-Id); headers; ca; insecure.
//
// Writes: annotate, silence create|expire. No dashboard/datasource deletes.
package grafana

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	httpc "github.com/muthuishere/tmvp/connectors/http"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() { connector.Register("grafana", New) }

// New returns an unopened grafana connector.
func New() connector.Connector { return &Grafana{} }

// Grafana is one API client.
type Grafana struct {
	t      *connector.Target
	base   *url.URL
	client *http.Client
	now    func() time.Time
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls [substring] — dashboards (uid, folder), datasources (uid, type), saved queries/*.expr"},
	{Name: "dashboards", Mode: connector.Read, Help: "dashboards [-F q=<title search>] [-F tag=<tag>] — uid, title, folder, tags"},
	{Name: "dashboard", Mode: connector.Read, Help: "dashboard <uid> — one row per panel query: panel, title, type, datasource, ref, expr"},
	{Name: "datasources", Mode: connector.Read, Help: "datasources — uid, name, type, default"},
	{Name: "alerts", Mode: connector.Read, Help: "alerts [-F state=firing|pending] — Grafana-managed (unified) alert instances: alert, state, severity, since, labels, summary"},
	{Name: "silences", Mode: connector.Read, Help: "silences — Grafana alertmanager silences: id, state, matchers, ends, comment"},
	{Name: "annotations", Mode: connector.Read, Help: "annotations [-F tags=a,b] [-F dashboard=<uid>] — over --since (default 24h)"},
	{Name: "query", Mode: connector.Read, Saved: true, Help: `query <datasource-uid> '<expr>' | query @saved — via /api/ds/query over --since (default 1h, ≤ 7d), rows capped by --limit; -F step=60s, other -F k=v go into the query object (e.g. -F queryType=range)`},
	{Name: "annotate", Mode: connector.Write, Default: true, Help: "annotate '<text>' [-F tags=deploy,api] [-F dashboard=<uid>] [-F panel=<id>] [-F time=RFC3339] [-F end=RFC3339]"},
	{Name: "silence", Mode: connector.Write, Help: `silence create '<alertname=X,env=~prod.*>' [-F duration=1h] | silence expire <id> — Grafana alertmanager (comment = --reason)`},
}

func (g *Grafana) Type() string            { return "grafana" }
func (g *Grafana) Verbs() []connector.Verb { return verbs }

// Guard validates arguments and mode before Open.
func (g *Grafana) Guard(c connector.Call) error {
	v, ok := connector.FindVerb(verbs, c.Verb)
	if !ok {
		return errs.New(errs.Usage, "grafana has no verb %q", c.Verb)
	}
	if v.Mode.Writable() && !c.Write {
		return errs.New(errs.PolicyBlocked, "%s changes Grafana", c.Verb).WithHint("tmvp write <env> <system> " + c.Verb + " … --reason \"…\"")
	}
	switch c.Verb {
	case "dashboard", "annotate":
		if len(c.Args) == 0 {
			return errs.New(errs.Usage, "%s needs an argument", c.Verb).WithHint(v.Help)
		}
	case "query":
		if len(c.Args) == 0 || (!strings.HasPrefix(c.Args[0], "@") && len(c.Args) < 2) {
			return errs.New(errs.Usage, "query needs <datasource-uid> '<expr>' or @saved").WithHint(v.Help)
		}
	case "silence":
		if len(c.Args) < 2 || (c.Args[0] != "create" && c.Args[0] != "expire") {
			return errs.New(errs.Usage, "silence create '<matchers>' | silence expire <id>")
		}
		if c.Args[0] == "create" {
			if _, err := parseMatchers(c.Args[1]); err != nil {
				return err
			}
		}
	}
	if c.Since > 30*24*time.Hour {
		return errs.New(errs.GuardRejected, "--since %s exceeds 30d", c.Since)
	}
	return nil
}

// Open parses the URL and verifies the token.
func (g *Grafana) Open(ctx context.Context, t *connector.Target) error {
	g.t, g.now = t, time.Now
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
		return errs.New(errs.Config, "%s.url must not carry credentials; use token", t.System)
	}
	g.base = u
	insecure, _ := t.StringOr(ctx, "insecure", "false")
	g.client = httpc.NewClient(t, insecure == "true")
	if t.Has("ca") {
		pem, err := t.File(ctx, "ca")
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return errs.New(errs.Config, "%s.ca holds no PEM certificate", t.System)
		}
		tr := g.client.Transport.(*http.Transport)
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return g.Check(ctx)
}

// Close drops idle connections.
func (g *Grafana) Close() error {
	if g.client != nil {
		g.client.CloseIdleConnections()
	}
	return nil
}

// Check calls /api/org (needs a valid token, any role).
func (g *Grafana) Check(ctx context.Context) error {
	var v map[string]any
	return g.call(ctx, http.MethodGet, "/api/org", nil, nil, &v)
}

// call sends one request; out (if non-nil) receives the decoded JSON body.
func (g *Grafana) call(ctx context.Context, method, path string, q url.Values, in, out any) error {
	return g.callN(ctx, method, path, q, in, out, g.t.Limits.Bytes)
}

// callN is call with an explicit response-size bound.
func (g *Grafana) callN(ctx context.Context, method, path string, q url.Values, in, out any, maxBytes int) error {
	u := *g.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = q.Encode()
	var body *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return errs.Wrap(errs.Runtime, err, "encode request")
		}
		body = bytes.NewReader(b)
	}
	var req *http.Request
	var err error
	if body != nil {
		req, err = http.NewRequestWithContext(ctx, method, u.String(), body)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, u.String(), nil)
	}
	if err != nil {
		return errs.New(errs.Usage, "cannot build request")
	}
	req.Header.Set("User-Agent", "tmvp")
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := httpc.ApplyAuth(ctx, g.t, req); err != nil {
		return err
	}
	if g.t.Has(string(g.t.Mode)+".org_id") || g.t.Has("org_id") {
		org, err := g.t.ModeString(ctx, "org_id")
		if err != nil {
			return err
		}
		req.Header.Set("X-Grafana-Org-Id", org)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return httpc.NetErr(ctx, err)
	}
	defer resp.Body.Close()
	b, err := httpc.ReadBounded(resp.Body, maxBytes)
	if err != nil {
		return err
	}
	msg := grafanaMsg(b)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errs.AuthError(fmt.Errorf("HTTP 401"))
	case resp.StatusCode == http.StatusForbidden:
		return errs.New(errs.PolicyBlocked, "HTTP 403: %s", msg).WithHint("the service account's role does not allow this")
	case resp.StatusCode == http.StatusNotFound:
		return errs.New(errs.NotFound, "HTTP 404: %s", msg)
	case resp.StatusCode == http.StatusBadRequest:
		return errs.New(errs.Usage, "HTTP 400: %s", msg)
	case resp.StatusCode == http.StatusGatewayTimeout:
		return errs.New(errs.Timeout, "HTTP 504: %s", msg)
	case resp.StatusCode >= 300 && !(path == "/api/ds/query" && resp.StatusCode == http.StatusMultiStatus):
		return errs.New(errs.Runtime, "HTTP %d: %s", resp.StatusCode, msg)
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return errs.Wrap(errs.Runtime, err, "decode %s", path)
	}
	return nil
}

func grafanaMsg(b []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	s := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		s = m.Message
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func ignoreLimit(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}

// Do runs one verb.
func (g *Grafana) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := g.Guard(c); err != nil {
		return err
	}
	return ignoreLimit(g.do(ctx, c, e))
}

func (g *Grafana) limit(c connector.Call) int {
	if c.Limit > 0 {
		return c.Limit
	}
	return g.t.Limits.Rows
}

type searchHit struct {
	UID         string   `json:"uid"`
	Title       string   `json:"title"`
	FolderTitle string   `json:"folderTitle"`
	Tags        []string `json:"tags"`
	URL         string   `json:"url"`
}

type datasource struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	IsDefault bool   `json:"isDefault"`
}

func (g *Grafana) do(ctx context.Context, c connector.Call, e connector.Emit) error {
	switch c.Verb {
	case "ls":
		filter := ""
		if len(c.Args) > 0 {
			filter = c.Args[0]
		}
		var hits []searchHit
		if err := g.call(ctx, http.MethodGet, "/api/search", url.Values{"type": {"dash-db"}, "limit": {strconv.Itoa(min(g.limit(c), 5000))}}, nil, &hits); err != nil {
			return err
		}
		for _, h := range hits {
			if filter != "" && !strings.Contains(strings.ToLower(h.Title), strings.ToLower(filter)) {
				continue
			}
			if err := e.Row(connector.Row{"kind": "dashboard", "name": h.Title, "uid": h.UID, "folder": h.FolderTitle}); err != nil {
				return err
			}
		}
		var ds []datasource
		if err := g.call(ctx, http.MethodGet, "/api/datasources", nil, nil, &ds); err != nil && errs.CodeOf(err) != errs.PolicyBlocked {
			return err // viewers may not list datasources; dashboards still help
		}
		for _, d := range ds {
			if filter != "" && !strings.Contains(strings.ToLower(d.Name), strings.ToLower(filter)) {
				continue
			}
			if err := e.Row(connector.Row{"kind": "datasource", "name": d.Name, "uid": d.UID, "type": d.Type}); err != nil {
				return err
			}
		}
		return g.t.EmitSaved(e, "queries", ".expr")
	case "dashboards":
		q := url.Values{"type": {"dash-db"}, "limit": {strconv.Itoa(min(g.limit(c), 5000))}}
		if s := c.Flags["q"]; s != "" {
			q.Set("query", s)
		}
		if s := c.Flags["tag"]; s != "" {
			q.Set("tag", s)
		}
		var hits []searchHit
		if err := g.call(ctx, http.MethodGet, "/api/search", q, nil, &hits); err != nil {
			return err
		}
		for _, h := range hits {
			if err := e.Row(connector.Row{"uid": h.UID, "title": h.Title, "folder": h.FolderTitle, "tags": strings.Join(h.Tags, ","), "url": h.URL}); err != nil {
				return err
			}
		}
		return nil
	case "dashboard":
		return g.dashboard(ctx, c.Args[0], e)
	case "datasources":
		var ds []datasource
		if err := g.call(ctx, http.MethodGet, "/api/datasources", nil, nil, &ds); err != nil {
			return err
		}
		for _, d := range ds {
			if err := e.Row(connector.Row{"uid": d.UID, "name": d.Name, "type": d.Type, "default": d.IsDefault}); err != nil {
				return err
			}
		}
		return nil
	case "alerts":
		return g.alerts(ctx, c, e)
	case "silences":
		return g.silences(ctx, e)
	case "annotations":
		return g.annotations(ctx, c, e)
	case "query":
		return g.query(ctx, c, e)
	case "annotate":
		return g.annotate(ctx, c, e)
	case "silence":
		return g.silence(ctx, c, e)
	}
	return errs.New(errs.Usage, "grafana has no verb %q", c.Verb)
}

func (g *Grafana) dashboard(ctx context.Context, uid string, e connector.Emit) error {
	var d struct {
		Dashboard struct {
			Title      string           `json:"title"`
			Panels     []map[string]any `json:"panels"`
			Datasource any              `json:"datasource"`
		} `json:"dashboard"`
	}
	if err := g.call(ctx, http.MethodGet, "/api/dashboards/uid/"+url.PathEscape(uid), nil, nil, &d); err != nil {
		return err
	}
	var walk func(ps []map[string]any) error
	walk = func(ps []map[string]any) error {
		for _, p := range ps {
			typ, _ := p["type"].(string)
			if typ == "row" {
				if sub, ok := p["panels"].([]any); ok {
					if err := walk(asMaps(sub)); err != nil {
						return err
					}
				}
				continue
			}
			base := connector.Row{"panel": p["id"], "title": p["title"], "type": typ}
			pds := dsName(p["datasource"])
			targets := asMaps(asSlice(p["targets"]))
			if len(targets) == 0 {
				base["datasource"] = pds
				if err := e.Row(base); err != nil {
					return err
				}
				continue
			}
			for _, tg := range targets {
				r := connector.Row{"panel": base["panel"], "title": base["title"], "type": typ, "ref": tg["refId"]}
				r["datasource"] = dsName(tg["datasource"])
				if r["datasource"] == "" || r["datasource"] == "-- Mixed --" {
					r["datasource"] = pds
				}
				for _, k := range []string{"expr", "query", "rawSql", "target", "expression"} {
					if s, ok := tg[k].(string); ok && s != "" {
						r["expr"] = s
						break
					}
				}
				if err := e.Row(r); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(d.Dashboard.Panels)
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

func asMaps(s []any) []map[string]any {
	out := make([]map[string]any, 0, len(s))
	for _, x := range s {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// dsName renders a panel/target datasource ref ({uid,type} or a legacy name).
func dsName(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		if u, ok := x["uid"].(string); ok && u != "" {
			return u
		}
		if t, ok := x["type"].(string); ok {
			return t
		}
	}
	return ""
}

func (g *Grafana) alerts(ctx context.Context, c connector.Call, e connector.Emit) error {
	var res struct {
		Data struct {
			Alerts []struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				State       string            `json:"state"`
				ActiveAt    string            `json:"activeAt"`
				Value       string            `json:"value"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if err := g.call(ctx, http.MethodGet, "/api/prometheus/grafana/api/v1/alerts", nil, nil, &res); err != nil {
		return err
	}
	want := strings.ToLower(c.Flags["state"])
	for _, a := range res.Data.Alerts {
		st := strings.ToLower(a.State)
		if strings.HasPrefix(st, "alerting") {
			st = "firing"
		}
		if want != "" && st != want {
			continue
		}
		summary := a.Annotations["summary"]
		if summary == "" {
			summary = a.Annotations["description"]
		}
		r := connector.Row{"alert": a.Labels["alertname"], "state": st, "severity": a.Labels["severity"], "since": a.ActiveAt,
			"value": a.Value, "labels": labelString(a.Labels, "alertname", "severity", "__alert_rule_uid__", "grafana_folder"), "summary": summary}
		if err := e.Row(r); err != nil {
			return err
		}
	}
	return nil
}

func (g *Grafana) silences(ctx context.Context, e connector.Emit) error {
	var ss []struct {
		ID       string                 `json:"id"`
		Status   struct{ State string } `json:"status"`
		Matchers []matcher              `json:"matchers"`
		StartsAt string                 `json:"startsAt"`
		EndsAt   string                 `json:"endsAt"`
		Comment  string                 `json:"comment"`
		By       string                 `json:"createdBy"`
	}
	if err := g.call(ctx, http.MethodGet, "/api/alertmanager/grafana/api/v2/silences", nil, nil, &ss); err != nil {
		return err
	}
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].EndsAt > ss[j].EndsAt })
	for _, s := range ss {
		ms := make([]string, len(s.Matchers))
		for i, m := range s.Matchers {
			ms[i] = m.String()
		}
		r := connector.Row{"id": s.ID, "state": s.Status.State, "matchers": strings.Join(ms, ","), "starts": s.StartsAt, "ends": s.EndsAt, "comment": s.Comment, "by": s.By}
		if err := e.Row(r); err != nil {
			return err
		}
	}
	return nil
}

func (g *Grafana) annotations(ctx context.Context, c connector.Call, e connector.Emit) error {
	since := c.Since
	if since <= 0 {
		since = 24 * time.Hour
	}
	now := g.now()
	q := url.Values{"from": {strconv.FormatInt(now.Add(-since).UnixMilli(), 10)}, "to": {strconv.FormatInt(now.UnixMilli(), 10)},
		"limit": {strconv.Itoa(min(g.limit(c), 5000))}}
	for _, tg := range splitList(c.Flags["tags"]) {
		q.Add("tags", tg)
	}
	if d := c.Flags["dashboard"]; d != "" {
		q.Set("dashboardUID", d)
	}
	var as []struct {
		ID           int64    `json:"id"`
		Time         int64    `json:"time"`
		TimeEnd      int64    `json:"timeEnd"`
		Text         string   `json:"text"`
		Tags         []string `json:"tags"`
		DashboardUID string   `json:"dashboardUID"`
		PanelID      int64    `json:"panelId"`
		Login        string   `json:"login"`
	}
	if err := g.call(ctx, http.MethodGet, "/api/annotations", q, nil, &as); err != nil {
		return err
	}
	for _, a := range as {
		r := connector.Row{"id": a.ID, "time": ms(a.Time), "text": a.Text, "tags": strings.Join(a.Tags, ","), "dashboard": a.DashboardUID, "panel": a.PanelID, "by": a.Login}
		if a.TimeEnd != 0 && a.TimeEnd != a.Time {
			r["time_end"] = ms(a.TimeEnd)
		}
		if err := e.Row(r); err != nil {
			return err
		}
	}
	return nil
}

func ms(v int64) string { return time.UnixMilli(v).UTC().Format(time.RFC3339Nano) }

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// query runs one expression through /api/ds/query and emits the frames as rows.
// target resolves query args (uid + expr, or @saved) into datasource uid and expression.
func (g *Grafana) target(c connector.Call) (string, string, error) {
	uid, expr := "", ""
	if strings.HasPrefix(c.Args[0], "@") {
		s, err := g.t.LoadSaved("queries", c.Args[0], ".expr")
		if err != nil {
			return "", "", errs.Wrap(errs.NotFound, err, "")
		}
		var fm struct {
			Datasource string `yaml:"datasource"`
		}
		_ = yaml.Unmarshal([]byte(s.FrontMatter), &fm)
		uid, expr = fm.Datasource, strings.TrimSpace(s.Body)
		if len(c.Args) > 1 {
			uid = c.Args[1]
		}
		if uid == "" {
			return "", "", errs.New(errs.Usage, "saved query %s has no datasource: front-matter", c.Args[0])
		}
	} else {
		uid, expr = c.Args[0], strings.Join(c.Args[1:], " ")
	}
	return uid, expr, nil
}

func (g *Grafana) query(ctx context.Context, c connector.Call, e connector.Emit) error {
	uid, expr, err := g.target(c)
	if err != nil {
		return err
	}
	since := c.Since
	if since <= 0 {
		since = time.Hour
	}
	if since > 7*24*time.Hour {
		return errs.New(errs.GuardRejected, "--since %s exceeds the 7d cap", since)
	}
	limit := g.limit(c)
	qo := map[string]any{"refId": "A", "datasource": map[string]any{"uid": uid}, "expr": expr, "query": expr,
		"maxDataPoints": min(limit, 1000), "maxLines": limit, "limit": limit}
	if st := c.Flags["step"]; st != "" {
		d, err := time.ParseDuration(st)
		if err != nil {
			return errs.New(errs.Usage, "step=%q is not a duration", st)
		}
		qo["intervalMs"] = d.Milliseconds()
	}
	for k, v := range c.Flags {
		if k == "step" {
			continue
		}
		qo[k] = flagValue(v)
	}
	now := g.now()
	in := map[string]any{"queries": []any{qo}, "from": strconv.FormatInt(now.Add(-since).UnixMilli(), 10), "to": strconv.FormatInt(now.UnixMilli(), 10)}
	var res struct {
		Results map[string]struct {
			Error  string  `json:"error"`
			Status int     `json:"status"`
			Frames []frame `json:"frames"`
		} `json:"results"`
	}
	if err := g.call(ctx, http.MethodPost, "/api/ds/query", nil, in, &res); err != nil {
		return err
	}
	r, ok := res.Results["A"]
	if !ok {
		return nil
	}
	if r.Error != "" {
		code := errs.Runtime
		if r.Status == http.StatusBadRequest || strings.Contains(strings.ToLower(r.Error), "parse") {
			code = errs.Usage
		}
		msg := r.Error
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return errs.New(code, "datasource %s: %s", uid, msg)
	}
	for _, f := range r.Frames {
		if err := f.emit(e, len(r.Frames) > 1); err != nil {
			return err
		}
	}
	return nil
}

func flagValue(v string) any {
	switch v {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return n
	}
	return v
}

type frame struct {
	Schema struct {
		Name   string `json:"name"`
		Fields []struct {
			Name   string            `json:"name"`
			Type   string            `json:"type"`
			Labels map[string]string `json:"labels"`
			Config struct {
				DisplayNameFromDS string `json:"displayNameFromDS"`
			} `json:"config"`
		} `json:"fields"`
	} `json:"schema"`
	Data struct {
		Values [][]any `json:"values"`
	} `json:"data"`
}

// emit writes one row per frame row: field name → value, time fields as
// RFC3339, series labels in "series".
func (f frame) emit(e connector.Emit, multi bool) error {
	if len(f.Data.Values) == 0 {
		return nil
	}
	n := len(f.Data.Values[0])
	var series []string
	for _, fl := range f.Schema.Fields {
		if len(fl.Labels) > 0 {
			series = append(series, labelString(fl.Labels))
		}
	}
	for i := 0; i < n; i++ {
		r := connector.Row{}
		if multi && f.Schema.Name != "" {
			r["frame"] = f.Schema.Name
		}
		if len(series) > 0 {
			r["series"] = strings.Join(series, " ")
		}
		for j, fl := range f.Schema.Fields {
			if j >= len(f.Data.Values) || i >= len(f.Data.Values[j]) {
				continue
			}
			v := f.Data.Values[j][i]
			name := fl.Name
			if name == "" {
				name = "field" + strconv.Itoa(j)
			}
			if fl.Type == "time" {
				if x, ok := v.(float64); ok {
					v = ms(int64(x))
				}
			}
			if m, ok := v.(map[string]any); ok {
				js, _ := json.Marshal(m)
				v = string(js)
			}
			r[name] = v
		}
		if err := e.Row(r); err != nil {
			return err
		}
	}
	return nil
}

func (g *Grafana) annotate(ctx context.Context, c connector.Call, e connector.Emit) error {
	in := map[string]any{"text": strings.Join(c.Args, " "), "tags": splitList(c.Flags["tags"])}
	if in["tags"] == nil {
		in["tags"] = []string{}
	}
	if d := c.Flags["dashboard"]; d != "" {
		in["dashboardUID"] = d
	}
	if p := c.Flags["panel"]; p != "" {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return errs.New(errs.Usage, "panel=%q is not a panel id", p)
		}
		in["panelId"] = n
	}
	for _, k := range []string{"time", "end"} {
		if s := c.Flags[k]; s != "" {
			ts, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return errs.New(errs.Usage, "%s=%q is not RFC3339", k, s)
			}
			if k == "time" {
				in["time"] = ts.UnixMilli()
			} else {
				in["timeEnd"] = ts.UnixMilli()
			}
		}
	}
	if c.Preview {
		js, _ := json.Marshal(in)
		return e.Row(connector.Row{"preview": true, "annotation": string(js)})
	}
	var out struct {
		ID      int64  `json:"id"`
		Message string `json:"message"`
	}
	if err := g.call(ctx, http.MethodPost, "/api/annotations", nil, in, &out); err != nil {
		return err
	}
	return e.Row(connector.Row{"id": out.ID, "message": out.Message})
}

type matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

func (m matcher) String() string {
	op := "="
	switch {
	case m.IsRegex && m.IsEqual:
		op = "=~"
	case m.IsRegex:
		op = "!~"
	case !m.IsEqual:
		op = "!="
	}
	return m.Name + op + strconv.Quote(m.Value)
}

// parseMatchers reads "alertname=X,env=~prod.*,team!=ops".
func parseMatchers(s string) ([]matcher, error) {
	var out []matcher
	for _, part := range splitList(strings.Trim(s, "{}")) {
		var m matcher
		found := false
		for _, op := range []string{"=~", "!~", "!=", "="} {
			if i := strings.Index(part, op); i > 0 {
				m = matcher{Name: strings.TrimSpace(part[:i]), Value: strings.Trim(strings.TrimSpace(part[i+len(op):]), `"`),
					IsRegex: op == "=~" || op == "!~", IsEqual: op == "=" || op == "=~"}
				found = true
				break
			}
		}
		if !found || m.Name == "" {
			return nil, errs.New(errs.Usage, "matcher %q: want name=value, name=~regex, name!=value", part)
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errs.New(errs.Usage, "silence needs at least one matcher")
	}
	hasEq := false
	for _, m := range out {
		if m.IsEqual && m.Value != "" && m.Value != ".*" && m.Value != ".+" {
			hasEq = true
		}
	}
	if !hasEq {
		return nil, errs.New(errs.PolicyBlocked, "silence would match every alert; add a positive matcher like alertname=X")
	}
	return out, nil
}

func (g *Grafana) silence(ctx context.Context, c connector.Call, e connector.Emit) error {
	if c.Args[0] == "expire" {
		id := c.Args[1]
		if c.Preview {
			return e.Row(connector.Row{"preview": true, "expire": id})
		}
		if err := g.call(ctx, http.MethodDelete, "/api/alertmanager/grafana/api/v2/silence/"+url.PathEscape(id), nil, nil, nil); err != nil {
			return err
		}
		return e.Row(connector.Row{"id": id, "expired": true})
	}
	ms, err := parseMatchers(c.Args[1])
	if err != nil {
		return err
	}
	dur := time.Hour
	if d := c.Flags["duration"]; d != "" {
		if dur, err = time.ParseDuration(d); err != nil || dur <= 0 {
			return errs.New(errs.Usage, "duration=%q is not a positive duration", d)
		}
	}
	if dur > 7*24*time.Hour {
		return errs.New(errs.GuardRejected, "silence duration %s exceeds 7d", dur)
	}
	comment := c.Reason
	if comment == "" {
		comment = "tmvp"
	}
	now := g.now().UTC()
	in := map[string]any{"matchers": ms, "startsAt": now.Format(time.RFC3339), "endsAt": now.Add(dur).Format(time.RFC3339),
		"createdBy": "tmvp", "comment": comment}
	if c.Preview {
		js, _ := json.Marshal(in)
		return e.Row(connector.Row{"preview": true, "silence": string(js)})
	}
	var out struct {
		ID string `json:"silenceID"`
	}
	if err := g.call(ctx, http.MethodPost, "/api/alertmanager/grafana/api/v2/silences", nil, in, &out); err != nil {
		return err
	}
	return e.Row(connector.Row{"id": out.ID, "ends": in["endsAt"]})
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
