// Package connector is the contract between tmvp's server and every system
// type (postgres, k8s, ssh, web, ...). See CONTRACT.md at the repo root.
//
// A connector package registers a Factory from init():
//
//	func init() { connector.Register("postgres", func() connector.Connector { return &PG{} }) }
//
// The server creates one Connector per product/env/system/mode, calls Open
// once (warm pool), then Do for every call, and Close on idle/reload.
package connector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Connector is one live connection (pool) to one system in one mode.
//
// Open is called once with the resolved Target; it should establish (and
// verify) the connection so auth errors surface here. Do runs one verb and
// streams results through Emit; it must honour ctx and stop when Emit
// returns ErrLimit. Do may be called concurrently. Close releases everything.
type Connector interface {
	Type() string
	Verbs() []Verb
	Open(ctx context.Context, t *Target) error
	Do(ctx context.Context, c Call, e Emit) error
	Close() error
}

// Factory makes a fresh, unopened Connector.
type Factory func() Connector

// VerbMode classifies a verb.
type VerbMode int

const (
	Read        VerbMode = iota // allowed in plain `tmvp <sel> <system> <verb>`
	Write                       // only via `tmvp write ... --reason`
	Destructive                 // write that also needs destructive: true on env/system
	// ReadWrite verbs run both ways: as a plain read (read credential; the
	// connector must refuse anything that mutates) and through
	// `tmvp write ... --reason` (write credential, may mutate). The connector
	// decides per call from Call.Write (= Target.Mode == ModeWrite) — e.g.
	// web `click users.ban`, cloud `call ec2 start-instances`.
	ReadWrite
)

// Readable reports whether the verb may run without `tmvp write`.
func (m VerbMode) Readable() bool { return m == Read || m == ReadWrite }

// Writable reports whether the verb may run through `tmvp write`.
func (m VerbMode) Writable() bool { return m != Read }

func (m VerbMode) String() string {
	switch m {
	case Write:
		return "write"
	case Destructive:
		return "destructive"
	case ReadWrite:
		return "read-write"
	}
	return "read"
}

// Verb describes one verb a connector supports.
type Verb struct {
	Name string
	Mode VerbMode
	Help string // one line, shown in help/check
	// Default marks the write verb used when `tmvp write <env> <system> <stmt>`
	// is given something that is not a verb name (e.g. raw SQL).
	Default bool
	// Saved marks the read verb that runs a saved item when the verb word
	// itself starts with "@": `tmvp prod bastion @disk` → `script @disk`.
	// At most one per connector. In `tmvp write`, an "@name" goes to the
	// Default write verb instead.
	Saved bool
}

// Call is one invocation.
type Call struct {
	Verb    string
	Args    []string
	Flags   map[string]string // free-form --key=value pass-through (connector-specific)
	Reason  string
	Preview bool          // write: show blast radius, change nothing
	Limit   int           // row/line cap for this call (already defaulted by the server)
	Since   time.Duration // --since, 0 = unset
	// Destructive is true when the env/system allows destructive operations.
	// Connectors that detect destructive statements themselves (SQL) must
	// refuse them with errs.PolicyBlocked when this is false.
	Destructive bool
	// Write is true when the call came through `tmvp write` (the connector
	// was opened with ModeWrite). ReadWrite verbs use it to decide per call.
	Write bool
	// Timeout is the effective hard timeout for this call (--timeout >
	// system > env > global limits.timeout > 10s). The server also sets it as
	// the ctx deadline; connectors pass it on as the server-side limit
	// (statement_timeout, max_execution_time, maxTimeMS, ES/Loki/Prom timeout)
	// via ServerTimeout. 0 = not set (tests): use Target.Limits.
	Timeout time.Duration
}

// Row is one result row. Keys "env" and "system" are added by the batch.
type Row = map[string]any

// Emit receives results. Both methods return ErrLimit once the output cap is
// reached; the connector should stop producing and return nil.
type Emit interface {
	Row(r Row) error
	Text(line string) error
}

// ErrLimit is returned by Emit when the output cap is reached.
var ErrLimit = errors.New("output limit reached")

// EmitFuncs adapts two funcs to Emit (handy in tests).
type EmitFuncs struct {
	RowFn  func(Row) error
	TextFn func(string) error
}

func (e EmitFuncs) Row(r Row) error {
	if e.RowFn == nil {
		return nil
	}
	return e.RowFn(r)
}

func (e EmitFuncs) Text(s string) error {
	if e.TextFn == nil {
		return nil
	}
	return e.TextFn(s)
}

// Collector is an Emit that records everything (tests).
type Collector struct {
	mu    sync.Mutex
	Rows  []Row
	Lines []string
}

func (c *Collector) Row(r Row) error {
	c.mu.Lock()
	c.Rows = append(c.Rows, r)
	c.mu.Unlock()
	return nil
}

func (c *Collector) Text(s string) error {
	c.mu.Lock()
	c.Lines = append(c.Lines, s)
	c.mu.Unlock()
	return nil
}

// Optional interfaces a connector may implement.

// LoginHandler handles `tmvp login <env> <system>` (e.g. web manual browser login).
type LoginHandler interface {
	Login(ctx context.Context, c Call, e Emit) error
}

// Guard is called on a fresh, UNOPENED instance before the server opens the
// connection, so policy errors (destructive statement, bad args) surface
// without touching the system or resolving secrets.
type Guard interface {
	Guard(c Call) error
}

// Checker is called by `tmvp check` after Open; return nil when healthy.
type Checker interface {
	Check(ctx context.Context) error
}

var (
	regMu     sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds a connector type. Call from init(). Panics on duplicates.
func Register(typ string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := factories[typ]; dup {
		panic(fmt.Sprintf("connector: type %q registered twice", typ))
	}
	factories[typ] = f
}

// Lookup returns the factory for typ.
func Lookup(typ string) (Factory, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	f, ok := factories[typ]
	return f, ok
}

// Types lists registered types, sorted.
func Types() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(factories))
	for t := range factories {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// VerbsOf returns the verbs of a registered type (nil if unknown).
func VerbsOf(typ string) []Verb {
	f, ok := Lookup(typ)
	if !ok {
		return nil
	}
	return f().Verbs()
}

// FindVerb returns the verb named name.
func FindVerb(vs []Verb, name string) (Verb, bool) {
	for _, v := range vs {
		if v.Name == name {
			return v, true
		}
	}
	return Verb{}, false
}

// SavedVerb returns the read verb marked Saved (runs a bare "@name").
func SavedVerb(vs []Verb) (Verb, bool) {
	for _, v := range vs {
		if v.Saved && v.Mode.Readable() {
			return v, true
		}
	}
	return Verb{}, false
}

// DefaultWriteVerb returns the verb marked Default (or the first write verb).
func DefaultWriteVerb(vs []Verb) (Verb, bool) {
	for _, v := range vs {
		if v.Default && v.Mode != Read {
			return v, true
		}
	}
	for _, v := range vs {
		if v.Mode != Read {
			return v, true
		}
	}
	return Verb{}, false
}
