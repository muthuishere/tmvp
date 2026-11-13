package httpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// API is one named endpoint: from assets apis/<name>.md front-matter
//
//	---
//	method: GET
//	path: /api/users/{id}
//	description: one user
//	params: [id]            # or a map name: description
//	mutating: false         # default: true for anything but GET/HEAD
//	---
//
// or from the system's `openapi:` field (local path relative to the skill
// dir, a path on base_url, or an http(s) URL). Asset files win over OpenAPI.
type API struct {
	Name, Method, Path, Description, Source string
	Params                                  []string
	Mutating                                bool
}

type apiFront struct {
	Method      string    `yaml:"method"`
	Path        string    `yaml:"path"`
	Description string    `yaml:"description"`
	Params      yaml.Node `yaml:"params"`
	Mutating    *bool     `yaml:"mutating"`
}

func paramNames(n yaml.Node) []string {
	var out []string
	switch n.Kind {
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode {
				out = append(out, c.Value)
			} else if c.Kind == yaml.MappingNode && len(c.Content) >= 2 {
				for i := 0; i+1 < len(c.Content); i += 2 {
					if c.Content[i].Value == "name" {
						out = append(out, c.Content[i+1].Value)
					}
				}
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			out = append(out, n.Content[i].Value)
		}
	}
	return out
}

func readMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead }

