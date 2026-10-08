package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

type ratePerToken struct{ in, out, cr, cw5, cw1 float64 }

type fakeRater map[string]ratePerToken

func (f fakeRater) BaseRateCost(model string, t TokenCounts) (float64, bool) {
	r, ok := f[model]
	if !ok {
		return 0, false
	}
	return float64(t.Input)*r.in + float64(t.Output)*r.out + float64(t.CacheRead)*r.cr +
		float64(t.CacheWrite5m)*r.cw5 + float64(t.CacheWrite1h)*r.cw1, true
}

var opusRates = fakeRater{"claude-opus-4-8": {in: 5e-6, out: 25e-6, cr: 0.5e-6, cw5: 6.25e-6, cw1: 10e-6}}

// opusBaseUSD is the base-rate cost of opusTokens under opusRates.
const opusBaseUSD = 77.505

var opusTokens = TokenCounts{Input: 1000, Output: 600_000, CacheRead: 50_000_000, CacheWrite: 6_000_000, CacheWrite5m: 6_000_000}

func stripSplit(t TokenCounts) TokenCounts {
	t.CacheWrite5m, t.CacheWrite1h = 0, 0
	return t
}

// pair builds a single-model session, quiet and stable for an hour, with
// matching tokens on both sides.
func pair(model string, hubUSD, vendorUSD float64) (*LedgerSession, *VendorSession) {
	base, _ := NormalizeModel(model)
	l := &LedgerSession{
		SessionID: "s1", Rows: 100, LastRowAt: t0.Add(-time.Hour),
		Models: map[string]LedgerModel{base: {Tokens: opusTokens, CostUSD: hubUSD}},
	}
	v := &VendorSession{
		SessionID: "s1", TokensStableSince: t0.Add(-time.Hour),
		Models: map[string]VendorModel{model: {Tokens: stripSplit(opusTokens), CostUSD: vendorUSD}},
	}
	return l, v
}

func evalAt(cfg Config, rater BaseRater, l *LedgerSession, v *VendorSession) SessionVerification {
	return New(nil, nil, rater, cfg).evaluate(t0, "s1", l, v)
}

func TestRCAClosureOnConvergedSession(t *testing.T) {
	// EVIDENCE.md §3 on-disk tokens for session e475e409 with the 1h-tier fix
	// applied (fable $339.91, sonnet $348.57, opus $83.38). The client agrees
	// per model; the only gap is its haiku utility spend, which never reaches
	// the JSONL.
	fable := TokenCounts{Input: 159_501, Output: 682_625, CacheRead: 229_716_741, CacheWrite: 4_237_245, CacheWrite5m: 1_370_231, CacheWrite1h: 2_867_014}
	sonnet := TokenCounts{Input: 3_942, Output: 1_398_360, CacheRead: 661_631_282, CacheWrite: 34_425_868, CacheWrite5m: 34_425_868}
	opus := TokenCounts{Input: 347_846, Output: 634_355, CacheRead: 54_368_789, CacheWrite: 6_176_315, CacheWrite5m: 6_176_315}
	l := &LedgerSession{SessionID: "s1", Rows: 3013, LastRowAt: t0.Add(-2 * time.Hour), Models: map[string]LedgerModel{
		"claude-fable-5":  {Tokens: fable, CostUSD: 339.91},
		"claude-sonnet-5": {Tokens: sonnet, CostUSD: 348.57},
		"claude-opus-4-8": {Tokens: opus, CostUSD: 83.38},
	}}
	v := &VendorSession{SessionID: "s1", TokensStableSince: t0.Add(-2 * time.Hour), Models: map[string]VendorModel{
		"claude-fable-5":            {Tokens: stripSplit(fable), CostUSD: 339.91},
		"claude-sonnet-5":           {Tokens: stripSplit(sonnet), CostUSD: 348.57},
		"claude-opus-4-8[1m]":       {Tokens: stripSplit(opus), CostUSD: 83.38},
		"claude-haiku-4-5-20251001": {Tokens: TokenCounts{Input: 21_000, Output: 1_000}, CostUSD: 0.28},
	}}
	got := evalAt(Config{}, opusRates, l, v)

	if got.Verdict != VerdictWithinTolerance {
		t.Fatalf("verdict = %s (%s), want within_tolerance", got.Verdict, got.Details.Reason)
	}
	if got.ConvergedAt == nil || !got.ConvergedAt.Equal(t0) {
		t.Errorf("ConvergedAt = %v, want %v", got.ConvergedAt, t0)
	}
	if got.HubUSD != 771.86 || got.VendorUSD != 772.14 || got.DeltaUSD != -0.28 {
		t.Errorf("hub/vendor/delta = %v/%v/%v, want 771.86/772.14/-0.28", got.HubUSD, got.VendorUSD, got.DeltaUSD)
	}
	if len(got.Details.Premium) != 1 || !got.Details.Premium[0].Evaluated || got.Details.Premium[0].Fired {
		t.Errorf("opus [1m] tripwire should evaluate and stay quiet at $0 premium: %+v", got.Details.Premium)
	}
	if got.Verdict.IsAlert() {
		t.Error("within_tolerance must not alert")
	}
	if !got.Details.Gates.TokenParity {
		t.Errorf("token parity should hold within the haiku floor: %+v", got.Details.Parity)
	}
}

