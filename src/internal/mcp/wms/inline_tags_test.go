package wms

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// Tests for parseInlineTagValues/applyInlineTags (WP2-multivalue-inline-tags):
// a `tags` map entry may be a plain string, a {value, description} object, or
// an array mixing either form — the array case is how a multi-cardinality key
// (e.g. github.issue) carries more than one value in one create call, which a
// single key→value(+description) entry cannot express.
//
// No dedicated test existed for this surface before this file (checked
// identity_test.go, manifest_test.go, wms_claim_test.go,
// wms_getentitytags_test.go, wms_relations_test.go, wms_reparenting_test.go,
// wms_search_test.go, wms_steward_test.go — none reference applyInlineTags,
// tagErrors, or a tags create-time argument), so the pre-existing plain-string
// and object forms are covered here as a baseline alongside the new array
// forms, per WP2 AC2.

// tagValues returns the sorted tag_value list for one key on one entity.
func tagValues(t *testing.T, store wms.Store, entityType, entityID, tagKey string) []string {
	t.Helper()
	tags, err := store.GetEntityTags(context.Background(), entityType, entityID)
	if err != nil {
		t.Fatalf("GetEntityTags(%s/%s): %v", entityType, entityID, err)
	}
	var out []string
	for _, tg := range tags {
		if tg.TagKey == tagKey {
			out = append(out, tg.TagValue)
		}
	}
	sort.Strings(out)
	return out
}

// TestApplyInlineTags_PlainStringForm: baseline (pre-existing form) — a plain
// string value applies one tag with no description.
func TestApplyInlineTags_PlainStringForm(t *testing.T) {
	store, oid := newStewardStore(t)
	errs := applyInlineTags(context.Background(), store, wms.EntityOutcome, oid, map[string]interface{}{
		"component": "harness",
	})
	if len(errs) != 0 {
		t.Fatalf("applyInlineTags errors = %v, want none", errs)
	}
	if got := tagValues(t, store, wms.EntityOutcome, oid, "component"); len(got) != 1 || got[0] != "harness" {
		t.Errorf("component tags = %v, want [harness]", got)
	}
}

// TestApplyInlineTags_ObjectForm: baseline (pre-existing form) — a
// {value, description} object applies one tag and records the description.
func TestApplyInlineTags_ObjectForm(t *testing.T) {
	store, oid := newStewardStore(t)
	errs := applyInlineTags(context.Background(), store, wms.EntityOutcome, oid, map[string]interface{}{
		"component": map[string]interface{}{"value": "harness", "description": "test/eval harness"},
	})
	if len(errs) != 0 {
		t.Fatalf("applyInlineTags errors = %v, want none", errs)
	}
	if got := tagValues(t, store, wms.EntityOutcome, oid, "component"); len(got) != 1 || got[0] != "harness" {
		t.Errorf("component tags = %v, want [harness]", got)
	}
	if got := listTagsDescription(t, store, "component", "harness"); got != "test/eval harness" {
		t.Errorf("component:harness description = %q, want %q", got, "test/eval harness")
	}
}

// TestApplyInlineTags_ArrayOfStrings: WP2 AC1 — an array of plain strings for
// one key applies each element as its own tag under that key, exercised
// through the full wms_createOutcome MCP path (not just a direct call to
// applyInlineTags), so the create→inline-tag round trip is proven end to end.
func TestApplyInlineTags_ArrayOfStrings(t *testing.T) {
	store, _ := newStewardStore(t)
	const oid = "out-array-strings"
	r, ce := call(t, store, ToolCreateOutcome, map[string]interface{}{
		"id": oid, "title": "array of strings",
		"tags": map[string]interface{}{"github.issue": []interface{}{"17", "11"}},
	})
	if ce != nil {
		t.Fatalf("createOutcome: %v", ce)
	}
	if got := resultText(t, r); !strings.HasPrefix(got, "Created outcome:") {
		t.Errorf("response = %q, want plain success text (no tagErrors)", got)
	}
	if got := tagValues(t, store, wms.EntityOutcome, oid, "github.issue"); len(got) != 2 || got[0] != "11" || got[1] != "17" {
		t.Errorf("github.issue tags = %v, want [11 17]", got)
	}
}

