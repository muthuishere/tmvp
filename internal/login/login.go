// Package login enforces manual logins (DESIGN §8.1): before a system is
// opened, its login.check command runs (cached ~5 min); failure yields
// login_required naming the login.manual command. tmvp never logs in itself.
package login

import (
	"context"
	"sync"
	"time"

	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/secret"
)

// TTL is how long a successful check is cached.
const TTL = 5 * time.Minute

// Spec is a system's login declaration.
type Spec struct {
	Manual string // command the user runs, or "browser"
	Check  string // non-interactive command; exit 0 = logged in
}

// RunFunc runs a check command (swappable in tests).
type RunFunc func(ctx context.Context, cmd, dir string) error

// Checker caches successful checks per key (product/env/system).
type Checker struct {
	Run RunFunc
	now func() time.Time
	mu  sync.Mutex
	ok  map[string]time.Time
}

// New returns a Checker that runs checks as real processes (30s timeout, no stdin).
func New() *Checker {
	return &Checker{Run: func(ctx context.Context, cmd, dir string) error {
		ctx, cancel := context.WithTimeout(ctx, secret.DefaultTimeout)
		defer cancel()
		_, err := secret.Exec(ctx, cmd, dir)
		return err
	}, now: time.Now, ok: map[string]time.Time{}}
}

// Ensure runs the check for key unless a recent success is cached.
// No check configured → nil.
func (c *Checker) Ensure(ctx context.Context, key string, s *Spec, dir, env, system string) error {
	if s == nil || s.Check == "" {
		return nil
	}
	c.mu.Lock()
	t, cached := c.ok[key]
	c.mu.Unlock()
	if cached && c.now().Sub(t) < TTL {
		return nil
	}
	if err := c.Run(ctx, s.Check, dir); err != nil {
		e := errs.New(errs.LoginRequired, "login required for %s.%s", env, system).At(env, system)
		if s.Manual == "browser" {
			e.Hint = "ask the user to run: tmvp login " + env + " " + system
		} else if s.Manual != "" {
			e.Hint = "ask the user to run: " + s.Manual + " (in Claude Code: ! " + s.Manual + "), then retry"
		} else {
			e.Hint = "the login check failed; ask the user to log in, then retry"
		}
		return e
	}
	c.mu.Lock()
	c.ok[key] = c.now()
	c.mu.Unlock()
	return nil
}

// Forget drops a cached success (e.g. after an auth error).
func (c *Checker) Forget(key string) {
	c.mu.Lock()
	delete(c.ok, key)
	c.mu.Unlock()
}
