package wms

import (
	"encoding/json"
	"strings"
	"testing"

	storeTypes "github.com/bmjdotnet/teamster/internal/store"
)

// These tests exercise wms_addRelation / wms_removeRelation / wms_listRelations
// / wms_listRelationKinds through HandleToolCall against a real store (WP3
// stage 2, outcome_relations). Same newStewardStore/call/createTestOutcome
// helpers as wms_reparenting_test.go. SKIPs when TEAMSTER_TEST_MYSQL_DSN is
// unset (see newStewardStore).

// TestAddRelation_HappyPath: a taxable relation between two existing outcomes
// is recorded and visible via wms_listRelations from either endpoint.
func TestAddRelation_HappyPath(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "auth-v2", "Auth rewrite")
	createTestOutcome(t, store, "auth-v1", "Original auth")

	if _, ce := call(t, store, ToolAddRelation, map[string]interface{}{
		"kind": "remediates", "fromType": "outcome", "fromID": "auth-v2",
		"toType": "outcome", "toID": "auth-v1", "note": "fixed the login bug",
	}); ce != nil {
		t.Fatalf("addRelation: %v", ce)
	}

	r, ce := call(t, store, ToolListRelations, map[string]interface{}{
		"entityType": "outcome", "entityID": "auth-v1",
	})
	if ce != nil {
		t.Fatalf("listRelations: %v", ce)
	}
	var rels []storeTypes.Relation
	if err := json.Unmarshal([]byte(resultText(t, r)), &rels); err != nil {
		t.Fatalf("unmarshal relations: %v", err)
	}
	found := false
	for _, rel := range rels {
		if rel.Kind == "remediates" && rel.FromID == "auth-v2" && rel.ToID == "auth-v1" {
			found = true
		}
	}
	if !found {
		t.Errorf("listRelations(auth-v1) = %+v, want a remediates edge from auth-v2", rels)
	}
}

// TestAddRelation_UnknownKind: a kind not present in relation_kinds is
// rejected, and the error names the valid set — kind must not be treated as
// free text.
func TestAddRelation_UnknownKind(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "a-uk", "A")
	createTestOutcome(t, store, "b-uk", "B")

	_, ce := call(t, store, ToolAddRelation, map[string]interface{}{
		"kind": "bogus-kind", "fromType": "outcome", "fromID": "a-uk",
		"toType": "outcome", "toID": "b-uk",
	})
	if ce == nil {
		t.Fatal("addRelation with unknown kind returned no error")
	}
	if !strings.Contains(ce.Message, "bogus-kind") {
		t.Errorf("error %q does not name the unknown kind", ce.Message)
	}
	if !strings.Contains(ce.Message, "remediates") {
		t.Errorf("error %q does not list a valid kind", ce.Message)
	}
}

// TestAddRelation_SelfLoopRejected: an entity cannot relate to itself.
func TestAddRelation_SelfLoopRejected(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "self-rel", "Self")

	_, ce := call(t, store, ToolAddRelation, map[string]interface{}{
		"kind": "remediates", "fromType": "outcome", "fromID": "self-rel",
		"toType": "outcome", "toID": "self-rel",
	})
	if ce == nil {
		t.Fatal("addRelation self-loop returned no error")
	}
}

// TestAddRelation_FromNotFound: an unknown fromID errors instead of silently
// no-op'ing.
func TestAddRelation_FromNotFound(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "to-exists", "Exists")

	_, ce := call(t, store, ToolAddRelation, map[string]interface{}{
		"kind": "remediates", "fromType": "outcome", "fromID": "does-not-exist-from",
		"toType": "outcome", "toID": "to-exists",
	})
	if ce == nil {
		t.Fatal("addRelation with unknown fromID returned no error")
	}
	if !strings.Contains(ce.Message, "does-not-exist-from") {
		t.Errorf("error %q does not name the missing from-entity", ce.Message)
	}
}

