package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

var _ connector.Estimator = (*MySQL)(nil)

// Calibration defaults (overridden per target by connector.Limits).
const (
	// defaultCostPerSecond: optimizer query_cost units per second — a rough
	// rule of thumb; calibrate with limits.cost_per_second.
	defaultCostPerSecond = 100000
	// defaultRowsPerSecond: rows examined per second.
	defaultRowsPerSecond = 1e6
)

// Estimate runs EXPLAIN FORMAT=JSON for query on the guarded read path.
func (m *MySQL) Estimate(ctx context.Context, c connector.Call) (connector.Estimate, error) {
	if c.Verb != "query" {
		return connector.Estimate{}, connector.ErrNoEstimate
	}
	q, args, err := m.statement(c.Args)
	if err != nil {
		return connector.Estimate{}, err
	}
	if err := readGate(q); err != nil {
		return connector.Estimate{}, err
	}
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return connector.Estimate{}, classify(err)
	}
	defer conn.Close()
	if err := m.session(ctx, conn, true, c); err != nil {
		return connector.Estimate{}, err
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return connector.Estimate{}, classify(err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only: always roll back
	var raw []byte
	if err := tx.QueryRowContext(ctx, "EXPLAIN FORMAT=JSON "+q, args...).Scan(&raw); err != nil {
		return connector.Estimate{}, classify(err)
	}
	return parseExplain(raw, m.t.Limits)
}

type tableScan struct {
	name   string
	access string
	rows   float64
}

// parseExplain turns EXPLAIN FORMAT=JSON output into an Estimate.
func parseExplain(raw []byte, l connector.Limits) (connector.Estimate, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return connector.Estimate{}, errs.New(errs.Runtime, "cannot parse EXPLAIN output")
	}
	qb, _ := doc["query_block"].(map[string]any)
	if qb == nil {
		return connector.Estimate{}, errs.New(errs.Runtime, "EXPLAIN output has no query_block")
	}
	var cost float64
	if ci, ok := qb["cost_info"].(map[string]any); ok {
		cost = num(ci["query_cost"])
	}
	var tables []tableScan
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if t, ok := x["table"].(map[string]any); ok {
				if name, ok := t["table_name"].(string); ok {
					a, _ := t["access_type"].(string)
					tables = append(tables, tableScan{name, a, num(t["rows_examined_per_scan"])})
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(qb)
	var rows float64
	var big *tableScan
	for i, t := range tables {
		rows += t.rows
		if t.access == "ALL" && (big == nil || t.rows > big.rows) {
			big = &tables[i]
		}
	}
	est := connector.Estimate{
		Duration: max(connector.PerSecond(cost, connector.Rate(l.CostPerSecond, defaultCostPerSecond)),
			connector.PerSecond(rows, connector.Rate(l.RowsPerSecond, defaultRowsPerSecond))),
		Rows:      int64(rows),
		Confident: cost > 0,
		Why:       fmt.Sprintf("query_cost %.0f", cost),
	}
	if big != nil {
		est.Why = fmt.Sprintf("full scan on %s, ~%s rows", big.name, connector.Human(big.rows))
	}
	return est, nil
}

// num reads a JSON number or numeric string ("12.50").
func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}
