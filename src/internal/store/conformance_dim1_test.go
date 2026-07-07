// Conformance dimension 1 (07-conformance.md): CRUD round-trip for every
// entity a backend must persist, run through the shared backends() harness
// (store_test.go) so Phase 16's sqlite entry exercises the same cases with
// zero test-code changes. Session and ActivityEvent round-trips already live
// in store_test.go (TestSessionRoundTrip, TestActivityEventOrdering, etc.) —
// not duplicated here.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// TestConformanceDim1_Prober is dimension 1's mandated one-liner (phase-15
// step 1 / F10, 01-interfaces.md's Prober subsection): every backend must
// answer Ping against a live, reachable connection.
func TestConformanceDim1_Prober(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		if err := s.Ping(context.Background()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	})
}

// TestConformanceDim1_Outcome is the Outcome CRUD round-trip: create → read
// back → field-equality → update (status, focus) → close (done).
func TestConformanceDim1_Outcome(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		want := &wms.Outcome{
			ID:          "dim1-o1",
			Title:       "Dim1 Outcome",
			Description: "conformance fixture",
			Status:      wms.StatusPending,
			Focus:       "initial focus",
			OriginHost:  "host-a",
		}
		if err := s.CreateOutcome(ctx, want); err != nil {
			t.Fatalf("CreateOutcome: %v", err)
		}
		got, err := s.GetOutcome(ctx, "dim1-o1")
		if err != nil {
			t.Fatalf("GetOutcome: %v", err)
		}
		if got.Title != want.Title || got.Description != want.Description ||
			got.Status != want.Status || got.Focus != want.Focus || got.OriginHost != want.OriginHost {
			t.Fatalf("round-trip mismatch: got %+v, want fields of %+v", got, want)
		}
		if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Fatalf("timestamps not populated: %+v", got)
		}

		if err := s.UpdateOutcomeStatus(ctx, "dim1-o1", wms.StatusActive); err != nil {
			t.Fatalf("UpdateOutcomeStatus: %v", err)
		}
		if err := s.UpdateOutcomeFocus(ctx, "dim1-o1", "revised focus"); err != nil {
			t.Fatalf("UpdateOutcomeFocus: %v", err)
		}
		got, err = s.GetOutcome(ctx, "dim1-o1")
		if err != nil {
			t.Fatalf("GetOutcome after update: %v", err)
		}
		if got.Status != wms.StatusActive || got.Focus != "revised focus" {
			t.Fatalf("update did not persist: %+v", got)
		}

		// Close: an outcome's terminal state is 'done', not a row delete.
		if err := s.UpdateOutcomeStatus(ctx, "dim1-o1", wms.StatusDone); err != nil {
			t.Fatalf("UpdateOutcomeStatus(done): %v", err)
		}
		got, err = s.GetOutcome(ctx, "dim1-o1")
		if err != nil {
			t.Fatalf("GetOutcome after close: %v", err)
		}
		if got.Status != wms.StatusDone {
			t.Fatalf("close status = %q, want done", got.Status)
		}
	})
}

// TestConformanceDim1_WorkUnit is the WorkUnit CRUD round-trip, including
// AssignWorkUnit/ClaimWorkUnit as additional update paths.
func TestConformanceDim1_WorkUnit(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "dim1-wu-o1", Title: "O", Status: wms.StatusActive}); err != nil {
			t.Fatalf("CreateOutcome: %v", err)
		}
		want := &wms.WorkUnit{
			ID:          "dim1-wu1",
			OutcomeID:   "dim1-wu-o1",
			Title:       "Dim1 WorkUnit",
			Description: "conformance fixture",
			Status:      wms.StatusPending,
			Focus:       "initial",
		}
		if err := s.CreateWorkUnit(ctx, want); err != nil {
			t.Fatalf("CreateWorkUnit: %v", err)
		}
		got, err := s.GetWorkUnit(ctx, "dim1-wu1")
		if err != nil {
			t.Fatalf("GetWorkUnit: %v", err)
		}
		if got.OutcomeID != want.OutcomeID || got.Title != want.Title ||
			got.Description != want.Description || got.Status != want.Status || got.Focus != want.Focus {
			t.Fatalf("round-trip mismatch: got %+v, want fields of %+v", got, want)
		}

		if err := s.AssignWorkUnit(ctx, "dim1-wu1", "@dim1-agent"); err != nil {
			t.Fatalf("AssignWorkUnit: %v", err)
		}
		if err := s.ClaimWorkUnit(ctx, "dim1-wu1", "@dim1-agent"); err != nil {
			t.Fatalf("ClaimWorkUnit: %v", err)
		}
		if err := s.UpdateWorkUnitFocus(ctx, "dim1-wu1", "revised"); err != nil {
			t.Fatalf("UpdateWorkUnitFocus: %v", err)
		}
		got, err = s.GetWorkUnit(ctx, "dim1-wu1")
		if err != nil {
			t.Fatalf("GetWorkUnit after update: %v", err)
		}
		if got.AgentID != "@dim1-agent" || got.Status != wms.StatusActive || got.Focus != "revised" {
			t.Fatalf("update did not persist: %+v", got)
		}

		// Close.
		if err := s.UpdateWorkUnitStatus(ctx, "dim1-wu1", wms.StatusDone); err != nil {
			t.Fatalf("UpdateWorkUnitStatus(done): %v", err)
		}
		got, err = s.GetWorkUnit(ctx, "dim1-wu1")
		if err != nil {
			t.Fatalf("GetWorkUnit after close: %v", err)
		}
		if got.Status != wms.StatusDone {
			t.Fatalf("close status = %q, want done", got.Status)
		}
	})
}

