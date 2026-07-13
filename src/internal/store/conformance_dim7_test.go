// Conformance dimension 7: RosterStore CRUD, binding, token lifecycle,
// and cascade revocation. Exercises both backends via the shared run() harness.
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

func TestRosterCreateAndGet(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		sid := "sess-1"
		entry := store.RosterEntry{
			RosterID:     "r-1",
			SessionID:    &sid,
			AgentName:    "@scout",
			Host:         "host-a",
			Runtime:      "claude_code",
			Model:        "opus",
			Relationship: "teammate",
			TeamName:     "ops",
			BusTeam:      "bus-1",
			CreatedAt:    now,
			BoundAt:      &now,
		}
		if err := s.CreateRosterEntry(ctx, entry); err != nil {
			t.Fatalf("CreateRosterEntry: %v", err)
		}
		got, err := s.GetRosterEntry(ctx, "r-1")
		if err != nil {
			t.Fatalf("GetRosterEntry: %v", err)
		}
		if got.RosterID != "r-1" || got.AgentName != "@scout" || got.Host != "host-a" {
			t.Fatalf("field mismatch: %+v", got)
		}
		if got.SessionID == nil || *got.SessionID != "sess-1" {
			t.Fatalf("session_id mismatch: %v", got.SessionID)
		}
		if got.Relationship != "teammate" || got.Runtime != "claude_code" {
			t.Fatalf("relationship/runtime mismatch: %+v", got)
		}
	})
}

func TestRosterGetNotFound(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		_, err := s.GetRosterEntry(ctx, "nonexistent")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
}

func TestRosterUpsertIdempotent(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		sid := "sess-u"
		entry := store.RosterEntry{
			RosterID:     "r-u1",
			SessionID:    &sid,
			AgentName:    "@impl",
			Host:         "host-a",
			Runtime:      "claude_code",
			Relationship: "teammate",
			CreatedAt:    now,
			BoundAt:      &now,
		}
		if err := s.UpsertRosterEntry(ctx, entry); err != nil {
			t.Fatal(err)
		}

		entry.Host = "host-b"
		entry.Model = "sonnet"
		if err := s.UpsertRosterEntry(ctx, entry); err != nil {
			t.Fatal(err)
		}

		got, err := s.GetRosterEntry(ctx, "r-u1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Host != "host-b" || got.Model != "sonnet" {
			t.Fatalf("upsert did not update: %+v", got)
		}
	})
}

func TestRosterBindSession(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)

		entry := store.RosterEntry{
			RosterID:     "r-bind",
			AgentName:    "@peer",
			Host:         "host-a",
			Runtime:      "codex",
			Relationship: "peer",
			CreatedAt:    now,
		}
		if err := s.CreateRosterEntry(ctx, entry); err != nil {
			t.Fatal(err)
		}

		// Verify it's unbound.
		got, err := s.GetRosterEntry(ctx, "r-bind")
		if err != nil {
			t.Fatal(err)
		}
		if got.SessionID != nil {
			t.Fatalf("expected nil session_id, got %v", got.SessionID)
		}
		if got.BoundAt != nil {
			t.Fatalf("expected nil bound_at, got %v", got.BoundAt)
		}

		// Bind.
		if err := s.BindRosterSession(ctx, "r-bind", "sess-peer"); err != nil {
			t.Fatal(err)
		}

		got, err = s.GetRosterEntry(ctx, "r-bind")
		if err != nil {
			t.Fatal(err)
		}
		if got.SessionID == nil || *got.SessionID != "sess-peer" {
			t.Fatalf("bind failed: session_id=%v", got.SessionID)
		}
		if got.BoundAt == nil {
			t.Fatal("bound_at should be set after bind")
		}

		// Idempotent same-value bind.
		if err := s.BindRosterSession(ctx, "r-bind", "sess-peer"); err != nil {
			t.Fatalf("idempotent bind should succeed: %v", err)
		}

		// Conflict on different session_id.
		err = s.BindRosterSession(ctx, "r-bind", "different-session")
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("expected ErrConflict, got %v", err)
		}
	})
}

func TestRosterBindNotFound(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		err := s.BindRosterSession(ctx, "nonexistent", "sess")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
}

func TestRosterResolveID(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		sid := "sess-resolve"
		entry := store.RosterEntry{
			RosterID:     "r-resolve",
			SessionID:    &sid,
			AgentName:    "@worker",
			Host:         "host-a",
			Runtime:      "claude_code",
			Relationship: "teammate",
			CreatedAt:    now,
			BoundAt:      &now,
		}
		if err := s.CreateRosterEntry(ctx, entry); err != nil {
			t.Fatal(err)
		}

		id, err := s.ResolveRosterID(ctx, "sess-resolve", "@worker")
		if err != nil {
			t.Fatal(err)
		}
		if id != "r-resolve" {
			t.Fatalf("resolve got %q, want r-resolve", id)
		}

		// Not found case.
		_, err = s.ResolveRosterID(ctx, "sess-resolve", "@nobody")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
}