// TestApplyInlineTags_ArrayOfObjects: an array of {value, description}
// objects applies each element with its own per-value description.
func TestApplyInlineTags_ArrayOfObjects(t *testing.T) {
	store, oid := newStewardStore(t)
	errs := applyInlineTags(context.Background(), store, wms.EntityOutcome, oid, map[string]interface{}{
		"github.issue": []interface{}{
			map[string]interface{}{"value": "17", "description": "first issue"},
			map[string]interface{}{"value": "11", "description": "second issue"},
		},
	})
	if len(errs) != 0 {
		t.Fatalf("applyInlineTags errors = %v, want none", errs)
	}
	if got := tagValues(t, store, wms.EntityOutcome, oid, "github.issue"); len(got) != 2 || got[0] != "11" || got[1] != "17" {
		t.Errorf("github.issue tags = %v, want [11 17]", got)
	}
	if got := listTagsDescription(t, store, "github.issue", "17"); got != "first issue" {
		t.Errorf("github.issue:17 description = %q, want %q", got, "first issue")
	}
}

// TestApplyInlineTags_RejectedValue: an invalid element (non-string, no
// value field) is rejected with a tagErrors entry naming the key, and does
// not abort or unwind the rest of the map.
func TestApplyInlineTags_RejectedValue(t *testing.T) {
	store, oid := newStewardStore(t)
	errs := applyInlineTags(context.Background(), store, wms.EntityOutcome, oid, map[string]interface{}{
		"component": "harness",
		"priority":  123.0, // not a string, not an object — invalid scalar
	})
	if len(errs) != 1 {
		t.Fatalf("applyInlineTags errors = %v, want exactly one", errs)
	}
	if !strings.Contains(errs[0], `"priority"`) {
		t.Errorf("error %q does not name the malformed key", errs[0])
	}
	if got := tagValues(t, store, wms.EntityOutcome, oid, "component"); len(got) != 1 || got[0] != "harness" {
		t.Errorf("component tags = %v, want [harness] (valid key must still apply)", got)
	}
	if got := tagValues(t, store, wms.EntityOutcome, oid, "priority"); len(got) != 0 {
		t.Errorf("priority tags = %v, want none", got)
	}
}

// TestApplyInlineTags_ArrayPartialFailure: WP2 AC3 — a tags map with one
// valid array entry and one malformed array entry creates the entity,
// applies the valid key's value, and returns exactly one tagErrors entry
// naming the bad key and identifying which array element failed — not two
// errors, not zero, and the create is not unwound.
func TestApplyInlineTags_ArrayPartialFailure(t *testing.T) {
	store, oid := newStewardStore(t)
	const wuID = "wu-array-partial-failure"
	r, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{
		"id": wuID, "title": "partial failure", "outcomeID": oid,
		"tags": map[string]interface{}{
			"github.issue": []interface{}{"17"},
			"priority":     []interface{}{123.0}, // non-string array element
		},
	})
	if ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	text := resultText(t, r)
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("decode createWorkUnit response %q: %v", text, err)
	}
	tagErrors, _ := decoded["tagErrors"].([]interface{})
	if len(tagErrors) != 1 {
		t.Fatalf("tagErrors = %v, want exactly one entry", tagErrors)
	}
	if msg, _ := tagErrors[0].(string); !strings.Contains(msg, `"priority"`) || !strings.Contains(msg, "[0]") {
		t.Errorf("tagErrors[0] = %q, want it to name %q and identify element [0]", msg, "priority")
	}
	if got := tagValues(t, store, wms.EntityWorkUnit, wuID, "github.issue"); len(got) != 1 || got[0] != "17" {
		t.Errorf("github.issue tags = %v, want [17]", got)
	}
	if got := tagValues(t, store, wms.EntityWorkUnit, wuID, "priority"); len(got) != 0 {
		t.Errorf("priority tags = %v, want none", got)
	}
	// The create itself must not be unwound by the tag failure.
	if _, err := store.GetWorkUnit(context.Background(), wuID); err != nil {
		t.Errorf("GetWorkUnit after partial tag failure: %v", err)
	}
}

