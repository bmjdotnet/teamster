package wms

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// Tests for three MCP-handler-level behaviors implemented in wms.go:
//
//   - The WorkUnit-under-terminal-Outcome create guard (ToolCreateWorkUnit;
//     operator ruling, ANALYSIS.md §5/§2.1.3).
//   - The permanent last-sibling-terminal nudge (closeoutNudge, wired into
//     wms_updateStatus and ToolUpdateWorkUnitStatus; operator ruling,
//     ANALYSIS.md §5/§1.2, superseding WP7's "crutch" framing).
//   - WP10 path 1/5 notes population (wms_updateStatus,
//     ToolUpdateWorkUnitStatus, wms_deliverResult, wms_claimWorkUnit).
//   - The sixth WP10 mutation path found by @skills/confirmed by the lead:
//     wms_createOutcome/wms_createWorkUnit with a non-default initial status
//     is an implicit pending→status transition that must also reach the
//     engine (ToolCreateOutcome, ToolCreateWorkUnit).
//
// The notes tests need a real engine wired to a JournalObserver — noopEngine
// (used by the rest of this package's tests) is a no-op and never reaches
// the journal, so those tests build their own engine via realEngineFor.

// realEngineFor wires a real *wms.EngineImpl with a JournalObserver attached,
// for tests asserting on wms_journal rows. Distinct from noopEngine (used
// everywhere else in this package for handler-only tests) because the notes
// tests need OnStatusChange to actually reach JournalObserver.
func realEngineFor(store wms.Store) wms.Engine {
	eng := wms.NewEngine(store, nil)
	eng.AddObserver(wms.NewJournalObserver(store))
	return eng
}

// callWithEngine invokes HandleToolCall against a caller-supplied engine
// (see realEngineFor), for tests that need OnStatusChange's side effects.
func callWithEngine(t *testing.T, store wms.Store, eng wms.Engine, name string, args map[string]interface{}) (Result, *CallError) {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{"name": name, "arguments": args})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return HandleToolCall(store, eng, raw)
}

// latestJournalNotes returns the `notes` column of the wms_journal row for
// the given entity, when exactly one status row exists for it — used by
// tests that write a single transition and then read it back.
func latestJournalNotes(t *testing.T, store wms.Store, entityType, entityID string) string {
	t.Helper()
	entries, err := store.GetJournalEntries(context.Background(), entityType, entityID, 1)
	if err != nil {
		t.Fatalf("GetJournalEntries(%s/%s): %v", entityType, entityID, err)
	}
	if len(entries) == 0 {
		t.Fatalf("no journal entries for %s/%s", entityType, entityID)
	}
	return entries[0].Notes
}

// journalNotesForTransition returns the `notes` column of the wms_journal
// row matching a specific old→new status transition for one entity. Needed
// whenever a test writes more than one status row for the same entity:
// wms_journal.created_at is second-granularity, so GetJournalEntries'
// `ORDER BY created_at DESC` does not reliably order two rows written in the
// same second — picking index 0 as "the latest" is not safe. Matching on
// the transition itself is unambiguous as long as the test's transitions are
// pairwise distinct, which they are here.
func journalNotesForTransition(t *testing.T, store wms.Store, entityType, entityID, oldStatus, newStatus string) string {
	t.Helper()
	entries, err := store.GetJournalEntries(context.Background(), entityType, entityID, 20)
	if err != nil {
		t.Fatalf("GetJournalEntries(%s/%s): %v", entityType, entityID, err)
	}
	for _, e := range entries {
		if e.Field == "status" && e.OldValue == oldStatus && e.NewValue == newStatus {
			return e.Notes
		}
	}
	t.Fatalf("no journal row for %s/%s transition %s→%s among %d entries", entityType, entityID, oldStatus, newStatus, len(entries))
	return ""
}

// --- WorkUnit-under-terminal-Outcome create guard ---