// TestAddRelation_ToNotFound: an unknown toID (non-external) errors the same
// way — the referential check that replaces the impossible FK.
func TestAddRelation_ToNotFound(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "from-exists", "Exists")

	_, ce := call(t, store, ToolAddRelation, map[string]interface{}{
		"kind": "remediates", "fromType": "outcome", "fromID": "from-exists",
		"toType": "outcome", "toID": "does-not-exist-to",
	})
	if ce == nil {
		t.Fatal("addRelation with unknown toID returned no error")
	}
	if !strings.Contains(ce.Message, "does-not-exist-to") {
		t.Errorf("error %q does not name the missing to-entity", ce.Message)
	}
}

// TestAddRelation_ExternalTarget: toType='external' accepts free text in
// toID with no backing entity — the residual-bucket path (§3.5).
func TestAddRelation_ExternalTarget(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "fixes-legacy", "Fixes legacy code")

	if _, ce := call(t, store, ToolAddRelation, map[string]interface{}{
		"kind": "remediates", "fromType": "outcome", "fromID": "fixes-legacy",
		"toType": "external", "toID": "pre-WMS billing script, no tracked Outcome",
	}); ce != nil {
		t.Fatalf("addRelation with external target: %v", ce)
	}

	r, ce := call(t, store, ToolListRelations, map[string]interface{}{
		"entityType": "outcome", "entityID": "fixes-legacy", "direction": "from",
	})
	if ce != nil {
		t.Fatalf("listRelations: %v", ce)
	}
	var rels []storeTypes.Relation
	if err := json.Unmarshal([]byte(resultText(t, r)), &rels); err != nil {
		t.Fatalf("unmarshal relations: %v", err)
	}
	found := false
	for _, rel := range rels {
		if rel.ToType == "external" && rel.ToID == "pre-WMS billing script, no tracked Outcome" {
			found = true
		}
	}
	if !found {
		t.Errorf("listRelations(fixes-legacy, from) = %+v, want the external-target edge", rels)
	}
}

// TestRemoveRelation_HappyPath: removes a previously added relation.
func TestRemoveRelation_HappyPath(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "rm-from", "From")
	createTestOutcome(t, store, "rm-to", "To")

	if _, ce := call(t, store, ToolAddRelation, map[string]interface{}{
		"kind": "remediates", "fromType": "outcome", "fromID": "rm-from",
		"toType": "outcome", "toID": "rm-to",
	}); ce != nil {
		t.Fatalf("addRelation: %v", ce)
	}
	if _, ce := call(t, store, ToolRemoveRelation, map[string]interface{}{
		"kind": "remediates", "fromType": "outcome", "fromID": "rm-from",
		"toType": "outcome", "toID": "rm-to",
	}); ce != nil {
		t.Fatalf("removeRelation: %v", ce)
	}

	r, ce := call(t, store, ToolListRelations, map[string]interface{}{
		"entityType": "outcome", "entityID": "rm-to",
	})
	if ce != nil {
		t.Fatalf("listRelations: %v", ce)
	}
	var rels []storeTypes.Relation
	if err := json.Unmarshal([]byte(resultText(t, r)), &rels); err != nil {
		t.Fatalf("unmarshal relations: %v", err)
	}
	for _, rel := range rels {
		if rel.FromID == "rm-from" && rel.ToID == "rm-to" {
			t.Errorf("relation rm-from->rm-to still present after removal: %+v", rel)
		}
	}
}

// TestRemoveRelation_NonexistentEdge: removing a relation that was never
// added succeeds as an idempotent no-op, matching wms_removeDependency /
// wms_removeOutcomeParent.
func TestRemoveRelation_NonexistentEdge(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "noop-from", "From")
	createTestOutcome(t, store, "noop-to", "To")

	if _, ce := call(t, store, ToolRemoveRelation, map[string]interface{}{
		"kind": "remediates", "fromType": "outcome", "fromID": "noop-from",
		"toType": "outcome", "toID": "noop-to",
	}); ce != nil {
		t.Fatalf("removeRelation on nonexistent edge: %v", ce)
	}
}

