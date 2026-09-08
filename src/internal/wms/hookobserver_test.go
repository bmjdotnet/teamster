package wms

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHookObserverSessionIDKey(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
	}))
	defer srv.Close()

	h := &HookObserver{serverURL: srv.URL, sessionID: "test-sid", host: "testhost"}
	h.OnStatusChange(StatusChange{
		EntityType: "task",
		EntityID:   "t1",
		OldStatus:  "active",
		NewStatus:  "complete",
	})

	if _, bad := got["_session_id"]; bad {
		t.Error("emitted _session_id (underscore-prefixed); want session_id")
	}
	if v, ok := got["session_id"]; !ok || v != "test-sid" {
		t.Errorf("session_id = %v, want test-sid", v)
	}
}

func TestHookObserverPrefersChangeSessionID(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
	}))
	defer srv.Close()

	h := &HookObserver{serverURL: srv.URL, sessionID: "stale-cached", host: "testhost"}
	h.OnStatusChange(StatusChange{
		EntityType: "workunit",
		EntityID:   "wu-1",
		OldStatus:  "active",
		NewStatus:  "done",
		SessionID:  "actual-session-abc123",
	})

	if v, ok := got["session_id"]; !ok || v != "actual-session-abc123" {
		t.Errorf("session_id = %v, want actual-session-abc123 (should prefer change.SessionID over cached)", v)
	}
}

// TestPostStatusChangeReportsSuccess is LF-VER-1's positive case: a 2xx
// response must report true, not just "didn't panic" — the whole point of
// this additive method over the pre-existing OnStatusChange is that a
// caller (the review sweep) can now tell success from failure at all.
func TestPostStatusChangeReportsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := &HookObserver{serverURL: srv.URL, sessionID: "test-sid", host: "testhost"}
	ok := h.PostStatusChange(StatusChange{
		EntityType: "workunit", EntityID: "wu-1",
		OldStatus: "on_hold", NewStatus: "abandoned",
	})
	if !ok {
		t.Error("PostStatusChange returned false on a 2xx response, want true")
	}
}

// TestPostStatusChangeReportsNon2xxFailure is LF-VER-1's namesake defect:
// previously post() returned nothing on a non-2xx response, so a run-level
// notified/failed count (WP3-DESIGN.md §5, §8) had no signal to count from.
func TestPostStatusChangeReportsNon2xxFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := &HookObserver{serverURL: srv.URL, sessionID: "test-sid", host: "testhost"}
	ok := h.PostStatusChange(StatusChange{
		EntityType: "workunit", EntityID: "wu-1",
		OldStatus: "review", NewStatus: "on_hold",
	})
	if ok {
		t.Error("PostStatusChange returned true on a 500 response, want false")
	}
}

// TestPostStatusChangeReportsRequestFailure covers the other silent path
// LF-VER-1 named: no listener at all (connection refused), not just a bad
// status code.
func TestPostStatusChangeReportsRequestFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	unreachable := srv.URL
	srv.Close() // closed before use: guarantees connection refused, not a race with a live listener

	h := &HookObserver{serverURL: unreachable, sessionID: "test-sid", host: "testhost"}
	ok := h.PostStatusChange(StatusChange{
		EntityType: "outcome", EntityID: "oc-1",
		OldStatus: "on_hold", NewStatus: "abandoned",
	})
	if ok {
		t.Error("PostStatusChange returned true against an unreachable server, want false")
	}
}

// TestPostFailureIsLogged is the "log non-2xx" half of LF-VER-1 (the return
// value is the other half, covered above) — a non-2xx response must not go
// by silently even for OnStatusChange, whose signature has no return value
// for a caller to check.
func TestPostFailureIsLogged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	h := &HookObserver{serverURL: srv.URL, sessionID: "test-sid", host: "testhost"}
	h.OnStatusChange(StatusChange{
		EntityType: "workunit", EntityID: "wu-1",
		OldStatus: "review", NewStatus: "on_hold",
	})

	if !strings.Contains(buf.String(), "non-2xx") {
		t.Errorf("expected a logged warning mentioning non-2xx, got: %s", buf.String())
	}
}
