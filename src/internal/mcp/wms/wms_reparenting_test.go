package wms

import (
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// These tests exercise wms_addOutcomeParent / wms_removeOutcomeParent through
// HandleToolCall against a real store (same newStewardStore/call/entityHasTag
// helpers as wms_steward_test.go). SKIPs when TEAMSTER_TEST_MYSQL_DSN is unset.

// createTestOutcome creates a bare outcome via the store directly (bypassing
// HandleToolCall) so tests can seed fixtures without depending on
// wms_createOutcome's own behavior.
func createTestOutcome(t *testing.T, store wms.Store, id, title string) {
	t.Helper()
	o := &wms.Outcome{ID: id, Title: title, Status: wms.StatusPending}
	if err := store.CreateOutcome(t.Context(), o); err != nil {
		t.Fatalf("seed outcome %s: %v", id, err)
	}
}

// TestAddOutcomeParent_HappyPath: adding an edge between two existing
// outcomes succeeds and is visible via GetOutcomeParents.
func TestAddOutcomeParent_HappyPath(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "parent-1", "Parent outcome")
	createTestOutcome(t, store, "child-1", "Child outcome")

	if _, ce := call(t, store, ToolAddOutcomeParent, map[string]interface{}{
		"parentID": "parent-1", "childID": "child-1",
	}); ce != nil {
		t.Fatalf("addOutcomeParent: %v", ce)
	}

	parents, err := store.GetOutcomeParents(t.Context(), "child-1")
	if err != nil {
		t.Fatalf("GetOutcomeParents: %v", err)
	}
	if len(parents) != 1 || parents[0] != "parent-1" {
		t.Errorf("GetOutcomeParents(child-1) = %v, want [parent-1]", parents)
	}
}

// TestAddOutcomeParent_ParentNotFound: an unknown parentID errors instead of
// silently no-op'ing (the INSERT IGNORE footgun the pre-flight check guards
// against).
func TestAddOutcomeParent_ParentNotFound(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "child-2", "Child outcome")

	_, ce := call(t, store, ToolAddOutcomeParent, map[string]interface{}{
		"parentID": "does-not-exist", "childID": "child-2",
	})
	if ce == nil {
		t.Fatal("addOutcomeParent with unknown parentID returned no error")
	}
	if !strings.Contains(ce.Message, "does-not-exist") {
		t.Errorf("error %q does not name the missing parent", ce.Message)
	}

	parents, err := store.GetOutcomeParents(t.Context(), "child-2")
	if err != nil {
		t.Fatalf("GetOutcomeParents: %v", err)
	}
	if len(parents) != 0 {
		t.Errorf("GetOutcomeParents(child-2) = %v, want no edge written", parents)
	}
}

// TestAddOutcomeParent_ChildNotFound: an unknown childID errors the same way.
func TestAddOutcomeParent_ChildNotFound(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "parent-3", "Parent outcome")

	_, ce := call(t, store, ToolAddOutcomeParent, map[string]interface{}{
		"parentID": "parent-3", "childID": "does-not-exist",
	})
	if ce == nil {
		t.Fatal("addOutcomeParent with unknown childID returned no error")
	}
	if !strings.Contains(ce.Message, "does-not-exist") {
		t.Errorf("error %q does not name the missing child", ce.Message)
	}
}

// TestAddOutcomeParent_SelfLoopRejected: an outcome cannot be its own parent.
func TestAddOutcomeParent_SelfLoopRejected(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "self-4", "Self outcome")

	_, ce := call(t, store, ToolAddOutcomeParent, map[string]interface{}{
		"parentID": "self-4", "childID": "self-4",
	})
	if ce == nil {
		t.Fatal("addOutcomeParent self-loop returned no error")
	}
	if !strings.Contains(ce.Message, "self-loop") {
		t.Errorf("error %q does not report a self-loop", ce.Message)
	}
}

// TestAddOutcomeParent_CycleRejected: A→B then B→A must be refused — it
// would turn the outcome DAG into a cycle.
func TestAddOutcomeParent_CycleRejected(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "a-5", "A")
	createTestOutcome(t, store, "b-5", "B")

	if _, ce := call(t, store, ToolAddOutcomeParent, map[string]interface{}{
		"parentID": "a-5", "childID": "b-5",
	}); ce != nil {
		t.Fatalf("addOutcomeParent a-5→b-5: %v", ce)
	}

	_, ce := call(t, store, ToolAddOutcomeParent, map[string]interface{}{
		"parentID": "b-5", "childID": "a-5",
	})
	if ce == nil {
		t.Fatal("addOutcomeParent b-5→a-5 (would create a cycle) returned no error")
	}
	if !strings.Contains(ce.Message, "cycle") {
		t.Errorf("error %q does not report a cycle", ce.Message)
	}
}

// TestRemoveOutcomeParent_HappyPath: removes a previously added edge.
func TestRemoveOutcomeParent_HappyPath(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "parent-6", "Parent")
	createTestOutcome(t, store, "child-6", "Child")

	if _, ce := call(t, store, ToolAddOutcomeParent, map[string]interface{}{
		"parentID": "parent-6", "childID": "child-6",
	}); ce != nil {
		t.Fatalf("addOutcomeParent: %v", ce)
	}
	if _, ce := call(t, store, ToolRemoveOutcomeParent, map[string]interface{}{
		"parentID": "parent-6", "childID": "child-6",
	}); ce != nil {
		t.Fatalf("removeOutcomeParent: %v", ce)
	}

	parents, err := store.GetOutcomeParents(t.Context(), "child-6")
	if err != nil {
		t.Fatalf("GetOutcomeParents: %v", err)
	}
	if len(parents) != 0 {
		t.Errorf("GetOutcomeParents(child-6) after removal = %v, want none", parents)
	}
}

// TestRemoveOutcomeParent_NonexistentEdge: removing an edge that was never
// added succeeds as an idempotent no-op, matching wms_removeDependency.
func TestRemoveOutcomeParent_NonexistentEdge(t *testing.T) {
	store, _ := newStewardStore(t)
	createTestOutcome(t, store, "parent-7", "Parent")
	createTestOutcome(t, store, "child-7", "Child")

	if _, ce := call(t, store, ToolRemoveOutcomeParent, map[string]interface{}{
		"parentID": "parent-7", "childID": "child-7",
	}); ce != nil {
		t.Fatalf("removeOutcomeParent on nonexistent edge: %v", ce)
	}
}
