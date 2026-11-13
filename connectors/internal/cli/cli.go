// Package cli holds the helpers the CLI-backed connectors (aws, gcp, az,
// git, script) share: resolving a CLI from PATH, running it bounded and
// isolated, classifying its failures, and deciding whether a CLI call is a
// read. Internal to connectors/.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/muthuishere/tmvp/internal/secure"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// Find resolves a CLI from PATH (exec.LookPath honours PATHEXT on Windows,
// so aws.exe / gcloud.cmd / az.cmd are found).
func Find(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		for _, ext := range []string{".exe", ".cmd", ".bat"} {
			if p, err2 := exec.LookPath(name + ext); err2 == nil {
				return p, nil
			}
		}
	}
	return "", errs.New(errs.Unsupported, "%s CLI not found on PATH", name).
		WithHint("install the official " + name + " CLI and make sure it is on PATH")
}

// Cmd is one CLI invocation.
type Cmd struct {
	Path    string
	Args    []string
	Env     map[string]string // overrides on top of the inherited environment
	Unset   []string          // inherited variables to drop (isolation)
	Dir     string
	Stdin   io.Reader
	Timeout time.Duration
}

func (c Cmd) build(ctx context.Context) (*exec.Cmd, context.CancelFunc) {
	cancel := context.CancelFunc(func() {})
	if c.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
	}
	x := exec.CommandContext(ctx, c.Path, c.Args...)
	x.Dir = c.Dir
	x.Stdin = c.Stdin
	x.Env = MergeEnv(os.Environ(), c.Env, c.Unset)
	x.WaitDelay = 2 * time.Second
	return x, cancel
}

// MergeEnv returns base minus unset/overridden keys plus over.
func MergeEnv(base []string, over map[string]string, unset []string) []string {
	drop := map[string]bool{}
	for _, k := range unset {
		drop[envKey(k)] = true
	}
	for k := range over {
		drop[envKey(k)] = true
	}
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[envKey(k)] {
			out = append(out, kv)
		}
	}
	for k, v := range over {
		out = append(out, k+"="+v)
	}
	return out
}

func envKey(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

// capWriter keeps at most n bytes and remembers it overflowed.
type capWriter struct {
	buf  bytes.Buffer
	n    int
	over bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.n - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.over = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.over = true
	}
	return len(p), nil
}

// Output runs c and returns stdout (capped at maxBytes; Truncated tells).
func Output(ctx context.Context, c Cmd, maxBytes int) (out []byte, truncated bool, err error) {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	x, cancel := c.build(ctx)
	defer cancel()
	so := &capWriter{n: maxBytes}
	se := &capWriter{n: 16 << 10}
	x.Stdout, x.Stderr = so, se
	runErr := x.Run()
	if runErr != nil {
		return so.buf.Bytes(), so.over, Classify(ctx, runErr, se.buf.String(), c.Timeout)
	}
	return so.buf.Bytes(), so.over, nil
}

