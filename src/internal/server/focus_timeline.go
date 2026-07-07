package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
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
	// _extract_brief_directive and store.IntervalStore.WriteBriefDirectiveInterval.
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

	if s.obsStore == nil {
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
		// WriteFocusInterval inserts unconditionally while the directive write
		// is gated on absence.
		if ev.Directive {
			err := s.obsStore.WriteBriefDirectiveInterval(ctx, ev.SessionID, agentName, ev.EntityType, ev.EntityID, briefDirectiveSource)
			switch {
			case err == nil:
				written++
			case errors.Is(err, store.ErrNotFound):
				// The brief named an entity not in WMS (typo/paraphrase). Skip +
				// count it; the session falls through to the B2 synthesized floor.
				slog.Warn("focus-timeline: brief-directive names unknown entity; skipping",
					"index", i, "session_id", ev.SessionID, "agent_name", agentName,
					"entity_type", ev.EntityType, "entity_id", ev.EntityID)
				badEntity++
				skipped++
			case errors.Is(err, store.ErrPrecondition):
				// Session already has a (real or directive) focus interval —
				// nothing to do, but not an error.
				skipped++
			default:
				slog.Warn("focus-timeline: error writing brief-directive interval",
					"index", i, "session_id", ev.SessionID, "agent_name", agentName, "error", err)
				skipped++
			}
			if host == "" {
				host = ev.Host
			}
			continue
		}

		if err := s.obsStore.WriteFocusInterval(ctx, ev.SessionID, agentName, ev.EntityType, ev.EntityID, ts); err != nil {
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

// briefDirectiveSource is the identity_source for a focus interval reconstructed
// from a focus-less teammate's dispatch brief (the intended focus the teammate
// was told to set but never did). It is distinct from 'remote_scraper' (a real
// wms_setFocus shipped from a transcript) and 'direct' (the live MCP path) so a
// directive interval is filterable, reversible, and — crucially — recognizable
// as subordinate to a real focus. focusTimelineFromIntervals consumes it like
// any other focus row, so recover-warmup/allocate attribute the session's cost
// to the resolved entity. The reverse path is RemoveBriefDirectiveIntervals.
const briefDirectiveSource = "brief_directive"
