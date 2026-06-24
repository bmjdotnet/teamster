package mysql

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/bmjdotnet/teamster/internal/store"
)

// GetStatusSummary collects system health metrics. Each query is independent;
// a failure zeroes the affected fields and logs a warning rather than failing
// the whole call.
func (s *Store) GetStatusSummary(ctx context.Context) (store.StatusSummary, error) {
	var sum store.StatusSummary

	// WMS entity counts — outcomes
	{
		rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM outcomes GROUP BY status`)
		if err != nil {
			slog.Warn("GetStatusSummary: outcomes count", "err", err)
		} else {
			for rows.Next() {
				var status string
				var n int
				if err := rows.Scan(&status, &n); err != nil {
					continue
				}
				switch status {
				case "done":
					sum.OutcomesDone += n
				default:
					sum.OutcomesOpen += n
				}
			}
			rows.Close() //nolint:errcheck
			if err := rows.Err(); err != nil {
				slog.Warn("GetStatusSummary: outcomes iteration", "err", err)
				sum.OutcomesOpen, sum.OutcomesDone = 0, 0
			}
		}
	}

	// WMS entity counts — workunits
	{
		rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM workunits GROUP BY status`)
		if err != nil {
			slog.Warn("GetStatusSummary: workunits count", "err", err)
		} else {
			for rows.Next() {
				var status string
				var n int
				if err := rows.Scan(&status, &n); err != nil {
					continue
				}
				switch status {
				case "done":
					sum.WorkUnitsDone += n
				default:
					sum.WorkUnitsOpen += n
				}
			}
			rows.Close() //nolint:errcheck
			if err := rows.Err(); err != nil {
				slog.Warn("GetStatusSummary: workunits iteration", "err", err)
				sum.WorkUnitsOpen, sum.WorkUnitsDone = 0, 0
			}
		}
	}

	// Active sessions
	{
		var sessions, agents, users, hosts int
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT session_id),
			       COUNT(*),
			       COUNT(DISTINCT NULLIF(username, '')),
			       COUNT(DISTINCT host)
			FROM sessions
			WHERE status IN ('active', 'idle')`).Scan(&sessions, &agents, &users, &hosts)
		if err != nil {
			slog.Warn("GetStatusSummary: active sessions", "err", err)
		} else {
			sum.ActiveSessions = sessions
			sum.ActiveAgents = agents
			sum.ActiveUsers = users
			sum.ActiveHosts = hosts
		}
	}

	// All-time distinct users
	{
		var n int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT username) FROM sessions WHERE username != ''`).Scan(&n)
		if err != nil {
			slog.Warn("GetStatusSummary: all-time users", "err", err)
		} else {
			sum.AllTimeUsers = n
		}
	}

	// Cost — today
	{
		var v float64
		err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(cost_usd), 0) FROM token_ledger WHERE timestamp >= CURDATE()`).Scan(&v)
		if err != nil {
			slog.Warn("GetStatusSummary: today cost", "err", err)
		} else {
			sum.TodayCostUSD = v
		}
	}

	// Cost — total
	{
		var v float64
		err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(cost_usd), 0) FROM token_ledger`).Scan(&v)
		if err != nil {
			slog.Warn("GetStatusSummary: total cost", "err", err)
		} else {
			sum.TotalCostUSD = v
		}
	}

	// Total messages
	{
		var n int64
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_ledger`).Scan(&n)
		if err != nil {
			slog.Warn("GetStatusSummary: message count", "err", err)
		} else {
			sum.TotalMessages = n
		}
	}

	// Distinct models
	{
		var n int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT model) FROM token_ledger WHERE model != ''`).Scan(&n)
		if err != nil {
			slog.Warn("GetStatusSummary: distinct models", "err", err)
		} else {
			sum.DistinctModels = n
		}
	}

	// DB size
	{
		var v float64
		err := s.db.QueryRowContext(ctx, `
			SELECT COALESCE(ROUND(SUM(data_length + index_length) / 1024 / 1024, 2), 0)
			FROM information_schema.tables
			WHERE table_schema = DATABASE()`).Scan(&v)
		if err != nil {
			slog.Warn("GetStatusSummary: db size", "err", err)
		} else {
			sum.DBSizeMB = v
		}
	}

	// Attribution
	{
		var total int64
		var mapped sql.NullInt64
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*),
			       COALESCE(SUM(CASE WHEN method != 'unallocated' THEN 1 ELSE 0 END), 0)
			FROM usage_attribution`).Scan(&total, &mapped)
		if err != nil {
			slog.Warn("GetStatusSummary: attribution", "err", err)
		} else {
			sum.TotalAttributions = total
			if mapped.Valid {
				sum.MappedAttributions = mapped.Int64
			}
		}
	}

	return sum, nil
}