// Lines runs c and streams stdout lines to e (bounded by maxLines/maxBytes).
// Reaching the cap kills the process and returns nil.
func Lines(ctx context.Context, c Cmd, e connector.Emit, maxLines, maxBytes int) error {
	if maxLines <= 0 {
		maxLines = 500
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	x, cancel := c.build(ctx)
	defer cancel()
	se := &capWriter{n: 16 << 10}
	x.Stderr = se
	pr, err := x.StdoutPipe()
	if err != nil {
		return errs.Wrap(errs.Runtime, err, "")
	}
	if err := x.Start(); err != nil {
		return Classify(ctx, err, "", c.Timeout)
	}
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n, b, capped := 0, 0, false
	for sc.Scan() {
		line := sc.Text()
		n++
		b += len(line) + 1
		if n > maxLines || b > maxBytes {
			capped = true
			break
		}
		if err := e.Text(line); err != nil {
			if errors.Is(err, connector.ErrLimit) {
				capped = true
				break
			}
			stop()
			_ = x.Wait()
			return err
		}
	}
	if capped {
		_ = e.Text(fmt.Sprintf("… output capped at %d lines / %d bytes", maxLines, maxBytes))
		stop()
		_ = x.Wait()
		return nil
	}
	_, _ = io.Copy(io.Discard, pr)
	if err := x.Wait(); err != nil {
		return Classify(ctx, err, se.buf.String(), c.Timeout)
	}
	return nil
}

var authMarks = []string{
	"expiredtoken", "invalidclienttokenid", "unrecognizedclientexception", "signaturedoesnotmatch",
	"unable to locate credentials", "the security token included in the request is invalid",
	"token has expired", "sso session", "invalid_grant", "reauthentication", "re-authenticate",
	"gcloud auth login", "invalid jwt", "unauthenticated", "aadsts", "please run 'az login'",
	"az login", "authentication failed", "permission denied (publickey", "could not read username",
	"authentication required", "401 unauthorized", "http 401",
}

// Classify maps a CLI failure to a tmvp error. The stderr tail is kept for
// context (never the argv, which may name resources but never secrets —
// secrets travel in env/files).
func Classify(ctx context.Context, runErr error, stderr string, timeout time.Duration) error {
	// A deadline (on ctx, or in runErr's chain) is a timeout whatever shape
	// the process death took: exec reports a context kill as an *ExitError
	// ("signal: killed"), which would otherwise read as a runtime failure.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(runErr, context.DeadlineExceeded) {
		if timeout > 0 {
			return errs.New(errs.Timeout, "CLI call exceeded %s", timeout)
		}
		return errs.Wrap(errs.Timeout, context.DeadlineExceeded, "CLI call exceeded its deadline")
	}
	// Cancellation (caller went away) is not the CLI's fault; keep
	// context.Canceled in the chain so callers can errors.Is it.
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(runErr, context.Canceled) {
		return errs.Wrap(errs.Runtime, context.Canceled, "CLI call canceled")
	}
	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		if errors.Is(runErr, exec.ErrNotFound) {
			return errs.Wrap(errs.Unsupported, runErr, "")
		}
		return errs.Wrap(errs.Runtime, runErr, "")
	}
	tail := Tail(stderr, 400)
	low := strings.ToLower(stderr)
	for _, m := range authMarks {
		if strings.Contains(low, m) {
			return errs.AuthError(errors.New(tail))
		}
	}
	if tail == "" {
		tail = ee.Error()
	}
	return errs.New(errs.Runtime, "exit %d: %s", ee.ExitCode(), tail)
}

// Tail returns the last n bytes of s, trimmed, on one logical block.
func Tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}

// Positional returns args that are not flags or flag values. A token after a
// "--flag" (without "=") is taken as that flag's value unless it looks like a
// flag itself; boolFlags lists flags that take no value.
func Positional(args []string, boolFlags map[string]bool) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") {
			if !strings.Contains(a, "=") && !boolFlags[a] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

// HasFlag reports whether any of names appears in args (as --x or --x=…).
func HasFlag(args []string, names ...string) string {
	for _, a := range args {
		k, _, _ := strings.Cut(a, "=")
		for _, n := range names {
			if k == n {
				return n
			}
		}
	}
	return ""
}

// Policy decides whether a CLI call is a read.
type Policy struct {
	ReadWords    map[string]bool // exact verbs that only read (list, describe, show ...)
	ReadPrefixes []string        // verb prefixes that only read (describe-, list-, get- ...)
	WriteWords   map[string]bool // verbs that mutate; first verb-like token wins
	Secret       map[string]bool // reads that reveal secret material → always blocked
}

// Verdict is the classification of a CLI call.
type Verdict int

const (
	Mutating Verdict = iota
	ReadOnly
	RevealsSecret
)

// Classify walks the positional words in order; the first word that is a
// known read or write verb decides. Unknown → Mutating (safe default).
func (p Policy) Classify(words []string) (Verdict, string) {
	for _, w := range words {
		lw := strings.ToLower(w)
		if p.Secret[lw] {
			return RevealsSecret, w
		}
		if p.ReadWords[lw] {
			return ReadOnly, w
		}
		for _, pre := range p.ReadPrefixes {
			if strings.HasPrefix(lw, pre) {
				return ReadOnly, w
			}
		}
		if p.WriteWords[lw] {
			return Mutating, w
		}
	}
	return Mutating, strings.Join(words, " ")
}

