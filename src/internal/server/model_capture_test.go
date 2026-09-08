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

// This test used to assert that a Stop CLOSED the session and that the
// close's own UpsertSession carried Model, because UpsertSession's UPSERT
// does "model = VALUES(model)" unconditionally (every column, every call —
// see internal/store/mysql/store.go), so a close that omitted Model would
// blindly wipe the model captured at registration.
//
// Stop no longer writes anything durable: it is a turn boundary, not a
// session boundary, so both halves of the old assertion are gone. The
// surviving obligation is the interesting one, and it is now stronger —
// a Stop must leave the session row completely alone. Sessions are closed
// by the reaper's phase 3 (MarkStaleSessionsClosed), which touches only
// the status column and so cannot clobber Model at all.
//
// Kept rather than deleted because the clobber hazard is real for every
// OTHER UpsertSession caller: if anyone reintroduces a Stop-time session
// write, this fails whether or not they remember to carry Model.
func TestDispatchObservabilityStopLeavesSessionRowAlone(t *testing.T) {
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

	// Negative assertion — there is no positive signal to poll for, so give
	// any detached goroutine time to have done the wrong thing.
	time.Sleep(500 * time.Millisecond)

	sess, err := s.obsStore.GetSession(context.Background(), store.SessionKey{SessionID: "sess-3"})
	if err != nil {
		t.Fatalf("GetSession after Stop: %v", err)
	}
	if sess.Status == store.SessionStatusClosed {
		t.Error("Stop closed the session; Stop is a turn boundary and must not write session status")
	}
	if sess.Model != "claude-opus-4-6" {
		t.Errorf("sess.Model after Stop = %q, want %q (must survive untouched)", sess.Model, "claude-opus-4-6")
	}
}
