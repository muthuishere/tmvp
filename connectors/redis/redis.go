// Package redis is the tmvp Redis connector.
//
// Config: <mode>.url (redis:// or rediss://; or url), optional <mode>.password
// (kept out of the url so validate can check it), optional tls: true to force
// TLS (tls_server_name overrides SNI). Connections go through the target's
// via-aware dialer; TLS is layered on top of that connection.
//
// Reads (info, get, scan, ttl, type) only issue read commands; scan is bounded
// by the row cap, an iteration budget and the statement timeout — KEYS is
// never used. Writes (set, del, expire) need tmvp write; --preview reports
// what exists. flushdb/flushall are destructive.
package redis

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() { connector.Register("redis", New) }

// New returns an unopened redis connector.
func New() connector.Connector { return &Redis{} }

// Redis is one client for one env/system/mode.
type Redis struct {
	t *connector.Target
	c goredis.UniversalClient
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls [-F sep=:] — key prefixes from a bounded SCAN sample (≤5000 keys), with counts and dbsize"},
	{Name: "info", Mode: connector.Read, Help: "info [section] → key/value rows"},
	{Name: "get", Mode: connector.Read, Help: "get <key> (string values; other types → use type)"},
	{Name: "scan", Mode: connector.Read, Help: "scan [pattern] [-F type=hash] (bounded SCAN, never KEYS)"},
	{Name: "ttl", Mode: connector.Read, Help: "ttl <key>"},
	{Name: "type", Mode: connector.Read, Help: "type <key>"},
	{Name: "set", Mode: connector.Write, Help: "set <key> <value> [ttl e.g. 10m]"},
	{Name: "del", Mode: connector.Write, Help: "del <key> [key...] (literal keys, no patterns)"},
	{Name: "expire", Mode: connector.Write, Help: "expire <key> <ttl e.g. 1h>"},
	{Name: "flushdb", Mode: connector.Destructive, Help: "flushdb (destructive)"},
	{Name: "flushall", Mode: connector.Destructive, Help: "flushall (destructive)"},
}

func (r *Redis) Type() string            { return "redis" }
func (r *Redis) Verbs() []connector.Verb { return verbs }

// options builds client options from the target without dialing.
func options(ctx context.Context, t *connector.Target) (*goredis.Options, error) {
	u, err := t.ModeString(ctx, "url")
	if err != nil {
		return nil, err
	}
	opt, err := goredis.ParseURL(u)
	if err != nil {
		return nil, errs.New(errs.Config, "%s.url is not a valid redis:// URL", t.Mode) // never echo the URL
	}
	if t.Has(string(t.Mode)+".password") || t.Has("password") {
		pw, err := t.ModeString(ctx, "password")
		if err != nil {
			return nil, err
		}
		opt.Password = pw
	}
	if t.Has(string(t.Mode)+".username") || t.Has("username") {
		if opt.Username, err = t.ModeString(ctx, "username"); err != nil {
			return nil, err
		}
	}
	if v, _ := t.StringOr(ctx, "tls", ""); v == "true" && opt.TLSConfig == nil {
		opt.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if opt.TLSConfig != nil {
		if sn, _ := t.StringOr(ctx, "tls_server_name", ""); sn != "" {
			opt.TLSConfig.ServerName = sn
		}
		if opt.TLSConfig.ServerName == "" {
			host, _, _ := net.SplitHostPort(opt.Addr)
			opt.TLSConfig.ServerName = host
		}
	}
	st := t.Limits.StatementTimeout
	if st <= 0 {
		st = connector.DefaultLimits.StatementTimeout
	}
	opt.DialTimeout = 10 * time.Second
	opt.ReadTimeout = st
	opt.WriteTimeout = st
	// Honour the caller's context deadline on every socket op, so the one
	// call-level deadline Do sets bounds the whole verb; Read/WriteTimeout
	// remain the per-op fallback only when ctx carries no deadline.
	opt.ContextTimeoutEnabled = true
	opt.PoolSize = 4
	opt.MaxRetries = 1
	opt.ClientName = "tmvp"
	// Custom dialer: go-redis does not apply TLSConfig when Dialer is set, so do it here.
	tlsCfg := opt.TLSConfig
	opt.Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network == "unix" {
			return nil, errors.New("unix sockets are not supported through tmvp")
		}
		conn, err := t.Dial(ctx, "tcp", addr)
		if err != nil || tlsCfg == nil {
			return conn, err
		}
		tc := tls.Client(conn, tlsCfg.Clone())
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		return tc, nil
	}
	return opt, nil
}

