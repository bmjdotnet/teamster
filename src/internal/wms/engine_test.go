package wms_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/bmjdotnet/teamster/internal/store/mysql"
	"github.com/bmjdotnet/teamster/internal/store/testguard"
	"github.com/bmjdotnet/teamster/internal/wms"
)

var mysqlSchemaCounter int64

func testEngine(t *testing.T) (*wms.EngineImpl, wms.Store) {
	t.Helper()
	dsn := testguard.RequireDSN(t)
	schema := fmt.Sprintf("teamster_engtest_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&mysqlSchemaCounter, 1))
	if err := engineMySQLEnsureSchema(dsn, schema); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	schemaDSN, err := engineMySQLRebindSchema(dsn, schema)
	if err != nil {
		t.Fatalf("rebind dsn: %v", err)
	}
	s, err := mysql.New(schemaDSN)
	if err != nil {
		t.Fatalf("mysql open: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		_ = engineMySQLDropSchema(dsn, schema)
	})
	return wms.NewEngine(s, nil), s
}


func engineMySQLEnsureSchema(dsn, schema string) error {
	serverDSN, err := engineMySQLRebindSchema(dsn, "")
	if err != nil {
		return err
	}
	db, err := engineMySQLConnect(serverDSN)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("CREATE DATABASE IF NOT EXISTS `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	return err
}

func engineMySQLDropSchema(dsn, schema string) error {
	serverDSN, err := engineMySQLRebindSchema(dsn, "")
	if err != nil {
		return err
	}
	db, err := engineMySQLConnect(serverDSN)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	return err
}

func engineMySQLRebindSchema(dsn, schema string) (string, error) {
	rest := strings.TrimPrefix(dsn, "mysql://")
	atIdx := strings.LastIndex(rest, "@")
	if atIdx < 0 {
		return "", fmt.Errorf("mysql DSN missing '@': %q", dsn)
	}
	creds := rest[:atIdx]
	hostpath := rest[atIdx+1:]
	hostport, dbAndQuery, _ := strings.Cut(hostpath, "/")
	_, query, _ := strings.Cut(dbAndQuery, "?")
	out := "mysql://" + creds + "@" + hostport + "/" + schema
	if query != "" {
		out += "?" + query
	}
	return out, nil
}

func engineMySQLConnect(dsn string) (*sql.DB, error) {
	rest := strings.TrimPrefix(dsn, "mysql://")
	atIdx := strings.LastIndex(rest, "@")
	if atIdx < 0 {
		return nil, fmt.Errorf("mysql DSN missing '@': %q", dsn)
	}
	creds := rest[:atIdx]
	hostpath := rest[atIdx+1:]
	hostport, dbname, _ := strings.Cut(hostpath, "/")
	dbname, _, _ = strings.Cut(dbname, "?")
	user, pass, _ := strings.Cut(creds, ":")
	cfg := mysqldriver.NewConfig()
	cfg.User = user
	cfg.Passwd = pass
	cfg.Net = "tcp"
	cfg.Addr = hostport
	cfg.DBName = dbname
	cfg.ParseTime = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return db, nil
}

// TestWorkUnitRollup verifies that WP7 removed the WorkUnit→Outcome rollup:
// completing every sibling WorkUnit under an Outcome must NOT auto-complete
// the Outcome. Regression test for a cascade the operator ruled out (R1) —
// an absent assertion proves nothing about removal, so this proves the
// Outcome's status is provably unchanged, not merely that nothing panicked.
func TestWorkUnitRollup(t *testing.T) {
	eng, s := testEngine(t)
	ctx := context.Background()

	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "rollup outcome", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu1", Title: "unit1", OutcomeID: "o1", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu2", Title: "unit2", OutcomeID: "o1", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}

	// Complete first unit.
	if err := s.UpdateWorkUnitStatus(ctx, "wu1", wms.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{EntityType: wms.EntityWorkUnit, EntityID: "wu1", OldStatus: wms.StatusActive, NewStatus: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	outcome, err := s.GetOutcome(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status == wms.StatusDone {
		t.Fatal("outcome should not be done while wu2 is still active")
	}

	// Complete the last sibling too — the Outcome must still NOT auto-complete.
	if err := s.UpdateWorkUnitStatus(ctx, "wu2", wms.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{EntityType: wms.EntityWorkUnit, EntityID: "wu2", OldStatus: wms.StatusActive, NewStatus: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	outcome, err = s.GetOutcome(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != wms.StatusActive {
		t.Fatalf("cascade removed by WP7: outcome must stay %q after all units complete, got %q", wms.StatusActive, outcome.Status)
	}
}

// TestOutcomeDAGRollup verifies that WP7 removed the Outcome→parent Outcome
// (DAG) rollup: completing every child Outcome must NOT auto-complete the
// parent. Regression test for the same R1 removal, DAG-parent shape.
func TestOutcomeDAGRollup(t *testing.T) {
	eng, s := testEngine(t)
	ctx := context.Background()

	// parent → child1, child2
	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "parent", Title: "parent", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "child1", Title: "child1", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "child2", Title: "child2", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddOutcomeEdge(ctx, "parent", "child1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddOutcomeEdge(ctx, "parent", "child2"); err != nil {
		t.Fatal(err)
	}

	// Complete child1 — parent should remain active.
	if err := s.UpdateOutcomeStatus(ctx, "child1", wms.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{EntityType: wms.EntityOutcome, EntityID: "child1", OldStatus: wms.StatusActive, NewStatus: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	parent, err := s.GetOutcome(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if parent.Status == wms.StatusDone {
		t.Fatal("parent should not be done while child2 is still active")
	}

	// Complete child2 too — the parent must still NOT auto-complete.
	if err := s.UpdateOutcomeStatus(ctx, "child2", wms.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{EntityType: wms.EntityOutcome, EntityID: "child2", OldStatus: wms.StatusActive, NewStatus: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	parent, err = s.GetOutcome(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if parent.Status != wms.StatusActive {
		t.Fatalf("cascade removed by WP7: parent outcome must stay %q after all children complete, got %q", wms.StatusActive, parent.Status)
	}
}

// TestWorkUnitDependencyCascade verifies that completing a blocking WorkUnit
// unblocks the dependent WorkUnit.
func TestWorkUnitDependencyCascade(t *testing.T) {
	eng, s := testEngine(t)
	ctx := context.Background()

	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "outcome", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-blocker", Title: "blocker", OutcomeID: "o1", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-blocked", Title: "blocked", OutcomeID: "o1", Status: wms.StatusBlocked}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEntityDependency(ctx, &wms.Dependency{
		BlockerType: wms.EntityWorkUnit, BlockerID: "wu-blocker",
		BlockedType: wms.EntityWorkUnit, BlockedID: "wu-blocked",
	}); err != nil {
		t.Fatal(err)
	}

	// Complete the blocker and trigger the engine.
	if err := s.UpdateWorkUnitStatus(ctx, "wu-blocker", wms.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{EntityType: wms.EntityWorkUnit, EntityID: "wu-blocker", OldStatus: wms.StatusActive, NewStatus: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}

	blocked, err := s.GetWorkUnit(ctx, "wu-blocked")
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Status == wms.StatusBlocked {
		t.Fatal("wu-blocked should have been unblocked after wu-blocker completed")
	}
}

// TestEvaluateUnblock_RecordsJournalEntry verifies WP10 path 4:
// evaluateUnblock's restore transition (previously a store write with no
// audit trail at all) now writes a wms_journal row via RecordMutation.
func TestEvaluateUnblock_RecordsJournalEntry(t *testing.T) {
	eng, s := testEngine(t)
	ctx := context.Background()

	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o1", Title: "outcome", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-blocker", Title: "blocker", OutcomeID: "o1", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-blocked", Title: "blocked", OutcomeID: "o1", Status: wms.StatusBlocked}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEntityDependency(ctx, &wms.Dependency{
		BlockerType: wms.EntityWorkUnit, BlockerID: "wu-blocker",
		BlockedType: wms.EntityWorkUnit, BlockedID: "wu-blocked",
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateWorkUnitStatus(ctx, "wu-blocker", wms.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{
		EntityType: wms.EntityWorkUnit, EntityID: "wu-blocker",
		OldStatus: wms.StatusActive, NewStatus: wms.StatusDone,
		SessionID: "sess-1", AgentName: "tester", Host: "host-1",
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := s.GetJournalEntries(ctx, wms.EntityWorkUnit, "wu-blocked", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 journal entry for the auto-unblock, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.OldValue != wms.StatusBlocked || e.NewValue != wms.StatusPending {
		t.Fatalf("expected blocked→pending, got %s→%s", e.OldValue, e.NewValue)
	}
	if !strings.Contains(e.Notes, "auto-unblocked") {
		t.Fatalf("expected notes to explain the auto-unblock, got %q", e.Notes)
	}
	if e.SessionID != "sess-1" || e.AgentID != "tester" || e.Host != "host-1" {
		t.Fatalf("expected attribution to carry through from the triggering change, got %+v", e)
	}
}

type recordingObserver struct {
	changes []wms.StatusChange
}

func (r *recordingObserver) OnStatusChange(c wms.StatusChange) { r.changes = append(r.changes, c) }
func (r *recordingObserver) OnFocusChange(_ wms.FocusUpdate)   {}

// TestObserverCalled verifies that all registered observers receive status changes.
func TestObserverCalled(t *testing.T) {
	eng, _ := testEngine(t)
	ctx := context.Background()

	obs := &recordingObserver{}
	eng.AddObserver(obs)

	change := wms.StatusChange{EntityType: wms.EntityOutcome, EntityID: "o-nonexistent", OldStatus: wms.StatusPending, NewStatus: wms.StatusActive}
	if err := eng.OnStatusChange(ctx, change); err != nil {
		t.Fatal(err)
	}

	if len(obs.changes) != 1 {
		t.Fatalf("expected 1 observer call, got %d", len(obs.changes))
	}
	if obs.changes[0] != change {
		t.Fatalf("observer received wrong change: %+v", obs.changes[0])
	}
}

// The following tests pin the LF-gfx-1 fix (VERIFY.md addendum §F): the
// engine clears a stale `resolution` tag on the done→review reopen edge, the
// sole edge leaving `done` (R2), so a later abandon can never carry a
// leftover `resolution:achieved`.

// TestClearResolutionOnReopen_Outcome_ClearsTagAndJournalsValue: done (with
// resolution:achieved) → review clears the tag and journals the cleared
// value in notes, attributed to the triggering change.
func TestClearResolutionOnReopen_Outcome_ClearsTagAndJournalsValue(t *testing.T) {
	eng, s := testEngine(t)
	ctx := context.Background()

	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o-reopen", Title: "reopen me", Status: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	if err := s.TagEntity(ctx, wms.EntityOutcome, "o-reopen", "resolution", "achieved", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateOutcomeStatus(ctx, "o-reopen", wms.StatusReview); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{
		EntityType: wms.EntityOutcome, EntityID: "o-reopen",
		OldStatus: wms.StatusDone, NewStatus: wms.StatusReview,
		SessionID: "sess-1", AgentName: "tester", Host: "host-1",
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := s.GetEntityTags(ctx, wms.EntityOutcome, "o-reopen")
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range tags {
		if tg.TagKey == "resolution" {
			t.Fatalf("resolution tag should have been cleared on reopen, still bound: %+v", tg)
		}
	}

	entries, err := s.GetJournalEntries(ctx, wms.EntityOutcome, "o-reopen", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 journal entry for the reopen clear, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.OldValue != "achieved" || e.NewValue != "" {
		t.Fatalf("expected achieved→\"\", got %s→%s", e.OldValue, e.NewValue)
	}
	if !strings.Contains(e.Notes, "achieved") {
		t.Fatalf("expected notes to record the cleared value, got %q", e.Notes)
	}
	if e.SessionID != "sess-1" || e.AgentID != "tester" || e.Host != "host-1" {
		t.Fatalf("expected attribution to carry through from the triggering change, got %+v", e)
	}
}

// TestClearResolutionOnReopen_JournalOrdering_CauseBeforeEffect pins the
// adversarial-review fix: the reopen's own "status" journal row (written by
// JournalObserver, when registered) must be inserted before the resolution
// tag's "cleared" row it causes, so a reader reconstructing wms_journal by
// insertion order sees cause before effect. testEngine registers no
// JournalObserver by default (see other tests in this file, whose "exactly
// 1 journal entry" assertions depend on that), so this test wires one in
// explicitly to exercise the interaction the other tests can't see.
func TestClearResolutionOnReopen_JournalOrdering_CauseBeforeEffect(t *testing.T) {
	eng, s := testEngine(t)
	ctx := context.Background()
	eng.AddObserver(wms.NewJournalObserver(s))

	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o-order", Title: "ordering", Status: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	if err := s.TagEntity(ctx, wms.EntityOutcome, "o-order", "resolution", "achieved", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateOutcomeStatus(ctx, "o-order", wms.StatusReview); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{
		EntityType: wms.EntityOutcome, EntityID: "o-order",
		OldStatus: wms.StatusDone, NewStatus: wms.StatusReview,
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := s.GetJournalEntries(ctx, wms.EntityOutcome, "o-order", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 journal entries (status + resolution clear), got %d: %+v", len(entries), entries)
	}
	// GetJournalEntries sorts by created_at DESC (display order, most recent
	// first) — sort our copy ascending by the monotonic ID so this assertion
	// is about insertion/causal order, independent of that display choice.
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	if entries[0].Field != "status" || entries[0].NewValue != wms.StatusReview {
		t.Fatalf("expected the reopen's own status row to be written first (cause), got %+v", entries[0])
	}
	if entries[1].Field != "resolution" {
		t.Fatalf("expected the resolution clear to be written second (effect), got %+v", entries[1])
	}
}

// TestClearResolutionOnReopen_ThenAbandon_TagStaysCleared: the LF-gfx-1
// repro end to end — done→review clears the tag, and a subsequent
// review→abandoned never resurrects it. Regression test for the corrupted
// entity (`abandoned` status still asserting `resolution:achieved`).
func TestClearResolutionOnReopen_ThenAbandon_TagStaysCleared(t *testing.T) {
	eng, s := testEngine(t)
	ctx := context.Background()

	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o-reopen-abandon", Title: "reopen then abandon", Status: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	if err := s.TagEntity(ctx, wms.EntityOutcome, "o-reopen-abandon", "resolution", "achieved", "manual", ""); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateOutcomeStatus(ctx, "o-reopen-abandon", wms.StatusReview); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{
		EntityType: wms.EntityOutcome, EntityID: "o-reopen-abandon",
		OldStatus: wms.StatusDone, NewStatus: wms.StatusReview,
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateOutcomeStatus(ctx, "o-reopen-abandon", wms.StatusAbandoned); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{
		EntityType: wms.EntityOutcome, EntityID: "o-reopen-abandon",
		OldStatus: wms.StatusReview, NewStatus: wms.StatusAbandoned,
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := s.GetEntityTags(ctx, wms.EntityOutcome, "o-reopen-abandon")
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range tags {
		if tg.TagKey == "resolution" {
			t.Fatalf("LF-gfx-1 regression: outcome abandoned via done→review→abandoned still carries resolution:%s", tg.TagValue)
		}
	}

	// Still exactly 1 journal row, from the reopen clear — abandon makes no
	// second attempt since the tag is already gone by then.
	entries, err := s.GetJournalEntries(ctx, wms.EntityOutcome, "o-reopen-abandon", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 journal entry total, got %d: %+v", len(entries), entries)
	}
}

// TestClearResolutionOnReopen_NoTag_NoOp: done (no resolution tag) → review
// writes no untag journal row — a no-op stays silent, no journal noise.
func TestClearResolutionOnReopen_NoTag_NoOp(t *testing.T) {
	eng, s := testEngine(t)
	ctx := context.Background()

	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o-no-tag", Title: "never tagged", Status: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateOutcomeStatus(ctx, "o-no-tag", wms.StatusReview); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{
		EntityType: wms.EntityOutcome, EntityID: "o-no-tag",
		OldStatus: wms.StatusDone, NewStatus: wms.StatusReview,
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := s.GetJournalEntries(ctx, wms.EntityOutcome, "o-no-tag", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no journal row when there was no resolution tag to clear, got %d: %+v", len(entries), entries)
	}
}

// TestClearResolutionOnReopen_WorkUnit_ClearsTagAndJournals: a WorkUnit
// reopen behaves identically to an Outcome reopen.
func TestClearResolutionOnReopen_WorkUnit_ClearsTagAndJournals(t *testing.T) {
	eng, s := testEngine(t)
	ctx := context.Background()

	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o-wu-reopen", Title: "parent", Status: wms.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: "wu-reopen", OutcomeID: "o-wu-reopen", Title: "unit", Status: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	if err := s.TagEntity(ctx, wms.EntityWorkUnit, "wu-reopen", "resolution", "achieved", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWorkUnitStatus(ctx, "wu-reopen", wms.StatusReview); err != nil {
		t.Fatal(err)
	}
	if err := eng.OnStatusChange(ctx, wms.StatusChange{
		EntityType: wms.EntityWorkUnit, EntityID: "wu-reopen",
		OldStatus: wms.StatusDone, NewStatus: wms.StatusReview,
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := s.GetEntityTags(ctx, wms.EntityWorkUnit, "wu-reopen")
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range tags {
		if tg.TagKey == "resolution" {
			t.Fatalf("resolution tag should have been cleared on reopen, still bound: %+v", tg)
		}
	}

	entries, err := s.GetJournalEntries(ctx, wms.EntityWorkUnit, "wu-reopen", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 journal entry for the reopen clear, got %d: %+v", len(entries), entries)
	}
	if entries[0].OldValue != "achieved" || entries[0].NewValue != "" {
		t.Fatalf("expected achieved→\"\", got %s→%s", entries[0].OldValue, entries[0].NewValue)
	}
}

// failOnDeleteEntityTag wraps a wms.Store and fails DeleteEntityTag with an
// injected error, passing every other call straight through via interface
// embedding (same pattern as internal/mcp/wms's failOnGetOutcome).
type failOnDeleteEntityTag struct {
	wms.Store
	injectedErr error
}

func (f *failOnDeleteEntityTag) DeleteEntityTag(_ context.Context, _, _, _, _ string) error {
	return f.injectedErr
}

// TestClearResolutionOnReopen_UntagFailureNeverBlocksReopen pins the
// swallow-and-log posture: a failed DeleteEntityTag must never surface as an
// OnStatusChange error, and must never write a journal row claiming a clear
// that didn't happen.
func TestClearResolutionOnReopen_UntagFailureNeverBlocksReopen(t *testing.T) {
	_, s := testEngine(t)
	ctx := context.Background()

	if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "o-untag-fail", Title: "fails to untag", Status: wms.StatusDone}); err != nil {
		t.Fatal(err)
	}
	if err := s.TagEntity(ctx, wms.EntityOutcome, "o-untag-fail", "resolution", "achieved", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateOutcomeStatus(ctx, "o-untag-fail", wms.StatusReview); err != nil {
		t.Fatal(err)
	}

	failing := &failOnDeleteEntityTag{Store: s, injectedErr: errors.New("injected: delete failed")}
	eng := wms.NewEngine(failing, nil)

	if err := eng.OnStatusChange(ctx, wms.StatusChange{
		EntityType: wms.EntityOutcome, EntityID: "o-untag-fail",
		OldStatus: wms.StatusDone, NewStatus: wms.StatusReview,
	}); err != nil {
		t.Fatalf("OnStatusChange must not fail when the tag clear fails, got: %v", err)
	}

	tags, err := s.GetEntityTags(ctx, wms.EntityOutcome, "o-untag-fail")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tg := range tags {
		if tg.TagKey == "resolution" && tg.TagValue == "achieved" {
			found = true
		}
	}
	if !found {
		t.Fatal("resolution tag should still be bound since DeleteEntityTag was injected to fail")
	}

	entries, err := s.GetJournalEntries(ctx, wms.EntityOutcome, "o-untag-fail", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no journal row for a failed clear, got %d: %+v", len(entries), entries)
	}
}
