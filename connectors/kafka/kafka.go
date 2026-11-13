// Package kafka is the tmvp connector for Apache Kafka (and wire-compatible
// brokers: Redpanda, MSK, Confluent, Event Hubs' Kafka endpoint). It embeds
// franz-go; every broker connection is dialled through the target's via chain.
//
// Config fields (mode-aware: read.* / write.* override the top level):
//
//	brokers: "b1:9092,b2:9092"         # or a YAML list of literal host:port
//	sasl: {mechanism: plain|scram-sha-256|scram-sha-512, user: ..., password: ${secrets.x}}
//	tls: true                          # or {ca: ${file:secrets.ca}, cert: ..., key: ..., server_name: ..., insecure: false}
//
// consume never joins a consumer group and never commits offsets.
package kafka

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() { connector.Register("kafka", New) }

// New returns an unopened kafka connector.
func New() connector.Connector { return &Kafka{} }

// Kafka is one cluster connection (admin + producer client).
type Kafka struct {
	t    *connector.Target
	opts []kgo.Opt // base options, reused for the per-call consume client
	cl   *kgo.Client
	adm  *kadm.Client

	lastEOF bool // Check saw the broker hang up (SASL rejection on some brokers)
}

const maxValue = 64 << 10 // bytes of a record value/key shown per row

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls [substring] — topics (partitions) + saved messages/*.json"},
	{Name: "topics", Mode: connector.Read, Help: "topics [substring] — topics with partition count, replication factor"},
	{Name: "describe", Mode: connector.Read, Help: "describe <topic> — per-partition leader/replicas/isr/start/end offsets, then non-default topic configs"},
	{Name: "groups", Mode: connector.Read, Help: "groups — consumer groups (state, protocol type)"},
	{Name: "lag", Mode: connector.Read, Help: "lag <group> — per topic/partition committed offset, end offset, lag, member"},
	{Name: "consume", Mode: connector.Read, Help: "consume <topic> [-F from=earliest|latest-N|<offset>|<RFC3339>] [-F partition=N] [-F wait=10s] — bounded by --limit and time; never joins a group or commits (default from=latest-10 per partition)"},
	{Name: "health", Mode: connector.Read, Help: "health — brokers, controller, cluster id"},
	{Name: "produce", Mode: connector.Write, Default: true, Help: "produce <topic> <value|@saved> [-F key=k] [-F partition=N] (--preview shows the record)"},
	{Name: "create-topic", Mode: connector.Write, Help: "create-topic <topic> [-F partitions=1] [-F replication=-1] [-F config.<k>=<v>]"},
	{Name: "delete-topic", Mode: connector.Destructive, Help: "delete-topic <topic>"},
	{Name: "delete-records", Mode: connector.Destructive, Help: "delete-records <topic> -F before=<offset>|latest [-F partition=N] — truncate partitions before an offset"},
	{Name: "reset-offsets", Mode: connector.Destructive, Help: "reset-offsets <group> <topic> -F to=earliest|latest|<RFC3339> — group must be empty (--preview shows old→new)"},
}

func (k *Kafka) Type() string            { return "kafka" }
func (k *Kafka) Verbs() []connector.Verb { return verbs }

func verbMode(name string) (connector.VerbMode, bool) {
	v, ok := connector.FindVerb(verbs, name)
	return v.Mode, ok
}

// Guard validates arguments without touching the cluster.
func (k *Kafka) Guard(c connector.Call) error {
	need := map[string]int{"describe": 1, "lag": 1, "consume": 1, "produce": 2, "create-topic": 1, "delete-topic": 1, "delete-records": 1, "reset-offsets": 2}
	if n, ok := need[c.Verb]; ok && len(c.Args) < n {
		return errs.New(errs.Usage, "%s needs %d argument(s)", c.Verb, n).WithHint(helpOf(c.Verb))
	}
	switch c.Verb {
	case "consume":
		if _, err := parseFrom(c.Flags["from"]); err != nil {
			return err
		}
	case "delete-records":
		if c.Flags["before"] == "" {
			return errs.New(errs.Usage, "delete-records needs -F before=<offset>|latest")
		}
	case "reset-offsets":
		if c.Flags["to"] == "" {
			return errs.New(errs.Usage, "reset-offsets needs -F to=earliest|latest|<RFC3339>")
		}
	}
	for _, a := range c.Args[:min(len(c.Args), 1)] {
		if c.Verb != "produce" && (strings.ContainsAny(a, "*?") || a == "") {
			return errs.New(errs.Usage, "%q: wildcards are not accepted", a)
		}
	}
	if m, ok := verbMode(c.Verb); ok {
		if m.Writable() && !c.Write {
			return errs.New(errs.PolicyBlocked, "%s mutates the cluster", c.Verb).WithHint("tmvp write <env> <system> " + c.Verb + " … --reason \"…\"")
		}
		if m == connector.Destructive && !c.Destructive {
			return errs.New(errs.PolicyBlocked, "%s is destructive; this env/system does not allow destructive ops", c.Verb).WithHint("set destructive: true on the system to allow it")
		}
	}
	return nil
}

func helpOf(verb string) string {
	v, _ := connector.FindVerb(verbs, verb)
	return v.Help
}

// Open builds the client and verifies brokers + SASL with a metadata request.
func (k *Kafka) Open(ctx context.Context, t *connector.Target) error {
	k.t = t
	brokers, err := k.brokers(ctx)
	if err != nil {
		return err
	}
	tlsCfg, err := k.tlsConfig(ctx)
	if err != nil {
		return err
	}
	mech, err := k.sasl(ctx)
	if err != nil {
		return err
	}
	dial := func(ctx context.Context, network, host string) (net.Conn, error) {
		conn, err := t.Dial(ctx, network, host)
		if err != nil || tlsCfg == nil {
			return conn, err
		}
		cfg := tlsCfg.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName, _, _ = net.SplitHostPort(host)
		}
		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		return tc, nil
	}
	k.opts = []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.Dialer(dial),
		kgo.ClientID("tmvp"),
		kgo.DialTimeout(10 * time.Second),
		kgo.RequestRetries(2),
		kgo.RetryTimeout(t.Limits.Timeout),
		kgo.ProducerBatchCompression(kgo.NoCompression()),
	}
	if mech != nil {
		k.opts = append(k.opts, kgo.SASL(mech))
	}
	cl, err := kgo.NewClient(k.opts...)
	if err != nil {
		return errs.Wrap(errs.Config, err, "kafka client")
	}
	k.cl, k.adm = cl, kadm.NewClient(cl)
	if err := k.Check(ctx); err != nil {
		if mech != nil && errs.CodeOf(err) == errs.Runtime && k.lastEOF {
			// Some brokers (and kfake) drop the connection instead of
			// answering SaslAuthenticate with an error code.
			err = errs.AuthError(errors.New("kafka: broker closed the connection during SASL authentication"))
		}
		cl.Close()
		k.cl = nil
		return err
	}
	return nil
}

func (k *Kafka) brokers(ctx context.Context) ([]string, error) {
	t := k.t
	key := "brokers"
	if t.Has(string(t.Mode) + ".brokers") {
		key = string(t.Mode) + ".brokers"
	}
	var out []string
	switch raw, _ := t.Raw(key); v := raw.(type) {
	case []any:
		for _, b := range v {
			out = append(out, strings.TrimSpace(fmt.Sprint(b)))
		}
	case nil:
		return nil, errs.New(errs.Config, "%s: brokers is not set", t.System).WithHint(`brokers: "host1:9092,host2:9092"`)
	default:
		s, err := t.String(ctx, key)
		if err != nil {
			return nil, err
		}
		for _, b := range strings.Split(s, ",") {
			if b = strings.TrimSpace(b); b != "" {
				out = append(out, b)
			}
		}
	}
	for _, b := range out {
		if _, _, err := net.SplitHostPort(b); err != nil || strings.Contains(b, "${") {
			return nil, errs.New(errs.Config, "%s: broker %q is not host:port", t.System, b).WithHint("interpolated brokers go in one comma-separated string")
		}
	}
	if len(out) == 0 {
		return nil, errs.New(errs.Config, "%s: brokers is empty", t.System)
	}
	return out, nil
}

// field resolves "<mode>.<path>" then "<path>".
func (k *Kafka) has(path string) (string, bool) {
	if p := string(k.t.Mode) + "." + path; k.t.Has(p) {
		return p, true
	}
	return path, k.t.Has(path)
}

func (k *Kafka) tlsConfig(ctx context.Context) (*tls.Config, error) {
	p, ok := k.has("tls")
	if !ok {
		return nil, nil
	}
	raw, _ := k.t.Raw(p)
	switch v := raw.(type) {
	case bool:
		if !v {
			return nil, nil
		}
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil
	case string:
		if v == "true" {
			return &tls.Config{MinVersion: tls.VersionTLS12}, nil
		}
		return nil, nil
	case map[string]any:
	default:
		return nil, errs.New(errs.Config, "%s: tls must be true or a map {ca, cert, key, server_name, insecure}", k.t.System)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if k.t.Has(p + ".ca") {
		ca, err := k.t.File(ctx, p+".ca")
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errs.New(errs.Config, "%s: tls.ca holds no PEM certificate", k.t.System)
		}
		cfg.RootCAs = pool
	}
	if k.t.Has(p+".cert") || k.t.Has(p+".key") {
		cert, err := k.t.File(ctx, p+".cert")
		if err != nil {
			return nil, err
		}
		key, err := k.t.File(ctx, p+".key")
		if err != nil {
			return nil, err
		}
		pair, err := tls.X509KeyPair(cert, key)
		if err != nil {
			return nil, errs.New(errs.Config, "%s: tls.cert/tls.key do not form a valid key pair", k.t.System)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	cfg.ServerName, _ = k.t.StringOr(ctx, p+".server_name", "")
	if ins, _ := k.t.StringOr(ctx, p+".insecure", "false"); ins == "true" {
		cfg.InsecureSkipVerify = true //nolint:gosec // opt-in per system
	}
	return cfg, nil
}

func (k *Kafka) sasl(ctx context.Context) (sasl.Mechanism, error) {
	p, ok := k.has("sasl")
	if !ok {
		return nil, nil
	}
	mechName, err := k.t.StringOr(ctx, p+".mechanism", "plain")
	if err != nil {
		return nil, err
	}
	user, err := k.t.String(ctx, p+".user")
	if err != nil {
		return nil, err
	}
	pass, err := k.t.String(ctx, p+".password")
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(mechName) {
	case "plain":
		return plain.Auth{User: user, Pass: pass}.AsMechanism(), nil
	case "scram-sha-256":
		return scram.Auth{User: user, Pass: pass}.AsSha256Mechanism(), nil
	case "scram-sha-512":
		return scram.Auth{User: user, Pass: pass}.AsSha512Mechanism(), nil
	}
	return nil, errs.New(errs.Config, "%s: sasl.mechanism %q is not plain|scram-sha-256|scram-sha-512", k.t.System, mechName)
}

// Close releases the client.
func (k *Kafka) Close() error {
	if k.cl != nil {
		k.cl.Close()
	}
	return nil
}

// Check issues a metadata request.
func (k *Kafka) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, k.t.Limits.Timeout)
	defer cancel()
	_, err := k.adm.BrokerMetadata(ctx)
	k.lastEOF = errors.Is(err, io.EOF)
	return k.mapErr(ctx, err, "metadata")
}

// mapErr turns franz-go/kerr errors into tmvp codes. Never includes credentials.
func (k *Kafka) mapErr(ctx context.Context, err error, what string) error {
	if err == nil {
		return nil
	}
	if errs.Is(err) {
		return err
	}
	switch {
	case errors.Is(err, kerr.SaslAuthenticationFailed), errors.Is(err, kerr.IllegalSaslState):
		return errs.AuthError(fmt.Errorf("kafka %s: SASL authentication failed", what))
	case errors.Is(err, kerr.UnsupportedSaslMechanism):
		return errs.New(errs.Config, "kafka %s: broker does not support the configured SASL mechanism", what)
	case errors.Is(err, kerr.TopicAuthorizationFailed), errors.Is(err, kerr.GroupAuthorizationFailed),
		errors.Is(err, kerr.ClusterAuthorizationFailed), errors.Is(err, kerr.TransactionalIDAuthorizationFailed):
		return errs.New(errs.PolicyBlocked, "kafka %s: %v (ACL denies this principal)", what, err)
	case errors.Is(err, kerr.UnknownTopicOrPartition), errors.Is(err, kerr.GroupIDNotFound):
		return errs.New(errs.NotFound, "kafka %s: %v", what, err)
	case errors.Is(err, kerr.NonEmptyGroup):
		return errs.New(errs.PolicyBlocked, "kafka %s: group has active members", what).WithHint("stop the consumers first")
	case errors.Is(err, kerr.TopicAlreadyExists), errors.Is(err, kerr.InvalidReplicationFactor), errors.Is(err, kerr.InvalidPartitions),
		errors.Is(err, kerr.InvalidTopicException), errors.Is(err, kerr.InvalidConfig), errors.Is(err, kerr.OffsetOutOfRange):
		return errs.New(errs.Usage, "kafka %s: %v", what, err)
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return errs.New(errs.Timeout, "kafka %s timed out", what)
	}
	var ne net.Error
	var oe *net.OpError
	if errors.As(err, &oe) || errors.As(err, &ne) || errors.Is(err, kgo.ErrClientClosed) || strings.Contains(err.Error(), "unable to dial") {
		return errs.New(errs.Unreachable, "kafka %s: cannot reach brokers", what)
	}
	var ke *kerr.Error
	if errors.As(err, &ke) {
		return errs.New(errs.Runtime, "kafka %s: %s", what, ke.Message)
	}
	return errs.New(errs.Runtime, "kafka %s: %v", what, err)
}

func ignoreLimit(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}

// Do runs one verb.
func (k *Kafka) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := k.Guard(c); err != nil {
		return err
	}
	if _, ok := verbMode(c.Verb); !ok {
		return errs.New(errs.Usage, "kafka has no verb %q", c.Verb)
	}
	ctx, cancel := context.WithTimeout(ctx, connector.CallTimeout(c, k.t.Limits))
	defer cancel()
	err := k.do(ctx, c, e)
	return k.mapErr(ctx, ignoreLimit(err), c.Verb)
}

func (k *Kafka) do(ctx context.Context, c connector.Call, e connector.Emit) error {
	switch c.Verb {
	case "ls", "topics":
		return k.topics(ctx, c, e)
	case "describe":
		return k.describe(ctx, c.Args[0], e)
	case "groups":
		gs, err := k.adm.ListGroups(ctx)
		if err != nil {
			return err
		}
		for _, g := range gs.Sorted() {
			if err := e.Row(connector.Row{"group": g.Group, "state": g.State, "protocol_type": g.ProtocolType, "coordinator": g.Coordinator}); err != nil {
				return err
			}
		}
		return nil
	case "lag":
		return k.lag(ctx, c.Args[0], e)
	case "consume":
		return k.consume(ctx, c, e)
	case "health":
		return k.health(ctx, e)
	case "produce":
		return k.produce(ctx, c, e)
	case "create-topic":
		return k.createTopic(ctx, c, e)
	case "delete-topic":
		if c.Preview {
			return k.describe(ctx, c.Args[0], e)
		}
		rs, err := k.adm.DeleteTopics(ctx, c.Args[0])
		if err != nil {
			return err
		}
		for _, r := range rs.Sorted() {
			if r.Err != nil {
				return r.Err
			}
			if err := e.Row(connector.Row{"topic": r.Topic, "deleted": true}); err != nil {
				return err
			}
		}
		return nil
	case "delete-records":
		return k.deleteRecords(ctx, c, e)
	case "reset-offsets":
		return k.resetOffsets(ctx, c, e)
	}
	return errs.New(errs.Usage, "kafka has no verb %q", c.Verb)
}

func (k *Kafka) topics(ctx context.Context, c connector.Call, e connector.Emit) error {
	td, err := k.adm.ListTopics(ctx)
	if err != nil {
		return err
	}
	filter := ""
	if len(c.Args) > 0 {
		filter = c.Args[0]
	}
	for _, d := range td.Sorted() {
		if filter != "" && !strings.Contains(d.Topic, filter) {
			continue
		}
		rf := 0
		if len(d.Partitions) > 0 {
			rf = len(d.Partitions.Sorted()[0].Replicas)
		}
		r := connector.Row{"name": d.Topic, "partitions": len(d.Partitions), "replication": rf}
		if c.Verb == "ls" {
			r = connector.Row{"kind": "topic", "name": d.Topic, "partitions": len(d.Partitions)}
		}
		if err := e.Row(r); err != nil {
			return err
		}
	}
	if c.Verb == "ls" {
		return k.t.EmitSaved(e, "messages", ".json")
	}
	return nil
}

func (k *Kafka) describe(ctx context.Context, topic string, e connector.Emit) error {
	td, err := k.adm.ListTopics(ctx, topic)
	if err != nil {
		return err
	}
	d, ok := td[topic]
	if !ok || d.Err != nil {
		if ok {
			return d.Err
		}
		return errs.New(errs.NotFound, "topic %q not found", topic)
	}
	starts, err := k.adm.ListStartOffsets(ctx, topic)
	if err != nil {
		return err
	}
	ends, err := k.adm.ListEndOffsets(ctx, topic)
	if err != nil {
		return err
	}
	for _, p := range d.Partitions.Sorted() {
		s, _ := starts.Lookup(topic, p.Partition)
		en, _ := ends.Lookup(topic, p.Partition)
		r := connector.Row{"kind": "partition", "topic": topic, "partition": p.Partition, "leader": p.Leader,
			"replicas": ints(p.Replicas), "isr": ints(p.ISR), "start": s.Offset, "end": en.Offset, "messages": en.Offset - s.Offset}
		if err := e.Row(r); err != nil {
			return err
		}
	}
	rc, err := k.adm.DescribeTopicConfigs(ctx, topic)
	if err != nil {
		return err
	}
	for _, res := range rc {
		for _, cf := range res.Configs {
			if cf.Source.String() == "DEFAULT_CONFIG" {
				continue
			}
			v := ""
			if cf.Sensitive {
				v = "(sensitive)"
			} else if cf.Value != nil {
				v = *cf.Value
			}
			if err := e.Row(connector.Row{"kind": "config", "topic": topic, "key": cf.Key, "value": v, "source": cf.Source.String()}); err != nil {
				return err
			}
		}
	}
	return nil
}

func ints(xs []int32) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = strconv.Itoa(int(x))
	}
	return strings.Join(s, ",")
}

func (k *Kafka) lag(ctx context.Context, group string, e connector.Emit) error {
	ls, err := k.adm.Lag(ctx, group)
	if err != nil {
		return err
	}
	g, ok := ls[group]
	if !ok {
		return errs.New(errs.NotFound, "group %q not found", group)
	}
	if g.DescribeErr != nil {
		return g.DescribeErr
	}
	if g.FetchErr != nil {
		return g.FetchErr
	}
	if g.State == "Dead" {
		return errs.New(errs.NotFound, "group %q not found", group)
	}
	for _, l := range g.Lag.Sorted() {
		member := ""
		if l.Member != nil {
			member = l.Member.ClientID + "@" + l.Member.ClientHost
		}
		r := connector.Row{"group": group, "state": g.State, "topic": l.Topic, "partition": l.Partition,
			"committed": l.Commit.At, "end": l.End.Offset, "lag": l.Lag, "member": member}
		if err := e.Row(r); err != nil {
			return err
		}
	}
	return nil
}

func (k *Kafka) health(ctx context.Context, e connector.Emit) error {
	md, err := k.adm.BrokerMetadata(ctx)
	if err != nil {
		return err
	}
	for _, b := range md.Brokers {
		rack := ""
		if b.Rack != nil {
			rack = *b.Rack
		}
		r := connector.Row{"broker": b.NodeID, "host": b.Host, "port": b.Port, "rack": rack,
			"controller": b.NodeID == md.Controller, "cluster": md.Cluster}
		if err := e.Row(r); err != nil {
			return err
		}
	}
	return nil
}

// fromSpec is a parsed -F from=.
type fromSpec struct {
	kind   string // earliest | latest | offset | time
	n      int64  // latest-N count or absolute offset
	millis int64
}

func parseFrom(s string) (fromSpec, error) {
	switch {
	case s == "":
		return fromSpec{kind: "latest", n: 10}, nil
	case s == "earliest" || s == "beginning":
		return fromSpec{kind: "earliest"}, nil
	case s == "latest":
		return fromSpec{kind: "latest", n: 0}, nil
	case strings.HasPrefix(s, "latest-"):
		n, err := strconv.ParseInt(strings.TrimPrefix(s, "latest-"), 10, 64)
		if err != nil || n < 0 {
			break
		}
		return fromSpec{kind: "latest", n: n}, nil
	default:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= 0 {
			return fromSpec{kind: "offset", n: n}, nil
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return fromSpec{kind: "time", millis: ts.UnixMilli()}, nil
		}
	}
	return fromSpec{}, errs.New(errs.Usage, "from=%q: want earliest, latest, latest-N, an offset or an RFC3339 time", s)
}

func (k *Kafka) partitionFilter(c connector.Call) (int32, bool, error) {
	p := c.Flags["partition"]
	if p == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(p, 10, 32)
	if err != nil || n < 0 {
		return 0, false, errs.New(errs.Usage, "partition=%q is not a partition number", p)
	}
	return int32(n), true, nil
}

func (k *Kafka) consume(ctx context.Context, c connector.Call, e connector.Emit) error {
	topic := c.Args[0]
	from, _ := parseFrom(c.Flags["from"])
	onlyP, filtered, err := k.partitionFilter(c)
	if err != nil {
		return err
	}
	starts, err := k.adm.ListStartOffsets(ctx, topic)
	if err != nil {
		return err
	}
	if err := starts.Error(); err != nil {
		return err
	}
	ends, err := k.adm.ListEndOffsets(ctx, topic)
	if err != nil {
		return err
	}
	var at kadm.ListedOffsets
	if from.kind == "time" {
		if at, err = k.adm.ListOffsetsAfterMilli(ctx, from.millis, topic); err != nil {
			return err
		}
	}
	plan := map[int32]kgo.Offset{}
	remaining := map[int32]int64{} // partition → end offset snapshot not yet reached
	ends.Each(func(o kadm.ListedOffset) {
		if o.Topic != topic || (filtered && o.Partition != onlyP) || o.Err != nil {
			return
		}
		s, _ := starts.Lookup(topic, o.Partition)
		var start int64
		switch from.kind {
		case "earliest":
			start = s.Offset
		case "latest":
			start = max(o.Offset-from.n, s.Offset)
		case "offset":
			start = min(max(from.n, s.Offset), o.Offset)
		case "time":
			a, ok := at.Lookup(topic, o.Partition)
			start = o.Offset
			if ok && a.Offset >= 0 {
				start = a.Offset
			}
		}
		plan[o.Partition] = kgo.NewOffset().At(start)
		if start < o.Offset {
			remaining[o.Partition] = o.Offset
		}
	})
	if len(plan) == 0 {
		return errs.New(errs.NotFound, "topic %q has no matching partitions", topic)
	}
	wait := time.Duration(0)
	if w := c.Flags["wait"]; w != "" {
		if wait, err = time.ParseDuration(w); err != nil || wait <= 0 {
			return errs.New(errs.Usage, "wait=%q is not a duration", w)
		}
	}
	if len(remaining) == 0 && wait == 0 {
		return nil // nothing to read and not asked to wait for new records
	}
	cl, err := kgo.NewClient(append(append([]kgo.Opt{}, k.opts...),
		kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: plan}),
		kgo.FetchMaxWait(500*time.Millisecond))...)
	if err != nil {
		return err
	}
	defer cl.Close()
	budget := connector.CallTimeout(c, k.t.Limits) - time.Second
	if wait > 0 && wait < budget {
		budget = wait
	}
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for {
		fs := cl.PollFetches(cctx)
		if cctx.Err() != nil {
			return nil // time bound reached: what we have is the answer
		}
		var ferr error
		fs.EachError(func(_ string, _ int32, err error) {
			if ferr == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				ferr = err
			}
		})
		if ferr != nil {
			return ferr
		}
		var stop error
		fs.EachRecord(func(r *kgo.Record) {
			if stop != nil {
				return
			}
			stop = e.Row(recordRow(r))
			if end, ok := remaining[r.Partition]; ok && r.Offset+1 >= end {
				delete(remaining, r.Partition)
			}
		})
		if stop != nil {
			return stop
		}
		if wait == 0 && len(remaining) == 0 {
			return nil
		}
	}
}

func recordRow(r *kgo.Record) connector.Row {
	row := connector.Row{"topic": r.Topic, "partition": r.Partition, "offset": r.Offset,
		"time": r.Timestamp.UTC().Format(time.RFC3339Nano)}
	put := func(name string, b []byte) {
		if b == nil {
			row[name] = nil
			return
		}
		trunc := len(b) > maxValue
		if trunc {
			b = b[:maxValue]
			row[name+"_truncated"] = true
		}
		if utf8.Valid(b) {
			row[name] = string(b)
		} else {
			row[name+"_b64"] = base64.StdEncoding.EncodeToString(b)
		}
	}
	put("key", r.Key)
	put("value", r.Value)
	if len(r.Headers) > 0 {
		h := make([]string, 0, len(r.Headers))
		for _, x := range r.Headers {
			h = append(h, x.Key+"="+string(x.Value))
		}
		row["headers"] = strings.Join(h, ",")
	}
	return row
}

func (k *Kafka) produce(ctx context.Context, c connector.Call, e connector.Emit) error {
	topic, value := c.Args[0], strings.Join(c.Args[1:], " ")
	if strings.HasPrefix(value, "@") && len(c.Args) == 2 {
		s, err := k.t.LoadSaved("messages", value, ".json")
		if err != nil {
			return errs.Wrap(errs.NotFound, err, "")
		}
		value = strings.TrimSpace(s.Body)
	}
	rec := &kgo.Record{Topic: topic, Value: []byte(value)}
	if key, ok := c.Flags["key"]; ok {
		rec.Key = []byte(key)
	}
	if p, ok, err := k.partitionFilter(c); err != nil {
		return err
	} else if ok {
		rec.Partition = p
	}
	if c.Preview {
		r := connector.Row{"preview": true, "topic": topic, "key": c.Flags["key"], "value": value, "bytes": len(value)}
		return e.Row(r)
	}
	var cl *kgo.Client = k.cl
	if _, ok := c.Flags["partition"]; ok {
		pc, err := kgo.NewClient(append(append([]kgo.Opt{}, k.opts...), kgo.RecordPartitioner(kgo.ManualPartitioner()))...)
		if err != nil {
			return err
		}
		defer pc.Close()
		cl = pc
	}
	res := cl.ProduceSync(ctx, rec)
	r, err := res.First()
	if err != nil {
		return err
	}
	return e.Row(connector.Row{"topic": r.Topic, "partition": r.Partition, "offset": r.Offset, "time": r.Timestamp.UTC().Format(time.RFC3339Nano)})
}

func (k *Kafka) createTopic(ctx context.Context, c connector.Call, e connector.Emit) error {
	parts, err := strconv.ParseInt(orDefault(c.Flags["partitions"], "1"), 10, 32)
	if err != nil || parts < 1 {
		return errs.New(errs.Usage, "partitions must be ≥ 1")
	}
	rf, err := strconv.ParseInt(orDefault(c.Flags["replication"], "-1"), 10, 16)
	if err != nil || rf == 0 || rf < -1 {
		return errs.New(errs.Usage, "replication must be ≥ 1 or -1 (broker default)")
	}
	cfg := map[string]*string{}
	keys := make([]string, 0)
	for k2, v := range c.Flags {
		if strings.HasPrefix(k2, "config.") {
			v := v
			cfg[strings.TrimPrefix(k2, "config.")] = &v
			keys = append(keys, k2)
		}
	}
	sort.Strings(keys)
	if c.Preview {
		return e.Row(connector.Row{"preview": true, "topic": c.Args[0], "partitions": parts, "replication": rf, "configs": strings.Join(keys, ",")})
	}
	r, err := k.adm.CreateTopic(ctx, int32(parts), int16(rf), cfg, c.Args[0])
	if err != nil {
		return err
	}
	return e.Row(connector.Row{"topic": r.Topic, "partitions": r.NumPartitions, "replication": r.ReplicationFactor, "created": true})
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (k *Kafka) deleteRecords(ctx context.Context, c connector.Call, e connector.Emit) error {
	topic := c.Args[0]
	onlyP, filtered, err := k.partitionFilter(c)
	if err != nil {
		return err
	}
	ends, err := k.adm.ListEndOffsets(ctx, topic)
	if err != nil {
		return err
	}
	if err := ends.Error(); err != nil {
		return err
	}
	before := c.Flags["before"]
	var abs int64 = -1
	if before != "latest" {
		if abs, err = strconv.ParseInt(before, 10, 64); err != nil || abs < 0 {
			return errs.New(errs.Usage, "before=%q: want an offset or latest", before)
		}
	}
	var os kadm.Offsets
	ends.Each(func(o kadm.ListedOffset) {
		if filtered && o.Partition != onlyP {
			return
		}
		at := o.Offset
		if abs >= 0 {
			at = min(abs, o.Offset)
		}
		os.AddOffset(topic, o.Partition, at, -1)
	})
	if len(os) == 0 {
		return errs.New(errs.NotFound, "topic %q has no matching partitions", topic)
	}
	if c.Preview {
		var rerr error
		os.Each(func(o kadm.Offset) {
			if rerr == nil {
				rerr = e.Row(connector.Row{"preview": true, "topic": o.Topic, "partition": o.Partition, "delete_before": o.At})
			}
		})
		return rerr
	}
	rs, err := k.adm.DeleteRecords(ctx, os)
	if err != nil {
		return err
	}
	for _, r := range rs.Sorted() {
		if r.Err != nil {
			return r.Err
		}
		if err := e.Row(connector.Row{"topic": r.Topic, "partition": r.Partition, "low_watermark": r.LowWatermark}); err != nil {
			return err
		}
	}
	return nil
}

func (k *Kafka) resetOffsets(ctx context.Context, c connector.Call, e connector.Emit) error {
	group, topic, to := c.Args[0], c.Args[1], c.Flags["to"]
	var target kadm.ListedOffsets
	var err error
	switch to {
	case "earliest":
		target, err = k.adm.ListStartOffsets(ctx, topic)
	case "latest":
		target, err = k.adm.ListEndOffsets(ctx, topic)
	default:
		ts, perr := time.Parse(time.RFC3339, to)
		if perr != nil {
			return errs.New(errs.Usage, "to=%q: want earliest, latest or an RFC3339 time", to)
		}
		target, err = k.adm.ListOffsetsAfterMilli(ctx, ts.UnixMilli(), topic)
	}
	if err != nil {
		return err
	}
	if err := target.Error(); err != nil {
		return err
	}
	current, err := k.adm.FetchOffsets(ctx, group)
	if err != nil {
		return err
	}
	os := target.Offsets()
	var rerr error
	if c.Preview {
		os.Each(func(o kadm.Offset) {
			old := int64(-1)
			if cur, ok := current.Lookup(o.Topic, o.Partition); ok {
				old = cur.At
			}
			if rerr == nil {
				rerr = e.Row(connector.Row{"preview": true, "group": group, "topic": o.Topic, "partition": o.Partition, "from": old, "to": o.At})
			}
		})
		return rerr
	}
	rs, err := k.adm.CommitOffsets(ctx, group, os)
	if err != nil {
		return err
	}
	if err := rs.Error(); err != nil {
		return err
	}
	rs.Each(func(r kadm.OffsetResponse) {
		if rerr == nil {
			rerr = e.Row(connector.Row{"group": group, "topic": r.Topic, "partition": r.Partition, "offset": r.At})
		}
	})
	return rerr
}
