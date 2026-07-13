// Package mysql is the MySQL-backed gauge.GaugeStore implementation.
// It takes its own *sql.DB — it is NOT part of internal/store/mysql.
package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
)

// Store is the MySQL-backed implementation of gauge.GaugeStore.
type Store struct {
	db *sql.DB
}

// New creates a GaugeStore backed by the given database connection.
func New(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) Upsert(ctx context.Context, row gauge.GaugeRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_health_gauge (
			host, session_id, agent_name, roster_id, runtime, model,
			long_context_active, context_window_tokens, context_tokens_used,
			context_tokens_free, context_fill_pct, context_reset_suspected,
			context_source, context_reported_at, session_cost_usd, statusline_json,
			composition_json, tokens_in_total, tokens_out_total,
			tool_call_counts_json, tool_calls_total, last_activity_ts, last_activity_tool,
			last_activity_display, pressure_level, pressure_level_since,
			collector_status, updated_at, fidelity_notes, session_total_cost_usd
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			roster_id               = VALUES(roster_id),
			runtime                 = VALUES(runtime),
			model                   = VALUES(model),
			long_context_active     = VALUES(long_context_active),
			context_window_tokens   = VALUES(context_window_tokens),
			context_tokens_used     = VALUES(context_tokens_used),
			context_tokens_free     = VALUES(context_tokens_free),
			context_fill_pct        = VALUES(context_fill_pct),
			context_reset_suspected = VALUES(context_reset_suspected),
			context_source          = VALUES(context_source),
			context_reported_at     = VALUES(context_reported_at),
			session_cost_usd        = VALUES(session_cost_usd),
			statusline_json         = VALUES(statusline_json),
			composition_json        = VALUES(composition_json),
			tokens_in_total         = VALUES(tokens_in_total),
			tokens_out_total        = VALUES(tokens_out_total),
			tool_call_counts_json   = VALUES(tool_call_counts_json),
			tool_calls_total        = VALUES(tool_calls_total),
			last_activity_ts        = VALUES(last_activity_ts),
			last_activity_tool      = VALUES(last_activity_tool),
			last_activity_display   = VALUES(last_activity_display),
			pressure_level          = VALUES(pressure_level),
			pressure_level_since    = VALUES(pressure_level_since),
			collector_status        = VALUES(collector_status),
			updated_at              = VALUES(updated_at),
			fidelity_notes          = VALUES(fidelity_notes),
			session_total_cost_usd  = VALUES(session_total_cost_usd)`,
		row.Host, row.SessionID, row.AgentName, nullStr(row.RosterID),
		row.Runtime, row.Model,
		row.LongContextActive, row.ContextWindowTokens, row.ContextTokensUsed,
		row.ContextTokensFree, row.ContextFillPct, row.ContextResetSuspected,
		contextSourceOrDefault(row.ContextSource), nullTime(row.ContextReportedAt),
		row.SessionCostUSD, nullStr(row.StatuslineJSON),
		nullStr(row.CompositionJSON), row.TokensInTotal, row.TokensOutTotal,
		nullStr(row.ToolCallCountsJSON), row.ToolCallsTotal, nullTime(row.LastActivityTs),
		row.LastActivityTool, row.LastActivityDisplay,
		row.PressureLevel, nullTime(row.PressureLevelSince),
		row.CollectorStatus, row.UpdatedAt.UTC(), nullStr(row.FidelityNotes),
		row.SessionTotalCostUSD)
	if err != nil {
		return fmt.Errorf("gauge.Upsert: %w", err)
	}
	return nil
}

func (s *Store) UpdateActivity(ctx context.Context, key gauge.GaugeKey, display, tool string, ts time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_health_gauge
		SET last_activity_display = ?, last_activity_tool = ?, last_activity_ts = ?
		WHERE host = ? AND session_id = ? AND agent_name = ?`,
		display, tool, ts.UTC(), key.Host, key.SessionID, key.AgentName)
	if err != nil {
		return fmt.Errorf("gauge.UpdateActivity: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, key gauge.GaugeKey) (gauge.GaugeRow, bool, error) {
	row := s.db.QueryRowContext(ctx, gaugeSelectCols+
		` FROM agent_health_gauge
		WHERE host = ? AND session_id = ? AND agent_name = ?`,
		key.Host, key.SessionID, key.AgentName)
	g, err := scanGaugeRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return g, false, nil
		}
		return g, false, fmt.Errorf("gauge.Get: %w", err)
	}
	return g, true, nil
}

