package spanembed

import (
	"context"
	"strings"
)

// ExplainSearch runs EXPLAIN ANALYZE over the statement Search would run for
// these arguments, in the same transaction setup, and reports the plan text
// and whether the search ranked exactly.
func (s *Store) ExplainSearch(ctx context.Context, embedding []float32, topK int, filter SearchFilter) (string, bool, error) {
	tx, err := s.beginSearch(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	query, args, exact, err := s.prepareSearch(ctx, tx, embedding, topK, filter)
	if err != nil {
		return "", false, err
	}
	rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) "+query, args)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", false, err
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	return plan.String(), exact, rows.Err()
}
