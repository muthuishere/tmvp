package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

var _ connector.Estimator = (*PG)(nil)

// Calibration defaults (overridden per target by connector.Limits).
const (
	// defaultCostPerSecond: planner cost units executed per second. A rough
	// rule of thumb for a warm, modern server (seq_page_cost=1 per 8KB page);
	// calibrate with limits.cost_per_second.
	defaultCostPerSecond = 100000
	// defaultSeqScanRows: a Seq Scan on a table bigger than this is flagged.
	defaultSeqScanRows = 1e6
	// defaultRowsPerSecond: rows a Seq Scan reads per second.
	defaultRowsPerSecond = 2e6
)

// Estimate runs EXPLAIN (FORMAT JSON) — never ANALYZE — for query.
func (p *PG) Estimate(ctx context.Context, c connector.Call) (connector.Estimate, error) {
	if c.Verb != "query" {
		return connector.Estimate{}, connector.ErrNoEstimate
	}
	sql, args, err := p.statement(c.Args)
	if err != nil {
		return connector.Estimate{}, err
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return connector.Estimate{}, classify(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // read-only: always roll back
	if err := p.setTimeouts(ctx, tx, c); err != nil {
		return connector.Estimate{}, err
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		return connector.Estimate{}, classify(err)
	}
	info, err := parsePlan(raw)
	if err != nil {
		return connector.Estimate{}, err
	}
	tuples := map[relKey]float64{}
	if len(info.seqScans) > 0 {
		var schemas, names []string
		for _, s := range info.seqScans {
			schemas = append(schemas, s.schema)
			names = append(names, s.rel)
		}
		rows, err := tx.Query(ctx, relTuplesSQL, schemas, names)
		if err != nil {
			return connector.Estimate{}, classify(err)
		}
		for rows.Next() {
			var k relKey
			var n *float64
			if err := rows.Scan(&k.schema, &k.rel, &n); err != nil {
				rows.Close()
				return connector.Estimate{}, classify(err)
			}
			if n != nil {
				tuples[k] = *n
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return connector.Estimate{}, classify(err)
		}
	}
	return buildEstimate(info, tuples, p.t.Limits), nil
}

// relTuplesSQL: pg_class.reltuples per (schema, relation); schema "" = search_path.
const relTuplesSQL = `select u.s, u.r, c.reltuples::float8
from unnest($1::text[], $2::text[]) as u(s, r)
left join pg_class c on c.oid = to_regclass(case when u.s = '' then quote_ident(u.r) else quote_ident(u.s) || '.' || quote_ident(u.r) end)`

type relKey struct{ schema, rel string }

type seqScan struct {
	relKey
	planRows float64
}

type planInfo struct {
	totalCost float64
	rows      float64
	topNode   string
	seqScans  []seqScan
}

type planNode struct {
	NodeType  string     `json:"Node Type"`
	Relation  string     `json:"Relation Name"`
	Schema    string     `json:"Schema"`
	TotalCost float64    `json:"Total Cost"`
	PlanRows  float64    `json:"Plan Rows"`
	Plans     []planNode `json:"Plans"`
}

// parsePlan reads EXPLAIN (FORMAT JSON) output.
func parsePlan(raw []byte) (planInfo, error) {
	var doc []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) == 0 {
		return planInfo{}, errs.New(errs.Runtime, "cannot parse EXPLAIN output")
	}
	top := doc[0].Plan
	info := planInfo{totalCost: top.TotalCost, rows: top.PlanRows, topNode: top.NodeType}
	if top.Relation != "" {
		info.topNode += " on " + top.Relation
	}
	var walk func(n planNode)
	walk = func(n planNode) {
		if n.NodeType == "Seq Scan" && n.Relation != "" {
			info.seqScans = append(info.seqScans, seqScan{relKey{n.Schema, n.Relation}, n.PlanRows})
		}
		for _, c := range n.Plans {
			walk(c)
		}
	}
	walk(top)
	return info, nil
}

// buildEstimate turns a parsed plan plus reltuples (missing/-1 = unknown)
// into an Estimate.
func buildEstimate(info planInfo, tuples map[relKey]float64, l connector.Limits) connector.Estimate {
	est := connector.Estimate{
		Duration:  connector.PerSecond(info.totalCost, connector.Rate(l.CostPerSecond, defaultCostPerSecond)),
		Rows:      int64(info.rows),
		Confident: true,
		Why:       info.topNode,
	}
	var big *seqScan
	for i := range info.seqScans {
		if big == nil || info.seqScans[i].planRows > big.planRows {
			big = &info.seqScans[i]
		}
	}
	if big != nil {
		est.Why = fmt.Sprintf("Seq Scan on %s, ~%s rows", big.rel, connector.Human(big.planRows))
	}
	var worst float64
	var worstRel string
	for _, s := range info.seqScans {
		n, ok := tuples[s.relKey]
		if !ok || n < 0 {
			est.Confident = false
			continue
		}
		if n > worst {
			worst, worstRel = n, s.rel
		}
	}
	if worst > connector.Rate(l.SeqScanRows, defaultSeqScanRows) {
		if d := connector.PerSecond(worst, connector.Rate(l.RowsPerSecond, defaultRowsPerSecond)); d > est.Duration {
			est.Duration = d
		}
		est.Why = fmt.Sprintf("Seq Scan on %s (~%s rows in table)", worstRel, connector.Human(worst))
	}
	return est
}
