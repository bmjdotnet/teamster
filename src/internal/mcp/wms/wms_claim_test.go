package wms

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// These tests exercise wms_claimWorkUnit's MCP handler error translation
// (wms.go ToolClaimWorkUnit case, ~line 891-946) through HandleToolCall
// against a real store, reusing newStewardStore/resultText from
// wms_steward_test.go. The store-layer claim-state matrix itself is covered
// by conformance_dim4_test.go — these tests only exercise the handler's
// error→CallError translation and response shape. SKIP when
// TEAMSTER_TEST_MYSQL_DSN is unset.

// callAsAgent invokes HandleToolCall with _meta.agent_type set, so the claim
// handler's agentID (p.Meta.AgentType) is under test control.
func callAsAgent(t *testing.T, store wms.Store, name, agentType string, args map[string]interface{}) (Result, *CallError) {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{
		"name":      name,
		"arguments": args,
		"_meta":     map[string]interface{}{"agent_type": agentType},
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return HandleToolCall(store, noopEngine{}, raw)
}

// claimWorkUnit calls wms_claimWorkUnit as agentType and decodes the response.
func claimWorkUnit(t *testing.T, store wms.Store, id, agentType string) (map[string]interface{}, *CallError) {
	t.Helper()
	r, ce := callAsAgent(t, store, ToolClaimWorkUnit, agentType, map[string]interface{}{"id": id})
	if ce != nil {
		return nil, ce
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(resultText(t, r)), &out); err != nil {
		t.Fatalf("decode claimWorkUnit result: %v", err)
	}
	return out, nil
}

// TestClaimWorkUnit_HappyPath: claiming a pending work unit succeeds, returns
// the brief/tags/claimed_by in the response, and the store reflects the
// pending->active transition with the claiming agent as owner.
func TestClaimWorkUnit_HappyPath(t *testing.T) {
	store, oid := newStewardStore(t)
	const wuID = "wu-claim-happy"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{
		"id": wuID, "title": "claim me", "outcomeID": oid, "brief": "do the thing",
	}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}

	resp, ce := claimWorkUnit(t, store, wuID, "@agent-a")
	if ce != nil {
		t.Fatalf("claimWorkUnit: %v", ce)
	}
	if resp["id"] != wuID {
		t.Errorf("response id = %v, want %q", resp["id"], wuID)
	}
	if resp["claimed_by"] != "@agent-a" {
		t.Errorf("response claimed_by = %v, want @agent-a", resp["claimed_by"])
	}
	if resp["brief"] != "do the thing" {
		t.Errorf("response brief = %v, want %q", resp["brief"], "do the thing")
	}
	// LF-VER-2/HOOKD-DRAIN.md §4: "opened" asserted a fact wms-mcp cannot
	// know synchronously (hookd's open is async, off a separate
	// WMSStatusChange POST, and may be declined). "requested" is what this
	// call actually did — dispatched the status-change event that triggers
	// hookd's attempt — without claiming hookd's result.
	if resp["focus_interval"] != "requested" {
		t.Errorf("response focus_interval = %v, want requested", resp["focus_interval"])
	}
	if _, ok := resp["claimed_at"]; !ok {
		t.Errorf("response missing claimed_at: %+v", resp)
	}

	wu, err := store.GetWorkUnit(t.Context(), wuID)
	if err != nil {
		t.Fatalf("GetWorkUnit: %v", err)
	}
	if wu.Status != wms.StatusActive {
		t.Errorf("status after claim = %q, want %q", wu.Status, wms.StatusActive)
	}
	if wu.AgentID != "@agent-a" {
		t.Errorf("AgentID after claim = %q, want @agent-a", wu.AgentID)
	}
}

// TestClaimWorkUnit_AlreadyClaimedByOther: claiming a work unit already
// active under a different agent returns an actionable error naming the WU
// id and the current owner, and does not change ownership.
func TestClaimWorkUnit_AlreadyClaimedByOther(t *testing.T) {
	store, oid := newStewardStore(t)
	const wuID = "wu-claim-owned"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{
		"id": wuID, "title": "owned already", "outcomeID": oid,
	}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := claimWorkUnit(t, store, wuID, "@agent-a"); ce != nil {
		t.Fatalf("first claim by @agent-a: %v", ce)
	}

	_, ce := claimWorkUnit(t, store, wuID, "@agent-b")
	if ce == nil {
		t.Fatal("claim by @agent-b on a WU owned by @agent-a returned no error")
	}
	if !strings.Contains(ce.Message, wuID) {
		t.Errorf("error %q does not name the work unit", ce.Message)
	}
	if !strings.Contains(ce.Message, "@agent-a") {
		t.Errorf("error %q does not name the current owner", ce.Message)
	}

	wu, err := store.GetWorkUnit(t.Context(), wuID)
	if err != nil {
		t.Fatalf("GetWorkUnit: %v", err)
	}
	if wu.AgentID != "@agent-a" {
		t.Errorf("AgentID after failed claim = %q, want unchanged @agent-a", wu.AgentID)
	}
}