// apis loads every defined API (assets first, then openapi), sorted by name.
// The openapi error (if any) is returned alongside what did load.
func (h *HTTP) apis(ctx context.Context) ([]*API, error) {
	saved, err := h.t.ListSaved("apis", ".md")
	if err != nil {
		return nil, errs.Wrap(errs.Config, err, "")
	}
	seen := map[string]bool{}
	var out []*API
	for _, s := range saved {
		var f apiFront
		if err := yaml.Unmarshal([]byte(s.FrontMatter), &f); err != nil {
			return nil, errs.Wrap(errs.Config, err, "api %s front-matter", s.Name)
		}
		if f.Path == "" {
			return nil, errs.New(errs.Config, "api %s: front-matter needs path:", s.Name)
		}
		m := strings.ToUpper(f.Method)
		if m == "" {
			m = http.MethodGet
		}
		a := &API{Name: s.Name, Method: m, Path: f.Path, Description: f.Description, Source: s.Path, Params: paramNames(f.Params), Mutating: !readMethod(m)}
		if f.Mutating != nil {
			a.Mutating = *f.Mutating || !readMethod(m)
		}
		seen[a.Name] = true
		out = append(out, a)
	}
	var oerr error
	if h.t.Has("openapi") {
		ops, err := h.openapi(ctx)
		if err != nil {
			oerr = err
		}
		for _, a := range ops {
			if !seen[a.Name] {
				seen[a.Name] = true
				out = append(out, a)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, oerr
}

type oaDoc struct {
	Paths map[string]map[string]struct {
		OperationID string `yaml:"operationId"`
		Summary     string `yaml:"summary"`
		Description string `yaml:"description"`
		Parameters  []struct {
			Name string `yaml:"name"`
			In   string `yaml:"in"`
		} `yaml:"parameters"`
	} `yaml:"paths"`
}

var nonName = regexp.MustCompile(`[^a-z0-9]+`)

func (h *HTTP) openapi(ctx context.Context) ([]*API, error) {
	src, err := h.t.String(ctx, "openapi")
	if err != nil {
		return nil, err
	}
	var b []byte
	switch {
	case strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://"):
		b, err = h.fetch(ctx, src, false)
	case strings.HasPrefix(src, "/"):
		b, err = h.fetch(ctx, src, true) // a path on base_url (with auth)
	default:
		p := src
		if !filepath.IsAbs(p) {
			p = filepath.Join(h.t.Dir, p)
		}
		b, err = os.ReadFile(p)
		if err != nil {
			err = errs.Wrap(errs.Config, err, "openapi file")
		}
	}
	if err != nil {
		return nil, err
	}
	var d oaDoc
	if err := yaml.Unmarshal(b, &d); err != nil { // YAML is a superset of JSON
		return nil, errs.Wrap(errs.Config, err, "openapi: parse")
	}
	var out []*API
	for p, ops := range d.Paths {
		for m, op := range ops {
			method := strings.ToUpper(m)
			switch method {
			case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE":
			default:
				continue // parameters, servers, ...
			}
			name := op.OperationID
			if name == "" {
				name = strings.Trim(nonName.ReplaceAllString(strings.ToLower(method+" "+p), "-"), "-")
			}
			desc := op.Summary
			if desc == "" {
				desc = op.Description
			}
			a := &API{Name: name, Method: method, Path: p, Description: desc, Source: "openapi", Mutating: !readMethod(method)}
			for _, pr := range op.Parameters {
				if pr.In == "path" || pr.In == "query" {
					a.Params = append(a.Params, pr.Name)
				}
			}
			out = append(out, a)
		}
	}
	return out, nil
}

func (h *HTTP) fetch(ctx context.Context, src string, onBase bool) ([]byte, error) {
	if onBase {
		resp, body, err := h.do(ctx, http.MethodGet, src, nil, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, statusErr(resp.StatusCode, body)
		}
		return body, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, errs.New(errs.Config, "openapi: bad URL")
	}
	resp, err := h.client.Do(req) // no credentials: a foreign host never gets them
	if err != nil {
		return nil, NetErr(ctx, err)
	}
	defer resp.Body.Close()
	body, err := ReadBounded(resp.Body, h.t.Limits.Bytes)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, errs.New(errs.Runtime, "openapi: HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func (h *HTTP) emitAPIs(ctx context.Context, filter string, e connector.Emit) error {
	list, oerr := h.apis(ctx)
	if list == nil && oerr != nil {
		return oerr
	}
	for _, a := range list {
		if filter != "" && !strings.Contains(a.Name, filter) && !strings.Contains(a.Path, filter) {
			continue
		}
		if err := e.Row(connector.Row{"kind": "api", "name": "@" + a.Name, "method": a.Method, "path": a.Path,
			"mutating": a.Mutating, "description": a.Description, "params": strings.Join(a.Params, ","), "source": a.Source}); err != nil {
			return ignoreLimit(err)
		}
	}
	return oerr
}

// call runs a defined API: call <name> [k=v ...]. {k} fills path params; the
// rest go to the query string (GET/HEAD/DELETE) or a JSON body. Mutating APIs
// run only through tmvp write.
func (h *HTTP) call(ctx context.Context, c connector.Call, e connector.Emit) error {
	name := strings.TrimPrefix(c.Args[0], "@")
	list, oerr := h.apis(ctx)
	var api *API
	for _, a := range list {
		if a.Name == name {
			api = a
		}
	}
	if api == nil {
		if oerr != nil {
			return oerr
		}
		return errs.New(errs.NotFound, "no api %q", name).WithHint("tmvp <env> " + h.t.System + " apis")
	}
	if api.Mutating && !c.Write {
		return errs.New(errs.PolicyBlocked, "api %s is mutating (%s)", name, api.Method).
			WithHint(`tmvp write <env> ` + h.t.System + " call " + name + ` ... --reason "why"`)
	}
	path := api.Path
	rest := map[string]string{}
	for _, kv := range c.Args[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return errs.New(errs.Usage, "call args are key=value, got %q", kv)
		}
		if strings.Contains(path, "{"+k+"}") {
			path = strings.ReplaceAll(path, "{"+k+"}", url.PathEscape(v))
		} else {
			rest[k] = v
		}
	}
	if i := strings.Index(path, "{"); i >= 0 {
		return errs.New(errs.Usage, "missing path param in %s", path).WithHint("call " + name + " <param>=<value>")
	}
	flags := map[string]string{}
	for k, v := range c.Flags {
		flags[k] = v
	}
	var payload []byte
	if readMethod(api.Method) || api.Method == http.MethodDelete {
		for k, v := range rest {
			flags["q."+k] = v
		}
	} else if len(rest) > 0 {
		payload, _ = json.Marshal(rest)
	}
	if c.Preview {
		u, err := h.resolve(path, flags)
		if err != nil {
			return err
		}
		return ignoreLimit(e.Row(connector.Row{"preview": true, "api": name, "method": api.Method, "url": u.Redacted(), "body_bytes": len(payload)}))
	}
	resp, body, err := h.do(ctx, api.Method, path, flags, payload)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return statusErr(resp.StatusCode, body)
	}
	if api.Mutating {
		if err := e.Row(connector.Row{"status": resp.StatusCode}); err != nil {
			return ignoreLimit(err)
		}
	}
	return EmitBody(e, resp.Header.Get("Content-Type"), body)
}
