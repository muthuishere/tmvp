// Package elasticsearch is the tmvp connector for Elasticsearch and
// OpenSearch (registered as both "elasticsearch" and "opensearch"). Plain
// net/http through the target's via-aware dialer; no vendor SDK.
//
// Config fields (mode-aware): url (or base_url); auth = api_key (ApiKey
// header) | token (Bearer) | user/password (basic); headers; ca
// (${file:secrets.ca}); insecure.
//
// A read connection only sends GET/HEAD, plus POST to the read endpoints
// _search, _count, _msearch, _mapping, _field_caps, _validate/query.
package elasticsearch

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

	"gopkg.in/yaml.v3"

	httpc "github.com/muthuishere/tmvp/connectors/http"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() {
	connector.Register("elasticsearch", New)
	connector.Register("opensearch", func() connector.Connector { return &ES{typ: "opensearch"} })
}

// New returns an unopened elasticsearch connector.
func New() connector.Connector { return &ES{typ: "elasticsearch"} }

// ES is one cluster client.
type ES struct {
	typ    string
	t      *connector.Target
	base   *url.URL
	client *http.Client
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls [substring] — indices (health, docs), aliases, saved queries/*.json"},
	{Name: "health", Mode: connector.Read, Help: "health — cluster health (status, nodes, shards)"},
	{Name: "indices", Mode: connector.Read, Help: "indices [pattern] — _cat/indices (health, status, docs, size)"},
	{Name: "mapping", Mode: connector.Read, Help: "mapping <index> — one row per field: index, field (dotted), type"},
	{Name: "search", Mode: connector.Read, Saved: true, Help: `search <index> '<json>'|@saved [-F q=<lucene>] — hits as rows; size ≤ --limit, server timeout; @saved = queries/<name>.json (front-matter index:)`},
	{Name: "count", Mode: connector.Read, Help: `count <index> ['<json>'|@saved] [-F q=<lucene>]`},
	{Name: "cat", Mode: connector.Read, Help: "cat <endpoint> [pattern] — allow-listed _cat endpoints: health indices aliases nodes shards allocation count segments thread_pool pending_tasks recovery templates master plugins fielddata tasks"},
	{Name: "get", Mode: connector.Read, Help: "get <index> <id> — one document"},
	{Name: "index", Mode: connector.Write, Default: true, Help: `index <index> '<json>'|@saved [-F id=<id>] — create/replace a document`},
	{Name: "update", Mode: connector.Write, Help: `update <index> <id> '<partial json>' — partial update (body without "doc"/"script" is wrapped in {"doc": …})`},
	{Name: "delete", Mode: connector.Write, Help: "delete <index> <id> — delete one document"},
	{Name: "update-by-query", Mode: connector.Write, Help: `update-by-query <index> '<json with query + script>' (--preview = count; no query = every doc → needs destructive)`},
	{Name: "delete-index", Mode: connector.Destructive, Help: "delete-index <index> — no wildcards, no _all"},
	{Name: "delete-by-query", Mode: connector.Destructive, Help: `delete-by-query <index> '<json with query>' (--preview = count)`},
	{Name: "reindex", Mode: connector.Destructive, Help: `reindex '<json {"source":…,"dest":…}>'`},
}

func (s *ES) Type() string            { return s.typ }
func (s *ES) Verbs() []connector.Verb { return verbs }

var catAllow = map[string]bool{"health": true, "indices": true, "aliases": true, "nodes": true, "shards": true, "allocation": true,
	"count": true, "segments": true, "thread_pool": true, "pending_tasks": true, "recovery": true, "templates": true,
	"master": true, "plugins": true, "fielddata": true, "tasks": true}

// readPOST lists the endpoints a read connection may POST to.
var readPOST = map[string]bool{"_search": true, "_count": true, "_msearch": true, "_mapping": true, "_field_caps": true, "query": true /* _validate/query */}