func TestLiveSessionIsSuppressedNotAlerted(t *testing.T) {
	// $35.19 of in-flight spend: client leads the on-disk JSONL.
	l, v := pair("claude-opus-4-8", 100.00, 135.19)
	v.TokensStableSince = t0.Add(-3 * time.Minute)
	got := evalAt(Config{}, nil, l, v)

	if got.Verdict != VerdictUnconverged || got.Details.Reason != ReasonVendorActive {
		t.Fatalf("verdict/reason = %s/%s, want unconverged/vendor_active", got.Verdict, got.Details.Reason)
	}
	if got.Verdict.IsAlert() {
		t.Error("an unconverged session must never alert")
	}
	if got.DeltaUSD != -35.19 {
		t.Errorf("live delta must still be recorded: got %v want -35.19", got.DeltaUSD)
	}
	if got.ConvergedAt != nil {
		t.Errorf("ConvergedAt = %v, want nil", got.ConvergedAt)
	}
}

func TestConvergenceGates(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*LedgerSession, *VendorSession)
		wantReason string
	}{
		{"vendor moved 14m ago", func(l *LedgerSession, v *VendorSession) {
			v.TokensStableSince = t0.Add(-14 * time.Minute)
		}, ReasonVendorActive},
		{"vendor stability unknown", func(l *LedgerSession, v *VendorSession) {
			v.TokensStableSince = time.Time{}
		}, ReasonVendorUnknown},
		{"vendor stable since the future (clock skew)", func(l *LedgerSession, v *VendorSession) {
			v.TokensStableSince = t0.Add(time.Minute)
		}, ReasonVendorActive},
		{"ledger row 5m old", func(l *LedgerSession, v *VendorSession) {
			l.LastRowAt = t0.Add(-5 * time.Minute)
		}, ReasonLedgerActive},
		{"ledger row in the future", func(l *LedgerSession, v *VendorSession) {
			l.LastRowAt = t0.Add(time.Minute)
		}, ReasonLedgerActive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, v := pair("claude-opus-4-8", 77.5, 80)
			tc.mutate(l, v)
			got := evalAt(Config{}, nil, l, v)
			if got.Verdict != VerdictUnconverged || got.Details.Reason != tc.wantReason {
				t.Errorf("verdict/reason = %s/%s, want unconverged/%s", got.Verdict, got.Details.Reason, tc.wantReason)
			}
			if g := got.Details.Gates; g.VendorStableForSec < 0 || g.LedgerQuietForSec < 0 {
				t.Errorf("gate durations must clamp at zero under clock skew: %+v", g)
			}
		})
	}

	t.Run("exactly at the window converges", func(t *testing.T) {
		l, v := pair("claude-opus-4-8", 77.5, 77.5)
		v.TokensStableSince = t0.Add(-15 * time.Minute)
		l.LastRowAt = t0.Add(-15 * time.Minute)
		if got := evalAt(Config{}, nil, l, v); got.Verdict != VerdictWithinTolerance {
			t.Errorf("verdict = %s, want within_tolerance", got.Verdict)
		}
	})
}

