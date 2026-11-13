// Package nats is the tmvp NATS / JetStream connector.
//
// Config: <mode>.url (nats://host:4222, comma-separated servers) and one of
// <mode>.creds (a .creds file: `${file:secrets.x}`, parsed in memory — never
// written to disk), <mode>.token, <mode>.user + <mode>.password, or
// <mode>.nkey (seed). All connections dial through the via-aware dialer; nats
// layers TLS on top when the server requires it.
//
// Reads: streams, consumers <stream>, sub <subject> (core subscribe, bounded
// by count and time), kv-get <bucket> <key>. Writes: pub, kv-put (tmvp write;
// --preview reports the target without publishing). purge and stream-delete
// are destructive.
package nats

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() { connector.Register("nats", New) }

// New returns an unopened nats connector.
func New() connector.Connector { return &NATS{} }

// NATS is one connection for one env/system/mode.
type NATS struct {
	t  *connector.Target
	nc *nats.Conn
	js jetstream.JetStream
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — JetStream streams, KV buckets and object stores (kind, name, msgs, bytes)"},
	{Name: "streams", Mode: connector.Read, Help: "streams → name, subjects, msgs, bytes, consumers"},
	{Name: "consumers", Mode: connector.Read, Help: "consumers <stream> → name, pending, ack pending, redelivered"},
	{Name: "sub", Mode: connector.Read, Help: "sub <subject> [-F wait=5s] (core subscribe; stops at --limit msgs or wait)"},
	{Name: "kv-get", Mode: connector.Read, Help: "kv-get <bucket> <key>"},
	{Name: "pub", Mode: connector.Write, Default: true, Help: "pub <subject> <data>"},
	{Name: "kv-put", Mode: connector.Write, Help: "kv-put <bucket> <key> <value>"},
	{Name: "purge", Mode: connector.Destructive, Help: "purge <stream> [-F subject=<filter>] (destructive)"},
	{Name: "stream-delete", Mode: connector.Destructive, Help: "stream-delete <stream> (destructive)"},
}

func (n *NATS) Type() string            { return "nats" }
func (n *NATS) Verbs() []connector.Verb { return verbs }

type dialer struct {
	t       *connector.Target
	timeout time.Duration
}

func (d dialer) Dial(network, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	return d.t.Dial(ctx, network, addr)
}

// has reports <mode>.key or key.
func has(t *connector.Target, key string) bool {
	return t.Has(string(t.Mode)+"."+key) || t.Has(key)
}

// modeFile reads <mode>.key (or key) as bytes, in memory.
func modeFile(ctx context.Context, t *connector.Target, key string) ([]byte, error) {
	p := string(t.Mode) + "." + key
	if !t.Has(p) {
		if t.Mode == connector.ModeWrite {
			return nil, errs.New(errs.Config, "%s not set", p)
		}
		p = key
	}
	return t.File(ctx, p)
}

// authOptions builds auth options from the target (no network).
func authOptions(ctx context.Context, t *connector.Target) ([]nats.Option, error) {
	switch {
	case has(t, "creds"):
		b, err := modeFile(ctx, t, "creds")
		if err != nil {
			return nil, err
		}
		jwt, err := nkeys.ParseDecoratedJWT(b)
		if err != nil || jwt == "" {
			return nil, errs.New(errs.Config, "%s.creds is not a NATS credentials file", t.Mode)
		}
		kp, err := nkeys.ParseDecoratedUserNKey(b)
		if err != nil {
			return nil, errs.New(errs.Config, "%s.creds has no user seed", t.Mode)
		}
		return []nats.Option{nats.UserJWT(
			func() (string, error) { return jwt, nil },
			func(nonce []byte) ([]byte, error) { return kp.Sign(nonce) },
		)}, nil
	case has(t, "nkey"):
		b, err := modeFile(ctx, t, "nkey")
		if err != nil {
			return nil, err
		}
		kp, err := nkeys.FromSeed([]byte(strings.TrimSpace(string(b))))
		if err != nil {
			return nil, errs.New(errs.Config, "%s.nkey is not an nkey seed", t.Mode)
		}
		pub, err := kp.PublicKey()
		if err != nil {
			return nil, errs.New(errs.Config, "%s.nkey is not an nkey seed", t.Mode)
		}
		return []nats.Option{nats.Nkey(pub, kp.Sign)}, nil
	case has(t, "token"):
		tok, err := t.ModeString(ctx, "token")
		if err != nil {
			return nil, err
		}
		return []nats.Option{nats.Token(tok)}, nil
	case has(t, "password"):
		user, err := t.ModeString(ctx, "user")
		if err != nil {
			return nil, err
		}
		pw, err := t.ModeString(ctx, "password")
		if err != nil {
			return nil, err
		}
		return []nats.Option{nats.UserInfo(user, pw)}, nil
	}
	return nil, nil
}

