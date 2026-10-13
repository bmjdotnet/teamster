package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

var _ store.VerificationStore = (*Store)(nil)

// UpsertVerifications implements store.VerificationStore. converged_at keeps
// the stored value when the incoming row is converged and clears when it is
// not; see the interface for why.
func (s *Store) UpsertVerifications(ctx context.Context, rows []store.Verification) error {
	rows, err := store.NormalizeVerifications(rows)
	if err != nil {
		return fmt.Errorf("UpsertVerifications: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("UpsertVerifications: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for _, v := range rows {
		var converged, details any
		if v.ConvergedAt != nil {
			converged = *v.ConvergedAt
		}
		if v.Details != nil {
			details = string(v.Details)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO cost_verification
				(session_id, runtime, hub_usd, vendor_usd, delta_usd, verdict, converged_at, evaluated_at, details)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (session_id, runtime) DO UPDATE SET
				hub_usd      = excluded.hub_usd,
				vendor_usd   = excluded.vendor_usd,
				delta_usd    = excluded.delta_usd,
				verdict      = excluded.verdict,
				converged_at = CASE WHEN excluded.converged_at IS NULL THEN NULL
				                    ELSE COALESCE(cost_verification.converged_at, excluded.converged_at) END,
				evaluated_at = excluded.evaluated_at,
				details      = excluded.details`,
			v.SessionID, v.Runtime, v.HubUSD, v.VendorUSD, v.DeltaUSD,
			v.Verdict, converged, v.EvaluatedAt, details,
		); err != nil {
			return fmt.Errorf("UpsertVerifications: %s/%s: %w", v.Runtime, v.SessionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("UpsertVerifications: commit: %w", err)
	}
	return nil
}

// ListVerifications implements store.VerificationStore.
func (s *Store) ListVerifications(ctx context.Context, runtime string, evaluatedSince time.Time) ([]store.Verification, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, runtime, hub_usd, vendor_usd, delta_usd, verdict, converged_at, evaluated_at, details
		FROM cost_verification
		WHERE runtime = ? AND evaluated_at >= ?
		ORDER BY session_id`,
		store.NormalizeRateRuntime(runtime), evaluatedSince.UTC())
	if err != nil {
		return nil, fmt.Errorf("ListVerifications: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []store.Verification
	for rows.Next() {
		var (
			v         store.Verification
			converged sql.NullTime
			details   sql.NullString
		)
		if err := rows.Scan(&v.SessionID, &v.Runtime, &v.HubUSD, &v.VendorUSD, &v.DeltaUSD,
			&v.Verdict, &converged, &v.EvaluatedAt, &details); err != nil {
			return nil, fmt.Errorf("ListVerifications: scan: %w", err)
		}
		v.EvaluatedAt = v.EvaluatedAt.UTC()
		if converged.Valid {
			t := converged.Time.UTC()
			v.ConvergedAt = &t
		}
		if details.Valid {
			v.Details = []byte(details.String)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListVerifications: %w", err)
	}
	return out, nil
}
