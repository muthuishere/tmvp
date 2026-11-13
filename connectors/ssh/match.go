package ssh

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/internal/errs"
)

// psEntry is one container from `docker ps -a`.
type psEntry struct{ Name, State string }

// matchContainer resolves want against the host's containers. An exact name
// wins; otherwise want is a prefix of a deploy-suffixed name (kamal
// `app-web-<sha>_uncommitted_<x>`, compose `proj-svc-1`): the single running
// container named want-… or want_… is used, or the single stopped one when
// none run. Several candidates are an error that lists them, never a guess.
func matchContainer(all []psEntry, want string) (string, error) {
	var running, stopped []string
	for _, c := range all {
		if c.Name == want {
			return want, nil
		}
		if !strings.HasPrefix(c.Name, want) || len(c.Name) == len(want) {
			continue
		}
		if sep := c.Name[len(want)]; sep != '-' && sep != '_' {
			continue
		}
		if c.State == "running" {
			running = append(running, c.Name)
		} else {
			stopped = append(stopped, c.Name)
		}
	}
	pick := running
	if len(pick) == 0 {
		pick = stopped
	}
	switch len(pick) {
	case 1:
		return pick[0], nil
	case 0:
		return "", errs.New(errs.NotFound, "container %s: not found (no exact or prefix match)", want).
			WithHint("tmvp <env> <ssh-system> docker ps -a")
	}
	sort.Strings(pick)
	if len(pick) > 5 {
		pick = append(pick[:5], "…")
	}
	return "", errs.New(errs.Usage, "container %s matches several: %s", want, strings.Join(pick, ", ")).
		WithHint("use a longer prefix or the full name")
}

// resolveContainer lists containers over ssh and applies matchContainer.
func (s *SSH) resolveContainer(ctx context.Context, want string, sudo bool) (string, error) {
	cmd := dockerPrefix(sudo) + "docker ps -a --format " + shQuote("{{.Names}}|{{.State}}") + " 2>&1"
	out, err := s.capture(ctx, cmd, 15*time.Second)
	if err != nil {
		return "", dockerErr(out, errs.Wrap(errs.Unreachable, err, "docker ps"), "container "+want)
	}
	var all []psEntry
	for _, l := range out {
		n, st, ok := strings.Cut(strings.TrimSpace(l), "|")
		if ok && n != "" {
			all = append(all, psEntry{n, st})
		}
	}
	return matchContainer(all, want)
}