// Guard checks arguments and modes before Open.
func (s *ES) Guard(c connector.Call) error {
	need := map[string]int{"mapping": 1, "search": 1, "count": 1, "cat": 1, "get": 2, "index": 2, "update": 3, "delete": 2,
		"update-by-query": 2, "delete-index": 1, "delete-by-query": 2, "reindex": 1}
	if n, ok := need[c.Verb]; ok && len(c.Args) < n {
		return errs.New(errs.Usage, "%s needs %d argument(s)", c.Verb, n).WithHint(helpOf(c.Verb))
	}
	v, ok := connector.FindVerb(verbs, c.Verb)
	if !ok {
		return errs.New(errs.Usage, "%s has no verb %q", s.typ, c.Verb)
	}
	if v.Mode.Writable() && !c.Write {
		return errs.New(errs.PolicyBlocked, "%s mutates the cluster", c.Verb).WithHint("tmvp write <env> <system> " + c.Verb + " … --reason \"…\"")
	}
	if v.Mode == connector.Destructive && !c.Destructive {
		return errs.New(errs.PolicyBlocked, "%s is destructive; this env/system does not allow destructive ops", c.Verb)
	}
	switch c.Verb {
	case "cat":
		if !catAllow[c.Args[0]] {
			return errs.New(errs.PolicyBlocked, "_cat/%s is not allow-listed", c.Args[0]).WithHint(helpOf("cat"))
		}
	case "delete-index":
		if idx := c.Args[0]; idx == "_all" || strings.ContainsAny(idx, "*?,") || strings.HasPrefix(idx, "_") {
			return errs.New(errs.PolicyBlocked, "delete-index takes one concrete index name, not %q", idx)
		}
	case "update-by-query", "delete-by-query":
		if strings.HasPrefix(c.Args[1], "@") {
			return nil // checked again in Do after loading
		}
		if err := checkByQuery(c, c.Args[1]); err != nil {
			return err
		}
	}
	for _, a := range c.Args[:min(len(c.Args), 1)] {
		if c.Verb != "reindex" && c.Verb != "cat" && !strings.HasPrefix(a, "@") && (a == "" || strings.ContainsAny(a, "/ #")) {
			return errs.New(errs.Usage, "%q is not an index name", a)
		}
	}
	return nil
}

// checkByQuery refuses by-query writes that match every document unless destructive.
func checkByQuery(c connector.Call, body string) error {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return errs.New(errs.Usage, "body is not a JSON object: %v", err)
	}
	q, has := m["query"]
	all := !has
	if qm, ok := q.(map[string]any); ok {
		if _, mAll := qm["match_all"]; mAll && len(qm) == 1 {
			all = true
		}
	}
	if all && !c.Destructive {
		return errs.New(errs.PolicyBlocked, "%s without a query (or match_all) touches every document", c.Verb).WithHint("add a query, or set destructive: true")
	}
	return nil
}

func helpOf(verb string) string {
	v, _ := connector.FindVerb(verbs, verb)
	return v.Help
}

// Open parses the URL, builds the client and verifies auth.
func (s *ES) Open(ctx context.Context, t *connector.Target) error {
	s.t = t
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
		return errs.New(errs.Config, "%s.url must not carry credentials; use api_key, token or user/password", t.System)
	}
	s.base = u
	insecure, _ := t.StringOr(ctx, "insecure", "false")
	s.client = httpc.NewClient(t, insecure == "true")
	if t.Has("ca") {
		pem, err := t.File(ctx, "ca")
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return errs.New(errs.Config, "%s.ca holds no PEM certificate", t.System)
		}
		tr := s.client.Transport.(*http.Transport)
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return s.Check(ctx)
}

// Close drops idle connections.
func (s *ES) Close() error {
	if s.client != nil {
		s.client.CloseIdleConnections()
	}
	return nil
}

// Check GETs / (verifies URL + credential).
func (s *ES) Check(ctx context.Context) error {
	_, err := s.req(ctx, http.MethodGet, "/", nil, nil)
	return err
}

func (s *ES) auth(ctx context.Context, req *http.Request) error {
	if err := httpc.ApplyAuth(ctx, s.t, req); err != nil {
		return err
	}
	if s.t.Has(string(s.t.Mode)+".api_key") || s.t.Has("api_key") {
		k, err := s.t.ModeString(ctx, "api_key")
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "ApiKey "+k)
	}
	return nil
}

