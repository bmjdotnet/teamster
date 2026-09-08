package server

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/hook"
	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
	"github.com/prometheus/client_golang/prometheus"
)

// TestMissingRequiredKeys covers the W2 soft close-out computation: which
// required keys lack a bound tag on the workunit. Order follows `required`.
func TestMissingRequiredKeys(t *testing.T) {
	tag := func(k string) wms.EntityTag { return wms.EntityTag{TagKey: k} }

	cases := []struct {
		name     string
		required []string
		present  []wms.EntityTag
		want     []string
	}{
		{
			name:     "all present",
			required: []string{"work-type", "phase"},
			present:  []wms.EntityTag{tag("work-type"), tag("phase")},
			want:     nil,
		},
		{
			name:     "one missing",
			required: []string{"work-type", "phase"},
			present:  []wms.EntityTag{tag("phase")},
			want:     []string{"work-type"},
		},
		{
			name:     "all missing preserves required order",
			required: []string{"work-type", "phase", "product"},
			present:  nil,
			want:     []string{"work-type", "phase", "product"},
		},
		{
			name:     "no required keys",
			required: nil,
			present:  []wms.EntityTag{tag("phase")},
			want:     nil,
		},
		{
			name:     "extra unrelated tags are ignored",
			required: []string{"work-type"},
			present:  []wms.EntityTag{tag("work-type"), tag("priority"), tag("product")},
			want:     nil,
		},
		{
			name:     "duplicate present tag still satisfies its key",
			required: []string{"work-type", "phase"},
			present:  []wms.EntityTag{tag("work-type"), tag("work-type")},
			want:     []string{"phase"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingRequiredKeys(tc.required, tc.present)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("missingRequiredKeys() = %v, want %v", got, tc.want)
			}
		})
	}
}

// fakeCloseoutStore satisfies store.Store via the embedded interface (left nil:
// any method the close-out path does not exercise will panic if called, which is
// the desired failure signal). It overrides only the methods the
// WMSStatusChange→done→workunit branch touches, including the inheritance walk
// warnMissingRequiredTags now does via wms.ResolveEntityTags: GetWorkUnit
// (to find the workunit's parent outcome) and GetEntityTags keyed by entity
// type/id (workunit tags vs. the outcome's, so a test can set them
// independently to exercise inheritance/shadowing).
type fakeCloseoutStore struct {
	store.Store
	required []string
	tags     []wms.EntityTag // direct tags on the workunit under test

	// outcomeID and outcomeTags are optional: unset (outcomeID=="") means the
	// workunit has no resolvable parent, matching the pre-inheritance tests'
	// behavior exactly (ResolveEntityTags never calls GetEntityTags for the
	// outcome when OutcomeID is empty).
	outcomeID   string
	outcomeTags []wms.EntityTag
}

func (f *fakeCloseoutStore) ListRequiredTagKeys(context.Context) ([]string, error) {
	return f.required, nil
}

func (f *fakeCloseoutStore) GetWorkUnit(_ context.Context, id string) (*wms.WorkUnit, error) {
	return &wms.WorkUnit{ID: id, OutcomeID: f.outcomeID}, nil
}

func (f *fakeCloseoutStore) GetEntityTags(_ context.Context, entityType, _ string) ([]wms.EntityTag, error) {
	if entityType == wms.EntityOutcome {
		return f.outcomeTags, nil
	}
	return f.tags, nil
}

func (f *fakeCloseoutStore) CloseFocusInterval(context.Context, store.SessionKey) error {
	return nil
}

func (f *fakeCloseoutStore) CloseFocusIntervalForEntity(context.Context, store.SessionKey, string, string) error {
	return nil
}

// TestEmitCloseOutWarning_WMSStatusChange exercises the full emit path: a
// workunit→done WMSStatusChange whose entity is missing a required tag must
// land a WMSCloseOutWarning record in the JSONL log carrying the entity id and
// the missing keys. Closes the gap the pure missingRequiredKeys test leaves —
// the goroutine, store reads, and buildRecord/emit are all in scope here.
func TestEmitCloseOutWarning_WMSStatusChange(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	s := &Server{
		cfg:      config.Config{Host: "testhost"},
		logFile:  f,
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker("testhost", time.Minute, time.Minute, nil),
		obsStore: &fakeCloseoutStore{
			required: []string{"work-type", "phase"},
			tags:     []wms.EntityTag{{TagKey: "phase"}}, // work-type missing
		},
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityWorkUnit,
		"wms_entity_id":   "wu-test",
		"wms_old_status":  "active",
		"wms_new_status":  wms.StatusDone,
		"wms_session_id":  "s1",
		"wms_agent_name":  "store",
	})

	rec := waitForCloseOutWarning(t, logPath)
	if got := rec["entity_id"]; got != "wu-test" {
		t.Errorf("entity_id = %v, want %q", got, "wu-test")
	}
	missing, _ := rec["missing"].([]interface{})
	if len(missing) != 1 || missing[0] != "work-type" {
		t.Errorf("missing = %v, want [work-type]", rec["missing"])
	}
	if got := rec["session"]; got != "s1" {
		t.Errorf("session = %v, want %q (should carry triggering session, not hardcoded 'wms')", got, "s1")
	}
}

