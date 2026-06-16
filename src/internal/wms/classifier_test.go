package wms

import (
	"context"
	"testing"
	"time"
)

// fakeClassifierStore implements just the Store methods RuleClassifier calls:
// GetEntityTags (to learn which keys an operator pinned), ListEventRecords (to
// build windows / detect re-entry) and TagEntity (to apply tags). The embedded
// Store is nil — any other method would panic, which is the intent: the test
// fails loudly if the classifier grows a new store dependency without test
// coverage.
type fakeClassifierStore struct {
	Store
	records  []EventRecord
	existing []EntityTag // bindings GetEntityTags returns (manual + classifier)
	tagsErr  error       // when set, GetEntityTags fails (best-effort fallback path)
	applied  []appliedCall
}

type appliedCall struct {
	entityType, entityID, tagKey, tagValue, source string
}

func (f *fakeClassifierStore) GetEntityTags(_ context.Context, _, _ string) ([]EntityTag, error) {
	return f.existing, f.tagsErr
}

func (f *fakeClassifierStore) ListEventRecords(_ context.Context, _, _ string, _ int) ([]EventRecord, error) {
	return f.records, nil
}

func (f *fakeClassifierStore) TagEntity(_ context.Context, entityType, entityID, tagKey, tagValue, source, _ string) error {
	f.applied = append(f.applied, appliedCall{entityType, entityID, tagKey, tagValue, source})
	return nil
}

// fakeSignalReader returns canned signals so each work-type rule can be
// exercised without touching disk.
type fakeSignalReader struct {
	sig *ActivitySignals
}

func (f *fakeSignalReader) ReadSignals(_ context.Context, _ []SessionWindow, _ string) (*ActivitySignals, error) {
	return f.sig, nil
}

// windowAwareSignalReader mirrors JSONLSignalReader's load-bearing semantics:
// it yields signals ONLY when at least one session window was built (the real
// reader returns empty when len(sessions)==0). This is what makes the
// lead-only test non-vacuous — if Classify builds zero windows for the lead
// (the pre-fix bug), this reader returns empty signals and no work-type tag is
// applied, so the test goes RED; once the window is built it returns sig and
// the test goes GREEN.
type windowAwareSignalReader struct {
	sig *ActivitySignals
}

func (f *windowAwareSignalReader) ReadSignals(_ context.Context, sessions []SessionWindow, _ string) (*ActivitySignals, error) {
	if len(sessions) == 0 {
		return &ActivitySignals{ToolTagCounts: map[string]int{}, FilesTouched: map[string]int{}}, nil
	}
	return f.sig, nil
}

// oneRecord returns a minimal record slice that yields a non-empty session
// window (so Classify proceeds to the signal-reading stage).
func oneActiveRecord() []EventRecord {
	return []EventRecord{
		{State: StatusActive, SessionID: "abc123def456ghi", AgentName: "@worker", StartedAt: time.Unix(1747526040, 0).UTC()},
	}
}

func classifyWith(t *testing.T, records []EventRecord, sig *ActivitySignals) []appliedCall {
	t.Helper()
	store, _ := classifyFull(t, &fakeClassifierStore{records: records}, sig)
	return store.applied
}

// classifyFull runs Classify against a caller-supplied store (so the test can
// seed existing tags / errors) and returns both the store (for applied calls)
// and the full result (for Applied/Skipped assertions).
func classifyFull(t *testing.T, store *fakeClassifierStore, sig *ActivitySignals) (*fakeClassifierStore, *ClassifierResult) {
	t.Helper()
	c := NewRuleClassifier(store, &fakeSignalReader{sig: sig}, "unused.jsonl")
	res, err := c.Classify(context.Background(), EntityWorkUnit, "wu-1")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	return store, res
}

func hasSkip(skipped []SkippedTag, key string) bool {
	for _, s := range skipped {
		if s.TagKey == key {
			return true
		}
	}
	return false
}

func hasTag(applied []appliedCall, key, value string) bool {
	for _, a := range applied {
		if a.tagKey == key && a.tagValue == value {
			return true
		}
	}
	return false
}

// --- Work-type rules (each given canned signals) ---

func TestClassify_WorkTypeResearch(t *testing.T) {
	// READ+GREP > 50% of tool tags → research.
	sig := &ActivitySignals{
		ToolTagCounts: map[string]int{"READ": 6, "GREP": 2, "EDIT": 2},
		TotalEvents:   10,
	}
	applied := classifyWith(t, oneActiveRecord(), sig)
	if !hasTag(applied, "work-type", "research") {
		t.Fatalf("expected work-type=research, got %+v", applied)
	}
}

func TestClassify_WorkTypeTest(t *testing.T) {
	// >60% of bash commands contain "test" → test (and READ/GREP under 50%).
	sig := &ActivitySignals{
		ToolTagCounts: map[string]int{"EXEC": 5},
		BashCommands:  []string{"go test ./...", "go test -run X", "make test", "ls"},
		TotalEvents:   5,
	}
	applied := classifyWith(t, oneActiveRecord(), sig)
	if !hasTag(applied, "work-type", "test") {
		t.Fatalf("expected work-type=test, got %+v", applied)
	}
}