func TestTokenParityLagThenCaptureGap(t *testing.T) {
	lag := func() (*LedgerSession, *VendorSession) {
		l, v := pair("claude-opus-4-8", 60, 94)
		m := v.Models["claude-opus-4-8"]
		m.Tokens.CacheRead = opusTokens.CacheRead * 122 / 100 // client 22% ahead of the hub
		v.Models["claude-opus-4-8"] = m
		return l, v
	}

	t.Run("within grace is unconverged", func(t *testing.T) {
		l, v := lag()
		got := evalAt(Config{}, nil, l, v)
		if got.Verdict != VerdictUnconverged || got.Details.Reason != ReasonTokenParity {
			t.Fatalf("verdict/reason = %s/%s, want unconverged/token_parity", got.Verdict, got.Details.Reason)
		}
		if got.Verdict.IsAlert() {
			t.Error("a lagging scraper must not alert")
		}
	})

	t.Run("past grace is a capture gap alert", func(t *testing.T) {
		l, v := lag()
		l.LastRowAt = t0.Add(-7 * time.Hour)
		v.TokensStableSince = t0.Add(-9 * time.Hour)
		got := evalAt(Config{}, nil, l, v)
		if got.Verdict != VerdictDiverged || got.Details.Reason != ReasonCaptureGap {
			t.Fatalf("verdict/reason = %s/%s, want diverged/capture_gap", got.Verdict, got.Details.Reason)
		}
		if !got.Verdict.IsAlert() {
			t.Error("capture gap must alert")
		}
	})

	t.Run("grace runs from the later of the two quiet periods", func(t *testing.T) {
		l, v := lag()
		l.LastRowAt = t0.Add(-7 * time.Hour)
		v.TokensStableSince = t0.Add(-2 * time.Hour)
		if got := evalAt(Config{}, nil, l, v); got.Verdict != VerdictUnconverged {
			t.Errorf("verdict = %s, want unconverged (vendor only quiet 2h)", got.Verdict)
		}
	})

	t.Run("grace also waits on a ledger that went quiet recently", func(t *testing.T) {
		l, v := lag()
		l.LastRowAt = t0.Add(-time.Hour)
		v.TokensStableSince = t0.Add(-9 * time.Hour)
		if got := evalAt(Config{}, nil, l, v); got.Verdict != VerdictUnconverged || got.Details.Reason != ReasonTokenParity {
			t.Errorf("verdict/reason = %s/%s, want unconverged/token_parity (ledger only quiet 1h)", got.Verdict, got.Details.Reason)
		}
	})

	t.Run("past grace with dollars inside the band does not alert", func(t *testing.T) {
		l, v := pair("claude-opus-4-8[1m]", 60, 60.4)
		vm := v.Models["claude-opus-4-8[1m]"]
		vm.Tokens.CacheRead = opusTokens.CacheRead * 122 / 100
		v.Models["claude-opus-4-8[1m]"] = vm
		l.LastRowAt = t0.Add(-7 * time.Hour)
		v.TokensStableSince = t0.Add(-9 * time.Hour)
		got := evalAt(Config{}, opusRates, l, v)
		if got.Verdict != VerdictWithinTolerance || got.ConvergedAt == nil {
			t.Fatalf("verdict=%s convergedAt=%v, want within_tolerance and settled", got.Verdict, got.ConvergedAt)
		}
		if got.Details.Gates.TokenParity {
			t.Error("parity must stay recorded as failed")
		}
		if len(got.Details.Premium) != 0 {
			t.Errorf("the tripwire must not run on untrusted tokens: %+v", got.Details.Premium)
		}
	})

	t.Run("vendor-only session with no ledger rows", func(t *testing.T) {
		_, v := pair("claude-opus-4-8", 0, 94)
		v.TokensStableSince = t0.Add(-time.Hour)
		got := evalAt(Config{}, nil, nil, v)
		if got.Verdict != VerdictUnconverged || got.Details.Reason != ReasonTokenParity || got.HubUSD != 0 {
			t.Fatalf("got %s/%s hub=%v, want unconverged/token_parity hub=0", got.Verdict, got.Details.Reason, got.HubUSD)
		}
		v.TokensStableSince = t0.Add(-7 * time.Hour)
		got = evalAt(Config{}, nil, nil, v)
		if got.Verdict != VerdictDiverged || got.Details.Reason != ReasonCaptureGap {
			t.Fatalf("got %s/%s, want diverged/capture_gap", got.Verdict, got.Details.Reason)
		}
	})

	t.Run("hub ahead of client also fails parity", func(t *testing.T) {
		l, v := pair("claude-opus-4-8", 77.5, 77.5)
		m := l.Models["claude-opus-4-8"]
		m.Tokens.Output *= 13 // the historical ~13x overcount shape
		l.Models["claude-opus-4-8"] = m
		got := evalAt(Config{}, nil, l, v)
		if got.Verdict != VerdictUnconverged || got.Details.Reason != ReasonTokenParity {
			t.Errorf("verdict/reason = %s/%s, want unconverged/token_parity", got.Verdict, got.Details.Reason)
		}
	})
}

