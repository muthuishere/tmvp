package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
)

// cmkv: per-env data for the module tests. cfg returns key/value rows,
// ver one version row, logs text lines; "down" fails in prod.
type cmkv struct{ env string }

var kvData = map[string]map[string]string{
	"dev":     {"payfast.url": "https://pay", "smtp.host": "mail", "banner": "d"},
	"staging": {"payfast.url": "https://pay", "smtp.host": "mail", "banner": "s"},
	"prod":    {"payfast.url": "https://pay-old", "smtp.host": "mail ", "banner": "p"},
}
var kvVer = map[string]int{"dev": 3, "staging": 3, "prod": 2}

func init() {
	testOnlyTypes["cmkv"] = true
	connector.Register("cmkv", func() connector.Connector { return &cmkv{} })
}

func (*cmkv) Type() string { return "cmkv" }
func (*cmkv) Verbs() []connector.Verb {
	return []connector.Verb{{Name: "cfg", Mode: connector.Read}, {Name: "ver", Mode: connector.Read},
		{Name: "logs", Mode: connector.Read}, {Name: "down", Mode: connector.Read}}
}
func (k *cmkv) Open(_ context.Context, t *connector.Target) error { k.env = t.Env; return nil }
func (*cmkv) Close() error                                        { return nil }
func (*cmkv) Estimate(context.Context, connector.Call) (connector.Estimate, error) {
	return connector.Estimate{}, connector.ErrNoEstimate
}
func (k *cmkv) Do(_ context.Context, c connector.Call, e connector.Emit) error {
	switch c.Verb {
	case "cfg":
		for _, key := range []string{"banner", "payfast.url", "smtp.host"} {
			if err := e.Row(connector.Row{"key": key, "value": kvData[k.env][key]}); err != nil {
				return err
			}
		}
	case "ver":
		return e.Row(connector.Row{"version": kvVer[k.env]})
	case "logs":
		_ = e.Text("GET /a 200")
		if k.env == "prod" {
			_ = e.Text("GET /refund 500")
			_ = e.Text("GET /refund 500 again")
		}
	case "down":
		if k.env == "prod" {
			return context.DeadlineExceeded
		}
		return e.Row(connector.Row{"ok": 1})
	}
	return nil
}

const kvCfg = `product: kvtest
default_env: dev
envs:
  dev:
    systems:
      kv: { type: cmkv }
  staging:
    systems:
      kv: { type: cmkv }
  prod:
    type: prod
    systems:
      kv: { type: cmkv }
`

func TestCodeModulesCheckClean(t *testing.T) {
	envSetup(t, kvCfg)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := codeEngine(cfg, codeFlags{maxSteps: 1e6, maxTime: 10 * time.Second, concurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	if d := eng.CheckModules(); len(d) != 0 {
		t.Fatalf("module diagnostics: %+v", d)
	}
	if n := len(eng.Registry.Modules()); n != 3 {
		t.Fatalf("want 3 modules, got %d", n)
	}
}

func TestModuleDrift(t *testing.T) {
	out := envSetup(t, kvCfg)
	code, env := codeRun(t, out, `
import { compareEnvs, oddOneOut, strayWhitespace } from "tmvp:drift"
const c = await compareEnvs("kv.cfg", {}, { key: "key", value: "value" })
const o = await oddOneOut("kv.cfg", {})
const f = await oddOneOut("kv.cfg", {}, { focus: "prod", envs: ["dev", "prod"] })
const w = await strayWhitespace("kv.cfg", {}, { key: "key", value: "value" })
export default { differs: c.differs.map(d => d.key), odd: o.odd.map(x => x.env + ":" + x.key + "=" + x.value),
  focus: f.odd.map(x => x.key), ws: w.found.map(x => x.env + ":" + x.key + "=" + x.value) }`)
	want := `{"differs":["banner","payfast.url","smtp.host"],"focus":["banner","payfast.url","smtp.host"],"odd":["prod:payfast.url=https://pay-old","prod:smtp.host=mail "],"ws":["prod:smtp.host=\"mail \""]}`
	if code != 0 || compact(env.Value) != want {
		t.Fatalf("code %d: %s\nwant %s", code, out, want)
	}
}

func TestModuleEvidence(t *testing.T) {
	out := envSetup(t, kvCfg)
	code, env := codeRun(t, out, `
import { gather, logHits, laggingEnvs } from "tmvp:evidence"
const g = await gather({ ver: { op: "kv.ver" }, logs: { op: "kv.logs", env: "prod" } })
const h = await logHits("kv.logs", {}, { match: " 500" })
const l = await laggingEnvs("kv.ver")
export default { g: [g.envs.prod.ver[0].version, g.envs.prod.logs.length, Object.keys(g.envs).length],
  hits: h.hits.prod.count, quiet: h.quiet, lag: l }`)
	want := `{"g":[2,3,3],"hits":2,"lag":{"behind":[{"env":"prod","missing":1,"value":2}],"current":["dev","staging"],"errors":[],"latest":3},"quiet":["dev","staging"]}`
	if code != 0 || compact(env.Value) != want {
		t.Fatalf("code %d: %s\nwant %s", code, out, want)
	}
}

func TestModuleHealth(t *testing.T) {
	out := envSetup(t, kvCfg)
	code, env := codeRun(t, out, `
import { failing } from "tmvp:health"
const r = await failing({ kv: { op: "kv.down" }, missing: { op: "nope.op" } })
export default { failing: r.failing.map(f => f.probe + "@" + (f.env || "")), ok: r.ok }`)
	if code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	v := compact(env.Value)
	if !strings.Contains(v, `"kv@prod"`) || !strings.Contains(v, `"missing@all"`) || !strings.Contains(v, `"ok":["kv@dev","kv@staging"]`) {
		t.Fatalf("got %s", v)
	}
}