// req sends one request and returns the bounded body of a 2xx response.
func (s *ES) req(ctx context.Context, method, path string, q url.Values, body []byte) ([]byte, error) {
	if s.t.Mode != connector.ModeWrite && !readAllowed(method, path) {
		return nil, errs.New(errs.PolicyBlocked, "read connection refuses %s %s", method, path).WithHint("use tmvp write for mutations")
	}
	u := *s.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = q.Encode()
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	var req *http.Request
	var err error
	if rd != nil {
		req, err = http.NewRequestWithContext(ctx, method, u.String(), rd)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, u.String(), nil)
	}
	if err != nil {
		return nil, errs.New(errs.Usage, "cannot build request")
	}
	req.Header.Set("User-Agent", "tmvp")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := s.auth(ctx, req); err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, httpc.NetErr(ctx, err)
	}
	defer resp.Body.Close()
	b, err := httpc.ReadBounded(resp.Body, s.t.Limits.Bytes)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errs.AuthError(fmt.Errorf("HTTP 401"))
	case resp.StatusCode == http.StatusForbidden:
		return nil, errs.New(errs.PolicyBlocked, "HTTP 403: %s", esReason(b)).WithHint("the credential lacks the privilege for this action")
	case resp.StatusCode == http.StatusNotFound && method != http.MethodGet:
		return nil, errs.New(errs.NotFound, "HTTP 404: %s", esReason(b))
	case resp.StatusCode == http.StatusNotFound && !bytes.Contains(b, []byte(`"found"`)):
		return nil, errs.New(errs.NotFound, "HTTP 404: %s", esReason(b))
	case resp.StatusCode == http.StatusBadRequest:
		return nil, errs.New(errs.Usage, "HTTP 400: %s", esReason(b))
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusGatewayTimeout:
		return nil, errs.New(errs.Timeout, "HTTP %d: %s", resp.StatusCode, esReason(b))
	case resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound:
		return nil, errs.New(errs.Runtime, "HTTP %d: %s", resp.StatusCode, esReason(b))
	}
	return b, nil
}

func readAllowed(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	case http.MethodPost:
		segs := strings.Split(strings.Trim(path, "/"), "/")
		return readPOST[segs[len(segs)-1]] && (segs[len(segs)-1] != "query" || len(segs) > 1 && segs[len(segs)-2] == "_validate")
	}
	return false
}

// esReason extracts error.type/reason, capped; never echoes request data.
func esReason(b []byte) string {
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != nil {
		switch x := e.Error.(type) {
		case string:
			return trunc(x)
		case map[string]any:
			if rc, ok := x["root_cause"].([]any); ok && len(rc) > 0 {
				if m, ok := rc[0].(map[string]any); ok {
					return trunc(fmt.Sprintf("%v: %v", m["type"], m["reason"]))
				}
			}
			return trunc(fmt.Sprintf("%v: %v", x["type"], x["reason"]))
		}
	}
	return trunc(strings.TrimSpace(string(b)))
}

func trunc(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

func ignoreLimit(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}

func esc(s string) string { return url.PathEscape(s) }

// body resolves '<json>' or @saved (queries/<name>.json) and its front-matter index.
func (s *ES) body(arg string) (string, string, error) {
	if !strings.HasPrefix(arg, "@") {
		return arg, "", nil
	}
	sv, err := s.t.LoadSaved("queries", arg, ".json")
	if err != nil {
		return "", "", errs.Wrap(errs.NotFound, err, "")
	}
	var fm struct {
		Index string `yaml:"index"`
	}
	_ = yaml.Unmarshal([]byte(sv.FrontMatter), &fm)
	return strings.TrimSpace(sv.Body), fm.Index, nil
}

func (s *ES) limit(c connector.Call) int {
	if c.Limit > 0 {
		return c.Limit
	}
	return s.t.Limits.Rows
}

// Do runs one verb.
func (s *ES) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := s.Guard(c); err != nil {
		return err
	}
	return ignoreLimit(s.do(ctx, c, e))
}