// TestConformanceDim1_TagAndEntityTag round-trips a tag binding: apply →
// read back (content + category + source) → delete → confirm gone.
func TestConformanceDim1_TagAndEntityTag(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "dim1-tag-o1", Title: "O", Status: wms.StatusActive}); err != nil {
			t.Fatalf("CreateOutcome: %v", err)
		}
		if err := s.TagEntity(ctx, wms.EntityOutcome, "dim1-tag-o1", "dim1key", "dim1val", "manual", "a fixture tag"); err != nil {
			t.Fatalf("TagEntity: %v", err)
		}
		ets, err := s.GetEntityTags(ctx, wms.EntityOutcome, "dim1-tag-o1")
		if err != nil {
			t.Fatalf("GetEntityTags: %v", err)
		}
		var found *wms.EntityTag
		for i := range ets {
			if ets[i].TagKey == "dim1key" && ets[i].TagValue == "dim1val" {
				found = &ets[i]
			}
		}
		if found == nil {
			t.Fatalf("dim1key=dim1val not found in %+v", ets)
		}
		if found.Source != "manual" || found.Description != "a fixture tag" || found.Category != "context" {
			t.Fatalf("tag fields mismatch: %+v", found)
		}

		if err := s.DeleteEntityTag(ctx, wms.EntityOutcome, "dim1-tag-o1", "dim1key", "dim1val"); err != nil {
			t.Fatalf("DeleteEntityTag: %v", err)
		}
		ets, err = s.GetEntityTags(ctx, wms.EntityOutcome, "dim1-tag-o1")
		if err != nil {
			t.Fatalf("GetEntityTags after delete: %v", err)
		}
		for _, et := range ets {
			if et.TagKey == "dim1key" {
				t.Fatalf("dim1key still present after DeleteEntityTag: %+v", ets)
			}
		}
	})
}

// TestConformanceDim1_EventRecord round-trips a kind='state' interval: open →
// read (GetOpenEventRecord) → transition (closes prior, opens new) → list
// (both records present, prior closed with EndedAt set).
func TestConformanceDim1_EventRecord(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "dim1-ev-o1", Title: "O", Status: wms.StatusPending}); err != nil {
			t.Fatalf("CreateOutcome: %v", err)
		}
		if err := s.OpenEventRecord(ctx, wms.EntityOutcome, "dim1-ev-o1", wms.StatusPending, "dim1-sess", "@dim1", "h"); err != nil {
			t.Fatalf("OpenEventRecord: %v", err)
		}
		open, err := s.GetOpenEventRecord(ctx, wms.EntityOutcome, "dim1-ev-o1")
		if err != nil {
			t.Fatalf("GetOpenEventRecord: %v", err)
		}
		if open.State != wms.StatusPending || open.EndedAt != nil {
			t.Fatalf("initial open record wrong: %+v", open)
		}

		if err := s.TransitionEventRecord(ctx, wms.EntityOutcome, "dim1-ev-o1", wms.StatusActive, "dim1-sess", "@dim1", "h"); err != nil {
			t.Fatalf("TransitionEventRecord: %v", err)
		}
		recs, err := s.ListEventRecords(ctx, wms.EntityOutcome, "dim1-ev-o1", 50)
		if err != nil {
			t.Fatalf("ListEventRecords: %v", err)
		}
		if len(recs) != 2 {
			t.Fatalf("expected 2 event records after transition, got %d: %+v", len(recs), recs)
		}
		var sawClosedPending, sawOpenActive bool
		for _, r := range recs {
			switch r.State {
			case wms.StatusPending:
				if r.EndedAt == nil {
					t.Fatalf("prior pending record must be closed: %+v", r)
				}
				sawClosedPending = true
			case wms.StatusActive:
				if r.EndedAt != nil {
					t.Fatalf("new active record must be open: %+v", r)
				}
				sawOpenActive = true
			}
		}
		if !sawClosedPending || !sawOpenActive {
			t.Fatalf("expected closed-pending + open-active, got %+v", recs)
		}
	})
}

