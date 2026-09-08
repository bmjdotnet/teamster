package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// fakeReader embeds a nil wms.Reader so any un-stubbed method call panics
// loudly rather than silently returning a zero value — same pattern
// wh2-required-tags-inherit's tags_inherit_test.go uses for its own fake.
type fakeReader struct {
	wms.Reader
	outcomes  map[string]*wms.Outcome
	workunits map[string][]*wms.WorkUnit    // keyed by outcome id
	children  map[string][]string           // keyed by outcome id
	journal   map[string][]wms.JournalEntry // keyed by "entityType:entityID"
	err       error                         // if set, every stubbed call below returns this
}

func (f *fakeReader) GetOutcome(ctx context.Context, id string) (*wms.Outcome, error) {
	if f.err != nil {
		return nil, f.err
	}
	o, ok := f.outcomes[id]
	if !ok {
		return nil, errors.New("fakeReader: outcome not found: " + id)
	}
	return o, nil
}

func (f *fakeReader) ListWorkUnits(ctx context.Context, outcomeID string) ([]*wms.WorkUnit, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.workunits[outcomeID], nil
}

func (f *fakeReader) GetOutcomeChildren(ctx context.Context, outcomeID string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.children[outcomeID], nil
}

func (f *fakeReader) GetJournalEntries(ctx context.Context, entityType, entityID string, limit int) ([]wms.JournalEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	entries := f.journal[entityType+":"+entityID]
	if limit > 0 && len(entries) > limit {
		return entries[:limit], nil
	}
	return entries, nil
}

// alwaysFullJournalReader simulates an entity whose journal history is
// larger than any fixed window — GetJournalEntries always returns exactly
// `limit` non-status rows, so isSweepParked's paging loop never terminates
// via the "fetched fewer than requested" signal and must hit the hardCap
// backstop instead.
type alwaysFullJournalReader struct {
	wms.Reader
}

func (alwaysFullJournalReader) GetJournalEntries(ctx context.Context, entityType, entityID string, limit int) ([]wms.JournalEntry, error) {
	entries := make([]wms.JournalEntry, limit)
	for i := range entries {
		entries[i] = wms.JournalEntry{Field: "sweep_evaluated", AgentID: sweepAgentID}
	}
	return entries, nil
}

// --- outcomeHasLiveDescendant ---

// TestOutcomeHasLiveDescendant_MAJOR1 reproduces the transitivity-break
// finding: a terminal child (C, done) hides a live grandchild WorkUnit —
// terminality is NOT transitive (CloseoutWarnings is advisory-only), so a
// walk that only checked C's own status would wrongly call G safe.
func TestOutcomeHasLiveDescendant_MAJOR1(t *testing.T) {
	f := &fakeReader{
		outcomes:  map[string]*wms.Outcome{"G": {ID: "G", Status: wms.StatusActive}, "C": {ID: "C", Status: wms.StatusDone}},
		children:  map[string][]string{"G": {"C"}},
		workunits: map[string][]*wms.WorkUnit{"C": {{ID: "wu1", Status: wms.StatusActive}}},
	}
	live, err := outcomeHasLiveDescendant(context.Background(), f, "G")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !live {
		t.Error("want live=true (C, though done, holds a live WorkUnit) — MAJOR-1 shape not caught")
	}
}

// TestOutcomeHasLiveDescendant_MAJORB reproduces the walk's own blind spot
// from its first fix: a non-terminal, CHILDLESS descendant Outcome (D) is
// invisible to a walk that only ever calls ListWorkUnits, never GetOutcome,
// on a visited node.
func TestOutcomeHasLiveDescendant_MAJORB(t *testing.T) {
	f := &fakeReader{
		outcomes: map[string]*wms.Outcome{
			"G": {ID: "G", Status: wms.StatusActive},
			"C": {ID: "C", Status: wms.StatusDone},
			"D": {ID: "D", Status: wms.StatusActive},
		},
		children: map[string][]string{"G": {"C"}, "C": {"D"}},
	}
	live, err := outcomeHasLiveDescendant(context.Background(), f, "G")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !live {
		t.Error("want live=true (D is non-terminal and childless) — MAJOR-B shape not caught")
	}
}

func TestOutcomeHasLiveDescendant_SafeFullyTerminalSubtree(t *testing.T) {
	f := &fakeReader{
		outcomes:  map[string]*wms.Outcome{"G": {ID: "G", Status: wms.StatusActive}, "C": {ID: "C", Status: wms.StatusDone}},
		children:  map[string][]string{"G": {"C"}},
		workunits: map[string][]*wms.WorkUnit{"C": {{ID: "wu1", Status: wms.StatusDone}}},
	}
	live, err := outcomeHasLiveDescendant(context.Background(), f, "G")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if live {
		t.Error("want live=false (fully terminal subtree), got true")
	}
}

