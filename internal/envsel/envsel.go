// Package envsel resolves which env(s) a command runs on and persists the
// per-product active env set by `tmvp use` (DESIGN §3.1).
//
// Precedence (first wins):
//
//	positional selector / --env  → explicit
//	TMVP_ENV                     → TMVP_ENV
//	`tmvp use` state             → use
//	config default_env           → default
//	nothing                      → none (reads run on all envs, as before)
package envsel

import (
	"encoding/json"
	"github.com/muthuishere/tmvp/internal/secure"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

// Selection is a resolved env selector and where it came from.
type Selection struct {
	Env    string // selector ("dev", "dev,prod", "prod*", "all"); "" when Source is none
	Source string // protocol.EnvExplicit | EnvFromVar | EnvFromUse | EnvDefault | EnvNone
	Where  string // human detail: state file, config path, ...
}

// Resolve applies the precedence to explicit (the positional/--env selector,
// "" when none was given). It does not validate the env names: Select does.
func Resolve(cfg *config.Config, explicit string) Selection {
	if explicit != "" {
		return Selection{Env: explicit, Source: protocol.EnvExplicit, Where: "command line"}
	}
	if v := strings.TrimSpace(os.Getenv("TMVP_ENV")); v != "" {
		return Selection{Env: v, Source: protocol.EnvFromVar, Where: "TMVP_ENV in this shell"}
	}
	if cfg != nil {
		if st, err := Load(); err == nil {
			if en := st.Envs[Key(cfg)]; en.Env != "" {
				return Selection{Env: en.Env, Source: protocol.EnvFromUse, Where: "tmvp use (" + StatePath() + ")"}
			}
		}
		if cfg.DefaultEnv != "" {
			return Selection{Env: cfg.DefaultEnv, Source: protocol.EnvDefault, Where: "default_env in " + cfg.Path}
		}
		if len(cfg.EnvOrder) > 0 { // no default_env: the first env in the config is the default
			return Selection{Env: cfg.EnvOrder[0], Source: protocol.EnvDefault, Where: "first env in " + cfg.Path}
		}
	}
	return Selection{Source: protocol.EnvNone, Where: "nothing selected an env: all envs"}
}

// Key identifies a product state slot like the engine does: canonical config
// path + product name, so two configs claiming the same product never share.
func Key(cfg *config.Config) string {
	return CanonicalPath(cfg.Path) + "#" + cfg.Product
}

// CanonicalPath is the absolute, symlink-resolved path (engine product key).
func CanonicalPath(p string) string {
	if p == "" {
		return ""
	}
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Clean(p)
}

// Entry is one product's persisted active env.
type Entry struct {
	Env     string    `json:"env"`
	Product string    `json:"product"`
	Config  string    `json:"config"`
	SetAt   time.Time `json:"set_at"`
	// Protected records that the env was protected when selected (writes need --force).
	Protected bool `json:"protected,omitempty"`
}

// State is the whole state file.
type State struct {
	Envs map[string]Entry `json:"envs"`
}

// StatePath is TMVP_HOME/state/env.json.
func StatePath() string { return filepath.Join(protocol.Home(), "state", "env.json") }

// Load reads the state file (missing = empty).
func Load() (*State, error) {
	st := &State{Envs: map[string]Entry{}}
	b, err := os.ReadFile(StatePath())
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, errs.Wrap(errs.Runtime, err, "read env state")
	}
	if err := json.Unmarshal(b, st); err != nil {
		return &State{Envs: map[string]Entry{}}, errs.Wrap(errs.Runtime, err, "parse env state %s", StatePath())
	}
	if st.Envs == nil {
		st.Envs = map[string]Entry{}
	}
	return st, nil
}

// Save writes the state file atomically (dir 0700, file 0600).
func (st *State) Save() error {
	p := StatePath()
	if err := secure.MkdirAll(filepath.Dir(p)); err != nil {
		return errs.Wrap(errs.Runtime, err, "env state dir")
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".env-*.json")
	if err != nil {
		return errs.Wrap(errs.Runtime, err, "env state")
	}
	_ = secure.File(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return errs.Wrap(errs.Runtime, err, "env state")
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return errs.Wrap(errs.Runtime, err, "env state")
	}
	return nil
}

// Use validates env and persists it as cfg's active env. Any env may be
// selected (owner decision): the force rule lives on writes, not on `use`.
func Use(cfg *config.Config, env string) (Entry, error) {
	if _, ok := cfg.Envs[env]; !ok {
		return Entry{}, errs.New(errs.NotFound, "unknown env %q", env).WithHint("envs: " + strings.Join(cfg.EnvOrder, ", "))
	}
	st, err := Load()
	if err != nil {
		return Entry{}, err
	}
	en := Entry{Env: env, Product: cfg.Product, Config: CanonicalPath(cfg.Path), SetAt: time.Now().UTC(), Protected: cfg.IsProtected(env)}
	st.Envs[Key(cfg)] = en
	return en, st.Save()
}

// Clear drops cfg's active env (back to default_env).
func Clear(cfg *config.Config) error {
	st, err := Load()
	if err != nil {
		return err
	}
	delete(st.Envs, Key(cfg))
	return st.Save()
}