func TestRosterListWithFilters(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		sid1, sid2 := "s1", "s2"

		entries := []store.RosterEntry{
			{RosterID: "r-l1", SessionID: &sid1, AgentName: "", Host: "host-a", Runtime: "claude_code", Relationship: "lead", CreatedAt: now, BoundAt: &now},
			{RosterID: "r-l2", SessionID: &sid2, AgentName: "@scout", Host: "host-a", Runtime: "codex", Relationship: "peer", BusTeam: "bus-x", CreatedAt: now.Add(time.Second), BoundAt: &now},
		}
		for _, e := range entries {
			if err := s.CreateRosterEntry(ctx, e); err != nil {
				t.Fatal(err)
			}
		}

		// No filter returns all.
		all, err := s.ListRosterEntries(ctx, store.RosterFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(all) < 2 {
			t.Fatalf("expected >= 2 entries, got %d", len(all))
		}

		// Filter by relationship.
		leads, err := s.ListRosterEntries(ctx, store.RosterFilter{Relationship: "lead"})
		if err != nil {
			t.Fatal(err)
		}
		if len(leads) != 1 || leads[0].RosterID != "r-l1" {
			t.Fatalf("lead filter: got %d entries", len(leads))
		}

		// Filter by bus_team.
		bus, err := s.ListRosterEntries(ctx, store.RosterFilter{BusTeam: "bus-x"})
		if err != nil {
			t.Fatal(err)
		}
		if len(bus) != 1 || bus[0].RosterID != "r-l2" {
			t.Fatalf("bus_team filter: got %d entries", len(bus))
		}

		// Filter by runtime.
		codex, err := s.ListRosterEntries(ctx, store.RosterFilter{Runtime: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		if len(codex) != 1 {
			t.Fatalf("runtime filter: got %d entries", len(codex))
		}
	})
}

func TestTokenCreateAndVerify(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		sid := "sess-tok"
		entry := store.RosterEntry{
			RosterID:     "r-tok",
			SessionID:    &sid,
			AgentName:    "@agent",
			Host:         "host-a",
			Runtime:      "claude_code",
			Relationship: "teammate",
			CreatedAt:    now,
			BoundAt:      &now,
		}
		if err := s.CreateRosterEntry(ctx, entry); err != nil {
			t.Fatal(err)
		}

		tok := store.AgentToken{
			TokenHash: "abc123def456",
			RosterID:  "r-tok",
			IssuedAt:  now,
		}
		if err := s.CreateToken(ctx, tok); err != nil {
			t.Fatal(err)
		}

		gotTok, gotEntry, err := s.VerifyToken(ctx, "abc123def456")
		if err != nil {
			t.Fatal(err)
		}
		if gotTok.RosterID != "r-tok" {
			t.Fatalf("token roster_id = %q", gotTok.RosterID)
		}
		if gotEntry.RosterID != "r-tok" || gotEntry.AgentName != "@agent" {
			t.Fatalf("entry mismatch: %+v", gotEntry)
		}
		if gotTok.RevokedAt != nil {
			t.Fatal("token should not be revoked")
		}
	})
}

func TestTokenVerifyNotFound(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		_, _, err := s.VerifyToken(ctx, "nonexistent-hash")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
}

func TestTokenRevoke(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		sid := "sess-revoke"
		entry := store.RosterEntry{
			RosterID: "r-rev", SessionID: &sid, AgentName: "@x",
			Host: "h", Runtime: "claude_code", Relationship: "teammate",
			CreatedAt: now, BoundAt: &now,
		}
		if err := s.CreateRosterEntry(ctx, entry); err != nil {
			t.Fatal(err)
		}
		tok := store.AgentToken{TokenHash: "revhash", RosterID: "r-rev", IssuedAt: now}
		if err := s.CreateToken(ctx, tok); err != nil {
			t.Fatal(err)
		}

		if err := s.RevokeToken(ctx, "r-rev"); err != nil {
			t.Fatal(err)
		}

		gotTok, _, err := s.VerifyToken(ctx, "revhash")
		if err != nil {
			t.Fatal(err)
		}
		if gotTok.RevokedAt == nil {
			t.Fatal("token should be revoked")
		}

		// Idempotent.
		if err := s.RevokeToken(ctx, "r-rev"); err != nil {
			t.Fatalf("idempotent revoke: %v", err)
		}
	})
}

