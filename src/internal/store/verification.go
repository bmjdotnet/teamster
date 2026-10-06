package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Column widths of cost_verification, enforced here so every backend rejects
// the same rows (SQLite has no length limit; MySQL would otherwise fail
// differently under strict mode).
const (
	verificationMaxSessionID = 64
	verificationMaxRuntime   = 32
	verificationMaxVerdict   = 32
)

// Verification is one cost_verification row: the OTel reconciler's latest
// verdict for a session. The store keeps verdicts as opaque strings and details
// as raw JSON, so it imports nothing from the reconciler.
type Verification struct {
	SessionID string
	// Runtime defaults to claude_code when empty (NormalizeRateRuntime).
	Runtime   string
	HubUSD    float64
	VendorUSD float64
	// DeltaUSD is HubUSD - VendorUSD, signed.
	DeltaUSD float64
	Verdict  string
	// ConvergedAt is when the session was first observed converged; nil when
	// it is not converged now.
	ConvergedAt *time.Time
	EvaluatedAt time.Time
	// Details is a JSON document, or empty for none.
	Details []byte
}

// VerificationStore persists the OTel reconciler's verdicts, one row per
// (session_id, runtime). It never touches token_ledger.
type VerificationStore interface {
	// UpsertVerifications writes the rows in one transaction, replacing the
	// existing row for each (session_id, runtime): the latest evaluation wins.
	// converged_at is the one column with memory. A row that arrives converged
	// keeps the converged_at already stored, so it stays "first observed
	// converged" across re-evaluations; a row that arrives not converged clears
	// it, so a session that falls out of convergence does not keep a stale
	// time. The whole batch is validated (NormalizeVerification) before any
	// write; a bad row rejects it all.
	UpsertVerifications(ctx context.Context, rows []Verification) error

	// ListVerifications returns the rows of the given runtime evaluated at or
	// after evaluatedSince, ordered by session_id. An empty runtime means
	// claude_code, as everywhere else (NormalizeRateRuntime).
	ListVerifications(ctx context.Context, runtime string, evaluatedSince time.Time) ([]Verification, error)
}

// NormalizeVerification applies defaults (runtime, UTC times, empty details to
// none) and validates a verification row before a backend writes it, so every
// backend enforces one definition of a valid row.
func NormalizeVerification(v Verification) (Verification, error) {
	v.Runtime = NormalizeRateRuntime(v.Runtime)
	switch v.Runtime {
	case RateRuntimeClaudeCode, RateRuntimeCodex:
	default:
		return Verification{}, fmt.Errorf("verification: unknown runtime %q", v.Runtime)
	}
	if v.SessionID == "" || len(v.SessionID) > verificationMaxSessionID {
		return Verification{}, fmt.Errorf("verification: session_id %q must be 1-%d bytes", v.SessionID, verificationMaxSessionID)
	}
	if v.Verdict == "" || len(v.Verdict) > verificationMaxVerdict {
		return Verification{}, fmt.Errorf("verification: verdict %q must be 1-%d bytes", v.Verdict, verificationMaxVerdict)
	}
	if v.EvaluatedAt.IsZero() {
		return Verification{}, fmt.Errorf("verification: evaluated_at is required")
	}
	v.EvaluatedAt = v.EvaluatedAt.UTC()
	if v.ConvergedAt != nil {
		if v.ConvergedAt.IsZero() {
			return Verification{}, fmt.Errorf("verification: converged_at must be nil, not the zero time")
		}
		t := v.ConvergedAt.UTC()
		v.ConvergedAt = &t
	}
	if len(v.Details) == 0 {
		v.Details = nil
	} else if !json.Valid(v.Details) {
		return Verification{}, fmt.Errorf("verification: details is not valid JSON")
	}
	return v, nil
}

// NormalizeVerifications normalizes every row, returning the first error with
// its index so a caller can find the offending row.
func NormalizeVerifications(rows []Verification) ([]Verification, error) {
	out := make([]Verification, len(rows))
	for i, r := range rows {
		n, err := NormalizeVerification(r)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
		out[i] = n
	}
	return out, nil
}