// Open connects.
func (n *NATS) Open(ctx context.Context, t *connector.Target) error {
	n.t = t
	url, err := t.ModeString(ctx, "url")
	if err != nil {
		return err
	}
	auth, err := authOptions(ctx, t)
	if err != nil {
		return err
	}
	opts := append([]nats.Option{
		nats.Name("tmvp"),
		nats.Timeout(10 * time.Second),
		nats.SetCustomDialer(dialer{t: t, timeout: 10 * time.Second}),
		nats.MaxReconnects(2),
		nats.NoCallbacksAfterClientClose(),
	}, auth...)
	if v, _ := t.StringOr(ctx, "tls", ""); v == "true" {
		opts = append(opts, nats.Secure())
	}
	type res struct {
		nc  *nats.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { nc, err := nats.Connect(url, opts...); ch <- res{nc, err} }()
	var r res
	select {
	case r = <-ch:
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.nc != nil {
				r.nc.Close()
			}
		}()
		return errs.Wrap(errs.Timeout, ctx.Err(), "")
	}
	if r.err != nil {
		return classify(r.err)
	}
	n.nc = r.nc
	js, err := jetstream.New(r.nc)
	if err != nil {
		r.nc.Close()
		return classify(err)
	}
	n.js = js
	return nil
}

// Close drains nothing: tmvp never holds subscriptions across calls.
func (n *NATS) Close() error {
	if n.nc != nil {
		n.nc.Close()
	}
	return nil
}

// Check implements connector.Checker.
func (n *NATS) Check(ctx context.Context) error {
	if n.nc == nil || !n.nc.IsConnected() {
		return errs.New(errs.Unreachable, "nats: not connected")
	}
	return classify(n.nc.FlushWithContext(ctx))
}

var minArgs = map[string]int{"consumers": 1, "sub": 1, "kv-get": 2, "pub": 2, "kv-put": 3, "purge": 1, "stream-delete": 1}

// Guard validates arguments and destructive policy before Open.
func (n *NATS) Guard(c connector.Call) error {
	if m := minArgs[c.Verb]; len(c.Args) < m {
		v, _ := connector.FindVerb(verbs, c.Verb)
		return errs.New(errs.Usage, "%s needs %d argument(s)", c.Verb, m).WithHint(v.Help)
	}
	switch c.Verb {
	case "sub":
		if strings.ContainsAny(c.Args[0], " \t") || c.Args[0] == "" {
			return errs.New(errs.Usage, "bad subject %q", c.Args[0])
		}
		if _, err := wait(c, time.Hour); err != nil {
			return err
		}
	case "pub":
		if strings.ContainsAny(c.Args[0], "*> \t") {
			return errs.New(errs.Usage, "cannot publish to a wildcard subject %q", c.Args[0])
		}
	case "purge", "stream-delete":
		if !c.Destructive {
			return errs.New(errs.PolicyBlocked, "%s is destructive: blocked, destructive: false here", c.Verb)
		}
	}
	return nil
}

// wait returns the sub wait time: -F wait (default 5s), capped at max.
func wait(c connector.Call, max time.Duration) (time.Duration, error) {
	d := 5 * time.Second
	if s := c.Flags["wait"]; s != "" {
		var err error
		if d, err = time.ParseDuration(s); err != nil || d <= 0 {
			if secs, err2 := strconv.Atoi(s); err2 == nil && secs > 0 {
				d = time.Duration(secs) * time.Second
			} else {
				return 0, errs.New(errs.Usage, "bad wait %q (e.g. 5s)", s)
			}
		}
	}
	return min(d, max), nil
}

func (n *NATS) limit(c connector.Call) int {
	if c.Limit > 0 {
		return c.Limit
	}
	if n.t != nil && n.t.Limits.Rows > 0 {
		return n.t.Limits.Rows
	}
	return connector.DefaultLimits.Rows
}

func (n *NATS) opCtx(ctx context.Context, c connector.Call) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, connector.ServerTimeout(c, n.t.Limits))
}

