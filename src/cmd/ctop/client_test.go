package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// wmsHandler builds an httptest handler that always returns result (or, if
// rpcErr != "", a JSON-RPC error envelope instead) for any /mcp/wms POST —
// mirroring internal/server.writeRPCResponse/writeRPCError's wire shape.
func wmsHandler(t *testing.T, wantTool string, resultText string, rpcErr string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req["method"] != "tools/call" {
			t.Errorf("method = %v, want tools/call", req["method"])
		}
		params, _ := req["params"].(map[string]interface{})
		if wantTool != "" && params["name"] != wantTool {
			t.Errorf("tool name = %v, want %v", params["name"], wantTool)
		}

		w.Header().Set("Content-Type", "application/json")
		if rpcErr != "" {
			json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
				"jsonrpc": "2.0", "id": 1,
				"error": map[string]interface{}{"code": -32000, "message": rpcErr},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]interface{}{
				"content": []map[string]interface{}{{"type": "text", "text": resultText}},
			},
		})
	}
}

func TestWMSCallParsesTextContent(t *testing.T) {
	srv := httptest.NewServer(wmsHandler(t, "wms_listOutcomes", `[{"id":"o1","title":"Outcome 1","status":"active"}]`, ""))
	defer srv.Close()

	c := &hubClient{base: srv.URL, http: srv.Client()}
	raw, err := c.WMSCall(context.Background(), "wms_listOutcomes", map[string]interface{}{"status": "open"})
	if err != nil {
		t.Fatalf("WMSCall() error = %v", err)
	}
	var outcomes []Outcome
	if err := json.Unmarshal(raw, &outcomes); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].ID != "o1" {
		t.Errorf("outcomes = %+v, want one outcome with id o1", outcomes)
	}
}

func TestWMSCallPropagatesRPCError(t *testing.T) {
	srv := httptest.NewServer(wmsHandler(t, "", "", "boom"))
	defer srv.Close()

	c := &hubClient{base: srv.URL, http: srv.Client()}
	_, err := c.WMSCall(context.Background(), "wms_listOutcomes", nil)
	if err == nil {
		t.Fatal("WMSCall() expected an error from the RPC error envelope")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want it to mention the RPC error message", err)
	}
}

// wmsMultiHandler routes each JSON-RPC call to a canned response by tool
// name — needed once a single client call (ListOutcomes) fans out to more
// than one MCP tool (wms_listOutcomes, then wms_getEntityTags per outcome).
func wmsMultiHandler(t *testing.T, responses map[string]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		params, _ := req["params"].(map[string]interface{})
		name, _ := params["name"].(string)
		resultText, ok := responses[name]
		if !ok {
			t.Fatalf("unexpected tool call %q", name)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]interface{}{
				"content": []map[string]interface{}{{"type": "text", "text": resultText}},
			},
		})
	}
}

func TestListOutcomesUnmarshalsArray(t *testing.T) {
	srv := httptest.NewServer(wmsMultiHandler(t, map[string]string{
		"wms_listOutcomes":  `[{"id":"o1","title":"A","status":"active","focus":"doing things"}]`,
		"wms_getEntityTags": `[]`,
	}))
	defer srv.Close()

	c := &hubClient{base: srv.URL, http: srv.Client()}
	outcomes, err := c.ListOutcomes(context.Background(), "open")
	if err != nil {
		t.Fatalf("ListOutcomes() error = %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].Title != "A" || outcomes[0].Focus != "doing things" {
		t.Errorf("ListOutcomes() = %+v", outcomes)
	}
}

func TestListOutcomesJoinsTeamTag(t *testing.T) {
	srv := httptest.NewServer(wmsMultiHandler(t, map[string]string{
		"wms_listOutcomes":  `[{"id":"o1","title":"A","status":"active"}]`,
		"wms_getEntityTags": `[{"tag_key":"team","tag_value":"demo-squad","category":"context","source":"manual"}]`,
	}))
	defer srv.Close()

	c := &hubClient{base: srv.URL, http: srv.Client()}
	outcomes, err := c.ListOutcomes(context.Background(), "open")
	if err != nil {
		t.Fatalf("ListOutcomes() error = %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].Tags["team"] != "demo-squad" {
		t.Errorf("ListOutcomes() = %+v, want Tags[team]=demo-squad", outcomes)
	}
}

func TestListWorkUnitsUnmarshalsArray(t *testing.T) {
	srv := httptest.NewServer(wmsHandler(t, "wms_listWorkUnits", `[{"id":"wu1","outcome_id":"o1","title":"Do X","status":"active","agent_id":"store"}]`, ""))
	defer srv.Close()

	c := &hubClient{base: srv.URL, http: srv.Client()}
	units, err := c.ListWorkUnits(context.Background(), "o1")
	if err != nil {
		t.Fatalf("ListWorkUnits() error = %v", err)
	}
	if len(units) != 1 || units[0].AgentID != "store" {
		t.Errorf("ListWorkUnits() = %+v", units)
	}
}

func TestListOutcomesHandlesNullResponse(t *testing.T) {
	srv := httptest.NewServer(wmsMultiHandler(t, map[string]string{
		"wms_listOutcomes": "null",
	}))
	defer srv.Close()

	c := &hubClient{base: srv.URL, http: srv.Client()}
	outcomes, err := c.ListOutcomes(context.Background(), "open")
	if err != nil {
		t.Fatalf("ListOutcomes() error = %v", err)
	}
	if len(outcomes) != 0 {
		t.Errorf("ListOutcomes() with a null result = %v, want empty", outcomes)
	}
}
