package config

import (
	"fmt"
	"strconv"
	"time"

	"github.com/muthuishere/tmvp/internal/errs"
)

// DefaultTimeout is the built-in per-call timeout when no flag or config
// level sets one (DESIGN §7, cost guard).
const DefaultTimeout = 10 * time.Second

// Timeout sources, most to least specific.
const (
	SourceFlag    = "flag"    // --timeout
	SourceSystem  = "system"  // envs.<env>.systems.<sys>.limits.timeout
	SourceEnv     = "env"     // envs.<env>.limits.timeout
	SourceGlobal  = "global"  // top-level limits.timeout
	SourceDefault = "default" // DefaultTimeout
)

type limitLayer struct {
	src string
	m   map[string]any
}

// limitLayers returns the limits maps for env/system, most specific first.
func (c *Config) limitLayers(env, system string) []limitLayer {
	var out []limitLayer
	if s := c.System(env, system); s != nil {
		if m, ok := s.Fields["limits"].(map[string]any); ok {
			out = append(out, limitLayer{SourceSystem, m})
		}
	}
	if e := c.Envs[env]; e != nil && e.Limits != nil {
		out = append(out, limitLayer{SourceEnv, e.Limits})
	}
	if c.Limits != nil {
		out = append(out, limitLayer{SourceGlobal, c.Limits})
	}
	return out
}

// MergedLimits merges global < env < system `limits:` key by key.
func (c *Config) MergedLimits(env, system string) map[string]any {
	ls := c.limitLayers(env, system)
	out := map[string]any{}
	for i := len(ls) - 1; i >= 0; i-- {
		for k, v := range ls[i].m {
			out[k] = v
		}
	}
	return out
}

// ParseLimitDuration parses a limits duration value ("30s", "2m", 15 = seconds).
func ParseLimitDuration(v any) (time.Duration, error) {
	switch x := v.(type) {
	case string:
		d, err := time.ParseDuration(x)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("%q is not a positive duration (e.g. 30s, 2m)", x)
		}
		return d, nil
	case int:
		if x > 0 {
			return time.Duration(x) * time.Second, nil
		}
	case float64:
		if x > 0 {
			return time.Duration(x * float64(time.Second)), nil
		}
	}
	return 0, fmt.Errorf("%v is not a positive duration (e.g. 30s, 2m)", v)
}

// ParseLimitFloat parses a numeric limit (cost_per_second, ...).
func ParseLimitFloat(v any) (float64, error) {
	switch x := v.(type) {
	case int:
		if x > 0 {
			return float64(x), nil
		}
	case float64:
		if x > 0 {
			return x, nil
		}
	case string:
		if f, err := strconv.ParseFloat(x, 64); err == nil && f > 0 {
			return f, nil
		}
	}
	return 0, fmt.Errorf("%v is not a positive number", v)
}

// ConfigTimeout is the effective timeout for env/system from config alone:
// system limits.timeout > env > global > DefaultTimeout, with its source.
func (c *Config) ConfigTimeout(env, system string) (time.Duration, string) {
	for _, l := range c.limitLayers(env, system) {
		if v, ok := l.m["timeout"]; ok {
			if d, err := ParseLimitDuration(v); err == nil {
				return d, l.src
			}
		}
	}
	return DefaultTimeout, SourceDefault
}

// EffectiveTimeout applies the full precedence: --timeout (flag, "" = unset)
// > system > env > global > built-in default.
func (c *Config) EffectiveTimeout(env, system, flag string) (time.Duration, string, error) {
	if flag != "" {
		d, err := ParseLimitDuration(flag)
		if err != nil {
			return 0, "", errs.New(errs.Usage, "--timeout %s", err.Error())
		}
		return d, SourceFlag, nil
	}
	d, src := c.ConfigTimeout(env, system)
	return d, src, nil
}

// validateLimits reports bad limits values at one level.
func validateLimits(m map[string]any, path string, add func(sev, path, format string, a ...any)) {
	for k, v := range m {
		switch k {
		case "timeout", "statement_timeout":
			if _, err := ParseLimitDuration(v); err != nil {
				add("error", path+"."+k, "%s", err.Error())
			}
		case "cost_per_second", "rows_per_second", "bytes_per_second", "points_per_second", "seq_scan_rows":
			if _, err := ParseLimitFloat(v); err != nil {
				add("error", path+"."+k, "%s", err.Error())
			}
		}
	}
}
