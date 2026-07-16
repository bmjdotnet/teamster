package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/sqlite"
)

// fakeGaugeStore is a trivial in-memory gauge.GaugeStore for health_api tests
// — mirrors internal/mcp/health's own test fake (duplicated: it's a small,
// package-local test double, not worth exporting a shared one for).
type fakeGaugeStore struct {
	mu   sync.Mutex
	rows map[gauge.GaugeKey]gauge.GaugeRow
}

func newFakeGaugeStore() *fakeGaugeStore {
	return &fakeGaugeStore{rows: make(map[gauge.GaugeKey]gauge.GaugeRow)}
}

func (f *fakeGaugeStore) Upsert(_ context.Context, row gauge.GaugeRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[gauge.GaugeKey{Host: row.Host, SessionID: row.SessionID, AgentName: row.AgentName}] = row
	return nil
}

func (f *fakeGaugeStore) Get(_ context.Context, key gauge.GaugeKey) (gauge.GaugeRow, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[key]
	return row, ok, nil
}

func (f *fakeGaugeStore) List(_ context.Context, filter gauge.GaugeFilter) ([]gauge.GaugeRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gauge.GaugeRow
	for _, row := range f.rows {
		if filter.RosterID != "" && (row.RosterID == nil || *row.RosterID != filter.RosterID) {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

func (f *fakeGaugeStore) SweepOffline(_ context.Context, cutoff time.Time) (int, error) {
	return 0, nil
}

func (f *fakeGaugeStore) UpdateActivity(_ context.Context, key gauge.GaugeKey, display, tool string, ts time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := f.rows[key]
	row.LastActivityDisplay = display
	row.LastActivityTool = tool
	row.LastActivityTs = &ts
	f.rows[key] = row
	return nil
}

func newTestHealthMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/api/agents", s.handleHealthAgentsAPI)
	mux.HandleFunc("GET /health/api/agents/{roster_id}", s.handleHealthSnapshotAPI)
	mux.HandleFunc("GET /health/api/alerts", s.handleHealthAlertsAPI)
	mux.HandleFunc("GET /health/api/team/{team_name}", s.handleHealthTeamAPI)
	return mux
}

func seedHealthAPIAgent(t *testing.T, ms store.Store, gs *fakeGaugeStore, rosterID, sessionID, agentName, teamName string, fillPct float64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	sid := sessionID
	if err := ms.CreateRosterEntry(ctx, store.RosterEntry{
		RosterID:  rosterID,
		SessionID: &sid,
		AgentName: agentName,
		Host:      "host-a",
		Runtime:   "claude_code",
		TeamName:  teamName,
		CreatedAt: now,
		BoundAt:   &now,
	}); err != nil {
		t.Fatalf("seed roster: %v", err)
	}
	_ = ms.UpsertSession(ctx, store.Session{
		SessionID: sessionID,
		AgentName: agentName,
		Host:      "host-a",
		LastSeen:  now,
		Status:    store.SessionStatusActive,
	})
	rid := rosterID
	_ = gs.Upsert(ctx, gauge.GaugeRow{
		Host:            "host-a",
		SessionID:       sessionID,
		AgentName:       agentName,
		RosterID:        &rid,
		Runtime:         "claude_code",
		Model:           "opus",
		PressureLevel:   "ok",
		CollectorStatus: "fresh",
		ContextFillPct:  fillPct,
		UpdatedAt:       now,
		LastActivityTs:  &now,
	})
}

func openTestObsStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestHealthAgentsAPI_ReturnsAgents(t *testing.T) {
	ms := openTestObsStore(t)
	gs := newFakeGaugeStore()
	seedHealthAPIAgent(t, ms, gs, "r-1", "sess-1", "@scout", "ops", 25.0)

	s := &Server{obsStore: ms, gaugeStore: gs}
	mux := newTestHealthMux(s)

	req := httptest.NewRequest(http.MethodGet, "/health/api/agents", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var views []struct {
		AgentName string `json:"agent_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, rec.Body.String())
	}
	if len(views) != 1 || views[0].AgentName != "@scout" {
		t.Fatalf("views = %+v, want [@scout]", views)
	}
}

func TestHealthAgentsAPI_NilGaugeStore_ReturnsServiceUnavailable(t *testing.T) {
	s := &Server{obsStore: openTestObsStore(t), gaugeStore: nil}
	mux := newTestHealthMux(s)

	req := httptest.NewRequest(http.MethodGet, "/health/api/agents", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHealthAlertsAPI_NoAlerts_NullPassthrough(t *testing.T) {
	s := &Server{obsStore: openTestObsStore(t), gaugeStore: newFakeGaugeStore()}
	mux := newTestHealthMux(s)

	req := httptest.NewRequest(http.MethodGet, "/health/api/alerts", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Gotcha per design doc §1.1: a nil-slice JSONResult marshals to the 4
	// bytes "null", passed straight through — both UIs must treat null as an
	// empty list. Confirm the veneer does not paper over this.
	body := rec.Body.String()
	if body != "null" {
		t.Fatalf("body = %q, want literal null for zero alerts", body)
	}
	var alerts []struct{ AgentName string }
	if err := json.Unmarshal(rec.Body.Bytes(), &alerts); err != nil {
		t.Fatalf("null must unmarshal cleanly into a slice: %v", err)
	}
	if alerts != nil {
		t.Fatalf("expected nil slice from null, got %+v", alerts)
	}
}

func TestHealthSnapshotAPI_PathValue(t *testing.T) {
	ms := openTestObsStore(t)
	gs := newFakeGaugeStore()
	seedHealthAPIAgent(t, ms, gs, "r-snap", "sess-snap", "@lead", "team-x", 60.0)

	s := &Server{obsStore: ms, gaugeStore: gs}
	mux := newTestHealthMux(s)

	req := httptest.NewRequest(http.MethodGet, "/health/api/agents/r-snap", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var snap struct {
		RosterID *string `json:"roster_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.RosterID == nil || *snap.RosterID != "r-snap" {
		t.Fatalf("roster_id = %v, want r-snap", snap.RosterID)
	}
}

func TestHealthSnapshotAPI_NotFound_Maps404(t *testing.T) {
	s := &Server{obsStore: openTestObsStore(t), gaugeStore: newFakeGaugeStore()}
	mux := newTestHealthMux(s)

	req := httptest.NewRequest(http.MethodGet, "/health/api/agents/does-not-exist", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rec.Code, rec.Body.String())
	}
}

func TestHealthTeamAPI_PathValue(t *testing.T) {
	ms := openTestObsStore(t)
	gs := newFakeGaugeStore()
	seedHealthAPIAgent(t, ms, gs, "r-1", "sess-1", "@a", "alpha", 30.0)
	seedHealthAPIAgent(t, ms, gs, "r-2", "sess-2", "@b", "alpha", 50.0)
	seedHealthAPIAgent(t, ms, gs, "r-3", "sess-3", "@c", "beta", 10.0)

	s := &Server{obsStore: ms, gaugeStore: gs}
	mux := newTestHealthMux(s)

	req := httptest.NewRequest(http.MethodGet, "/health/api/team/alpha", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var summary struct {
		TeamName   string `json:"team_name"`
		AgentCount int    `json:"agent_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if summary.TeamName != "alpha" || summary.AgentCount != 2 {
		t.Fatalf("summary = %+v, want team alpha with 2 agents (not beta's)", summary)
	}
}
