package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/bmjdotnet/teamster/internal/pricing"
	"github.com/bmjdotnet/teamster/internal/reconciler"
	"github.com/bmjdotnet/teamster/internal/reconciler/storeadapter"
	"github.com/bmjdotnet/teamster/internal/store"
)

// verificationLookback bounds the sessions re-evaluated each pass. It must
// exceed the reconciler's CaptureGapGrace (6h) so a session with a token gap
// is still in range when its grace period ends and the cost band decides.
const verificationLookback = 24 * time.Hour

// verificationStore is the slice of store.Store the verification pass uses.
type verificationStore interface {
	store.ReconciliationStore
	store.VerificationStore
	pricing.RateSource
}

// baseRaterAdapter lets the reconciler price tokens at base rates without
// importing internal/pricing, which the reconciler must stay independent of.
type baseRaterAdapter struct {
	ctx      context.Context
	resolver *pricing.Resolver
}

var _ reconciler.BaseRater = (*baseRaterAdapter)(nil)

func (a *baseRaterAdapter) BaseRateCost(model string, t reconciler.TokenCounts) (float64, bool) {
	base, _ := reconciler.NormalizeModel(model)
	// Ledger rows that predate the 5m/1h cache-write split carry only the
	// total; price the unsplit remainder at the 5m rate.
	cacheWrite5m := t.CacheWrite5m + max(0, t.CacheWrite-t.CacheWrite5m-t.CacheWrite1h)
	p := a.resolver.Price(a.ctx, reconciler.RuntimeClaudeCode, base, time.Time{}, pricing.Tokens{
		Input:        t.Input,
		Output:       t.Output,
		CacheRead:    t.CacheRead,
		CacheWrite5m: cacheWrite5m,
		CacheWrite1h: t.CacheWrite1h,
	})
	return p.CostUSD, p.Source != pricing.SourceUnknown
}

// verifyCosts runs the OTel cost reconciler over recent sessions and stores the
// verdicts in cost_verification. Like the legacy reconcile in Runner.Run it is
// a monitor: a failure is logged and never fails the pass that already wrote
// allocation and rollup. A nil prom disables it. With dryRun it evaluates and
// logs but writes nothing.
func verifyCosts(ctx context.Context, st verificationStore, prom reconciler.PromReader, dryRun bool, logger *slog.Logger) {
	if prom == nil {
		return
	}

	resolver := pricing.NewResolver(st)
	// The resolver logs a failed refresh itself and serves embedded rates.
	_ = resolver.Refresh(ctx)
	rec := reconciler.New(prom, storeadapter.NewLedger(st), &baseRaterAdapter{ctx: ctx, resolver: resolver}, reconciler.Config{})

	results, err := rec.ReconcileSince(ctx, time.Now().Add(-verificationLookback))
	if err != nil {
		logger.Warn("verification failed (allocation/rollup already written)", "error", err)
		return
	}

	counts := map[reconciler.Verdict]int{}
	for _, v := range results {
		counts[v.Verdict]++
		if v.Verdict.IsAlert() {
			logger.Warn("verification: cost alert",
				"session_id", v.SessionID, "verdict", v.Verdict, "reason", v.Details.Reason,
				"hub_usd", v.HubUSD, "vendor_usd", v.VendorUSD, "delta_usd", v.DeltaUSD)
		}
	}
	if !dryRun && len(results) > 0 {
		if err := storeadapter.NewVerifications(st).Save(ctx, results); err != nil {
			logger.Warn("verification: save failed (allocation/rollup already written)", "error", err)
			return
		}
	}
	logger.Info("verification complete",
		"sessions", len(results),
		"within_tolerance", counts[reconciler.VerdictWithinTolerance],
		"unconverged", counts[reconciler.VerdictUnconverged],
		"diverged", counts[reconciler.VerdictDiverged],
		"premium_pricing", counts[reconciler.VerdictPremiumPricing],
		"unverifiable", counts[reconciler.VerdictUnverifiable],
		"dry_run", dryRun)
}