func TestTokenCascadeRevoke(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		sid1, sid2, sid3 := "s-lead", "s-peer", "s-nested"
		parentRef := "r-lead"
		grandparentRef := "r-peer"

		// lead -> peer -> nested
		lead := store.RosterEntry{
			RosterID: "r-lead", SessionID: &sid1, AgentName: "",
			Host: "h", Runtime: "claude_code", Relationship: "lead",
			CreatedAt: now, BoundAt: &now,
		}
		peer := store.RosterEntry{
			RosterID: "r-peer", SessionID: &sid2, AgentName: "@peer",
			Host: "h", Runtime: "codex", Relationship: "peer",
			ParentRef: &parentRef, CreatedAt: now, BoundAt: &now,
		}
		nested := store.RosterEntry{
			RosterID: "r-nested", SessionID: &sid3, AgentName: "@nested",
			Host: "h", Runtime: "codex", Relationship: "peer",
			ParentRef: &grandparentRef, CreatedAt: now, BoundAt: &now,
		}

		for _, e := range []store.RosterEntry{lead, peer, nested} {
			if err := s.CreateRosterEntry(ctx, e); err != nil {
				t.Fatal(err)
			}
		}

		for _, tok := range []store.AgentToken{
			{TokenHash: "lead-tok", RosterID: "r-lead", IssuedAt: now},
			{TokenHash: "peer-tok", RosterID: "r-peer", IssuedAt: now},
			{TokenHash: "nested-tok", RosterID: "r-nested", IssuedAt: now},
		} {
			if err := s.CreateToken(ctx, tok); err != nil {
				t.Fatal(err)
			}
		}

		n, err := s.RevokeTokenCascade(ctx, "r-lead")
		if err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Fatalf("cascade revoked %d tokens, want 3", n)
		}

		for _, hash := range []string{"lead-tok", "peer-tok", "nested-tok"} {
			tok, _, err := s.VerifyToken(ctx, hash)
			if err != nil {
				t.Fatal(err)
			}
			if tok.RevokedAt == nil {
				t.Fatalf("token %q should be revoked after cascade", hash)
			}
		}
	})
}

func TestTokenTouchLastUsed(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		sid := "sess-touch"
		entry := store.RosterEntry{
			RosterID: "r-touch", SessionID: &sid, AgentName: "@t",
			Host: "h", Runtime: "claude_code", Relationship: "teammate",
			CreatedAt: now, BoundAt: &now,
		}
		if err := s.CreateRosterEntry(ctx, entry); err != nil {
			t.Fatal(err)
		}
		tok := store.AgentToken{TokenHash: "touch-hash", RosterID: "r-touch", IssuedAt: now}
		if err := s.CreateToken(ctx, tok); err != nil {
			t.Fatal(err)
		}

		if err := s.TouchTokenLastUsed(ctx, "touch-hash"); err != nil {
			t.Fatal(err)
		}

		gotTok, _, err := s.VerifyToken(ctx, "touch-hash")
		if err != nil {
			t.Fatal(err)
		}
		if gotTok.LastUsedAt == nil {
			t.Fatal("last_used_at should be set after touch")
		}
	})
}

func TestRosterCreateConflict(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)

		entry := store.RosterEntry{
			RosterID: "r-dup", AgentName: "@dup",
			Host: "h", Runtime: "claude_code", Relationship: "teammate",
			CreatedAt: now,
		}
		if err := s.CreateRosterEntry(ctx, entry); err != nil {
			t.Fatal(err)
		}

		// Duplicate roster_id.
		err := s.CreateRosterEntry(ctx, entry)
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("expected ErrConflict on dup roster_id, got %v", err)
		}
	})
}

func TestRosterUnboundEntry(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)

		// Unbound entry (nil session_id).
		entry := store.RosterEntry{
			RosterID:     "r-unbound",
			AgentName:    "@spawned",
			Host:         "host-a",
			Runtime:      "codex",
			Relationship: "peer",
			CreatedAt:    now,
		}
		if err := s.CreateRosterEntry(ctx, entry); err != nil {
			t.Fatal(err)
		}

		got, err := s.GetRosterEntry(ctx, "r-unbound")
		if err != nil {
			t.Fatal(err)
		}
		if got.SessionID != nil {
			t.Fatalf("expected nil session_id, got %v", got.SessionID)
		}
		if got.BoundAt != nil {
			t.Fatalf("expected nil bound_at, got %v", got.BoundAt)
		}

		// Token on unbound entry.
		tok := store.AgentToken{TokenHash: "unbound-tok", RosterID: "r-unbound", IssuedAt: now}
		if err := s.CreateToken(ctx, tok); err != nil {
			t.Fatal(err)
		}

		gotTok, gotEntry, err := s.VerifyToken(ctx, "unbound-tok")
		if err != nil {
			t.Fatal(err)
		}
		if gotTok.RosterID != "r-unbound" {
			t.Fatalf("wrong roster_id: %q", gotTok.RosterID)
		}
		if gotEntry.SessionID != nil {
			t.Fatalf("unbound entry should have nil session_id in verify: %v", gotEntry.SessionID)
		}

		// Bind it.
		if err := s.BindRosterSession(ctx, "r-unbound", "sess-new"); err != nil {
			t.Fatal(err)
		}

		// Verify again — now bound.
		_, gotEntry, err = s.VerifyToken(ctx, "unbound-tok")
		if err != nil {
			t.Fatal(err)
		}
		if gotEntry.SessionID == nil || *gotEntry.SessionID != "sess-new" {
			t.Fatalf("after bind, session_id should be sess-new: %v", gotEntry.SessionID)
		}
	})
}
