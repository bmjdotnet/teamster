package server

import (
	"context"
	"testing"

	"github.com/bmjdotnet/teamster/internal/hook"
	"github.com/bmjdotnet/teamster/internal/store"
)

func TestHostFromData(t *testing.T) {
	if got := hostFromData(map[string]interface{}{"_host": "remote-box-1"}, "hubhost"); got != "remote-box-1" {
		t.Errorf("hostFromData with _host set = %q, want %q", got, "remote-box-1")
	}
	if got := hostFromData(map[string]interface{}{}, "hubhost"); got != "hubhost" {
		t.Errorf("hostFromData with no _host = %q, want fallback %q", got, "hubhost")
	}
	if got := hostFromData(nil, "hubhost"); got != "hubhost" {
		t.Errorf("hostFromData(nil) = %q, want fallback %q", got, "hubhost")
	}
	if got := hostFromData(map[string]interface{}{"_host": 42}, "hubhost"); got != "hubhost" {
		t.Errorf("hostFromData with non-string _host = %q, want fallback (type assertion fails safely)", got)
	}
}

// TestDispatchObservabilityCapturesRemoteHost proves a PreToolUse event
// carrying data["_host"] (the hook client's enrichment field, populated on
// remote installs from TEAMSTER_HOST) attributes the session row to the
// remote host, not the hub's own s.cfg.Host.
func TestDispatchObservabilityCapturesRemoteHost(t *testing.T) {
	s := newModelCaptureTestServer(t)

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "UserPromptSubmit",
		SessionID:     "sess-remote-1",
	}, map[string]interface{}{"_host": "remote-box-1"})

	waitForSessionRow(t, s.obsStore, "sess-remote-1")
	sess, err := s.obsStore.GetSession(context.Background(), store.SessionKey{SessionID: "sess-remote-1"})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Host != "remote-box-1" {
		t.Errorf("sess.Host = %q, want %q — must attribute to the event's origin host, not the hub's configured host", sess.Host, "remote-box-1")
	}
}

// TestDispatchObservabilitySubagentStartCapturesRemoteHost covers the
// registerSubagentStart / registerNewSubagentInstance paths: a SubagentStart
// event carrying data["_host"] must roster + session the subagent under the
// remote host, matching the lead's own session on the same box.
func TestDispatchObservabilitySubagentStartCapturesRemoteHost(t *testing.T) {
	s := newModelCaptureTestServer(t)

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "SubagentStart",
		SessionID:     "sess-remote-2",
		AgentType:     "Explore",
		AgentID:       "aexplore-remote-1",
	}, map[string]interface{}{"_host": "remote-box-2"})

	entry := waitForRosterEntry(t, s.obsStore, "sess-remote-2", "@Explore")
	if entry.Host != "remote-box-2" {
		t.Errorf("roster entry Host = %q, want %q", entry.Host, "remote-box-2")
	}

	sess, err := s.obsStore.GetSession(context.Background(), store.SessionKey{SessionID: "sess-remote-2", AgentName: "@Explore"})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Host != "remote-box-2" {
		t.Errorf("sess.Host = %q, want %q", sess.Host, "remote-box-2")
	}
}

// TestDispatchObservabilityNoHostFallsBackToConfiguredHost is the regression
// this fix must not introduce: an event with no _host field (hub-local
// clients, or older remotes not yet sending it) must still fall back to
// s.cfg.Host, not leave the column empty.
func TestDispatchObservabilityNoHostFallsBackToConfiguredHost(t *testing.T) {
	s := newModelCaptureTestServer(t)

	s.dispatchObservability(hook.HookEvent{
		HookEventName: "UserPromptSubmit",
		SessionID:     "sess-nohost",
	}, map[string]interface{}{})

	waitForSessionRow(t, s.obsStore, "sess-nohost")
	sess, err := s.obsStore.GetSession(context.Background(), store.SessionKey{SessionID: "sess-nohost"})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Host != "testhost" {
		t.Errorf("sess.Host = %q, want fallback %q (cfg.Host)", sess.Host, "testhost")
	}
}
