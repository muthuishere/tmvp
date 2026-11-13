package skill

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/connector"
)

var update = flag.Bool("update", false, "rewrite golden files")

func scaffold(t *testing.T, systems string) string {
	t.Helper()
	specs, err := ParseSystems(systems)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := New(Options{Product: "reqsume", Path: t.TempDir(), Envs: []string{"dev", "staging", "prod"}, Systems: specs})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// tree returns "relpath\n---\ncontent" for every file, sorted.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		rel, _ := filepath.Rel(dir, f)
		c, _ := os.ReadFile(f)
		b.WriteString("=== " + filepath.ToSlash(rel) + "\n" + string(c))
	}
	// the temp dir appears in SKILL.md (--skill <dir>); normalise it
	return strings.ReplaceAll(b.String(), filepath.ToSlash(dir), "<SKILLDIR>")
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		_ = os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s mismatch (go test ./internal/skill -update to accept)\n--- got\n%s", name, got)
	}
}

func validate(t *testing.T, path string) []config.Issue {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var bad []config.Issue
	for _, is := range cfg.Validate(config.ValidateOptions{}) {
		if is.Level == "error" {
			bad = append(bad, is)
		}
	}
	return bad
}

const allTypes = "db:postgres,my:mysql,docs:mongo,cache:redis,bus:nats,events:kafka,cluster:k8s,bastion:ssh," +
	"db-tunnel:portforward,cloud:gcp,aws:aws,azure:az,admin:web,api:http,prom:prometheus,infra:git,odd:custom"

func TestScaffoldGolden(t *testing.T) {
	dir := scaffold(t, "db:postgres,cluster:k8s,admin:web")
	if filepath.Base(dir) != "reqsume-tmvp" {
		t.Fatalf("dir %s", dir)
	}
	golden(t, "scaffold.golden", tree(t, dir))
}

func TestScaffoldValidates(t *testing.T) {
	dir := scaffold(t, allTypes)
	if bad := validate(t, filepath.Join(dir, "config.yaml")); len(bad) > 0 {
		t.Fatalf("validate errors: %v", bad)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if !strings.Contains(string(b), "# SECRET: read-only DSN") {
		t.Error("missing per-slot comment")
	}
	for _, f := range []string{"assets/db/queries/.gitkeep", "assets/cluster/manifests/.gitkeep",
		"assets/admin/pages/.gitkeep", "assets/api/apis/.gitkeep", "assets/bus/notes/.gitkeep", "SKILL.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Error(err)
		}
	}
}

func TestScaffoldRefusesOverwrite(t *testing.T) {
	dir := scaffold(t, "db:postgres")
	if _, err := New(Options{Product: "reqsume", Path: filepath.Dir(dir)}); err == nil {
		t.Fatal("expected refusal")
	}
}

