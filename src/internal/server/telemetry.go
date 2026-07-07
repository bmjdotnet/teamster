package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
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

	agentName, err := s.obsStore.AgentNameForSession(context.Background(), sessionID)
	if err != nil {
		return ""
	}

	s.telemetryAgents.mu.Lock()
	s.telemetryAgents.cache[sessionID] = agentName
	s.telemetryAgents.mu.Unlock()
	return agentName
}

// telemetryColumnsPerRow is the placeholder count of one VALUES group below.
// MySQL caps a prepared statement at 65535 placeholders; maxTelemetryRowsPerInsert
// keeps every chunk well under that ceiling (1000*21 = 21000).
const telemetryColumnsPerRow = 21
const maxTelemetryRowsPerInsert = 1000

// flushTelemetryBatch inserts batch in chunks of maxTelemetryRowsPerInsert and
// returns the first chunk error encountered. A failing chunk is logged with its
// index, row count, and error, and the remaining chunks are still attempted —
// re-inserts are idempotent via UpsertTelemetryBatch's uq_message-keyed
// conflict resolution, so a later drain of the same rows is harmless.
func (s *Server) flushTelemetryBatch(batch []TelemetryRow) error {
	if len(batch) == 0 || s.obsStore == nil {
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
	rows := make([]store.TelemetryRow, 0, len(chunk))

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

		rows = append(rows, store.TelemetryRow{
			SessionID:        row.SessionID,
			MessageID:        row.MessageID,
			AgentName:        agentName,
			Host:             row.Host,
			Username:         row.Username,
			Model:            row.Model,
			InputTokens:      row.InputTokens,
			OutputTokens:     row.OutputTokens,
			CacheReadTokens:  row.CacheReadTokens,
			CacheWriteTokens: row.CacheWriteTokens,
			CacheWrite1h:     row.CacheWrite1h,
			CacheWrite5m:     row.CacheWrite5m,
			NText:            int64(row.NText),
			NToolUse:         int64(row.NToolUse),
			NThinking:        int64(row.NThinking),
			TotalInput:       row.TotalInput,
			StopReason:       row.StopReason,
			ServiceTier:      row.ServiceTier,
			Speed:            row.Speed,
			CostUSD:          row.CostUSD,
			Timestamp:        ts.UTC(),
		})
	}

	if len(rows) == 0 {
		return nil
	}

	_, err := s.obsStore.UpsertTelemetryBatch(context.Background(), rows)
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
		// Re-draining already-committed rows is idempotent via
		// UpsertTelemetryBatch's conflict-resolution contract, so retaining the
		// whole spool is safe.
		slog.Error("telemetry: fallback drain failed, retaining spool for retry",
			"rows", len(rows), "error", err)
		return
	}

	os.Truncate(s.telemetry.fallback, 0) //nolint:errcheck
}
