package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// fakeGCStore implements the narrow interfaces countStaleEntities,
// closeStaleEntities and closeStaleEntitiesAndDrain need, without a live
// MySQL fixture.
type fakeGCStore struct {
	outcomes  []*wms.Outcome
	workUnits map[string][]*wms.WorkUnit
	// children maps an outcome ID to its direct child outcome IDs, mirroring
	// GetOutcomeChildren — lets a test exercise outcomeHasLiveDescendant's
	// walk into nested child Outcomes, not just an outcome's own WorkUnits.
	children map[string][]string

	outcomeStatus  map[string]string
	workUnitStatus map[string]string
	journal        []wms.JournalEntry

	intervals []*fakeInterval

	// closeIntervalsErr, when set, makes CloseIntervalsOnTerminalEntities
	// fail — models a real store error (e.g. a lost DB connection) so the
	// drain-error branch in closeStaleEntitiesAndDrain/runWMSGC can be
	// exercised without a live DB failure.
	closeIntervalsErr error
}

// fakeInterval is a pointer-held fixture so CloseIntervalsOnTerminalEntities
// can mutate endedAt in place and a test can observe the mutation on the
// same pointer it seeded the store with.
type fakeInterval struct {
	entityType string
	entityID   string
	endedAt    bool
}

// CloseIntervalsOnTerminalEntities mirrors the real store method's SQL
// condition (store.go: `e.status IN ('done', 'abandoned')`, i.e.
// wms.IsTerminal) exactly: close every still-open interval whose entity is
// currently terminal, checking the *current* status — the update maps
// UpdateOutcomeStatus/UpdateWorkUnitStatus just wrote, not the original
// fixture's Status field — so a call made after closeStaleEntities in the
// same run sees entities that run itself just abandoned.
func (f *fakeGCStore) CloseIntervalsOnTerminalEntities(_ context.Context) (int64, error) {
	if f.closeIntervalsErr != nil {
		return 0, f.closeIntervalsErr
	}
	var n int64
	for _, iv := range f.intervals {
		if iv.endedAt {
			continue
		}
		if wms.IsTerminal(iv.entityType, f.currentStatus(iv.entityType, iv.entityID)) {
			iv.endedAt = true
			n++
		}
	}
	return n, nil
}

func (f *fakeGCStore) currentStatus(entityType, id string) string {
	switch entityType {
	case wms.EntityOutcome:
		if s, ok := f.outcomeStatus[id]; ok {
			return s
		}
		for _, o := range f.outcomes {
			if o.ID == id {
				return o.Status
			}
		}
	case wms.EntityWorkUnit:
		if s, ok := f.workUnitStatus[id]; ok {
			return s
		}
		for _, wus := range f.workUnits {
			for _, wu := range wus {
				if wu.ID == id {
					return wu.Status
				}
			}
		}
	}
	return ""
}

func (f *fakeGCStore) ListOutcomes(_ context.Context, _ string, _ map[string]string, _ string, _ string) ([]*wms.Outcome, error) {
	return f.outcomes, nil
}

// ListWorkUnits overlays any status already written by UpdateWorkUnitStatus
// onto the seeded fixture — mirroring a real store's read-after-write
// consistency. Without this, outcomeHasLiveDescendant's own fresh
// ListWorkUnits call (made from within the SAME closeStaleEntities pass that
// just abandoned a stale direct child) would see the pre-abandon fixture
// status and misreport that WorkUnit as still live, wrongly blocking its
// parent Outcome from closing in the same run.
func (f *fakeGCStore) ListWorkUnits(_ context.Context, outcomeID string) ([]*wms.WorkUnit, error) {
	wus := f.workUnits[outcomeID]
	out := make([]*wms.WorkUnit, len(wus))
	for i, wu := range wus {
		cp := *wu
		if s, ok := f.workUnitStatus[wu.ID]; ok {
			cp.Status = s
		}
		out[i] = &cp
	}
	return out, nil
}

