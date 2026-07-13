package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNudgeDeliveryPostsToHookd(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody nudgeRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	d := NewNudgeDelivery(srv.URL + "/nudge")

	alert := Alert{
		SessionID: "sess-1",
		AgentName: "@scout",
		Level:     LevelWarning,
		Message:   "warning context pressure: 78% of context window used",
	}

	if err := d.Deliver(context.Background(), alert); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/nudge" {
		t.Fatalf("path = %s, want /nudge", gotPath)
	}
	if gotBody.SessionID != alert.SessionID {
		t.Fatalf("session_id = %s, want %s", gotBody.SessionID, alert.SessionID)
	}
	if gotBody.AgentName != alert.AgentName {
		t.Fatalf("agent_name = %s, want %s", gotBody.AgentName, alert.AgentName)
	}
	if gotBody.Message != alert.Message {
		t.Fatalf("message = %s, want %s", gotBody.Message, alert.Message)
	}
}

func TestNudgeDeliveryNonOKStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "read-only mode", http.StatusForbidden)
	}))
	defer srv.Close()

	d := NewNudgeDelivery(srv.URL + "/nudge")

	err := d.Deliver(context.Background(), Alert{SessionID: "s1", AgentName: "@a", Message: "m"})
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
}