// TestClaimWorkUnit_NotClaimable: claiming a work unit in a terminal status
// (review/done/blocked) returns an error naming that status, for every such
// status.
func TestClaimWorkUnit_NotClaimable(t *testing.T) {
	store, oid := newStewardStore(t)

	for _, status := range []string{wms.StatusReview, wms.StatusDone, wms.StatusBlocked} {
		wuID := "wu-claim-notclaimable-" + status
		if err := store.CreateWorkUnit(t.Context(), &wms.WorkUnit{
			ID: wuID, OutcomeID: oid, Title: "not claimable", Status: status,
		}); err != nil {
			t.Fatalf("seed CreateWorkUnit(%s): %v", status, err)
		}

		_, ce := claimWorkUnit(t, store, wuID, "@agent-a")
		if ce == nil {
			t.Fatalf("claim on status=%s returned no error", status)
		}
		if !strings.Contains(ce.Message, wuID) {
			t.Errorf("error %q does not name the work unit", ce.Message)
		}
		if !strings.Contains(ce.Message, status) {
			t.Errorf("error %q does not name status %q", ce.Message, status)
		}
	}
}

// TestClaimWorkUnit_NotFound: claiming an unknown work unit id returns a
// clean error naming it, not a raw store error.
func TestClaimWorkUnit_NotFound(t *testing.T) {
	store, _ := newStewardStore(t)

	_, ce := claimWorkUnit(t, store, "wu-does-not-exist", "@agent-a")
	if ce == nil {
		t.Fatal("claim on an unknown work unit returned no error")
	}
	if !strings.Contains(ce.Message, "wu-does-not-exist") {
		t.Errorf("error %q does not name the missing work unit", ce.Message)
	}
}

// TestClaimWorkUnit_IdempotentReclaim: a second claim by the SAME agent that
// already owns the (active) work unit succeeds rather than erroring, and
// leaves ownership unchanged.
func TestClaimWorkUnit_IdempotentReclaim(t *testing.T) {
	store, oid := newStewardStore(t)
	const wuID = "wu-claim-idempotent"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{
		"id": wuID, "title": "reclaim me", "outcomeID": oid,
	}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := claimWorkUnit(t, store, wuID, "@agent-a"); ce != nil {
		t.Fatalf("first claim: %v", ce)
	}

	resp, ce := claimWorkUnit(t, store, wuID, "@agent-a")
	if ce != nil {
		t.Fatalf("idempotent re-claim by same agent: %v", ce)
	}
	if resp["claimed_by"] != "@agent-a" {
		t.Errorf("response claimed_by = %v, want @agent-a", resp["claimed_by"])
	}
	// No status change on an idempotent re-claim, so no WMSStatusChange event
	// reaches hookd, so nothing was requested — distinct from the real-claim
	// "requested" case above.
	if resp["focus_interval"] != "unchanged" {
		t.Errorf("response focus_interval = %v, want unchanged (no new request on an idempotent re-claim)", resp["focus_interval"])
	}

	wu, err := store.GetWorkUnit(t.Context(), wuID)
	if err != nil {
		t.Fatalf("GetWorkUnit: %v", err)
	}
	if wu.Status != wms.StatusActive || wu.AgentID != "@agent-a" {
		t.Errorf("state after idempotent re-claim = status=%q agent=%q, want active/@agent-a", wu.Status, wu.AgentID)
	}
}

func timelineStates(t *testing.T, store wms.Store, eng wms.Engine, wuID string) []wms.EventRecord {
	t.Helper()
	r, ce := callWithEngine(t, store, eng, ToolGetTimeline, map[string]interface{}{"entityType": wms.EntityWorkUnit, "entityID": wuID})
	if ce != nil {
		t.Fatalf("getTimeline: %v", ce)
	}
	var recs []wms.EventRecord
	if err := json.Unmarshal([]byte(resultText(t, r)), &recs); err != nil {
		t.Fatalf("decode timeline: %v", err)
	}
	// ListEventRecords returns newest first; reverse to chronological order.
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	return recs
}

// TestClaimWorkUnit_TimelineHasActiveInterval: a claim must close the
// pending interval and open an active one, so duration reporting does not
// count active work as pending time (#33).
func TestClaimWorkUnit_TimelineHasActiveInterval(t *testing.T) {
	store, oid := newStewardStore(t)
	eng := realEngineFor(store)
	const wuID = "wu-claim-timeline"
	if _, ce := callWithEngine(t, store, eng, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := callWithEngine(t, store, eng, ToolClaimWorkUnit, map[string]interface{}{"id": wuID}); ce != nil {
		t.Fatalf("claimWorkUnit: %v", ce)
	}

	recs := timelineStates(t, store, eng, wuID)
	if len(recs) != 2 || recs[0].State != wms.StatusPending || recs[1].State != wms.StatusActive {
		t.Fatalf("timeline after claim = %+v, want pending then active", recs)
	}
	if recs[0].EndedAt == nil {
		t.Error("pending interval not closed after claim")
	}
	if recs[1].EndedAt != nil {
		t.Error("active interval should be open after claim")
	}

	if _, ce := callWithEngine(t, store, eng, ToolDeliverResult, map[string]interface{}{"id": wuID, "summary": "s", "result": "r"}); ce != nil {
		t.Fatalf("deliverResult: %v", ce)
	}
	if _, ce := callWithEngine(t, store, eng, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": wuID, "status": wms.StatusDone}); ce != nil {
		t.Fatalf("complete: %v", ce)
	}
	recs = timelineStates(t, store, eng, wuID)
	want := []string{wms.StatusPending, wms.StatusActive, wms.StatusReview, wms.StatusDone}
	if len(recs) != len(want) {
		t.Fatalf("full timeline = %+v, want states %v", recs, want)
	}
	for i, w := range want {
		if recs[i].State != w {
			t.Errorf("timeline[%d].State = %q, want %q", i, recs[i].State, w)
		}
	}
}