func (s *ES) do(ctx context.Context, c connector.Call, e connector.Emit) error {
	switch c.Verb {
	case "ls":
		return s.ls(ctx, c, e)
	case "health":
		return s.getRow(ctx, "/_cluster/health", nil, e)
	case "indices":
		p := "/_cat/indices"
		if len(c.Args) > 0 {
			p += "/" + esc(c.Args[0])
		}
		return s.cat(ctx, p, e)
	case "cat":
		p := "/_cat/" + c.Args[0]
		if len(c.Args) > 1 {
			p += "/" + esc(c.Args[1])
		}
		return s.cat(ctx, p, e)
	case "mapping":
		return s.mapping(ctx, c.Args[0], e)
	case "search", "count":
		return s.search(ctx, c, e)
	case "get":
		b, err := s.req(ctx, http.MethodGet, "/"+esc(c.Args[0])+"/_doc/"+esc(c.Args[1]), nil, nil)
		if err != nil {
			return err
		}
		var d map[string]any
		if err := json.Unmarshal(b, &d); err != nil {
			return errs.Wrap(errs.Runtime, err, "decode document")
		}
		if f, _ := d["found"].(bool); !f {
			return errs.New(errs.NotFound, "document %s/%s not found", c.Args[0], c.Args[1])
		}
		return e.Row(hitRow(d))
	case "index":
		return s.index(ctx, c, e)
	case "update":
		body := c.Args[2]
		var m map[string]any
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			return errs.New(errs.Usage, "body is not a JSON object")
		}
		if _, ok := m["doc"]; !ok {
			if _, ok := m["script"]; !ok {
				m = map[string]any{"doc": m}
			}
		}
		nb, _ := json.Marshal(m)
		if c.Preview {
			return e.Row(connector.Row{"preview": true, "index": c.Args[0], "id": c.Args[1], "body": string(nb)})
		}
		return s.writeRow(ctx, http.MethodPost, "/"+esc(c.Args[0])+"/_update/"+esc(c.Args[1]), nil, nb, e)
	case "delete":
		if c.Preview {
			return s.do(ctx, connector.Call{Verb: "get", Args: c.Args[:2]}, e)
		}
		return s.writeRow(ctx, http.MethodDelete, "/"+esc(c.Args[0])+"/_doc/"+esc(c.Args[1]), nil, nil, e)
	case "update-by-query", "delete-by-query":
		body, _, err := s.body(c.Args[1])
		if err != nil {
			return err
		}
		if err := checkByQuery(c, body); err != nil {
			return err
		}
		if c.Preview {
			return s.countFor(ctx, c.Args[0], body, e)
		}
		ep := "/_update_by_query"
		if c.Verb == "delete-by-query" {
			ep = "/_delete_by_query"
		}
		return s.writeRow(ctx, http.MethodPost, "/"+esc(c.Args[0])+ep, url.Values{"conflicts": {"proceed"}}, []byte(body), e)
	case "delete-index":
		if c.Preview {
			return s.cat(ctx, "/_cat/indices/"+esc(c.Args[0]), e)
		}
		return s.writeRow(ctx, http.MethodDelete, "/"+esc(c.Args[0]), nil, nil, e)
	case "reindex":
		body, _, err := s.body(c.Args[0])
		if err != nil {
			return err
		}
		if !json.Valid([]byte(body)) {
			return errs.New(errs.Usage, "reindex body is not JSON")
		}
		if c.Preview {
			return e.Row(connector.Row{"preview": true, "body": body})
		}
		return s.writeRow(ctx, http.MethodPost, "/_reindex", url.Values{"wait_for_completion": {"true"}}, []byte(body), e)
	}
	return errs.New(errs.Usage, "%s has no verb %q", s.typ, c.Verb)
}

func (s *ES) ls(ctx context.Context, c connector.Call, e connector.Emit) error {
	filter := ""
	if len(c.Args) > 0 {
		filter = c.Args[0]
	}
	var idx []map[string]any
	if err := s.getJSON(ctx, "/_cat/indices", url.Values{"format": {"json"}, "h": {"index,health,docs.count,store.size"}, "s": {"index"}}, &idx); err != nil {
		return err
	}
	for _, r := range idx {
		n := fmt.Sprint(r["index"])
		if strings.HasPrefix(n, ".") || filter != "" && !strings.Contains(n, filter) {
			continue
		}
		if err := e.Row(connector.Row{"kind": "index", "name": n, "health": r["health"], "docs": r["docs.count"], "size": r["store.size"]}); err != nil {
			return err
		}
	}
	var als []map[string]any
	if err := s.getJSON(ctx, "/_cat/aliases", url.Values{"format": {"json"}, "h": {"alias,index"}, "s": {"alias"}}, &als); err != nil {
		return err
	}
	for _, r := range als {
		n := fmt.Sprint(r["alias"])
		if strings.HasPrefix(n, ".") || filter != "" && !strings.Contains(n, filter) {
			continue
		}
		if err := e.Row(connector.Row{"kind": "alias", "name": n, "index": r["index"]}); err != nil {
			return err
		}
	}
	return s.t.EmitSaved(e, "queries", ".json")
}

func (s *ES) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	b, err := s.req(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errs.Wrap(errs.Runtime, err, "decode %s", path)
	}
	return nil
}

func (s *ES) getRow(ctx context.Context, path string, q url.Values, e connector.Emit) error {
	var m map[string]any
	if err := s.getJSON(ctx, path, q, &m); err != nil {
		return err
	}
	return e.Row(m)
}