func TestPricingGapIsDivergedNotCaptureGap(t *testing.T) {
	// Tokens agree, dollars do not: the hub's rates are wrong.
	l, v := pair("claude-opus-4-8", 71.06, 38.63)
	got := evalAt(Config{}, nil, l, v)

	if got.Verdict != VerdictDiverged || got.Details.Reason != ReasonCostOutsideBand {
		t.Fatalf("verdict/reason = %s/%s, want diverged/cost_outside_band", got.Verdict, got.Details.Reason)
	}
	if got.ConvergedAt == nil {
		t.Error("a pricing-gap session is converged; ConvergedAt must be set")
	}
	if len(got.Details.Models) != 1 || !got.Details.Models[0].ParityOK || got.Details.Models[0].DeltaUSD != 32.43 {
		t.Errorf("model row should show parity ok with a $32.43 pricing delta: %+v", got.Details.Models)
	}
}

func TestToleranceBand(t *testing.T) {
	tests := []struct {
		client, delta float64
		want          Verdict
	}{
		{50, 0.99, VerdictWithinTolerance},
		{50, 1.01, VerdictDiverged},
		{50, -1.01, VerdictDiverged},
		{50, -0.99, VerdictWithinTolerance},
		{1000, 4.90, VerdictWithinTolerance}, // 0.5% of 1000 = 5.00 beats the $1 floor
		{1000, 5.10, VerdictDiverged},
		{1000, -5.10, VerdictDiverged},
		{0.26, 0.26, VerdictWithinTolerance}, // the permanent haiku floor
	}
	for _, tc := range tests {
		l, v := pair("claude-opus-4-8", tc.client+tc.delta, tc.client)
		got := evalAt(Config{}, nil, l, v)
		if got.Verdict != tc.want {
			t.Errorf("client=%v delta=%v: verdict = %s, want %s (tolerance %v)", tc.client, tc.delta, got.Verdict, tc.want, got.Details.ToleranceUSD)
		}
	}
}

func TestToleranceIsConfigurable(t *testing.T) {
	l, v := pair("claude-opus-4-8", 52, 50)
	if got := evalAt(Config{ToleranceUSD: 5}, nil, l, v); got.Verdict != VerdictWithinTolerance {
		t.Errorf("verdict = %s, want within_tolerance under a $5 floor", got.Verdict)
	}
}

func TestOneMLabelNormalizesForMatching(t *testing.T) {
	l, v := pair("claude-opus-4-8[1m]", opusBaseUSD, opusBaseUSD+0.1)
	got := evalAt(Config{}, opusRates, l, v)

	if got.Verdict != VerdictWithinTolerance {
		t.Fatalf("verdict = %s (%s), want within_tolerance", got.Verdict, got.Details.Reason)
	}
	if len(got.Details.Models) != 1 {
		t.Fatalf("[1m] and base must collapse to one model row, got %+v", got.Details.Models)
	}
	m := got.Details.Models[0]
	if m.Model != "claude-opus-4-8" || !reflect.DeepEqual(m.VendorLabels, []string{"claude-opus-4-8[1m]"}) || !m.ParityOK {
		t.Errorf("model row = %+v", m)
	}
	if len(got.Details.Premium) != 1 || !got.Details.Premium[0].Evaluated || got.Details.Premium[0].Fired {
		t.Errorf("tripwire should run and stay quiet at $0 premium: %+v", got.Details.Premium)
	}
}

func TestMixedLabelsMergeAndTripwireUsesCombinedVendorCost(t *testing.T) {
	half := TokenCounts{Input: 500, Output: 300_000, CacheRead: 25_000_000, CacheWrite: 3_000_000}
	l, _ := pair("claude-opus-4-8", opusBaseUSD, 0)
	v := &VendorSession{SessionID: "s1", TokensStableSince: t0.Add(-time.Hour), Models: map[string]VendorModel{
		"claude-opus-4-8":     {Tokens: half, CostUSD: opusBaseUSD / 2},
		"claude-opus-4-8[1m]": {Tokens: half, CostUSD: opusBaseUSD / 2},
	}}
	got := evalAt(Config{}, opusRates, l, v)

	if got.Verdict != VerdictWithinTolerance {
		t.Fatalf("verdict = %s (%s), want within_tolerance", got.Verdict, got.Details.Reason)
	}
	if want := []string{"claude-opus-4-8", "claude-opus-4-8[1m]"}; !reflect.DeepEqual(got.Details.Models[0].VendorLabels, want) {
		t.Errorf("labels = %v, want %v", got.Details.Models[0].VendorLabels, want)
	}
}

