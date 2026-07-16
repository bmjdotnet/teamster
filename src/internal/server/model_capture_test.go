package server

import (
	"context"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/hook"
	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/sqlite"
	"github.com/prometheus/client_golang/prometheus"
)

func TestModelFromData(t *testing.T) {
	if got := modelFromData(map[string]interface{}{"_model": "claude-opus-4-6"}); got != "claude-opus-4-6" {
		t.Errorf("modelFromData with _model set = %q, want %q", got, "claude-opus-4-6")
	}
	if got := modelFromData(map[string]interface{}{}); got != "" {
		t.Errorf("modelFromData with no _model = %q, want empty", got)
	}
	if got := modelFromData(nil); got != "" {
		t.Errorf("modelFromData(nil) = %q, want empty", got)
	}
	if got := modelFromData(map[string]interface{}{"_model": 42}); got != "" {
		t.Errorf("modelFromData with non-string _model = %q, want empty (type assertion fails safely)", got)
	}
}

func newModelCaptureTestServer(t *testing.T) *Server {
	t.Helper()
	obsStore, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { obsStore.Close() })

	tracker := observability.NewSessionTracker("testhost", 5*time.Minute, 30*time.Second, nil)
	return &Server{
		cfg:      config.Config{Host: "testhost"},
		sessions: tracker,
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
		obsStore: obsStore,
	}
}

// TestDispatchObservabilityCapturesModelAtRegistration is the structural fix
// this test file is named for: hookd should capture the model at session
// registration time (dispatchObservability's isNew branch) instead of
// leaving sessions.model empty until health-collector's first token_ledger
// pass. data["_model"] is what internal/hook.ProcessEvent's getModel()
// attaches client-side (the operator's configured model from
// ~/.claude/settings.json), present on every hub-local hook event.
func TestDispatchObservabilityCapturesModelAtRegistration(t *testing.T) {
	s := newModelCaptureTestServer(t)

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "UserPromptSubmit",
		SessionID:     "sess-1",
	}, map[string]interface{}{"_model": "claude-opus-4-6"})

	waitForSessionRow(t, s.obsStore, "sess-1")
	sess, err := s.obsStore.GetSession(context.Background(), store.SessionKey{SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Model != "claude-opus-4-6" {
		t.Errorf("sess.Model = %q, want %q — model should be captured at registration, not left empty", sess.Model, "claude-opus-4-6")
	}
}

// TestDispatchObservabilityNoModelStaysEmpty proves a session whose hook
// events never carry _model (no explicit model configured in
// ~/.claude/settings.json, the CLI default is used instead) still registers
// cleanly with an empty model — no regression, no fabricated value.
func TestDispatchObservabilityNoModelStaysEmpty(t *testing.T) {
	s := newModelCaptureTestServer(t)

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "UserPromptSubmit",
		SessionID:     "sess-2",
	}, map[string]interface{}{})

	waitForSessionRow(t, s.obsStore, "sess-2")
	sess, err := s.obsStore.GetSession(context.Background(), store.SessionKey{SessionID: "sess-2"})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Model != "" {
		t.Errorf("sess.Model = %q, want empty (no _model in the event data)", sess.Model)
	}
}

// TestDispatchObservabilityStopPreservesModel is the regression this fix
// must not introduce: UpsertSession's UPSERT does "model = VALUES(model)"
// unconditionally (every column, every call — see internal/store/mysql/
// store.go), so if the Stop/close branch's store.Session literal didn't
// also carry Model, the very next Stop event would blindly wipe out the
// model captured at registration. Since the hook client attaches the same
// settings.json-derived _model on every event (registration, refresh, and
// Stop alike), passing it through on close re-writes the same value rather
// than clobbering it — this proves that in practice.
func TestDispatchObservabilityStopPreservesModel(t *testing.T) {
	s := newModelCaptureTestServer(t)

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "UserPromptSubmit",
		SessionID:     "sess-3",
	}, map[string]interface{}{"_model": "claude-opus-4-6"})
	waitForSessionRow(t, s.obsStore, "sess-3")

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "Stop",
		SessionID:     "sess-3",
	}, map[string]interface{}{"_model": "claude-opus-4-6"})

	deadline := time.Now().Add(2 * time.Second)
	var sess store.Session
	for time.Now().Before(deadline) {
		var err error
		sess, err = s.obsStore.GetSession(context.Background(), store.SessionKey{SessionID: "sess-3"})
		if err == nil && sess.Status == store.SessionStatusClosed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sess.Status != store.SessionStatusClosed {
		t.Fatalf("session never closed within deadline (status = %q)", sess.Status)
	}
	if sess.Model != "claude-opus-4-6" {
		t.Errorf("sess.Model after Stop = %q, want %q (must survive session close, not get wiped)", sess.Model, "claude-opus-4-6")
	}
}
