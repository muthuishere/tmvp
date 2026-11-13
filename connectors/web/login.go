package web

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func jsonString(raw json.RawMessage, out *string) error { return json.Unmarshal(raw, out) }

// loginPollEvery is how often the headed login is checked for completion.
var loginPollEvery = 2 * time.Second

// Login handles `tmvp login <env> <system> [--user name]`.
// Scripted logins run their steps headless; manual: browser opens a headed
// browser, waits for the human to finish (success element visible, or the URL
// leaving the login page), then saves the session state 0600.
func (w *Web) Login(ctx context.Context, c connector.Call, e connector.Emit) error {
	s, err := w.session(c)
	if err != nil {
		return err
	}
	l := sessLock(s.name)
	l.Lock()
	defer l.Unlock()
	if w.assets.Login == nil && !w.manual {
		return e.Row(connector.Row{"user": s.user, "status": "no login declared"})
	}
	if !w.manual {
		return s.login(ctx, e)
	}
	spec := w.assets.Login
	start := w.base.String() + "/"
	page := spec.Page
	if page != "" {
		if u, _, err := w.target(page); err == nil {
			start = u
		}
	}
	h := &sess{w: w, user: s.user, name: s.name + "-login", state: s.state}
	w.mu.Lock()
	w.used[h.name] = true
	w.mu.Unlock()
	args := []string{"open", "--headed"}
	if w.browser != "" {
		args = append(args, "--browser="+w.browser)
	}
	if _, err := call(ctx, w.r, w.work, h.name, w.scrub, append(args, start)...); err != nil {
		return err
	}
	setOpen(h.name, true)
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = w.r.Run(cctx, w.work, []string{"-s=" + h.name, "close"})
		cancel()
		setOpen(h.name, false)
	}()
	_ = e.Text("a browser window is open: log in there (SSO / 2FA / consent); tmvp saves the session when done")
	var loc string
	if len(spec.Success) > 0 {
		if loc, err = w.specLocator(spec.Success, page); err != nil {
			return err
		}
	}
	for {
		var done bool
		if loc != "" {
			done, err = h.visible(ctx, loc)
		} else {
			var cur string
			cur, err = h.url(ctx)
			done = cur != "" && !strings.HasPrefix(cur, start) && !strings.Contains(strings.ToLower(cur), "login")
		}
		if err != nil {
			return err
		}
		if done {
			break
		}
		if err := sleep(ctx, loginPollEvery); err != nil {
			return errs.New(errs.LoginRequired, "manual login did not finish in time").
				WithHint("run tmvp login again and complete the login in the browser window")
		}
	}
	if err := h.saveState(ctx); err != nil {
		return err
	}
	// the headless session reloads the new state on next use
	if isOpen(s.name) {
		_, _ = w.r.Run(ctx, w.work, []string{"-s=" + s.name, "close"})
		setOpen(s.name, false)
	}
	exp, _ := s.fresh()
	return e.Row(connector.Row{"user": s.user, "status": "session saved", "expires": exp.UTC().Format(time.RFC3339)})
}