// TestListRelations_BothDirections: querying either endpoint of a relation
// surfaces it, and the from-only/to-only direction filters narrow correctly.
func TestListRelations_BothDirections(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "dir-a", "A")
	createTestOutcome(t, store, "dir-b", "B")

	if _, ce := call(t, store, ToolAddRelation, map[string]interface{}{
		"kind": "remediates", "fromType": "outcome", "fromID": "dir-a",
		"toType": "outcome", "toID": "dir-b",
	}); ce != nil {
		t.Fatalf("addRelation: %v", ce)
	}

	// From dir-a's side, direction=from should surface the edge (dir-a is the
	// remediation). direction=to should not.
	rFrom, ce := call(t, store, ToolListRelations, map[string]interface{}{
		"entityType": "outcome", "entityID": "dir-a", "direction": "from",
	})
	if ce != nil {
		t.Fatalf("listRelations(dir-a, from): %v", ce)
	}
	var relsFrom []storeTypes.Relation
	if err := json.Unmarshal([]byte(resultText(t, rFrom)), &relsFrom); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(relsFrom) != 1 || relsFrom[0].FromID != "dir-a" || relsFrom[0].ToID != "dir-b" {
		t.Errorf("listRelations(dir-a, from) = %+v, want exactly the dir-a->dir-b edge", relsFrom)
	}

	// From dir-b's side, direction=to should surface the edge (dir-b is the
	// prior work being remediated).
	rTo, ce := call(t, store, ToolListRelations, map[string]interface{}{
		"entityType": "outcome", "entityID": "dir-b", "direction": "to",
	})
	if ce != nil {
		t.Fatalf("listRelations(dir-b, to): %v", ce)
	}
	var relsTo []storeTypes.Relation
	if err := json.Unmarshal([]byte(resultText(t, rTo)), &relsTo); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(relsTo) != 1 || relsTo[0].FromID != "dir-a" || relsTo[0].ToID != "dir-b" {
		t.Errorf("listRelations(dir-b, to) = %+v, want exactly the dir-a->dir-b edge", relsTo)
	}

	// direction=both (default) from either endpoint should also surface it.
	rBoth, ce := call(t, store, ToolListRelations, map[string]interface{}{
		"entityType": "outcome", "entityID": "dir-b",
	})
	if ce != nil {
		t.Fatalf("listRelations(dir-b, both): %v", ce)
	}
	var relsBoth []storeTypes.Relation
	if err := json.Unmarshal([]byte(resultText(t, rBoth)), &relsBoth); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := false
	for _, rel := range relsBoth {
		if rel.FromID == "dir-a" && rel.ToID == "dir-b" {
			found = true
		}
	}
	if !found {
		t.Errorf("listRelations(dir-b) with default direction = %+v, want the dir-a->dir-b edge", relsBoth)
	}
}

// TestListRelationKinds_ReturnsAllSeeded: the vocabulary discovery tool
// returns all 8 seeded kinds from the relation_kinds migration (§3.2).
func TestListRelationKinds_ReturnsAllSeeded(t *testing.T) {
	store, _ := newStewardStore(t)

	r, ce := call(t, store, ToolListRelationKinds, map[string]interface{}{})
	if ce != nil {
		t.Fatalf("listRelationKinds: %v", ce)
	}
	var kinds []storeTypes.RelationKind
	if err := json.Unmarshal([]byte(resultText(t, r)), &kinds); err != nil {
		t.Fatalf("unmarshal kinds: %v", err)
	}

	want := map[string]bool{
		"remediates":           false,
		"addresses-limitation": false,
		"fulfills-realization": false,
		"reverts":              false,
		"supersedes":           false,
		"follows-up-on":        false,
		"discovered-during":    false,
		"duplicate-of":         false,
	}
	for _, k := range kinds {
		if _, ok := want[k.Kind]; ok {
			want[k.Kind] = true
		}
	}
	for kind, seen := range want {
		if !seen {
			t.Errorf("listRelationKinds() missing seeded kind %q; got %+v", kind, kinds)
		}
	}
	if len(kinds) != len(want) {
		t.Errorf("listRelationKinds() returned %d kinds, want exactly %d seeded", len(kinds), len(want))
	}
}
