package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
)

func postContextReport(t *testing.T, s *Server, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/context", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	s.handleContextReport(rec, req)
	return rec
}

func TestHandleContextReport_CreatesNewGaugeRow(t *testing.T) {
	gs := newFakeGaugeStore()
	s := &Server{gaugeStore: gs}

	rec := postContextReport(t, s, map[string]interface{}{
		"session_id":          "sess-1",
		"agent_name":          "",
		"host":                "hub01",
		"context_window_size": 1_000_000,
		"used_percentage":     26.5,
		"total_input_tokens":  265343,
		"session_cost_usd":    12.34,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	row, found, err := gs.Get(context.Background(), gauge.GaugeKey{Host: "hub01", SessionID: "sess-1", AgentName: ""})
	if err != nil || !found {
		t.Fatalf("expected gauge row to exist: found=%v err=%v", found, err)
	}
	// session_cost_usd in the request is decoded but ignored — health-collector
	// alone owns this field now (per-agent token_ledger sum, no double-counting
	// of teammate spend the way statusLine's own figure did). A brand-new row
	// (health-collector hasn't polled yet) has no cost data, so it stays 0.
	if row.SessionCostUSD != 0 {
		t.Fatalf("session_cost_usd = %v, want 0 (request's session_cost_usd is ignored)", row.SessionCostUSD)
	}
	if row.ContextWindowTokens != 1_000_000 {
		t.Fatalf("context_window_tokens = %d, want 1000000", row.ContextWindowTokens)
	}
	if row.ContextTokensUsed != 265343 {
		t.Fatalf("context_tokens_used = %d, want 265343", row.ContextTokensUsed)
	}
	if row.ContextTokensFree != 1_000_000-265343 {
		t.Fatalf("context_tokens_free = %d, want %d", row.ContextTokensFree, 1_000_000-265343)
	}
	if row.ContextFillPct != 0.265 {
		t.Fatalf("context_fill_pct = %v, want 0.265", row.ContextFillPct)
	}
	if !row.LongContextActive {
		t.Fatal("expected long_context_active = true for a 1M window")
	}
	if row.ContextSource != gauge.ContextSourceStatusline {
		t.Fatalf("context_source = %q, want %q", row.ContextSource, gauge.ContextSourceStatusline)
	}
}

func TestHandleContextReport_PreservesNonContextFields(t *testing.T) {
	gs := newFakeGaugeStore()
	s := &Server{gaugeStore: gs}

	// Simulate health-collector having already written model/token/cost totals.
	now := time.Now().UTC()
	if err := gs.Upsert(context.Background(), gauge.GaugeRow{
		Host:            "hub01",
		SessionID:       "sess-1",
		AgentName:       "",
		Runtime:         "claude_code",
		Model:           "claude-opus-4-6",
		TokensInTotal:   999,
		TokensOutTotal:  111,
		SessionCostUSD:  5.67,
		PressureLevel:   "warning",
		CollectorStatus: "fresh",
		ContextSource:   gauge.ContextSourceHeuristic,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("seed gauge row: %v", err)
	}

	rec := postContextReport(t, s, map[string]interface{}{
		"session_id":          "sess-1",
		"agent_name":          "",
		"host":                "hub01",
		"context_window_size": 200_000,
		"used_percentage":     50.0,
		"total_input_tokens":  100_000,
		"session_cost_usd":    99.99, // must be ignored — see assertion below
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	row, found, err := gs.Get(context.Background(), gauge.GaugeKey{Host: "hub01", SessionID: "sess-1", AgentName: ""})
	if err != nil || !found {
		t.Fatalf("expected gauge row to exist: found=%v err=%v", found, err)
	}
	// Untouched by the context report — owned by health-collector.
	if row.Model != "claude-opus-4-6" {
		t.Fatalf("model = %q, want preserved claude-opus-4-6", row.Model)
	}
	if row.TokensInTotal != 999 || row.TokensOutTotal != 111 {
		t.Fatalf("tokens_in/out = %d/%d, want preserved 999/111", row.TokensInTotal, row.TokensOutTotal)
	}
	if row.PressureLevel != "warning" {
		t.Fatalf("pressure_level = %q, want preserved warning", row.PressureLevel)
	}
	if row.SessionCostUSD != 5.67 {
		t.Fatalf("session_cost_usd = %v, want preserved 5.67 (request's 99.99 must be ignored)", row.SessionCostUSD)
	}
	// Updated by the context report.
	if row.ContextWindowTokens != 200_000 || row.ContextTokensUsed != 100_000 {
		t.Fatalf("context fields not updated: %+v", row)
	}
	if row.ContextSource != gauge.ContextSourceStatusline {
		t.Fatalf("context_source = %q, want %q", row.ContextSource, gauge.ContextSourceStatusline)
	}
}

// TestHandleContextReport_ModelOverride_SubagentSourced confirms a request
// carrying a model (subagentStatusLine's authoritative per-task model)
// overrides whatever token_ledger-derived value health-collector wrote —
// the whole point of the addendum (token_ledger's per-teammate model
// attribution is the thing being replaced).
func TestHandleContextReport_ModelOverride_SubagentSourced(t *testing.T) {
	gs := newFakeGaugeStore()
	s := &Server{gaugeStore: gs}

	now := time.Now().UTC()
	if err := gs.Upsert(context.Background(), gauge.GaugeRow{
		Host:      "hub01",
		SessionID: "sess-1",
		AgentName: "@scout",
		Runtime:   "claude_code",
		Model:     "claude-opus-4-6", // wrong — token_ledger misattributed this teammate's model
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed gauge row: %v", err)
	}

	rec := postContextReport(t, s, map[string]interface{}{
		"session_id":          "sess-1",
		"agent_name":          "@scout",
		"host":                "hub01",
		"context_window_size": 1_000_000,
		"used_percentage":     26.5,
		"total_input_tokens":  265343,
		"model":               "claude-sonnet-5",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	row, found, err := gs.Get(context.Background(), gauge.GaugeKey{Host: "hub01", SessionID: "sess-1", AgentName: "@scout"})
	if err != nil || !found {
		t.Fatalf("expected gauge row to exist: found=%v err=%v", found, err)
	}
	if row.Model != "claude-sonnet-5" {
		t.Fatalf("model = %q, want claude-sonnet-5 (authoritative override)", row.Model)
	}
}

// TestHandleContextReport_StatuslineJSON_Stored confirms the pre-encoded
// blob round-trips into the gauge row untouched.
func TestHandleContextReport_StatuslineJSON_Stored(t *testing.T) {
	gs := newFakeGaugeStore()
	s := &Server{gaugeStore: gs}

	blob := `{"cache_read_input_tokens":2000,"cache_creation_input_tokens":5000,"output_tokens":1200}`
	rec := postContextReport(t, s, map[string]interface{}{
		"session_id":          "sess-1",
		"agent_name":          "",
		"host":                "hub01",
		"context_window_size": 200_000,
		"used_percentage":     10.0,
		"total_input_tokens":  20_000,
		"statusline_json":     blob,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	row, found, err := gs.Get(context.Background(), gauge.GaugeKey{Host: "hub01", SessionID: "sess-1", AgentName: ""})
	if err != nil || !found {
		t.Fatalf("expected gauge row to exist: found=%v err=%v", found, err)
	}
	if row.StatuslineJSON == nil || *row.StatuslineJSON != blob {
		t.Fatalf("statusline_json = %v, want %q", row.StatuslineJSON, blob)
	}
}

func TestHandleContextReport_NilGaugeStore_ReturnsServiceUnavailable(t *testing.T) {
	s := &Server{gaugeStore: nil}
	rec := postContextReport(t, s, map[string]interface{}{"session_id": "s1", "host": "hub01"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleContextReport_MissingSessionID_BadRequest(t *testing.T) {
	s := &Server{gaugeStore: newFakeGaugeStore()}
	rec := postContextReport(t, s, map[string]interface{}{"host": "hub01"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleContextReport_WrongMethod(t *testing.T) {
	s := &Server{gaugeStore: newFakeGaugeStore()}
	req := httptest.NewRequest(http.MethodGet, "/context", nil)
	rec := httptest.NewRecorder()
	s.handleContextReport(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
