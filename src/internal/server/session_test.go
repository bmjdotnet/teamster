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

	body := `{"session_id":"sess-1","agent_name":"@worker","host":"hub-1","username":"bmj",` +
		`"runtime":"codex","cwd":"/mnt/ai/gh","model":"gpt-5-codex","originator":"codex_exec","cli_version":"0.142.5"}`
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
	if sess.Host != "hub-1" || sess.Username != "bmj" || sess.Runtime != "codex" ||
		sess.Cwd != "/mnt/ai/gh" || sess.Model != "gpt-5-codex" ||
		sess.Originator != "codex_exec" || sess.CliVersion != "0.142.5" {
		t.Fatalf("session row = %+v, fields did not round-trip", sess)
	}
}

// TestHandleSession_Idempotent proves a repeated identical POST (the remote/
// hub-local timer re-posts every run) is a no-op that never errors and never
// creates a second row for the same (session_id, agent_name) key.
func TestHandleSession_Idempotent(t *testing.T) {
	s, db := freshSessionDB(t)
	body := `{"session_id":"sess-2","host":"hub-1","username":"bmj","runtime":"codex"}`

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
