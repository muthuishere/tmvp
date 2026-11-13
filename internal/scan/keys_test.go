package scan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvKeysNeverReturnsValues(t *testing.T) {
	d := t.TempDir()
	w := func(p, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(d, p)), 0o755)
		os.WriteFile(filepath.Join(d, p), []byte(s), 0o600)
	}
	w(".env", "DB_URL=FAKE_SECRET_1\n# comment\nEMPTY=\n")
	w("api/.env.production", "export REDIS_URL='FAKE_SECRET_2'\nX = \"\" # none\n")
	w("infra/prod.env", "S3_KEY=FAKE_SECRET_3\n")
	w("node_modules/x/.env", "NOPE=1\n")
	w("README.md", "A=1\n")
	ks, err := EnvKeys(d)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(ks)
	if strings.Contains(string(b), "FAKE_SECRET") || strings.Contains(string(b), "NOPE") || strings.Contains(string(b), `"A"`) {
		t.Fatalf("leak or wrong file: %s", b)
	}
	got := []string{}
	for _, k := range ks {
		got = append(got, k.File+":"+k.Env+":"+k.Name+":"+map[bool]string{true: "set", false: "empty"}[k.Set])
	}
	want := ".env::DB_URL:set .env::EMPTY:empty api/.env.production:production:REDIS_URL:set api/.env.production:production:X:empty infra/prod.env:prod:S3_KEY:set"
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, " "), want)
	}
}

func TestDotenvValue(t *testing.T) {
	d := t.TempDir()
	os.Mkdir(filepath.Join(d, ".git"), 0o755)
	os.MkdirAll(filepath.Join(d, ".agents", "skills", "x"), 0o755)
	os.WriteFile(filepath.Join(d, ".env.prod"), []byte("export PGPASSWORD='FAKE_PW_1 # x'\nEMPTY=\nPGPASSWORD=FAKE_PW_2 # later wins\n"), 0o600)
	p := ResolveDotenv(filepath.Join(d, ".agents", "skills", "x"), ".env.prod")
	if p != filepath.Join(d, ".env.prod") {
		t.Fatalf("resolve %s", p)
	}
	if v, err := DotenvValue(p, "PGPASSWORD"); err != nil || string(v) != "FAKE_PW_2" {
		t.Fatalf("value %q %v", v, err)
	}
	for _, k := range []string{"EMPTY", "NOPE"} {
		_, err := DotenvValue(p, k)
		if err == nil || strings.Contains(err.Error(), "FAKE_PW") {
			t.Fatalf("%s: %v", k, err)
		}
	}
}

func TestResolveDotenvForms(t *testing.T) {
	d := t.TempDir()
	os.Mkdir(filepath.Join(d, ".git"), 0o755)
	cfg := filepath.Join(d, ".agents", "skills", "x")
	os.MkdirAll(cfg, 0o755)
	os.WriteFile(filepath.Join(cfg, "local.env"), []byte("A=FAKE_1\n"), 0o600)
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		".env.prod":        filepath.Join(d, ".env.prod"),           // repo root
		"./secrets.env":    filepath.Join(cfg, "secrets.env"),       // config dir
		"../shared/.env":   filepath.Join(cfg, "..", "shared/.env"), // config dir
		"local.env":        filepath.Join(cfg, "local.env"),         // not at root → config dir
		"/etc/acme/.env":   "/etc/acme/.env",
		"~/acme/.env.prod": filepath.Join(home, "acme/.env.prod"),
	} {
		if got := ResolveDotenv(cfg, in); filepath.Clean(got) != filepath.Clean(want) {
			t.Errorf("%s → %s want %s", in, got, want)
		}
	}
}

func TestPortablePaths(t *testing.T) {
	d := t.TempDir()
	os.Mkdir(filepath.Join(d, ".git"), 0o755)
	cfg := filepath.Join(d, "cfg")
	os.MkdirAll(cfg, 0o755)
	if got, want := ResolveDotenv(cfg, `.\secrets\.env.prod`), filepath.Join(cfg, "secrets", ".env.prod"); got != want {
		t.Errorf("backslash relative: %s want %s", got, want)
	}
	if got, want := ResolveDotenv(cfg, `envs\.env.prod`), filepath.Join(d, "envs", ".env.prod"); got != want {
		t.Errorf("backslash repo-relative: %s want %s", got, want)
	}
	for _, p := range []string{`C:\acme\.env.prod`, `C:/acme/.env.prod`, `\\server\share\.env`} {
		if !isWindowsAbs(p) {
			t.Errorf("%s not seen as a Windows absolute path", p)
		}
		if got := ResolveDotenv(cfg, p); strings.HasPrefix(got, d) {
			t.Errorf("%s was joined to a dir: %s", p, got)
		}
	}
}