func TestScaffoldBadInput(t *testing.T) {
	for _, s := range []string{"db", "db:postgres,db:mysql", "Bad Name:postgres"} {
		if _, err := ParseSystems(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if _, err := New(Options{Product: "../x", Path: t.TempDir()}); err == nil {
		t.Error("bad product accepted")
	}
}

func TestSkillAddPreservesComments(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	orig := `# top comment kept
version: 1
product: reqsume
vars:
  region: ap-south-1 # region comment
envs:
  dev:
    destructive: true
    # systems comment
    systems:
      db: { type: postgres, read: { dsn: "$(sec get DEV_RO --print)" } } # hand note
  prod:
    destructive: false
    systems:
      db: { type: postgres, read: { dsn: "$(sec get PROD_RO --print)" } }
`
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	touched, err := AddSystem(p, "cluster", "k8s", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(touched, ",") != "dev,prod" {
		t.Fatalf("touched %v", touched)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	for _, want := range []string{"# top comment kept", "# region comment", "# systems comment", "# hand note",
		`db: {type: postgres, read: {dsn: "$(sec get DEV_RO --print)"}}`, "cluster_kubeconfig:", "REPLACE_ME_REQSUME_PROD_CLUSTER_KUBECONFIG"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Index(s, "region") > strings.Index(s, "envs") || strings.Index(s, "  dev:") > strings.Index(s, "  prod:") {
		t.Error("order changed")
	}
	if bad := validate(t, p); len(bad) > 0 {
		t.Fatalf("validate: %v", bad)
	}
	if _, err := os.Stat(filepath.Join(dir, "assets", "cluster", "notes", ".gitkeep")); err != nil {
		t.Error(err)
	}
	// SKILL.md regenerated from config (hand-written SKILL.md: not touched → error from Sync)
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		t.Error(err)
	}
	// duplicate and unknown env refused
	if _, err := AddSystem(p, "cluster", "k8s", nil); err == nil {
		t.Error("duplicate accepted")
	}
	if _, err := AddSystem(p, "cache", "redis", []string{"qa"}); err == nil {
		t.Error("unknown env accepted")
	}
	// only one env
	if _, err := AddSystem(p, "cache", "redis", []string{"prod"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(p)
	if cfg.System("dev", "cache") != nil || cfg.System("prod", "cache") == nil {
		t.Error("--env not honoured")
	}
}

const exportCfg = `version: 1
product: reqsume
vars: { namespace: reqsume }
secrets:
  ssh_key: "$(echo FAKE_SECRET_123)"
envs:
  dev:
    secrets: { db_ro: "${FAKE_PG_ENV}" }
    systems:
      db: { type: postgres, host: db.dev.local, dbname: app, read: { dsn: "${secrets.db_ro}", user: ro, password: "$(echo FAKE_SECRET_123)" } }
  prod:
    secrets:
      db_ro: "$(echo FAKE_SECRET_123)"
      kubeconfig: "$file(echo FAKE_SECRET_123 > ${out})"
    systems:
      bastion: { type: ssh, host: 10.8.0.5, user: deploy, key: "${file:secrets.ssh_key}" }
      db-tunnel: { type: portforward, via: bastion, remote: db.internal:5432 }
      db: { type: postgres, via: db-tunnel, read: { dsn: "${secrets.db_ro}" } }
      cluster: { type: k8s, kubeconfig: "${file:secrets.kubeconfig}", context: reqsume-prod, namespace: "${vars.namespace}" }
`

func TestExportNoSecrets(t *testing.T) {
	t.Setenv("FAKE_PG_ENV", "FAKE_SECRET_123")
	p := filepath.Join(t.TempDir(), "config.yaml")
	_ = os.WriteFile(p, []byte(exportCfg), 0o644)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	pg, err := PgService(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	kc, err := Kubeconfig(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"pg": pg, "kube": kc} {
		if strings.Contains(out, "FAKE_SECRET_123") || strings.Contains(out, "echo ") {
			t.Errorf("%s export leaks a secret or command:\n%s", name, out)
		}
	}
	for _, want := range []string{"[reqsume-dev-db]", "host=db.dev.local", "dbname=app", "user=ro",
		"[reqsume-prod-db]", "host=127.0.0.1",
		// same port `tmvp prod db-tunnel open` listens on (portforward connector default)
		"port=" + connector.LocalPortString("reqsume", "prod", "db-tunnel"), "tmvp prod db-tunnel open", "db.internal:5432", "envs.prod.secrets.db_ro"} {
		if !strings.Contains(pg, want) {
			t.Errorf("pg-service missing %q:\n%s", want, pg)
		}
	}
	for _, want := range []string{"- name: reqsume-prod-cluster", "cluster: reqsume-prod", "namespace: reqsume", "envs.prod.secrets.kubeconfig"} {
		if !strings.Contains(kc, want) {
			t.Errorf("kubeconfig missing %q:\n%s", want, kc)
		}
	}
	if _, err := PgService(cfg, "nope"); err == nil {
		t.Error("unknown env accepted")
	}
	if _, err := Kubeconfig(cfg, "dev"); err == nil {
		t.Error("dev has no k8s; want not_found")
	}
}

func TestNoReferenceGuesses(t *testing.T) {
	dir := scaffold(t, allTypes)
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) != ".gitkeep" && filepath.Base(p) != "SKILL.md" && filepath.Base(p) != "config.yaml" {
			t.Errorf("unexpected pre-written asset %s", p)
		}
		return err
	})
}

func TestSkillMDFromConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.MkdirAll(filepath.Join(dir, "shared"), 0o755)
	cfg := `version: 1
product: acme
assets: [assets, shared]
envs:
  dev:
    destructive: true
    systems:
      db: { type: postgres, read: { dsn: "$(sec get X --print)" }, labels: { role: primary } }
  prod:
    systems:
      db: { type: postgres, read: { dsn: "$(sec get Y --print)" }, labels: { role: primary } }
      api: { type: http, base_url: "https://api.example.test" }
`
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Sync(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	s := strings.ReplaceAll(string(b), filepath.ToSlash(dir), "<SKILLDIR>")
	golden(t, "skill_from_config.golden", s)
	// deterministic
	if err := Sync(p); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if string(b) != string(b2) {
		t.Error("not deterministic")
	}
	// hand-written SKILL.md is never overwritten
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: x\ndescription: y\n---\nmine\n"), 0o644)
	if err := Sync(p); err == nil {
		t.Error("overwrote a hand-written SKILL.md")
	}
}

func TestLint(t *testing.T) {
	dir := scaffold(t, "db:postgres,admin:web,api:http")
	if is := Lint(dir, LintOptions{}); len(is) > 0 {
		t.Fatalf("fresh scaffold should lint clean: %v", is)
	}
	w := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("assets/db/queries/ok.sql", "/*---\ndescription: fine\n---*/\nselect 1\n")
	w("assets/admin/pages/login.md", "---\npage: login\nelements:\n  password: { testid: pw }\n---\n")
	if is := Lint(dir, LintOptions{}); len(is) > 0 {
		t.Fatalf("valid items flagged: %v", is)
	}
	w("assets/db/queries/bare.sql", "select 2\n")
	w("assets/api/notes/creds.md", "token: FAKE_SECRET_123\nsee postgres://u:FAKE_SECRET_123@h/db\n")
	is := Lint(dir, LintOptions{})
	var msgs []string
	for _, i := range is {
		msgs = append(msgs, i.Path+" "+i.Msg)
		if strings.Contains(i.Msg, "FAKE_SECRET_123") {
			t.Error("lint echoed a secret value")
		}
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{"bare.sql saved item has no front-matter", "creds.md:1 literal value in secret field token", "creds.md:2 credentials embedded in URL"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	_ = os.Remove(filepath.Join(dir, "SKILL.md"))
	if is := Lint(dir, LintOptions{}); len(is) == 0 || !strings.Contains(is[0].Msg+is[len(is)-1].Msg+fmt.Sprint(is), "SKILL.md missing") {
		t.Errorf("missing SKILL.md not flagged: %v", is)
	}
}

func TestNewFromAndLinkClaude(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src.yaml")
	body := "version: 1\nproduct: reqsume\nenvs:\n  prod2:\n    systems:\n      box: {type: ssh, host: h, user: u, key: FAKE_KEY_PATH}\n      db: {type: postgres, dsn: FAKE_DSN, via: [box]}\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Product: "other", Path: filepath.Join(root, ".agents", "skills"), From: src}); err == nil {
		t.Fatal("product mismatch accepted")
	}
	if _, err := os.Stat(filepath.Join(root, ".agents", "skills", "other-tmvp")); err == nil {
		t.Fatal("mismatch left a folder behind")
	}
	dir, err := New(Options{Product: "reqsume", Path: filepath.Join(root, ".agents", "skills"), From: src})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "config.yaml")); string(b) != body {
		t.Fatalf("config not adopted verbatim:\n%s", b)
	}
	for _, p := range []string{"SKILL.md", "assets/box/notes", "assets/db/queries"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	link, err := LinkClaude(dir)
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need Developer Mode:", err)
		}
		t.Fatal(err)
	}
	if want := filepath.Join(root, ".claude", "skills", "reqsume-tmvp"); link != want {
		t.Fatalf("link %s want %s", link, want)
	}
	if b, err := os.ReadFile(filepath.Join(link, "SKILL.md")); err != nil || !strings.Contains(string(b), "name: reqsume-tmvp") {
		t.Fatalf("link does not resolve: %v", err)
	}
	if again, err := LinkClaude(dir); err != nil || again != link {
		t.Fatalf("relink: %s %v", again, err)
	}
	if l, err := LinkClaude(t.TempDir()); err != nil || l != "" {
		t.Fatalf("outside .agents/skills: %q %v", l, err)
	}
}

func TestSyncListsReferences(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("version: 1\nproduct: acme\nenvs:\n  dev: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "references"), 0o755)
	os.WriteFile(filepath.Join(dir, "references", "db.md"), []byte("---\ndescription: tables and who uses them\n---\n# db\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "references", "projects.md"), []byte("# Projects and blast radius\n"), 0o644)
	if err := Sync(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	for _, want := range []string{"`references/db.md` — tables and who uses them", "`references/projects.md` — Projects and blast radius"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %q in\n%s", want, b)
		}
	}
}
