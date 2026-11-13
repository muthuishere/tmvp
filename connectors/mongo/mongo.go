// Package mongo is the tmvp MongoDB connector.
//
// Reads (collections, find, aggregate, count) use the read: credential with
// secondaryPreferred reads, a server-side time limit (maxTimeMS, derived from
// limits.statement_timeout) and a document cap. An aggregate with $out/$merge
// is a write and is refused through the read verb. Writes (insert, update,
// delete, aggregate-out) need tmvp write; --preview counts what would change.
// drop and delete/update with an empty filter are destructive.
//
// Filters, pipelines and documents are MongoDB Extended JSON (relaxed), given
// inline or as @saved from assets/<system>/queries/<name>.json.
package mongo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() { connector.Register("mongo", New) }

// New returns an unopened mongo connector.
func New() connector.Connector { return &Mongo{} }

// Mongo is one client for one env/system/mode.
type Mongo struct {
	t      *connector.Target
	client *mongo.Client
	dbName string
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls [db] — saved queries (@name, all asset roots) then live collections/views"},
	{Name: "collections", Mode: connector.Read, Help: "collections [db] → collection names and types"},
	{Name: "find", Mode: connector.Read, Help: `find <coll> ['{filter}'|@saved] [-F projection='{..}' -F sort='{..}' -F skip=N]`},
	{Name: "aggregate", Mode: connector.Read, Help: `aggregate <coll> '[pipeline]'|@saved ($out/$merge refused: use aggregate-out)`},
	{Name: "count", Mode: connector.Read, Help: `count <coll> ['{filter}'|@saved]`},
	{Name: "insert", Mode: connector.Write, Help: `insert <coll> '{doc}'|'[docs]'|@saved`},
	{Name: "update", Mode: connector.Write, Help: `update <coll> '{filter}' '{update}' (updateMany; empty filter = destructive)`},
	{Name: "delete", Mode: connector.Write, Help: `delete <coll> '{filter}' (deleteMany; empty filter = destructive)`},
	{Name: "aggregate-out", Mode: connector.Write, Help: `aggregate-out <coll> '[pipeline with $out/$merge]'|@saved`},
	{Name: "drop", Mode: connector.Destructive, Help: "drop <coll> (destructive)"},
}

func (m *Mongo) Type() string            { return "mongo" }
func (m *Mongo) Verbs() []connector.Verb { return verbs }

// dbFromURI returns the database in the URI path ("" when absent).
func dbFromURI(uri string) string {
	_, rest, ok := strings.Cut(uri, "://")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		rest = rest[i+1:]
	}
	_, path, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	path, _, _ = strings.Cut(path, "?")
	return path
}

// Open connects with <mode>.uri (or uri) and pings.
func (m *Mongo) Open(ctx context.Context, t *connector.Target) error {
	m.t = t
	uri, err := t.ModeString(ctx, "uri")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(uri, "mongodb://") && !strings.HasPrefix(uri, "mongodb+srv://") {
		return errs.New(errs.Config, "%s.uri is not a mongodb:// URI", t.Mode)
	}
	m.dbName = dbFromURI(uri)
	if db, _ := t.StringOr(ctx, "database", ""); db != "" {
		m.dbName = db
	}
	opts := options.Client().ApplyURI(uri).
		SetAppName("tmvp").
		SetDialer(dialer{t}).
		SetConnectTimeout(10 * time.Second).
		SetServerSelectionTimeout(10 * time.Second).
		SetMaxPoolSize(4)
	if t.Mode == connector.ModeRead {
		opts.SetReadPreference(readpref.SecondaryPreferred())
	} else {
		opts.SetReadPreference(readpref.Primary())
	}
	if err := opts.Validate(); err != nil {
		return errs.New(errs.Config, "%s.uri: invalid mongo options", t.Mode) // never echo the URI
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return errs.New(errs.Config, "%s.uri: invalid mongo options", t.Mode)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.WithoutCancel(ctx))
		return classify(err)
	}
	m.client = client
	return nil
}

type dialer struct{ t *connector.Target }

func (d dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.t.Dial(ctx, network, addr)
}

// Close disconnects.
func (m *Mongo) Close() error {
	if m.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return m.client.Disconnect(ctx)
	}
	return nil
}

