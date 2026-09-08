package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/hook"
	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
	"github.com/prometheus/client_golang/prometheus"
)

// WP9 (server.go:981): the focus-interval close on a terminal status must
// fire for `abandoned` as well as `done` — an entity abandoned via the live
// MCP path leaks an open focus interval otherwise, the same class of bug
// `out-drainpipe` fixed once before. Switched to wms.IsTerminal so this stays
// correct regardless of what else the engine ever adds to the terminal set.

// spyCloseStore records every CloseFocusIntervalForEntity call so the test
// can assert both the positive (fires on abandoned) and negative
// (does NOT fire on a non-terminal status) cases.
type spyCloseStore struct {
	store.Store
	mu    sync.Mutex
	calls []spyCloseCall
}

type spyCloseCall struct {
	entityType string
	entityID   string
}

func (f *spyCloseStore) CloseFocusIntervalForEntity(_ context.Context, _ store.SessionKey, entityType, entityID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, spyCloseCall{entityType: entityType, entityID: entityID})
	return nil
}

func (f *spyCloseStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newIntervalCloseTestServer(t *testing.T, obsStore store.Store) *Server {
	t.Helper()
	s := &Server{
		cfg:      config.Config{Host: "testhost"},
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker("testhost", time.Minute, time.Minute, nil),
		obsStore: obsStore,
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)
	return s
}

func waitForCloseCallCount(t *testing.T, spy *spyCloseStore, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if spy.callCount() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("CloseFocusIntervalForEntity called %d time(s), want %d within deadline", spy.callCount(), want)
}

func TestCloseFocusInterval_FiresOnAbandoned(t *testing.T) {
	spy := &spyCloseStore{}
	s := newIntervalCloseTestServer(t, spy)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityWorkUnit,
		"wms_entity_id":   "wu-abandoned",
		"wms_old_status":  "active",
		"wms_new_status":  wms.StatusAbandoned,
		"wms_session_id":  "s1",
		"wms_agent_name":  "store",
	})

	waitForCloseCallCount(t, spy, 1)
	if got := spy.calls[0].entityID; got != "wu-abandoned" {
		t.Errorf("entity_id = %q, want %q", got, "wu-abandoned")
	}
}

func TestCloseFocusInterval_FiresOnDone(t *testing.T) {
	spy := &spyCloseStore{}
	s := newIntervalCloseTestServer(t, spy)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityOutcome,
		"wms_entity_id":   "out-done",
		"wms_old_status":  "active",
		"wms_new_status":  wms.StatusDone,
		"wms_session_id":  "s1",
		"wms_agent_name":  "store",
	})

	waitForCloseCallCount(t, spy, 1)
	if got := spy.calls[0].entityID; got != "out-done" {
		t.Errorf("entity_id = %q, want %q", got, "out-done")
	}
}

// Negative case: a non-terminal transition (e.g. active -> review) must not
// close the focus interval. Race-inherent (proving a non-event never
// happens), so this asserts zero calls after a fixed wait rather than
// polling for a positive signal.
func TestCloseFocusInterval_DoesNotFireOnNonTerminal(t *testing.T) {
	spy := &spyCloseStore{}
	s := newIntervalCloseTestServer(t, spy)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityWorkUnit,
		"wms_entity_id":   "wu-review",
		"wms_old_status":  "active",
		"wms_new_status":  wms.StatusReview,
		"wms_session_id":  "s1",
		"wms_agent_name":  "store",
	})

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if spy.callCount() > 0 {
			t.Fatalf("CloseFocusIntervalForEntity called on non-terminal status %q", wms.StatusReview)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Negative case, entity-type axis: wms.IsTerminal's switch is the sole
// remaining guard against closing an interval for an entity type this path
// was never meant to touch (server.go's own check used to spell out
// EntityOutcome/EntityWorkUnit explicitly; that restriction now lives
// entirely in IsTerminal's default case, in wms's file, not this one). If a
// future entity type is ever added to WMSStatusChange without a matching
// IsTerminal case, this must still not fire — pin it here so a silent
// widening of this write path shows up as a failure in this package, not
// just as a missing assertion in wms's.
func TestCloseFocusInterval_DoesNotFireOnUnknownEntityType(t *testing.T) {
	spy := &spyCloseStore{}
	s := newIntervalCloseTestServer(t, spy)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": "project",
		"wms_entity_id":   "proj-1",
		"wms_old_status":  "active",
		"wms_new_status":  wms.StatusDone,
		"wms_session_id":  "s1",
		"wms_agent_name":  "store",
	})

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if spy.callCount() > 0 {
			t.Fatalf("CloseFocusIntervalForEntity called for entity_type %q, which IsTerminal does not cover", "project")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
