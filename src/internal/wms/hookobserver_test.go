package wms

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