// TestOutcomeHasLiveDescendant_ErrorFailsClosed is MAJOR-A/MINOR-C's
// regression guard: every store-error path must return (true, err), never
// (false, err) — a safety gate whose false answer licenses an irreversible
// abandon downstream.
func TestOutcomeHasLiveDescendant_ErrorFailsClosed(t *testing.T) {
	f := &fakeReader{err: errors.New("boom")}
	live, err := outcomeHasLiveDescendant(context.Background(), f, "G")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !live {
		t.Error("want live=true on a store error (fail closed), got false")
	}
}

// TestOutcomeHasLiveDescendant_SweepParkedDescendantExempt and its converse
// below are round 4's central safety property, exercised at the walk level
// (not just isLive standalone): a sweep-parked on_hold descendant does not
// block; a human-parked one does.
func TestOutcomeHasLiveDescendant_SweepParkedDescendantExempt(t *testing.T) {
	f := &fakeReader{
		outcomes: map[string]*wms.Outcome{"G": {ID: "G", Status: wms.StatusActive}, "C": {ID: "C", Status: wms.StatusOnHold}},
		children: map[string][]string{"G": {"C"}},
		journal:  map[string][]wms.JournalEntry{"outcome:C": {{Field: "status", AgentID: sweepAgentID}}},
	}
	live, err := outcomeHasLiveDescendant(context.Background(), f, "G")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if live {
		t.Error("want live=false (C is sweep-parked on_hold, exempt), got true")
	}
}

func TestOutcomeHasLiveDescendant_HumanParkedDescendantBlocks(t *testing.T) {
	f := &fakeReader{
		outcomes: map[string]*wms.Outcome{"G": {ID: "G", Status: wms.StatusActive}, "C": {ID: "C", Status: wms.StatusOnHold}},
		children: map[string][]string{"G": {"C"}},
		journal:  map[string][]wms.JournalEntry{"outcome:C": {{Field: "status", AgentID: "wms-close (alice)"}}},
	}
	live, err := outcomeHasLiveDescendant(context.Background(), f, "G")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !live {
		t.Error("want live=true (C is human-parked on_hold, blocks), got false")
	}
}

// TestOutcomeHasLiveDescendant_CycleTerminates proves termination rests on
// the visited-set alone, with no depth cap (MAJOR-A: an earlier depth cap
// failed open when tripped, for no safety benefit the visited-set didn't
// already provide).
func TestOutcomeHasLiveDescendant_CycleTerminates(t *testing.T) {
	f := &fakeReader{
		outcomes: map[string]*wms.Outcome{"A": {ID: "A", Status: wms.StatusDone}, "B": {ID: "B", Status: wms.StatusDone}},
		children: map[string][]string{"A": {"B"}, "B": {"A"}}, // A -> B -> A
	}
	live, err := outcomeHasLiveDescendant(context.Background(), f, "A")
	if err != nil {
		t.Fatalf("unexpected error (cycle should terminate cleanly): %v", err)
	}
	if live {
		t.Error("want live=false (both terminal, cycle self-terminates via visited-set), got true")
	}
}

// --- isLive ---