func TestPremiumTripwire(t *testing.T) {
	t.Run("fires and takes precedence over diverged", func(t *testing.T) {
		// Client bills $20 over base rates on the [1m] label; the hub priced at base.
		l, v := pair("claude-opus-4-8[1m]", opusBaseUSD, opusBaseUSD+20)
		got := evalAt(Config{}, opusRates, l, v)

		if got.Verdict != VerdictPremiumPricing {
			t.Fatalf("verdict = %s, want premium_pricing", got.Verdict)
		}
		if !got.Verdict.IsAlert() {
			t.Error("premium_pricing must alert")
		}
		p := got.Details.Premium[0]
		if !p.Fired || !p.Evaluated || p.ExcessUSD != 20 || p.BaseRateUSD != opusBaseUSD {
			t.Errorf("premium row = %+v", p)
		}
	})

	t.Run("hub undercount does not read as premium", func(t *testing.T) {
		// The 1h-tier defect shape: hub below client, client equal to correct base rates.
		l, v := pair("claude-opus-4-8[1m]", opusBaseUSD-20, opusBaseUSD)
		got := evalAt(Config{}, opusRates, l, v)
		if got.Verdict != VerdictDiverged {
			t.Fatalf("verdict = %s, want diverged (not premium_pricing)", got.Verdict)
		}
		if got.Details.Premium[0].Fired {
			t.Error("tripwire fired on a hub undercount")
		}
	})

	t.Run("client below base rate is not a premium", func(t *testing.T) {
		l, v := pair("claude-opus-4-8[1m]", opusBaseUSD, opusBaseUSD-20)
		got := evalAt(Config{}, opusRates, l, v)
		if got.Verdict != VerdictDiverged {
			t.Fatalf("verdict = %s, want diverged (not premium_pricing)", got.Verdict)
		}
		if p := got.Details.Premium[0]; p.Fired || p.ExcessUSD != -20 {
			t.Errorf("premium row = %+v, want unfired with excess -20", p)
		}
	})

	t.Run("not evaluated without a rater", func(t *testing.T) {
		l, v := pair("claude-opus-4-8[1m]", opusBaseUSD, opusBaseUSD+20)
		got := evalAt(Config{}, nil, l, v)
		if got.Verdict != VerdictDiverged {
			t.Fatalf("verdict = %s, want diverged", got.Verdict)
		}
		if p := got.Details.Premium[0]; p.Evaluated || p.Fired {
			t.Errorf("premium row = %+v, want unevaluated", p)
		}
	})

	t.Run("not evaluated for a model the rater does not know", func(t *testing.T) {
		l, v := pair("claude-opus-4-8[1m]", opusBaseUSD, opusBaseUSD)
		got := evalAt(Config{}, fakeRater{}, l, v)
		if p := got.Details.Premium[0]; p.Evaluated {
			t.Errorf("premium row = %+v, want unevaluated", p)
		}
		if got.Verdict != VerdictWithinTolerance {
			t.Errorf("verdict = %s, want within_tolerance", got.Verdict)
		}
	})

	t.Run("no premium rows without a [1m] label", func(t *testing.T) {
		l, v := pair("claude-opus-4-8", opusBaseUSD, opusBaseUSD)
		if got := evalAt(Config{}, opusRates, l, v); len(got.Details.Premium) != 0 {
			t.Errorf("premium rows = %+v, want none", got.Details.Premium)
		}
	})

	t.Run("not run on an unconverged session", func(t *testing.T) {
		l, v := pair("claude-opus-4-8[1m]", opusBaseUSD, opusBaseUSD+20)
		v.TokensStableSince = t0.Add(-time.Minute)
		got := evalAt(Config{}, opusRates, l, v)
		if got.Verdict != VerdictUnconverged || len(got.Details.Premium) != 0 {
			t.Errorf("verdict=%s premium=%+v, want unconverged with no premium rows", got.Verdict, got.Details.Premium)
		}
	})

	t.Run("excess inside tolerance stays quiet", func(t *testing.T) {
		l, v := pair("claude-opus-4-8[1m]", opusBaseUSD, opusBaseUSD+0.9)
		got := evalAt(Config{}, opusRates, l, v)
		if got.Verdict != VerdictWithinTolerance || got.Details.Premium[0].Fired {
			t.Errorf("verdict=%s premium=%+v", got.Verdict, got.Details.Premium)
		}
	})
}

