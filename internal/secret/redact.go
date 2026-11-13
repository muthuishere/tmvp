package secret

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Mask replaces every redacted value.
const Mask = "[REDACTED]"

// MinSecretLen: shorter values are not redacted (they would shred output).
const MinSecretLen = 4

// Redactor removes secret values — plain, base64 (std/url, padded/raw),
// url-escaped, hex (lower/upper) and JSON-escaped forms, plus each line of a
// multi-line value — from text. Safe for concurrent use.
type Redactor struct {
	mu       sync.RWMutex
	forms    map[string]bool
	replacer *strings.Replacer
	maxLen   int
}

// NewRedactor returns an empty redactor.
func NewRedactor() *Redactor { return &Redactor{forms: map[string]bool{}} }

// Forms returns every encoded form of v that is redacted.
func Forms(v string) []string {
	if len(v) < MinSecretLen {
		return nil
	}
	b := []byte(v)
	set := map[string]bool{
		v:                                       true,
		base64.StdEncoding.EncodeToString(b):    true,
		base64.RawStdEncoding.EncodeToString(b): true,
		base64.URLEncoding.EncodeToString(b):    true,
		base64.RawURLEncoding.EncodeToString(b): true,
		hex.EncodeToString(b):                   true,
		strings.ToUpper(hex.EncodeToString(b)):  true,
		url.QueryEscape(v):                      true,
		url.PathEscape(v):                       true,
	}
	for _, j := range jsonForms(v) {
		set[j] = true
	}
	if strings.ContainsAny(v, "\r\n") {
		for _, line := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if line = strings.TrimSpace(line); len(line) >= 12 {
				set[line] = true
				for _, j := range jsonForms(line) {
					set[j] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		if len(f) >= MinSecretLen {
			out = append(out, f)
		}
	}
	return out
}

// jsonForms returns v JSON-string-escaped, with and without HTML escaping
// (json.Marshal escapes <>& as \u003c...; encoders with SetEscapeHTML(false) don't).
func jsonForms(v string) []string {
	var out []string
	if j, err := json.Marshal(v); err == nil {
		out = append(out, string(j[1:len(j)-1]))
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) == nil {
		j := strings.TrimSuffix(b.String(), "\n")
		out = append(out, j[1:len(j)-1])
	}
	return out
}

// Add registers a secret value (all its forms).
func (r *Redactor) Add(v string) {
	fs := Forms(v)
	if len(fs) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, f := range fs {
		if !r.forms[f] {
			r.forms[f] = true
			changed = true
		}
	}
	if changed {
		r.rebuild()
	}
}

// AddBasicAuth registers a basic-auth pair: the password and the
// base64("user:pass") credential that travels in the Authorization header
// (Add also covers its url/hex/raw variants). The user alone is not a secret.
func (r *Redactor) AddBasicAuth(user, pass string) {
	if r == nil || pass == "" {
		return
	}
	r.Add(pass)
	r.Add(base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)))
}

func (r *Redactor) rebuild() {
	all := make([]string, 0, len(r.forms))
	for f := range r.forms {
		all = append(all, f)
	}
	// longest first so overlapping forms redact maximally
	sort.Slice(all, func(i, j int) bool {
		if len(all[i]) != len(all[j]) {
			return len(all[i]) > len(all[j])
		}
		return all[i] < all[j]
	})
	pairs := make([]string, 0, 2*len(all))
	r.maxLen = 0
	for _, f := range all {
		pairs = append(pairs, f, Mask)
		if len(f) > r.maxLen {
			r.maxLen = len(f)
		}
	}
	r.replacer = strings.NewReplacer(pairs...)
}

// Redact returns s with every registered form masked.
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	rep := r.replacer
	r.mu.RUnlock()
	if rep == nil {
		return s
	}
	// repeat until stable: masking can expose an adjacent form
	for i := 0; i < 3; i++ {
		n := rep.Replace(s)
		if n == s {
			break
		}
		s = n
	}
	return s
}

func (r *Redactor) holdBack() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.maxLen == 0 {
		return 0
	}
	return r.maxLen - 1
}

// Writer wraps w so every byte written is redacted, including values split
// across Write calls. Call Flush (or Close) at the end of the stream.
func (r *Redactor) Writer(w io.Writer) *RedactWriter { return &RedactWriter{r: r, w: w} }

// RedactWriter is a streaming redacting writer.
type RedactWriter struct {
	mu  sync.Mutex
	r   *Redactor
	w   io.Writer
	buf []byte
}

// Write buffers p, redacts, and forwards everything except a tail that could
// still be the start of a secret.
func (rw *RedactWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.buf = append(rw.buf, p...)
	red := rw.r.Redact(string(rw.buf))
	hold := rw.r.holdBack()
	if len(red) <= hold {
		rw.buf = []byte(red)
		return len(p), nil
	}
	cut := len(red) - hold
	if _, err := io.WriteString(rw.w, red[:cut]); err != nil {
		return 0, err
	}
	rw.buf = []byte(red[cut:])
	return len(p), nil
}

// Flush writes the held-back tail.
func (rw *RedactWriter) Flush() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.buf) == 0 {
		return nil
	}
	_, err := io.WriteString(rw.w, rw.r.Redact(string(rw.buf)))
	rw.buf = rw.buf[:0]
	return err
}

// Close flushes.
func (rw *RedactWriter) Close() error { return rw.Flush() }
