package wms

import (
	"context"
	"strings"
	"testing"
	"time"

	storeTypes "github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// Tests for wms_setPhase's gate and error semantics (LF-skills-2/LF-VER-5):
// the gate must consult the workunit's own status, not merely whether a
// state interval happens to be open, and a failure to land the phase must be
// a real CallError naming the actual cause rather than a silent TextResult.

// TestSetPhase_UnknownWorkUnitCleanError: setPhase against a nonexistent
// workunit gives a clean "workunit %s not found" sentence rather than
// passing the store's not-found error through raw — consistent with item 4's
// principle elsewhere in this same WU (no unpolished store errors on the
// wire), applied here too since setPhase gained its own GetWorkUnit call.
func TestSetPhase_UnknownWorkUnitCleanError(t *testing.T) {
	store, _ := newStewardStore(t)
	const badID = "wu-setphase-does-not-exist"
	_, ce := call(t, store, ToolSetPhase, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": badID, "phase": "build",
	})
	if ce == nil {
		t.Fatal("setPhase on an unknown workunit returned no error")
	}
	if !strings.Contains(ce.Message, badID) {
		t.Errorf("error %q does not name the workunit id", ce.Message)
	}
	if strings.Contains(ce.Message, "store:") || strings.Contains(ce.Message, "GetWorkUnit") {
		t.Errorf("error %q leaks the raw store error shape", ce.Message)
	}
}

// TestSetPhase_RejectsPendingWorkUnit: a workunit that was never activated
// has no open interval AND is not active — the gate must name the real
// cause (not active), not the interval.
func TestSetPhase_RejectsPendingWorkUnit(t *testing.T) {
	store, oid := newStewardStore(t)
	const wuID = "wu-setphase-pending"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	_, ce := call(t, store, ToolSetPhase, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": wuID, "phase": "build",
	})
	if ce == nil {
		t.Fatal("setPhase on a pending workunit returned no error")
	}
	if !strings.Contains(ce.Message, "not active") {
		t.Errorf("error %q does not name the real cause (not active)", ce.Message)
	}
	if !strings.Contains(ce.Message, wms.StatusPending) {
		t.Errorf("error %q does not name the workunit's actual status", ce.Message)
	}
}

// TestSetPhase_ErrorsWhenActiveButIntervalDrained is the regression case this
// WU fixes: an ACTIVE workunit whose state interval was closed out from under
// it (e.g. by hookd's per-turn Stop drain — HOOKD-DRAIN.md §4/LF-VER-5) used
// to get the misdiagnosis "transition it active first", false on its face
// since the unit already is active, returned as a silent TextResult a caller
// not reading the body would miss. It must now be a real CallError naming
// the actual cause (active, but no open interval), and it would have FAILED
// against the pre-fix code (pre-fix: no error, a TextResult with the false
// "transition it active first" advice).
func TestSetPhase_ErrorsWhenActiveButIntervalDrained(t *testing.T) {
	store, oid := newStewardStore(t)
	const wuID = "wu-setphase-drained"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := call(t, store, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": wuID, "status": wms.StatusActive}); ce != nil {
		t.Fatalf("workunit pending->active: %v", ce)
	}
	// Confirm the activation opened a state interval, then simulate the
	// drain the same way hookd's per-turn Stop handler does it
	// (CloseSessionIntervals — an unconditional UPDATE by session_id with no
	// `kind` filter, so it closes state intervals too, not just focus ones —
	// see HOOKD-DRAIN.md §4/§3.3): this closes the interval WITHOUT any
	// status transition, leaving the workunit active but interval-less,
	// which TransitionEventRecord alone cannot produce (it always pairs a
	// close with a status write).
	ctx := context.Background()
	rec, err := store.GetOpenEventRecord(ctx, wms.EntityWorkUnit, wuID)
	if err != nil {
		t.Fatalf("GetOpenEventRecord: %v", err)
	}
	if rec == nil {
		t.Fatal("no open interval after activation — test setup assumption broken")
	}
	if _, err := store.CloseSessionIntervals(ctx, rec.SessionID, rec.AgentName, time.Now().UTC()); err != nil {
		t.Fatalf("simulate drain (CloseSessionIntervals): %v", err)
	}
	if rec2, err := store.GetOpenEventRecord(ctx, wms.EntityWorkUnit, wuID); err != nil && !storeTypes.IsNotFound(err) {
		t.Fatalf("GetOpenEventRecord after drain: %v", err)
	} else if rec2 != nil {
		t.Fatal("interval still open after simulated drain — test setup did not reproduce the scenario")
	}
	wu, err := store.GetWorkUnit(ctx, wuID)
	if err != nil {
		t.Fatalf("GetWorkUnit after drain: %v", err)
	}
	if wu.Status != wms.StatusActive {
		t.Fatalf("workunit status = %q after simulated drain, want unchanged %q — test setup corrupted status", wu.Status, wms.StatusActive)
	}

	_, ce := call(t, store, ToolSetPhase, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": wuID, "phase": "build",
	})
	if ce == nil {
		t.Fatal("setPhase on an active-but-drained workunit returned no error (silent no-op, the pre-fix bug)")
	}
	if strings.Contains(ce.Message, "transition it active first") {
		t.Errorf("error %q repeats the false pre-fix diagnosis (the unit already is active)", ce.Message)
	}
	if !strings.Contains(ce.Message, "no open state interval") {
		t.Errorf("error %q does not name the real cause (no open interval despite active status)", ce.Message)
	}
}