// TestConformanceDim1_FocusInterval round-trips a kind='focus' interval:
// open → confirm via HasAnyFocusInterval → close → confirm ended.
func TestConformanceDim1_FocusInterval(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		key := store.SessionKey{SessionID: "dim1-focus-sess", AgentName: "@dim1"}
		if err := s.UpsertSession(ctx, store.Session{SessionID: key.SessionID, AgentName: key.AgentName, Host: "h"}); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
		has, err := s.HasAnyFocusInterval(ctx, key)
		if err != nil {
			t.Fatalf("HasAnyFocusInterval (none yet): %v", err)
		}
		if has {
			t.Fatalf("expected no focus interval before OpenFocusInterval")
		}

		if err := s.OpenFocusInterval(ctx, key, wms.EntityWorkUnit, "dim1-focus-wu"); err != nil {
			t.Fatalf("OpenFocusInterval: %v", err)
		}
		has, err = s.HasAnyFocusInterval(ctx, key)
		if err != nil {
			t.Fatalf("HasAnyFocusInterval (after open): %v", err)
		}
		if !has {
			t.Fatalf("expected a focus interval after OpenFocusInterval")
		}

		if err := s.CloseFocusInterval(ctx, key); err != nil {
			t.Fatalf("CloseFocusInterval: %v", err)
		}
		n, err := s.CloseSessionIntervals(ctx, key.SessionID, key.AgentName, time.Now().UTC())
		if err != nil {
			t.Fatalf("CloseSessionIntervals (idempotent no-op check): %v", err)
		}
		if n != 0 {
			t.Fatalf("expected 0 intervals still open after CloseFocusInterval, closed %d more", n)
		}
	})
}

// TestConformanceDim1_Journal round-trips a journal entry: write → read back
// with field-equality.
func TestConformanceDim1_Journal(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		entry := wms.JournalEntry{
			EntityType: wms.EntityOutcome,
			EntityID:   "dim1-journal-o1",
			Field:      "status",
			OldValue:   "pending",
			NewValue:   "active",
			AgentID:    "@dim1",
			Host:       "h",
			SessionID:  "dim1-sess",
			Notes:      "conformance fixture",
		}
		if err := s.WriteJournalEntry(ctx, entry); err != nil {
			t.Fatalf("WriteJournalEntry: %v", err)
		}
		got, err := s.GetJournalEntries(ctx, wms.EntityOutcome, "dim1-journal-o1", 10)
		if err != nil {
			t.Fatalf("GetJournalEntries: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 journal entry, got %d", len(got))
		}
		e := got[0]
		if e.Field != entry.Field || e.OldValue != entry.OldValue || e.NewValue != entry.NewValue ||
			e.AgentID != entry.AgentID || e.Host != entry.Host || e.SessionID != entry.SessionID || e.Notes != entry.Notes {
			t.Fatalf("round-trip mismatch: got %+v, want fields of %+v", e, entry)
		}
		if e.CreatedAt.IsZero() {
			t.Fatalf("CreatedAt not populated: %+v", e)
		}
	})
}

