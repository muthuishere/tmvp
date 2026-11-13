package mongo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func TestVerbs(t *testing.T) {
	f, ok := connector.Lookup("mongo")
	if !ok {
		t.Fatal("not registered")
	}
	want := map[string]connector.VerbMode{"collections": connector.Read, "find": connector.Read, "aggregate": connector.Read, "count": connector.Read,
		"insert": connector.Write, "update": connector.Write, "delete": connector.Write, "aggregate-out": connector.Write, "drop": connector.Destructive}
	for n, m := range want {
		if v, ok := connector.FindVerb(f().Verbs(), n); !ok || v.Mode != m {
			t.Errorf("verb %s: %+v", n, v)
		}
	}
}

func TestGuard(t *testing.T) {
	m := &Mongo{}
	cases := []struct {
		c    connector.Call
		want errs.Code
	}{
		{connector.Call{Verb: "aggregate", Args: []string{"c", `[{"$match":{}},{"$out":"x"}]`}}, errs.PolicyBlocked},
		{connector.Call{Verb: "aggregate", Args: []string{"c", `[{"$match":{}},{"$merge":{"into":"x"}}]`}}, errs.PolicyBlocked},
		{connector.Call{Verb: "aggregate", Args: []string{"c", `[{"$group":{"_id":"$a"}}]`}}, ""},
		{connector.Call{Verb: "aggregate", Args: []string{"c", `not json`}}, errs.Usage},
		{connector.Call{Verb: "aggregate-out", Args: []string{"c", `[{"$match":{}}]`}}, errs.Usage},
		{connector.Call{Verb: "delete", Args: []string{"c", `{}`}}, errs.PolicyBlocked},
		{connector.Call{Verb: "delete", Args: []string{"c", ` `}}, errs.PolicyBlocked},
		{connector.Call{Verb: "delete", Args: []string{"c", `{}`}, Destructive: true}, ""},
		{connector.Call{Verb: "delete", Args: []string{"c", `{"_id":{"$oid":"64b7f0c2a1b2c3d4e5f60718"}}`}}, ""},
		{connector.Call{Verb: "update", Args: []string{"c", `{}`, `{"$set":{"a":1}}`}}, errs.PolicyBlocked},
		{connector.Call{Verb: "update", Args: []string{"c", `{"a":1}`}}, errs.Usage},
		{connector.Call{Verb: "find"}, errs.Usage},
	}
	for _, c := range cases {
		if got := errs.CodeOf(m.Guard(c.c)); got != c.want {
			t.Errorf("%s %v → %s want %s", c.c.Verb, c.c.Args, got, c.want)
		}
	}
}

func TestWriteRefusedInReadMode(t *testing.T) {
	m := &Mongo{t: connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{})}
	for _, c := range []connector.Call{{Verb: "insert", Args: []string{"c", "{}"}}, {Verb: "drop", Args: []string{"c"}, Destructive: true}} {
		if err := m.Do(context.Background(), c, &connector.Collector{}); errs.CodeOf(err) != errs.PolicyBlocked {
			t.Errorf("%s: %v", c.Verb, err)
		}
	}
}

func TestParsing(t *testing.T) {
	if db := dbFromURI("mongodb+srv://u:FAKE_SECRET_123@c.example/app?retryWrites=true"); db != "app" {
		t.Fatal(db)
	}
	if db := dbFromURI("mongodb://a:1,b:2/?replicaSet=rs"); db != "" {
		t.Fatal(db)
	}
	docs, err := parseDocs(`[{"a":1},{"b":2}]`)
	if err != nil || len(docs) != 2 {
		t.Fatal(err)
	}
	r, err := toRow(mustRaw(t, bson.D{{Key: "_id", Value: bson.NewObjectID()}, {Key: "n", Value: int32(3)}, {Key: "s", Value: "x"}}))
	if err != nil || r["n"] != float64(3) || r["s"] != "x" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, ok := r["_id"].(string); !ok {
		t.Fatalf("oid not flattened: %#v", r["_id"])
	}
}

func mustRaw(t *testing.T, d bson.D) bson.Raw {
	b, err := bson.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOpenBadURIDoesNotEchoIt(t *testing.T) {
	m := &Mongo{}
	tg := connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{"read": map[string]any{"uri": "mongodb://u:FAKE_SECRET_123@h:notaport/db"}})
	err := m.Open(context.Background(), tg)
	if err == nil || strings.Contains(err.Error(), "FAKE_SECRET_123") {
		t.Fatalf("%v", err)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		e    error
		want errs.Code
	}{
		{mongo.CommandError{Code: 18, Message: "Authentication failed."}, errs.Auth},
		{mongo.CommandError{Code: 13, Message: "not authorized"}, errs.PolicyBlocked},
		{mongo.CommandError{Code: 50, Message: "operation exceeded time"}, errs.GuardRejected},
		{mongo.CommandError{Code: 2, Message: "bad value"}, errs.Runtime},
		{errors.New("connection() error: auth error: sasl conversation"), errs.Auth},
	}
	for _, tc := range cases {
		e, want := tc.e, tc.want
		if got := errs.CodeOf(classify(e)); got != want {
			t.Errorf("%v → %s want %s", e, got, want)
		}
	}
}

