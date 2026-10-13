package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

type repriceOptions struct {
	Backfill bool
	Apply    bool
	Session  string
	Model    string
	Since    time.Duration
	Reason   string
}

type modelDrift struct {
	count    int
	oldTotal float64
	newTotal float64
}

func runReprice(ctx context.Context, st store.RepricerStore, opts repriceOptions, out io.Writer) error {
	filter := store.RepriceFilter{SessionID: opts.Session, Model: opts.Model}
	if opts.Since > 0 {
		filter.Since = time.Now().Add(-opts.Since)
	}
	if opts.Backfill {
		return runBackfill(ctx, st, filter, opts, out)
	}

	rows, err := st.DriftedRows(ctx, filter)
	if err != nil {
		return fmt.Errorf("query drifted rows: %w", err)
	}

	byModel := map[string]*modelDrift{}
	var oldSum, newSum float64
	for _, r := range rows {
		d := byModel[r.Model]
		if d == nil {
			d = &modelDrift{}
			byModel[r.Model] = d
		}
		d.count++
		d.oldTotal += r.OldCostUSD
		d.newTotal += r.NewCostUSD
		oldSum += r.OldCostUSD
		newSum += r.NewCostUSD
	}
	models := make([]string, 0, len(byModel))
	for m := range byModel {
		models = append(models, m)
	}
	sort.Strings(models)

	mode := "dry-run"
	if opts.Apply {
		mode = "apply"
	}
	fmt.Fprintf(out, "reprice mode=%s\n", mode)
	fmt.Fprintf(out, "rows_with_drift=%d\n", len(rows))
	fmt.Fprintf(out, "%-40s %8s %14s %14s %14s\n", "model", "count", "old_total", "new_total", "delta")
	for _, m := range models {
		d := byModel[m]
		fmt.Fprintf(out, "%-40s %8d %14.6f %14.6f %+14.6f\n", m, d.count, d.oldTotal, d.newTotal, d.newTotal-d.oldTotal)
	}
	fmt.Fprintf(out, "%-40s %8d %14.6f %14.6f %+14.6f\n", "TOTAL", len(rows), oldSum, newSum, newSum-oldSum)

	if !opts.Apply || len(rows) == 0 {
		return nil
	}

	operator := os.Getenv("USER")
	if operator == "" {
		operator = "unknown"
	}
	n, err := st.ApplyReprice(ctx, rows, opts.Reason, operator)
	if err != nil {
		return fmt.Errorf("apply reprice: %w", err)
	}
	fmt.Fprintf(out, "applied rows_updated=%d journaled=%d operator=%s\n", n, len(rows), operator)
	return nil
}

func runBackfill(ctx context.Context, st store.RepricerStore, filter store.RepriceFilter, opts repriceOptions, out io.Writer) error {
	ps, ok := st.(store.PricingStore)
	if !ok {
		return fmt.Errorf("store backend does not support rate resolution")
	}
	candidates, err := st.BackfillCandidates(ctx, filter)
	if err != nil {
		return fmt.Errorf("query backfill candidates: %w", err)
	}

	type resolved struct {
		store.BackfillMapping
		rateKey string
	}
	var mappings []resolved
	var unresolved []store.BackfillCandidate
	totalRows, mappedRows := 0, 0
	for _, c := range candidates {
		totalRows += c.Count
		rate, err := ps.ResolveRate(ctx, c.Runtime, c.Model, store.SeedRateValidFrom)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				unresolved = append(unresolved, c)
				continue
			}
			return fmt.Errorf("resolve rate %s/%s: %w", c.Runtime, c.Model, err)
		}
		mappedRows += c.Count
		mappings = append(mappings, resolved{
			BackfillMapping: store.BackfillMapping{Runtime: c.Runtime, Model: c.Model, RateID: rate.ID, Count: c.Count},
			rateKey:         rate.ModelKey,
		})
	}

	mode := "dry-run"
	if opts.Apply {
		mode = "apply"
	}
	fmt.Fprintf(out, "backfill mode=%s\n", mode)
	fmt.Fprintf(out, "candidates=%d total_rows=%d\n", len(candidates), totalRows)
	fmt.Fprintf(out, "%-12s %-40s %8s %8s  %s\n", "runtime", "model", "n_rows", "rate_id", "rate_key")
	for _, m := range mappings {
		fmt.Fprintf(out, "%-12s %-40s %8d %8d  %s\n", m.Runtime, m.Model, m.Count, m.RateID, m.rateKey)
	}
	unresolvedRows := 0
	for _, c := range unresolved {
		unresolvedRows += c.Count
		fmt.Fprintf(out, "%-12s %-40s %8d %8s  %s\n", c.Runtime, c.Model, c.Count, "-", "(no matching rate, left NULL)")
	}
	fmt.Fprintf(out, "mapped_rows=%d unresolved=%d unresolved_rows=%d\n", mappedRows, len(unresolved), unresolvedRows)

	if !opts.Apply || len(mappings) == 0 {
		return nil
	}

	operator := os.Getenv("USER")
	if operator == "" {
		operator = "unknown"
	}
	plain := make([]store.BackfillMapping, len(mappings))
	for i, m := range mappings {
		plain[i] = m.BackfillMapping
	}
	n, err := st.ApplyBackfill(ctx, plain, filter, opts.Reason, operator)
	if err != nil {
		return fmt.Errorf("apply backfill: %w", err)
	}
	fmt.Fprintf(out, "applied rows_updated=%d journaled=%d operator=%s\n", n, n, operator)
	return nil
}