func TestClassify_WorkTypeDocs(t *testing.T) {
	// >40% of files touched are .md → docs.
	sig := &ActivitySignals{
		ToolTagCounts: map[string]int{"EDIT": 5},
		FilesTouched:  map[string]int{".md": 3, ".go": 2},
		TotalEvents:   5,
	}
	applied := classifyWith(t, oneActiveRecord(), sig)
	if !hasTag(applied, "work-type", "docs") {
		t.Fatalf("expected work-type=docs, got %+v", applied)
	}
}

func TestClassify_WorkTypeFeatureFallback(t *testing.T) {
	// Activity present but no rule matches → feature fallback.
	sig := &ActivitySignals{
		ToolTagCounts: map[string]int{"EDIT": 8, "READ": 1},
		BashCommands:  []string{"go build ./..."},
		FilesTouched:  map[string]int{".go": 5},
		TotalEvents:   9,
	}
	applied := classifyWith(t, oneActiveRecord(), sig)
	if !hasTag(applied, "work-type", "feature") {
		t.Fatalf("expected work-type=feature fallback, got %+v", applied)
	}
}

func TestClassify_NoActivitySkips(t *testing.T) {
	// Records exist (so we get past the empty-records guard) but signals are
	// empty → no work-type tag applied at all.
	sig := &ActivitySignals{ToolTagCounts: map[string]int{}, FilesTouched: map[string]int{}, TotalEvents: 0}
	applied := classifyWith(t, oneActiveRecord(), sig)
	for _, a := range applied {
		if a.tagKey == "work-type" {
			t.Fatalf("expected no work-type tag with zero activity, got %+v", applied)
		}
	}
}

// TestClassify_LeadEmptyAgentBuildsWindow is the single-agent (solo) guard: a
// lead-only session carries the empty agent_name (lead == ""), so the classifier
// must still build a signal window for it and apply a work-type tag. Before the
// B0 fix the window-building guard required AgentName != "", which dropped the
// lead, yielded zero windows, and silently skipped work-type for every solo
// operator. The reader here is window-aware (yields signals only when a window
// exists), so this test goes RED on the pre-fix guard and GREEN after — it is
// NOT vacuous. Every other classifier test uses AgentName:"@worker", so this is
// the only coverage of the empty-agent (lead) path.
func TestClassify_LeadEmptyAgentBuildsWindow(t *testing.T) {
	records := []EventRecord{
		{State: StatusActive, SessionID: "abc123def456ghi", AgentName: "", StartedAt: time.Unix(1747526040, 0).UTC()},
	}
	// READ+GREP > 50% → research, but only if a window was built for the lead.
	sig := &ActivitySignals{
		ToolTagCounts: map[string]int{"READ": 6, "GREP": 2, "EDIT": 2},
		TotalEvents:   10,
	}
	store := &fakeClassifierStore{records: records}
	c := NewRuleClassifier(store, &windowAwareSignalReader{sig: sig}, "unused.jsonl")
	if _, err := c.Classify(context.Background(), EntityWorkUnit, "wu-1"); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if !hasTag(store.applied, "work-type", "research") {
		t.Fatalf("expected work-type=research for lead (empty agent_name); the window was not built — got %+v", store.applied)
	}
}

// --- Re-entry rule (phase=rework), independent of signals ---

func TestClassify_PhaseReworkOnReEntry(t *testing.T) {
	// ListEventRecords returns newest-first; the classifier iterates oldest→
	// newest. A done/review followed by a return to active is re-entry.
	records := []EventRecord{
		{State: StatusActive, SessionID: "abc123def456ghi", AgentName: "@worker", StartedAt: time.Unix(1747526100, 0).UTC()}, // newest
		{State: StatusReview, SessionID: "abc123def456ghi", AgentName: "@worker", StartedAt: time.Unix(1747526040, 0).UTC()}, // older
	}
	sig := &ActivitySignals{ToolTagCounts: map[string]int{"EDIT": 1}, TotalEvents: 1}
	applied := classifyWith(t, records, sig)
	if !hasTag(applied, "phase", "rework") {
		t.Fatalf("expected phase=rework on re-entry, got %+v", applied)
	}
}

func TestClassify_NoReworkWithoutReEntry(t *testing.T) {
	// A linear pending→active with no prior done/review must NOT tag rework.
	records := []EventRecord{
		{State: StatusActive, SessionID: "abc123def456ghi", AgentName: "@worker", StartedAt: time.Unix(1747526100, 0).UTC()},
		{State: StatusPending, SessionID: "abc123def456ghi", AgentName: "@worker", StartedAt: time.Unix(1747526040, 0).UTC()},
	}
	sig := &ActivitySignals{ToolTagCounts: map[string]int{"EDIT": 1}, TotalEvents: 1}
	applied := classifyWith(t, records, sig)
	if hasTag(applied, "phase", "rework") {
		t.Fatalf("did not expect phase=rework without re-entry, got %+v", applied)
	}
}