// Open connects and pings.
func (r *Redis) Open(ctx context.Context, t *connector.Target) error {
	r.t = t
	opt, err := options(ctx, t)
	if err != nil {
		return err
	}
	c := goredis.NewClient(opt)
	if err := c.Ping(ctx).Err(); err != nil {
		c.Close()
		return classify(err)
	}
	r.c = c
	return nil
}

// Close closes the client.
func (r *Redis) Close() error {
	if r.c != nil {
		return r.c.Close()
	}
	return nil
}

// Check implements connector.Checker.
func (r *Redis) Check(ctx context.Context) error { return classify(r.c.Ping(ctx).Err()) }

var minArgs = map[string]int{"get": 1, "ttl": 1, "type": 1, "set": 2, "del": 1, "expire": 2}

// Guard validates arguments before Open.
func (r *Redis) Guard(c connector.Call) error {
	if n := minArgs[c.Verb]; len(c.Args) < n {
		v, _ := connector.FindVerb(verbs, c.Verb)
		return errs.New(errs.Usage, "%s needs %d argument(s)", c.Verb, n).WithHint(v.Help)
	}
	switch c.Verb {
	case "set":
		if len(c.Args) > 2 {
			if _, err := parseTTL(c.Args[2]); err != nil {
				return err
			}
		}
	case "expire":
		if _, err := parseTTL(c.Args[1]); err != nil {
			return err
		}
	case "flushdb", "flushall":
		if !c.Destructive {
			return errs.New(errs.PolicyBlocked, "%s is destructive: blocked, destructive: false here", c.Verb)
		}
	}
	return nil
}

// parseTTL accepts Go durations (10m, 1h30m) or plain seconds.
func parseTTL(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Second {
		return 0, errs.New(errs.Usage, "bad ttl %q (use seconds or a duration like 10m)", s)
	}
	return d, nil
}

func (r *Redis) limit(c connector.Call) int {
	if c.Limit > 0 {
		return c.Limit
	}
	if r.t != nil && r.t.Limits.Rows > 0 {
		return r.t.Limits.Rows
	}
	return connector.DefaultLimits.Rows
}

// Do runs one verb.
func (r *Redis) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := r.Guard(connector.Call{Verb: c.Verb, Args: c.Args, Destructive: c.Destructive || r.t.Destructive}); err != nil {
		return err
	}
	if v, ok := connector.FindVerb(verbs, c.Verb); ok && v.Mode != connector.Read && r.t.Mode != connector.ModeWrite {
		return errs.New(errs.PolicyBlocked, "%s needs tmvp write", c.Verb).
			WithHint(`tmvp write <env> <system> ` + c.Verb + ` ... --reason "why"`)
	}
	// One deadline for the whole verb (connector.CallTimeout): a multi-command
	// read like scan/ls cannot stretch past it one command at a time.
	budget := connector.CallTimeout(c, r.t.Limits)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	err := r.do(ctx, c, e)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errs.New(errs.Timeout, "redis %s exceeded the call timeout %s", c.Verb, budget)
	}
	return err
}