func TestIsLive_HumanOnHoldCountsAsLive(t *testing.T) {
	f := &fakeReader{journal: map[string][]wms.JournalEntry{"workunit:wu1": {{Field: "status", AgentID: "wms-close (alice)"}}}}
	live, err := isLive(context.Background(), f, wms.EntityWorkUnit, "wu1", wms.StatusOnHold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !live {
		t.Error("want live=true (human-parked on_hold still blocks) — MAJOR-2 shape")
	}
}

func TestIsLive_SweepOnHoldDoesNotCountAsLive(t *testing.T) {
	f := &fakeReader{journal: map[string][]wms.JournalEntry{"workunit:wu1": {{Field: "status", AgentID: sweepAgentID}}}}
	live, err := isLive(context.Background(), f, wms.EntityWorkUnit, "wu1", wms.StatusOnHold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if live {
		t.Error("want live=false (sweep-parked on_hold is exempt), got true")
	}
}

func TestIsLive_FailsClosedWhenSweepParkedCheckErrors(t *testing.T) {
	f := &fakeReader{err: errors.New("boom")}
	live, err := isLive(context.Background(), f, wms.EntityWorkUnit, "wu1", wms.StatusOnHold)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !live {
		t.Error("want live=true on error (fail closed), got false")
	}
}

func TestIsLive_TerminalNeverLive(t *testing.T) {
	f := &fakeReader{}
	live, err := isLive(context.Background(), f, wms.EntityWorkUnit, "wu1", wms.StatusDone)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if live {
		t.Error("want live=false (done is terminal), got true")
	}
}

// --- isSweepParked ---

func TestIsSweepParked_GenuinelyFreshEntity(t *testing.T) {
	f := &fakeReader{journal: map[string][]wms.JournalEntry{"workunit:wu1": {}}}
	parked, err := isSweepParked(context.Background(), f, wms.EntityWorkUnit, "wu1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parked {
		t.Error("want parked=false (no status row anywhere in history), got true")
	}
}

// TestIsSweepParked_FoundOnLaterPage proves the loop actually pages rather
// than giving up after the first fetch: 50 skip-tracking rows, then the
// true parking row at position 51 — invisible to a limit=50 fetch, only
// found once the loop doubles to limit=100.
func TestIsSweepParked_FoundOnLaterPage(t *testing.T) {
	entries := make([]wms.JournalEntry, 0, 51)
	for i := 0; i < 50; i++ {
		entries = append(entries, wms.JournalEntry{Field: "sweep_evaluated", AgentID: sweepAgentID})
	}
	entries = append(entries, wms.JournalEntry{Field: "status", AgentID: sweepAgentID})
	f := &fakeReader{journal: map[string][]wms.JournalEntry{"workunit:wu1": entries}}
	parked, err := isSweepParked(context.Background(), f, wms.EntityWorkUnit, "wu1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !parked {
		t.Error("want parked=true (status row found only on the doubled second fetch), got false")
	}
}

// TestIsSweepParked_SkipsAnyNonStatusFieldNotJustSweepEvaluated proves the
// scan is generic — it skips every row whose field isn't "status",
// regardless of which field it actually is, not only this sweep's own
// sweep_evaluated rows. wh2-required-tags-inherit's redelivery-in-review fix
// writes field="deliverable" journal rows, a second real source of
// non-status noise above a parking row (lead's note, VERIFY.md §7) — this
// mixes both kinds in one history to prove neither confuses the scan.
func TestIsSweepParked_SkipsAnyNonStatusFieldNotJustSweepEvaluated(t *testing.T) {
	entries := []wms.JournalEntry{
		{Field: "deliverable", AgentID: "some-agent"},
		{Field: "sweep_evaluated", AgentID: sweepAgentID},
		{Field: "deliverable", AgentID: "some-agent"},
		{Field: "status", AgentID: sweepAgentID},
	}
	f := &fakeReader{journal: map[string][]wms.JournalEntry{"workunit:wu1": entries}}
	parked, err := isSweepParked(context.Background(), f, wms.EntityWorkUnit, "wu1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !parked {
		t.Error("want parked=true (the status row is found past a mix of deliverable and sweep_evaluated rows), got false")
	}
}

func TestIsSweepParked_HumanRowFoundOnLaterPage(t *testing.T) {
	entries := make([]wms.JournalEntry, 0, 51)
	for i := 0; i < 50; i++ {
		entries = append(entries, wms.JournalEntry{Field: "sweep_evaluated", AgentID: sweepAgentID})
	}
	entries = append(entries, wms.JournalEntry{Field: "status", AgentID: "wms-close (alice)"})
	f := &fakeReader{journal: map[string][]wms.JournalEntry{"workunit:wu1": entries}}
	parked, err := isSweepParked(context.Background(), f, wms.EntityWorkUnit, "wu1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parked {
		t.Error("want parked=false (the found status row is human-authored), got true")
	}
}

// TestIsSweepParked_HardCapExceeded proves the window-truncation backstop
// fires rather than ever silently guessing "not sweep-parked" — round-5's
// fix over the round-4-addendum-2 fixed-limit version.
func TestIsSweepParked_HardCapExceeded(t *testing.T) {
	f := alwaysFullJournalReader{}
	parked, err := isSweepParked(context.Background(), f, wms.EntityWorkUnit, "wu1")
	if !errors.Is(err, errJournalWindowTruncated) {
		t.Errorf("want errJournalWindowTruncated, got %v", err)
	}
	if parked {
		t.Error("want parked=false alongside the sentinel (isLive is what converts this to fail-closed live=true), got true")
	}
}

// --- hasDeliverable ---

func TestHasDeliverable(t *testing.T) {
	f := &fakeReader{}
	// ListDeliverables isn't stubbed on fakeReader (delegates to the
	// embedded nil wms.Reader), so route this one through a tiny adapter
	// instead of extending fakeReader for a single method.
	del := &deliverableReader{entries: map[string]bool{"workunit:wu1": true}}
	got, err := hasDeliverable(context.Background(), del, wms.EntityWorkUnit, "wu1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("want true (a deliverable exists), got false")
	}
	got, err = hasDeliverable(context.Background(), del, wms.EntityWorkUnit, "wu2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("want false (no deliverable), got true")
	}
	_ = f
}

type deliverableReader struct {
	wms.Reader
	entries map[string]bool
}

func (d *deliverableReader) ListDeliverables(ctx context.Context, entityType, entityID string, limit int) ([]wms.Deliverable, error) {
	if d.entries[entityType+":"+entityID] {
		return []wms.Deliverable{{ID: 1}}, nil
	}
	return nil, nil
}

// --- check-then-act re-verification (@auditor MAJOR, 2026-09-02) ---
//
// No status write in wms_review_sweep.go was guarded by a conditional
// UPDATE (UpdateWorkUnitStatus/UpdateOutcomeStatus carry a plain
// `WHERE id = ?`), so every disposition loop now re-fetches the entity
// immediately before writing and re-applies its candidacy predicate. These
// tests model the race directly: call the predicate with the entity AS IT
// WOULD BE RE-FETCHED, after a mutation happened between the initial list
// query and this entity's turn — the same shape a concurrent human action
// would produce, without needing real concurrency to reproduce it.

func TestStillReviewStage1Candidate(t *testing.T) {
	threshold := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC) // stage1Threshold = now - 168h
	stillIdle := threshold.Add(-time.Hour)                    // updated before the threshold: still idle
	touchedSince := threshold.Add(time.Hour)                  // updated after the threshold: touched since listing

	cases := []struct {
		name string
		wu   *wms.WorkUnit
		want bool
	}{
		{"still a candidate: review, still idle", &wms.WorkUnit{Status: wms.StatusReview, UpdatedAt: stillIdle}, true},
		{"race: reactivated to active between list and disposal", &wms.WorkUnit{Status: wms.StatusActive, UpdatedAt: stillIdle}, false},
		{"race: human parked it on_hold between list and disposal", &wms.WorkUnit{Status: wms.StatusOnHold, UpdatedAt: stillIdle}, false},
		{"race: delivered and moved to done between list and disposal", &wms.WorkUnit{Status: wms.StatusDone, UpdatedAt: stillIdle}, false},
		{"race: touched (updated_at bumped) between list and disposal, status unchanged", &wms.WorkUnit{Status: wms.StatusReview, UpdatedAt: touchedSince}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stillReviewStage1Candidate(c.wu, threshold); got != c.want {
				t.Errorf("stillReviewStage1Candidate() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestStillOutcomeStage1Candidate(t *testing.T) {
	threshold := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	stillIdle := threshold.Add(-time.Hour)
	touchedSince := threshold.Add(time.Hour)

	cases := []struct {
		name string
		o    *wms.Outcome
		want bool
	}{
		{"still a candidate: active, still idle", &wms.Outcome{Status: wms.StatusActive, UpdatedAt: stillIdle}, true},
		{"race: closed done between list and disposal", &wms.Outcome{Status: wms.StatusDone, UpdatedAt: stillIdle}, false},
		{"race: closed abandoned between list and disposal", &wms.Outcome{Status: wms.StatusAbandoned, UpdatedAt: stillIdle}, false},
		{"race: human parked it on_hold between list and disposal", &wms.Outcome{Status: wms.StatusOnHold, UpdatedAt: stillIdle}, false},
		{"race: touched (updated_at bumped) between list and disposal, status unchanged", &wms.Outcome{Status: wms.StatusActive, UpdatedAt: touchedSince}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stillOutcomeStage1Candidate(c.o, threshold); got != c.want {
				t.Errorf("stillOutcomeStage1Candidate() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestContainsWorkUnitID/TestContainsOutcomeID model Stage 2's re-verification:
// the loop re-runs ListOnHoldAbandonCandidateWorkUnits/...Outcomes immediately
// before disposing of each entity and checks membership in the FRESH result —
// "id present in the initial list, absent from the re-run" is exactly what a
// race (reactivation, a later journal/interval/deliverable row, or simply
// aging out of the rescue window some other way) would produce.
func TestContainsWorkUnitID(t *testing.T) {
	fresh := []*wms.WorkUnit{{ID: "wu-a"}, {ID: "wu-c"}}
	if !containsWorkUnitID(fresh, "wu-a") {
		t.Error("want true: wu-a is still in the re-run candidate list")
	}
	if containsWorkUnitID(fresh, "wu-b") {
		t.Error("want false: wu-b was in the initial list but is gone from the re-run (race) — must not be treated as still a candidate")
	}
	if containsWorkUnitID(nil, "wu-a") {
		t.Error("want false on an empty re-run result")
	}
}

func TestContainsOutcomeID(t *testing.T) {
	fresh := []*wms.Outcome{{ID: "oc-a"}, {ID: "oc-c"}}
	if !containsOutcomeID(fresh, "oc-a") {
		t.Error("want true: oc-a is still in the re-run candidate list")
	}
	if containsOutcomeID(fresh, "oc-b") {
		t.Error("want false: oc-b was in the initial list but is gone from the re-run (race) — must not be treated as still a candidate")
	}
	if containsOutcomeID(nil, "oc-a") {
		t.Error("want false on an empty re-run result")
	}
}
