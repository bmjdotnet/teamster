package mysql

import (
	"context"
	"strings"

	"github.com/bmjdotnet/teamster/internal/store"
)

// telemetryColumnsPerRow is the placeholder count of one VALUES group below.
// MySQL caps a prepared statement at 65535 placeholders; maxTelemetryRowsPerInsert
// keeps every chunk well under that ceiling (1000*21 = 21000).
const telemetryColumnsPerRow = 21
const maxTelemetryRowsPerInsert = 1000

// UpsertTelemetryBatch implements store.TelemetryStore. It chunks rows into
// groups of maxTelemetryRowsPerInsert to respect MySQL's placeholder limit,
// executing one INSERT ... ON DUPLICATE KEY UPDATE per chunk. A failing chunk
// is not fatal to the rest of the batch — every chunk is attempted and the
// first error encountered is returned, matching the caller's re-insert being
// idempotent via uq_message.
func (s *Store) UpsertTelemetryBatch(ctx context.Context, rows []store.TelemetryRow) (int64, error) {
	var total int64
	var firstErr error
	for start := 0; start < len(rows); start += maxTelemetryRowsPerInsert {
		end := start + maxTelemetryRowsPerInsert
		if end > len(rows) {
			end = len(rows)
		}
		n, err := s.upsertTelemetryChunk(ctx, rows[start:end])
		total += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return total, firstErr
}

func (s *Store) upsertTelemetryChunk(ctx context.Context, chunk []store.TelemetryRow) (int64, error) {
	if len(chunk) == 0 {
		return 0, nil
	}

	const queryPrefix = `INSERT INTO token_ledger
		(session_id, message_id, agent_name, host, username, model,
		 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		 cache_write_1h, cache_write_5m,
		 n_text, n_tool_use, n_thinking,
		 total_input, stop_reason, service_tier, speed,
		 cost_usd, timestamp)
	VALUES `

	args := make([]interface{}, 0, len(chunk)*telemetryColumnsPerRow)
	placeholders := make([]string, 0, len(chunk))

	for _, row := range chunk {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			row.SessionID, row.MessageID, row.AgentName, row.Host, row.Username, row.Model,
			row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheWriteTokens,
			row.CacheWrite1h, row.CacheWrite5m,
			row.NText, row.NToolUse, row.NThinking,
			row.TotalInput, row.StopReason, row.ServiceTier, row.Speed,
			row.CostUSD, row.Timestamp.UTC(),
		)
	}

	// On message_id conflict, keep the row with the greater output_tokens. The
	// scraper keys rows by (message.id|requestId) and emits the max-usage
	// member of each request's content-block lines, but a request whose lines
	// straddle a scraper poll boundary can arrive as two partial inserts; the
	// later, more-complete one must win rather than the first writer. Guarding on
	// output_tokens keeps an equal/lesser re-insert a no-op (idempotent) while
	// letting a fuller snapshot overwrite the token/cost columns.
	//
	// output_tokens MUST be assigned LAST: MySQL evaluates ON DUPLICATE KEY UPDATE
	// assignments left to right and a later expression sees the already-updated
	// value of an earlier column. If output_tokens were updated first, every
	// subsequent IF(VALUES(output_tokens) > output_tokens, …) would compare against
	// the new value (equal → false) and silently keep the stale partial cost/cache
	// columns. Keeping it last means all guards compare against the pre-update
	// output_tokens.
	query := queryPrefix + strings.Join(placeholders, ", ") +
		` ON DUPLICATE KEY UPDATE
			input_tokens       = IF(VALUES(output_tokens) > output_tokens, VALUES(input_tokens), input_tokens),
			cache_read_tokens  = IF(VALUES(output_tokens) > output_tokens, VALUES(cache_read_tokens), cache_read_tokens),
			cache_write_tokens = IF(VALUES(output_tokens) > output_tokens, VALUES(cache_write_tokens), cache_write_tokens),
			cache_write_1h     = IF(VALUES(output_tokens) > output_tokens, VALUES(cache_write_1h), cache_write_1h),
			cache_write_5m     = IF(VALUES(output_tokens) > output_tokens, VALUES(cache_write_5m), cache_write_5m),
			n_text             = IF(VALUES(output_tokens) > output_tokens, VALUES(n_text), n_text),
			n_tool_use         = IF(VALUES(output_tokens) > output_tokens, VALUES(n_tool_use), n_tool_use),
			n_thinking         = IF(VALUES(output_tokens) > output_tokens, VALUES(n_thinking), n_thinking),
			total_input        = IF(VALUES(output_tokens) > output_tokens, VALUES(total_input), total_input),
			stop_reason        = IF(VALUES(output_tokens) > output_tokens, VALUES(stop_reason), stop_reason),
			cost_usd           = IF(VALUES(output_tokens) > output_tokens, VALUES(cost_usd), cost_usd),
			session_id         = VALUES(session_id),
			output_tokens      = IF(VALUES(output_tokens) > output_tokens, VALUES(output_tokens), output_tokens)`

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AgentNameForSession implements store.TelemetryStore. It resolves the
// agent_name to stamp on an empty-stamped telemetry row for sessionID.
func (s *Store) AgentNameForSession(ctx context.Context, sessionID string) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT agent_name FROM sessions WHERE session_id = ?`, sessionID)
	if err != nil {
		return "", err
	}
	defer rows.Close() //nolint:errcheck

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return "", err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	return resolveAgentFromNames(names), nil
}

// resolveAgentFromNames maps the set of agent_name rows recorded for a session
// to the agent that produced an empty-stamped telemetry row.
//
// The scraper stamps subagent (teammate) rows directly with "@<name>" from the
// sibling .meta.json and only ever sends an empty agent_name for the MAIN
// session transcript — which is the lead. So an empty-stamped row is always the
// lead and must resolve to "" even when teammate rows share the session_id (the
// common team case). Promoting it to a teammate name — the old behavior — stole
// the lead's main-file cost and assigned it to whichever teammate sorted first.
//
// The only case where a non-empty name is returned is a solo session whose sole
// recorded row is a teammate with no lead row at all (len==1, non-empty), which
// preserves attribution for that degenerate shape.
func resolveAgentFromNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		// Team session: lead + teammates under one session_id. An empty-stamped
		// row is the lead.
		return ""
	}
}
