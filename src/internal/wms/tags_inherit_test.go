package wms

import (
	"context"
	"errors"
	"testing"
)

// fakeTagInheritStore implements just the Reader methods ResolveEntityTags
// calls: GetWorkUnit, GetOutcome, GetEntityTags. The embedded Reader is nil —
// any other method call panics, the intent being a test that fails loudly if
// ResolveEntityTags grows a new store dependency without test coverage.
type fakeTagInheritStore struct {
	Reader
	workUnits map[string]*WorkUnit
	outcomes  map[string]*Outcome
	tags      map[string][]EntityTag // key: entityType+":"+entityID
}

func (f *fakeTagInheritStore) GetWorkUnit(_ context.Context, id string) (*WorkUnit, error) {
	wu, ok := f.workUnits[id]
	if !ok {
		return nil, errors.New("workunit not found")
	}
	return wu, nil
}

func (f *fakeTagInheritStore) GetOutcome(_ context.Context, id string) (*Outcome, error) {
	o, ok := f.outcomes[id]
	if !ok {
		return nil, errors.New("outcome not found")
	}
	return o, nil
}

func (f *fakeTagInheritStore) GetEntityTags(_ context.Context, entityType, entityID string) ([]EntityTag, error) {
	return f.tags[entityType+":"+entityID], nil
}

func productTag(value string) EntityTag {
	return EntityTag{TagKey: "product", TagValue: value}
}

// A required key set only on the parent outcome resolves as an inherited
// binding on the workunit — this is the defect the fix addresses: previously
// the close-out warning read GetEntityTags (direct only) and never saw it.
func TestResolveEntityTags_WorkUnitInheritsFromOutcome(t *testing.T) {
	store := &fakeTagInheritStore{
		workUnits: map[string]*WorkUnit{"wu-1": {ID: "wu-1", OutcomeID: "out-1"}},
		tags: map[string][]EntityTag{
			"outcome:out-1": {productTag("teamster")},
		},
	}
	got, err := ResolveEntityTags(context.Background(), store, EntityWorkUnit, "wu-1")
	if err != nil {
		t.Fatalf("ResolveEntityTags: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d tags, want 1: %+v", len(got), got)
	}
	if got[0].TagKey != "product" || got[0].TagValue != "teamster" {
		t.Errorf("tag = %+v, want product:teamster", got[0])
	}
	if !got[0].Inherited {
		t.Error("Inherited = false, want true (bound on the outcome, not the workunit)")
	}
	if got[0].Origin != "out-1" {
		t.Errorf("Origin = %q, want the outcome id out-1", got[0].Origin)
	}
}

// A workunit's own binding for a key shadows the outcome's differently-valued
// row for that same key — the outcome's row must not also appear.
func TestResolveEntityTags_WorkUnitOwnBindingShadowsOutcome(t *testing.T) {
	store := &fakeTagInheritStore{
		workUnits: map[string]*WorkUnit{"wu-1": {ID: "wu-1", OutcomeID: "out-1"}},
		tags: map[string][]EntityTag{
			"workunit:wu-1": {productTag("wms-hygiene")},
			"outcome:out-1": {productTag("teamster")},
		},
	}
	got, err := ResolveEntityTags(context.Background(), store, EntityWorkUnit, "wu-1")
	if err != nil {
		t.Fatalf("ResolveEntityTags: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d tags, want 1 (the outcome's shadowed row must not also appear): %+v", len(got), got)
	}
	if got[0].TagValue != "wms-hygiene" {
		t.Errorf("TagValue = %q, want the workunit's own wms-hygiene, not the outcome's teamster", got[0].TagValue)
	}
	if got[0].Inherited {
		t.Error("Inherited = true, want false (the workunit's own binding wins)")
	}
	if got[0].Origin != "wu-1" {
		t.Errorf("Origin = %q, want the workunit's own id wu-1", got[0].Origin)
	}
}

// Neither the workunit nor its outcome carries the key — it is simply absent
// from the resolved set (missingRequiredKeys is what turns an absence into a
// warning; this only proves the absence).
func TestResolveEntityTags_NeitherHasKey(t *testing.T) {
	store := &fakeTagInheritStore{
		workUnits: map[string]*WorkUnit{"wu-1": {ID: "wu-1", OutcomeID: "out-1"}},
		tags:      map[string][]EntityTag{},
	}
	got, err := ResolveEntityTags(context.Background(), store, EntityWorkUnit, "wu-1")
	if err != nil {
		t.Fatalf("ResolveEntityTags: %v", err)
	}
	for _, rt := range got {
		if rt.TagKey == "product" {
			t.Fatalf("unexpected product tag in resolved set: %+v", got)
		}
	}
}

// An Outcome entity never inherits further — only its own direct bindings
// come back, regardless of what other data the store holds.
func TestResolveEntityTags_OutcomeDoesNotInheritFurther(t *testing.T) {
	store := &fakeTagInheritStore{
		outcomes: map[string]*Outcome{"out-1": {ID: "out-1"}},
		tags: map[string][]EntityTag{
			"outcome:out-1": {productTag("teamster")},
		},
	}
	got, err := ResolveEntityTags(context.Background(), store, EntityOutcome, "out-1")
	if err != nil {
		t.Fatalf("ResolveEntityTags: %v", err)
	}
	if len(got) != 1 || got[0].Inherited {
		t.Errorf("got %+v, want exactly one direct (Inherited=false) tag", got)
	}
}

// An unknown workunit surfaces the store's not-found error rather than a
// silently empty tag list.
func TestResolveEntityTags_UnknownWorkUnit(t *testing.T) {
	store := &fakeTagInheritStore{workUnits: map[string]*WorkUnit{}}
	if _, err := ResolveEntityTags(context.Background(), store, EntityWorkUnit, "wu-ghost"); err == nil {
		t.Error("expected a not-found error for an unknown workunit, got nil")
	}
}

// Same for an unknown outcome.
func TestResolveEntityTags_UnknownOutcome(t *testing.T) {
	store := &fakeTagInheritStore{outcomes: map[string]*Outcome{}}
	if _, err := ResolveEntityTags(context.Background(), store, EntityOutcome, "out-ghost"); err == nil {
		t.Error("expected a not-found error for an unknown outcome, got nil")
	}
}