func (r *Redis) do(ctx context.Context, c connector.Call, e connector.Emit) error {
	switch c.Verb {
	case "info":
		return r.info(ctx, c, e)
	case "get":
		v, err := r.c.Get(ctx, c.Args[0]).Result()
		if errors.Is(err, goredis.Nil) {
			return ignoreLimit(e.Row(connector.Row{"key": c.Args[0], "exists": false, "value": nil}))
		}
		if err != nil {
			return classify(err)
		}
		return ignoreLimit(e.Row(connector.Row{"key": c.Args[0], "exists": true, "value": v}))
	case "ttl":
		d, err := r.c.TTL(ctx, c.Args[0]).Result()
		if err != nil {
			return classify(err)
		}
		return ignoreLimit(e.Row(ttlRow(c.Args[0], d)))
	case "type":
		v, err := r.c.Type(ctx, c.Args[0]).Result()
		if err != nil {
			return classify(err)
		}
		return ignoreLimit(e.Row(connector.Row{"key": c.Args[0], "type": v}))
	case "scan":
		return r.scan(ctx, c, e)
	case "ls":
		return r.ls(ctx, c, e)
	case "set", "del", "expire", "flushdb", "flushall":
		return r.write(ctx, c, e)
	}
	return errs.New(errs.Usage, "redis has no verb %q", c.Verb)
}

func ttlRow(key string, d time.Duration) connector.Row {
	row := connector.Row{"key": key}
	switch {
	case d == -2 || d == -2*time.Second || d == -2*time.Nanosecond:
		row["exists"], row["ttl_s"] = false, nil
	case d < 0:
		row["exists"], row["ttl_s"] = true, nil // no expiry
	default:
		row["exists"], row["ttl_s"] = true, int64(d/time.Second)
	}
	return row
}

func (r *Redis) info(ctx context.Context, c connector.Call, e connector.Emit) error {
	s, err := r.c.Info(ctx, c.Args...).Result()
	if err != nil {
		return classify(err)
	}
	for _, row := range parseInfo(s) {
		if err := e.Row(row); err != nil {
			return ignoreLimit(err)
		}
	}
	return nil
}

// parseInfo turns INFO text into {section,key,value} rows.
func parseInfo(s string) []connector.Row {
	var out []connector.Row
	section := ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			section = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "#")))
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out = append(out, connector.Row{"section": section, "key": k, "value": v})
	}
	return out
}

// lsSample bounds `ls`: keys sampled and SCAN round trips.
const lsSample, lsIter = 5000, 50

// ls samples keys with a bounded SCAN and groups them by prefix (text before
// the first separator, default ":"). Values are never read.
func (r *Redis) ls(ctx context.Context, c connector.Call, e connector.Emit) error {
	sep := c.Flags["sep"]
	if sep == "" {
		sep = ":"
	}
	size, err := r.c.DBSize(ctx).Result()
	if err != nil {
		return classify(err)
	}
	counts := map[string]int{}
	examples := map[string]string{}
	var cursor uint64
	sampled := 0
	for iter := 0; iter < lsIter && sampled < lsSample; iter++ {
		keys, next, err := r.c.Scan(ctx, cursor, "*", 500).Result()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				break
			}
			return classify(err)
		}
		for _, k := range keys {
			p := k
			if i := strings.Index(k, sep); i >= 0 {
				p = k[:i+len(sep)] + "*"
			}
			if counts[p] == 0 {
				examples[p] = k
			}
			counts[p]++
			sampled++
		}
		if cursor = next; cursor == 0 {
			break
		}
	}
	prefixes := make([]string, 0, len(counts))
	for p := range counts {
		prefixes = append(prefixes, p)
	}
	sort.Slice(prefixes, func(i, j int) bool {
		if counts[prefixes[i]] != counts[prefixes[j]] {
			return counts[prefixes[i]] > counts[prefixes[j]]
		}
		return prefixes[i] < prefixes[j]
	})
	if err := e.Row(connector.Row{"kind": "dbsize", "name": "keys", "count": size, "sampled": sampled}); err != nil {
		return ignoreLimit(err)
	}
	for _, p := range prefixes {
		if err := e.Row(connector.Row{"kind": "prefix", "name": p, "count": counts[p], "example": examples[p]}); err != nil {
			return ignoreLimit(err)
		}
	}
	return nil
}