// TestConformanceDim1_Dependency round-trips a blocker→blocked edge: add →
// read (both directions) → remove → confirm gone.
func TestConformanceDim1_Dependency(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if err := s.CreateOutcome(ctx, &wms.Outcome{ID: "dim1-dep-o1", Title: "O", Status: wms.StatusActive}); err != nil {
			t.Fatalf("CreateOutcome: %v", err)
		}
		for _, id := range []string{"dim1-dep-a", "dim1-dep-b"} {
			if err := s.CreateWorkUnit(ctx, &wms.WorkUnit{ID: id, OutcomeID: "dim1-dep-o1", Title: id, Status: wms.StatusPending}); err != nil {
				t.Fatalf("CreateWorkUnit %s: %v", id, err)
			}
		}
		dep := &wms.Dependency{
			BlockerType: wms.EntityWorkUnit, BlockerID: "dim1-dep-a",
			BlockedType: wms.EntityWorkUnit, BlockedID: "dim1-dep-b",
		}
		if err := s.AddEntityDependency(ctx, dep); err != nil {
			t.Fatalf("AddEntityDependency: %v", err)
		}
		blockers, err := s.ListEntityDependencyBlockers(ctx, wms.EntityWorkUnit, "dim1-dep-b")
		if err != nil {
			t.Fatalf("ListEntityDependencyBlockers: %v", err)
		}
		if len(blockers) != 1 || blockers[0].BlockerID != "dim1-dep-a" {
			t.Fatalf("expected b blocked by a, got %+v", blockers)
		}
		dependents, err := s.ListEntityDependencyDependents(ctx, wms.EntityWorkUnit, "dim1-dep-a")
		if err != nil {
			t.Fatalf("ListEntityDependencyDependents: %v", err)
		}
		if len(dependents) != 1 || dependents[0].BlockedID != "dim1-dep-b" {
			t.Fatalf("expected a blocks b, got %+v", dependents)
		}

		if err := s.RemoveEntityDependency(ctx, wms.EntityWorkUnit, "dim1-dep-a", wms.EntityWorkUnit, "dim1-dep-b"); err != nil {
			t.Fatalf("RemoveEntityDependency: %v", err)
		}
		blockers, err = s.ListEntityDependencyBlockers(ctx, wms.EntityWorkUnit, "dim1-dep-b")
		if err != nil {
			t.Fatalf("ListEntityDependencyBlockers after remove: %v", err)
		}
		if len(blockers) != 0 {
			t.Fatalf("expected no blockers after remove, got %+v", blockers)
		}
	})
}

// TestConformanceDim1_TokenLedgerRow round-trips a token_ledger row: insert →
// read back (field-equality) → re-insert with higher output_tokens (the
// documented winner-wins upsert) → read back updated values.
func TestConformanceDim1_TokenLedgerRow(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		ts := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
		row := store.TelemetryRow{
			SessionID:    "dim1-ledger-sess",
			MessageID:    "dim1-msg-1",
			AgentName:    "dim1agent",
			Host:         "h",
			Model:        "claude-opus-4-8",
			InputTokens:  100,
			OutputTokens: 50,
			TotalInput:   100,
			CostUSD:      1.25,
			Timestamp:    ts,
		}
		n, err := s.UpsertTelemetryBatch(ctx, []store.TelemetryRow{row})
		if err != nil {
			t.Fatalf("UpsertTelemetryBatch: %v", err)
		}
		if n != 1 {
			t.Fatalf("UpsertTelemetryBatch wrote %d rows, want 1", n)
		}
		rows, err := s.TokenLedgerRows(ctx, ts.Add(-time.Minute))
		if err != nil {
			t.Fatalf("TokenLedgerRows: %v", err)
		}
		var found *store.LedgerRow
		for i := range rows {
			if rows[i].Timestamp.Equal(ts) {
				found = &rows[i]
			}
		}
		if found == nil {
			t.Fatalf("seeded row not found in %+v", rows)
		}
		if found.Input != row.InputTokens || found.Output != row.OutputTokens || found.CostUSD != row.CostUSD {
			t.Fatalf("round-trip mismatch: got %+v, want input=%d output=%d cost=%v",
				found, row.InputTokens, row.OutputTokens, row.CostUSD)
		}

		// Update via the documented greater-output_tokens-wins upsert.
		row.OutputTokens = 200
		row.CostUSD = 4.0
		if _, err := s.UpsertTelemetryBatch(ctx, []store.TelemetryRow{row}); err != nil {
			t.Fatalf("UpsertTelemetryBatch (update): %v", err)
		}
		rows, err = s.TokenLedgerRows(ctx, ts.Add(-time.Minute))
		if err != nil {
			t.Fatalf("TokenLedgerRows after update: %v", err)
		}
		found = nil
		for i := range rows {
			if rows[i].Timestamp.Equal(ts) {
				found = &rows[i]
			}
		}
		if found == nil {
			t.Fatalf("row missing after update: %+v", rows)
		}
		if found.Output != 200 || found.CostUSD != 4.0 {
			t.Fatalf("update did not win: got %+v", found)
		}
	})
}
