package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

var _ store.ReconciliationStore = (*Store)(nil)

// reconcileIDChunk bounds the IN (...) list. SQLite's variable ceiling is far
// higher; the same bound keeps one statement small and matches the mysql backend.
const reconcileIDChunk = 500

// ReconcileSessions implements store.ReconciliationStore: one GROUP BY over
// token_ledger per chunk of ids, folded into per-session aggregates. Read-only.
func (s *Store) ReconcileSessions(ctx context.Context, sessionIDs []string) (map[string]store.LedgerSession, error) {
	out := map[string]store.LedgerSession{}
	for _, chunk := range store.UniqueIDChunks(sessionIDs, reconcileIDChunk) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT session_id, model, COUNT(*), MAX(timestamp),
			       SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens),
			       SUM(cache_write_tokens), SUM(cache_write_5m), SUM(cache_write_1h),
			       SUM(cost_usd)
			FROM token_ledger
			WHERE session_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`)
			GROUP BY session_id, model`, args...)
		if err != nil {
			return nil, fmt.Errorf("ReconcileSessions: %w", err)
		}
		for rows.Next() {
			var (
				sessionID, model string
				n                int64
				last             aggTime
				t                store.LedgerTokens
				cost             float64
			)
			if err := rows.Scan(&sessionID, &model, &n, &last,
				&t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.CacheWrite5m, &t.CacheWrite1h, &cost); err != nil {
				rows.Close() //nolint:errcheck
				return nil, fmt.Errorf("ReconcileSessions: scan: %w", err)
			}
			store.FoldLedgerGroup(out, sessionID, model, n, last.Time, t, cost)
		}
		err = rows.Err()
		rows.Close() //nolint:errcheck
		if err != nil {
			return nil, fmt.Errorf("ReconcileSessions: %w", err)
		}
	}
	return out, nil
}

// ReconcileSessionsSince implements store.ReconciliationStore.
func (s *Store) ReconcileSessionsSince(ctx context.Context, runtime string, since time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT session_id FROM token_ledger WHERE runtime = ? AND timestamp >= ? ORDER BY session_id`,
		store.NormalizeRateRuntime(runtime), since.UTC())
	if err != nil {
		return nil, fmt.Errorf("ReconcileSessionsSince: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("ReconcileSessionsSince: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ReconcileSessionsSince: %w", err)
	}
	return ids, nil
}
