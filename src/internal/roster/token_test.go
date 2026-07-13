package roster_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bmjdotnet/teamster/internal/roster"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/sqlite"
)

func openTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestS3TokenSpike exercises the full token lifecycle specified in P0 §4.3.
func TestS3TokenSpike(t *testing.T) {
	t.Run("(a) mint returns raw token once, never retrievable again", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()

		rosterID, rawToken, err := roster.RegisterPeer(ctx, s, roster.RegisterOpts{
			SessionID:    "sess-a",
			AgentName:    "@scout",
			Host:         "host-a",
			Runtime:      "claude_code",
			Model:        "opus",
			Relationship: "teammate",
		})
		if err != nil {
			t.Fatalf("RegisterPeer: %v", err)
		}
		if rosterID == "" || rawToken == "" {
			t.Fatal("expected non-empty roster_id and raw token")
		}

		// The raw token is 64 hex chars (32 bytes).
		if len(rawToken) != 64 {
			t.Fatalf("raw token length = %d, want 64", len(rawToken))
		}

		// The token hash in the store differs from the raw value.
		hashHex := roster.HashToken(rawToken)
		if hashHex == rawToken {
			t.Fatal("hash should differ from raw token")
		}

		// Verify the stored hash matches what we compute.
		tok, _, err := s.VerifyToken(ctx, hashHex)
		if err != nil {
			t.Fatalf("store.VerifyToken: %v", err)
		}
		if tok.TokenHash != hashHex {
			t.Fatalf("stored hash = %s, want %s", tok.TokenHash, hashHex)
		}
		if tok.RosterID != rosterID {
			t.Fatalf("token roster_id = %s, want %s", tok.RosterID, rosterID)
		}
	})

	t.Run("(b) verify succeeds with correct token", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()

		_, rawToken, err := roster.RegisterPeer(ctx, s, roster.RegisterOpts{
			SessionID:    "sess-b",
			AgentName:    "@impl",
			Host:         "host-a",
			Runtime:      "claude_code",
			Model:        "sonnet",
			Relationship: "teammate",
		})
		if err != nil {
			t.Fatalf("RegisterPeer: %v", err)
		}

		tok, entry, err := roster.VerifyToken(ctx, s, rawToken)
		if err != nil {
			t.Fatalf("VerifyToken: %v", err)
		}
		if tok.RevokedAt != nil {
			t.Fatal("token should not be revoked")
		}
		if entry.AgentName != "@impl" {
			t.Fatalf("agent_name = %s, want @impl", entry.AgentName)
		}
		if entry.SessionID == nil || *entry.SessionID != "sess-b" {
			t.Fatalf("session_id = %v, want sess-b", entry.SessionID)
		}
	})

	t.Run("(c) verify rejects garbage/missing tokens", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()

		_, _, err := roster.VerifyToken(ctx, s, "0000000000000000000000000000000000000000000000000000000000000000")
		if err == nil {
			t.Fatal("expected error for garbage token")
		}
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got: %v", err)
		}

		_, _, err = roster.VerifyToken(ctx, s, "")
		if err == nil {
			t.Fatal("expected error for empty token")
		}
	})

	t.Run("(d) revoke makes subsequent verify fail", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()

		rosterID, rawToken, err := roster.RegisterPeer(ctx, s, roster.RegisterOpts{
			SessionID:    "sess-d",
			AgentName:    "@worker",
			Host:         "host-a",
			Runtime:      "claude_code",
			Model:        "sonnet",
			Relationship: "teammate",
		})
		if err != nil {
			t.Fatalf("RegisterPeer: %v", err)
		}

		// Verify works before revocation.
		_, _, err = roster.VerifyToken(ctx, s, rawToken)
		if err != nil {
			t.Fatalf("pre-revoke VerifyToken: %v", err)
		}

		// Revoke.
		if err := s.RevokeToken(ctx, rosterID); err != nil {
			t.Fatalf("RevokeToken: %v", err)
		}

		// Verify fails after revocation.
		_, _, err = roster.VerifyToken(ctx, s, rawToken)
		if err == nil {
			t.Fatal("expected error after revocation")
		}
		if got := err.Error(); !contains(got, "revoked") {
			t.Fatalf("error should mention revoked, got: %s", got)
		}
	})

	t.Run("(e) unbound->bound flow (BLOCKER 2)", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()

		// Step 1: Register with NO session_id (spawn-time, unbound).
		rosterID, rawToken, err := roster.RegisterPeer(ctx, s, roster.RegisterOpts{
			AgentName:    "@peer-codex",
			Host:         "host-b",
			Runtime:      "codex",
			Model:        "o3-pro",
			Relationship: "peer",
		})
		if err != nil {
			t.Fatalf("RegisterPeer (unbound): %v", err)
		}

		// Step 2: Verify succeeds, but session_id is nil.
		tok, entry, err := roster.VerifyToken(ctx, s, rawToken)
		if err != nil {
			t.Fatalf("VerifyToken (unbound): %v", err)
		}
		if tok.RevokedAt != nil {
			t.Fatal("unbound token should not be revoked")
		}
		if entry.SessionID != nil {
			t.Fatalf("unbound entry should have nil session_id, got %v", *entry.SessionID)
		}
		if entry.BoundAt != nil {
			t.Fatal("unbound entry should have nil bound_at")
		}

		// Step 3: Bind session.
		realSessionID := "codex-session-xyz"
		if err := s.BindRosterSession(ctx, rosterID, realSessionID); err != nil {
			t.Fatalf("BindRosterSession: %v", err)
		}

		// Step 4: Verify now returns populated session_id.
		_, entry, err = roster.VerifyToken(ctx, s, rawToken)
		if err != nil {
			t.Fatalf("VerifyToken (post-bind): %v", err)
		}
		if entry.SessionID == nil || *entry.SessionID != realSessionID {
			t.Fatalf("post-bind session_id = %v, want %s", entry.SessionID, realSessionID)
		}
		if entry.BoundAt == nil {
			t.Fatal("post-bind bound_at should be set")
		}

		// Step 5: Rebind with DIFFERENT session_id is rejected.
		err = s.BindRosterSession(ctx, rosterID, "different-session")
		if err == nil {
			t.Fatal("expected error on rebind with different session_id")
		}
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("expected ErrConflict on rebind, got: %v", err)
		}
	})
}

