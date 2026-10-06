package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

const pricingUsage = `usage: teamster pricing <subcommand>

Edits the model_pricing rate card. All rates are USD per MILLION tokens.

subcommands:
  list                      show rates (default: those in effect now)
  add                       insert a rate row (a rate change = a new row with a later --valid-from)
  close <id>                end an open rate row's validity

flags (list):
  --runtime <r>             claude_code or codex (default: both)
  --at <time>               rates in effect at this instant (default now)
  --all                     every row, closed history included (ignores --at)
  --json                    machine-readable output

flags (add) — all five rates are required (a forgotten rate must not silently be $0):
  --model <key>             model id, prefix, or class token
  --input <n> --output <n> --cache-read <n> --cache-write-5m <n> --cache-write-1h <n>
  --source-url <url>        REQUIRED: where the rate is published (rates are cited, never remembered)
  --runtime <r>             claude_code (default) or codex
  --match-kind <kind>       prefix (default; matches the key and any longer id starting with it,
                            so dated variants are covered), exact, or class
  --variant <v>             base (default) or 1m
  --valid-from <time>       when the rate takes effect (default now)
  --fetched-at <time>       when the source was consulted (default now)
  --notes <text>            free text

flags (close):
  --at <time>               end of validity, exclusive (default now)

Times are RFC3339 (2026-10-03T00:00:00Z), a date (2026-10-03, UTC midnight), or "now".

Closing a row does not make the model unknown while a shorter-prefix row or a class
row still matches it; close or shadow every row that matches a retired model.`

// runPricing dispatches the `teamster pricing <subcommand>` family. Returns
// the exit code. The store is opened only for a real subcommand, and resolves
// its DSN exactly as the other CLI subcommands do ($TEAMSTER_STORE_DSN, then
// teamster.yaml) — so a stray invocation reaches the live hub.
func runPricing(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, pricingUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stdout, pricingUsage)
		return 0
	case "list", "add", "close":
	default:
		fmt.Fprintf(os.Stderr, "unknown pricing subcommand: %s\n%s\n", args[0], pricingUsage)
		return 2
	}
	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pricing %s: %v\n", args[0], err)
		return 1
	}
	defer s.Close() //nolint:errcheck
	return runPricingWith(context.Background(), s, args, os.Stdout, os.Stderr, time.Now)
}

// runPricingWith is runPricing over an already-open store with injectable
// output and clock, so tests exercise the real behavior without a live DSN.
func runPricingWith(ctx context.Context, st store.PricingStore, args []string, stdout, stderr io.Writer, now func() time.Time) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, pricingUsage)
		return 2
	}
	switch args[0] {
	case "list":
		return pricingList(ctx, st, args[1:], stdout, stderr, now)
	case "add":
		return pricingAdd(ctx, st, args[1:], stdout, stderr, now)
	case "close":
		return pricingClose(ctx, st, args[1:], stdout, stderr, now)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, pricingUsage)
		return 0
	}
	fmt.Fprintf(stderr, "unknown pricing subcommand: %s\n%s\n", args[0], pricingUsage)
	return 2
}

