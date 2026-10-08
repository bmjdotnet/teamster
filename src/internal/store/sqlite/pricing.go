package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

var _ store.PricingStore = (*Store)(nil)

const modelPricingColumns = `id, runtime, match_kind, model_key, variant,
	input_per_mtok, output_per_mtok, cache_read_per_mtok, cache_write_5m_per_mtok, cache_write_1h_per_mtok,
	valid_from, valid_to, source_url, fetched_at, notes`

const modelPricingInsertColumns = `runtime, match_kind, model_key, variant,
	input_per_mtok, output_per_mtok, cache_read_per_mtok, cache_write_5m_per_mtok, cache_write_1h_per_mtok,
	valid_from, valid_to, source_url, fetched_at, notes`

func modelPricingInsertArgs(r store.ModelRate) []any {
	var validTo, notes any
	if r.ValidTo != nil {
		validTo = r.ValidTo.UTC()
	}
	if r.Notes != "" {
		notes = r.Notes
	}
	return []any{
		r.Runtime, r.MatchKind, r.ModelKey, r.Variant,
		r.InputPerMtok, r.OutputPerMtok, r.CacheReadPerMtok, r.CacheWrite5mPerMtok, r.CacheWrite1hPerMtok,
		r.ValidFrom.UTC(), validTo, r.SourceURL, r.FetchedAt.UTC(), notes,
	}
}

// seedModelPricing inserts the frozen v72 seed (mirrors mysql v73).
// INSERT OR IGNORE against the unique index makes a re-run a no-op.
func seedModelPricing(ctx context.Context, x store.Execer) error {
	return insertSeedRates(ctx, x, store.ModelPricingSeedV1())
}

// seedEmbeddedFallbackSentinels inserts the frozen sentinel rows (mirrors
// mysql v76).
func seedEmbeddedFallbackSentinels(ctx context.Context, x store.Execer) error {
	return insertSeedRates(ctx, x, store.EmbeddedFallbackSentinelsV1())
}

func insertSeedRates(ctx context.Context, x store.Execer, seed []store.ModelRate) error {
	for _, raw := range seed {
		r, err := store.NormalizeModelRate(raw)
		if err != nil {
			return fmt.Errorf("seed model_pricing %s/%s: %w", raw.Runtime, raw.ModelKey, err)
		}
		if _, err := x.Exec(ctx,
			`INSERT OR IGNORE INTO model_pricing (`+modelPricingInsertColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			modelPricingInsertArgs(r)...); err != nil {
			return fmt.Errorf("seed model_pricing %s/%s: %w", r.Runtime, r.ModelKey, err)
		}
	}
	return nil
}

// ResolveRate implements store.PricingStore via the shared chain.
func (s *Store) ResolveRate(ctx context.Context, runtime, model string, at time.Time) (store.ModelRate, error) {
	rates, err := s.queryRates(ctx, store.NormalizeRateRuntime(runtime))
	if err != nil {
		return store.ModelRate{}, fmt.Errorf("ResolveRate: %w", err)
	}
	return store.SelectRate(rates, runtime, model, at)
}

// ListRates implements store.PricingStore.
func (s *Store) ListRates(ctx context.Context, filter store.RateFilter) ([]store.ModelRate, error) {
	rates, err := s.queryRates(ctx, filter.Runtime)
	if err != nil {
		return nil, fmt.Errorf("ListRates: %w", err)
	}
	if filter.At != nil {
		rates = store.EffectiveRates(rates, *filter.At)
		store.SortRates(rates)
	}
	return rates, nil
}

// UpsertRate implements store.PricingStore (insert-only in WP2).
func (s *Store) UpsertRate(ctx context.Context, rate store.ModelRate) (int64, error) {
	r, err := store.NormalizeModelRate(rate)
	if err != nil {
		return 0, fmt.Errorf("UpsertRate: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO model_pricing (`+modelPricingInsertColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		modelPricingInsertArgs(r)...)
	if err != nil {
		if ce := classifyConflict("UpsertRate", err); errors.Is(ce, store.ErrConflict) {
			return 0, ce
		}
		return 0, fmt.Errorf("UpsertRate: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("UpsertRate: last insert id: %w", err)
	}
	return id, nil
}

// queryRates reads every row (history included) for runtime, or for all
// runtimes when runtime is empty. Times are filtered in Go (store.RateInEffect)
// rather than in SQL: the table is tiny, and this keeps the temporal logic out
// of SQLite's text-compared DATETIME values.
func (s *Store) queryRates(ctx context.Context, runtime string) ([]store.ModelRate, error) {
	query := `SELECT ` + modelPricingColumns + ` FROM model_pricing`
	var args []any
	if runtime != "" {
		query += ` WHERE runtime = ?`
		args = append(args, runtime)
	}
	query += ` ORDER BY runtime, match_kind, model_key, variant, valid_from, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []store.ModelRate
	for rows.Next() {
		var (
			r       store.ModelRate
			validTo sql.NullTime
			notes   sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.Runtime, &r.MatchKind, &r.ModelKey, &r.Variant,
			&r.InputPerMtok, &r.OutputPerMtok, &r.CacheReadPerMtok, &r.CacheWrite5mPerMtok, &r.CacheWrite1hPerMtok,
			&r.ValidFrom, &validTo, &r.SourceURL, &r.FetchedAt, &notes); err != nil {
			return nil, err
		}
		r.ValidFrom = r.ValidFrom.UTC()
		r.FetchedAt = r.FetchedAt.UTC()
		if validTo.Valid {
			vt := validTo.Time.UTC()
			r.ValidTo = &vt
		}
		r.Notes = notes.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// CloseRate implements store.PricingStore. The row is read and validated in Go
// (keeping time comparison out of SQLite's text-compared DATETIMEs), then
// closed by a guarded UPDATE (valid_to IS NULL) so a concurrent close loses
// cleanly instead of overwriting.
func (s *Store) CloseRate(ctx context.Context, id int64, validTo time.Time) error {
	if validTo.IsZero() {
		return fmt.Errorf("CloseRate: valid_to is required")
	}
	validTo = validTo.UTC()
	entityID := strconv.FormatInt(id, 10)

	var from time.Time
	var closed sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT valid_from, valid_to FROM model_pricing WHERE id = ?`, id).Scan(&from, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		return store.NotFound("CloseRate", "model_rate", entityID)
	}
	if err != nil {
		return fmt.Errorf("CloseRate: %w", err)
	}
	if closed.Valid {
		return fmt.Errorf("rate already closed at %s: %w", closed.Time.UTC().Format(time.RFC3339Nano),
			store.Precondition("CloseRate", "model_rate", entityID))
	}
	if !validTo.After(from.UTC()) {
		return fmt.Errorf("valid_to %s must be after valid_from %s: %w", validTo.Format(time.RFC3339Nano),
			from.UTC().Format(time.RFC3339Nano), store.Precondition("CloseRate", "model_rate", entityID))
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE model_pricing SET valid_to = ? WHERE id = ? AND valid_to IS NULL`, validTo, id)
	if err != nil {
		return fmt.Errorf("CloseRate: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("CloseRate: %w", err)
	} else if n == 0 {
		return fmt.Errorf("rate was closed concurrently: %w", store.Precondition("CloseRate", "model_rate", entityID))
	}
	return nil
}