func TestCascadeRevoke(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Lead registers.
	leadID, leadToken, err := roster.RegisterPeer(ctx, s, roster.RegisterOpts{
		SessionID:    "sess-lead",
		AgentName:    "",
		Host:         "host-a",
		Runtime:      "claude_code",
		Model:        "opus",
		Relationship: "lead",
	})
	if err != nil {
		t.Fatalf("register lead: %v", err)
	}

	// Peer registers with parent_ref = lead.
	peerID, peerToken, err := roster.RegisterPeer(ctx, s, roster.RegisterOpts{
		SessionID:    "sess-peer",
		AgentName:    "@worker",
		Host:         "host-b",
		Runtime:      "codex",
		Model:        "o3-pro",
		Relationship: "peer",
		ParentRef:    &leadID,
	})
	if err != nil {
		t.Fatalf("register peer: %v", err)
	}

	// Nested peer registers with parent_ref = peer.
	_, nestedToken, err := roster.RegisterPeer(ctx, s, roster.RegisterOpts{
		SessionID:    "sess-nested",
		AgentName:    "@sub-worker",
		Host:         "host-b",
		Runtime:      "codex",
		Model:        "o3-pro",
		Relationship: "peer",
		ParentRef:    &peerID,
	})
	if err != nil {
		t.Fatalf("register nested: %v", err)
	}

	// All three tokens verify before cascade.
	for name, tok := range map[string]string{"lead": leadToken, "peer": peerToken, "nested": nestedToken} {
		if _, _, err := roster.VerifyToken(ctx, s, tok); err != nil {
			t.Fatalf("pre-cascade %s verify: %v", name, err)
		}
	}

	// Cascade revoke from lead.
	n, err := s.RevokeTokenCascade(ctx, leadID)
	if err != nil {
		t.Fatalf("RevokeTokenCascade: %v", err)
	}
	if n != 3 {
		t.Fatalf("cascade revoked %d tokens, want 3", n)
	}

	// All three tokens fail verification.
	for name, tok := range map[string]string{"lead": leadToken, "peer": peerToken, "nested": nestedToken} {
		_, _, err := roster.VerifyToken(ctx, s, tok)
		if err == nil {
			t.Fatalf("post-cascade %s verify should fail", name)
		}
		if got := err.Error(); !contains(got, "revoked") {
			t.Fatalf("post-cascade %s error should mention revoked, got: %s", name, got)
		}
	}
}

func TestIdempotentBind(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rosterID, _, err := roster.RegisterPeer(ctx, s, roster.RegisterOpts{
		AgentName:    "@idempotent",
		Host:         "host-a",
		Runtime:      "codex",
		Model:        "o3-pro",
		Relationship: "peer",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	sessionID := "sess-idempotent"

	// First bind.
	if err := s.BindRosterSession(ctx, rosterID, sessionID); err != nil {
		t.Fatalf("first bind: %v", err)
	}

	// Second bind with same session_id succeeds (idempotent).
	if err := s.BindRosterSession(ctx, rosterID, sessionID); err != nil {
		t.Fatalf("idempotent rebind: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
