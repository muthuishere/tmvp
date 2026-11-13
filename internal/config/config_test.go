package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `
version: 1
product: demo
vars: { region: ap-south-1, namespace: demo }
secrets:
  ssh_key: "$(sec get DEMO_SSH --print)"
  shared: "$(echo FAKE_SHARED)"
envs:
  dev:
    destructive: true
    vars: { domain: dev.example.com }
    secrets: { db_ro: "$(sec get DEMO_DEV_DB --print)" }
    systems:
      db: { type: postgres, read: { dsn: "${secrets.db_ro}" }, labels: { role: primary } }
  prod:
    vars: { domain: example.com }
    secrets:
      db_ro: "$(sec get DEMO_PROD_DB --print)"
      kubeconfig: "$file(node fetch.js --out ${out})"
    systems:
      bastion:   { type: ssh, host: 10.0.0.5, user: deploy, key: "${secrets.ssh_key}" }
      db-tunnel: { type: portforward, via: bastion, remote: "db.internal:5432" }
      db:        { type: postgres, via: db-tunnel, read: { dsn: "${secrets.db_ro}" }, labels: { role: primary } }
      analytics: { type: postgres, read: { dsn: "$(sec get DEMO_ANALYTICS --print)" }, labels: { role: reporting } }
      cluster:   { type: k8s, kubeconfig: "${file:secrets.kubeconfig}", namespace: "${vars.namespace}" }
      admin:
        type: web
        base_url: "https://admin.${vars.domain}"
        note: "cost $$5 in ${REGION_X:-eu}"
        login: { manual: "gcloud auth login", check: "gcloud auth print-access-token" }
        destructive: false
`

