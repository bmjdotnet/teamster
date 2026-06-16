package wms

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeCloseoutStore implements just the Reader methods CloseoutWarnings touches.
// All other Reader methods are unused and panic if called, so a test that drifts
// onto an unexpected read fails loudly rather than silently.
type fakeCloseoutStore struct {
	units    []*WorkUnit
	unitsErr error
	tags     []EntityTag
	tagsErr  error
}

func (f *fakeCloseoutStore) ListWorkUnits(_ context.Context, _ string) ([]*WorkUnit, error) {
	return f.units, f.unitsErr
}

func (f *fakeCloseoutStore) GetEntityTags(_ context.Context, _, _ string) ([]EntityTag, error) {
	return f.tags, f.tagsErr
}

// --- unused Reader surface (panic if a test path unexpectedly calls them) ---

func (f *fakeCloseoutStore) RoleAllowed(context.Context, string, string, string, string) (bool, error) {
	panic("unexpected")
}
func (f *fakeCloseoutStore) GetJournalEntries(context.Context, string, string, int) ([]JournalEntry, error) {
	panic("unexpected")
}
func (f *fakeCloseoutStore) GetOpenEventRecord(context.Context, string, string) (*EventRecord, error) {
	panic("unexpected")
}
func (f *fakeCloseoutStore) ListEventRecords(context.Context, string, string, int) ([]EventRecord, error) {
	panic("unexpected")
}
func (f *fakeCloseoutStore) ListTags(context.Context) ([]Tag, error) { panic("unexpected") }
func (f *fakeCloseoutStore) ListRequiredTagKeys(context.Context) ([]string, error) {
	panic("unexpected")
}
func (f *fakeCloseoutStore) GetOutcome(context.Context, string) (*Outcome, error) {
	panic("unexpected")
}
func (f *fakeCloseoutStore) ListOutcomes(context.Context, string, map[string]string, string) ([]*Outcome, error) {
	panic("unexpected")
}
func (f *fakeCloseoutStore) GetWorkUnit(context.Context, string) (*WorkUnit, error) {
	panic("unexpected")
}
func (f *fakeCloseoutStore) ListReadyWorkUnits(context.Context, string) ([]*WorkUnit, error) {
	panic("unexpected")
}
func (f *fakeCloseoutStore) GetOutcomeParents(context.Context, string) ([]string, error) {
	panic("unexpected")
}
func (f *fakeCloseoutStore) GetOutcomeChildren(context.Context, string) ([]string, error) {
	panic("unexpected")
}
func (f *fakeCloseoutStore) ListEntityDependencyBlockers(context.Context, string, string) ([]*Dependency, error) {
	panic("unexpected")
}
func (f *fakeCloseoutStore) ListEntityDependencyDependents(context.Context, string, string) ([]*Dependency, error) {
	panic("unexpected")
}

func resolutionTag() EntityTag {
	return EntityTag{TagKey: resolutionTagKey, TagValue: "achieved", Source: "manual"}
}

// (a)/(c): a non-terminal child work unit warns, naming the unit and status.
func TestCloseoutWarnings_PendingChildWarns(t *testing.T) {
	store := &fakeCloseoutStore{
		units: []*WorkUnit{{ID: "pizzahut-sieve", Status: StatusPending}},
		tags:  []EntityTag{resolutionTag()}, // isolate the child-status warning
	}
	w := CloseoutWarnings(context.Background(), store, "test-solo-mode", StatusDone)
	if len(w) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(w), w)
	}
	if !strings.Contains(w[0], "pizzahut-sieve") || !strings.Contains(w[0], StatusPending) {
		t.Fatalf("warning should name the unit and its status, got: %q", w[0])
	}
}

// An active/review child also warns — non-terminal is the trigger, not pending.
func TestCloseoutWarnings_ActiveAndReviewChildrenWarn(t *testing.T) {
	store := &fakeCloseoutStore{
		units: []*WorkUnit{
			{ID: "wu-active", Status: StatusActive},
			{ID: "wu-review", Status: StatusReview},
		},
		tags: []EntityTag{resolutionTag()},
	}
	w := CloseoutWarnings(context.Background(), store, "o1", StatusDone)
	if len(w) != 1 {
		t.Fatalf("expected 1 child-status warning, got %d: %v", len(w), w)
	}
	if !strings.Contains(w[0], "wu-active") || !strings.Contains(w[0], "wu-review") {
		t.Fatalf("warning should name both open units, got: %q", w[0])
	}
}