// Set builds a lookup set.
func Set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// Opt resolves "<mode>.<key>", then "<key>", returning "" when neither is set.
// In write mode it never falls back to read.*.
func Opt(ctx context.Context, t *connector.Target, key string) (string, error) {
	if t.Has(string(t.Mode)+"."+key) || t.Has(key) {
		return t.ModeString(ctx, key)
	}
	return "", nil
}

// OptPath is Opt for the field path that would be used (for File/FilePath).
func OptPath(t *connector.Target, key string) string {
	if p := string(t.Mode) + "." + key; t.Has(p) {
		return p
	}
	if t.Has(key) {
		return key
	}
	return ""
}

// IsFileRef reports whether a field is written as "${file:secrets.x}".
func IsFileRef(t *connector.Target, path string) bool {
	v, ok := t.Raw(path)
	s, isStr := v.(string)
	return ok && isStr && strings.HasPrefix(strings.TrimSpace(s), "${file:")
}

// TempDir makes a private (0700) per-call directory; call cleanup when done.
func TempDir(prefix string) (string, func(), error) {
	d, err := secure.MkdirTemp("", prefix)
	if err != nil {
		return "", func() {}, errs.Wrap(errs.Runtime, err, "temp dir")
	}
	return d, func() { _ = os.RemoveAll(d) }, nil
}

// SinceOr returns c.Since, or def when unset; refuses ranges above max.
func SinceOr(c connector.Call, def, max time.Duration) (time.Duration, error) {
	d := c.Since
	if d <= 0 {
		d = def
	}
	if max > 0 && d > max {
		return 0, errs.New(errs.GuardRejected, "--since %s exceeds the %s cap", d, max).WithHint("narrow --since")
	}
	return d, nil
}

// LimitOr returns c.Limit or def.
func LimitOr(c connector.Call, def int) int {
	if c.Limit > 0 {
		return c.Limit
	}
	return def
}

// IgnoreLimit turns ErrLimit into nil.
func IgnoreLimit(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}

// EmitJSON emits CLI JSON output: an array of objects → rows, an object → one
// row, scalars/arrays of scalars → text. Stops on ErrLimit.
func EmitJSON(e connector.Emit, data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		for _, l := range strings.Split(string(data), "\n") {
			if err := e.Text(l); err != nil {
				return IgnoreLimit(err)
			}
		}
		return nil
	}
	return IgnoreLimit(emitValue(e, v))
}

func emitValue(e connector.Emit, v any) error {
	switch x := v.(type) {
	case map[string]any:
		return e.Row(x)
	case []any:
		for _, it := range x {
			var err error
			if m, ok := it.(map[string]any); ok {
				err = e.Row(m)
			} else {
				err = e.Text(fmt.Sprint(it))
			}
			if err != nil {
				return err
			}
		}
		return nil
	case nil:
		return nil
	}
	return e.Text(fmt.Sprint(v))
}

// WriteCall maps the ReadWrite verb `call` to the write verb `cli` when the
// call came through `tmvp write`, so `tmvp write prod cloud call ec2
// start-instances` and `... cli ec2 start-instances` behave the same.
func WriteCall(c connector.Call) connector.Call {
	if c.Verb == "call" && c.Write {
		c.Verb = "cli"
	}
	return c
}

// ScopeRow is the `ls` row for a cloud system: kind "scope" plus the named
// configured (non-secret) fields that are set, e.g. profile, region, project.
// Never pass secret-ish field names here.
func ScopeRow(ctx context.Context, t *connector.Target, keys ...string) connector.Row {
	r := connector.Row{"kind": "scope", "name": t.System}
	for _, k := range keys {
		if !t.Has(k) {
			continue
		}
		if v, err := t.String(ctx, k); err == nil && v != "" {
			r[k] = v
		}
	}
	return r
}
