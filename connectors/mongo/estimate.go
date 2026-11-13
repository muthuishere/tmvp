package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/muthuishere/tmvp/internal/connector"
)

var _ connector.Estimator = (*Mongo)(nil)

// defaultRowsPerSecond: documents a COLLSCAN reads per second (rough
// default; calibrate with limits.rows_per_second).
const defaultRowsPerSecond = 1e6

// Estimate runs explain (verbosity queryPlanner — nothing executes) for
// find, aggregate and count.
func (m *Mongo) Estimate(ctx context.Context, c connector.Call) (connector.Estimate, error) {
	switch c.Verb {
	case "find", "aggregate", "count":
	default:
		return connector.Estimate{}, connector.ErrNoEstimate
	}
	if err := m.guardArgs(c); err != nil {
		return connector.Estimate{}, err
	}
	ctx, cancel := m.opCtx(ctx, c)
	defer cancel()
	db, err := m.db(c)
	if err != nil {
		return connector.Estimate{}, err
	}
	coll := c.Args[0]
	var cmd bson.D
	switch c.Verb {
	case "aggregate":
		s, err := m.argAt(c, 1)
		if err != nil {
			return connector.Estimate{}, err
		}
		p, err := parsePipeline(s)
		if err != nil {
			return connector.Estimate{}, err
		}
		if err := readPipeline(p); err != nil {
			return connector.Estimate{}, err
		}
		cmd = bson.D{{Key: "aggregate", Value: coll}, {Key: "pipeline", Value: p}, {Key: "cursor", Value: bson.D{}}}
	default:
		f, err := m.filterAt(c, 1)
		if err != nil {
			return connector.Estimate{}, err
		}
		if f == nil {
			f = bson.D{}
		}
		if c.Verb == "find" {
			cmd = bson.D{{Key: "find", Value: coll}, {Key: "filter", Value: f}}
		} else {
			cmd = bson.D{{Key: "count", Value: coll}, {Key: "query", Value: f}}
		}
	}
	var out bson.M
	err = db.RunCommand(ctx, bson.D{{Key: "explain", Value: cmd}, {Key: "verbosity", Value: "queryPlanner"}}).Decode(&out)
	if err != nil {
		return connector.Estimate{}, classify(err)
	}
	pl := parsePlan(out)
	var docs int64
	if pl.collscan {
		if docs, err = db.Collection(coll).EstimatedDocumentCount(ctx); err != nil {
			return connector.Estimate{}, classify(err)
		}
	}
	return buildEstimate(coll, pl, docs, m.t.Limits), nil
}

type plan struct {
	collscan bool
	index    string // first IXSCAN/COUNT_SCAN index name
	stages   int
}

// parsePlan walks an explain result (any nesting: queryPlanner.winningPlan,
// aggregate stages[].$cursor, sharded shards[], SBE queryPlan) for stages.
func parsePlan(v any) plan {
	var p plan
	var walk func(v any)
	visit := func(key string, val any) {
		if key == "rejectedPlans" {
			return // only the winning plan matters
		}
		if key == "stage" {
			if s, ok := val.(string); ok {
				p.stages++
				if s == "COLLSCAN" {
					p.collscan = true
				}
			}
		}
		walk(val)
	}
	walk = func(v any) {
		switch x := v.(type) {
		case bson.M:
			walk(map[string]any(x))
		case map[string]any:
			if s, _ := x["stage"].(string); (s == "IXSCAN" || s == "COUNT_SCAN") && p.index == "" {
				p.index, _ = x["indexName"].(string)
			}
			for k, val := range x {
				visit(k, val)
			}
		case bson.D:
			m := make(map[string]any, len(x))
			for _, e := range x {
				m[e.Key] = e.Value
			}
			walk(m)
		case bson.A:
			walk([]any(x))
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return p
}

// buildEstimate: COLLSCAN → docs / rows-per-second; IXSCAN → ~0, heuristic.
func buildEstimate(coll string, p plan, docs int64, l connector.Limits) connector.Estimate {
	switch {
	case p.collscan:
		return connector.Estimate{
			Duration:  connector.PerSecond(float64(docs), connector.Rate(l.RowsPerSecond, defaultRowsPerSecond)),
			Rows:      docs,
			Why:       fmt.Sprintf("COLLSCAN on %s, ~%s docs", coll, connector.Human(float64(docs))),
			Confident: true,
		}
	case p.index != "":
		return connector.Estimate{Why: "IXSCAN " + p.index}
	case p.stages == 0:
		return connector.Estimate{Why: "no plan in explain output"}
	}
	return connector.Estimate{Why: "no collection scan"}
}
