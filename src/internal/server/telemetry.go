package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// TelemetryRow is a single token-usage record posted by the scraper.
type TelemetryRow struct {
	MessageID        string  `json:"message_id"`
	SessionID        string  `json:"session_id"`
	AgentName        string  `json:"agent_name"`
	Host             string  `json:"host"`
	Username         string  `json:"username"`
	Model            string  `json:"model"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheWrite1h     int64   `json:"cache_write_1h"`
	CacheWrite5m     int64   `json:"cache_write_5m"`
	NText            int     `json:"n_text"`
	NToolUse         int     `json:"n_tool_use"`
	NThinking        int     `json:"n_thinking"`
	TotalInput       int64   `json:"total_input"`
	StopReason       string  `json:"stop_reason"`
	ServiceTier      string  `json:"service_tier"`
	Speed            string  `json:"speed"`
	CostUSD          float64 `json:"cost_usd"`
	Timestamp        string  `json:"timestamp"`
}

// telemetryQueue holds the channel and fallback path for the telemetry writer.
type telemetryQueue struct {
	ch       chan TelemetryRow
	fallback string
}

// agentCache caches session_id → agent_name lookups to avoid repeated DB queries.
type agentCache struct {
	mu    sync.RWMutex
	cache map[string]string
}

// handleTelemetry accepts POST /telemetry with a TelemetryRow JSON body.
func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var row TelemetryRow
	if err := json.Unmarshal(body, &row); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if row.MessageID == "" || row.SessionID == "" || row.Timestamp == "" {
		http.Error(w, "missing required fields: message_id, session_id, timestamp", http.StatusBadRequest)
		return
	}

	if s.telemetry == nil {
		http.Error(w, "telemetry store not available", http.StatusServiceUnavailable)
		return
	}

	select {
	case s.telemetry.ch <- row:
	default:
		if err := s.writeTelemetryFallback(row); err != nil {
			slog.Error("telemetry fallback write failed", "error", err)
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"status":"queued"}`)) //nolint:errcheck
}

func (s *Server) writeTelemetryFallback(row TelemetryRow) error {
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.telemetry.fallback, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	_, err = f.Write(append(data, '\n'))
	return err
}

// startTelemetryWriter is the long-lived goroutine that batches and inserts rows.
func (s *Server) startTelemetryWriter() {
	ctx := s.telemetryCtx

	s.drainTelemetryFallback(ctx)

	batch := make([]TelemetryRow, 0, 100)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	// Live-writer inserts ignore the chunk error: flushTelemetryBatch already
	// logs each failing chunk, and a live batch is never re-spooled (matching
	// the original drop-on-failure behavior — the spool is a channel-overflow
	// buffer only). Only drainTelemetryFallback acts on the returned error.
	for {
		select {
		case <-ctx.Done():
			if len(batch) > 0 {
				s.flushTelemetryBatch(batch) //nolint:errcheck
			}
			for {
				select {
				case row := <-s.telemetry.ch:
					batch = append(batch, row)
					if len(batch) >= 100 {
						s.flushTelemetryBatch(batch) //nolint:errcheck
						batch = batch[:0]
					}
				default:
					if len(batch) > 0 {
						s.flushTelemetryBatch(batch) //nolint:errcheck
					}
					return
				}
			}
		case row := <-s.telemetry.ch:
			batch = append(batch, row)
			if len(batch) >= 100 {
				s.flushTelemetryBatch(batch) //nolint:errcheck
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				s.flushTelemetryBatch(batch) //nolint:errcheck
				batch = batch[:0]
			}
		}
	}
}