func (s *Store) List(ctx context.Context, filter gauge.GaugeFilter) ([]gauge.GaugeRow, error) {
	q := gaugeSelectCols + ` FROM agent_health_gauge`
	var where []string
	var args []any

	if filter.Host != "" {
		where = append(where, "host = ?")
		args = append(args, filter.Host)
	}
	if filter.Runtime != "" {
		where = append(where, "runtime = ?")
		args = append(args, filter.Runtime)
	}
	if filter.RosterID != "" {
		where = append(where, "roster_id = ?")
		args = append(args, filter.RosterID)
	}
	if filter.MinUpdatedAt != nil {
		where = append(where, "updated_at >= ?")
		args = append(args, filter.MinUpdatedAt.UTC())
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY updated_at DESC, session_id, agent_name"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gauge.List: %w", err)
	}
	defer rows.Close()

	var result []gauge.GaugeRow
	for rows.Next() {
		g, err := scanGaugeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("gauge.List scan: %w", err)
		}
		result = append(result, g)
	}
	return result, rows.Err()
}

func (s *Store) SweepOffline(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_health_gauge WHERE updated_at < ?`, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("gauge.SweepOffline: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// --- helpers ---

const gaugeSelectCols = `SELECT host, session_id, agent_name, roster_id, runtime, model,
	long_context_active, context_window_tokens, context_tokens_used,
	context_tokens_free, context_fill_pct, context_reset_suspected,
	context_source, context_reported_at, session_cost_usd, statusline_json,
	composition_json, tokens_in_total, tokens_out_total,
	tool_call_counts_json, tool_calls_total, last_activity_ts, last_activity_tool,
	last_activity_display, pressure_level, pressure_level_since,
	collector_status, updated_at, fidelity_notes, session_total_cost_usd`

func nullStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// contextSourceOrDefault defaults an unset ContextSource to "heuristic" so
// existing callers that don't yet set the field (any Upsert written before
// this column existed) don't write an empty string into a NOT NULL column.
func contextSourceOrDefault(source string) string {
	if source == "" {
		return gauge.ContextSourceHeuristic
	}
	return source
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanGaugeRow(row rowScanner) (gauge.GaugeRow, error) {
	var g gauge.GaugeRow
	var rosterID, statuslineJSON, compositionJSON, toolCallCountsJSON, fidelityNotes sql.NullString
	var lastActivityTs, pressureLevelSince, contextReportedAt sql.NullTime

	err := row.Scan(
		&g.Host, &g.SessionID, &g.AgentName, &rosterID, &g.Runtime, &g.Model,
		&g.LongContextActive, &g.ContextWindowTokens, &g.ContextTokensUsed,
		&g.ContextTokensFree, &g.ContextFillPct, &g.ContextResetSuspected,
		&g.ContextSource, &contextReportedAt, &g.SessionCostUSD, &statuslineJSON,
		&compositionJSON, &g.TokensInTotal, &g.TokensOutTotal,
		&toolCallCountsJSON, &g.ToolCallsTotal, &lastActivityTs, &g.LastActivityTool,
		&g.LastActivityDisplay, &g.PressureLevel, &pressureLevelSince,
		&g.CollectorStatus, &g.UpdatedAt, &fidelityNotes, &g.SessionTotalCostUSD)
	if err != nil {
		return g, err
	}
	if rosterID.Valid {
		g.RosterID = &rosterID.String
	}
	if statuslineJSON.Valid {
		g.StatuslineJSON = &statuslineJSON.String
	}
	if compositionJSON.Valid {
		g.CompositionJSON = &compositionJSON.String
	}
	if toolCallCountsJSON.Valid {
		g.ToolCallCountsJSON = &toolCallCountsJSON.String
	}
	if fidelityNotes.Valid {
		g.FidelityNotes = &fidelityNotes.String
	}
	if lastActivityTs.Valid {
		g.LastActivityTs = &lastActivityTs.Time
	}
	if pressureLevelSince.Valid {
		g.PressureLevelSince = &pressureLevelSince.Time
	}
	if contextReportedAt.Valid {
		g.ContextReportedAt = &contextReportedAt.Time
	}
	return g, nil
}