// Do runs one verb.
func (n *NATS) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := n.Guard(connector.Call{Verb: c.Verb, Args: c.Args, Flags: c.Flags, Destructive: c.Destructive || n.t.Destructive}); err != nil {
		return err
	}
	if v, ok := connector.FindVerb(verbs, c.Verb); ok && v.Mode != connector.Read && n.t.Mode != connector.ModeWrite {
		return errs.New(errs.PolicyBlocked, "%s needs tmvp write", c.Verb).
			WithHint(`tmvp write <env> <system> ` + c.Verb + ` ... --reason "why"`)
	}
	if c.Verb == "sub" {
		return n.sub(ctx, c, e)
	}
	ctx, cancel := n.opCtx(ctx, c)
	defer cancel()
	switch c.Verb {
	case "ls":
		l := n.js.ListStreams(ctx)
		for si := range l.Info() {
			kind, name := "stream", si.Config.Name
			switch {
			case strings.HasPrefix(name, "KV_"):
				kind, name = "kv", strings.TrimPrefix(name, "KV_")
			case strings.HasPrefix(name, "OBJ_"):
				kind, name = "object-store", strings.TrimPrefix(name, "OBJ_")
			}
			if err := e.Row(connector.Row{"kind": kind, "name": name, "subjects": strings.Join(si.Config.Subjects, ","),
				"msgs": si.State.Msgs, "bytes": si.State.Bytes}); err != nil {
				return ignoreLimit(err)
			}
		}
		return classify(l.Err())
	case "streams":
		l := n.js.ListStreams(ctx)
		for si := range l.Info() {
			if err := e.Row(connector.Row{"name": si.Config.Name, "subjects": strings.Join(si.Config.Subjects, ","),
				"msgs": si.State.Msgs, "bytes": si.State.Bytes, "consumers": si.State.Consumers,
				"created": si.Created.UTC().Format(time.RFC3339)}); err != nil {
				return ignoreLimit(err)
			}
		}
		return classify(l.Err())
	case "consumers":
		s, err := n.js.Stream(ctx, c.Args[0])
		if err != nil {
			return classify(err)
		}
		l := s.ListConsumers(ctx)
		for ci := range l.Info() {
			if err := e.Row(connector.Row{"stream": ci.Stream, "name": ci.Name, "pending": ci.NumPending,
				"ack_pending": ci.NumAckPending, "redelivered": ci.NumRedelivered, "waiting": ci.NumWaiting}); err != nil {
				return ignoreLimit(err)
			}
		}
		return classify(l.Err())
	case "kv-get":
		kv, err := n.js.KeyValue(ctx, c.Args[0])
		if err != nil {
			return classify(err)
		}
		ent, err := kv.Get(ctx, c.Args[1])
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ignoreLimit(e.Row(connector.Row{"bucket": c.Args[0], "key": c.Args[1], "exists": false, "value": nil}))
		}
		if err != nil {
			return classify(err)
		}
		return ignoreLimit(e.Row(connector.Row{"bucket": c.Args[0], "key": c.Args[1], "exists": true,
			"value": text(ent.Value()), "revision": ent.Revision(), "created": ent.Created().UTC().Format(time.RFC3339)}))
	case "pub":
		return n.pub(ctx, c, e)
	case "kv-put":
		kv, err := n.js.KeyValue(ctx, c.Args[0])
		if err != nil {
			return classify(err)
		}
		row := connector.Row{"bucket": c.Args[0], "key": c.Args[1], "preview": c.Preview}
		if c.Preview {
			ent, err := kv.Get(ctx, c.Args[1])
			switch {
			case errors.Is(err, jetstream.ErrKeyNotFound):
				row["exists"] = false
			case err != nil:
				return classify(err)
			default:
				row["exists"], row["revision"] = true, ent.Revision()
			}
			row["committed"] = false
			return ignoreLimit(e.Row(row))
		}
		rev, err := kv.Put(ctx, c.Args[1], []byte(c.Args[2]))
		if err != nil {
			return classify(err)
		}
		row["revision"], row["committed"] = rev, true
		return ignoreLimit(e.Row(row))
	case "purge", "stream-delete":
		s, err := n.js.Stream(ctx, c.Args[0])
		if err != nil {
			return classify(err)
		}
		info, err := s.Info(ctx)
		if err != nil {
			return classify(err)
		}
		row := connector.Row{"stream": c.Args[0], "msgs": info.State.Msgs, "bytes": info.State.Bytes, "preview": c.Preview}
		if subj := c.Flags["subject"]; subj != "" && c.Verb == "purge" {
			row["subject"] = subj
		}
		if !c.Preview {
			if c.Verb == "purge" {
				var po []jetstream.StreamPurgeOpt
				if subj := c.Flags["subject"]; subj != "" {
					po = append(po, jetstream.WithPurgeSubject(subj))
				}
				err = s.Purge(ctx, po...)
			} else {
				err = n.js.DeleteStream(ctx, c.Args[0])
			}
			if err != nil {
				return classify(err)
			}
		}
		row["committed"] = !c.Preview
		return ignoreLimit(e.Row(row))
	}
	return errs.New(errs.Usage, "nats has no verb %q", c.Verb)
}

