package config

import (
	"strings"
	"testing"
	"time"
)

func TestTimeoutPrecedenceMatrix(t *testing.T) {
	const y = `
version: 1
product: p
limits: { timeout: 40s }
envs:
  a:
    limits: { timeout: 30s }
    systems:
      sys:  { type: x, limits: { timeout: 20s } }
      bare: { type: x }
  b:
    systems:
      bare: { type: x }
      sys:  { type: x, limits: { timeout: 5 } }
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	noGlobal, _ := Parse([]byte("version: 1\nproduct: p\nenvs:\n  a:\n    systems:\n      bare: { type: x }\n"))
	cases := []struct {
		c        *Config
		env, sys string
		flag     string
		want     time.Duration
		wantSrc  string
	}{
		{c, "a", "sys", "3m", 3 * time.Minute, SourceFlag},
		{c, "a", "sys", "", 20 * time.Second, SourceSystem},
		{c, "a", "bare", "", 30 * time.Second, SourceEnv},
		{c, "b", "bare", "", 40 * time.Second, SourceGlobal},
		{c, "b", "sys", "", 5 * time.Second, SourceSystem}, // bare number = seconds
		{c, "b", "bare", "1s", time.Second, SourceFlag},    // flag can also lower it
		{noGlobal, "a", "bare", "", DefaultTimeout, SourceDefault},
	}
	for _, tc := range cases {
		d, src, err := tc.c.EffectiveTimeout(tc.env, tc.sys, tc.flag)
		if err != nil || d != tc.want || src != tc.wantSrc {
			t.Errorf("%s.%s flag=%q: got %s/%s/%v want %s/%s", tc.env, tc.sys, tc.flag, d, src, err, tc.want, tc.wantSrc)
		}
	}
	if _, _, err := c.EffectiveTimeout("a", "sys", "soon"); err == nil {
		t.Fatal("bad --timeout must be a usage error")
	}
	if DefaultTimeout != 10*time.Second {
		t.Fatal("built-in default is 10s")
	}
	if m := c.MergedLimits("a", "sys"); m["timeout"] != "20s" {
		t.Fatalf("merge: %v", m)
	}
}

func TestValidateLimits(t *testing.T) {
	c, err := Parse([]byte("version: 1\nproduct: p\nlimits: { timeout: nope }\nenvs:\n  a:\n    limits: { cost_per_second: -1 }\n    systems:\n      s: { type: x, limits: { timeout: 0s } }\n"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, is := range c.Validate(ValidateOptions{}) {
		if is.Level == "error" && (is.Path == "limits.timeout" || is.Path == "envs.a.limits.cost_per_second" || is.Path == "envs.a.systems.s.limits.timeout") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("want 3 limit errors, got %d: %v", n, c.Validate(ValidateOptions{}))
	}
}

func TestShowEffectiveTimeout(t *testing.T) {
	c, _ := Parse([]byte("version: 1\nproduct: p\nlimits: { timeout: 45s }\nenvs:\n  a:\n    systems:\n      s: { type: x }\n"))
	out, err := c.ShowResolved()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "effective_timeout: 45s (global)") {
		t.Fatalf("show: %s", out)
	}
}
