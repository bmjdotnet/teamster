// Package reconciler cross-checks per-session hub spend (token_ledger) against
// the Claude Code client's own OTel cost counters. It is read-only by
// construction: no reader or sink interface here can write token_ledger, and
// it deliberately does not import internal/pricing, so a pricing-code defect
// cannot hide itself from the check.
package reconciler

import (
	"context"
	"strings"
	"time"
)

const RuntimeClaudeCode = "claude_code"

// Verdict is the reconciliation outcome for one session.
type Verdict string

const (
	// VerdictWithinTolerance: converged and |hub - client| inside the band.
	VerdictWithinTolerance Verdict = "within_tolerance"
	// VerdictUnconverged: hub and client are not yet comparable (live session
	// or hub still catching up). Informational, never an alert.
	VerdictUnconverged Verdict = "unconverged"
	// VerdictDiverged: converged and outside the band, or a capture gap that
	// outlived CaptureGapGrace.
	VerdictDiverged Verdict = "diverged"
	// VerdictPremiumPricing: a [1m]-labeled model's client cost exceeds its
	// base-rate recomputation beyond tolerance. Distinct from diverged.
	VerdictPremiumPricing Verdict = "premium_pricing"
	// VerdictUnverifiable: no client-side series exist for the session.
	VerdictUnverifiable Verdict = "unverifiable"
)

// IsAlert reports whether the verdict should page.
func (v Verdict) IsAlert() bool {
	return v == VerdictDiverged || v == VerdictPremiumPricing
}

// Machine-readable reasons carried in Details.Reason.
const (
	ReasonVendorActive    = "vendor_active"
	ReasonVendorUnknown   = "vendor_activity_unknown"
	ReasonLedgerActive    = "ledger_active"
	ReasonTokenParity     = "token_parity"
	ReasonCaptureGap      = "capture_gap"
	ReasonNoVendorSeries  = "no_vendor_series"
	ReasonCostOutsideBand = "cost_outside_band"
)