func (s *ES) cat(ctx context.Context, path string, e connector.Emit) error {
	var rows []map[string]any
	if err := s.getJSON(ctx, path, url.Values{"format": {"json"}}, &rows); err != nil {
		return err
	}
	for _, r := range rows {
		if err := e.Row(r); err != nil {
			return err
		}
	}
	return nil
}

func (s *ES) writeRow(ctx context.Context, method, path string, q url.Values, body []byte, e connector.Emit) error {
	b, err := s.req(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return errs.Wrap(errs.Runtime, err, "decode response")
	}
	if f, ok := m["failures"].([]any); ok && len(f) == 0 {
		delete(m, "failures")
	}
	if f, ok := m["result"].(string); ok && f == "not_found" {
		return errs.New(errs.NotFound, "document not found")
	}
	delete(m, "_shards")
	return e.Row(flatten(m))
}

func (s *ES) mapping(ctx context.Context, index string, e connector.Emit) error {
	var m map[string]struct {
		Mappings struct {
			Properties map[string]any `json:"properties"`
		} `json:"mappings"`
	}
	if err := s.getJSON(ctx, "/"+esc(index)+"/_mapping", nil, &m); err != nil {
		return err
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := walkProps(n, "", m[n].Mappings.Properties, e); err != nil {
			return err
		}
	}
	return nil
}

func walkProps(index, prefix string, props map[string]any, e connector.Emit) error {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p, _ := props[k].(map[string]any)
		name := prefix + k
		typ, _ := p["type"].(string)
		if sub, ok := p["properties"].(map[string]any); ok {
			if typ == "" {
				typ = "object"
			}
			if err := e.Row(connector.Row{"index": index, "field": name, "type": typ}); err != nil {
				return err
			}
			if err := walkProps(index, name+".", sub, e); err != nil {
				return err
			}
			continue
		}
		if err := e.Row(connector.Row{"index": index, "field": name, "type": typ}); err != nil {
			return err
		}
	}
	return nil
}

// resolve turns search/count args into index, body object and URL params.
func (s *ES) resolve(c connector.Call) (string, map[string]any, url.Values, error) {
	index, bodyArg := c.Args[0], ""
	if len(c.Args) > 1 {
		bodyArg = c.Args[1]
	}
	if strings.HasPrefix(index, "@") { // bare @saved: index from front-matter (or the next arg)
		b, fmIndex, err := s.body(index)
		if err != nil {
			return "", nil, nil, err
		}
		bodyArg, index = b, fmIndex
		if len(c.Args) > 1 {
			index = c.Args[1]
		}
		if index == "" {
			return "", nil, nil, errs.New(errs.Usage, "saved query %s has no index: front-matter", c.Args[0]).WithHint("add `index: logs-*` to its front-matter or pass the index")
		}
	} else if bodyArg != "" {
		b, _, err := s.body(bodyArg)
		if err != nil {
			return "", nil, nil, err
		}
		bodyArg = b
	}
	m := map[string]any{}
	if strings.TrimSpace(bodyArg) != "" {
		if err := json.Unmarshal([]byte(bodyArg), &m); err != nil {
			return "", nil, nil, errs.New(errs.Usage, "body is not a JSON object: %v", err)
		}
	}
	q := url.Values{}
	if lq := c.Flags["q"]; lq != "" {
		q.Set("q", lq)
	}
	return index, m, q, nil
}

func (s *ES) search(ctx context.Context, c connector.Call, e connector.Emit) error {
	index, m, q, err := s.resolve(c)
	if err != nil {
		return err
	}
	if c.Verb == "count" {
		for k := range m {
			if k != "query" {
				delete(m, k)
			}
		}
		b, _ := json.Marshal(m)
		if len(m) == 0 {
			b = nil
		}
		var out struct {
			Count int64 `json:"count"`
		}
		rb, err := s.req(ctx, http.MethodPost, "/"+esc(index)+"/_count", q, b)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(rb, &out); err != nil {
			return errs.Wrap(errs.Runtime, err, "decode count")
		}
		return e.Row(connector.Row{"index": index, "count": out.Count})
	}
	limit := s.limit(c)
	size := min(10, limit) // ES default page; --limit asks for more
	if c.Limit > 0 {
		size = c.Limit
	}
	if sz, ok := m["size"].(float64); ok {
		size = min(int(sz), limit)
	}
	m["size"] = size
	if from, ok := m["from"].(float64); ok && int(from)+size > 10000 {
		return errs.New(errs.GuardRejected, "from+size beyond 10000; use search_after")
	}
	if _, ok := m["timeout"]; !ok {
		m["timeout"] = strconv.FormatInt(connector.ServerTimeout(c, s.t.Limits).Milliseconds(), 10) + "ms"
	}
	b, _ := json.Marshal(m)
	rb, err := s.req(ctx, http.MethodPost, "/"+esc(index)+"/_search", q, b)
	if err != nil {
		return err
	}
	var res struct {
		TimedOut bool `json:"timed_out"`
		Hits     struct {
			Hits []map[string]any `json:"hits"`
		} `json:"hits"`
		Aggregations map[string]any `json:"aggregations"`
	}
	if err := json.Unmarshal(rb, &res); err != nil {
		return errs.Wrap(errs.Runtime, err, "decode search")
	}
	for _, h := range res.Hits.Hits {
		if err := e.Row(hitRow(h)); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(res.Aggregations))
	for n := range res.Aggregations {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := emitAgg(n, res.Aggregations[n], e); err != nil {
			return err
		}
	}
	if res.TimedOut {
		return e.Text("warning: search timed out on some shards; results are partial")
	}
	return nil
}