// Check implements connector.Checker.
func (m *Mongo) Check(ctx context.Context) error { return classify(m.client.Ping(ctx, nil)) }

// Guard rejects policy problems in inline arguments before Open.
func (m *Mongo) Guard(c connector.Call) error {
	if err := m.guardArgs(c); err != nil {
		return err
	}
	switch c.Verb {
	case "aggregate":
		if len(c.Args) > 1 && !strings.HasPrefix(c.Args[1], "@") {
			p, err := parsePipeline(c.Args[1])
			if err != nil {
				return err
			}
			return readPipeline(p)
		}
	case "aggregate-out":
		if len(c.Args) > 1 && !strings.HasPrefix(c.Args[1], "@") {
			p, err := parsePipeline(c.Args[1])
			if err != nil {
				return err
			}
			if writesStage(p) == "" {
				return errs.New(errs.Usage, "aggregate-out needs a $out or $merge stage").WithHint("use aggregate for reads")
			}
		}
	case "delete", "update":
		if len(c.Args) > 1 && !strings.HasPrefix(c.Args[1], "@") {
			f, err := parseDoc(c.Args[1])
			if err != nil {
				return err
			}
			return destructiveErr(c.Verb, f, c.Destructive)
		}
	}
	return nil
}

func (m *Mongo) guardArgs(c connector.Call) error {
	need := map[string]int{"find": 1, "count": 1, "aggregate": 2, "aggregate-out": 2, "insert": 2, "update": 3, "delete": 2, "drop": 1}
	if n, ok := need[c.Verb]; ok && len(c.Args) < n {
		return errs.New(errs.Usage, "%s needs %d argument(s)", c.Verb, n).WithHint(helpOf(c.Verb))
	}
	return nil
}

func helpOf(verb string) string {
	if v, ok := connector.FindVerb(verbs, verb); ok {
		return v.Help
	}
	return ""
}

func destructiveErr(verb string, filter bson.D, allowed bool) error {
	if len(filter) == 0 && !allowed {
		return errs.New(errs.PolicyBlocked, "%s with an empty filter touches every document: blocked, destructive: false here", verb).
			WithHint("narrow the filter, or set destructive: true on the env/system if this is intended")
	}
	return nil
}

// writesStage returns "$out"/"$merge" when the pipeline writes.
func writesStage(p []bson.D) string {
	for _, st := range p {
		for _, e := range st {
			if e.Key == "$out" || e.Key == "$merge" {
				return e.Key
			}
		}
	}
	return ""
}

func readPipeline(p []bson.D) error {
	if s := writesStage(p); s != "" {
		return errs.New(errs.PolicyBlocked, "aggregate with %s writes data", s).
			WithHint(`tmvp write <env> <system> aggregate-out <coll> '<pipeline>' --reason "why"`)
	}
	return nil
}

// ---- argument parsing ----

func parseDoc(s string) (bson.D, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return bson.D{}, nil
	}
	var d bson.D
	if err := bson.UnmarshalExtJSON([]byte(s), false, &d); err != nil {
		return nil, errs.New(errs.Usage, "not a JSON object: %v", err)
	}
	return d, nil
}

func parsePipeline(s string) ([]bson.D, error) {
	var wrap struct {
		P []bson.D `bson:"p"`
	}
	if err := bson.UnmarshalExtJSON([]byte(`{"p":`+strings.TrimSpace(s)+`}`), false, &wrap); err != nil {
		return nil, errs.New(errs.Usage, "pipeline is not a JSON array of stages: %v", err)
	}
	return wrap.P, nil
}

func parseDocs(s string) ([]any, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		p, err := parsePipeline(s)
		if err != nil {
			return nil, errs.New(errs.Usage, "documents must be a JSON object or array of objects")
		}
		out := make([]any, len(p))
		for i := range p {
			out[i] = p[i]
		}
		return out, nil
	}
	d, err := parseDoc(s)
	if err != nil {
		return nil, err
	}
	return []any{d}, nil
}