// (b): no resolution tag warns.
func TestCloseoutWarnings_MissingResolutionWarns(t *testing.T) {
	store := &fakeCloseoutStore{
		units: []*WorkUnit{{ID: "wu1", Status: StatusDone}}, // child terminal, isolate (b)
		tags:  []EntityTag{{TagKey: "product", TagValue: "teamster"}},
	}
	w := CloseoutWarnings(context.Background(), store, "o1", StatusDone)
	if len(w) != 1 {
		t.Fatalf("expected 1 resolution warning, got %d: %v", len(w), w)
	}
	if !strings.Contains(w[0], "resolution") {
		t.Fatalf("warning should mention resolution, got: %q", w[0])
	}
}

// Both misses at once produce both warnings.
func TestCloseoutWarnings_BothMissesWarnTwice(t *testing.T) {
	store := &fakeCloseoutStore{
		units: []*WorkUnit{{ID: "wu1", Status: StatusActive}},
		tags:  nil, // no resolution tag
	}
	w := CloseoutWarnings(context.Background(), store, "o1", StatusDone)
	if len(w) != 2 {
		t.Fatalf("expected 2 warnings, got %d: %v", len(w), w)
	}
}

// A clean close-out (all children done, resolution tagged) produces NO warning.
func TestCloseoutWarnings_CleanCloseoutSilent(t *testing.T) {
	store := &fakeCloseoutStore{
		units: []*WorkUnit{
			{ID: "wu1", Status: StatusDone},
			{ID: "wu2", Status: StatusDone},
		},
		tags: []EntityTag{resolutionTag()},
	}
	w := CloseoutWarnings(context.Background(), store, "o1", StatusDone)
	if len(w) != 0 {
		t.Fatalf("clean close-out should warn nothing, got %d: %v", len(w), w)
	}
	if got := FormatCloseoutWarnings(w); got != "" {
		t.Fatalf("clean close-out format should be empty, got: %q", got)
	}
}

// An outcome with zero work units and a resolution tag is a clean close-out.
func TestCloseoutWarnings_NoChildrenSilent(t *testing.T) {
	store := &fakeCloseoutStore{units: nil, tags: []EntityTag{resolutionTag()}}
	w := CloseoutWarnings(context.Background(), store, "o1", StatusDone)
	if len(w) != 0 {
		t.Fatalf("childless resolved outcome should warn nothing, got: %v", w)
	}
}

// Non-done transitions never warn (the helper short-circuits).
func TestCloseoutWarnings_NonDoneTransitionSilent(t *testing.T) {
	// A store whose reads would panic — proving the helper never queries on a
	// non-terminal transition.
	store := &fakeCloseoutStore{}
	for _, st := range []string{StatusActive, StatusReview, StatusBlocked, StatusPending} {
		if w := CloseoutWarnings(context.Background(), store, "o1", st); w != nil {
			t.Fatalf("transition to %q should not warn, got: %v", st, w)
		}
	}
}

// Store-read failures degrade silently — a warning that can't be computed is
// simply omitted; the transition is never blocked.
func TestCloseoutWarnings_StoreErrorsDegradeSilently(t *testing.T) {
	store := &fakeCloseoutStore{
		unitsErr: errors.New("boom"),
		tagsErr:  errors.New("boom"),
	}
	w := CloseoutWarnings(context.Background(), store, "o1", StatusDone)
	if len(w) != 0 {
		t.Fatalf("read errors should suppress warnings, got: %v", w)
	}
}

// FormatCloseoutWarnings renders a labeled, bulleted advisory block.
func TestFormatCloseoutWarnings_RendersBlock(t *testing.T) {
	got := FormatCloseoutWarnings([]string{"first", "second"})
	if !strings.Contains(got, "advisory") {
		t.Fatalf("block should label itself advisory, got: %q", got)
	}
	if !strings.Contains(got, "- first") || !strings.Contains(got, "- second") {
		t.Fatalf("block should bullet each warning, got: %q", got)
	}
}