func emitAgg(name string, v any, e connector.Emit) error {
	m, _ := v.(map[string]any)
	if bs, ok := m["buckets"].([]any); ok {
		for _, b := range bs {
			bm, _ := b.(map[string]any)
			r := connector.Row{"aggregation": name}
			for k, x := range bm {
				if sub, ok := x.(map[string]any); ok {
					if val, ok := sub["value"]; ok {
						r[k] = val
						continue
					}
					js, _ := json.Marshal(sub)
					r[k] = string(js)
					continue
				}
				r[k] = x
			}
			if err := e.Row(r); err != nil {
				return err
			}
		}
		return nil
	}
	r := connector.Row{"aggregation": name}
	if val, ok := m["value"]; ok {
		r["value"] = val
	} else {
		js, _ := json.Marshal(m)
		r["result"] = string(js)
	}
	return e.Row(r)
}

// hitRow flattens one hit/document: _index, _id, _score + _source fields.
func hitRow(h map[string]any) connector.Row {
	r := connector.Row{"_index": h["_index"], "_id": h["_id"]}
	if sc, ok := h["_score"]; ok && sc != nil {
		r["_score"] = sc
	}
	if src, ok := h["_source"].(map[string]any); ok {
		for k, v := range flatten(src) {
			r[k] = v
		}
	}
	return r
}

// flatten turns nested objects into dotted keys; arrays/objects below
// depth 4 become JSON strings.
func flatten(m map[string]any) connector.Row {
	out := connector.Row{}
	var walk func(prefix string, v any, depth int)
	walk = func(prefix string, v any, depth int) {
		switch x := v.(type) {
		case map[string]any:
			if depth >= 4 {
				js, _ := json.Marshal(x)
				out[prefix] = string(js)
				return
			}
			for k, y := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, y, depth+1)
			}
		case []any:
			js, _ := json.Marshal(x)
			out[prefix] = string(js)
		default:
			out[prefix] = x
		}
	}
	walk("", m, 0)
	return out
}

func (s *ES) countFor(ctx context.Context, index, body string, e connector.Emit) error {
	var m map[string]any
	_ = json.Unmarshal([]byte(body), &m)
	q := map[string]any{}
	if v, ok := m["query"]; ok {
		q["query"] = v
	}
	b, _ := json.Marshal(q)
	rb, err := s.req(ctx, http.MethodPost, "/"+esc(index)+"/_count", nil, b)
	if err != nil {
		return err
	}
	var out struct {
		Count int64 `json:"count"`
	}
	_ = json.Unmarshal(rb, &out)
	return e.Row(connector.Row{"preview": true, "index": index, "would_affect": out.Count})
}

func (s *ES) index(ctx context.Context, c connector.Call, e connector.Emit) error {
	body, _, err := s.body(c.Args[1])
	if err != nil {
		return err
	}
	if !json.Valid([]byte(body)) {
		return errs.New(errs.Usage, "document is not JSON")
	}
	if c.Preview {
		return e.Row(connector.Row{"preview": true, "index": c.Args[0], "id": c.Flags["id"], "body": body})
	}
	if id := c.Flags["id"]; id != "" {
		return s.writeRow(ctx, http.MethodPut, "/"+esc(c.Args[0])+"/_doc/"+esc(id), nil, []byte(body), e)
	}
	return s.writeRow(ctx, http.MethodPost, "/"+esc(c.Args[0])+"/_doc", nil, []byte(body), e)
}