// TestEmitCloseOutWarning_NoWarnWhenSatisfied is the negative case: a workunit
// with every required tag set produces no WMSCloseOutWarning record.
func TestEmitCloseOutWarning_NoWarnWhenSatisfied(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	s := &Server{
		cfg:      config.Config{Host: "testhost"},
		logFile:  f,
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker("testhost", time.Minute, time.Minute, nil),
		obsStore: &fakeCloseoutStore{
			required: []string{"work-type"},
			tags:     []wms.EntityTag{{TagKey: "work-type"}},
		},
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityWorkUnit,
		"wms_entity_id":   "wu-ok",
		"wms_new_status":  wms.StatusDone,
		"wms_session_id":  "s1",
	})

	// Give the detached goroutine time to (not) write.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if findCloseOutWarning(logPath) != nil {
			t.Fatal("unexpected WMSCloseOutWarning emitted when required tags satisfied")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEmitCloseOutWarning_InheritedFromOutcome is the regression case for the
// inheritance fix: a required key (product) is bound only on the workunit's
// parent outcome, never on the workunit itself. Before the fix this produced
// a spurious warning on nearly every close-out (MCP-KG-3) because the check
// read GetEntityTags directly; now it must count as satisfied, so no warning
// is queued and no WMSCloseOutWarning record lands.
func TestEmitCloseOutWarning_InheritedFromOutcome(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	s := &Server{
		cfg:      config.Config{Host: "testhost"},
		logFile:  f,
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker("testhost", time.Minute, time.Minute, nil),
		obsStore: &fakeCloseoutStore{
			required:    []string{"product"},
			outcomeID:   "out-1",
			outcomeTags: []wms.EntityTag{{TagKey: "product", TagValue: "teamster"}},
		},
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityWorkUnit,
		"wms_entity_id":   "wu-inherit",
		"wms_new_status":  wms.StatusDone,
		"wms_session_id":  "s1",
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if findCloseOutWarning(logPath) != nil {
			t.Fatal("unexpected WMSCloseOutWarning emitted when the required tag is satisfied via outcome inheritance")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEmitCloseOutWarning_MissingOnWorkUnitAndOutcome is the negative case:
// the required key is bound on neither the workunit nor its parent outcome,
// so the warning must still fire.
func TestEmitCloseOutWarning_MissingOnWorkUnitAndOutcome(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	s := &Server{
		cfg:      config.Config{Host: "testhost"},
		logFile:  f,
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker("testhost", time.Minute, time.Minute, nil),
		obsStore: &fakeCloseoutStore{
			required:  []string{"product"},
			outcomeID: "out-1",
			// tags and outcomeTags both left empty: product is bound nowhere.
		},
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityWorkUnit,
		"wms_entity_id":   "wu-neither",
		"wms_new_status":  wms.StatusDone,
		"wms_session_id":  "s1",
	})

	rec := waitForCloseOutWarning(t, logPath)
	missing, _ := rec["missing"].([]interface{})
	if len(missing) != 1 || missing[0] != "product" {
		t.Errorf("missing = %v, want [product]", rec["missing"])
	}
}

// TestEmitCloseOutWarning_WorkUnitBindingShadowsOutcome: the workunit carries
// its own binding for the required key (a different value than the outcome's)
// — no warning. This is a regression guard for the direct-binding case
// through the real warning path (missingRequiredKeys keys on TagKey only, so
// it can't itself distinguish "satisfied by the workunit's own value" from
// "satisfied by the outcome's" — that distinction is what
// TestResolveEntityTags_WorkUnitOwnBindingShadowsOutcome in internal/wms
// proves, asserting TagValue/Inherited/Origin directly).
func TestEmitCloseOutWarning_WorkUnitBindingShadowsOutcome(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	s := &Server{
		cfg:      config.Config{Host: "testhost"},
		logFile:  f,
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker("testhost", time.Minute, time.Minute, nil),
		obsStore: &fakeCloseoutStore{
			required:    []string{"product"},
			tags:        []wms.EntityTag{{TagKey: "product", TagValue: "wms-hygiene"}},
			outcomeID:   "out-1",
			outcomeTags: []wms.EntityTag{{TagKey: "product", TagValue: "teamster"}},
		},
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityWorkUnit,
		"wms_entity_id":   "wu-shadow",
		"wms_new_status":  wms.StatusDone,
		"wms_session_id":  "s1",
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if findCloseOutWarning(logPath) != nil {
			t.Fatal("unexpected WMSCloseOutWarning emitted when the workunit's own binding satisfies the required key")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWarnMissingRequiredTags_SweepIdentity_NoQueue is the regression case for
// wh2-sweep-warning-queue: a close posted under the review sweep's fixed,
// non-live identity (wms.ReviewSweepAgentID as both session and agent, exactly
// as recordSweepClose in cmd/teamster/wms_review_sweep.go sets them) must still
// land its WMSCloseOutWarning JSONL record, but must NOT queue an agent-facing
// nudge — nothing ever calls consume/clearAgent/clearSession for that
// (session, agent) pair, so a queued entry there is permanently unreachable.
// queue() runs (if at all) synchronously before emitCloseOutWarning inside the
// same detached goroutine, so observing the JSONL record first makes the
// consume() check below race-free: whatever queue() did has already happened.
func TestWarnMissingRequiredTags_SweepIdentity_NoQueue(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	s := &Server{
		cfg:      config.Config{Host: "testhost"},
		logFile:  f,
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker("testhost", time.Minute, time.Minute, nil),
		obsStore: &fakeCloseoutStore{
			required: []string{"work-type"},
			// tags left empty: work-type missing, exactly like an unreviewed
			// sweep-closed workunit.
		},
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityWorkUnit,
		"wms_entity_id":   "wu-sweep",
		"wms_new_status":  wms.StatusDone,
		"wms_session_id":  wms.ReviewSweepAgentID,
		"wms_agent_name":  wms.ReviewSweepAgentID,
	})

	rec := waitForCloseOutWarning(t, logPath)
	if got := rec["entity_id"]; got != "wu-sweep" {
		t.Errorf("entity_id = %v, want %q", got, "wu-sweep")
	}
	// Not just "at least one" — the audit trail (constraint 1) must be
	// exactly the single record this one close produces. A test that only
	// checked presence would pass just as well if the guard's queue() were
	// accidentally left in place emitting the record twice, or if some other
	// duplicate-emit defect crept in; count is the assertion that actually
	// pins "the audit trail is unaffected" rather than merely "not deleted."
	if n := countCloseOutWarnings(logPath); n != 1 {
		t.Fatalf("found %d WMSCloseOutWarning record(s) for the sweep-identity close, want exactly 1", n)
	}

	if queued := s.wmsWarnings.consume(wms.ReviewSweepAgentID, agentNameFor(wms.ReviewSweepAgentID)); queued != "" {
		t.Errorf("wmsWarningQueue holds an entry for the sweep's fixed identity, which can never consume it: %q", queued)
	}
}

// TestWarnMissingRequiredTags_LiveSession_StillQueued is the positive control
// for the same fix: an ordinary live session's close-out warning must still
// queue exactly as before, and be consumable (the shape PreToolUse's
// consume() call exercises).
func TestWarnMissingRequiredTags_LiveSession_StillQueued(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	s := &Server{
		cfg:      config.Config{Host: "testhost"},
		logFile:  f,
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker("testhost", time.Minute, time.Minute, nil),
		obsStore: &fakeCloseoutStore{
			required: []string{"work-type"},
		},
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityWorkUnit,
		"wms_entity_id":   "wu-live",
		"wms_new_status":  wms.StatusDone,
		"wms_session_id":  "sess-live-1",
		"wms_agent_name":  "scout",
	})

	waitForCloseOutWarning(t, logPath)

	queued := s.wmsWarnings.consume("sess-live-1", agentNameFor("scout"))
	if queued == "" {
		t.Fatal("expected a queued close-out warning for a live session, got none")
	}
	if !strings.Contains(queued, "wu-live") {
		t.Errorf("queued warning = %q, want it to mention wu-live", queued)
	}
	// consume() is destructive (matches PreToolUse's real usage) — a second
	// call must find nothing left, proving this was a real dequeue.
	if again := s.wmsWarnings.consume("sess-live-1", agentNameFor("scout")); again != "" {
		t.Errorf("consume() after consume() returned %q, want empty (queue not actually drained)", again)
	}
}

// waitForCloseOutWarning polls the JSONL log until a WMSCloseOutWarning record
// appears (the emit runs in a detached goroutine) or a 2s deadline elapses.
func waitForCloseOutWarning(t *testing.T, logPath string) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec := findCloseOutWarning(logPath); rec != nil {
			return rec
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no WMSCloseOutWarning record in %s within deadline", logPath)
	return nil
}

// countCloseOutWarnings scans the JSONL log and counts every record whose
// event field is WMSCloseOutWarning. Used where "a record was written" is
// not a strong enough assertion and the test needs "exactly one."
func countCloseOutWarnings(logPath string) int {
	f, err := os.Open(logPath)
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	n := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["event"] == "WMSCloseOutWarning" {
			n++
		}
	}
	return n
}

// findCloseOutWarning scans the JSONL log for a record whose event field is
// WMSCloseOutWarning and returns it, or nil if none is present.
func findCloseOutWarning(logPath string) map[string]interface{} {
	f, err := os.Open(logPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["event"] == "WMSCloseOutWarning" {
			return rec
		}
	}
	return nil
}
