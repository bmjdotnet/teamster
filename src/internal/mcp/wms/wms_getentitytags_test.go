package wms

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// These tests exercise wms_getEntityTags end-to-end through HandleToolCall
// against a real store, reusing newStewardStore/call/resultText from
// wms_steward_test.go. They SKIP when TEAMSTER_TEST_MYSQL_DSN is unset.

// getEntityTags calls the tool and decodes the response.
func getEntityTags(t *testing.T, store wms.Store, entityType, entityID string) ([]EntityTagView, *CallError) {
	t.Helper()
	r, ce := call(t, store, ToolGetEntityTags, map[string]interface{}{
		"entityType": entityType, "entityID": entityID,
	})
	if ce != nil {
		return nil, ce
	}
	var out []EntityTagView
	if err := json.Unmarshal([]byte(resultText(t, r)), &out); err != nil {
		t.Fatalf("decode getEntityTags result: %v", err)
	}
	return out, nil
}

// findTagView returns the row for (key,value) in got, or nil.
func findTagView(got []EntityTagView, key, value string) *EntityTagView {
	for i := range got {
		if got[i].TagKey == key && got[i].TagValue == value {
			return &got[i]
		}
	}
	return nil
}

// TestGetEntityTagsOutcomeDirect: an outcome's own tags come back direct
// (inherited=false, origin=self), and wms_getOutcome's response shape is
// unaffected by this new tool existing.
func TestGetEntityTagsOutcomeDirect(t *testing.T) {
	store, oid := newStewardStore(t)

	if _, ce := call(t, store, ToolTagEntity, map[string]interface{}{
		"entityType": wms.EntityOutcome, "entityID": oid,
		"tagKey": "component", "tagValue": "harness", "description": "test",
	}); ce != nil {
		t.Fatalf("tagEntity: %v", ce)
	}

	got, ce := getEntityTags(t, store, wms.EntityOutcome, oid)
	if ce != nil {
		t.Fatalf("getEntityTags: %v", ce)
	}
	tv := findTagView(got, "component", "harness")
	if tv == nil {
		t.Fatalf("component:harness not in %+v", got)
	}
	if tv.Inherited {
		t.Errorf("outcome's own tag reported inherited=true")
	}
	if tv.Origin != oid {
		t.Errorf("origin = %q, want %q (self)", tv.Origin, oid)
	}
	if tv.Source != "manual" {
		t.Errorf("source = %q, want manual", tv.Source)
	}

	// getOutcome's response shape must be unchanged by this tool's existence.
	r, ce := call(t, store, ToolGetOutcome, map[string]interface{}{"id": oid})
	if ce != nil {
		t.Fatalf("getOutcome: %v", ce)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(resultText(t, r)), &raw); err != nil {
		t.Fatalf("decode getOutcome: %v", err)
	}
	if _, ok := raw["tags"]; ok {
		t.Errorf("getOutcome response now carries a 'tags' field — response shape changed")
	}
}

