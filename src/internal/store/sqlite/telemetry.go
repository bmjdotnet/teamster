package sqlite

import (
	"context"
	"strings"

	"github.com/bmjdotnet/teamster/internal/store"
)

var _ store.TelemetryStore = (*Store)(nil)

// telemetryColumnsPerRow is the placeholder count of one VALUES group below.
// SQLite's own parameter ceiling (SQLITE_MAX_VARIABLE_NUMBER, 32766 on modern
// builds) is far more permissive than MySQL's max_allowed_packet-driven
// limit, but the same chunk size is kept for consistency with the mysql
// backend and to keep any single statement small (1000*21 = 21000
// placeholders, well under either engine's ceiling).
const telemetryColumnsPerRow = 21
const maxTelemetryRowsPerInsert = 1000

// UpsertTelemetryBatch implements store.TelemetryStore. It chunks rows into
// groups of maxTelemetryRowsPerInsert and executes one INSERT ... ON
// CONFLICT(message_id) DO UPDATE per chunk. A failing chunk is not fatal to
// the rest of the batch — every chunk is attempted and the first error
// encountered is returned, matching the caller's re-insert being idempotent
// via the uq_message unique index.
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
	// later, more-complete one must win rather than the first writer. Guarding
	// on output_tokens keeps an equal/lesser re-insert a no-op (idempotent)
	// while letting a fuller snapshot overwrite the token/cost columns.
	//
	// Unlike MySQL's ON DUPLICATE KEY UPDATE (which evaluates assignments
	// left-to-right, so output_tokens there MUST be assigned last or earlier
	// IF(...) guards would compare against an already-updated value), SQLite's
	// ON CONFLICT DO UPDATE SET has no such ordering hazard: every `excluded.*`
	// reference always means "the proposed new row's original value," and
	// every bare column reference always means "the pre-update stored value,"
	// regardless of clause order within the same SET list. So every guarded
	// column can (and does) use the identical CASE condition independently.
	//
	// agent_name/host/username/model/service_tier/speed are deliberately NOT
	// in this SET list — on conflict they keep their original stored values,
	// exactly like MySQL's ON DUPLICATE KEY UPDATE only touching columns it
	// explicitly lists. session_id is the one unconditional overwrite,
	// mirroring MySQL's `session_id = VALUES(session_id)`.
	query := queryPrefix + strings.Join(placeholders, ", ") +
		` ON CONFLICT(message_id) DO UPDATE SET
			session_id         = excluded.session_id,
			input_tokens       = CASE WHEN excluded.output_tokens > token_ledger.output_tokens THEN excluded.input_tokens ELSE token_ledger.input_tokens END,
			cache_read_tokens  = CASE WHEN excluded.output_tokens > token_ledger.output_tokens THEN excluded.cache_read_tokens ELSE token_ledger.cache_read_tokens END,
			cache_write_tokens = CASE WHEN excluded.output_tokens > token_ledger.output_tokens THEN excluded.cache_write_tokens ELSE token_ledger.cache_write_tokens END,
			cache_write_1h     = CASE WHEN excluded.output_tokens > token_ledger.output_tokens THEN excluded.cache_write_1h ELSE token_ledger.cache_write_1h END,
			cache_write_5m     = CASE WHEN excluded.output_tokens > token_ledger.output_tokens THEN excluded.cache_write_5m ELSE token_ledger.cache_write_5m END,
			n_text             = CASE WHEN excluded.output_tokens > token_ledger.output_tokens THEN excluded.n_text ELSE token_ledger.n_text END,
			n_tool_use         = CASE WHEN excluded.output_tokens > token_ledger.output_tokens THEN excluded.n_tool_use ELSE token_ledger.n_tool_use END,
			n_thinking         = CASE WHEN excluded.output_tokens > token_ledger.output_tokens THEN excluded.n_thinking ELSE token_ledger.n_thinking END,
			total_input        = CASE WHEN excluded.output_tokens > token_ledger.output_tokens THEN excluded.total_input ELSE token_ledger.total_input END,
			stop_reason        = CASE WHEN excluded.output_tokens > token_ledger.output_tokens THEN excluded.stop_reason ELSE token_ledger.stop_reason END,
			cost_usd           = CASE WHEN excluded.output_tokens > token_ledger.output_tokens THEN excluded.cost_usd ELSE token_ledger.cost_usd END,
			output_tokens      = CASE WHEN excluded.output_tokens > token_ledger.output_tokens THEN excluded.output_tokens ELSE token_ledger.output_tokens END`

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

// resolveAgentFromNames maps the set of agent_name rows recorded for a
// session to the agent that produced an empty-stamped telemetry row.
//
// The scraper stamps subagent (teammate) rows directly with "@<name>" from
// the sibling .meta.json and only ever sends an empty agent_name for the
// MAIN session transcript — which is the lead. So an empty-stamped row is
// always the lead and must resolve to "" even when teammate rows share the
// session_id (the common team case). Promoting it to a teammate name would
// steal the lead's main-file cost and assign it to whichever teammate sorted
// first.
//
// The only case where a non-empty name is returned is a solo session whose
// sole recorded row is a teammate with no lead row at all (len==1,
// non-empty), which preserves attribution for that degenerate shape.
func resolveAgentFromNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		// Team session: lead + teammates under one session_id. An
		// empty-stamped row is the lead.
		return ""
	}
}