func TestNoVendorSeriesIsUnverifiable(t *testing.T) {
	l, _ := pair("claude-opus-4-8", 12.34, 0)
	for name, v := range map[string]*VendorSession{
		"absent":  nil,
		"no data": {SessionID: "s1", Models: map[string]VendorModel{}},
	} {
		got := evalAt(Config{}, nil, l, v)
		if got.Verdict != VerdictUnverifiable || got.Details.Reason != ReasonNoVendorSeries {
			t.Errorf("%s: verdict/reason = %s/%s", name, got.Verdict, got.Details.Reason)
		}
		if got.HubUSD != 12.34 || got.VendorUSD != 0 || got.DeltaUSD != 0 {
			t.Errorf("%s: hub/vendor/delta = %v/%v/%v", name, got.HubUSD, got.VendorUSD, got.DeltaUSD)
		}
		if got.Verdict.IsAlert() {
			t.Errorf("%s: unverifiable must not alert", name)
		}
	}
}

func TestHaikuUtilityFloorDoesNotBreakParity(t *testing.T) {
	l, v := pair("claude-opus-4-8", 77.5, 77.5+0.26)
	v.Models["claude-haiku-4-5-20251001"] = VendorModel{Tokens: TokenCounts{Input: 20_921, Output: 1_001}, CostUSD: 0}
	got := evalAt(Config{}, nil, l, v)
	if got.Verdict != VerdictWithinTolerance || !got.Details.Gates.TokenParity {
		t.Errorf("verdict=%s parity=%+v", got.Verdict, got.Details.Parity)
	}
}

func TestParityFloorsArePerType(t *testing.T) {
	bump := func(f func(m *VendorModel)) *SessionVerification {
		l, v := pair("claude-opus-4-8", 77.5, 77.5)
		m := v.Models["claude-opus-4-8"]
		f(&m)
		v.Models["claude-opus-4-8"] = m
		got := evalAt(Config{}, nil, l, v)
		return &got
	}
	tests := []struct {
		name string
		f    func(m *VendorModel)
		want string
	}{
		{"output within 20k floor", func(m *VendorModel) { m.Tokens.Output += 19_000 }, ""},
		{"output past 20k floor", func(m *VendorModel) { m.Tokens.Output += 25_000 }, ReasonTokenParity},
		{"input within 100k floor", func(m *VendorModel) { m.Tokens.Input += 99_000 }, ""},
		{"input past 100k floor", func(m *VendorModel) { m.Tokens.Input += 101_000 }, ReasonTokenParity},
		{"cache read within 500k floor", func(m *VendorModel) { m.Tokens.CacheRead += 400_000 }, ""},
		{"cache read past 500k floor and 1%", func(m *VendorModel) { m.Tokens.CacheRead += 600_000 + opusTokens.CacheRead/100 }, ReasonTokenParity},
		{"cache write within 100k floor", func(m *VendorModel) { m.Tokens.CacheWrite += 99_000 }, ""},
		{"cache write past 100k floor", func(m *VendorModel) { m.Tokens.CacheWrite += 150_000 }, ReasonTokenParity},
	}
	for _, tc := range tests {
		got := bump(tc.f)
		if got.Details.Reason != tc.want {
			t.Errorf("%s: reason = %q, want %q (parity %+v)", tc.name, got.Details.Reason, tc.want, got.Details.Parity)
		}
	}
}

func TestHaikuOnlySessionResidueStaysConverged(t *testing.T) {
	// Measured on the hub (session 09c827e4): a haiku-only session where the
	// client's background utility calls leave ~135k extra cache-read tokens
	// and $0.06 that the JSONL never carries.
	l := &LedgerSession{SessionID: "s1", Rows: 10, LastRowAt: t0.Add(-18 * time.Hour), Models: map[string]LedgerModel{
		"claude-haiku-4-5-20251001": {Tokens: TokenCounts{Input: 150, Output: 3_000, CacheRead: 301_713, CacheWrite: 12_000}, CostUSD: 0.14},
	}}
	v := &VendorSession{SessionID: "s1", TokensStableSince: t0.Add(-18 * time.Hour), Models: map[string]VendorModel{
		"claude-haiku-4-5-20251001": {Tokens: TokenCounts{Input: 21_000, Output: 4_000, CacheRead: 436_159, CacheWrite: 12_000}, CostUSD: 0.20},
	}}
	got := evalAt(Config{}, nil, l, v)
	if got.Verdict != VerdictWithinTolerance || !got.Details.Gates.TokenParity {
		t.Errorf("verdict=%s reason=%s parity=%+v, want within_tolerance with parity holding", got.Verdict, got.Details.Reason, got.Details.Parity)
	}
}