// GetOutcome overlays any status already written by UpdateOutcomeStatus onto
// the seeded fixture, same read-after-write rationale as ListWorkUnits above.
func (f *fakeGCStore) GetOutcome(_ context.Context, id string) (*wms.Outcome, error) {
	for _, o := range f.outcomes {
		if o.ID == id {
			cp := *o
			if s, ok := f.outcomeStatus[id]; ok {
				cp.Status = s
			}
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("fakeGCStore: outcome %s not found", id)
}

// GetOutcomeChildren returns id's direct child outcome IDs from the fixture
// — the edge outcomeHasLiveDescendant's walk follows into nested Outcomes.
func (f *fakeGCStore) GetOutcomeChildren(_ context.Context, outcomeID string) ([]string, error) {
	return f.children[outcomeID], nil
}

// GetJournalEntries returns entries matching entityType/entityID, newest
// first (matching the real store's ORDER BY created_at DESC semantics),
// capped at limit — only exercised by isSweepParked when an entity's status
// is on_hold, which none of this file's fixtures use today.
func (f *fakeGCStore) GetJournalEntries(_ context.Context, entityType, entityID string, limit int) ([]wms.JournalEntry, error) {
	var matched []wms.JournalEntry
	for i := len(f.journal) - 1; i >= 0; i-- {
		e := f.journal[i]
		if e.EntityType == entityType && e.EntityID == entityID {
			matched = append(matched, e)
			if len(matched) >= limit {
				break
			}
		}
	}
	return matched, nil
}

func (f *fakeGCStore) UpdateOutcomeStatus(_ context.Context, id, status string) error {
	if f.outcomeStatus == nil {
		f.outcomeStatus = map[string]string{}
	}
	f.outcomeStatus[id] = status
	return nil
}

func (f *fakeGCStore) UpdateWorkUnitStatus(_ context.Context, id, status string) error {
	if f.workUnitStatus == nil {
		f.workUnitStatus = map[string]string{}
	}
	f.workUnitStatus[id] = status
	return nil
}

func (f *fakeGCStore) WriteJournalEntry(_ context.Context, entry wms.JournalEntry) error {
	f.journal = append(f.journal, entry)
	return nil
}

// TestCloseStaleEntities_ClosesAsAbandonedNotDone is a regression test for
// WP9/R3: gc must never inflate completion metrics by closing stale,
// never-delivered work as "done". It must set status=abandoned and record
// an audit-trail journal row (WP10 path 3), and must NOT write a
// resolution tag (the status now carries that meaning by itself).
func TestCloseStaleEntities_ClosesAsAbandonedNotDone(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-10 * 24 * time.Hour)
	threshold := now.Add(-7 * 24 * time.Hour)

	f := &fakeGCStore{
		outcomes: []*wms.Outcome{
			{ID: "oc-1", Title: "stale outcome", Status: wms.StatusActive, UpdatedAt: stale},
		},
		workUnits: map[string][]*wms.WorkUnit{
			"oc-1": {
				{ID: "wu-1", OutcomeID: "oc-1", Title: "stale wu", Status: wms.StatusActive, UpdatedAt: stale},
			},
		},
	}

	closed := closeStaleEntities(context.Background(), f, threshold, "7d", "gc-host")

	if closed != 2 {
		t.Fatalf("closed = %d, want 2 (one workunit, one outcome)", closed)
	}
	if got := f.workUnitStatus["wu-1"]; got != wms.StatusAbandoned {
		t.Errorf("wu-1 status = %q, want %q", got, wms.StatusAbandoned)
	}
	if got := f.outcomeStatus["oc-1"]; got != wms.StatusAbandoned {
		t.Errorf("oc-1 status = %q, want %q", got, wms.StatusAbandoned)
	}

	if len(f.journal) != 2 {
		t.Fatalf("journal entries = %d, want 2", len(f.journal))
	}
	for _, entry := range f.journal {
		if entry.NewValue != wms.StatusAbandoned {
			t.Errorf("journal entry NewValue = %q, want %q", entry.NewValue, wms.StatusAbandoned)
		}
		if entry.OldValue != wms.StatusActive {
			t.Errorf("journal entry OldValue = %q, want %q", entry.OldValue, wms.StatusActive)
		}
		if !strings.Contains(entry.Notes, "wms gc") || !strings.Contains(entry.Notes, "7d") {
			t.Errorf("journal entry Notes = %q, want it to name `wms gc` and the threshold 7d", entry.Notes)
		}
		if entry.Host != "gc-host" {
			t.Errorf("journal entry Host = %q, want %q (redteam MINOR 4: gc runs unattended from a timer, host is the only identity available)", entry.Host, "gc-host")
		}
	}
}

// TestCloseStaleEntities_OutcomeWithActiveChildNotClosed confirms the
// hasActiveChild guard survives the IsTerminal rewrite: an outcome must not
// be closed while it still has a non-terminal (including on_hold) child.
func TestCloseStaleEntities_OutcomeWithActiveChildNotClosed(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-10 * 24 * time.Hour)
	fresh := now
	threshold := now.Add(-7 * 24 * time.Hour)

	f := &fakeGCStore{
		outcomes: []*wms.Outcome{
			{ID: "oc-1", Title: "stale outcome", Status: wms.StatusActive, UpdatedAt: stale},
		},
		workUnits: map[string][]*wms.WorkUnit{
			"oc-1": {
				{ID: "wu-1", OutcomeID: "oc-1", Title: "still active", Status: wms.StatusActive, UpdatedAt: fresh},
			},
		},
	}

	closed := closeStaleEntities(context.Background(), f, threshold, "7d", "gc-host")

	if closed != 0 {
		t.Fatalf("closed = %d, want 0 (outcome has a non-stale active child)", closed)
	}
	if _, ok := f.outcomeStatus["oc-1"]; ok {
		t.Errorf("oc-1 status was updated, want untouched")
	}
}

