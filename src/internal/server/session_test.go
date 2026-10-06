package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/storetest"
)

// freshSessionDB mirrors freshTelemetryDB: a throwaway fully-migrated MySQL
// schema wired into a bare Server, scoped for handleSession tests. SKIPs when
// TEAMSTER_TEST_MYSQL_DSN is unset.
func freshSessionDB(t *testing.T) (*Server, store.Store) {
	t.Helper()
	st := storetest.Open(t, "teamster_session")
	return &Server{obsStore: st}, st
}

// TestHandleSession_Upsert proves the endpoint wraps store.UpsertSession
// end-to-end (docs/specs/CODEX-INSTALL.md "Migration path for later"): a POST
// lands a row readable via GetSession, carrying the Codex-only fields
// (runtime/cwd/model/originator/cli_version) the codex-scraper tailer sends.
func TestHandleSession_Upsert(t *testing.T) {
	s, db := freshSessionDB(t)

	body := `{"session_id":"sess-1","agent_name":"@worker","host":"hub-1","username":"testuser",` +
		`"runtime":"codex","cwd":"/tmp/test-workspace","model":"gpt-5-codex","originator":"codex_exec","cli_version":"0.142.5"}`
	req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	sess, err := db.GetSession(context.Background(), store.SessionKey{SessionID: "sess-1", AgentName: "@worker"})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Host != "hub-1" || sess.Username != "testuser" || sess.Runtime != "codex" ||
		sess.Cwd != "/tmp/test-workspace" || sess.Model != "gpt-5-codex" ||
		sess.Originator != "codex_exec" || sess.CliVersion != "0.142.5" {
		t.Fatalf("session row = %+v, fields did not round-trip", sess)
	}
}

// TestHandleSession_Idempotent proves a repeated identical POST (the remote/
// hub-local timer re-posts every run) is a no-op that never errors and never
// creates a second row for the same (session_id, agent_name) key.
func TestHandleSession_Idempotent(t *testing.T) {
	s, db := freshSessionDB(t)
	body := `{"session_id":"sess-2","host":"hub-1","username":"testuser","runtime":"codex"}`

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleSession(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("post %d: status = %d, want 200; body = %s", i, w.Code, w.Body.String())
		}
	}

	var count int
	storetest.QueryRow(t, context.Background(), db,
		`SELECT COUNT(*) FROM sessions WHERE session_id = ?`, []any{"sess-2"}, &count)
	if count != 1 {
		t.Fatalf("row count = %d, want 1 (re-posts must upsert, not insert)", count)
	}
}

func TestHandleSession_MissingSessionID(t *testing.T) {
	s, _ := freshSessionDB(t)
	req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(`{"host":"hub-1"}`))
	w := httptest.NewRecorder()
	s.handleSession(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}

func TestHandleSession_InvalidJSON(t *testing.T) {
	s, _ := freshSessionDB(t)
	req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(`not json`))
	w := httptest.NewRecorder()
	s.handleSession(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}

func TestHandleSession_WrongMethod(t *testing.T) {
	s, _ := freshSessionDB(t)
	req := httptest.NewRequest(http.MethodGet, "/session", nil)
	w := httptest.NewRecorder()
	s.handleSession(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body = %s", w.Code, w.Body.String())
	}
}

// TestHandleSession_NoStore covers hookd running with no store configured
// (cfg.StoreDSN unset) — same degraded posture /telemetry documents.
func TestHandleSession_NoStore(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(`{"session_id":"sess-3"}`))
	w := httptest.NewRecorder()
	s.handleSession(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
}

