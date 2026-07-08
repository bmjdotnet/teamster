package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/bmjdotnet/teamster/internal/store"
)

// SessionRow is the wire shape POSTed to /session: a Codex (or future
// non-hook-driven runtime) sessions-row upsert. Field set mirrors what the
// codex-scraper tailer's upsertCodexSession already builds locally — no WMS
// pointer fields (team/project/goal/task/workitem/focus), since those are
// owned by the hook/MCP-driven path this endpoint does not touch.
type SessionRow struct {
	SessionID  string `json:"session_id"`
	AgentName  string `json:"agent_name"`
	Host       string `json:"host"`
	Username   string `json:"username"`
	Runtime    string `json:"runtime"`
	Cwd        string `json:"cwd"`
	Model      string `json:"model"`
	Originator string `json:"originator"`
	CliVersion string `json:"cli_version"`
}

// handleSession accepts POST /session with a SessionRow JSON body and upserts
// it via store.UpsertSession — the same call the codex-scraper tailer made
// directly before this endpoint existed (see docs/specs/CODEX-INSTALL.md,
// "Migration path for later"). Synchronous and unbatched, unlike /telemetry:
// session upserts are one per poll (not one per token_count event), so there
// is no need for the queue/fallback-spool machinery telemetry rows need.
//
// Validation is single-sourced: this handler does not re-declare "SessionID
// is required" — store.ValidateSession is the one place that rule lives, and
// UpsertSession's own backend implementations call the identical function, so
// an HTTP caller and a direct-store caller are held to exactly the same bar.
//
// Telemetry-quiet by construction: this only calls s.obsStore.UpsertSession,
// never touching s.logFile/s.bus/s.wmsEng — no feed event or SSE publish
// results from a routine session upsert (matching the existing direct
// s.obsStore.UpsertSession call in the Stop handler, which is equally quiet).
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var row SessionRow
	if err := json.Unmarshal(body, &row); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	sess := store.Session{
		SessionID:  row.SessionID,
		AgentName:  row.AgentName,
		Host:       row.Host,
		Username:   row.Username,
		Status:     store.SessionStatusActive,
		Runtime:    row.Runtime,
		Cwd:        row.Cwd,
		Model:      row.Model,
		Originator: row.Originator,
		CliVersion: row.CliVersion,
	}

	if err := store.ValidateSession(sess); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if s.obsStore == nil {
		http.Error(w, "session store not available", http.StatusServiceUnavailable)
		return
	}

	if err := s.obsStore.UpsertSession(context.Background(), sess); err != nil {
		http.Error(w, "session upsert failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
}