// --- Manual-tag respect (MINOR-2) ---

// A manual work-type binding must survive a Classify run that would otherwise
// write a DIFFERENT work-type value — the regression case @critic flagged
// (a differing manual value would otherwise leave the entity with TWO values).
func TestClassify_ManualWorkTypeNotOverwritten(t *testing.T) {
	// Signals would normally produce work-type=research (READ+GREP > 50%).
	sig := &ActivitySignals{
		ToolTagCounts: map[string]int{"READ": 6, "GREP": 2, "EDIT": 2},
		TotalEvents:   10,
	}
	store := &fakeClassifierStore{
		records:  oneActiveRecord(),
		existing: []EntityTag{{TagKey: "work-type", TagValue: "bug", Category: "lifecycle", Source: "manual"}},
	}
	store, res := classifyFull(t, store, sig)

	// The classifier must NOT have called TagEntity for work-type at all.
	for _, a := range store.applied {
		if a.tagKey == "work-type" {
			t.Fatalf("classifier overwrote manual work-type: applied %+v", a)
		}
	}
	if !hasSkip(res.Skipped, "work-type") {
		t.Fatalf("expected work-type recorded in Skipped, got %+v", res.Skipped)
	}
}

// A manual phase binding pins the key, so phase=rework is skipped on re-entry.
func TestClassify_ManualPhaseNotOverwritten(t *testing.T) {
	records := []EventRecord{
		{State: StatusActive, SessionID: "abc123def456ghi", AgentName: "@worker", StartedAt: time.Unix(1747526100, 0).UTC()},
		{State: StatusReview, SessionID: "abc123def456ghi", AgentName: "@worker", StartedAt: time.Unix(1747526040, 0).UTC()},
	}
	sig := &ActivitySignals{ToolTagCounts: map[string]int{"EDIT": 1}, TotalEvents: 1}
	store := &fakeClassifierStore{
		records:  records,
		existing: []EntityTag{{TagKey: "phase", TagValue: "build", Category: "lifecycle", Source: "manual"}},
	}
	store, res := classifyFull(t, store, sig)

	if hasTag(store.applied, "phase", "rework") {
		t.Fatalf("classifier overwrote manual phase, got %+v", store.applied)
	}
	if !hasSkip(res.Skipped, "phase") {
		t.Fatalf("expected phase recorded in Skipped, got %+v", res.Skipped)
	}
}

// A prior source="classifier" binding must NOT pin the key: re-classification
// (including a changed value) stays allowed. Only source="manual" blocks.
func TestClassify_ClassifierBindingDoesNotBlock(t *testing.T) {
	sig := &ActivitySignals{
		ToolTagCounts: map[string]int{"READ": 6, "GREP": 2, "EDIT": 2},
		TotalEvents:   10,
	}
	store := &fakeClassifierStore{
		records:  oneActiveRecord(),
		existing: []EntityTag{{TagKey: "work-type", TagValue: "feature", Category: "lifecycle", Source: "classifier"}},
	}
	store, res := classifyFull(t, store, sig)

	if !hasTag(store.applied, "work-type", "research") {
		t.Fatalf("expected re-classification to work-type=research, got %+v", store.applied)
	}
	if hasSkip(res.Skipped, "work-type") {
		t.Fatalf("did not expect work-type skipped over a prior classifier binding, got %+v", res.Skipped)
	}
}

// The manual-keys set is built only from source="manual" bindings; a manual
// binding on a DIFFERENT key (here phase) does not pin work-type.
func TestClassify_ManualKeysSetIsKeyScoped(t *testing.T) {
	sig := &ActivitySignals{
		ToolTagCounts: map[string]int{"READ": 6, "GREP": 2, "EDIT": 2},
		TotalEvents:   10,
	}
	store := &fakeClassifierStore{
		records:  oneActiveRecord(),
		existing: []EntityTag{{TagKey: "phase", TagValue: "build", Category: "lifecycle", Source: "manual"}},
	}
	store, _ = classifyFull(t, store, sig)

	if !hasTag(store.applied, "work-type", "research") {
		t.Fatalf("manual phase should not pin work-type; expected research, got %+v", store.applied)
	}
}

// If GetEntityTags fails, the classifier falls back to unconditional tagging
// (best-effort) rather than suppressing all tags.
func TestClassify_GetEntityTagsErrorFallsBack(t *testing.T) {
	sig := &ActivitySignals{
		ToolTagCounts: map[string]int{"READ": 6, "GREP": 2, "EDIT": 2},
		TotalEvents:   10,
	}
	store := &fakeClassifierStore{
		records: oneActiveRecord(),
		tagsErr: errTags,
	}
	store, _ = classifyFull(t, store, sig)

	if !hasTag(store.applied, "work-type", "research") {
		t.Fatalf("expected unconditional tagging on GetEntityTags error, got %+v", store.applied)
	}
}

var errTags = errClassifierTest("get entity tags failed")

type errClassifierTest string

func (e errClassifierTest) Error() string { return string(e) }
