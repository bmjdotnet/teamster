package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/store/sqlite"
)

var pricingNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func newPricingTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func runPricingT(t *testing.T, st store.PricingStore, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runPricingWith(context.Background(), st, args, &out, &errb, func() time.Time { return pricingNow })
	return code, out.String(), errb.String()
}

func addArgs(model string, extra ...string) []string {
	base := []string{"add", "--model", model, "--source-url", "https://example.test/pricing",
		"--input", "2", "--output", "10", "--cache-read", "0.2", "--cache-write-5m", "2.5", "--cache-write-1h", "4"}
	return append(base, extra...)
}

func countRates(t *testing.T, st store.PricingStore) int {
	t.Helper()
	all, err := st.ListRates(context.Background(), store.RateFilter{})
	if err != nil {
		t.Fatalf("ListRates: %v", err)
	}
	return len(all)
}

func TestPricingListDefaultAndFilters(t *testing.T) {
	st := newPricingTestStore(t)
	seed := len(store.ModelPricingSeedV1()) + len(store.EmbeddedFallbackSentinelsV1())

	code, out, errs := runPricingT(t, st, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, errs)
	}
	if lines := strings.Count(strings.TrimRight(out, "\n"), "\n") + 1; lines != seed+1 {
		t.Errorf("list printed %d lines, want header + %d rows", lines, seed)
	}
	for _, want := range []string{"MODEL", "claude-opus-4-8", "gpt-6-luna", "https://platform.claude.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q", want)
		}
	}

	_, out, _ = runPricingT(t, st, "list", "--runtime", "codex")
	if strings.Contains(out, "claude-opus") || !strings.Contains(out, "gpt-5.5") {
		t.Errorf("--runtime codex output wrong:\n%s", out)
	}

	code, out, errs = runPricingT(t, st, "list", "--json")
	if code != 0 {
		t.Fatalf("list --json exit %d: %s", code, errs)
	}
	var rows []rateRowJSON
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("list --json is not valid JSON: %v\n%s", err, out)
	}
	if len(rows) != seed {
		t.Errorf("--json returned %d rows, want %d", len(rows), seed)
	}
	var found bool
	for _, r := range rows {
		if r.ModelKey == "claude-opus-4-8" {
			found = true
			if r.InputPerMtok != 5 || r.ValidTo != nil || r.Runtime != "claude_code" || r.ID == 0 {
				t.Errorf("opus-4-8 json row = %+v", r)
			}
		}
	}
	if !found {
		t.Error("claude-opus-4-8 missing from --json")
	}

	// Before the epoch-sentinel seed validity nothing is in effect.
	_, out, _ = runPricingT(t, st, "list", "--at", "1969-12-31")
	if strings.Count(out, "\n") != 1 {
		t.Errorf("--at before the seed validity should list only the header:\n%s", out)
	}
	if code, _, _ := runPricingT(t, st, "list", "--at", "yesterday-ish"); code != 2 {
		t.Errorf("bad --at exit = %d, want 2", code)
	}
	if code, _, _ := runPricingT(t, st, "list", "stray"); code != 2 {
		t.Errorf("stray argument exit = %d, want 2", code)
	}
}