// TokenCounts is a bundle of token sums. CacheWrite is the total of all
// cache-creation tokens (OTel "cacheCreation"); CacheWrite5m/CacheWrite1h are
// the ledger's TTL split and stay zero on the client side, which has none.
type TokenCounts struct {
	Input        int64 `json:"input"`
	Output       int64 `json:"output"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite   int64 `json:"cache_write"`
	CacheWrite5m int64 `json:"cache_write_5m,omitempty"`
	CacheWrite1h int64 `json:"cache_write_1h,omitempty"`
}

func (t *TokenCounts) add(o TokenCounts) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.CacheWrite5m += o.CacheWrite5m
	t.CacheWrite1h += o.CacheWrite1h
}

// LedgerModel is the hub's aggregate for one model within a session.
type LedgerModel struct {
	Tokens  TokenCounts
	CostUSD float64
}

// LedgerSession is the hub-side view of one session: SUM over token_ledger
// rows. LastRowAt is MAX(token_ledger.timestamp), zero when there are no rows.
type LedgerSession struct {
	SessionID string
	Rows      int64
	LastRowAt time.Time
	Models    map[string]LedgerModel
}

// VendorModel is the client's aggregate for one model label within a session,
// summed across agent_name and every other label.
type VendorModel struct {
	Tokens  TokenCounts
	CostUSD float64
}

// VendorSession is the client-side (OTel) view of one session. Models is keyed
// by the raw OTel label, so "claude-opus-4-8[1m]" stays distinct from
// "claude-opus-4-8". TokensStableSince is the earliest time the session's
// token counters are known to have been unchanged; zero means unknown.
type VendorSession struct {
	SessionID         string
	Models            map[string]VendorModel
	TokensStableSince time.Time
}

// PromReader reads the client-side view from Prometheus. Sessions with no
// series are absent from the result. stabilityHorizon is how far back the
// reader must look to establish counter stability; TokensStableSince saturates
// there, so a long-quiet session reports "at least the horizon", not its true age.
type PromReader interface {
	ReadSessions(ctx context.Context, sessionIDs []string, at time.Time, stabilityHorizon time.Duration) (map[string]VendorSession, error)
}

// LedgerReader reads the hub-side view from token_ledger. Sessions with no
// rows are absent from the result. Implementations must not write.
type LedgerReader interface {
	ReadSessions(ctx context.Context, sessionIDs []string) (map[string]LedgerSession, error)
	// SessionsSince lists sessions with ledger rows timestamped at or after since.
	SessionsSince(ctx context.Context, since time.Time) ([]string, error)
}

// BaseRater prices tokens at a model's published base (non-premium) rates.
// It exists for the [1m] tripwire and is injected so this package never
// imports the code path it is checking. ok is false for an unknown model.
// Ledger rows that predate the TTL split can have CacheWrite5m+CacheWrite1h
// below CacheWrite; implementations should price that remainder at the 5m
// rate or the tripwire will read a missing cache-write charge as a premium.
type BaseRater interface {
	BaseRateCost(model string, tokens TokenCounts) (usd float64, ok bool)
}

// Config tunes gating and tolerance. Zero fields take the defaults below.
type Config struct {
	// ConvergenceWindow: client token counters unchanged, and the newest
	// ledger row at least this old, before a session counts as converged.
	ConvergenceWindow time.Duration
	// ToleranceUSD and ToleranceFrac: alert when |hub - client| exceeds
	// max(ToleranceUSD, ToleranceFrac * client).
	ToleranceUSD  float64
	ToleranceFrac float64
	// ParityRelTol and ParityFloor: per token type, hub and client counts
	// agree when they differ by at most max(floor, ParityRelTol * larger).
	// The floors absorb the background haiku calls that reach OTel but never
	// the JSONL.
	ParityRelTol float64
	ParityFloor  ParityFloors
	// CaptureGapGrace: once counters and ledger have both been quiet this
	// long, a session that still fails token parity is a permanent gap rather
	// than a lagging scraper. Parity then stops gating and the cost band
	// decides, so only a gap that also moves dollars alerts.
	CaptureGapGrace time.Duration
}

// ParityFloors are per-type absolute token tolerances. Each default is sized
// so the floor stays near the $1.00 tolerance at the most expensive model's
// rate, and above the haiku-only residue measured on the hub (about 135k
// cache-read and 21k input tokens per session).
type ParityFloors struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

const (
	defaultConvergenceWindow = 15 * time.Minute
	defaultToleranceUSD      = 1.00
	defaultToleranceFrac     = 0.005
	defaultParityRelTol      = 0.01
	defaultCaptureGapGrace   = 6 * time.Hour
)

var defaultParityFloor = ParityFloors{Input: 100_000, Output: 20_000, CacheRead: 500_000, CacheWrite: 100_000}

func (c Config) withDefaults() Config {
	if c.ConvergenceWindow <= 0 {
		c.ConvergenceWindow = defaultConvergenceWindow
	}
	if c.ToleranceUSD <= 0 {
		c.ToleranceUSD = defaultToleranceUSD
	}
	if c.ToleranceFrac <= 0 {
		c.ToleranceFrac = defaultToleranceFrac
	}
	if c.ParityRelTol <= 0 {
		c.ParityRelTol = defaultParityRelTol
	}
	if c.ParityFloor.Input <= 0 {
		c.ParityFloor.Input = defaultParityFloor.Input
	}
	if c.ParityFloor.Output <= 0 {
		c.ParityFloor.Output = defaultParityFloor.Output
	}
	if c.ParityFloor.CacheRead <= 0 {
		c.ParityFloor.CacheRead = defaultParityFloor.CacheRead
	}
	if c.ParityFloor.CacheWrite <= 0 {
		c.ParityFloor.CacheWrite = defaultParityFloor.CacheWrite
	}
	if c.CaptureGapGrace <= 0 {
		c.CaptureGapGrace = defaultCaptureGapGrace
	}
	return c
}

// SessionVerification is one row of reconciliation output, shaped for the
// cost_verification table. DeltaUSD is hub - client. VendorUSD and DeltaUSD
// are zero for VerdictUnverifiable. ConvergedAt is the evaluation time when
// the session was observed converged, nil otherwise. store.VerificationStore
// merges it on upsert: a converged row keeps the stored time, a nil one clears
// it, so it means "first observed converged in the current converged stretch".
type SessionVerification struct {
	SessionID   string     `json:"session_id"`
	Runtime     string     `json:"runtime"`
	HubUSD      float64    `json:"hub_usd"`
	VendorUSD   float64    `json:"vendor_usd"`
	DeltaUSD    float64    `json:"delta_usd"`
	Verdict     Verdict    `json:"verdict"`
	ConvergedAt *time.Time `json:"converged_at,omitempty"`
	EvaluatedAt time.Time  `json:"evaluated_at"`
	Details     Details    `json:"details"`
}

// Details is the JSON payload stored beside a verdict.
type Details struct {
	Reason       string        `json:"reason,omitempty"`
	ToleranceUSD float64       `json:"tolerance_usd"`
	Gates        Gates         `json:"gates"`
	Parity       []TokenParity `json:"token_parity,omitempty"`
	Models       []ModelRow    `json:"models,omitempty"`
	Premium      []PremiumRow  `json:"premium,omitempty"`
}

// Gates records each convergence gate and how long it has held.
type Gates struct {
	VendorStable       bool  `json:"vendor_stable"`
	VendorStableForSec int64 `json:"vendor_stable_for_sec"`
	LedgerQuiet        bool  `json:"ledger_quiet"`
	LedgerQuietForSec  int64 `json:"ledger_quiet_for_sec"`
	TokenParity        bool  `json:"token_parity"`
}

// TokenParity compares one token type across hub and client. Delta is
// hub - client.
type TokenParity struct {
	Type         string `json:"type"`
	HubTokens    int64  `json:"hub_tokens"`
	VendorTokens int64  `json:"vendor_tokens"`
	Delta        int64  `json:"delta"`
	Tolerance    int64  `json:"tolerance"`
	OK           bool   `json:"ok"`
}

// ModelRow is the per-model breakdown, keyed by normalized model id. A model
// with ParityOK true but a large DeltaUSD is a pricing gap; ParityOK false is
// a capture gap.
type ModelRow struct {
	Model        string   `json:"model"`
	HubUSD       float64  `json:"hub_usd"`
	VendorUSD    float64  `json:"vendor_usd"`
	DeltaUSD     float64  `json:"delta_usd"`
	VendorLabels []string `json:"vendor_labels"`
	ParityOK     bool     `json:"parity_ok"`
}

// PremiumRow is the [1m] tripwire result for one base model. Evaluated is
// false when no base rate was available to compare against.
type PremiumRow struct {
	Model        string  `json:"model"`
	VendorUSD    float64 `json:"vendor_usd"`
	BaseRateUSD  float64 `json:"base_rate_usd"`
	ExcessUSD    float64 `json:"excess_usd"`
	ToleranceUSD float64 `json:"tolerance_usd"`
	Evaluated    bool    `json:"evaluated"`
	Fired        bool    `json:"fired"`
}

// NormalizeModel strips a trailing bracketed variant such as "[1m]" from a
// model label. premium is true for the 1M-context variant.
func NormalizeModel(label string) (base string, premium bool) {
	label = strings.TrimSpace(label)
	i := strings.LastIndexByte(label, '[')
	if i <= 0 || !strings.HasSuffix(label, "]") {
		return label, false
	}
	variant := strings.ToLower(label[i+1 : len(label)-1])
	return label[:i], variant == "1m"
}