func (s *Server) resolveAgentForTelemetry(sessionID string) string {
	s.telemetryAgents.mu.RLock()
	if name, ok := s.telemetryAgents.cache[sessionID]; ok {
		s.telemetryAgents.mu.RUnlock()
		return name
	}
	s.telemetryAgents.mu.RUnlock()

	rows, err := s.wmsDB.Query(
		`SELECT agent_name FROM sessions WHERE session_id = ?`, sessionID)
	if err != nil {
		return ""
	}
	defer rows.Close() //nolint:errcheck

	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}

	agentName := resolveAgentFromNames(names)

	s.telemetryAgents.mu.Lock()
	s.telemetryAgents.cache[sessionID] = agentName
	s.telemetryAgents.mu.Unlock()
	return agentName
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

// telemetryColumnsPerRow is the placeholder count of one VALUES group below.
// MySQL caps a prepared statement at 65535 placeholders; maxTelemetryRowsPerInsert
// keeps every chunk well under that ceiling (1000*21 = 21000).
const telemetryColumnsPerRow = 21
const maxTelemetryRowsPerInsert = 1000

// flushTelemetryBatch inserts batch in chunks of maxTelemetryRowsPerInsert and
// returns the first chunk error encountered. A failing chunk is logged with its
// index, row count, and error, and the remaining chunks are still attempted —
// re-inserts are idempotent via the uq_message unique key + ON DUPLICATE KEY
// UPDATE, so a later drain of the same rows is harmless.
func (s *Server) flushTelemetryBatch(batch []TelemetryRow) error {
	if len(batch) == 0 || s.wmsDB == nil {
		return nil
	}

	var firstErr error
	for start, chunkIdx := 0, 0; start < len(batch); start, chunkIdx = start+maxTelemetryRowsPerInsert, chunkIdx+1 {
		end := start + maxTelemetryRowsPerInsert
		if end > len(batch) {
			end = len(batch)
		}
		chunk := batch[start:end]
		if err := s.insertTelemetryChunk(chunk); err != nil {
			slog.Error("telemetry chunk insert failed",
				"chunk", chunkIdx, "rows", len(chunk), "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *Server) insertTelemetryChunk(chunk []TelemetryRow) error {
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
		agentName := row.AgentName
		if agentName == "" {
			agentName = s.resolveAgentForTelemetry(row.SessionID)
		}

		ts, err := time.Parse(time.RFC3339Nano, row.Timestamp)
		if err != nil {
			slog.Warn("telemetry: bad timestamp, skipping", "message_id", row.MessageID, "error", err)
			continue
		}

		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			row.SessionID, row.MessageID, agentName, row.Host, row.Username, row.Model,
			row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheWriteTokens,
			row.CacheWrite1h, row.CacheWrite5m,
			row.NText, row.NToolUse, row.NThinking,
			row.TotalInput, row.StopReason, row.ServiceTier, row.Speed,
			row.CostUSD, ts.UTC(),
		)
	}

	if len(placeholders) == 0 {
		return nil
	}

	// On message_id conflict, keep the row with the greater output_tokens. The
	// scraper now keys rows by (message.id|requestId) and emits the max-usage
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

	_, err := s.wmsDB.Exec(query, args...)
	return err
}

func (s *Server) drainTelemetryFallback(ctx context.Context) {
	data, err := os.ReadFile(s.telemetry.fallback)
	if err != nil || len(data) == 0 {
		return
	}

	var rows []TelemetryRow
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var row TelemetryRow
		if json.Unmarshal(line, &row) == nil {
			rows = append(rows, row)
		}
	}

	if len(rows) == 0 {
		os.Truncate(s.telemetry.fallback, 0) //nolint:errcheck
		return
	}

	slog.Info("telemetry: draining fallback", "rows", len(rows))
	if err := s.flushTelemetryBatch(rows); err != nil {
		// Retain the spool so the next hookd restart re-drains it (now chunked).
		// Re-draining already-committed rows is idempotent via uq_message +
		// ON DUPLICATE KEY UPDATE, so retaining the whole spool is safe.
		slog.Error("telemetry: fallback drain failed, retaining spool for retry",
			"rows", len(rows), "error", err)
		return
	}

	os.Truncate(s.telemetry.fallback, 0) //nolint:errcheck
}
