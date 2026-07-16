package classify

import (
	"context"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// fakeSignalReader returns canned signals regardless of window, so each phase
// rule can be exercised deterministically without touching JSONL.
type fakeSignalReader struct {
	sig *wms.ActivitySignals
}

func (f *fakeSignalReader) ReadSignals(_ context.Context, sessions []wms.SessionWindow, _ string) (*wms.ActivitySignals, error) {
	// Honor the "no window → no events" contract so intervalWindows's empty-slice
	// path is exercised by the no-session test.
	if len(sessions) == 0 {
		return &wms.ActivitySignals{ToolTagCounts: map[string]int{}, FilesTouched: map[string]int{}}, nil
	}
	if f.sig == nil {
		return &wms.ActivitySignals{ToolTagCounts: map[string]int{}, FilesTouched: map[string]int{}}, nil
	}
	return f.sig, nil
}

func tags(m map[string]int, total int) *wms.ActivitySignals {
	return &wms.ActivitySignals{ToolTagCounts: m, FilesTouched: map[string]int{}, TotalEvents: total}
}

func runnerWith(sig *wms.ActivitySignals) *Runner {
	return New(nil, &fakeSignalReader{sig: sig}, "unused.jsonl", nil)
}

func closedRec(state, session, agent string) wms.EventRecord {
	start := time.Now().Add(-time.Hour)
	end := time.Now()
	return wms.EventRecord{
		ID: 1, EntityType: wms.EntityWorkUnit, EntityID: "wu", State: state,
		StartedAt: start, EndedAt: &end, SessionID: session, AgentName: agent,
	}
}

// fakeBatchSignalReader is a wms.BatchSignalReader whose ReadSignals arm
// returns an obviously-wrong sentinel (a huge unmatched-tag count) so any test
// relying on it accidentally being called instead of the batched path fails
// loudly rather than silently passing.
type fakeBatchSignalReader struct {
	calls                     int
	lastWindows               []wms.SessionWindow
	lastLowerBound            time.Time
	lastUpperBound            time.Time
	perWindow                 []*wms.ActivitySignals
	readSignalsCalledMistaken bool
}

func (f *fakeBatchSignalReader) ReadSignals(_ context.Context, _ []wms.SessionWindow, _ string) (*wms.ActivitySignals, error) {
	f.readSignalsCalledMistaken = true
	return &wms.ActivitySignals{ToolTagCounts: map[string]int{"SENTINEL_WRONG_PATH": 9999}, FilesTouched: map[string]int{}, TotalEvents: 9999}, nil
}

func (f *fakeBatchSignalReader) ReadSignalsBatch(_ context.Context, windows []wms.SessionWindow, _ string, lowerBound, upperBound time.Time) ([]*wms.ActivitySignals, error) {
	f.calls++
	f.lastWindows = windows
	f.lastLowerBound = lowerBound
	f.lastUpperBound = upperBound
	out := make([]*wms.ActivitySignals, len(windows))
	for i := range windows {
		if i < len(f.perWindow) {
			out[i] = f.perWindow[i]
		} else {
			out[i] = &wms.ActivitySignals{ToolTagCounts: map[string]int{}, FilesTouched: map[string]int{}}
		}
	}
	return out, nil
}

// TestBatchReadSignals_ScansOnceAndScopesToSignalNeedingIntervals is the GH
// #13 perf-fix regression: a batch of intervals must produce exactly ONE
// ReadSignalsBatch call (not one per interval — the bug), the request must
// exclude intervals whose phase is already determined without signals
// (rework via reEntry, review via state) and sessionless intervals (which
// never queried the log even pre-fix), and the lower/upper bound must be the
// min/max Start/End of exactly the windows actually sent.
func TestBatchReadSignals_ScansOnceAndScopesToSignalNeedingIntervals(t *testing.T) {
	base := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	mk := func(id int64, state, session, agent string, startMin, endMin int) wms.EventRecord {
		s := base.Add(time.Duration(startMin) * time.Minute)
		e := base.Add(time.Duration(endMin) * time.Minute)
		return wms.EventRecord{
			ID: id, EntityType: wms.EntityWorkUnit, EntityID: "wu", State: state,
			StartedAt: s, EndedAt: &e, SessionID: session, AgentName: agent,
		}
	}

	reworkRec := mk(1, "active", "sessrework0001", "ag", 0, 5)     // excluded: reEntry
	reviewRec := mk(2, "review", "sessreview0001", "ag", 10, 15)   // excluded: review state
	sessionless := mk(3, "active", "", "", 20, 25)                 // excluded: no session
	active1 := mk(4, "active", "sessactive00001", "ag", 30, 35)     // included
	active2 := mk(5, "active", "sessactive00002", "ag", 100, 105)   // included, widest window

	intervals := []wms.EventRecord{reworkRec, reviewRec, sessionless, active1, active2}
	reEntry := map[int64]bool{reworkRec.ID: true}

	fake := &fakeBatchSignalReader{}
	r := New(nil, fake, "unused.jsonl", nil)

	sigs, err := r.batchReadSignals(context.Background(), intervals, reEntry)
	if err != nil {
		t.Fatalf("batchReadSignals: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("ReadSignalsBatch called %d times, want exactly 1 (one scan per pass, not per interval)", fake.calls)
	}
	if fake.readSignalsCalledMistaken {
		t.Fatalf("ReadSignals (per-window) was called — batched path should never fall back to it when available")
	}
	if len(fake.lastWindows) != 2 {
		t.Fatalf("windows sent = %d, want 2 (only active1, active2 — rework/review/sessionless excluded)", len(fake.lastWindows))
	}
	if len(sigs) != 2 {
		t.Fatalf("result map has %d entries, want 2", len(sigs))
	}
	if _, ok := sigs[reworkRec.ID]; ok {
		t.Error("rework interval should not be in the signals map (never queried)")
	}
	if _, ok := sigs[reviewRec.ID]; ok {
		t.Error("review interval should not be in the signals map (never queried)")
	}
	if _, ok := sigs[sessionless.ID]; ok {
		t.Error("sessionless interval should not be in the signals map (never queried)")
	}
	if _, ok := sigs[active1.ID]; !ok {
		t.Error("active1 missing from signals map")
	}
	if _, ok := sigs[active2.ID]; !ok {
		t.Error("active2 missing from signals map")
	}

	wantLower := active1.StartedAt // earliest Start among included windows
	wantUpper := *active2.EndedAt  // latest End among included windows
	if !fake.lastLowerBound.Equal(wantLower) {
		t.Errorf("lowerBound = %v, want %v (active1's Start — the earliest included window)", fake.lastLowerBound, wantLower)
	}
	if !fake.lastUpperBound.Equal(wantUpper) {
		t.Errorf("upperBound = %v, want %v (active2's End — the latest included window)", fake.lastUpperBound, wantUpper)
	}
}

// TestBatchReadSignals_FallsBackWhenReaderLacksBatchSupport confirms
// batchReadSignals reports "no batch support" (nil, nil) — not an error — for
// a plain wms.SignalReader, so classifyPhases's fallback to the per-interval
// derivePhase path is a graceful degrade, not a failure.
func TestBatchReadSignals_FallsBackWhenReaderLacksBatchSupport(t *testing.T) {
	r := New(nil, &fakeSignalReader{}, "unused.jsonl", nil)
	sigs, err := r.batchReadSignals(context.Background(), []wms.EventRecord{closedRec("active", "sess", "ag")}, map[int64]bool{})
	if err != nil {
		t.Fatalf("batchReadSignals: %v", err)
	}
	if sigs != nil {
		t.Fatalf("sigs = %v, want nil (signal that the caller must fall back)", sigs)
	}
}

func TestDerivePhase_Rules(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		state   string
		reEntry bool
		sig     *wms.ActivitySignals
		want    string
		wantNil bool // expect errNoSignal (phase left NULL)
	}{
		{
			name: "rework wins over everything", state: "active", reEntry: true,
			sig: tags(map[string]int{"EDIT": 10}, 10), want: "rework",
		},
		{
			name: "review from interval state", state: "review",
			sig: tags(map[string]int{"EDIT": 10}, 10), want: "review",
		},
		{
			name: "test from bash-command ratio", state: "active",
			sig:  &wms.ActivitySignals{ToolTagCounts: map[string]int{"EXEC": 3}, BashCommands: []string{"go test ./...", "go test -run X", "ls"}, TotalEvents: 3},
			want: "test",
		},
		{
			name: "build from edit/write dominance", state: "active",
			sig: tags(map[string]int{"EDIT": 5, "WRITE": 2, "READ": 1}, 8), want: "build",
		},
		{
			name: "design from read/grep dominance", state: "active",
			sig: tags(map[string]int{"READ": 6, "GREP": 2, "EXEC": 1}, 9), want: "design",
		},
		{
			name: "build is the activity default", state: "active",
			sig: tags(map[string]int{"EXEC": 5, "COMM": 3}, 8), want: "build",
		},
		{
			name: "no signal with a window leaves phase NULL", state: "active",
			sig: tags(map[string]int{}, 0), wantNil: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := runnerWith(tc.sig)
			rec := closedRec(tc.state, "sessxxxxxxxx", "ag")
			phase, err := r.derivePhase(ctx, rec, tc.reEntry)
			if tc.wantNil {
				if err != errNoSignal {
					t.Fatalf("want errNoSignal, got phase=%q err=%v", phase, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("derivePhase: %v", err)
			}
			if phase != tc.want {
				t.Errorf("phase = %q, want %q", phase, tc.want)
			}
		})
	}
}

// TestDerivePhase_SessionlessCostedIntervalIsBuild is the discriminating
// regression test for the B4 phase under-derivation gap: a CLOSED interval with
// NO session/agent (the status-transition case — TransitionEventRecord gets an
// empty _meta because Claude Code does not put session_id/agent_type in the MCP
// envelope) but a positive duration demonstrably had lifecycle activity. Before
// the fix intervalWindows returned an empty window → ReadSignals TotalEvents==0
// → errNoSignal → phase NULL, even for a costed, hour-long active interval (92%
// of the dominant "(unclassified)" cost). It must now take the rule-6
// build default. A session-less interval with ZERO duration (an instantaneous
// transition, no work) still stays NULL.
func TestDerivePhase_SessionlessCostedIntervalIsBuild(t *testing.T) {
	ctx := context.Background()
	// The signal reader honors the empty-window contract, so its canned signals
	// are unreachable for a session-less interval — proving the build comes from
	// the duration fallback, not from leaked signals.
	r := runnerWith(tags(map[string]int{"READ": 99}, 99)) // would be design IF a window existed

	// Positive-duration session-less interval → build (was NULL pre-fix).
	duratedRec := closedRec("active", "", "") // closedRec spans started_at..ended_at (1h)
	phase, err := r.derivePhase(ctx, duratedRec, false)
	if err != nil {
		t.Fatalf("durated session-less interval: unexpected err %v (want phase=build)", err)
	}
	if phase != "build" {
		t.Errorf("durated session-less interval phase = %q, want %q", phase, "build")
	}

	// Zero-duration session-less interval (instantaneous transition) → still NULL.
	now := time.Now()
	zeroRec := wms.EventRecord{
		ID: 2, EntityType: wms.EntityWorkUnit, EntityID: "wu", State: "active",
		StartedAt: now, EndedAt: &now, // started == ended → no duration
	}
	if _, err := r.derivePhase(ctx, zeroRec, false); err != errNoSignal {
		t.Errorf("zero-duration session-less interval: want errNoSignal (NULL), got %v", err)
	}
}

func TestMarkReEntry(t *testing.T) {
	t0 := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }
	mk := func(id int64, eid, state string, startMin int) wms.EventRecord {
		return wms.EventRecord{ID: id, EntityType: wms.EntityWorkUnit, EntityID: eid, State: state, StartedAt: at(startMin)}
	}

	// wu-x's first review/done ENDS at minute 10. Active at 0 (before closure end
	// → NOT rework), active at 20 and 40 (after closure end → rework). wu-y never
	// closes (absent from the map) → its active is not rework.
	intervals := []wms.EventRecord{
		mk(1, "wu-x", "active", 0),  // first-pass active, before closure end
		mk(3, "wu-x", "active", 20), // re-entry
		mk(4, "wu-y", "active", 5),  // wu-y never closed
		mk(6, "wu-x", "active", 40), // re-entry again
	}
	// Closure map: wu-x's earliest review/done ENDED at minute 10. Note the
	// closing review/done interval itself is NOT in the batch — proving
	// cross-batch detection (M1 fix).
	firstClosure := map[[2]string]time.Time{
		{wms.EntityWorkUnit, "wu-x"}: at(10),
	}

	got := markReEntry(intervals, firstClosure)
	if !got[3] {
		t.Error("interval 3 should be rework (active at 20, after wu-x closure end at 10)")
	}
	if !got[6] {
		t.Error("interval 6 should be rework (active at 40, after wu-x closure end at 10)")
	}
	if got[1] {
		t.Error("interval 1 (active at 0, before closure end) is first-pass — not rework")
	}
	if got[4] {
		t.Error("interval 4 (wu-y, never closed) is not rework")
	}
	if len(got) != 2 {
		t.Errorf("re-entry set size = %d, want 2", len(got))
	}
}