// TestCreateWorkUnit_RejectsTerminalOutcome: creating a WorkUnit under an
// Outcome already in a terminal status (done) is rejected with a message
// naming the outcome, its status, and the reopen edge — no auto-reopen, no
// silent creation under a closed outcome.
func TestCreateWorkUnit_RejectsTerminalOutcome(t *testing.T) {
	store, oid := newStewardStore(t)
	if _, ce := call(t, store, ToolUpdateOutcomeStatus, map[string]interface{}{"id": oid, "status": wms.StatusActive}); ce != nil {
		t.Fatalf("outcome pending->active: %v", ce)
	}
	if _, ce := call(t, store, ToolUpdateOutcomeStatus, map[string]interface{}{"id": oid, "status": wms.StatusDone}); ce != nil {
		t.Fatalf("outcome active->done: %v", ce)
	}

	_, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{
		"id": "wu-under-terminal", "title": "should be rejected", "outcomeID": oid,
	})
	if ce == nil {
		t.Fatal("createWorkUnit under a done outcome returned no error")
	}
	if !strings.Contains(ce.Message, oid) {
		t.Errorf("error %q does not name the outcome", ce.Message)
	}
	if !strings.Contains(ce.Message, wms.StatusDone) {
		t.Errorf("error %q does not name the outcome's status", ce.Message)
	}
	if !strings.Contains(ce.Message, "wms_updateOutcomeStatus") {
		t.Errorf("error %q does not point at the reopen edge", ce.Message)
	}
	if _, err := store.GetWorkUnit(context.Background(), "wu-under-terminal"); err == nil {
		t.Error("rejected work unit was created anyway")
	}
}

// TestCreateWorkUnit_AllowsOpenOutcome: the guard does not fire for a
// non-terminal Outcome — the ordinary create path is unaffected.
func TestCreateWorkUnit_AllowsOpenOutcome(t *testing.T) {
	store, oid := newStewardStore(t)
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{
		"id": "wu-under-open", "title": "should succeed", "outcomeID": oid,
	}); ce != nil {
		t.Fatalf("createWorkUnit under a pending outcome: %v", ce)
	}
	if _, err := store.GetWorkUnit(context.Background(), "wu-under-open"); err != nil {
		t.Errorf("GetWorkUnit after create: %v", err)
	}
}

// --- Permanent last-sibling-terminal nudge ---

// TestCloseoutNudge_ViaToolUpdateWorkUnitStatus: silent while a sibling is
// still open, fires exactly once the last sibling reaches done, and leaves
// the Outcome's own status untouched (WP7 already removed the auto-close
// cascade — this nudge is the deliberate replacement signal, not a revival
// of the cascade).
func TestCloseoutNudge_ViaToolUpdateWorkUnitStatus(t *testing.T) {
	store, oid := newStewardStore(t)
	if _, ce := call(t, store, ToolUpdateOutcomeStatus, map[string]interface{}{"id": oid, "status": wms.StatusActive}); ce != nil {
		t.Fatalf("outcome pending->active: %v", ce)
	}
	for _, id := range []string{"wu-nudge-1", "wu-nudge-2"} {
		if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{"id": id, "title": id, "outcomeID": oid}); ce != nil {
			t.Fatalf("createWorkUnit %s: %v", id, ce)
		}
		if _, ce := call(t, store, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": id, "status": wms.StatusActive}); ce != nil {
			t.Fatalf("workunit %s pending->active: %v", id, ce)
		}
	}

	r1, ce := call(t, store, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": "wu-nudge-1", "status": wms.StatusDone})
	if ce != nil {
		t.Fatalf("workunit 1 active->done: %v", ce)
	}
	if got := resultText(t, r1); strings.Contains(got, "does not close automatically") {
		t.Errorf("nudge fired with a sibling still open: %q", got)
	}

	r2, ce := call(t, store, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": "wu-nudge-2", "status": wms.StatusDone})
	if ce != nil {
		t.Fatalf("workunit 2 active->done: %v", ce)
	}
	got := resultText(t, r2)
	if !strings.Contains(got, "does not close automatically") {
		t.Errorf("nudge did not fire on the last sibling: %q", got)
	}
	if !strings.Contains(got, oid) {
		t.Errorf("nudge %q does not name the outcome", got)
	}
	if !strings.Contains(got, strconv.Itoa(2)) {
		t.Errorf("nudge %q does not name the sibling count", got)
	}

	o, err := store.GetOutcome(context.Background(), oid)
	if err != nil {
		t.Fatalf("GetOutcome: %v", err)
	}
	if o.Status != wms.StatusActive {
		t.Errorf("outcome status = %q, want unchanged %q (nudge must not close it)", o.Status, wms.StatusActive)
	}
}

// TestCloseoutNudge_ViaGenericUpdateStatus: the same nudge fires through the
// generic wms_updateStatus entrypoint, not just ToolUpdateWorkUnitStatus.
func TestCloseoutNudge_ViaGenericUpdateStatus(t *testing.T) {
	store, oid := newStewardStore(t)
	if _, ce := call(t, store, ToolUpdateOutcomeStatus, map[string]interface{}{"id": oid, "status": wms.StatusActive}); ce != nil {
		t.Fatalf("outcome pending->active: %v", ce)
	}
	const wuID = "wu-nudge-generic"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := call(t, store, "wms_updateStatus", map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": wuID, "status": wms.StatusActive,
	}); ce != nil {
		t.Fatalf("workunit pending->active via wms_updateStatus: %v", ce)
	}
	r, ce := call(t, store, "wms_updateStatus", map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": wuID, "status": wms.StatusDone,
	})
	if ce != nil {
		t.Fatalf("workunit active->done via wms_updateStatus: %v", ce)
	}
	if got := resultText(t, r); !strings.Contains(got, "does not close automatically") {
		t.Errorf("nudge did not fire via wms_updateStatus: %q", got)
	}
}