// TestApplyInlineTags_SingleCardinalityArrayRejected: WP2 AC4 — passing an
// array for a key defined cardinality:single is caught and reported in
// tagErrors, not silently accepted as "last value wins" (store.TagEntity's
// own single-cardinality replace behavior, which would otherwise apply here
// with zero error signal — see singleCardinalityKey's doc comment).
func TestApplyInlineTags_SingleCardinalityArrayRejected(t *testing.T) {
	store, oid := newStewardStore(t)
	if _, ce := call(t, store, ToolDefineTag, map[string]interface{}{
		"tagKey": "priority", "cardinality": "single", "values": []interface{}{"p1", "p2"},
	}); ce != nil {
		t.Fatalf("seed defineTag priority=single: %v", ce)
	}

	errs := applyInlineTags(context.Background(), store, wms.EntityOutcome, oid, map[string]interface{}{
		"priority": []interface{}{"p1", "p2"},
	})
	if len(errs) != 1 {
		t.Fatalf("applyInlineTags errors = %v, want exactly one", errs)
	}
	if !strings.Contains(errs[0], `"priority"`) || !strings.Contains(errs[0], "cardinality:single") {
		t.Errorf("error %q does not explain the cardinality:single rejection", errs[0])
	}
	if got := tagValues(t, store, wms.EntityOutcome, oid, "priority"); len(got) != 0 {
		t.Errorf("priority tags = %v, want none (rejected outright, not last-value-wins)", got)
	}
}

// TestApplyInlineTags_SingleCardinalityStillCaught_WhenOnlyValueRetired pins
// an adversarial-review MAJOR: singleCardinalityKey must ask the exact same
// question TagEntity's own cardinality resolution asks
// (`SELECT cardinality FROM tags WHERE tag_key = ? AND cardinality =
// 'single' LIMIT 1`, no retired filter), not SearchTags's `retired = 0`
// filtered view. Before the fix, retiring the only single-cardinality VALUE
// row for a key (via `teamster tags delete-value`, store.RetireTagValue)
// made the guard see zero non-retired single rows and conclude "not
// single", while TagEntity's own unfiltered query still saw the retired row
// and applied single-cardinality replace semantics underneath the guard —
// silently keeping only the last of N submitted values with zero tagErrors,
// the exact silent-data-loss bug this guard exists to close.
func TestApplyInlineTags_SingleCardinalityStillCaught_WhenOnlyValueRetired(t *testing.T) {
	store, oid := newStewardStore(t)
	if _, ce := call(t, store, ToolDefineTag, map[string]interface{}{
		"tagKey": "priority", "cardinality": "single", "values": []interface{}{"p1"},
	}); ce != nil {
		t.Fatalf("seed defineTag priority=single: %v", ce)
	}
	// Retire the only single-cardinality VALUE row for this key. After this,
	// zero non-retired rows exist for "priority" — SearchTags(tagKey,"")
	// (retired=0 filtered) returns nothing, but TagEntity's own query (no
	// retired filter) still finds this row and still resolves "single".
	if err := store.RetireTagValue(context.Background(), "priority", "p1"); err != nil {
		t.Fatalf("RetireTagValue(priority, p1): %v", err)
	}

	if !singleCardinalityKey(context.Background(), store, "priority") {
		t.Fatal("singleCardinalityKey(priority) = false after retiring the only value row, want true (matching TagEntity's own unfiltered predicate)")
	}

	// New values (not the retired p1) submitted as an array must still be
	// rejected outright, not silently reduced to one surviving value.
	errs := applyInlineTags(context.Background(), store, wms.EntityOutcome, oid, map[string]interface{}{
		"priority": []interface{}{"p2", "p3"},
	})
	if len(errs) != 1 {
		t.Fatalf("applyInlineTags errors = %v, want exactly one (rejected as single-cardinality)", errs)
	}
	if !strings.Contains(errs[0], "cardinality:single") {
		t.Errorf("error %q does not explain the cardinality:single rejection", errs[0])
	}
	if got := tagValues(t, store, wms.EntityOutcome, oid, "priority"); len(got) != 0 {
		t.Errorf("priority tags = %v, want none — pre-fix this silently kept one of [p2 p3] via TagEntity's own replace semantics", got)
	}
}

// TestApplyInlineTags_EmptyArrayRejectedWithSpecificMessage pins an
// adversarial-review NOTE: an empty array ({"key": []}) must be reported
// with a message naming the actual problem (empty array), not the generic
// scalar-type-mismatch message meant for a non-array, non-object,
// non-string value.
func TestApplyInlineTags_EmptyArrayRejectedWithSpecificMessage(t *testing.T) {
	store, oid := newStewardStore(t)
	errs := applyInlineTags(context.Background(), store, wms.EntityOutcome, oid, map[string]interface{}{
		"github.issue": []interface{}{},
	})
	if len(errs) != 1 {
		t.Fatalf("applyInlineTags errors = %v, want exactly one", errs)
	}
	if !strings.Contains(errs[0], "array must not be empty") {
		t.Errorf("error %q does not name the actual problem (empty array)", errs[0])
	}
}
