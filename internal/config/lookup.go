package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/muthuishere/tmvp/internal/errs"
)

// FindOptions controls config lookup (DESIGN §3).
type FindOptions struct {
	ConfigPath string // --config: wins over everything
	SkillPath  string // --skill: <skill dir>/config.yaml
	Product    string // optional product hint (TMVP_PRODUCT) for ~/.tmvp/<product>/
	Cwd        string // default os.Getwd()
	Home       string // default os.UserHomeDir()
}

// Find returns the config path by lookup order; first found wins:
//
//	--config, --skill, ./.tmvp/config.yaml, .agents/skills/<product>-tmvp/config.yaml,
//	~/.tmvp/<product>/config.yaml, ~/.tmvp/config.yaml
//
// The repo-local locations are searched from Cwd upward to the repo root
// (a directory containing .git) or the filesystem root.
func Find(o FindOptions) (string, error) {
	if o.ConfigPath != "" {
		return mustExist(o.ConfigPath)
	}
	if o.SkillPath != "" {
		p := o.SkillPath
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			p = filepath.Join(p, "config.yaml")
		}
		return mustExist(p)
	}
	if o.Cwd == "" {
		o.Cwd, _ = os.Getwd()
	}
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	dir := o.Cwd
	for {
		if p := filepath.Join(dir, ".tmvp", "config.yaml"); isFile(p) && !samePath(dir, o.Home) {
			return p, nil
		}
		matches, _ := filepath.Glob(filepath.Join(dir, ".agents", "skills", "*-tmvp", "config.yaml"))
		sort.Strings(matches)
		if o.Product != "" {
			p := filepath.Join(dir, ".agents", "skills", o.Product+"-tmvp", "config.yaml")
			if isFile(p) {
				return p, nil
			}
		} else if len(matches) == 1 {
			return matches[0], nil
		} else if len(matches) > 1 {
			names := make([]string, len(matches))
			for i, m := range matches {
				names[i] = filepath.Base(filepath.Dir(m))
			}
			return "", errs.New(errs.Config, "several skills found (%s)", strings.Join(names, ", ")).
				WithHint("pass --skill <dir> or set TMVP_PRODUCT")
		}
		if isDir(filepath.Join(dir, ".git")) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if o.Product != "" {
		if p := filepath.Join(o.Home, ".tmvp", o.Product, "config.yaml"); isFile(p) {
			return p, nil
		}
	}
	if p := filepath.Join(o.Home, ".tmvp", "config.yaml"); isFile(p) {
		return p, nil
	}
	return "", errs.New(errs.Config, "no tmvp config found").
		WithHint("run inside the product repo, or pass --skill <dir> / --config <file>")
}

func mustExist(p string) (string, error) {
	abs, err := filepath.Abs(ExpandHome(p))
	if err != nil {
		return "", errs.Wrap(errs.Config, err, "config path")
	}
	if !isFile(abs) {
		return "", errs.New(errs.Config, "config not found: %s", abs)
	}
	return abs, nil
}

func isFile(p string) bool { st, err := os.Stat(p); return err == nil && !st.IsDir() }
func isDir(p string) bool  { st, err := os.Stat(p); return err == nil && st.IsDir() }
func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}