func (n *NATS) pub(ctx context.Context, c connector.Call, e connector.Emit) error {
	subj, data := c.Args[0], []byte(c.Args[1])
	row := connector.Row{"subject": subj, "bytes": len(data), "preview": c.Preview}
	if stream, err := n.js.StreamNameBySubject(ctx, subj); err == nil {
		row["stream"] = stream
	}
	if c.Preview {
		row["committed"] = false
		return ignoreLimit(e.Row(row))
	}
	if s, ok := row["stream"].(string); ok && s != "" {
		ack, err := n.js.Publish(ctx, subj, data)
		if err != nil {
			return classify(err)
		}
		row["seq"] = ack.Sequence
	} else {
		if err := n.nc.Publish(subj, data); err != nil {
			return classify(err)
		}
		if err := n.nc.FlushWithContext(ctx); err != nil {
			return classify(err)
		}
	}
	row["committed"] = true
	return ignoreLimit(e.Row(row))
}

// sub is a bounded core subscription: stops at the message cap or wait time.
func (n *NATS) sub(ctx context.Context, c connector.Call, e connector.Emit) error {
	maxWait := connector.CallTimeout(c, n.t.Limits)
	w, err := wait(c, maxWait)
	if err != nil {
		return err
	}
	limit := n.limit(c)
	ch := make(chan *nats.Msg, min(limit, 1024))
	s, err := n.nc.ChanSubscribe(c.Args[0], ch)
	if err != nil {
		return classify(err)
	}
	defer s.Unsubscribe() //nolint:errcheck
	_ = s.AutoUnsubscribe(limit)
	_ = s.SetPendingLimits(limit, 8<<20) // slow-consumer guard
	timer := time.NewTimer(w)
	defer timer.Stop()
	for got := 0; got < limit; {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case m := <-ch:
			got++
			r := connector.Row{"subject": m.Subject, "bytes": len(m.Data), "data": text(m.Data)}
			if m.Reply != "" {
				r["reply"] = m.Reply
			}
			if err := e.Row(r); err != nil {
				return ignoreLimit(err)
			}
		}
	}
	return nil
}

func text(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return "base64:" + base64.StdEncoding.EncodeToString(b)
}

func ignoreLimit(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}

// classify maps client errors to tmvp codes.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var te *errs.Error
	if errors.As(err, &te) {
		return err
	}
	msg := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, nats.ErrAuthorization), errors.Is(err, nats.ErrAuthExpired),
		errors.Is(err, nats.ErrAuthRevoked), strings.Contains(msg, "authorization violation"),
		strings.Contains(msg, "authentication expired"):
		return errs.AuthError(errors.New("nats: authorization violation"))
	case strings.Contains(msg, "permissions violation"):
		return errs.New(errs.PolicyBlocked, "nats: permissions violation").WithHint("this credential may not do that; writes go through tmvp write")
	case errors.Is(err, nats.ErrNoServers), errors.Is(err, nats.ErrConnectionClosed):
		return errs.New(errs.Unreachable, "cannot reach nats")
	case errors.Is(err, jetstream.ErrJetStreamNotEnabled), errors.Is(err, jetstream.ErrJetStreamNotEnabledForAccount):
		return errs.New(errs.Unsupported, "jetstream is not enabled on this server/account")
	case errors.Is(err, jetstream.ErrStreamNotFound), errors.Is(err, jetstream.ErrBucketNotFound), errors.Is(err, jetstream.ErrConsumerNotFound):
		return errs.New(errs.NotFound, "nats: %s", err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, nats.ErrTimeout):
		return errs.New(errs.Timeout, "nats: timed out")
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return errs.Wrap(errs.Unreachable, err, "")
	}
	return errs.New(errs.Runtime, "nats: %s", err.Error())
}