// arg resolves an inline value or @saved (assets/<system>/queries/<name>.json).
func (m *Mongo) arg(s string) (string, error) {
	if !strings.HasPrefix(s, "@") {
		return s, nil
	}
	sv, err := m.t.LoadSaved("queries", s, ".json")
	if err != nil {
		return "", errs.Wrap(errs.NotFound, err, "")
	}
	return sv.Body, nil
}

func (m *Mongo) argAt(c connector.Call, i int) (string, error) {
	if i >= len(c.Args) {
		return "", nil
	}
	return m.arg(c.Args[i])
}

// ---- execution ----

func (m *Mongo) limit(c connector.Call) int {
	if c.Limit > 0 {
		return c.Limit
	}
	if m.t != nil && m.t.Limits.Rows > 0 {
		return m.t.Limits.Rows
	}
	return connector.DefaultLimits.Rows
}

// opCtx bounds the operation; the driver turns the deadline into maxTimeMS.
// It follows the call (connector.ServerTimeout), so --timeout moves it.
func (m *Mongo) opCtx(ctx context.Context, c connector.Call) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, connector.ServerTimeout(c, m.t.Limits))
}

func (m *Mongo) db(c connector.Call) (*mongo.Database, error) {
	name := m.dbName
	if v := c.Flags["db"]; v != "" {
		name = v
	}
	if name == "" {
		return nil, errs.New(errs.Config, "no database: put it in the uri path, set database:, or pass -F db=<name>")
	}
	return m.client.Database(name), nil
}

// Do runs one verb.
func (m *Mongo) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := m.guardArgs(c); err != nil {
		return err
	}
	if v, ok := connector.FindVerb(verbs, c.Verb); ok && v.Mode != connector.Read && m.t.Mode != connector.ModeWrite {
		return errs.New(errs.PolicyBlocked, "%s needs tmvp write", c.Verb).
			WithHint(`tmvp write <env> <system> ` + c.Verb + ` ... --reason "why"`)
	}
	ctx, cancel := m.opCtx(ctx, c)
	defer cancel()
	if c.Verb == "ls" {
		if err := m.t.EmitSaved(e, "queries", ".json"); err != nil {
			return errs.Wrap(errs.Runtime, err, "")
		}
		c.Verb = "collections"
	}
	if c.Verb == "collections" && len(c.Args) > 0 {
		c.Flags = withFlag(c.Flags, "db", c.Args[0])
	}
	db, err := m.db(c)
	if err != nil {
		return err
	}
	switch c.Verb {
	case "collections":
		specs, err := db.ListCollectionSpecifications(ctx, bson.D{})
		if err != nil {
			return classify(err)
		}
		for _, s := range specs {
			if err := e.Row(connector.Row{"kind": s.Type, "name": s.Name, "type": s.Type, "read_only": s.ReadOnly}); err != nil {
				return ignoreLimit(err)
			}
		}
		return nil
	case "find":
		return m.find(ctx, db, c, e)
	case "count":
		f, err := m.filterAt(c, 1)
		if err != nil {
			return err
		}
		n, err := db.Collection(c.Args[0]).CountDocuments(ctx, f)
		if err != nil {
			return classify(err)
		}
		return ignoreLimit(e.Row(connector.Row{"collection": c.Args[0], "count": n}))
	case "aggregate", "aggregate-out":
		return m.aggregate(ctx, db, c, e)
	case "insert":
		return m.insert(ctx, db, c, e)
	case "update", "delete":
		return m.modify(ctx, db, c, e)
	case "drop":
		if !(c.Destructive || m.t.Destructive) {
			return errs.New(errs.PolicyBlocked, "drop is destructive: blocked, destructive: false here")
		}
		coll := db.Collection(c.Args[0])
		n, err := coll.EstimatedDocumentCount(ctx)
		if err != nil {
			return classify(err)
		}
		row := connector.Row{"collection": c.Args[0], "documents": n, "preview": c.Preview}
		if !c.Preview {
			if err := coll.Drop(ctx); err != nil {
				return classify(err)
			}
		}
		row["committed"] = !c.Preview
		return ignoreLimit(e.Row(row))
	}
	return errs.New(errs.Usage, "mongo has no verb %q", c.Verb)
}

func withFlag(f map[string]string, k, v string) map[string]string {
	out := map[string]string{k: v}
	for kk, vv := range f {
		if kk != k {
			out[kk] = vv
		}
	}
	return out
}

