package config

import (
	"strings"
	"testing"

	"github.com/muthuishere/tmvp/internal/errs"
)

const typed = `version: 1
product: acme
envs:
  dev-a:   { type: development }
  dev-b:   { type: dev, destructive: false }
  stage:   { type: uat }
  prod-eu: { type: production }
  prod-us: { type: PROD }
  legacy:  {}
`

func TestEnvTypes(t *testing.T) {
	c, err := Parse([]byte(typed))
	if err != nil {
		t.Fatal(err)
	}
	for env, want := range map[string]string{"dev-a": "dev", "dev-b": "dev", "stage": "staging", "prod-eu": "prod", "prod-us": "prod", "legacy": ""} {
		if c.Envs[env].Type != want {
			t.Errorf("%s type %q want %q", env, c.Envs[env].Type, want)
		}
	}
	// protection follows type unless destructive is set explicitly
	for env, prot := range map[string]bool{"dev-a": false, "dev-b": true, "stage": true, "prod-eu": true, "prod-us": true, "legacy": true} {
		if c.IsProtected(env) != prot {
			t.Errorf("%s protected=%v want %v", env, c.IsProtected(env), prot)
		}
	}
	for sel, want := range map[string]string{"type:prod": "prod-eu,prod-us", "prod": "prod-eu,prod-us", "production": "prod-eu,prod-us",
		"dev": "dev-a,dev-b", "type:staging,legacy": "stage,legacy", "prod-eu": "prod-eu", "dev-*": "dev-a,dev-b"} {
		got, err := c.matchEnvs(sel)
		if err != nil || strings.Join(got, ",") != want {
			t.Errorf("%s → %v %v, want %s", sel, got, err, want)
		}
		if !c.IsEnvSelector(sel) {
			t.Errorf("%s not an env selector", sel)
		}
	}
	if _, err := c.matchEnvs("type:local"); errs.CodeOf(err) != errs.NotFound {
		t.Errorf("type:local: %v", err)
	}
}

func TestProdTypeCannotBeDestructive(t *testing.T) {
	_, err := Parse([]byte("version: 1\nproduct: a\nenvs:\n  p: { type: production, destructive: true }\n"))
	if errs.CodeOf(err) != errs.Config {
		t.Fatalf("got %v", err)
	}
}

func TestDotenvSecret(t *testing.T) {
	c, err := Parse([]byte("version: 1\nproduct: a\nsecrets:\n  pw: \"$dotenv(.env.prod, PGPASSWORD)\"\n  q: \"$dotenv( 'my env/.env' , DB_URL )\"\nenvs:\n  dev: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d := c.Secrets["pw"]; d.DotenvFile != ".env.prod" || d.DotenvKey != "PGPASSWORD" || d.Command != "" {
		t.Fatalf("%+v", d)
	}
	if d := c.Secrets["q"]; d.DotenvFile != "my env/.env" || d.DotenvKey != "DB_URL" {
		t.Fatalf("%+v", d)
	}
	for _, is := range c.Validate(ValidateOptions{}) {
		if is.Level == "error" && strings.Contains(is.Path, "secrets") {
			t.Fatalf("validate: %+v", is)
		}
	}
}
