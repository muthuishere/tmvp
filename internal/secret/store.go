// Package secret runs secret commands lazily, holds values in memory keyed by
// product, materialises file-shaped secrets for external CLIs, and redacts
// every value from any output stream (DESIGN §8).
package secret

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/tmvp/internal/errs"
)

// Def describes one secret slot to fetch. The server builds Defs from the
// config; this package knows nothing about config.
type Def struct {
	Key         string // unique slot key within the product ("scope/name")
	Name        string // human name for messages (config key), never the value
	Command     string // command text (vars expanded, ${out} kept for File)
	File        bool   // $file(cmd)
	Dir         string // working dir for the command (skill dir)
	Fingerprint string // changes when the definition changes
	// Fetch, when set, replaces running Command (e.g. env-var backed secrets).
	Fetch func(ctx context.Context) ([]byte, error)
}

// Status is a names-only view of a slot.
type Status struct {
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	State     string    `json:"state"` // ok | failed | pending
	Reason    string    `json:"reason,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

type entry struct {
	fp        string
	name      string
	val       []byte
	err       error
	expiresAt time.Time
	fetchedAt time.Time
	done      chan struct{}
}

// failureTTL: a failed fetch is retried after this long.
const failureTTL = 30 * time.Second

// refreshMargin: a value with expires_at is re-fetched this long before expiry.
const refreshMargin = 60 * time.Second

// Store is one product's in-memory secret store.
type Store struct {
	Product string
	runner  Runner
	red     *Redactor
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]*entry

	// salt keys the value fingerprints (HMAC-SHA256): random per store, never
	// persisted, so a fingerprint cannot be checked against a guessed value
	// outside this process.
	salt []byte
	hist map[string]*history // survives Clear/Invalidate: "changed since previous fetch"

	// OnEvent, when set, receives every fetch/refresh/invalidate/failure
	// (names and outcomes only — never a value). Called without s.mu held.
	OnEvent func(Event)
}

// Event is one secret lifecycle event (audit "secret" records).
type Event struct {
	Key, Name string
	Kind      string // fetch | refresh | invalidate | failure
	Trigger   string // the command that caused it (see WithTrigger)
	Updated   string // yes | no | unknown (value changed since the previous fetch)
	Code      string // error code on failure
	Error     string // redacted message on failure
	ExpiresAt time.Time
}

type history struct {
	fp        string // salted hash of the last good value
	updated   string // yes | no | unknown
	fetchedAt time.Time
	fetches   int
}

// Info is a names-only, value-free view of one slot.
type Info struct {
	State     string    // ok | failed | pending | absent
	Code      string    // error code when failed
	Reason    string    // redacted
	FetchedAt time.Time // last fetch attempt
	ExpiresAt time.Time
	Updated   string // yes | no | unknown
	Fetches   int
}

type triggerKey struct{}

// WithTrigger tags ctx with the command that may fetch secrets (audit only).
func WithTrigger(ctx context.Context, trigger string) context.Context {
	return context.WithValue(ctx, triggerKey{}, trigger)
}

func triggerOf(ctx context.Context) string {
	s, _ := ctx.Value(triggerKey{}).(string)
	return s
}

// NewStore creates a store. red may be nil (no redaction registration).
func NewStore(product string, r Runner, red *Redactor) *Store {
	if r == nil {
		r = ExecRunner{}
	}
	salt := make([]byte, 32)
	_, _ = rand.Read(salt)
	return &Store{Product: product, runner: r, red: red, now: time.Now, entries: map[string]*entry{},
		salt: salt, hist: map[string]*history{}}
}

// Get returns the value of d, running its command on first use (single
// flight per slot). Errors are *errs.Error with code secret_missing.
func (s *Store) Get(ctx context.Context, d Def) (string, error) {
	b, err := s.GetBytes(ctx, d)
	return string(b), err
}

// GetBytes is Get returning bytes (file-shaped secrets).
func (s *Store) GetBytes(ctx context.Context, d Def) ([]byte, error) {
	for {
		s.mu.Lock()
		e := s.entries[d.Key]
		if e != nil && e.fp != d.Fingerprint {
			e = nil // definition changed → refetch
		}
		if e != nil {
			select {
			case <-e.done:
				if s.fresh(e) {
					s.mu.Unlock()
					return e.val, e.err
				}
				e = nil
			default:
				s.mu.Unlock()
				select {
				case <-e.done:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
		e = &entry{fp: d.Fingerprint, name: d.Name, done: make(chan struct{})}
		s.entries[d.Key] = e
		s.mu.Unlock()
		s.fetch(ctx, d, e)
		return e.val, e.err
	}
}

func (s *Store) fresh(e *entry) bool {
	now := s.now()
	if e.err != nil {
		return now.Sub(e.fetchedAt) < failureTTL
	}
	if !e.expiresAt.IsZero() && now.After(e.expiresAt.Add(-refreshMargin)) {
		return false
	}
	return true
}

func (s *Store) fetch(ctx context.Context, d Def, e *entry) {
	defer close(e.done)
	var out []byte
	var err error
	// detach from caller cancellation so a cancelled request doesn't poison the slot
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultTimeout+5*time.Second)
	defer cancel()
	if d.Fetch != nil {
		out, err = d.Fetch(fctx)
	} else {
		out, err = s.runner.Run(fctx, d.Command, d.File, d.Dir)
	}
	e.fetchedAt = s.now()
	defer s.record(ctx, d, e)
	if err != nil {
		reason := err.Error()
		if s.red != nil {
			reason = s.red.Redact(reason)
		}
		e.err = errs.New(errs.SecretMissing, "secret %q: %s", d.Name, reason).
			WithHint("check access to the secret command for " + d.Name)
		return
	}
	val, exp := parseOutput(out, d.File)
	if len(val) == 0 {
		e.err = errs.New(errs.SecretMissing, "secret %q: command printed nothing", d.Name)
		return
	}
	if len(val) < MinSecretLen {
		wipe(val)
		e.err = errs.New(errs.SecretInvalid, "secret %q is shorter than %d characters", d.Name, MinSecretLen).
			WithHint("value too short to redact safely; use a longer secret")
		return
	}
	e.val, e.expiresAt = val, exp
	if s.red != nil {
		s.red.Add(string(val))
	}
}

// fingerprint is a salted hash of a value (never stored outside memory).
func (s *Store) fingerprint(val []byte) string {
	m := hmac.New(sha256.New, s.salt)
	m.Write(val)
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// record updates the value history after a fetch and emits the event.
func (s *Store) record(ctx context.Context, d Def, e *entry) {
	ev := Event{Key: d.Key, Name: d.Name, Trigger: triggerOf(ctx), Updated: "unknown", ExpiresAt: e.expiresAt}
	s.mu.Lock()
	h := s.hist[d.Key]
	if h == nil {
		h = &history{updated: "unknown"}
		s.hist[d.Key] = h
		ev.Kind = "fetch"
	} else {
		ev.Kind = "refresh"
	}
	h.fetchedAt, h.fetches = e.fetchedAt, h.fetches+1
	if e.err != nil {
		ee := errs.As(e.err)
		ev.Kind, ev.Code, ev.Error = "failure", string(ee.Code), ee.Message
	} else {
		fp := s.fingerprint(e.val)
		switch {
		case h.fp == "":
			h.updated = "unknown"
		case h.fp == fp:
			h.updated = "no"
		default:
			h.updated = "yes"
		}
		h.fp = fp
		ev.Updated = h.updated
	}
	cb := s.OnEvent
	s.mu.Unlock()
	if cb != nil {
		cb(ev)
	}
}

// Info returns the value-free state of slot key.
func (s *Store) Info(key string) Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := Info{State: "absent", Updated: "unknown"}
	if h := s.hist[key]; h != nil {
		in.Updated, in.FetchedAt, in.Fetches = h.updated, h.fetchedAt, h.fetches
	}
	e := s.entries[key]
	if e == nil {
		return in
	}
	select {
	case <-e.done:
		in.FetchedAt = e.fetchedAt
		if e.err != nil {
			ee := errs.As(e.err)
			in.State, in.Code, in.Reason = "failed", string(ee.Code), ee.Message
		} else {
			in.State, in.ExpiresAt = "ok", e.expiresAt
		}
	default:
		in.State = "pending"
	}
	return in
}

// Inspect calls f with the cached value of key (if fetched ok). f must not
// retain, log or return the bytes — only derived metadata (cert dates).
func (s *Store) Inspect(key string, f func(val []byte)) bool {
	s.mu.Lock()
	e := s.entries[key]
	s.mu.Unlock()
	if e == nil {
		return false
	}
	select {
	case <-e.done:
	default:
		return false
	}
	if e.err != nil || len(e.val) == 0 {
		return false
	}
	f(e.val)
	return true
}

// parseOutput trims one trailing newline from stdout values and understands
// {"value": "...", "expires_at": "RFC3339"} (AWS credential_process shape).
func parseOutput(out []byte, file bool) ([]byte, time.Time) {
	t := bytes.TrimSpace(out)
	if len(t) > 0 && t[0] == '{' {
		var j struct {
			Value     *string `json:"value"`
			ExpiresAt string  `json:"expires_at"`
		}
		if json.Unmarshal(t, &j) == nil && j.Value != nil {
			var exp time.Time
			if j.ExpiresAt != "" {
				exp, _ = time.Parse(time.RFC3339, j.ExpiresAt)
			}
			return []byte(*j.Value), exp
		}
	}
	if file {
		return out, time.Time{}
	}
	return []byte(strings.TrimRight(string(out), "\r\n")), time.Time{}
}

// Prefetch fetches several slots in parallel; returns per-key errors.
func (s *Store) Prefetch(ctx context.Context, defs []Def) map[string]error {
	var mu sync.Mutex
	res := map[string]error{}
	var wg sync.WaitGroup
	for _, d := range defs {
		wg.Add(1)
		go func(d Def) {
			defer wg.Done()
			_, err := s.GetBytes(ctx, d)
			mu.Lock()
			res[d.Key] = err
			mu.Unlock()
		}(d)
	}
	wg.Wait()
	return res
}

// Invalidate re-runs d after an auth error. If the new value equals the old
// one it returns secret_invalid (the caller must not retry); otherwise nil and
// the caller retries once with the new value.
func (s *Store) Invalidate(ctx context.Context, d Def) error {
	s.mu.Lock()
	old := s.entries[d.Key]
	delete(s.entries, d.Key)
	s.mu.Unlock()
	var oldVal []byte
	if old != nil {
		<-old.done
		oldVal = old.val
	}
	if cb := s.OnEvent; cb != nil {
		cb(Event{Key: d.Key, Name: d.Name, Kind: "invalidate", Trigger: triggerOf(ctx), Updated: "unknown"})
	}
	nv, err := s.GetBytes(ctx, d)
	if err != nil {
		return err
	}
	if oldVal != nil && bytes.Equal(oldVal, nv) {
		return errs.New(errs.SecretInvalid, "secret %q was rejected and re-fetching returned the same value", d.Name).
			WithHint("rotate or fix the secret in your secret manager")
	}
	return nil
}

// Retain drops every slot not in defs or whose fingerprint changed (config
// reload). Returns the keys dropped.
func (s *Store) Retain(defs []Def) []string {
	keep := map[string]string{}
	for _, d := range defs {
		keep[d.Key] = d.Fingerprint
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var dropped []string
	for k, e := range s.entries {
		if fp, ok := keep[k]; !ok || fp != e.fp {
			delete(s.entries, k)
			dropped = append(dropped, k)
		}
	}
	return dropped
}

// Clear drops every value from memory (secrets clear / idle shutdown).
func (s *Store) Clear() {
	s.mu.Lock()
	for k, e := range s.entries {
		select {
		case <-e.done:
			wipe(e.val)
		default:
		}
		delete(s.entries, k)
	}
	s.mu.Unlock()
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Status lists slots, names only.
func (s *Store) Status() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Status
	for k, e := range s.entries {
		st := Status{Key: k, Name: e.name, State: "pending"}
		select {
		case <-e.done:
			if e.err != nil {
				st.State, st.Reason = "failed", errs.As(e.err).Message
			} else {
				st.State, st.ExpiresAt = "ok", e.expiresAt
			}
		default:
		}
		out = append(out, st)
	}
	return out
}
