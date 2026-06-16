package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// focusTimelineEvent is one event in the POST /focus-timeline batch from the
// remote token-scraper. Each event represents an agent's wms_setFocus call
// extracted from a transcript on a remote host.
type focusTimelineEvent struct {
	Type       string `json:"type"`
	SessionID  string `json:"session_id"`
	Host       string `json:"host"`
	Username   string `json:"username"`
	AgentName  string `json:"agent_name"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Focus      string `json:"focus"`
	Timestamp  string `json:"timestamp"`
}

// handleFocusTimeline accepts POST /focus-timeline with a JSON array of focus
// events from the remote token-scraper and writes them into wms_intervals as
// kind='focus' rows. The normal Allocate pass then finds them via focusAt() and
// attributes cost — no RecoverFocus needed for remote sessions.
func (s *Server) handleFocusTimeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.wmsDB == nil {
		http.Error(w, "store not available", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var events []focusTimelineEvent
	if err := json.Unmarshal(body, &events); err != nil {
		http.Error(w, "invalid json: expected array of focus events", http.StatusBadRequest)
		return
	}

	if len(events) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "written": 0}) //nolint:errcheck
		return
	}

	ctx := r.Context()
	var written, skipped int
	var host string

	for i, ev := range events {
		if ev.SessionID == "" || ev.EntityType == "" || ev.EntityID == "" || ev.Timestamp == "" {
			slog.Warn("focus-timeline: skipping event with missing fields", "index", i,
				"session_id", ev.SessionID, "entity_type", ev.EntityType,
				"entity_id", ev.EntityID, "timestamp", ev.Timestamp)
			skipped++
			continue
		}

		ts, err := time.Parse(time.RFC3339, ev.Timestamp)
		if err != nil {
			// Try RFC3339Nano as fallback.
			ts, err = time.Parse(time.RFC3339Nano, ev.Timestamp)
			if err != nil {
				slog.Warn("focus-timeline: skipping event with bad timestamp",
					"index", i, "timestamp", ev.Timestamp, "error", err)
				skipped++
				continue
			}
		}
		ts = ts.UTC()

		agentName := normalizeAgentName(ev.AgentName)

		if err := s.writeFocusInterval(ctx, ev.SessionID, agentName, ev.EntityType, ev.EntityID, ts); err != nil {
			slog.Warn("focus-timeline: error writing interval",
				"index", i, "session_id", ev.SessionID, "agent_name", agentName, "error", err)
			skipped++
			continue
		}
		written++

		if host == "" {
			host = ev.Host
		}
	}

	slog.Info("focus-timeline: batch processed",
		"host", host, "received", len(events), "written", written, "skipped", skipped)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
		"status":  "ok",
		"written": written,
		"skipped": skipped,
	})
}

// normalizeAgentName ensures agent_name uses the canonical form: "" for lead,
// "@name" for teammates. The scraper may send with or without the @ prefix.
func normalizeAgentName(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.HasPrefix(raw, "@") {
		return "@" + raw
	}
	return raw
}

// writeFocusInterval closes any open focus interval for (session, agent) at the
// given timestamp and inserts a new one. Uses INSERT IGNORE with a dedup key of
// (session_id, agent_name, entity_type, entity_id, started_at) to be idempotent
// on scraper re-sends.
func (s *Server) writeFocusInterval(ctx context.Context, sessionID, agentName, entityType, entityID string, at time.Time) error {
	tx, err := s.wmsDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// Close any open focus interval for this (session, agent).
	if _, err := tx.ExecContext(ctx, `
		UPDATE wms_intervals
		SET ended_at = ?,
		    duration_ms = TIMESTAMPDIFF(MICROSECOND, started_at, ?) / 1000
		WHERE kind = 'focus' AND session_id = ? AND agent_name = ? AND ended_at IS NULL`,
		at, at, sessionID, agentName); err != nil {
		return err
	}

	// Insert the new focus interval. INSERT IGNORE handles dedup: if the same
	// (session_id, agent_name, entity_type, entity_id, started_at) already exists
	// via the uq_open unique index, the INSERT is a no-op.
	if _, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO wms_intervals
			(kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus', ?, ?, '', ?, ?, '', ?, 'remote_scraper')`,
		entityType, entityID, sessionID, agentName, at); err != nil {
		return err
	}

	return tx.Commit()
}