// TestCloseStaleEntities_AlreadyAbandonedNotReclosed is a regression test
// for the IsTerminal cleanup: an entity that a prior gc run already closed
// as abandoned must not be treated as a fresh stale candidate.
func TestCloseStaleEntities_AlreadyAbandonedNotReclosed(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-30 * 24 * time.Hour)
	threshold := now.Add(-7 * 24 * time.Hour)

	f := &fakeGCStore{
		outcomes: []*wms.Outcome{
			{ID: "oc-1", Title: "already abandoned", Status: wms.StatusAbandoned, UpdatedAt: stale},
		},
		workUnits: map[string][]*wms.WorkUnit{
			"oc-1": {
				{ID: "wu-1", OutcomeID: "oc-1", Title: "already abandoned", Status: wms.StatusAbandoned, UpdatedAt: stale},
			},
		},
	}

	closed := closeStaleEntities(context.Background(), f, threshold, "7d", "gc-host")

	if closed != 0 {
		t.Fatalf("closed = %d, want 0 (both entities are already terminal)", closed)
	}
	staleOutcomes, staleWUs, candidates, err := countStaleEntities(context.Background(), f, threshold)
	if err != nil {
		t.Fatalf("countStaleEntities: %v", err)
	}
	if staleOutcomes != 0 || staleWUs != 0 {
		t.Errorf("countStaleEntities = (%d, %d), want (0, 0) for already-abandoned entities", staleOutcomes, staleWUs)
	}
	if len(candidates) != 0 {
		t.Errorf("countStaleEntities candidates = %d, want 0 for already-abandoned entities", len(candidates))
	}
}

// TestCloseStaleEntities_OutcomeWithLiveNestedGrandchildNotClosed is the
// regression test for the descendant-walk gap: gc's hasActiveChild guard
// only ever inspected an Outcome's own DIRECT WorkUnits, so a stale root
// Outcome whose only "child" is another Outcome — even one that is itself
// terminal — got abandoned without anyone noticing that terminal child
// Outcome still has a live WorkUnit nested under it. closeStaleEntities must
// now walk the full subtree (via outcomeHasLiveDescendant, shared with wms
// review-sweep) and skip the root.
func TestCloseStaleEntities_OutcomeWithLiveNestedGrandchildNotClosed(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-10 * 24 * time.Hour)
	fresh := now
	threshold := now.Add(-7 * 24 * time.Hour)

	f := &fakeGCStore{
		outcomes: []*wms.Outcome{
			{ID: "oc-root", Title: "stale root, no direct WUs", Status: wms.StatusActive, UpdatedAt: stale},
			{ID: "oc-child", Title: "terminal child outcome", Status: wms.StatusDone, UpdatedAt: stale},
		},
		workUnits: map[string][]*wms.WorkUnit{
			"oc-root":  {},
			"oc-child": {{ID: "wu-grandchild", OutcomeID: "oc-child", Title: "still active", Status: wms.StatusActive, UpdatedAt: fresh}},
		},
		children: map[string][]string{
			"oc-root": {"oc-child"},
		},
	}

	closed := closeStaleEntities(context.Background(), f, threshold, "7d", "gc-host")

	if closed != 0 {
		t.Fatalf("closed = %d, want 0 (oc-root has a live nested grandchild WorkUnit under a terminal child Outcome)", closed)
	}
	if _, ok := f.outcomeStatus["oc-root"]; ok {
		t.Errorf("oc-root status was updated, want untouched")
	}

	staleOutcomes, _, candidates, err := countStaleEntities(context.Background(), f, threshold)
	if err != nil {
		t.Fatalf("countStaleEntities: %v", err)
	}
	if staleOutcomes != 0 {
		t.Errorf("countStaleEntities staleOutcomes = %d, want 0 (dry-run preview must not overclaim what closeStaleEntities would actually abandon)", staleOutcomes)
	}
	for _, c := range candidates {
		if c.id == "oc-root" {
			t.Errorf("countStaleEntities listed oc-root as a candidate, want it excluded (live nested descendant)")
		}
	}
}

