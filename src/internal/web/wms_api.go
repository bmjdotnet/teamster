package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

type drainRequest struct {
	Scope          string `json:"scope"`
	SessionID      string `json:"session_id"`
	OlderThanHours int    `json:"older_than_hours"`
	DryRun         *bool  `json:"dry_run"`
}

type drainResponse struct {
	AffectedIntervals int64 `json:"affected_intervals"`
	DryRun            bool  `json:"dry_run"`
}

func writeJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}

func HandleDrainAPI(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if st == nil {
			writeJSONError(w, "WMS store unavailable", http.StatusServiceUnavailable)
			return
		}

		var req drainRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		isDryRun := req.DryRun == nil || *req.DryRun

		ctx := r.Context()
		resp := drainResponse{DryRun: isDryRun}

		if isDryRun {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
			return
		}

		var n int64
		var err error

		switch req.Scope {
		case "done-entities":
			n, err = st.CloseIntervalsOnTerminalEntities(ctx)
		case "session":
			if req.SessionID == "" {
				writeJSONError(w, "session_id required for scope=session", http.StatusBadRequest)
				return
			}
			n, err = st.CloseSessionIntervals(ctx, req.SessionID, "", time.Now().UTC())
		case "older-than":
			if req.OlderThanHours <= 0 {
				writeJSONError(w, "older_than_hours must be positive for scope=older-than", http.StatusBadRequest)
				return
			}
			threshold := time.Now().UTC().Add(-time.Duration(req.OlderThanHours) * time.Hour)
			n, err = st.CloseIntervalsForStaleSessions(ctx, threshold)
		case "":
			n1, err1 := st.CloseIntervalsOnTerminalEntities(ctx)
			if err1 != nil {
				writeJSONError(w, err1.Error(), http.StatusInternalServerError)
				return
			}
			n2, err2 := st.CloseIntervalsForClosedSessions(ctx)
			if err2 != nil {
				writeJSONError(w, err2.Error(), http.StatusInternalServerError)
				return
			}
			n = n1 + n2
		default:
			writeJSONError(w, "scope must be done-entities, session, or older-than", http.StatusBadRequest)
			return
		}

		if err != nil {
			writeJSONError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		resp.AffectedIntervals = n
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}
}
