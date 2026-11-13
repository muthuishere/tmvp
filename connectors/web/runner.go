package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"

	"github.com/muthuishere/tmvp/internal/errs"
)

// CLI is the external binary tmvp drives. tmvp never installs it.
const CLI = "playwright-cli"

// Runner runs one playwright-cli invocation in dir and returns stdout.
// Tests replace it with a fake; production uses execRunner.
type Runner interface {
	Run(ctx context.Context, dir string, args []string) ([]byte, error)
}

// NewRunner is the factory used by New (swappable in tests).
var NewRunner = func() Runner { return execRunner{} }

// lookPath checks the CLI is installed (swappable in tests).
var lookPath = exec.LookPath

type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, CLI, args...)
	cmd.Dir = dir // keeps .playwright-cli/ snapshot files out of the repo
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil && out.Len() == 0 {
		return nil, &runErr{msg: errLine(errb.String()), err: err}
	}
	return out.Bytes(), nil
}

type runErr struct {
	msg string
	err error
}

func (e *runErr) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return e.err.Error()
}

// reply is playwright-cli's --json envelope.
type reply struct {
	IsError  bool            `json:"isError"`
	Error    string          `json:"error"`
	Result   json.RawMessage `json:"result"`
	Snapshot json.RawMessage `json:"snapshot"`
}

// errNotOpen marks "the browser '<s>' is not open" so the caller can reopen once.
var errNotOpen = errors.New("browser session not open")

// call runs `playwright-cli -s=<session> --json <args>` and decodes the envelope.
// scrub removes secret values (user creds) from any error text.
func call(ctx context.Context, r Runner, dir, session string, scrub func(string) string, args ...string) (*reply, error) {
	full := append([]string{"-s=" + session, "--json"}, args...)
	out, err := r.Run(ctx, dir, full)
	if ctx.Err() != nil {
		return nil, errs.Wrap(errs.Timeout, ctx.Err(), "playwright-cli %s", args[0])
	}
	if err != nil {
		msg := scrub(err.Error())
		if strings.Contains(msg, "is not open") {
			return nil, errNotOpen
		}
		return nil, classify(args[0], msg)
	}
	var rep reply
	if jerr := json.Unmarshal(bytes.TrimSpace(out), &rep); jerr != nil {
		return nil, errs.New(errs.Runtime, "playwright-cli %s: unexpected output", args[0])
	}
	if rep.IsError {
		msg := scrub(rep.Error)
		if strings.Contains(msg, "is not open") {
			return nil, errNotOpen
		}
		return nil, classify(args[0], msg)
	}
	return &rep, nil
}

func classify(verb, msg string) *errs.Error {
	msg = strings.TrimPrefix(strings.TrimSpace(msg), "Error: ")
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "does not match any elements"):
		return errs.New(errs.NotFound, "%s: %s", verb, msg).WithHint("check the element locator in pages/*.md, or run `snapshot`")
	case strings.Contains(l, "timeout"):
		return errs.New(errs.Timeout, "%s: %s", verb, msg)
	case strings.Contains(l, "net::err") || strings.Contains(l, "ns_error"):
		return errs.New(errs.Unreachable, "%s: %s", verb, msg)
	case strings.Contains(l, "executable doesn't exist") || strings.Contains(l, "install-browser"):
		return errs.New(errs.Unsupported, "%s: %s", verb, msg).WithHint("run: playwright-cli install-browser")
	}
	return errs.New(errs.Runtime, "%s: %s", verb, msg)
}

// evalString decodes `eval` results: result is a JSON-encoded JSON value.
func evalValue(rep *reply) any {
	var s string
	if json.Unmarshal(rep.Result, &s) != nil {
		return nil
	}
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	return v
}

// errLine picks the "Error: ..." line out of a node stack trace.
func errLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "Error:") {
			return l
		}
	}
	return strings.TrimSpace(s)
}
