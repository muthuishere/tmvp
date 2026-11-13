// Package errs defines tmvp's error codes and exit codes (DESIGN §10).
//
// Every error that reaches the user carries a Code, a Message, an optional
// Hint and the env/system it belongs to. Exit codes:
//
//	0 ok · 1 runtime/timeout/unreachable/auth · 2 config/secret_missing/login_required ·
//	3 policy_blocked/too_expensive · 4 partial · 5 guard_rejected
//
// too_expensive shares exit 3 with policy_blocked: like a policy refusal it
// is decided before the system runs anything, and the fix is a decision by
// the caller (narrow the query, raise --timeout, or --force).
package errs

import (
	"context"
	"errors"
	"fmt"
)

// Code is a stable, machine-readable error code.
type Code string

const (
	Runtime       Code = "runtime"
	Timeout       Code = "timeout"
	Unreachable   Code = "unreachable"
	Auth          Code = "auth_error" // credential rejected; server invalidates secrets + retries once
	Config        Code = "config_error"
	Usage         Code = "usage"
	NotFound      Code = "not_found"
	SecretMissing Code = "secret_missing"
	SecretInvalid Code = "secret_invalid" // re-fetched secret is identical and still rejected
	LoginRequired Code = "login_required"
	PolicyBlocked Code = "policy_blocked" // read-only violation, missing --reason, destructive not allowed, multi-target write
	// TooExpensive: the read's pre-run estimate exceeds its effective
	// timeout, so it was not run (cost guard, DESIGN §7). Exit 3.
	TooExpensive  Code = "too_expensive"
	Partial       Code = "partial"
	GuardRejected Code = "guard_rejected" // limits/timeouts/guards refused the call
	Skipped       Code = "skipped"        // target lacks the system/verb (not a failure)
	Unsupported   Code = "unsupported"
	ServerStale   Code = "server_stale" // server runs another build of tmvp; the client restarts it
)

// ExitCode maps a code to the process exit code.
func ExitCode(c Code) int {
	switch c {
	case "":
		return 0
	case Config, Usage, NotFound, SecretMissing, SecretInvalid, LoginRequired:
		return 2
	case PolicyBlocked, TooExpensive:
		return 3
	case Partial:
		return 4
	case GuardRejected:
		return 5
	case Skipped:
		return 0
	default:
		return 1
	}
}

// Error is the structured tmvp error.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	Env     string `json:"env,omitempty"`
	System  string `json:"system,omitempty"`
	Err     error  `json:"-"`
}

func (e *Error) Error() string {
	s := string(e.Code) + ": " + e.Message
	if e.Env != "" || e.System != "" {
		s = fmt.Sprintf("[%s.%s] %s", e.Env, e.System, s)
	}
	if e.Hint != "" {
		s += " (hint: " + e.Hint + ")"
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// New builds an Error.
func New(c Code, format string, a ...any) *Error {
	return &Error{Code: c, Message: fmt.Sprintf(format, a...)}
}

// Wrap builds an Error around a cause. The cause's text becomes the message.
func Wrap(c Code, err error, format string, a ...any) *Error {
	msg := fmt.Sprintf(format, a...)
	if err != nil {
		if msg != "" {
			msg += ": "
		}
		msg += err.Error()
	}
	return &Error{Code: c, Message: msg, Err: err}
}

// WithHint returns e with a hint set.
func (e *Error) WithHint(h string) *Error { e.Hint = h; return e }

// At returns e tagged with env/system (does not overwrite existing tags).
func (e *Error) At(env, system string) *Error {
	if e.Env == "" {
		e.Env = env
	}
	if e.System == "" {
		e.System = system
	}
	return e
}

// AuthError marks err as "the credential was rejected". The server reacts by
// re-running the secrets this target used and retrying once (DESIGN §8).
func AuthError(err error) *Error { return Wrap(Auth, err, "authentication failed") }

// As converts err to an *Error for reporting: a tmvp error in the chain is
// returned as is; any other non-nil error becomes Runtime (or Timeout for
// deadlines). It returns nil only for a nil err — so it can NOT be used to
// test "is this already a tmvp error"; use From / Is for that.
func As(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Wrap(Timeout, err, "")
	}
	return Wrap(Runtime, err, "")
}

// From returns the *Error in err's chain, or nil when err is nil or not a
// tmvp error (unlike As, it never converts).
func From(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// Is reports whether err is a tmvp error carrying one of codes (any code when
// none are given). Plain Go errors are never Is, even though As would map
// them to Runtime.
func Is(err error, codes ...Code) bool {
	e := From(err)
	if e == nil {
		return false
	}
	if len(codes) == 0 {
		return true
	}
	for _, c := range codes {
		if e.Code == c {
			return true
		}
	}
	return false
}

// CodeOf returns the code of err ("" for nil).
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	return As(err).Code
}

// IsAuth reports whether err is an auth error.
func IsAuth(err error) bool { return CodeOf(err) == Auth }