// TestSession_ReadOnlyModeRejects proves RegisterRoutes wires /session to the
// same reject-with-403 handler as /telemetry when hookd runs read-only
// (replicas) — the rejection lives at the routing layer, not inside
// handleSession itself, exactly mirroring handleTelemetry's posture (neither
// handler has an inline read-only check; RegisterRoutes swaps the whole route
// to `reject` before the handler is ever reached).
func TestSession_ReadOnlyModeRejects(t *testing.T) {
	s := &Server{cfg: config.Config{ReadOnly: true}}
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(`{"session_id":"sess-4"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (read-only mode); body = %s", w.Code, w.Body.String())
	}
}

func postSession(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleSession(w, req)
	return w
}

func rosterBySession(t *testing.T, db store.Store, sessionID, agent string) store.RosterEntry {
	t.Helper()
	id, err := db.ResolveRosterID(context.Background(), sessionID, agent)
	if err != nil {
		t.Fatalf("ResolveRosterID(%q,%q): %v", sessionID, agent, err)
	}
	e, err := db.GetRosterEntry(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRosterEntry: %v", err)
	}
	return e
}

func TestHandleSession_SubagentHealSequence(t *testing.T) {
	s, db := freshSessionDB(t)
	ctx := context.Background()
	lead := `{"session_id":"root-1","host":"hub-1","username":"u","runtime":"codex"}`
	sub := `{"session_id":"root-1","agent_name":"@Avicenna","host":"hub-1","username":"u","runtime":"codex","relationship":"subagent","agent_id":"thr-9"}`

	if w := postSession(t, s, sub); w.Code != http.StatusOK {
		t.Fatalf("subagent first: %d %s", w.Code, w.Body.String())
	}
	e := rosterBySession(t, db, "root-1", "@Avicenna")
	if e.Relationship != "subagent" {
		t.Errorf("relationship = %q, want subagent", e.Relationship)
	}
	if e.ParentRef != nil && *e.ParentRef != "" {
		t.Errorf("parent_ref = %q before lead exists, want empty", *e.ParentRef)
	}

	if w := postSession(t, s, lead); w.Code != http.StatusOK {
		t.Fatalf("lead: %d %s", w.Code, w.Body.String())
	}
	p := rosterBySession(t, db, "root-1", "")
	p.TeamName = "crew"
	if err := db.UpsertRosterEntry(ctx, p); err != nil {
		t.Fatalf("set team: %v", err)
	}

	for i := 0; i < 2; i++ {
		if w := postSession(t, s, sub); w.Code != http.StatusOK {
			t.Fatalf("subagent re-post %d: %d %s", i, w.Code, w.Body.String())
		}
		e = rosterBySession(t, db, "root-1", "@Avicenna")
		if e.ParentRef == nil || *e.ParentRef != p.RosterID {
			t.Errorf("re-post %d: parent_ref = %v, want %q", i, e.ParentRef, p.RosterID)
		}
		if e.TeamName != "crew" {
			t.Errorf("re-post %d: team_name = %q, want crew", i, e.TeamName)
		}
	}
}

func TestHandleSession_RosterIdentity(t *testing.T) {
	lead := `{"session_id":"root-1","host":"hub-1","username":"u","runtime":"codex"}`
	tests := []struct {
		name        string
		seedParent  bool
		body        string
		agent       string
		wantRel     string
		wantParent  bool
		wantTeam    string
		wantAgentID string
	}{
		{"subagent with parent", true,
			`{"session_id":"root-1","agent_name":"@Avicenna","host":"hub-1","username":"u","runtime":"codex","relationship":"subagent","agent_id":"thr-9","future_field":42}`,
			"@Avicenna", "subagent", true, "crew", "thr-9"},
		{"subagent without parent", false,
			`{"session_id":"root-1","agent_name":"@Avicenna","host":"hub-1","username":"u","runtime":"codex","relationship":"subagent","agent_id":"thr-9"}`,
			"@Avicenna", "subagent", false, "", "thr-9"},
		{"lead unchanged", false, lead, "", "lead", false, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, db := freshSessionDB(t)
			ctx := context.Background()
			if tc.seedParent {
				if w := postSession(t, s, lead); w.Code != http.StatusOK {
					t.Fatalf("seed lead: %d %s", w.Code, w.Body.String())
				}
				p := rosterBySession(t, db, "root-1", "")
				p.TeamName = "crew"
				if err := db.UpsertRosterEntry(ctx, p); err != nil {
					t.Fatalf("set team: %v", err)
				}
			}
			if w := postSession(t, s, tc.body); w.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
			}
			e := rosterBySession(t, db, "root-1", tc.agent)
			if e.Relationship != tc.wantRel {
				t.Errorf("relationship = %q, want %q", e.Relationship, tc.wantRel)
			}
			if tc.wantParent {
				parent := rosterBySession(t, db, "root-1", "")
				if e.ParentRef == nil || *e.ParentRef != parent.RosterID {
					t.Errorf("parent_ref = %v, want %q", e.ParentRef, parent.RosterID)
				}
			} else if e.ParentRef != nil && *e.ParentRef != "" {
				t.Errorf("parent_ref = %q, want empty", *e.ParentRef)
			}
			if e.TeamName != tc.wantTeam {
				t.Errorf("team_name = %q, want %q", e.TeamName, tc.wantTeam)
			}
			var agentID string
			storetest.QueryRow(t, ctx, db,
				`SELECT COALESCE(agent_id,'') FROM agent_roster WHERE roster_id = ?`, []any{e.RosterID}, &agentID)
			if agentID != tc.wantAgentID {
				t.Errorf("agent_id = %q, want %q", agentID, tc.wantAgentID)
			}
		})
	}
}