// parsePricingTime accepts "now", RFC3339, or a YYYY-MM-DD date (UTC midnight).
func parsePricingTime(s string, now time.Time) (time.Time, error) {
	if s == "now" {
		return now.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q (want RFC3339, YYYY-MM-DD, or now)", s)
}

func formatRate(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func formatRateTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func rateTo(r store.ModelRate) string {
	if r.ValidTo == nil {
		return "-"
	}
	return formatRateTime(*r.ValidTo)
}

type rateRowJSON struct {
	ID                  int64   `json:"id"`
	Runtime             string  `json:"runtime"`
	MatchKind           string  `json:"match_kind"`
	ModelKey            string  `json:"model_key"`
	Variant             string  `json:"variant"`
	InputPerMtok        float64 `json:"input_per_mtok"`
	OutputPerMtok       float64 `json:"output_per_mtok"`
	CacheReadPerMtok    float64 `json:"cache_read_per_mtok"`
	CacheWrite5mPerMtok float64 `json:"cache_write_5m_per_mtok"`
	CacheWrite1hPerMtok float64 `json:"cache_write_1h_per_mtok"`
	ValidFrom           string  `json:"valid_from"`
	ValidTo             *string `json:"valid_to"`
	SourceURL           string  `json:"source_url"`
	FetchedAt           string  `json:"fetched_at"`
	Notes               string  `json:"notes,omitempty"`
}

func toRateJSON(r store.ModelRate) rateRowJSON {
	j := rateRowJSON{
		ID: r.ID, Runtime: r.Runtime, MatchKind: r.MatchKind, ModelKey: r.ModelKey, Variant: r.Variant,
		InputPerMtok: r.InputPerMtok, OutputPerMtok: r.OutputPerMtok, CacheReadPerMtok: r.CacheReadPerMtok,
		CacheWrite5mPerMtok: r.CacheWrite5mPerMtok, CacheWrite1hPerMtok: r.CacheWrite1hPerMtok,
		ValidFrom: formatRateTime(r.ValidFrom), SourceURL: r.SourceURL, FetchedAt: formatRateTime(r.FetchedAt), Notes: r.Notes,
	}
	if r.ValidTo != nil {
		s := formatRateTime(*r.ValidTo)
		j.ValidTo = &s
	}
	return j
}

func writeRateTable(w io.Writer, rates []store.ModelRate) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tRUNTIME\tMATCH\tMODEL\tVARIANT\tINPUT\tOUTPUT\tCACHE_READ\tCW_5M\tCW_1H\tVALID_FROM\tVALID_TO\tSOURCE")
	for _, r := range rates {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Runtime, r.MatchKind, r.ModelKey, r.Variant,
			formatRate(r.InputPerMtok), formatRate(r.OutputPerMtok), formatRate(r.CacheReadPerMtok),
			formatRate(r.CacheWrite5mPerMtok), formatRate(r.CacheWrite1hPerMtok),
			formatRateTime(r.ValidFrom), rateTo(r), r.SourceURL)
	}
	tw.Flush() //nolint:errcheck
}

func pricingList(ctx context.Context, st store.PricingStore, args []string, stdout, stderr io.Writer, now func() time.Time) int {
	fs := flag.NewFlagSet("teamster pricing list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runtime := fs.String("runtime", "", "claude_code or codex (default: both)")
	at := fs.String("at", "now", "rates in effect at this instant")
	all := fs.Bool("all", false, "every row, closed history included")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: teamster pricing list [--runtime r] [--at time | --all] [--json]")
		return 2
	}
	filter := store.RateFilter{Runtime: *runtime}
	if !*all {
		t, err := parsePricingTime(*at, now())
		if err != nil {
			fmt.Fprintf(stderr, "pricing list: --at: %v\n", err)
			return 2
		}
		filter.At = &t
	}
	rates, err := st.ListRates(ctx, filter)
	if err != nil {
		fmt.Fprintf(stderr, "pricing list: %v\n", err)
		return 1
	}
	if *jsonOut {
		out := make([]rateRowJSON, 0, len(rates))
		for _, r := range rates {
			out = append(out, toRateJSON(r))
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "pricing list: %v\n", err)
			return 1
		}
		return 0
	}
	writeRateTable(stdout, rates)
	return 0
}