// TestLive: TMVP_TEST_MONGO_URL (with a database in the path). sec run TMVP_TEST_MONGO_URL -- go test ./connectors/mongo -run Live
func TestLive(t *testing.T) {
	u := os.Getenv("TMVP_TEST_MONGO_URL")
	if u == "" {
		t.Skip("TMVP_TEST_MONGO_URL not set")
	}
	ctx := context.Background()
	open := func(mode connector.Mode) *Mongo {
		m := &Mongo{}
		if err := m.Open(ctx, connector.NewTarget(connector.Target{Mode: mode}, connector.StaticFields{string(mode): map[string]any{"uri": u}, "database": "tmvp_live"})); err != nil {
			t.Fatal(err)
		}
		return m
	}
	w, r := open(connector.ModeWrite), open(connector.ModeRead)
	defer w.Close()
	defer r.Close()
	var c connector.Collector
	if err := w.Do(ctx, connector.Call{Verb: "insert", Args: []string{"t", `[{"a":1},{"a":2}]`}}, &c); err != nil {
		t.Fatal(err)
	}
	c = connector.Collector{}
	if err := r.Do(ctx, connector.Call{Verb: "find", Args: []string{"t", `{"a":{"$gte":1}}`}, Limit: 1}, &c); err != nil || len(c.Rows) != 1 {
		t.Fatalf("find %v %+v", err, c.Rows)
	}
	c = connector.Collector{}
	if err := w.Do(ctx, connector.Call{Verb: "delete", Preview: true, Args: []string{"t", `{"a":1}`}}, &c); err != nil || c.Rows[0]["matched"] != int64(1) {
		t.Fatalf("preview %v %+v", err, c.Rows)
	}
	_ = w.Do(ctx, connector.Call{Verb: "drop", Destructive: true, Args: []string{"t"}}, &c)
}

const explainCollscan = `{
  "explainVersion": "1",
  "queryPlanner": {
    "namespace": "app.events", "indexFilterSet": false, "parsedQuery": {"kind": {"$eq": "click"}},
    "planCacheShapeHash": "8F2A1B3C", "maxIndexedOrSolutionsReached": false,
    "winningPlan": {"stage": "LIMIT", "limitAmount": 1000,
      "inputStage": {"stage": "COLLSCAN", "filter": {"kind": {"$eq": "click"}}, "direction": "forward"}},
    "rejectedPlans": []
  },
  "command": {"find": "events", "filter": {"kind": "click"}, "$db": "app"},
  "serverInfo": {"host": "db-1", "port": 27017, "version": "7.0.12"},
  "ok": 1
}`

const explainIxscanAgg = `{
  "explainVersion": "1",
  "stages": [
    {"$cursor": {"queryPlanner": {"namespace": "app.events",
      "winningPlan": {"stage": "FETCH", "inputStage": {"stage": "IXSCAN", "keyPattern": {"user_id": 1},
        "indexName": "user_id_1", "isMultiKey": false, "direction": "forward",
        "indexBounds": {"user_id": ["[42, 42]"]}}},
      "rejectedPlans": [{"stage": "COLLSCAN", "direction": "forward"}]}}},
    {"$group": {"_id": "$kind", "n": {"$sum": {"$const": 1}}}}
  ],
  "ok": 1
}`

func TestEstimateFromExplain(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(explainCollscan), &doc); err != nil {
		t.Fatal(err)
	}
	p := parsePlan(doc)
	if !p.collscan {
		t.Fatalf("%+v", p)
	}
	e := buildEstimate("events", p, 4_200_000, connector.Limits{})
	if e.Why != "COLLSCAN on events, ~4.2M docs" || e.Duration != 4200*time.Millisecond || !e.Confident || e.Rows != 4_200_000 {
		t.Fatalf("%+v", e)
	}
	if e := buildEstimate("events", p, 4_200_000, connector.Limits{RowsPerSecond: 1e5}); e.Duration != 42*time.Second {
		t.Fatalf("%+v", e)
	}
	doc = nil
	if err := json.Unmarshal([]byte(explainIxscanAgg), &doc); err != nil {
		t.Fatal(err)
	}
	p = parsePlan(doc) // the COLLSCAN is a rejected plan: ignored
	e = buildEstimate("events", p, 0, connector.Limits{})
	if p.collscan || e.Why != "IXSCAN user_id_1" || e.Duration != 0 || e.Confident {
		t.Fatalf("%+v %+v", p, e)
	}
	// bson.D nesting (as the driver decodes) is walked too
	p = parsePlan(bson.M{"queryPlanner": bson.D{{Key: "winningPlan", Value: bson.D{{Key: "stage", Value: "COLLSCAN"}}}}})
	if !p.collscan {
		t.Fatalf("%+v", p)
	}
}

func TestEstimateNotForOtherVerbs(t *testing.T) {
	m := &Mongo{}
	for _, v := range []string{"ls", "collections", "insert", "update", "delete", "aggregate-out", "drop"} {
		if _, err := m.Estimate(context.Background(), connector.Call{Verb: v}); !errors.Is(err, connector.ErrNoEstimate) {
			t.Fatalf("%s: %v", v, err)
		}
	}
}