func mustParse(t *testing.T, s string) *Config {
	t.Helper()
	c, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseAndOrder(t *testing.T) {
	c := mustParse(t, sample)
	if c.Product != "demo" || strings.Join(c.EnvOrder, ",") != "dev,prod" {
		t.Fatalf("got %q %v", c.Product, c.EnvOrder)
	}
	p := c.Envs["prod"]
	if strings.Join(p.SystemOrder, ",") != "bastion,db-tunnel,db,analytics,cluster,admin" {
		t.Fatalf("order %v", p.SystemOrder)
	}
	if got := p.Systems["db"].Via; len(got) != 1 || got[0] != "db-tunnel" {
		t.Fatalf("via %v", got)
	}
	if p.Systems["admin"].Login.Manual != "gcloud auth login" {
		t.Fatal("login not parsed")
	}
	if c.IsDestructive("prod", "admin") || !c.IsDestructive("dev", "db") {
		t.Fatal("destructive flags")
	}
	if d := c.Envs["prod"].Secrets["kubeconfig"]; !d.File || d.Command != "node fetch.js --out ${out}" {
		t.Fatalf("file secret %+v", d)
	}
}

func TestInlineCommandBecomesAnonymousSecret(t *testing.T) {
	c := mustParse(t, sample)
	v, _ := c.System("prod", "analytics").Get("read.dsn")
	if v != "${secrets._analytics.read.dsn}" {
		t.Fatalf("rewritten = %v", v)
	}
	d := c.LookupSecret("prod", "_analytics.read.dsn")
	if d == nil || !d.Anonymous || d.Command != "sec get DEMO_ANALYTICS --print" {
		t.Fatalf("anon def %+v", d)
	}
}

func TestExpandField(t *testing.T) {
	c := mustParse(t, sample)
	Getenv = func(k string) (string, bool) {
		if k == "SET_ME" {
			return "yes", true
		}
		return "", false
	}
	defer func() { Getenv = os.LookupEnv }()
	calls := 0
	sec := func(d *SecretDef) (string, error) { calls++; return "FAKE_" + d.Key(), nil }
	got, err := c.ExpandField("prod", "${vars.domain}/${vars.region}/${secrets.db_ro}/${user.name}/$$x/${SET_ME}", sec)
	if err != nil {
		t.Fatal(err)
	}
	if got != "example.com/ap-south-1/FAKE_prod/db_ro/${user.name}/$x/yes" || calls != 1 {
		t.Fatalf("got %q calls %d", got, calls)
	}
	got, _ = c.ExpandField("prod", "${secrets.shared}", sec)
	if got != "FAKE_/shared" {
		t.Fatalf("top-level secret: %q", got)
	}
	v, _ := c.System("prod", "admin").Get("note")
	got, err = c.ExpandField("prod", v.(string), nil)
	if err != nil || got != "cost $5 in eu" {
		t.Fatalf("default/escape: %q %v", got, err)
	}
	if _, err := c.ExpandField("prod", "${NOPE_UNSET}", nil); err == nil {
		t.Fatal("unset env var must error")
	}
	if _, err := c.ExpandField("prod", "${file:secrets.kubeconfig}", sec); err == nil {
		t.Fatal("file ref via ExpandField must error")
	}
	if n, ok := FileRef("${file:secrets.kubeconfig}"); !ok || n != "kubeconfig" {
		t.Fatal("FileRef")
	}
}

func TestInterpolationAfterParseCannotChangeStructure(t *testing.T) {
	c := mustParse(t, sample)
	evil := "x\n  injected: true\n"
	got, err := c.ExpandField("prod", "${secrets.db_ro}", func(*SecretDef) (string, error) { return evil, nil })
	if err != nil || got != evil {
		t.Fatal("value must be substituted verbatim")
	}
	if _, ok := c.System("prod", "db").Get("injected"); ok {
		t.Fatal("structure changed")
	}
}

func TestSelectors(t *testing.T) {
	c := mustParse(t, sample)
	cases := []struct {
		sel    Selector
		labels string
		want   string
	}{
		{Selector{Env: "all", System: "db"}, "", "dev.db prod.db"},
		{Selector{Env: "prod", System: "db"}, "", "prod.db"},
		{Selector{Env: "dev,prod", System: "analytics"}, "", "dev.analytics(missing) prod.analytics"},
		{ParseSelector("prod.*"), "role=primary", "prod.db"},
		{ParseSelector("*.db"), "", "dev.db prod.db"},
		{Selector{Env: "prod", System: "*"}, "role in (primary,reporting)", "prod.db prod.analytics"},
		{Selector{Env: "prod", System: "*"}, "role notin (primary),role", "prod.analytics"},
		{Selector{Env: "prod", System: "*"}, "!role", "prod.bastion prod.db-tunnel prod.cluster prod.admin"},
	}
	for _, tc := range cases {
		ls, err := ParseLabels(tc.labels)
		if err != nil {
			t.Fatal(err)
		}
		tc.sel.Labels = ls
		ts, err := c.Select(tc.sel)
		if err != nil {
			t.Fatalf("%+v: %v", tc.sel, err)
		}
		var got []string
		for _, x := range ts {
			s := x.Env + "." + x.Name
			if x.System == nil {
				s += "(missing)"
			}
			got = append(got, s)
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%+v %q: got %v want %s", tc.sel, tc.labels, got, tc.want)
		}
	}
	if _, err := c.Select(Selector{Env: "nope"}); err == nil {
		t.Fatal("unknown env must error")
	}
	if _, err := c.Select(Selector{Env: "all", System: "nope"}); err == nil {
		t.Fatal("unknown system must error")
	}
	if !c.IsEnvSelector("prod,dev") || c.IsEnvSelector("db") {
		t.Fatal("IsEnvSelector")
	}
}

func TestValidate(t *testing.T) {
	c := mustParse(t, sample)
	if is := errorsOf(c.Validate(ValidateOptions{})); len(is) != 0 {
		t.Fatalf("sample should validate: %v", is)
	}
	bad := `
version: 1
product: demo
secrets: { lit: "FAKE_LITERAL_VALUE", envok: "${SOME_ENV}" }
envs:
  db:
    systems:
      db: { type: postgres, dsn: "postgres://u:FAKE_PW@h/db" }
  query:
    systems:
      a: { type: ssh, via: b, password: "FAKE_PLAIN" }
      b: { type: ssh, via: a }
      c: { type: postgres, url: "https://u:FAKE_PW@host/x", via: zz, x: "${vars.nope}" }
      write: { type: postgres }
`
	c = mustParse(t, bad)
	is := c.Validate(ValidateOptions{
		KnownType: func(t string) bool { return t == "postgres" || t == "ssh" },
		Verbs: func(t string) []string {
			if t == "postgres" {
				return []string{"query", "describe"}
			}
			return nil
		},
	})
	all := ""
	for _, i := range errorsOf(is) {
		all += i.String() + "\n"
	}
	for _, want := range []string{
		"secrets.lit: literal secret value",
		`env "db" collides with a system name`,
		`env "query" collides with a postgres verb`,
		"literal value in secret field dsn",
		"literal value in secret field password",
		"credentials embedded in URL",
		"via cycle",
		`via "zz": no such system`,
		`unknown var "nope"`,
		`collides with the tmvp command "write"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "envok") {
		t.Errorf("env-backed secret should be allowed:\n%s", all)
	}
}

func errorsOf(is []Issue) []Issue {
	var out []Issue
	for _, i := range is {
		if i.Level == "error" {
			out = append(out, i)
		}
	}
	return out
}

func TestViaChain(t *testing.T) {
	c := mustParse(t, sample)
	ch, err := c.ViaChain("prod", "db")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 2 || ch[0].Name != "bastion" || ch[1].Name != "db-tunnel" {
		t.Fatalf("chain %v", ch)
	}
}

func TestShowResolvedMasksSecrets(t *testing.T) {
	c := mustParse(t, sample)
	out, err := c.ShowResolved()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<secret:db_ro>", "<file:secret:kubeconfig>", "https://admin.example.com", "<secret:_analytics.read.dsn>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
}

func TestFindLookupOrder(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(repo, "a", "b")
	must := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("product: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
	_ = os.MkdirAll(sub, 0o700)
	o := FindOptions{Cwd: sub, Home: home}
	if _, err := Find(o); err == nil {
		t.Fatal("expected not found")
	}
	must(filepath.Join(home, ".tmvp", "config.yaml"))
	expect := func(want string) {
		t.Helper()
		got, err := Find(o)
		if err != nil || got != want {
			t.Fatalf("got %q %v want %q", got, err, want)
		}
	}
	expect(filepath.Join(home, ".tmvp", "config.yaml"))
	o.Product = "x"
	must(filepath.Join(home, ".tmvp", "x", "config.yaml"))
	expect(filepath.Join(home, ".tmvp", "x", "config.yaml"))
	must(filepath.Join(repo, ".agents", "skills", "x-tmvp", "config.yaml"))
	expect(filepath.Join(repo, ".agents", "skills", "x-tmvp", "config.yaml"))
	must(filepath.Join(repo, ".tmvp", "config.yaml"))
	expect(filepath.Join(repo, ".tmvp", "config.yaml"))
	o.SkillPath = filepath.Join(repo, ".agents", "skills", "x-tmvp")
	expect(filepath.Join(repo, ".agents", "skills", "x-tmvp", "config.yaml"))
	o.ConfigPath = filepath.Join(home, ".tmvp", "config.yaml")
	expect(filepath.Join(home, ".tmvp", "config.yaml"))
}

func TestShowResolvedNeverPrintsEnvSecrets(t *testing.T) {
	t.Setenv("TMVP_T_PLAIN", "FAKE_SECRET_123")
	t.Setenv("TMVP_T_DB_PASSWORD", "FAKE_SECRET_123")
	t.Setenv("TMVP_T_HOST", "db.example.test")
	c := mustParse(t, `
version: 1
product: p
vars: { pw: "${TMVP_T_PLAIN}" }
envs:
  dev:
    systems:
      api:
        type: http
        base_url: "https://u:${TMVP_T_PLAIN}@${TMVP_T_HOST}/x"
        token: "${TMVP_T_PLAIN}"
        password: "${vars.pw}"
        note: "${TMVP_T_DB_PASSWORD}"
        read: { dsn: "postgres://u:${vars.pw}@h/db" }
`)
	out, err := c.ShowResolved()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "FAKE_SECRET_123") {
		t.Fatalf("secret value printed:\n%s", out)
	}
	if !strings.Contains(out, "db.example.test") || !strings.Contains(out, "<env:TMVP_T_DB_PASSWORD>") {
		t.Fatalf("non-secret env expansion or masking missing:\n%s", out)
	}
}

func TestAssetRoots(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte("version: 1\nproduct: x\nassets: [assets, ../other/ops]\nenvs: {}\n"), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	rs := c.AssetRoots()
	if len(rs) != 2 || rs[0] != filepath.Join(dir, "assets") || rs[1] != filepath.Join(filepath.Dir(dir), "other", "ops") {
		t.Fatalf("roots %v", rs)
	}
	var bad int
	for _, is := range c.Validate(ValidateOptions{}) {
		if strings.HasPrefix(is.Path, "assets.") && is.Level == "error" {
			bad++
		}
	}
	if bad != 2 {
		t.Fatalf("missing asset paths not flagged (%d)", bad)
	}
	c.Assets = nil
	if rs := c.AssetRoots(); len(rs) != 1 || rs[0] != filepath.Join(dir, "assets") {
		t.Fatalf("default %v", rs)
	}
}