func (m *Mongo) filterAt(c connector.Call, i int) (bson.D, error) {
	s, err := m.argAt(c, i)
	if err != nil {
		return nil, err
	}
	return parseDoc(s)
}

func (m *Mongo) find(ctx context.Context, db *mongo.Database, c connector.Call, e connector.Emit) error {
	f, err := m.filterAt(c, 1)
	if err != nil {
		return err
	}
	limit := m.limit(c)
	o := options.Find().SetLimit(int64(limit)).SetBatchSize(int32(min(limit, 500)))
	if s := c.Flags["projection"]; s != "" {
		p, err := parseDoc(s)
		if err != nil {
			return err
		}
		o.SetProjection(p)
	}
	if s := c.Flags["sort"]; s != "" {
		p, err := parseDoc(s)
		if err != nil {
			return err
		}
		o.SetSort(p)
	}
	if s := c.Flags["skip"]; s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return errs.New(errs.Usage, "skip must be a non-negative integer")
		}
		o.SetSkip(n)
	}
	cur, err := db.Collection(c.Args[0]).Find(ctx, f, o)
	if err != nil {
		return classify(err)
	}
	return drain(ctx, cur, limit, e)
}

func (m *Mongo) aggregate(ctx context.Context, db *mongo.Database, c connector.Call, e connector.Emit) error {
	s, err := m.argAt(c, 1)
	if err != nil {
		return err
	}
	p, err := parsePipeline(s)
	if err != nil {
		return err
	}
	stage := writesStage(p)
	if c.Verb == "aggregate" {
		if err := readPipeline(p); err != nil {
			return err
		}
	} else if stage == "" {
		return errs.New(errs.Usage, "aggregate-out needs a $out or $merge stage").WithHint("use aggregate for reads")
	}
	limit := m.limit(c)
	if c.Verb == "aggregate-out" && c.Preview {
		// run everything before the write stage and count what would be written
		head := make([]bson.D, 0, len(p))
		for _, st := range p {
			if len(st) > 0 && (st[0].Key == "$out" || st[0].Key == "$merge") {
				break
			}
			head = append(head, st)
		}
		head = append(head, bson.D{{Key: "$count", Value: "n"}})
		cur, err := db.Collection(c.Args[0]).Aggregate(ctx, head)
		if err != nil {
			return classify(err)
		}
		defer cur.Close(ctx)
		var n any = int32(0)
		if cur.Next(ctx) {
			n = cur.Current.Lookup("n").AsInt64()
		}
		return ignoreLimit(e.Row(connector.Row{"collection": c.Args[0], "stage": stage, "documents": n, "preview": true, "committed": false}))
	}
	if c.Verb == "aggregate" {
		p = append(p, bson.D{{Key: "$limit", Value: limit}})
	}
	cur, err := db.Collection(c.Args[0]).Aggregate(ctx, p, options.Aggregate().SetBatchSize(int32(min(limit, 500))))
	if err != nil {
		return classify(err)
	}
	if c.Verb == "aggregate-out" {
		_ = cur.Close(ctx)
		return ignoreLimit(e.Row(connector.Row{"collection": c.Args[0], "stage": stage, "preview": false, "committed": true}))
	}
	return drain(ctx, cur, limit, e)
}

func (m *Mongo) insert(ctx context.Context, db *mongo.Database, c connector.Call, e connector.Emit) error {
	s, err := m.argAt(c, 1)
	if err != nil {
		return err
	}
	docs, err := parseDocs(s)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return errs.New(errs.Usage, "nothing to insert")
	}
	row := connector.Row{"collection": c.Args[0], "documents": len(docs), "preview": c.Preview}
	if c.Preview {
		row["committed"] = false
		return ignoreLimit(e.Row(row))
	}
	res, err := db.Collection(c.Args[0]).InsertMany(ctx, docs)
	if err != nil {
		return classify(err)
	}
	row["inserted"] = len(res.InsertedIDs)
	row["committed"] = true
	return ignoreLimit(e.Row(row))
}

