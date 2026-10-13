# Pricing rate-change runbook

Operator procedures for the `model_pricing` rate card. All rates are USD per
**million** tokens. Rates are temporal: every row carries `valid_from` and an
optional `valid_to`, and each priced `token_ledger` row records the `rate_id`
it was priced against, so history never reprices itself.

`teamster pricing` talks to the store resolved from `$TEAMSTER_STORE_DSN`
(then `teamster.yaml`). On a hub host that is the **live** database, so every
`add`/`close` is a production change. Each one is logged (operator, host, full
row) as `pricing: rate added` / `pricing: rate closed`.

## How resolution works

`ResolveRate(runtime, model, at)` considers rows of that runtime where
`valid_from <= at < valid_to` (`valid_to` NULL = open), then picks
exact match, else longest prefix, else class token. A `[1m]` model tries
variant `1m` first, then falls back to `base`. No match is a loud unknown
(WARN + counter), never a silent $0.

If two rows for the same (runtime, match_kind, model_key, variant) overlap,
the one with the later `valid_from` wins (ties: higher id). So a new row
shadows its predecessor even if you forget to close it — but close it anyway
so the card reads cleanly.

Seed rows carry `valid_from` = Unix epoch (`SeedRateValidFrom`), meaning
"since forever": every historical ledger row resolves against them.

## 1. Add a rate

```bash
teamster pricing add \
  --model claude-opus-5 --runtime claude_code --match-kind prefix --variant base \
  --input 5 --output 25 --cache-read 0.5 --cache-write-5m 6.25 --cache-write-1h 10 \
  --source-url https://platform.claude.com/docs/en/about-claude/pricing \
  --valid-from 2026-10-03 --notes "launch pricing"
```

Required: `--model`, `--source-url`, and **all five rates** (pass `0`
explicitly; an omitted rate must not silently price at $0). Defaults:
`--runtime claude_code` (or `codex`), `--match-kind prefix` (`exact` and
`class` also valid; prefix covers dated variants), `--variant base` (or `1m`),
`--valid-from now`, `--fetched-at now`. Times: RFC3339, `YYYY-MM-DD` (UTC
midnight), or `now`. Rates take at most 6 decimal places.

Check it:

```bash
teamster pricing list --runtime claude_code            # in effect now
teamster pricing list --at 2026-09-01                  # in effect at an instant
teamster pricing list --all --json                     # full history
```

A new model with no earlier row needs only `add`. A model that already has an
open row is a rate change: use section 2.

## 2. Change a rate (close-and-insert)

When a vendor changes a price effective date `D`:

```bash
teamster pricing list --runtime claude_code | grep claude-opus-5   # note the open row's ID
teamster pricing close 42 --at 2026-11-01                          # old row: valid_to = D
teamster pricing add --model claude-opus-5 \
  --input 4 --output 20 --cache-read 0.4 --cache-write-5m 5 --cache-write-1h 8 \
  --source-url https://platform.claude.com/docs/en/about-claude/pricing \
  --valid-from 2026-11-01
```

Rules:

- Use the **same instant** for `close --at` and `add --valid-from` so there is
  no gap (a gap leaves that window unresolvable) and no overlap.
- `close` sets `valid_to` (exclusive). It fails if the row is already closed or
  `valid_to` is not after `valid_from`.
- Never edit a rate row in place; there is no CLI for it and none should be
  written. Once any ledger row has priced against a row (`token_ledger.rate_id`),
  treat it as immutable. This is a convention, not a database constraint:
  editing a referenced row silently changes what stored costs *should* be and
  shows up as drift (section 3).
- Closing a row does not make the model unknown if a shorter-prefix or class
  row still matches. To retire a model, close or shadow every row that matches
  it.
- `valid_from` in the past is allowed but does not reprice anything already in
  the ledger; those rows keep their old `rate_id`. Backdating only affects rows
  priced afterwards (e.g. late-arriving transcripts).
- Seed rows (v73 seed) are frozen migration history. Rate changes are new
  rows, never edits to the seed.

## 3. Validate after a change

```bash
rollup --reprice --reprice-dry-run                          # default; no writes
rollup --reprice --reprice-model claude-opus-5 --reprice-since 720h
```

For each `token_ledger` row with a non-NULL `rate_id`, the tool recomputes
cost from that row's own `rate_id` (input, output, cache read, 5m and 1h cache
writes) and flags rows where stored `cost_usd` differs by more than
`0.000001`. Output is per-model count, old total, new total, and delta.
Optional filters: `--reprice-session`, `--reprice-model`, `--reprice-since`.
`--reprice` cannot be combined with other rollup flags.

Rows priced from the embedded fallback tables (the rate store or hookd was
unreachable at scrape time) carry the sentinel rate row `embedded-fallback-v1`
(one per runtime, all rates zero) as their `rate_id`. The drift query excludes
these rows, since recomputing against zero rates would zero their cost. So
`rows_with_drift=0` says nothing about fallback-priced rows.

**Zero drift (`rows_with_drift=0`) after a rate change is the correct result.**
Old rows point at the old (now closed) rate row, so a new row cannot move them.

**Non-zero drift** means a stored cost no longer equals its rate row's price:

1. Identify the models in the table. Run `teamster pricing list --all --json`
   and confirm no referenced row was edited directly in the database.
2. If a rate row was wrongly entered or modified, restore it (or close it and
   add the correct row), then decide whether the affected ledger rows' stored
   costs should be rewritten. No tool repoints a ledger row to a different
   rate row; `--reprice-apply` only rewrites `cost_usd` against the row's
   existing `rate_id`.
3. If the stored costs were wrong (for example a past scraper defect), the
   recompute is the corrected value; apply it.

Apply (operator-invoked only; never run from automation):

```bash
rollup --reprice --reprice-apply \
  --reprice-reason "opus-5 cache-write-1h entered as 10, published 12; corrected row 43" \
  --reprice-model claude-opus-5
```

`--reprice-apply` requires `--reprice-reason`, conflicts with an explicit
`--reprice-dry-run=true`, updates the drifted rows, and journals every change
(old/new cost, old/new rate id, reason, operator, time) to `reprice_journal`.
It rewrites only `cost_usd`: `rate_id` is never changed, so a drift journal
row's old and new rate id are always equal. Always run the dry run with the
same filters first and read the delta.

## 4. Backfill `rate_id` on unstamped rows

`token_ledger.rate_id` is NULL on rows written before it existed (migration
v75). Drift validation skips them. To stamp them:

```bash
rollup --reprice --reprice-backfill                       # dry run (default)
rollup --reprice --reprice-backfill --reprice-model claude-opus-5 --reprice-since 720h
rollup --reprice --reprice-backfill --reprice-apply \
  --reprice-reason "backfill rate_id on pre-v75 history"
```

For each (runtime, model) among rows with NULL `rate_id` and a non-empty
model, the tool resolves the rate in effect at `SeedRateValidFrom` (the epoch,
i.e. the seed-era card) and stamps that row's id. It does **not** change
`cost_usd`. Output: `candidates`, `total_rows`, a per-model table of row count
and chosen `rate_id`, then `mapped_rows`, `unresolved` and `unresolved_rows`.
Models with no matching rate are left NULL and listed as
`(no matching rate, left NULL)`. Filters are `--reprice-session`,
`--reprice-model` and `--reprice-since`. It is dry-run by default;
`--reprice-apply` requires `--reprice-reason`, and every stamped row is
journaled to `reprice_journal` with `old_rate_id` NULL and equal old/new cost.
Run the backfill before relying on drift validation for history.
