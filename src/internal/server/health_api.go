package server

import (
	"encoding/json"
	"net/http"

	mcphealth "github.com/bmjdotnet/teamster/internal/mcp/health"
)

// writeHealthAPIError maps a health CallError onto an HTTP status and writes
// a {"error": message} JSON body: NOT_FOUND -> 404, INVALID_ARGUMENT -> 400,
// anything else -> 500.
func writeHealthAPIError(w http.ResponseWriter, cerr *mcphealth.CallError) {
	status := http.StatusInternalServerError
	switch cerr.Reason {
	case "NOT_FOUND":
		status = http.StatusNotFound
	case "INVALID_ARGUMENT":
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": cerr.Message}) //nolint:errcheck
}

// handleHealthAgentsAPI serves GET /health/api/agents, an unscoped operator
// veneer over health_listAgents (see mcphealth.DashboardJSON).
func (s *Server) handleHealthAgentsAPI(w http.ResponseWriter, r *http.Request) {
	if s.gaugeStore == nil || s.obsStore == nil {
		http.Error(w, `{"error":"gauge store unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	args := map[string]interface{}{}
	q := r.URL.Query()
	if v := q.Get("host"); v != "" {
		args["host"] = v
	}
	if v := q.Get("runtime"); v != "" {
		args["runtime"] = v
	}
	if vs := q["liveness"]; len(vs) > 0 {
		arr := make([]interface{}, len(vs))
		for i, v := range vs {
			arr[i] = v
		}
		args["liveness"] = arr
	}

	payload, cerr := mcphealth.DashboardJSON(s.obsStore, s.gaugeStore, s.turnStates.IsProcessing, "health_listAgents", args)
	if cerr != nil {
		writeHealthAPIError(w, cerr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload) //nolint:errcheck
}

// handleHealthSnapshotAPI serves GET /health/api/agents/{roster_id}, an
// unscoped operator veneer over health_getAgentSnapshot.
func (s *Server) handleHealthSnapshotAPI(w http.ResponseWriter, r *http.Request) {
	if s.gaugeStore == nil || s.obsStore == nil {
		http.Error(w, `{"error":"gauge store unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	args := map[string]interface{}{"roster_id": r.PathValue("roster_id")}
	payload, cerr := mcphealth.DashboardJSON(s.obsStore, s.gaugeStore, s.turnStates.IsProcessing, "health_getAgentSnapshot", args)
	if cerr != nil {
		writeHealthAPIError(w, cerr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload) //nolint:errcheck
}

// handleHealthAlertsAPI serves GET /health/api/alerts, an unscoped operator
// veneer over health_getPressureAlerts.
func (s *Server) handleHealthAlertsAPI(w http.ResponseWriter, r *http.Request) {
	if s.gaugeStore == nil || s.obsStore == nil {
		http.Error(w, `{"error":"gauge store unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	payload, cerr := mcphealth.DashboardJSON(s.obsStore, s.gaugeStore, s.turnStates.IsProcessing, "health_getPressureAlerts", map[string]interface{}{})
	if cerr != nil {
		writeHealthAPIError(w, cerr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload) //nolint:errcheck
}

// handleHealthTeamAPI serves GET /health/api/team/{team_name}, an unscoped
// operator veneer over health_getTeamSummary.
func (s *Server) handleHealthTeamAPI(w http.ResponseWriter, r *http.Request) {
	if s.gaugeStore == nil || s.obsStore == nil {
		http.Error(w, `{"error":"gauge store unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	args := map[string]interface{}{"team_name": r.PathValue("team_name")}
	payload, cerr := mcphealth.DashboardJSON(s.obsStore, s.gaugeStore, s.turnStates.IsProcessing, "health_getTeamSummary", args)
	if cerr != nil {
		writeHealthAPIError(w, cerr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload) //nolint:errcheck
}
