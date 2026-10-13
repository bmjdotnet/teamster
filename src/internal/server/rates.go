package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

type rateJSON struct {
	ID                  int64      `json:"id"`
	Runtime             string     `json:"runtime"`
	MatchKind           string     `json:"match_kind"`
	ModelKey            string     `json:"model_key"`
	Variant             string     `json:"variant"`
	InputPerMtok        string     `json:"input_per_mtok"`
	OutputPerMtok       string     `json:"output_per_mtok"`
	CacheReadPerMtok    string     `json:"cache_read_per_mtok"`
	CacheWrite5mPerMtok string     `json:"cache_write_5m_per_mtok"`
	CacheWrite1hPerMtok string     `json:"cache_write_1h_per_mtok"`
	ValidFrom           time.Time  `json:"valid_from"`
	ValidTo             *time.Time `json:"valid_to"`
	SourceURL           string     `json:"source_url"`
	FetchedAt           time.Time  `json:"fetched_at"`
	Notes               string     `json:"notes"`
}

type ratesResponse struct {
	Rates     []rateJSON `json:"rates"`
	Count     int        `json:"count"`
	FetchedAt time.Time  `json:"fetched_at"`
}

// formatRatePerMtok renders a rate at its DECIMAL(12,6) storage precision so
// consumers parse an exact decimal string instead of a binary float.
func formatRatePerMtok(v float64) string {
	return strconv.FormatFloat(v, 'f', 6, 64)
}

func toRateJSON(r store.ModelRate) rateJSON {
	out := rateJSON{
		ID:                  r.ID,
		Runtime:             r.Runtime,
		MatchKind:           r.MatchKind,
		ModelKey:            r.ModelKey,
		Variant:             r.Variant,
		InputPerMtok:        formatRatePerMtok(r.InputPerMtok),
		OutputPerMtok:       formatRatePerMtok(r.OutputPerMtok),
		CacheReadPerMtok:    formatRatePerMtok(r.CacheReadPerMtok),
		CacheWrite5mPerMtok: formatRatePerMtok(r.CacheWrite5mPerMtok),
		CacheWrite1hPerMtok: formatRatePerMtok(r.CacheWrite1hPerMtok),
		ValidFrom:           r.ValidFrom.UTC(),
		SourceURL:           r.SourceURL,
		FetchedAt:           r.FetchedAt.UTC(),
		Notes:               r.Notes,
	}
	if r.ValidTo != nil {
		vt := r.ValidTo.UTC()
		out.ValidTo = &vt
	}
	return out
}

func writeRatesError(w http.ResponseWriter, status int, msg string) {
	body, _ := json.Marshal(map[string]string{"error": msg})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body) //nolint:errcheck
}

// handleRates serves GET /rates, the model_pricing rate table as JSON, for
// scrapers that have no direct store connection. Pure read: registered in
// both read-only and write-capable modes. Optional ?runtime= narrows in the
// store; ?model= matches model_key exactly.
func (s *Server) handleRates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeRatesError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.obsStore == nil {
		writeRatesError(w, http.StatusServiceUnavailable, "rate store unavailable")
		return
	}

	q := r.URL.Query()
	rates, err := s.obsStore.ListRates(r.Context(), store.RateFilter{Runtime: q.Get("runtime")})
	if err != nil {
		slog.Error("list rates", "error", err)
		writeRatesError(w, http.StatusInternalServerError, "internal error")
		return
	}

	model := q.Get("model")
	out := make([]rateJSON, 0, len(rates))
	for _, rate := range rates {
		if model != "" && rate.ModelKey != model {
			continue
		}
		out = append(out, toRateJSON(rate))
	}

	body, err := json.Marshal(ratesResponse{
		Rates:     out,
		Count:     len(out),
		FetchedAt: time.Now().UTC(),
	})
	if err != nil {
		slog.Error("encode rates", "error", err)
		writeRatesError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body) //nolint:errcheck
}