func (m *Mongo) modify(ctx context.Context, db *mongo.Database, c connector.Call, e connector.Emit) error {
	f, err := m.filterAt(c, 1)
	if err != nil {
		return err
	}
	if err := destructiveErr(c.Verb, f, c.Destructive || m.t.Destructive); err != nil {
		return err
	}
	coll := db.Collection(c.Args[0])
	row := connector.Row{"collection": c.Args[0], "preview": c.Preview}
	var upd bson.D
	if c.Verb == "update" {
		s, err := m.argAt(c, 2)
		if err != nil {
			return err
		}
		if upd, err = parseDoc(s); err != nil {
			return err
		}
		if len(upd) == 0 || !strings.HasPrefix(upd[0].Key, "$") {
			return errs.New(errs.Usage, "update document must use operators ($set, $inc, ...)")
		}
	}
	if c.Preview {
		n, err := coll.CountDocuments(ctx, f)
		if err != nil {
			return classify(err)
		}
		row["matched"] = n
		row["committed"] = false
		return ignoreLimit(e.Row(row))
	}
	if c.Verb == "update" {
		res, err := coll.UpdateMany(ctx, f, upd)
		if err != nil {
			return classify(err)
		}
		row["matched"], row["modified"] = res.MatchedCount, res.ModifiedCount
	} else {
		res, err := coll.DeleteMany(ctx, f)
		if err != nil {
			return classify(err)
		}
		row["deleted"] = res.DeletedCount
	}
	row["committed"] = true
	return ignoreLimit(e.Row(row))
}

func drain(ctx context.Context, cur *mongo.Cursor, limit int, e connector.Emit) error {
	defer cur.Close(context.WithoutCancel(ctx))
	n := 0
	for cur.Next(ctx) {
		r, err := toRow(cur.Current)
		if err != nil {
			return errs.Wrap(errs.Runtime, err, "decode document")
		}
		if err := e.Row(r); err != nil {
			return ignoreLimit(err)
		}
		if n++; n >= limit {
			return nil
		}
	}
	return classify(cur.Err())
}

// toRow turns a BSON document into a flat JSON-friendly row (relaxed
// Extended JSON: ObjectId → {"$oid": ...}, dates → {"$date": ...}).
func toRow(raw bson.Raw) (connector.Row, error) {
	b, err := bson.MarshalExtJSON(raw, false, false)
	if err != nil {
		return nil, err
	}
	var r connector.Row
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	for k, v := range r {
		if mm, ok := v.(map[string]any); ok && len(mm) == 1 {
			if s, ok := mm["$oid"]; ok {
				r[k] = s
			} else if d, ok := mm["$date"]; ok {
				r[k] = d
			}
		}
	}
	return r, nil
}

func ignoreLimit(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}

// classify maps driver errors to tmvp codes.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var te *errs.Error
	if errors.As(err, &te) {
		return err
	}
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		switch ce.Code {
		case 18:
			return errs.AuthError(errors.New("mongo: authentication failed"))
		case 13:
			return errs.New(errs.PolicyBlocked, "mongo: not authorized: %s", ce.Message).
				WithHint("the read credential cannot do this; writes go through tmvp write")
		case 50:
			return errs.New(errs.GuardRejected, "operation exceeded maxTimeMS guard").
				WithHint("narrow the filter/pipeline or raise limits.statement_timeout")
		}
	}
	msg := err.Error()
	if strings.Contains(msg, "AuthenticationFailed") || strings.Contains(msg, "auth error") || strings.Contains(msg, "unable to authenticate") {
		return errs.AuthError(errors.New("mongo: authentication failed"))
	}
	if mongo.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
		if strings.Contains(msg, "server selection") {
			return errs.New(errs.Unreachable, "cannot reach mongo (server selection timeout)")
		}
		return errs.New(errs.GuardRejected, "operation timed out (maxTimeMS guard)").
			WithHint("narrow the filter/pipeline or raise limits.statement_timeout")
	}
	if mongo.IsNetworkError(err) || strings.Contains(msg, "server selection") {
		return errs.New(errs.Unreachable, "cannot reach mongo")
	}
	if ce.Code != 0 {
		return errs.New(errs.Runtime, "mongo %d: %s", ce.Code, ce.Message)
	}
	return errs.New(errs.Runtime, "mongo: %s", firstLine(msg))
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return fmt.Sprint(s)
}
