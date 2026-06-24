package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	// Directive marks an event parsed from a focus-less teammate's dispatch
	// brief rather than a real wms_setFocus call (the teammate was TOLD to
	// focus on this entity but never did). It is written as a subordinate
	// interval (identity_source='brief_directive') that NEVER overrides a real
	// focus interval for the same session+agent. See the scraper's
	// _extract_brief_directive and writeBriefDirectiveInterval below.
	Directive bool `json:"directive"`
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
	var written, skipped, badEntity int
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

		// A brief-directive event is the INTENDED focus of a focus-less teammate
		// (parsed from its dispatch brief), not a real wms_setFocus call. It is
		// written subordinately: only when the session+agent has NO focus
		// interval at all, and never overrides a real one. A real focus that
		// arrives later (this or a future batch) keeps precedence because
		// writeFocusInterval inserts unconditionally while the directive write
		// is gated on absence.
		if ev.Directive {
			res, err := s.writeBriefDirectiveInterval(ctx, ev.SessionID, agentName, ev.EntityType, ev.EntityID, ts)
			if err != nil {
				slog.Warn("focus-timeline: error writing brief-directive interval",
					"index", i, "session_id", ev.SessionID, "agent_name", agentName, "error", err)
				skipped++
				continue
			}
			switch res {
			case directiveInserted:
				written++
			case directiveBadEntity:
				// The brief named an entity not in WMS (typo/paraphrase). Skip +
				// count it; the session falls through to the B2 synthesized floor.
				slog.Warn("focus-timeline: brief-directive names unknown entity; skipping",
					"index", i, "session_id", ev.SessionID, "agent_name", agentName,
					"entity_type", ev.EntityType, "entity_id", ev.EntityID)
				badEntity++
				skipped++
			default: // directiveHasFocus
				// Session already has a (real or directive) focus interval —
				// nothing to do, but not an error.
				skipped++
			}
			if host == "" {
				host = ev.Host
			}
			continue
		}

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
		"host", host, "received", len(events), "written", written,
		"skipped", skipped, "bad_entity", badEntity)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
		"status":     "ok",
		"written":    written,
		"skipped":    skipped,
		"bad_entity": badEntity,
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

	// Cross-writer dedup (focus-interval-dual-writer fix): if an open focus
	// interval for (session, agent) is ALREADY this exact entity, this is the same
	// logical setFocus the other writer (the hub wms-mcp 'direct' path,
	// OpenFocusInterval) already opened — no-op rather than blind-close-and-reopen.
	// Without this, the scraper and the direct path each open a row for one
	// setFocus and each close the other's, collapsing both to negative width.
	// Mirrors OpenFocusInterval's same-entity guard. The FOR UPDATE serializes the
	// two writers WHEN a row already exists (the common case: one writer wins, the
	// other sees it here and no-ops). In the rarer race where BOTH writers find no
	// open row and both insert, two open rows for the SAME entity result — harmless:
	// focusAt resolves them identically (mostSpecific + same entity), and neither is
	// negative-width. The load-bearing invariant (no ended_at < started_at) is held
	// unconditionally by the ordering-safe close below, not by this dedup.
	var curType, curID string
	err = tx.QueryRowContext(ctx, `
		SELECT entity_type, entity_id FROM wms_intervals
		WHERE kind = 'focus' AND session_id = ? AND agent_name = ? AND ended_at IS NULL
		ORDER BY started_at DESC LIMIT 1 FOR UPDATE`,
		sessionID, agentName).Scan(&curType, &curID)
	if err == nil && curType == entityType && curID == entityID {
		return tx.Commit() // already focused on this exact entity
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Close any open focus interval for this (session, agent). ORDERING-SAFE: the
	// `started_at <= ?` guard means an out-of-order close (the incoming ts predates
	// an open interval's start) never produces `ended_at < started_at`; that
	// interval stays open for a later valid close. Same guard as
	// closeOpenFocusIntervals — see its comment for why this is safe on both the
	// single-writer hub and the dual-writer remote path.
	if _, err := tx.ExecContext(ctx, `
		UPDATE wms_intervals
		SET ended_at = ?,
		    duration_ms = TIMESTAMPDIFF(MICROSECOND, started_at, ?) / 1000
		WHERE kind = 'focus' AND session_id = ? AND agent_name = ? AND ended_at IS NULL
		  AND started_at <= ?`,
		at, at, sessionID, agentName, at); err != nil {
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

// briefDirectiveSource is the identity_source for a focus interval reconstructed
// from a focus-less teammate's dispatch brief (the intended focus the teammate
// was told to set but never did). It is distinct from 'remote_scraper' (a real
// wms_setFocus shipped from a transcript) and 'direct' (the live MCP path) so a
// directive interval is filterable, reversible, and — crucially — recognizable
// as subordinate to a real focus. focusTimelineFromIntervals consumes it like
// any other focus row, so recover-warmup/allocate attribute the session's cost
// to the resolved entity. The reverse path is RemoveBriefDirectiveIntervals.
const briefDirectiveSource = "brief_directive"

// directiveResult is the outcome of one brief-directive write attempt.
type directiveResult int

const (
	directiveInserted   directiveResult = iota // a directive interval was created
	directiveHasFocus                          // session already had a focus interval (subordinate no-op)
	directiveBadEntity                         // the brief named an entity that does not exist in WMS
)

// writeBriefDirectiveInterval materializes a focus-less teammate's INTENDED
// focus (from its dispatch brief) as a subordinate, open-ended focus interval —
// but ONLY when (a) the session+agent has no focus interval of ANY source yet,
// and (b) the brief's named entity actually EXISTS in WMS. It returns
// directiveInserted, directiveHasFocus, or directiveBadEntity.
//
// Entity validation (focus-interval-dual-writer / B1 guardrail): the remote
// scraper cannot check WMS, so it ships whatever entityID it parsed from the
// brief. The hub MUST confirm the entity exists before materializing an
// interval — a typo'd or paraphrased brief must NOT create a bogus focus
// interval that would mis-attribute the session's whole cost. An unknown entity
// is skipped + counted (directiveBadEntity); the session falls through to the
// B2 synthesized floor instead.
//
// The interval is open-ended (ended_at NULL): the brief is the teammate's first
// instruction, so the intended focus covers the whole session from its first
// message (`at` is the session's first-record timestamp). A real focus that
// arrives later via writeFocusInterval is NOT blocked by this row — but in
// practice a real-focus session never reaches this path because the scraper only
// emits a directive for sessions with no setFocus, and this insert is gated on
// absence anyway.
func (s *Server) writeBriefDirectiveInterval(ctx context.Context, sessionID, agentName, entityType, entityID string, at time.Time) (directiveResult, error) {
	tx, err := s.wmsDB.BeginTx(ctx, nil)
	if err != nil {
		return directiveHasFocus, err
	}
	defer tx.Rollback() //nolint:errcheck

	// Subordinate gate: do nothing if ANY focus interval already exists for this
	// (session, agent) — a real setFocus (remote_scraper/direct), or a directive
	// we already wrote. SELECT ... FOR UPDATE serializes concurrent directive
	// writers so exactly one row is inserted.
	var exists int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM wms_intervals
		WHERE kind = 'focus' AND session_id = ? AND agent_name = ?
		LIMIT 1 FOR UPDATE`,
		sessionID, agentName).Scan(&exists)
	if err == nil {
		// A focus interval already exists — leave it; directive is subordinate.
		return directiveHasFocus, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return directiveHasFocus, err
	}

	// Validate the named entity exists in WMS (workunit or outcome). A brief that
	// names a non-existent entity (typo/paraphrase) must not create an interval.
	var table string
	switch entityType {
	case "workunit":
		table = "workunits"
	case "outcome":
		table = "outcomes"
	default:
		return directiveBadEntity, tx.Commit()
	}
	var found int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM "+table+" WHERE id = ? LIMIT 1", entityID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return directiveBadEntity, tx.Commit()
	}
	if err != nil {
		return directiveHasFocus, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO wms_intervals
			(kind, entity_type, entity_id, state, session_id, agent_name, host, started_at, identity_source)
		VALUES ('focus', ?, ?, '', ?, ?, '', ?, ?)`,
		entityType, entityID, sessionID, agentName, at, briefDirectiveSource); err != nil {
		return directiveHasFocus, err
	}

	return directiveInserted, tx.Commit()
}