// scan walks SCAN cursors with COUNT, stopping at the row cap, an iteration
// budget, or the statement timeout — never KEYS.
func (r *Redis) scan(ctx context.Context, c connector.Call, e connector.Emit) error {
	pattern := "*"
	if len(c.Args) > 0 {
		pattern = c.Args[0]
	}
	limit := r.limit(c)
	const count, maxIter = 500, 2000
	var cursor uint64
	seen := 0
	for iter := 0; iter < maxIter; iter++ {
		var keys []string
		var err error
		if t := c.Flags["type"]; t != "" {
			keys, cursor, err = r.c.ScanType(ctx, cursor, pattern, count, t).Result()
		} else {
			keys, cursor, err = r.c.Scan(ctx, cursor, pattern, count).Result()
		}
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return errs.New(errs.GuardRejected, "scan stopped by timeout guard after %d keys", seen).
					WithHint("narrow the pattern or raise limits.statement_timeout")
			}
			return classify(err)
		}
		for _, k := range keys {
			if err := e.Row(connector.Row{"key": k}); err != nil {
				return ignoreLimit(err)
			}
			if seen++; seen >= limit {
				return nil
			}
		}
		if cursor == 0 {
			return nil
		}
	}
	return errs.New(errs.GuardRejected, "scan stopped after %d iterations (%d keys)", maxIter, seen).
		WithHint("narrow the pattern")
}

func (r *Redis) write(ctx context.Context, c connector.Call, e connector.Emit) error {
	row := connector.Row{"preview": c.Preview}
	if c.Preview {
		switch c.Verb {
		case "set", "expire":
			k := c.Args[0]
			typ, err := r.c.Type(ctx, k).Result()
			if err != nil {
				return classify(err)
			}
			d, err := r.c.TTL(ctx, k).Result()
			if err != nil {
				return classify(err)
			}
			row = ttlRow(k, d)
			row["preview"], row["type"] = true, typ
		case "del":
			n, err := r.c.Exists(ctx, c.Args...).Result()
			if err != nil {
				return classify(err)
			}
			row["keys_existing"] = n
		case "flushdb", "flushall":
			n, err := r.c.DBSize(ctx).Result()
			if err != nil {
				return classify(err)
			}
			row["db_keys"] = n
		}
		row["committed"] = false
		return ignoreLimit(e.Row(row))
	}
	var err error
	switch c.Verb {
	case "set":
		var ttl time.Duration
		if len(c.Args) > 2 {
			ttl, _ = parseTTL(c.Args[2])
		}
		err = r.c.Set(ctx, c.Args[0], c.Args[1], ttl).Err()
		row["key"] = c.Args[0]
	case "del":
		var n int64
		n, err = r.c.Del(ctx, c.Args...).Result()
		row["deleted"] = n
	case "expire":
		ttl, _ := parseTTL(c.Args[1])
		var ok bool
		ok, err = r.c.Expire(ctx, c.Args[0], ttl).Result()
		row["key"], row["applied"] = c.Args[0], ok
	case "flushdb":
		err = r.c.FlushDB(ctx).Err()
	case "flushall":
		err = r.c.FlushAll(ctx).Err()
	}
	if err != nil {
		return classify(err)
	}
	row["committed"] = true
	return ignoreLimit(e.Row(row))
}

func ignoreLimit(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}

// classify maps client errors to tmvp codes. Redis error replies never
// contain the password.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var te *errs.Error
	if errors.As(err, &te) {
		return err
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "WRONGPASS"), strings.HasPrefix(msg, "NOAUTH"),
		strings.Contains(msg, "invalid password"), strings.Contains(msg, "invalid username-password"):
		return errs.AuthError(errors.New("redis: authentication failed"))
	case strings.HasPrefix(msg, "NOPERM"):
		return errs.New(errs.PolicyBlocked, "redis: %s", msg).WithHint("this credential's ACL does not allow it; writes go through tmvp write")
	case strings.HasPrefix(msg, "READONLY"):
		return errs.New(errs.PolicyBlocked, "redis: replica is read-only").WithHint("point write.url at the primary")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(errs.Timeout, err, "")
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return errs.New(errs.GuardRejected, "redis command timed out").WithHint("raise limits.statement_timeout")
		}
		return errs.Wrap(errs.Unreachable, err, "")
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return errs.Wrap(errs.Unreachable, err, "")
	}
	return errs.New(errs.Runtime, "redis: %s", msg)
}