// TestCloseoutNudge_SilentWhenOutcomeAlreadyTerminal: if the Outcome itself
// is already terminal by the time the last sibling closes, the nudge must
// stay silent — there is nothing left to "run close-out" on.
func TestCloseoutNudge_SilentWhenOutcomeAlreadyTerminal(t *testing.T) {
	store, oid := newStewardStore(t)
	const wuID = "wu-nudge-outcome-terminal"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := call(t, store, ToolUpdateOutcomeStatus, map[string]interface{}{"id": oid, "status": wms.StatusActive}); ce != nil {
		t.Fatalf("outcome pending->active: %v", ce)
	}
	if _, ce := call(t, store, ToolUpdateOutcomeStatus, map[string]interface{}{"id": oid, "status": wms.StatusDone}); ce != nil {
		t.Fatalf("outcome active->done: %v", ce)
	}
	if _, ce := call(t, store, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": wuID, "status": wms.StatusActive}); ce != nil {
		t.Fatalf("workunit pending->active: %v", ce)
	}
	r, ce := call(t, store, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": wuID, "status": wms.StatusDone})
	if ce != nil {
		t.Fatalf("workunit active->done: %v", ce)
	}
	if got := resultText(t, r); strings.Contains(got, "does not close automatically") {
		t.Errorf("nudge fired even though the outcome was already terminal: %q", got)
	}
}

// --- WP10 path 1/5: notes population ---

// TestUpdateWorkUnitStatus_NotesGivenAndDefaulted: WP10 AC2 — a caller-
// supplied notes argument round-trips into the journal row exactly; when
// omitted, the row's notes is non-empty and names the tool.
func TestUpdateWorkUnitStatus_NotesGivenAndDefaulted(t *testing.T) {
	store, oid := newStewardStore(t)
	eng := realEngineFor(store)
	const wuID = "wu-notes"
	if _, ce := callWithEngine(t, store, eng, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}

	const explicitNotes = "picking this up now"
	if _, ce := callWithEngine(t, store, eng, ToolUpdateWorkUnitStatus, map[string]interface{}{
		"id": wuID, "status": wms.StatusActive, "notes": explicitNotes,
	}); ce != nil {
		t.Fatalf("workunit pending->active with notes: %v", ce)
	}
	if got := journalNotesForTransition(t, store, wms.EntityWorkUnit, wuID, wms.StatusPending, wms.StatusActive); got != explicitNotes {
		t.Errorf("journal notes = %q, want exactly %q", got, explicitNotes)
	}

	if _, ce := callWithEngine(t, store, eng, ToolUpdateWorkUnitStatus, map[string]interface{}{
		"id": wuID, "status": wms.StatusReview,
	}); ce != nil {
		t.Fatalf("workunit active->review without notes: %v", ce)
	}
	got := journalNotesForTransition(t, store, wms.EntityWorkUnit, wuID, wms.StatusActive, wms.StatusReview)
	if got == "" {
		t.Error("journal notes empty when omitted, want an honest default")
	}
	if !strings.Contains(got, ToolUpdateWorkUnitStatus) {
		t.Errorf("default journal notes %q does not name the tool", got)
	}
}

// TestGenericUpdateStatus_NotesRoundtrip: same AC2 guarantee through the
// generic wms_updateStatus entrypoint.
func TestGenericUpdateStatus_NotesRoundtrip(t *testing.T) {
	store, oid := newStewardStore(t)
	eng := realEngineFor(store)
	const wuID = "wu-notes-generic"
	if _, ce := callWithEngine(t, store, eng, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	const explicitNotes = "generic path notes"
	if _, ce := callWithEngine(t, store, eng, "wms_updateStatus", map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": wuID, "status": wms.StatusActive, "notes": explicitNotes,
	}); ce != nil {
		t.Fatalf("wms_updateStatus with notes: %v", ce)
	}
	if got := latestJournalNotes(t, store, wms.EntityWorkUnit, wuID); got != explicitNotes {
		t.Errorf("journal notes = %q, want exactly %q", got, explicitNotes)
	}
}

// TestDeliverResult_NotesEqualsSummary: WP10 AC3 — the journal row written
// by wms_deliverResult's active→review transition carries the delivered
// summary as its notes.
func TestDeliverResult_NotesEqualsSummary(t *testing.T) {
	store, oid := newStewardStore(t)
	eng := realEngineFor(store)
	const wuID = "wu-deliver-notes"
	if _, ce := callWithEngine(t, store, eng, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := callWithEngine(t, store, eng, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": wuID, "status": wms.StatusActive}); ce != nil {
		t.Fatalf("workunit pending->active: %v", ce)
	}
	const summary = "did the thing, verified with tests"
	if _, ce := callWithEngine(t, store, eng, ToolDeliverResult, map[string]interface{}{
		"id": wuID, "summary": summary, "result": "full write-up",
	}); ce != nil {
		t.Fatalf("deliverResult: %v", ce)
	}
	if got := journalNotesForTransition(t, store, wms.EntityWorkUnit, wuID, wms.StatusActive, wms.StatusReview); got != summary {
		t.Errorf("journal notes = %q, want exactly the summary %q", got, summary)
	}
}

// TestDeliverResult_RedeliveryInReviewAppendsWithoutTransition pins the
// MCP-KG-2 fix: a work unit already in review can be delivered again — the
// tool's own description has always said redelivery is allowed, and
// wms_listDeliverables documents "take the last row," but the code
// previously refused with "is not active" once status left active, forcing
// a pointless review→active→review round-trip before every re-delivery.
// This test would FAIL on pre-fix code (the second deliverResult call would
// return a CallError instead of succeeding).
func TestDeliverResult_RedeliveryInReviewAppendsWithoutTransition(t *testing.T) {
	store, oid := newStewardStore(t)
	eng := realEngineFor(store)
	const wuID = "wu-deliver-redeliver"
	if _, ce := callWithEngine(t, store, eng, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := callWithEngine(t, store, eng, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": wuID, "status": wms.StatusActive}); ce != nil {
		t.Fatalf("workunit pending->active: %v", ce)
	}
	if _, ce := callWithEngine(t, store, eng, ToolDeliverResult, map[string]interface{}{
		"id": wuID, "summary": "first pass", "result": "first write-up",
	}); ce != nil {
		t.Fatalf("first deliverResult: %v", ce)
	}
	wu, err := store.GetWorkUnit(context.Background(), wuID)
	if err != nil {
		t.Fatalf("GetWorkUnit: %v", err)
	}
	if wu.Status != wms.StatusReview {
		t.Fatalf("status after first deliver = %q, want %q", wu.Status, wms.StatusReview)
	}

	const secondSummary = "addressed review findings"
	r, ce := callWithEngine(t, store, eng, ToolDeliverResult, map[string]interface{}{
		"id": wuID, "summary": secondSummary, "result": "second write-up",
	})
	if ce != nil {
		t.Fatalf("redelivery while in review was rejected: %v", ce)
	}
	if got := resultText(t, r); !strings.Contains(got, wms.StatusReview) {
		t.Errorf("redelivery response %q does not say the status is unchanged", got)
	}

	wu, err = store.GetWorkUnit(context.Background(), wuID)
	if err != nil {
		t.Fatalf("GetWorkUnit after redelivery: %v", err)
	}
	if wu.Status != wms.StatusReview {
		t.Errorf("status after redelivery = %q, want unchanged %q (no transition on redelivery)", wu.Status, wms.StatusReview)
	}

	delivs, err := store.ListDeliverables(context.Background(), wms.EntityWorkUnit, wuID, 10)
	if err != nil {
		t.Fatalf("ListDeliverables: %v", err)
	}
	if len(delivs) != 2 {
		t.Fatalf("deliverable count = %d, want 2 (append-only)", len(delivs))
	}
	if delivs[len(delivs)-1].Summary != secondSummary {
		t.Errorf("last deliverable summary = %q, want %q", delivs[len(delivs)-1].Summary, secondSummary)
	}

	entries, err := store.GetJournalEntries(context.Background(), wms.EntityWorkUnit, wuID, 10)
	if err != nil {
		t.Fatalf("GetJournalEntries: %v", err)
	}
	foundRedeliveryNote := false
	for _, e := range entries {
		if e.Field == "deliverable" && strings.Contains(e.Notes, secondSummary) {
			foundRedeliveryNote = true
		}
	}
	if !foundRedeliveryNote {
		t.Error("no journal entry records the redelivery (expected field=deliverable, notes containing the second summary)")
	}
}

// TestDeliverResult_RejectsWhenDone: the gate widens from "active only" to
// "active or review," not further — a terminal work unit still refuses
// delivery.
func TestDeliverResult_RejectsWhenDone(t *testing.T) {
	store, oid := newStewardStore(t)
	eng := realEngineFor(store)
	const wuID = "wu-deliver-done"
	if _, ce := callWithEngine(t, store, eng, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := callWithEngine(t, store, eng, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": wuID, "status": wms.StatusActive}); ce != nil {
		t.Fatalf("workunit pending->active: %v", ce)
	}
	if _, ce := callWithEngine(t, store, eng, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": wuID, "status": wms.StatusDone}); ce != nil {
		t.Fatalf("workunit active->done: %v", ce)
	}
	_, ce := callWithEngine(t, store, eng, ToolDeliverResult, map[string]interface{}{
		"id": wuID, "summary": "too late", "result": "should be rejected",
	})
	if ce == nil {
		t.Fatal("deliverResult on a done workunit returned no error")
	}
	if !strings.Contains(ce.Message, wms.StatusDone) {
		t.Errorf("error %q does not name the actual status", ce.Message)
	}
}

// TestClaimWorkUnit_NotesNamesAgent: WP10 AC7 — claiming a pending WorkUnit
// writes a journal row whose notes name the claiming agent (previously: the
// row existed but notes was empty).
func TestClaimWorkUnit_NotesNamesAgent(t *testing.T) {
	store, oid := newStewardStore(t)
	eng := realEngineFor(store)
	const wuID = "wu-claim-notes"
	if _, ce := callWithEngine(t, store, eng, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	raw, err := json.Marshal(map[string]interface{}{
		"name":      ToolClaimWorkUnit,
		"arguments": map[string]interface{}{"id": wuID},
		"_meta":     map[string]interface{}{"agent_type": "@agent-notes"},
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	if _, ce := HandleToolCall(store, eng, raw); ce != nil {
		t.Fatalf("claimWorkUnit: %v", ce)
	}
	if got := latestJournalNotes(t, store, wms.EntityWorkUnit, wuID); !strings.Contains(got, "@agent-notes") {
		t.Errorf("journal notes = %q, want it to name the claiming agent", got)
	}
}

// --- Sixth WP10 path: create with a non-default initial status ---

// TestCreateOutcome_NonDefaultStatus_ReachesJournal: creating an Outcome with
// status="active" (the WP1b/WP12-Site-B "fold status into create" pattern)
// must write exactly one wms_journal row for the implicit pending→active
// transition, naming the tool in its notes — previously this silently wrote
// nothing.
func TestCreateOutcome_NonDefaultStatus_ReachesJournal(t *testing.T) {
	store, _ := newStewardStore(t)
	eng := realEngineFor(store)
	const oid = "out-create-active"
	if _, ce := callWithEngine(t, store, eng, ToolCreateOutcome, map[string]interface{}{
		"id": oid, "title": oid, "status": wms.StatusActive,
	}); ce != nil {
		t.Fatalf("createOutcome with status=active: %v", ce)
	}
	entries, err := store.GetJournalEntries(context.Background(), wms.EntityOutcome, oid, 10)
	if err != nil {
		t.Fatalf("GetJournalEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("journal entries for %s = %d, want exactly 1", oid, len(entries))
	}
	if entries[0].OldValue != wms.StatusPending || entries[0].NewValue != wms.StatusActive {
		t.Errorf("journal transition = %s→%s, want pending→active", entries[0].OldValue, entries[0].NewValue)
	}
	if !strings.Contains(entries[0].Notes, ToolCreateOutcome) {
		t.Errorf("journal notes = %q, want it to name %s", entries[0].Notes, ToolCreateOutcome)
	}
}

// TestCreateOutcome_DefaultStatus_NoJournalRow: creating an Outcome at the
// default pending status must NOT synthesize a phantom pending→pending
// transition — the engine call is conditional on a non-default status.
func TestCreateOutcome_DefaultStatus_NoJournalRow(t *testing.T) {
	store, _ := newStewardStore(t)
	eng := realEngineFor(store)
	const oid = "out-create-pending"
	if _, ce := callWithEngine(t, store, eng, ToolCreateOutcome, map[string]interface{}{
		"id": oid, "title": oid,
	}); ce != nil {
		t.Fatalf("createOutcome: %v", ce)
	}
	entries, err := store.GetJournalEntries(context.Background(), wms.EntityOutcome, oid, 10)
	if err != nil {
		t.Fatalf("GetJournalEntries: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("journal entries for a default-pending create = %d, want 0: %+v", len(entries), entries)
	}
}

// TestCreateWorkUnit_NonDefaultStatus_ReachesJournal: same guarantee as
// TestCreateOutcome_NonDefaultStatus_ReachesJournal, for WorkUnits.
func TestCreateWorkUnit_NonDefaultStatus_ReachesJournal(t *testing.T) {
	store, oid := newStewardStore(t)
	eng := realEngineFor(store)
	const wuID = "wu-create-active"
	if _, ce := callWithEngine(t, store, eng, ToolCreateWorkUnit, map[string]interface{}{
		"id": wuID, "title": wuID, "outcomeID": oid, "status": wms.StatusActive,
	}); ce != nil {
		t.Fatalf("createWorkUnit with status=active: %v", ce)
	}
	entries, err := store.GetJournalEntries(context.Background(), wms.EntityWorkUnit, wuID, 10)
	if err != nil {
		t.Fatalf("GetJournalEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("journal entries for %s = %d, want exactly 1", wuID, len(entries))
	}
	if entries[0].OldValue != wms.StatusPending || entries[0].NewValue != wms.StatusActive {
		t.Errorf("journal transition = %s→%s, want pending→active", entries[0].OldValue, entries[0].NewValue)
	}
	if !strings.Contains(entries[0].Notes, ToolCreateWorkUnit) {
		t.Errorf("journal notes = %q, want it to name %s", entries[0].Notes, ToolCreateWorkUnit)
	}
}

// TestCreateWorkUnit_StatusEquivalence_CreateActiveVsCreateThenUpdate is the
// lead's invariant: create(status=active) must leave the same journal
// coverage (a pending→active row, reaching JournalObserver) that
// create()+update(active) leaves. The two paths are NOT expected to produce
// an identical *interval* history — create(status=active) opens exactly one
// interval (OpenEventRecord, active) since the entity was never truly
// pending; create()+update(active) opens a pending interval and then closes
// it via TransitionEventRecord when the update happens. That is not a bug:
// OnStatusChange itself never touches wms_intervals/TransitionEventRecord
// (verified by reading engine.go — its three steps are dependency-unblock,
// the WorkUnit→Outcome advisory, and observer notification only), so this
// fix cannot and does not change interval-side behavior. What must be equal
// is the journal-row/observer-call coverage of the transition itself.
func TestCreateWorkUnit_StatusEquivalence_CreateActiveVsCreateThenUpdate(t *testing.T) {
	store, oid := newStewardStore(t)
	eng := realEngineFor(store)

	const wuTwoCall = "wu-equiv-two-call"
	if _, ce := callWithEngine(t, store, eng, ToolCreateWorkUnit, map[string]interface{}{
		"id": wuTwoCall, "title": wuTwoCall, "outcomeID": oid,
	}); ce != nil {
		t.Fatalf("createWorkUnit (pending): %v", ce)
	}
	if _, ce := callWithEngine(t, store, eng, ToolUpdateWorkUnitStatus, map[string]interface{}{
		"id": wuTwoCall, "status": wms.StatusActive,
	}); ce != nil {
		t.Fatalf("updateWorkUnitStatus pending->active: %v", ce)
	}

	const wuOneCall = "wu-equiv-one-call"
	if _, ce := callWithEngine(t, store, eng, ToolCreateWorkUnit, map[string]interface{}{
		"id": wuOneCall, "title": wuOneCall, "outcomeID": oid, "status": wms.StatusActive,
	}); ce != nil {
		t.Fatalf("createWorkUnit (status=active): %v", ce)
	}

	// Both paths must have a journal row for the pending->active transition
	// (existence check — journalNotesForTransition fatals if absent). Notes
	// text legitimately differs (one names the update tool, the other names
	// the create tool) and is not compared here.
	journalNotesForTransition(t, store, wms.EntityWorkUnit, wuTwoCall, wms.StatusPending, wms.StatusActive)
	journalNotesForTransition(t, store, wms.EntityWorkUnit, wuOneCall, wms.StatusPending, wms.StatusActive)

	// Both paths must leave the entity itself in the same terminal state:
	// status=active, in the store.
	twoCall, err := store.GetWorkUnit(context.Background(), wuTwoCall)
	if err != nil {
		t.Fatalf("GetWorkUnit(%s): %v", wuTwoCall, err)
	}
	oneCall, err := store.GetWorkUnit(context.Background(), wuOneCall)
	if err != nil {
		t.Fatalf("GetWorkUnit(%s): %v", wuOneCall, err)
	}
	if twoCall.Status != wms.StatusActive || oneCall.Status != wms.StatusActive {
		t.Errorf("status = two-call:%q one-call:%q, want both %q", twoCall.Status, oneCall.Status, wms.StatusActive)
	}
}

// --- Adversarial-review fixes (redteam pass on wh-impl-mcp) ---

// TestClaimWorkUnit_LeadClaimNoteNotDangling pins the MINOR finding: a lead
// claim (p.Meta.AgentType empty by design, per CLAUDE.md's "claimed WorkUnit
// agent_id" convention) must not synthesize a journal note that trails off
// after "claimed by " — it must name the lead explicitly instead.
// callWithEngine sends no `_meta`, so p.Meta.AgentType is "" here exactly as
// it is for a real lead-initiated claim.
func TestClaimWorkUnit_LeadClaimNoteNotDangling(t *testing.T) {
	store, oid := newStewardStore(t)
	eng := realEngineFor(store)
	const wuID = "wu-claim-lead"
	if _, ce := callWithEngine(t, store, eng, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := callWithEngine(t, store, eng, ToolClaimWorkUnit, map[string]interface{}{"id": wuID}); ce != nil {
		t.Fatalf("claimWorkUnit (lead, no agent_type): %v", ce)
	}
	got := latestJournalNotes(t, store, wms.EntityWorkUnit, wuID)
	if strings.HasSuffix(got, "claimed by ") || strings.HasSuffix(got, "claimed by") {
		t.Errorf("journal notes = %q, dangles after \"claimed by\" for an empty agent type", got)
	}
	if !strings.Contains(got, "the lead") {
		t.Errorf("journal notes = %q, want it to name the lead explicitly", got)
	}
}

// failOnGetOutcome wraps a wms.Store and fails GetOutcome for exactly one
// outcome id with an injected, non-not-found error, passing every other
// call straight through via interface embedding. Pins the adversarial-review
// NOTE that the terminal-Outcome guard must fail closed on a genuine store
// error rather than silently letting creation proceed past a check that
// could not actually run.
type failOnGetOutcome struct {
	wms.Store
	failOutcomeID string
	injectedErr   error
}

func (f *failOnGetOutcome) GetOutcome(ctx context.Context, id string) (*wms.Outcome, error) {
	if f.failOutcomeID != "" && id == f.failOutcomeID {
		return nil, f.injectedErr
	}
	return f.Store.GetOutcome(ctx, id)
}

// TestCreateWorkUnit_GenuineStoreErrorFailsClosed pins the NOTE 3 fix: a
// non-not-found GetOutcome failure during the terminal-Outcome guard must
// fail the create call outright (surfacing the underlying error), not
// silently let creation proceed as if the Outcome were open.
func TestCreateWorkUnit_GenuineStoreErrorFailsClosed(t *testing.T) {
	store, oid := newStewardStore(t)
	injected := errors.New("injected: connection reset")
	failing := &failOnGetOutcome{Store: store, failOutcomeID: oid, injectedErr: injected}

	const wuID = "wu-store-error"
	_, ce := call(t, failing, ToolCreateWorkUnit, map[string]interface{}{
		"id": wuID, "title": "should fail closed", "outcomeID": oid,
	})
	if ce == nil {
		t.Fatal("createWorkUnit returned no error when GetOutcome failed with a non-not-found error")
	}
	if !strings.Contains(ce.Message, "injected: connection reset") {
		t.Errorf("error %q does not surface the underlying store failure", ce.Message)
	}
	if _, err := store.GetWorkUnit(context.Background(), wuID); err == nil {
		t.Error("work unit was created despite the guard's store read failing")
	}
}

// TestCreateWorkUnit_UnknownOutcomeCleanError pins the LF-VER-3 fix: an
// unknown outcomeID gets a clean sentence naming the outcome, resolved
// before any backend-specific INSERT runs, instead of falling through to
// CreateWorkUnit's raw FK-violation driver error (which never even named the
// outcomeID — the MySQL 1452 text has no room for it). This is the one test
// in this file that would fail on pre-fix code: the old fall-through message
// contained "FOREIGN KEY" and "fk_wu_outcome", not the outcome id.
func TestCreateWorkUnit_UnknownOutcomeCleanError(t *testing.T) {
	store, _ := newStewardStore(t)
	const badOutcome = "outcome-does-not-exist"
	_, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{
		"id": "wu-unknown-outcome", "title": "x", "outcomeID": badOutcome,
	})
	if ce == nil {
		t.Fatal("createWorkUnit under an unknown outcomeID returned no error")
	}
	if strings.Contains(ce.Message, "reopen it first") {
		t.Errorf("error %q incorrectly took the terminal-outcome guard path for an unknown outcome", ce.Message)
	}
	if !strings.Contains(ce.Message, badOutcome) {
		t.Errorf("error %q does not name the outcome id", ce.Message)
	}
	for _, leak := range []string{"FOREIGN KEY", "foreign key", "Error 1452", "fk_wu_outcome", "CONSTRAINT"} {
		if strings.Contains(ce.Message, leak) {
			t.Errorf("error %q leaks raw driver/schema internals (%q)", ce.Message, leak)
		}
	}
	if _, err := store.GetWorkUnit(context.Background(), "wu-unknown-outcome"); err == nil {
		t.Error("rejected work unit was created anyway")
	}
}
