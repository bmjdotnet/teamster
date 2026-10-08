// Package storeadapter connects the reconciler to the store. It lives apart
// from the reconciler package on purpose: the reconciler must never import
// internal/store (a test enforces it), so this is the one place that sees both.
package storeadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bmjdotnet/teamster/internal/reconciler"
	"github.com/bmjdotnet/teamster/internal/store"
)

var _ reconciler.LedgerReader = (*Ledger)(nil)

// Ledger is a reconciler.LedgerReader over store.ReconciliationStore. It reads
// Claude Code sessions only, since the reconciler hard-codes that runtime on
// every verdict it emits.
type Ledger struct {
	s store.ReconciliationStore
}

// NewLedger wraps s.
func NewLedger(s store.ReconciliationStore) *Ledger {
	return &Ledger{s: s}
}

// ReadSessions implements reconciler.LedgerReader.
func (l *Ledger) ReadSessions(ctx context.Context, sessionIDs []string) (map[string]reconciler.LedgerSession, error) {
	got, err := l.s.ReconcileSessions(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]reconciler.LedgerSession, len(got))
	for id, s := range got {
		models := make(map[string]reconciler.LedgerModel, len(s.Models))
		for model, m := range s.Models {
			models[model] = reconciler.LedgerModel{
				Tokens: reconciler.TokenCounts{
					Input:        m.Tokens.Input,
					Output:       m.Tokens.Output,
					CacheRead:    m.Tokens.CacheRead,
					CacheWrite:   m.Tokens.CacheWrite,
					CacheWrite5m: m.Tokens.CacheWrite5m,
					CacheWrite1h: m.Tokens.CacheWrite1h,
				},
				CostUSD: m.CostUSD,
			}
		}
		out[id] = reconciler.LedgerSession{SessionID: s.SessionID, Rows: s.Rows, LastRowAt: s.LastRowAt, Models: models}
	}
	return out, nil
}

// SessionsSince implements reconciler.LedgerReader.
func (l *Ledger) SessionsSince(ctx context.Context, since time.Time) ([]string, error) {
	return l.s.ReconcileSessionsSince(ctx, reconciler.RuntimeClaudeCode, since)
}

// Verifications persists reconciler results through store.VerificationStore.
type Verifications struct {
	s store.VerificationStore
}

// NewVerifications wraps s.
func NewVerifications(s store.VerificationStore) *Verifications {
	return &Verifications{s: s}
}

// Save upserts one cost_verification row per result, in one transaction. The
// reconciler's Details are stored as their JSON encoding.
func (v *Verifications) Save(ctx context.Context, results []reconciler.SessionVerification) error {
	rows := make([]store.Verification, len(results))
	for i, r := range results {
		details, err := json.Marshal(r.Details)
		if err != nil {
			return fmt.Errorf("storeadapter: encode details for session %s: %w", r.SessionID, err)
		}
		rows[i] = store.Verification{
			SessionID:   r.SessionID,
			Runtime:     r.Runtime,
			HubUSD:      r.HubUSD,
			VendorUSD:   r.VendorUSD,
			DeltaUSD:    r.DeltaUSD,
			Verdict:     string(r.Verdict),
			ConvergedAt: r.ConvergedAt,
			EvaluatedAt: r.EvaluatedAt,
			Details:     details,
		}
	}
	return v.s.UpsertVerifications(ctx, rows)
}
