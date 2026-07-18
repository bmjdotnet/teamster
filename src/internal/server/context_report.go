package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
)

// longContextThreshold is the boundary above which a statusLine-reported
// context_window_size counts as an extended-context window. 200000 is the
// documented Claude Code default; anything larger means the 1M (or other
// extended) variant is active for this session's account/plan.
const longContextThreshold = 200_000

// contextReportRequest is the wire shape POSTed to /context by
// skel/lib/scripts/teamster-statusline.sh. The script normalizes both the
// main statusLine's flat context_window fields and subagentStatusLine's
// per-task fields (contextWindowSize/tokenCount) into this one shape before
// forwarding, so this handler never needs to know which Claude Code feature
// produced the data.
type contextReportRequest struct {
	SessionID         string  `json:"session_id"`
	AgentName         string  `json:"agent_name"`
	Host              string  `json:"host"`
	ContextWindowSize int64   `json:"context_window_size"`
	UsedPercentage    float64 `json:"used_percentage"`
	TotalInputTokens  int64   `json:"total_input_tokens"`
	// SessionCostUSD is decoded but intentionally unused: statusLine's own
	// cost figure double-counts every teammate's spend on top of the
	// lead's, so health-collector's per-agent token_ledger sum is the sole
	// source of row.SessionCostUSD now (see handleContextReport's doc
	// comment). Kept in the struct only so the script's payload still
	// decodes cleanly; not worth a breaking wire-shape change just to
	// drop an ignored field.
	SessionCostUSD float64 `json:"session_cost_usd"`
	// Model is only ever present on a subagentStatusLine-sourced report — the
	// authoritative per-task model Claude Code itself resolved, which
	// replaces token_ledger's buggier per-teammate model attribution. Empty
	// on a main-statusLine report, in which case row.Model is left untouched
	// (health-collector owns it from token_ledger for the lead).
	Model string `json:"model"`
	// StatuslineJSON is a pre-encoded JSON object string (built by the
	// script) holding whatever statusLine fields don't warrant their own
	// column — cache_read/creation_input_tokens + output_tokens (main) or
	// status (subagent). Rendered by dashboards when present, same as
	// composition_json/tool_call_counts_json.
	StatuslineJSON string `json:"statusline_json"`
}

// handleContextReport accepts POST /context: an authoritative, Claude-Code-
// resolved context-window snapshot forwarded by the statusLine script. It is
// a partial update — only the context-window fields (context_window_tokens,
// context_tokens_used, context_tokens_free, context_fill_pct,
// long_context_active, context_source) and statusline_json are
// unconditionally touched; model is touched only when the request carries
// one (subagentStatusLine reports only). Everything else (tokens_in/out_total,
// session_cost_usd, pressure_level, ...) is preserved from the existing gauge
// row, which is why this reads-before-writing rather than building a fresh
// gauge.GaugeRow from scratch — health-collector owns those fields and must
// not have them clobbered by a statusLine tick landing between two of its
// polls. session_cost_usd in particular is now health-collector's alone:
// it computes each agent's own cost from token_ledger's component columns
// (never total_input), which is what fixed the lead's statusLine-reported
// session_cost_usd double-counting every teammate's spend on top of its
// own — a per-agent ledger sum has no such double-count, statusLine's own
// cost field is unused here.
func (s *Server) handleContextReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.gaugeStore == nil {
		http.Error(w, `{"error":"gauge store unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req contextReportRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.SessionID == "" || req.Host == "" {
		http.Error(w, "session_id and host are required", http.StatusBadRequest)
		return
	}

	ctx := context.Background()
	key := gauge.GaugeKey{Host: shortHostname(req.Host), SessionID: req.SessionID, AgentName: req.AgentName}

	row, found, err := s.gaugeStore.Get(ctx, key)
	if err != nil {
		http.Error(w, `{"error":"gauge lookup failed"}`, http.StatusInternalServerError)
		return
	}
	if !found {
		// First signal for this agent (statusLine ticks can arrive before
		// health-collector's next 15s poll creates the base row). Seed a
		// minimal row — health-collector's own upsert fills in
		// model/tokens_in_total/tokens_out_total/pressure_level on its next
		// tick, preserving whatever context fields we set below since it
		// checks context_source before recomputing the heuristic.
		row = gauge.GaugeRow{
			Host:      req.Host,
			SessionID: req.SessionID,
			AgentName: req.AgentName,
			Runtime:   "claude_code",
		}
	}

	contextUsed := req.TotalInputTokens
	free := req.ContextWindowSize - contextUsed
	if free < 0 {
		free = 0
	}

	row.ContextWindowTokens = req.ContextWindowSize
	row.ContextTokensUsed = contextUsed
	row.ContextTokensFree = free
	row.ContextFillPct = req.UsedPercentage / 100.0
	row.LongContextActive = req.ContextWindowSize > longContextThreshold
	row.ContextSource = gauge.ContextSourceStatusline
	now := time.Now().UTC()
	row.ContextReportedAt = &now
	if req.Model != "" {
		row.Model = req.Model
	}
	if req.StatuslineJSON != "" {
		blob := req.StatuslineJSON
		row.StatuslineJSON = &blob
	}
	row.CollectorStatus = "fresh"
	row.UpdatedAt = time.Now().UTC()

	if err := s.gaugeStore.Upsert(ctx, row); err != nil {
		http.Error(w, `{"error":"gauge upsert failed"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
}
