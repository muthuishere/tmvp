package server

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/secret"
)

// usedSecrets records which secret slots a target resolved (for auth retry).
type usedSecrets struct {
	mu sync.Mutex
	m  map[string]secret.Def
}

func (u *usedSecrets) add(d secret.Def) {
	u.mu.Lock()
	u.m[d.Key] = d
	u.mu.Unlock()
}

func (u *usedSecrets) list() []secret.Def {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]secret.Def, 0, len(u.m))
	for _, d := range u.m {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// fields implements connector.Fields over a config system with lazy secrets.
type fields struct {
	cfg   *config.Config
	env   string
	sys   *config.System
	store *secret.Store
	used  *usedSecrets
}

func (f *fields) Raw(path string) (any, bool) { return f.sys.Get(path) }

func (f *fields) String(ctx context.Context, path string) (string, error) {
	v, ok := f.sys.Get(path)
	if !ok {
		return "", errs.New(errs.Config, "field %q is not set", path).At(f.env, f.sys.Name)
	}
	s, isStr := v.(string)
	if !isStr {
		switch v.(type) {
		case map[string]any, []any:
			return "", errs.New(errs.Config, "field %q is not a string", path).At(f.env, f.sys.Name)
		}
		return fmt.Sprint(v), nil
	}
	if _, isFile := config.FileRef(s); isFile {
		return "", errs.New(errs.Config, "field %q is a file secret; use File/FilePath", path).At(f.env, f.sys.Name)
	}
	out, err := f.cfg.ExpandField(f.env, s, func(d *config.SecretDef) (string, error) {
		sd := toSecretDef(f.cfg, d)
		f.used.add(sd)
		return f.store.Get(ctx, sd)
	})
	if err != nil {
		return "", errs.As(err).At(f.env, f.sys.Name)
	}
	return out, nil
}

func (f *fields) File(ctx context.Context, path string) ([]byte, error) {
	v, ok := f.sys.Get(path)
	if !ok {
		return nil, errs.New(errs.Config, "field %q is not set", path).At(f.env, f.sys.Name)
	}
	s, _ := v.(string)
	name, isFile := config.FileRef(s)
	if !isFile {
		// allow a plain ${secrets.x} too: content is the value
		refs := config.Refs(s)
		if len(refs) == 1 && refs[0].Kind == config.RefSecret && "${"+refs[0].Raw+"}" == s {
			name = refs[0].Name
		} else {
			str, err := f.String(ctx, path)
			return []byte(str), err
		}
	}
	d := f.cfg.LookupSecret(f.env, name)
	if d == nil {
		return nil, errs.New(errs.Config, "unknown secret %q", name).At(f.env, f.sys.Name)
	}
	sd := toSecretDef(f.cfg, d)
	f.used.add(sd)
	b, err := f.store.GetBytes(ctx, sd)
	if err != nil {
		return nil, errs.As(err).At(f.env, f.sys.Name)
	}
	return b, nil
}

func (f *fields) FilePath(ctx context.Context, path string) (string, func(), error) {
	b, err := f.File(ctx, path)
	if err != nil {
		return "", nil, err
	}
	return secret.Materialize(f.sys.Name+"-"+path, b)
}