// TestGetEntityTagsWorkUnitInherits: a workunit with no tag of its own for a
// key surfaces the outcome's tag as inherited=true with origin=outcomeID; a
// workunit's own binding for the same key shadows the outcome's (per-key
// override, matching the mysql entity_tags_resolved view semantics).
func TestGetEntityTagsWorkUnitInherits(t *testing.T) {
	store, oid := newStewardStore(t)

	if _, ce := call(t, store, ToolTagEntity, map[string]interface{}{
		"entityType": wms.EntityOutcome, "entityID": oid,
		"tagKey": "project", "tagValue": "vault", "description": "test",
	}); ce != nil {
		t.Fatalf("tagEntity outcome project: %v", ce)
	}
	if _, ce := call(t, store, ToolTagEntity, map[string]interface{}{
		"entityType": wms.EntityOutcome, "entityID": oid,
		"tagKey": "component", "tagValue": "harness", "description": "test",
	}); ce != nil {
		t.Fatalf("tagEntity outcome component: %v", ce)
	}

	const wuID = "wu-inherit"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{
		"id": wuID, "title": "inherits from outcome", "outcomeID": oid,
	}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	// The workunit sets its own component, overriding the outcome's.
	if _, ce := call(t, store, ToolTagEntity, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": wuID,
		"tagKey": "component", "tagValue": "wms", "description": "test",
	}); ce != nil {
		t.Fatalf("tagEntity workunit component: %v", ce)
	}

	got, ce := getEntityTags(t, store, wms.EntityWorkUnit, wuID)
	if ce != nil {
		t.Fatalf("getEntityTags: %v", ce)
	}

	// project: not set directly on the workunit -> inherited from the outcome.
	proj := findTagView(got, "project", "vault")
	if proj == nil {
		t.Fatalf("project:vault (inherited) missing from %+v", got)
	}
	if !proj.Inherited {
		t.Errorf("project:vault should be inherited=true")
	}
	if proj.Origin != oid {
		t.Errorf("project origin = %q, want outcome %q", proj.Origin, oid)
	}

	// component: the workunit's own direct binding shadows the outcome's.
	comp := findTagView(got, "component", "wms")
	if comp == nil {
		t.Fatalf("component:wms (direct) missing from %+v", got)
	}
	if comp.Inherited {
		t.Errorf("workunit's own component tag reported inherited=true")
	}
	if comp.Origin != wuID {
		t.Errorf("component origin = %q, want self %q", comp.Origin, wuID)
	}
	if shadowed := findTagView(got, "component", "harness"); shadowed != nil {
		t.Errorf("outcome's shadowed component:harness leaked through: %+v", shadowed)
	}

	// getWorkUnit's response shape must be unchanged by this tool's existence.
	r, ce := call(t, store, ToolGetWorkUnit, map[string]interface{}{"id": wuID})
	if ce != nil {
		t.Fatalf("getWorkUnit: %v", ce)
	}
	var wu wms.WorkUnit
	if err := json.Unmarshal([]byte(resultText(t, r)), &wu); err != nil {
		t.Fatalf("decode getWorkUnit: %v", err)
	}
	if wu.ID != wuID {
		t.Errorf("getWorkUnit id = %q, want %q", wu.ID, wuID)
	}
}

// TestGetEntityTagsEmptyAndUnknown: a taglesss entity returns an empty array
// (not an error, not null), and an unknown entity returns a clean error.
func TestGetEntityTagsEmptyAndUnknown(t *testing.T) {
	store, oid := newStewardStore(t)

	const wuID = "wu-notags"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{
		"id": wuID, "title": "no tags anywhere", "outcomeID": oid,
	}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	// createWorkUnit auto-applies runtime:<...> (classifier-sourced); strip it
	// so this is a genuine zero-tags case rather than fighting that feature.
	if _, ce := call(t, store, ToolUntagEntity, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": wuID, "tagKey": "runtime",
	}); ce != nil {
		t.Fatalf("untagEntity runtime: %v", ce)
	}

	got, ce := getEntityTags(t, store, wms.EntityWorkUnit, wuID)
	if ce != nil {
		t.Fatalf("getEntityTags on tagless workunit: %v", ce)
	}
	if len(got) != 0 {
		t.Errorf("tagless workunit returned %d tags, want 0: %+v", len(got), got)
	}
	r, _ := call(t, store, ToolGetEntityTags, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": wuID,
	})
	if txt := resultText(t, r); txt != "[]" {
		t.Errorf("tagless workunit JSON = %q, want literal []", txt)
	}

	if _, ce := getEntityTags(t, store, wms.EntityWorkUnit, "wu-does-not-exist"); ce == nil {
		t.Fatal("getEntityTags on an unknown workunit returned no error")
	}
	if _, ce := getEntityTags(t, store, wms.EntityOutcome, "out-does-not-exist"); ce == nil {
		t.Fatal("getEntityTags on an unknown outcome returned no error")
	}

	if _, ce := getEntityTags(t, store, "bogus", oid); ce == nil {
		t.Fatal("getEntityTags with an invalid entityType returned no error")
	} else if !strings.Contains(ce.Message, "outcome|workunit") {
		t.Errorf("invalid entityType error = %q, want it to name outcome|workunit", ce.Message)
	}
}