// TestCloseStaleEntitiesAndDrain_ClosesIntervalOfNewlyAbandonedEntity is the
// LF-CLI-2 regression test (VERIFY.md Addendum §E): a stale workunit's own
// open interval must be closed in the SAME gc run that abandons it, not
// left for some later run's phase 1 to catch. A sibling workunit that is
// still fresh (and so keeps the outcome from closing too) must be
// completely untouched — neither its status nor its interval.
func TestCloseStaleEntitiesAndDrain_ClosesIntervalOfNewlyAbandonedEntity(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-10 * 24 * time.Hour)
	fresh := now
	threshold := now.Add(-7 * 24 * time.Hour)

	staleInterval := &fakeInterval{entityType: wms.EntityWorkUnit, entityID: "wu-stale"}
	freshInterval := &fakeInterval{entityType: wms.EntityWorkUnit, entityID: "wu-fresh"}

	f := &fakeGCStore{
		outcomes: []*wms.Outcome{
			{ID: "oc-1", Title: "parent", Status: wms.StatusActive, UpdatedAt: fresh},
		},
		workUnits: map[string][]*wms.WorkUnit{
			"oc-1": {
				{ID: "wu-stale", OutcomeID: "oc-1", Title: "stale wu", Status: wms.StatusActive, UpdatedAt: stale},
				{ID: "wu-fresh", OutcomeID: "oc-1", Title: "fresh wu", Status: wms.StatusActive, UpdatedAt: fresh},
			},
		},
		intervals: []*fakeInterval{staleInterval, freshInterval},
	}

	closed, drained, err := closeStaleEntitiesAndDrain(context.Background(), f, threshold, "7d", "gc-host")
	if err != nil {
		t.Fatalf("closeStaleEntitiesAndDrain: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 (only wu-stale is idle past threshold)", closed)
	}
	if drained != 1 {
		t.Fatalf("drained = %d, want 1 (wu-stale's interval, closed in this same run)", drained)
	}
	if !staleInterval.endedAt {
		t.Error("wu-stale's interval was not closed — LF-CLI-2 not fixed")
	}
	if freshInterval.endedAt {
		t.Error("wu-fresh's interval was closed, want untouched (its entity never went terminal)")
	}
	if got := f.workUnitStatus["wu-stale"]; got != wms.StatusAbandoned {
		t.Errorf("wu-stale status = %q, want %q", got, wms.StatusAbandoned)
	}
	if _, ok := f.workUnitStatus["wu-fresh"]; ok {
		t.Error("wu-fresh status was updated, want untouched")
	}
}

