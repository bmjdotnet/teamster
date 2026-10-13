package sqlite

import (
	"context"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

const repriceRecompute = `(tl.input_tokens * mp.input_per_mtok +
		tl.output_tokens * mp.output_per_mtok +
		tl.cache_read_tokens * mp.cache_read_per_mtok +
		tl.cache_write_5m * mp.cache_write_5m_per_mtok +
		tl.cache_write_1h * mp.cache_write_1h_per_mtok) / 1000000.0`

const repriceBatchSize = 500

// DriftedRows implements store.RepricerStore.
func (s *Store) DriftedRows(ctx context.Context, f store.RepriceFilter) ([]store.DriftedRow, error) {
	q := `SELECT tl.session_id, tl.message_id, tl.model, tl.runtime, tl.rate_id,
		tl.cost_usd, ` + repriceRecompute + `
		FROM token_ledger tl
		INNER JOIN model_pricing mp ON mp.id = tl.rate_id
		WHERE tl.rate_id IS NOT NULL
		  AND mp.model_key NOT LIKE 'embedded-fallback-v%'
		  AND ABS(tl.cost_usd - ` + repriceRecompute + `) > 0.000001`
	var args []interface{}
	if f.SessionID != "" {
		q += " AND tl.session_id = ?"
		args = append(args, f.SessionID)
	}
	if f.Model != "" {
		q += " AND tl.model = ?"
		args = append(args, f.Model)
	}
	if !f.Since.IsZero() {
		q += " AND tl.timestamp >= ?"
		args = append(args, f.Since.UTC())
	}
	q += " ORDER BY tl.timestamp"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []store.DriftedRow
	for rows.Next() {
		var r store.DriftedRow
		if err := rows.Scan(&r.SessionID, &r.MessageID, &r.Model, &r.Runtime, &r.RateID, &r.OldCostUSD, &r.NewCostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApplyReprice implements store.RepricerStore.
func (s *Store) ApplyReprice(ctx context.Context, rows []store.DriftedRow, reason, operator string) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	now := time.Now().UTC()
	var updated int64
	for start := 0; start < len(rows); start += repriceBatchSize {
		end := start + repriceBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]

		ph := make([]string, 0, len(chunk))
		args := make([]interface{}, 0, len(chunk)*9)
		for _, r := range chunk {
			res, err := tx.ExecContext(ctx,
				`UPDATE token_ledger SET cost_usd = ?, rate_id = ? WHERE session_id = ? AND message_id = ?`,
				r.NewCostUSD, r.RateID, r.SessionID, r.MessageID)
			if err != nil {
				return 0, err
			}
			n, _ := res.RowsAffected()
			updated += n
			ph = append(ph, "(?, ?, ?, ?, ?, ?, ?, ?, ?)")
			args = append(args, r.SessionID, r.MessageID, r.OldCostUSD, r.NewCostUSD,
				r.RateID, r.RateID, reason, operator, now)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reprice_journal
			 (session_id, message_id, old_cost_usd, new_cost_usd, old_rate_id, new_rate_id, reason, operator, repriced_at)
			 VALUES `+strings.Join(ph, ", "), args...); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}

// BackfillCandidates implements store.RepricerStore.
func (s *Store) BackfillCandidates(ctx context.Context, f store.RepriceFilter) ([]store.BackfillCandidate, error) {
	where, args := backfillWhere(f)
	rows, err := s.db.QueryContext(ctx,
		`SELECT tl.runtime, tl.model, COUNT(*) AS n_rows FROM token_ledger tl WHERE `+where+
			` GROUP BY tl.runtime, tl.model ORDER BY n_rows DESC, tl.runtime, tl.model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []store.BackfillCandidate
	for rows.Next() {
		var c store.BackfillCandidate
		if err := rows.Scan(&c.Runtime, &c.Model, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ApplyBackfill implements store.RepricerStore.
func (s *Store) ApplyBackfill(ctx context.Context, mappings []store.BackfillMapping, f store.RepriceFilter, reason, operator string) (int64, error) {
	if len(mappings) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	now := time.Now().UTC()
	var updated int64
	for _, m := range mappings {
		where, whereArgs := backfillWhere(f)
		where += " AND tl.runtime = ? AND tl.model = ?"
		whereArgs = append(whereArgs, m.Runtime, m.Model)

		journalArgs := append([]interface{}{m.RateID, reason, operator, now}, whereArgs...)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reprice_journal
			 (session_id, message_id, old_cost_usd, new_cost_usd, old_rate_id, new_rate_id, reason, operator, repriced_at)
			 SELECT tl.session_id, tl.message_id, tl.cost_usd, tl.cost_usd, NULL, ?, ?, ?, ?
			 FROM token_ledger tl WHERE `+where, journalArgs...); err != nil {
			return 0, err
		}
		updateArgs := append([]interface{}{m.RateID}, whereArgs...)
		res, err := tx.ExecContext(ctx,
			`UPDATE token_ledger AS tl SET rate_id = ? WHERE `+where, updateArgs...)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		updated += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}

func backfillWhere(f store.RepriceFilter) (string, []interface{}) {
	where := "tl.rate_id IS NULL AND tl.model != ''"
	var args []interface{}
	if f.SessionID != "" {
		where += " AND tl.session_id = ?"
		args = append(args, f.SessionID)
	}
	if f.Model != "" {
		where += " AND tl.model = ?"
		args = append(args, f.Model)
	}
	if !f.Since.IsZero() {
		where += " AND tl.timestamp >= ?"
		args = append(args, f.Since.UTC())
	}
	return where, args
}
