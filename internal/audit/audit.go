// Package audit writes the per-product event log: JSONL in OTel log-record
// shape, one line per command; for writes an intent record before execution
// and an outcome record after (DESIGN §9). Every line passes the redactor.
package audit

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"github.com/muthuishere/tmvp/internal/secure"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/muthuishere/tmvp/internal/secret"
)

// FileName is the event log file inside the log dir.
const FileName = "events.jsonl"

// Record is an OTel-shaped log record.
type Record struct {
	TimeUnixNano   string         `json:"timeUnixNano"`
	Time           string         `json:"time"` // RFC3339Nano, human readable
	SeverityText   string         `json:"severityText"`
	SeverityNumber int            `json:"severityNumber"`
	Body           string         `json:"body"`
	Attributes     map[string]any `json:"attributes"`
	Resource       map[string]any `json:"resource"`
	TraceID        string         `json:"traceId,omitempty"`
}

// Logger is the single writer for one product's log.
type Logger struct {
	product string
	dir     string
	red     *secret.Redactor
	mu      sync.Mutex
	now     func() time.Time
}

// New creates a logger writing to dir/events.jsonl (dir created 0700).
func New(product, dir string, red *secret.Redactor) *Logger {
	return &Logger{product: product, dir: dir, red: red, now: time.Now}
}

// Dir returns the log dir.
func (l *Logger) Dir() string { return l.dir }

// NewID returns a random id tying intent and outcome records together.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Event records one command. attrs typically: op, env, system, verb, mode,
// outcome, code, duration_ms, rows, agent, reason.
func (l *Logger) Event(body string, attrs map[string]any) error {
	sev, num := "INFO", 9
	if o, _ := attrs["outcome"].(string); o != "" && o != "ok" && o != "skipped" {
		sev, num = "ERROR", 17
	}
	return l.write(body, sev, num, "", attrs)
}

// WriteIntent records a write before it runs. Returns the id for WriteOutcome.
func (l *Logger) WriteIntent(attrs map[string]any) (string, error) {
	id := NewID()
	a := copyAttrs(attrs)
	a["phase"] = "intent"
	return id, l.write("write.intent", "WARN", 13, id, a)
}

// WriteOutcome records the result of a write.
func (l *Logger) WriteOutcome(id string, attrs map[string]any) error {
	a := copyAttrs(attrs)
	a["phase"] = "outcome"
	return l.write("write.outcome", "WARN", 13, id, a)
}

func copyAttrs(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (l *Logger) write(body, sev string, num int, trace string, attrs map[string]any) error {
	now := l.now()
	rec := Record{
		TimeUnixNano: strconv.FormatInt(now.UnixNano(), 10), Time: now.UTC().Format(time.RFC3339Nano),
		SeverityText: sev, SeverityNumber: num, Body: body, Attributes: attrs, TraceID: trace,
		Resource: map[string]any{"service.name": "tmvp", "tmvp.product": l.product, "user": userName(), "host.pid": os.Getpid()},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line := string(b)
	if l.red != nil {
		line = l.red.Redact(line)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := secure.MkdirAll(l.dir); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(l.dir, FileName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	if err == nil {
		err = f.Sync() // write intent must survive a crash mid-write
	}
	return err
}

func userName() string {
	for _, k := range []string{"USER", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// ReadOptions filters Read.
type ReadOptions struct {
	Since      time.Duration // 0 = everything
	WritesOnly bool
	Limit      int // 0 = no limit (newest last)
}

// Read returns records from dir, oldest first.
func Read(dir string, o ReadOptions) ([]Record, error) {
	f, err := os.Open(filepath.Join(dir, FileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var cutoff time.Time
	if o.Since > 0 {
		cutoff = time.Now().Add(-o.Since)
	}
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if !cutoff.IsZero() {
			ns, _ := strconv.ParseInt(r.TimeUnixNano, 10, 64)
			if time.Unix(0, ns).Before(cutoff) {
				continue
			}
		}
		if o.WritesOnly {
			if op, _ := r.Attributes["op"].(string); op != "write" {
				continue
			}
		}
		out = append(out, r)
	}
	if o.Limit > 0 && len(out) > o.Limit {
		out = out[len(out)-o.Limit:]
	}
	return out, sc.Err()
}