// TestCloseIntervalsOnTerminalEntities_IdempotentOnSecondCall pins the
// fake's fidelity to the real store method's idempotency predicate
// (`ended_at IS NULL` in the SQL — read, not assumed): once an interval's
// endedAt is set, a second call must find nothing new to close.
//
// This calls the fake's CloseIntervalsOnTerminalEntities directly rather
// than going through closeStaleEntitiesAndDrain twice on purpose: the
// fake's ListOutcomes/ListWorkUnits return the seeded fixtures verbatim and
// never consult the status-write maps the way CloseIntervalsOnTerminalEntities
// does, so driving closeStaleEntities a second time against this fake would
// re-evaluate staleness against the *original* fixture status and
// re-abandon it — an artifact of the double's read/write asymmetry, not a
// production bug (the real store's ListOutcomes/ListWorkUnits read live DB
// state on every call). The actual "gc run twice, drains nothing the second
// time" guarantee is confirmed live: @cli-fire's throwaway-schema repro reported
// "nothing to collect" on a second `--confirm` run (redteam review).
func TestCloseIntervalsOnTerminalEntities_IdempotentOnSecondCall(t *testing.T) {
	iv := &fakeInterval{entityType: wms.EntityWorkUnit, entityID: "wu-1"}
	f := &fakeGCStore{
		workUnits: map[string][]*wms.WorkUnit{
			"oc-1": {{ID: "wu-1", OutcomeID: "oc-1", Status: wms.StatusAbandoned}},
		},
		intervals: []*fakeInterval{iv},
	}

	n1, err := f.CloseIntervalsOnTerminalEntities(context.Background())
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if n1 != 1 {
		t.Fatalf("first call drained = %d, want 1", n1)
	}

	n2, err := f.CloseIntervalsOnTerminalEntities(context.Background())
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second call drained = %d, want 0 (already closed, must be idempotent)", n2)
	}
}

// TestCloseStaleEntitiesAndDrain_PropagatesDrainErrorWithEntityCount
// exercises the drain-error branch itself (flagged by @cli-fire's
// validation as read-but-not-run): when CloseIntervalsOnTerminalEntities
// fails, closeStaleEntitiesAndDrain must still propagate the error AND
// report the correct closedEntities count — phase 4's status/journal writes
// already committed before the drain call failed, and runWMSGC's error path
// depends on this count to report that durable work rather than discard it
// (redteam review nit 1).
func TestCloseStaleEntitiesAndDrain_PropagatesDrainErrorWithEntityCount(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-10 * 24 * time.Hour)
	threshold := now.Add(-7 * 24 * time.Hour)
	wantErr := errors.New("boom: lost connection")

	f := &fakeGCStore{
		outcomes: []*wms.Outcome{
			{ID: "oc-1", Title: "stale outcome", Status: wms.StatusActive, UpdatedAt: stale},
		},
		workUnits: map[string][]*wms.WorkUnit{
			"oc-1": {
				{ID: "wu-1", OutcomeID: "oc-1", Title: "stale wu", Status: wms.StatusActive, UpdatedAt: stale},
			},
		},
		closeIntervalsErr: wantErr,
	}

	closed, drained, err := closeStaleEntitiesAndDrain(context.Background(), f, threshold, "7d", "gc-host")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if drained != 0 {
		t.Errorf("drained = %d, want 0 on a drain error", drained)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2 — phase 4's writes happened before the drain failed and must still be reported", closed)
	}
	if got := f.workUnitStatus["wu-1"]; got != wms.StatusAbandoned {
		t.Errorf("wu-1 status = %q, want %q (durable despite the drain error)", got, wms.StatusAbandoned)
	}
	if got := f.outcomeStatus["oc-1"]; got != wms.StatusAbandoned {
		t.Errorf("oc-1 status = %q, want %q (durable despite the drain error)", got, wms.StatusAbandoned)
	}
}

// TestConfirmGCInteractive_RefusesNonTTY pins the refusal-before-prompt
// property: confirmGCInteractive must check stdin's TTY-ness before it ever
// prints the blast-radius warning or attempts to read a response, so a
// piped/redirected caller (a script, a timer, another agent) is refused
// outright rather than hanging on a read. Stdin is swapped to /dev/null for
// the duration of the test — /dev/null is never a TTY, regardless of
// whether the enclosing `go test` invocation itself happens to have one.
func TestConfirmGCInteractive_RefusesNonTTY(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()

	origStdin := os.Stdin
	os.Stdin = devNull
	defer func() { os.Stdin = origStdin }()

	gotErr := confirmGCInteractive(1, 2)
	if gotErr == nil {
		t.Fatal("confirmGCInteractive returned nil, want a refusal error for non-TTY stdin")
	}
	if !strings.Contains(gotErr.Error(), "not a TTY") {
		t.Errorf("err = %q, want it to mention TTY refusal", gotErr.Error())
	}
}