// TestSetPhase_SucceedsOnActiveWorkUnit: the ordinary path is unaffected —
// an active workunit with its interval intact still lands the phase and
// says what it wrote.
func TestSetPhase_SucceedsOnActiveWorkUnit(t *testing.T) {
	store, oid := newStewardStore(t)
	const wuID = "wu-setphase-ok"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := call(t, store, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": wuID, "status": wms.StatusActive}); ce != nil {
		t.Fatalf("workunit pending->active: %v", ce)
	}
	r, ce := call(t, store, ToolSetPhase, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": wuID, "phase": "build",
	})
	if ce != nil {
		t.Fatalf("setPhase on an active workunit: %v", ce)
	}
	got := resultText(t, r)
	if !strings.Contains(got, "build") || !strings.Contains(got, wuID) {
		t.Errorf("success message %q does not say what was written", got)
	}
	rec, err := store.GetOpenEventRecord(context.Background(), wms.EntityWorkUnit, wuID)
	if err != nil {
		t.Fatalf("GetOpenEventRecord: %v", err)
	}
	if rec == nil || rec.Phase == nil || *rec.Phase != "build" {
		t.Errorf("interval phase = %+v, want \"build\" landed on the open interval", rec)
	}
}

// TestSetPhase_SucceedsOnReviewWorkUnit pins the adversarial-review fix: a
// workunit in review (the status wms_deliverResult leaves it in through the
// whole VALIDATE/ADVERSARIAL-REVIEW/send-back loop — BRIEF-COMMON hard rule
// 7, execution-loop.md's phase=test/review/iterate mapping) must accept
// phase declarations, not just active ones. wms_deliverResult's own
// active->review transition opens a fresh "review" state interval via
// TransitionEventRecord, so this is the ordinary shape, not a contrived one.
// This is the test that would have FAILED against the narrower active-only
// gate this WU shipped first (adversarial review caught it before commit):
// a review-status workunit would have hard-errored with "is not active",
// and the error's own advice — transition it active first — would have
// corrupted the status the review loop depends on.
func TestSetPhase_SucceedsOnReviewWorkUnit(t *testing.T) {
	store, oid := newStewardStore(t)
	const wuID = "wu-setphase-review"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := call(t, store, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": wuID, "status": wms.StatusActive}); ce != nil {
		t.Fatalf("workunit pending->active: %v", ce)
	}
	if _, ce := call(t, store, ToolDeliverResult, map[string]interface{}{
		"id": wuID, "summary": "done, ready for review", "result": "full write-up",
	}); ce != nil {
		t.Fatalf("deliverResult active->review: %v", ce)
	}
	wu, err := store.GetWorkUnit(context.Background(), wuID)
	if err != nil {
		t.Fatalf("GetWorkUnit: %v", err)
	}
	if wu.Status != wms.StatusReview {
		t.Fatalf("status after deliverResult = %q, want %q — test setup assumption broken", wu.Status, wms.StatusReview)
	}

	r, ce := call(t, store, ToolSetPhase, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": wuID, "phase": "review",
	})
	if ce != nil {
		t.Fatalf("setPhase on a review-status workunit: %v", ce)
	}
	got := resultText(t, r)
	if !strings.Contains(got, "review") || !strings.Contains(got, wuID) {
		t.Errorf("success message %q does not say what was written", got)
	}
	rec, err := store.GetOpenEventRecord(context.Background(), wms.EntityWorkUnit, wuID)
	if err != nil {
		t.Fatalf("GetOpenEventRecord: %v", err)
	}
	if rec == nil || rec.Phase == nil || *rec.Phase != "review" {
		t.Errorf("interval phase = %+v, want \"review\" landed on the open interval", rec)
	}
}

// TestSetPhase_ErrorsWhenReviewButIntervalDrained: the drained-interval error
// case applies to review status too, and the message must name the actual
// status (review), not hardcode "active" — proving the message is genuinely
// dynamic, not just widened at the gate.
func TestSetPhase_ErrorsWhenReviewButIntervalDrained(t *testing.T) {
	store, oid := newStewardStore(t)
	const wuID = "wu-setphase-review-drained"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}
	if _, ce := call(t, store, ToolUpdateWorkUnitStatus, map[string]interface{}{"id": wuID, "status": wms.StatusActive}); ce != nil {
		t.Fatalf("workunit pending->active: %v", ce)
	}
	if _, ce := call(t, store, ToolDeliverResult, map[string]interface{}{
		"id": wuID, "summary": "s", "result": "r",
	}); ce != nil {
		t.Fatalf("deliverResult active->review: %v", ce)
	}

	ctx := context.Background()
	rec, err := store.GetOpenEventRecord(ctx, wms.EntityWorkUnit, wuID)
	if err != nil {
		t.Fatalf("GetOpenEventRecord: %v", err)
	}
	if rec == nil {
		t.Fatal("no open interval after deliverResult — test setup assumption broken")
	}
	if _, err := store.CloseSessionIntervals(ctx, rec.SessionID, rec.AgentName, time.Now().UTC()); err != nil {
		t.Fatalf("simulate drain (CloseSessionIntervals): %v", err)
	}

	_, ce := call(t, store, ToolSetPhase, map[string]interface{}{
		"entityType": wms.EntityWorkUnit, "entityID": wuID, "phase": "iterate",
	})
	if ce == nil {
		t.Fatal("setPhase on a review-but-drained workunit returned no error")
	}
	if !strings.Contains(ce.Message, "no open state interval") {
		t.Errorf("error %q does not name the real cause", ce.Message)
	}
	if !strings.Contains(ce.Message, wms.StatusReview) {
		t.Errorf("error %q does not name the actual status (review), it must not hardcode \"active\"", ce.Message)
	}
}