func pricingAdd(ctx context.Context, st store.PricingStore, args []string, stdout, stderr io.Writer, now func() time.Time) int {
	fs := flag.NewFlagSet("teamster pricing add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runtime := fs.String("runtime", store.RateRuntimeClaudeCode, "claude_code or codex")
	match := fs.String("match-kind", store.RateMatchPrefix, "prefix, exact, or class")
	model := fs.String("model", "", "model id, prefix, or class token")
	variant := fs.String("variant", store.RateVariantBase, "base or 1m")
	input := fs.Float64("input", 0, "input USD per Mtok")
	output := fs.Float64("output", 0, "output USD per Mtok")
	cacheRead := fs.Float64("cache-read", 0, "cache read USD per Mtok")
	cw5m := fs.Float64("cache-write-5m", 0, "5-minute cache write USD per Mtok")
	cw1h := fs.Float64("cache-write-1h", 0, "1-hour cache write USD per Mtok")
	sourceURL := fs.String("source-url", "", "where the rate is published (required)")
	validFrom := fs.String("valid-from", "now", "when the rate takes effect")
	fetchedAt := fs.String("fetched-at", "now", "when the source was consulted")
	notes := fs.String("notes", "", "free text")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "pricing add: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var missing []string
	for _, name := range []string{"model", "source-url", "input", "output", "cache-read", "cache-write-5m", "cache-write-1h"} {
		if !set[name] {
			missing = append(missing, "--"+name)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "pricing add: missing required flag(s): %s\n(every rate must be given explicitly, 0 included — an omitted rate must not silently price at $0)\n",
			strings.Join(missing, ", "))
		return 2
	}

	from, err := parsePricingTime(*validFrom, now())
	if err != nil {
		fmt.Fprintf(stderr, "pricing add: --valid-from: %v\n", err)
		return 2
	}
	fetched, err := parsePricingTime(*fetchedAt, now())
	if err != nil {
		fmt.Fprintf(stderr, "pricing add: --fetched-at: %v\n", err)
		return 2
	}

	rate := store.ModelRate{
		Runtime: *runtime, MatchKind: *match, ModelKey: *model, Variant: *variant,
		InputPerMtok: *input, OutputPerMtok: *output, CacheReadPerMtok: *cacheRead,
		CacheWrite5mPerMtok: *cw5m, CacheWrite1hPerMtok: *cw1h,
		ValidFrom: from, SourceURL: *sourceURL, FetchedAt: fetched, Notes: *notes,
	}
	id, err := st.UpsertRate(ctx, rate)
	if err != nil {
		fmt.Fprintf(stderr, "pricing add: %v\n", err)
		return 1
	}
	rate.ID = id
	journalPricingChange("rate added", rate)
	fmt.Fprintf(stdout, "added rate id=%d\n", id)
	writeRateTable(stdout, []store.ModelRate{rate})
	return 0
}

func pricingClose(ctx context.Context, st store.PricingStore, args []string, stdout, stderr io.Writer, now func() time.Time) int {
	fs := flag.NewFlagSet("teamster pricing close", flag.ContinueOnError)
	fs.SetOutput(stderr)
	at := fs.String("at", "now", "end of validity, exclusive")
	// Accept the id before or after --at.
	var idArg string
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		idArg, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if idArg == "" && fs.NArg() == 1 {
		idArg = fs.Arg(0)
	} else if fs.NArg() != 0 {
		idArg = ""
	}
	id, err := strconv.ParseInt(idArg, 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintln(stderr, "usage: teamster pricing close <id> [--at time]")
		return 2
	}
	validTo, err := parsePricingTime(*at, now())
	if err != nil {
		fmt.Fprintf(stderr, "pricing close: --at: %v\n", err)
		return 2
	}

	all, err := st.ListRates(ctx, store.RateFilter{})
	if err != nil {
		fmt.Fprintf(stderr, "pricing close: %v\n", err)
		return 1
	}
	var target *store.ModelRate
	for i := range all {
		if all[i].ID == id {
			target = &all[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintf(stderr, "pricing close: no rate with id %d\n", id)
		return 1
	}
	fmt.Fprintln(stdout, "closing:")
	writeRateTable(stdout, []store.ModelRate{*target})

	if err := st.CloseRate(ctx, id, validTo); err != nil {
		fmt.Fprintf(stderr, "pricing close: %v\n", err)
		return 1
	}
	closed := *target
	closed.ValidTo = &validTo
	journalPricingChange("rate closed", closed)
	fmt.Fprintf(stdout, "closed rate id=%d at %s\n", id, formatRateTime(validTo))
	return 0
}

// journalPricingChange records a sanctioned rate edit in the log, with the
// operator and the full row, so a rate change is attributable after the fact.
func journalPricingChange(what string, r store.ModelRate) {
	operator := "unknown"
	if u, err := user.Current(); err == nil {
		operator = u.Username
	}
	host, _ := os.Hostname()
	valid := formatRateTime(r.ValidFrom)
	if r.ValidTo != nil {
		valid += ".." + formatRateTime(*r.ValidTo)
	}
	slog.Info("pricing: "+what,
		"id", r.ID, "runtime", r.Runtime, "match", r.MatchKind, "model", r.ModelKey, "variant", r.Variant,
		"input", r.InputPerMtok, "output", r.OutputPerMtok, "cache_read", r.CacheReadPerMtok,
		"cache_write_5m", r.CacheWrite5mPerMtok, "cache_write_1h", r.CacheWrite1hPerMtok,
		"valid", valid, "source_url", r.SourceURL, "operator", operator, "host", host)
}
