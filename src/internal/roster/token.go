package roster

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

// RegisterOpts are the parameters for RegisterPeer.
type RegisterOpts struct {
	SessionID    string // empty = unbound (spawn-time)
	AgentName    string
	Host         string
	Runtime      string
	Model        string
	Relationship string
	TeamName     string
	BusTeam      string
	ParentRef    *string
}

// MintToken generates a high-entropy random bearer token, stores its SHA-256
// hash in agent_tokens keyed to rosterID, and returns the raw token value
// exactly once. The raw value is NEVER persisted in any Teamster-controlled
// store.
func MintToken(ctx context.Context, s store.RosterStore, rosterID string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	rawHex := hex.EncodeToString(raw)

	hash := sha256.Sum256([]byte(rawHex))
	hashHex := hex.EncodeToString(hash[:])

	token := store.AgentToken{
		TokenHash: hashHex,
		RosterID:  rosterID,
		IssuedAt:  time.Now().UTC(),
	}
	if err := s.CreateToken(ctx, token); err != nil {
		return "", fmt.Errorf("storing token: %w", err)
	}

	return rawHex, nil
}

// HashToken returns the SHA-256 hex digest of a raw bearer token,
// matching the hash stored in agent_tokens.
func HashToken(raw string) string {
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

// VerifyToken checks a raw bearer token against the store. Returns the
// associated token record and roster entry. Updates last_used_at on success.
func VerifyToken(ctx context.Context, s store.RosterStore, rawToken string) (store.AgentToken, store.RosterEntry, error) {
	hashHex := HashToken(rawToken)

	tok, entry, err := s.VerifyToken(ctx, hashHex)
	if err != nil {
		return tok, entry, err
	}

	if tok.RevokedAt != nil {
		return tok, entry, fmt.Errorf("token revoked at %s", tok.RevokedAt.Format(time.RFC3339))
	}

	if tok.ExpiresAt != nil && tok.ExpiresAt.Before(time.Now().UTC()) {
		return tok, entry, fmt.Errorf("token expired at %s", tok.ExpiresAt.Format(time.RFC3339))
	}

	_ = s.TouchTokenLastUsed(ctx, hashHex)

	return tok, entry, nil
}

// RegisterPeer creates an agent_roster entry and mints a token for it.
// If opts.SessionID is empty, the entry is created unbound (spawn-time).
// Returns the roster_id and raw bearer token.
func RegisterPeer(ctx context.Context, s store.RosterStore, opts RegisterOpts) (string, string, error) {
	rosterID := GenerateRosterID()

	entry := store.RosterEntry{
		RosterID:     rosterID,
		AgentName:    opts.AgentName,
		Host:         opts.Host,
		Runtime:      opts.Runtime,
		Model:        opts.Model,
		Relationship: opts.Relationship,
		TeamName:     opts.TeamName,
		BusTeam:      opts.BusTeam,
		ParentRef:    opts.ParentRef,
		CreatedAt:    time.Now().UTC(),
	}

	if opts.SessionID != "" {
		entry.SessionID = &opts.SessionID
		now := time.Now().UTC()
		entry.BoundAt = &now
	}

	if err := s.CreateRosterEntry(ctx, entry); err != nil {
		return "", "", fmt.Errorf("creating roster entry: %w", err)
	}

	rawToken, err := MintToken(ctx, s, rosterID)
	if err != nil {
		return "", "", fmt.Errorf("minting token: %w", err)
	}

	return rosterID, rawToken, nil
}