// README gate 3 at the CLI level: change a rate through the admin surface; new
// usage resolves at the new rate, old usage and the old row are untouched.
func TestPricingAddRateChangeLeavesHistoryUntouched(t *testing.T) {
	st := newPricingTestStore(t)
	ctx := context.Background()
	old, err := st.ResolveRate(ctx, "claude_code", "claude-sonnet-4-6", pricingNow)
	if err != nil {
		t.Fatalf("ResolveRate: %v", err)
	}

	code, out, errs := runPricingT(t, st, addArgs("claude-sonnet-4-6", "--valid-from", "2026-11-01", "--notes", "test change")...)
	if code != 0 {
		t.Fatalf("add exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "added rate id=") || !strings.Contains(out, "claude-sonnet-4-6") {
		t.Errorf("add output = %q", out)
	}

	before, _ := st.ResolveRate(ctx, "claude_code", "claude-sonnet-4-6", time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))
	after, _ := st.ResolveRate(ctx, "claude_code", "claude-sonnet-4-6", time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	if before.ID != old.ID || before.InputPerMtok != old.InputPerMtok {
		t.Errorf("usage before the change resolved %+v, want the original row %+v", before, old)
	}
	if after.ID == old.ID || after.InputPerMtok != 2 || after.OutputPerMtok != 10 || after.Notes != "test change" {
		t.Errorf("usage after the change resolved %+v, want the new 2/10 row", after)
	}

	_, out, _ = runPricingT(t, st, "list", "--all", "--runtime", "claude_code")
	if strings.Count(out, "claude-sonnet-4-6") != 2 {
		t.Errorf("--all should show both the original and the new sonnet-4-6 row:\n%s", out)
	}
}

func TestPricingAddRequiresEveryRateAndTheSource(t *testing.T) {
	st := newPricingTestStore(t)
	before := countRates(t, st)
	full := addArgs("claude-probe-model")
	for _, flagName := range []string{"--model", "--source-url", "--input", "--output", "--cache-read", "--cache-write-5m", "--cache-write-1h"} {
		var args []string
		for i := 0; i < len(full); i++ {
			if full[i] == flagName {
				i++ // drop the flag and its value
				continue
			}
			args = append(args, full[i])
		}
		code, _, errs := runPricingT(t, st, args...)
		if code != 2 || !strings.Contains(errs, flagName) {
			t.Errorf("omitting %s: exit %d, stderr %q, want exit 2 naming the flag", flagName, code, errs)
		}
	}
	if n := countRates(t, st); n != before {
		t.Errorf("a rejected add stored a row: %d before, %d after", before, n)
	}
}

func TestPricingAddAcceptsExplicitZeroRate(t *testing.T) {
	st := newPricingTestStore(t)
	args := addArgs("gpt-zero-tier", "--runtime", "codex")
	for i, a := range args {
		if a == "--cache-write-1h" {
			args[i+1] = "0"
		}
	}
	if code, _, errs := runPricingT(t, st, args...); code != 0 {
		t.Fatalf("explicit 0 rate rejected (exit %d): %s", code, errs)
	}
	r, err := st.ResolveRate(context.Background(), "codex", "gpt-zero-tier", pricingNow)
	if err != nil || r.CacheWrite1hPerMtok != 0 || r.InputPerMtok != 2 {
		t.Errorf("stored rate = %+v, %v", r, err)
	}
}

func TestPricingAddRejectionsStoreNothing(t *testing.T) {
	st := newPricingTestStore(t)
	before := countRates(t, st)
	for name, extra := range map[string][]string{
		"unknown runtime": {"--runtime", "gemini"},
		"unknown match":   {"--match-kind", "regex"},
		"unknown variant": {"--variant", "2m"},
	} {
		if code, _, errs := runPricingT(t, st, addArgs("claude-bad-"+strings.ReplaceAll(name, " ", "-"), extra...)...); code != 1 {
			t.Errorf("%s: exit %d (%s), want 1", name, code, errs)
		}
	}
	if code, _, errs := runPricingT(t, st, addArgs("claude-label[1m]")...); code != 1 {
		t.Errorf("[1m] key: exit %d (%s), want 1", code, errs)
	}
	sub := addArgs("claude-submicro")
	for i, a := range sub {
		if a == "--cache-read" {
			sub[i+1] = "0.0000001"
		}
	}
	if code, _, errs := runPricingT(t, st, sub...); code != 1 || !strings.Contains(errs, "decimal places") {
		t.Errorf("sub-microdollar rate: exit %d, stderr %q, want 1 naming the precision", code, errs)
	}
	if n := countRates(t, st); n != before {
		t.Errorf("rejected adds stored rows: %d before, %d after", before, n)
	}

	// The same key at the same valid_from twice is a conflict, not a silent overwrite.
	if code, _, errs := runPricingT(t, st, addArgs("claude-dup", "--valid-from", "2026-12-01")...); code != 0 {
		t.Fatalf("first add exit %d: %s", code, errs)
	}
	if code, _, errs := runPricingT(t, st, addArgs("claude-dup", "--valid-from", "2026-12-01")...); code != 1 || !strings.Contains(errs, "conflict") {
		t.Errorf("duplicate add: exit %d, stderr %q, want 1 with a conflict", code, errs)
	}
}

func TestPricingAddTimeDefaultsAndFormats(t *testing.T) {
	st := newPricingTestStore(t)
	ctx := context.Background()
	for model, extra := range map[string][]string{
		"claude-t-default": nil,
		"claude-t-date":    {"--valid-from", "2026-09-01"},
		"claude-t-rfc":     {"--valid-from", "2026-09-02T03:04:05Z"},
	} {
		if code, _, errs := runPricingT(t, st, addArgs(model, extra...)...); code != 0 {
			t.Fatalf("%s: exit %d: %s", model, code, errs)
		}
	}
	want := map[string]time.Time{
		"claude-t-default": pricingNow,
		"claude-t-date":    time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		"claude-t-rfc":     time.Date(2026, 9, 2, 3, 4, 5, 0, time.UTC),
	}
	all, _ := st.ListRates(ctx, store.RateFilter{})
	for _, r := range all {
		if w, ok := want[r.ModelKey]; ok {
			if !r.ValidFrom.Equal(w) {
				t.Errorf("%s valid_from = %v, want %v", r.ModelKey, r.ValidFrom, w)
			}
			if !r.FetchedAt.Equal(pricingNow) {
				t.Errorf("%s fetched_at = %v, want the default now %v", r.ModelKey, r.FetchedAt, pricingNow)
			}
			delete(want, r.ModelKey)
		}
	}
	if len(want) != 0 {
		t.Errorf("rows not found: %v", want)
	}
	if code, _, _ := runPricingT(t, st, addArgs("claude-t-bad", "--valid-from", "next tuesday")...); code != 2 {
		t.Errorf("unparseable --valid-from exit = %d, want 2", code)
	}
}

func TestPricingClose(t *testing.T) {
	st := newPricingTestStore(t)
	ctx := context.Background()
	if code, _, errs := runPricingT(t, st, addArgs("claude-closeme", "--match-kind", "exact", "--valid-from", "2026-09-01")...); code != 0 {
		t.Fatalf("add exit %d: %s", code, errs)
	}
	rate, err := st.ResolveRate(ctx, "claude_code", "claude-closeme", pricingNow)
	if err != nil {
		t.Fatalf("ResolveRate: %v", err)
	}
	id := rate.ID

	code, out, errs := runPricingT(t, st, "close", itoa(id), "--at", "2026-10-01")
	if code != 0 {
		t.Fatalf("close exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "closing:") || !strings.Contains(out, "claude-closeme") || !strings.Contains(out, "closed rate id="+itoa(id)) {
		t.Errorf("close output = %q", out)
	}
	if _, err := st.ResolveRate(ctx, "claude_code", "claude-closeme", pricingNow); err == nil {
		t.Error("a closed exact row still resolves after its valid_to")
	}
	if _, out, _ := runPricingT(t, st, "list"); strings.Contains(out, "claude-closeme") {
		t.Error("default list (in effect now) still shows the closed row")
	}
	if _, out, _ := runPricingT(t, st, "list", "--all"); !strings.Contains(out, "claude-closeme") {
		t.Error("list --all must keep the closed row (history is never dropped)")
	}

	if code, _, _ := runPricingT(t, st, "close", itoa(id)); code != 1 {
		t.Errorf("closing an already-closed row: exit %d, want 1", code)
	}
	if code, _, errs := runPricingT(t, st, "close", "987654"); code != 1 || !strings.Contains(errs, "no rate with id") {
		t.Errorf("unknown id: exit %d, stderr %q", code, errs)
	}
	for _, args := range [][]string{{"close"}, {"close", "abc"}, {"close", "0"}, {"close", "1", "2"}} {
		if code, _, _ := runPricingT(t, st, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}

	// --at may precede the id.
	if code, _, errs := runPricingT(t, st, addArgs("claude-closeme2", "--match-kind", "exact", "--valid-from", "2026-09-01")...); code != 0 {
		t.Fatalf("add exit %d: %s", code, errs)
	}
	r2, _ := st.ResolveRate(ctx, "claude_code", "claude-closeme2", pricingNow)
	if code, _, errs := runPricingT(t, st, "close", "--at", "2026-10-01", itoa(r2.ID)); code != 0 {
		t.Errorf("--at before the id: exit %d: %s", code, errs)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestPricingUsageAndUnknownSubcommand(t *testing.T) {
	st := newPricingTestStore(t)
	if code, _, errs := runPricingT(t, st); code != 2 || !strings.Contains(errs, "usage: teamster pricing") {
		t.Errorf("no args: exit %d, stderr %q", code, errs)
	}
	if code, out, _ := runPricingT(t, st, "help"); code != 0 || !strings.Contains(out, "--source-url") {
		t.Errorf("help: exit %d, stdout %q", code, out)
	}
	if code, _, errs := runPricingT(t, st, "bogus"); code != 2 || !strings.Contains(errs, "unknown pricing subcommand") {
		t.Errorf("bogus: exit %d, stderr %q", code, errs)
	}
	if !strings.Contains(pricingUsage, "shorter-prefix") {
		t.Error("usage must warn that closing a row can fall through to a shorter-prefix row")
	}
}