func TestNormalizeModel(t *testing.T) {
	tests := []struct {
		in      string
		base    string
		premium bool
	}{
		{"claude-opus-4-8", "claude-opus-4-8", false},
		{"claude-opus-4-8[1m]", "claude-opus-4-8", true},
		{"claude-opus-4-8[1M]", "claude-opus-4-8", true},
		{"claude-opus-4-6[1m]", "claude-opus-4-6", true},
		{"  claude-sonnet-5[1m] ", "claude-sonnet-5", true},
		{"claude-opus-4-8[2m]", "claude-opus-4-8", false},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", false},
		{"[1m]", "[1m]", false},
		{"", "", false},
	}
	for _, tc := range tests {
		base, premium := NormalizeModel(tc.in)
		if base != tc.base || premium != tc.premium {
			t.Errorf("NormalizeModel(%q) = %q,%v want %q,%v", tc.in, base, premium, tc.base, tc.premium)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	want := Config{
		ConvergenceWindow: 15 * time.Minute, ToleranceUSD: 1, ToleranceFrac: 0.005,
		ParityRelTol: 0.01, CaptureGapGrace: 6 * time.Hour,
		ParityFloor: ParityFloors{Input: 100_000, Output: 20_000, CacheRead: 500_000, CacheWrite: 100_000},
	}
	if c != want {
		t.Errorf("defaults = %+v, want %+v", c, want)
	}
	keep := Config{ConvergenceWindow: time.Minute, ParityFloor: ParityFloors{Output: 7}}.withDefaults()
	if keep.ConvergenceWindow != time.Minute || keep.ParityFloor.Output != 7 || keep.ParityFloor.CacheRead != 500_000 {
		t.Errorf("explicit values must survive and unset ones default: %+v", keep)
	}
}

type fakeProm struct {
	sessions  map[string]VendorSession
	err       error
	gotIDs    []string
	gotAt     time.Time
	gotWindow time.Duration
}

func (f *fakeProm) ReadSessions(_ context.Context, ids []string, at time.Time, w time.Duration) (map[string]VendorSession, error) {
	f.gotIDs, f.gotAt, f.gotWindow = ids, at, w
	return f.sessions, f.err
}

type fakeLedger struct {
	sessions map[string]LedgerSession
	since    []string
	err      error
	sinceErr error
	gotIDs   []string
	gotSince time.Time
}

func (f *fakeLedger) ReadSessions(_ context.Context, ids []string) (map[string]LedgerSession, error) {
	f.gotIDs = ids
	return f.sessions, f.err
}

func (f *fakeLedger) SessionsSince(_ context.Context, since time.Time) ([]string, error) {
	f.gotSince = since
	return f.since, f.sinceErr
}

func newFakes() (*fakeProm, *fakeLedger) {
	l1, v1 := pair("claude-opus-4-8", 77.5, 77.6)
	l2, v2 := pair("claude-opus-4-8", 10, 50)
	l1.SessionID, v1.SessionID, l2.SessionID, v2.SessionID = "b-session", "b-session", "a-session", "a-session"
	return &fakeProm{sessions: map[string]VendorSession{"a-session": *v2, "b-session": *v1}},
		&fakeLedger{sessions: map[string]LedgerSession{"a-session": *l2, "b-session": *l1, "c-session": {SessionID: "c-session", Models: map[string]LedgerModel{}}}}
}

func TestReconcilePlumbing(t *testing.T) {
	prom, ledger := newFakes()
	r := New(prom, ledger, nil, Config{ConvergenceWindow: 20 * time.Minute})
	r.now = func() time.Time { return t0 }

	got, err := r.Reconcile(context.Background(), []string{"b-session", "a-session", "b-session", "", "c-session"})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(got))
	for i, g := range got {
		ids[i] = g.SessionID
	}
	if want := []string{"a-session", "b-session", "c-session"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("result order = %v, want %v", ids, want)
	}
	if !reflect.DeepEqual(prom.gotIDs, ledger.gotIDs) || len(prom.gotIDs) != 3 {
		t.Errorf("readers must see the same deduped ids: prom=%v ledger=%v", prom.gotIDs, ledger.gotIDs)
	}
	if !prom.gotAt.Equal(t0) || prom.gotWindow != 6*time.Hour {
		t.Errorf("prom got at=%v horizon=%v, want %v / 6h (the capture-gap grace outranks the 20m window)", prom.gotAt, prom.gotWindow, t0)
	}
	if _, err := New(prom, ledger, nil, Config{ConvergenceWindow: 8 * time.Hour}).Reconcile(context.Background(), []string{"a-session"}); err != nil {
		t.Fatal(err)
	}
	if prom.gotWindow != 8*time.Hour {
		t.Errorf("horizon = %v, want 8h when the window exceeds the grace", prom.gotWindow)
	}
	verdicts := map[string]Verdict{}
	for _, g := range got {
		verdicts[g.SessionID] = g.Verdict
	}
	want := map[string]Verdict{"a-session": VerdictDiverged, "b-session": VerdictWithinTolerance, "c-session": VerdictUnverifiable}
	if !reflect.DeepEqual(verdicts, want) {
		t.Errorf("verdicts = %v, want %v", verdicts, want)
	}
}

func TestReconcileSince(t *testing.T) {
	prom, ledger := newFakes()
	ledger.since = []string{"a-session", "b-session"}
	r := New(prom, ledger, nil, Config{})
	r.now = func() time.Time { return t0 }

	since := t0.Add(-48 * time.Hour)
	got, err := r.ReconcileSince(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !ledger.gotSince.Equal(since) {
		t.Errorf("results=%d gotSince=%v", len(got), ledger.gotSince)
	}
}

func TestReconcileEmptyInputSkipsReaders(t *testing.T) {
	prom, ledger := newFakes()
	got, err := New(prom, ledger, nil, Config{}).Reconcile(context.Background(), nil)
	if err != nil || got != nil || prom.gotIDs != nil || ledger.gotIDs != nil {
		t.Errorf("got=%v err=%v promIDs=%v ledgerIDs=%v", got, err, prom.gotIDs, ledger.gotIDs)
	}
}

func TestReaderErrorsFailTheWholePass(t *testing.T) {
	boom := errors.New("boom")
	tests := map[string]func(*fakeProm, *fakeLedger){
		"prom":   func(p *fakeProm, l *fakeLedger) { p.err = boom },
		"ledger": func(p *fakeProm, l *fakeLedger) { l.err = boom },
		"list":   func(p *fakeProm, l *fakeLedger) { l.sinceErr = boom },
	}
	for name, mutate := range tests {
		prom, ledger := newFakes()
		ledger.since = []string{"a-session"}
		mutate(prom, ledger)
		got, err := New(prom, ledger, nil, Config{}).ReconcileSince(context.Background(), t0)
		if !errors.Is(err, boom) || got != nil {
			t.Errorf("%s: got=%v err=%v, want nil + wrapped boom", name, got, err)
		}
	}
}

func TestVerificationMarshalsToJSON(t *testing.T) {
	l, v := pair("claude-opus-4-8[1m]", opusBaseUSD, opusBaseUSD)
	got := evalAt(Config{}, opusRates, l, v)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back["verdict"] != "within_tolerance" || back["runtime"] != "claude_code" {
		t.Errorf("json = %s", raw)
	}
	for _, k := range []string{"hub_usd", "vendor_usd", "delta_usd", "converged_at", "details"} {
		if _, ok := back[k]; !ok {
			t.Errorf("json missing %q: %s", k, raw)
		}
	}
	if strings.Contains(string(raw), "-0,") || strings.Contains(string(raw), ":-0}") {
		t.Errorf("negative zero leaked into json: %s", raw)
	}
}

func TestRound6HasNoNegativeZero(t *testing.T) {
	if got := round6(-1e-9); math.Signbit(got) {
		t.Errorf("round6(-1e-9) = %v, want +0", got)
	}
	if got := round6(0.1234565); got != 0.123457 && got != 0.123456 {
		t.Errorf("round6 = %v", got)
	}
}

// The reconciler must stay independent of the pricing code it checks and
// physically unable to write the ledger: no import of internal/pricing,
// internal/store, or a SQL driver from any non-test file in this package.
func TestPackageImportRestrictions(t *testing.T) {
	forbidden := []string{
		"github.com/bmjdotnet/teamster/internal/pricing",
		"github.com/bmjdotnet/teamster/internal/store",
		"database/sql",
		"github.com/go-sql-driver/mysql",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbidden {
				if path == bad || strings.HasPrefix(path, bad+"/") {
					t.Errorf("%s imports forbidden package %s", f, path)
				}
			}
		}
	}
	if checked < 3 {
		t.Fatalf("only %d non-test files checked; glob is wrong", checked)
	}
}
