# Codex Support — Install & Operate

**Status: partial.** This document currently covers only WP3 (the codex-scraper
cost/ledger tailer). WP7 owns assembling the full installer-integration guide
(MCP server wiring, hooks trust provisioning, skills plugin, uninstall
recipe) — merge that content in here rather than starting a second doc.

## codex-scraper — the Codex cost/ledger tailer

`codex-scraper` tails Codex CLI rollout JSONL files
(`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` and
`~/.codex/archived_sessions/*.jsonl`) and is the **sole writer of Codex cost
data**: it POSTs per-`token_count`-event telemetry rows to hookd's
`/telemetry` endpoint (the same contract `token-scraper` uses for Claude
Code) and upserts the Codex `sessions` row itself via a direct store
connection, because hookd's hook-event pipeline never fires for Codex and
WMS/cost attribution must not depend on Codex hooks (a separate, optional
component) being installed or trusted.

It is a **oneshot** binary driven by a systemd timer (`teamster-codex-scraper.timer`,
every 10 minutes) — not a daemon like `token-scraper`. Each run processes
whatever new bytes have appeared since its last run, using a persisted
per-file byte-offset cursor (`$BASEDIR/var/codex-scraper-cursors.json`),
and exits.

### Requirements / configuration

| Env var | Required | Purpose |
|---|---|---|
| `TEAMSTER_STORE_DSN` | Yes | Direct store connection for the Codex sessions-row upsert. If unset, the binary still ledgers cost via `/telemetry` but logs a warning and skips the sessions row. |
| `CODEX_HOME` | No (defaults to `~/.codex`) | Root of the Codex CLI's session data. |
| `TEAMSTER_TELEMETRY_URL` / `TEAMSTER_HOOK_SERVER_URL` | No (derived) | Where ledger rows are POSTed; derived from the hook server URL exactly like `token-scraper`. |
| `CODEX_SCRAPER_SESSION_ROOTS` | No | Comma-separated override of the directories walked for `*.jsonl` (testing/isolation only). |
| `SCRAPER_DRY_RUN` | No | `true`/`1` logs derived rows instead of POSTing/upserting — no writes anywhere. |

Graceful on a host with no `codex` CLI installed: the timer still fires, the
binary finds no rollout files under `$CODEX_HOME`, and exits 0 every run.

### What it derives, and how

- **Session identity** (`sessions.runtime='codex'`, `cwd`, `model`,
  `originator`, `cli_version`) comes from each file's `session_meta` (first
  line) and subsequent `turn_context` records (model can change turn to
  turn; the tailer keeps the last-seen value).
- **Ledger rows** (`token_ledger`) come from `event_msg.token_count` events'
  **`last_token_usage`** field only — never `total_token_usage`, which is
  cumulative across the whole session and would double-count on every
  subsequent event if summed.

  **`cached_input_tokens` and `reasoning_output_tokens` are SUBSETS, not
  additional tokens** — verified against live evidence: `total_tokens ==
  input_tokens + output_tokens` always, with the cached/reasoning fields
  never adding to that sum (an earlier version of this tailer got this
  wrong and double-counted/overcounted; caught in review before merge).
  Derivation fed to `pricing.ComputeCost(model, inputTokens, outputTokens,
  cacheReadTokens, cacheWriteTokens)`:
  - `inputTokens` = `input_tokens - cached_input_tokens` (the uncached
    remainder, billed at the full input rate)
  - `cacheReadTokens` = `cached_input_tokens` (billed at the cheaper
    cache-read rate)
  - `outputTokens` = `output_tokens` as-is — `reasoning_output_tokens` is
    already inside this number (OpenAI bills reasoning at the output rate
    by inclusion, not by adding it again) but is also stored in its own
    `token_ledger.reasoning_output_tokens` column for raw-count fidelity
  - `cacheWriteTokens` = 0 always (no Codex/OpenAI equivalent)

  This differs from Claude Code's transcript semantics, where
  `input_tokens` already excludes cache reads — the tailer does not reuse
  `token-scraper`'s bucket-handling assumptions. A runtime sanity check
  logs loudly if `total_tokens != input_tokens + output_tokens` for a given
  event (a signal upstream semantics have drifted).

  **Known upstream quirk (openai/codex#20981):** some internal Codex
  sub-tasks (e.g. an auto-review pass) report the literal model string
  `"codex-auto-review"` in `turn_context` instead of the real underlying
  model. The tailer ignores that sentinel and keeps whichever real model
  was last seen, rather than mispricing the turn against a non-existent
  model ID.
- **message_id** (the ledger's dedup/idempotency key) is manufactured as
  `codex:<session_id>:<sequence>`, since Codex's `token_count` events carry no
  content-derived unique id the way Claude's `message.id`+`requestId` do.
  Sequence numbers are derived purely from scan order, which is why this
  survives a full re-scan (see Retention below) without double-counting.

### Schema additions (mysql migration v51 / sqlite v51, additive-only)

- `sessions`: `runtime` (default `'claude'`), `cwd`, `model`, `originator`,
  `cli_version`.
- `token_ledger`: `runtime` (default `'claude'`), `reasoning_output_tokens`.

Existing Claude Code rows are unaffected (all new columns default to
`'claude'`/empty/0). The golden-schema fixture was regenerated at
`testdata/golden_schema_v51.txt`.

### Known blind spots (v1)

- **`--ephemeral` Codex runs are invisible.** Codex skips persisting the
  rollout file entirely for `--ephemeral` sessions — there is nothing on
  disk for the tailer to read. Not chased in v1; document as a known gap.
- **Solo-only.** No Codex Agent Teams in v1 (operator decision); every
  ledger/session row carries `agent_name=""`.
- **MCP tool-call activity is parsed but not yet shipped as telemetry.**
  `event_msg.mcp_tool_call_end` (server/tool/args/duration/result, branching
  correctly on `result.Ok` vs `result.Err` — a cancelled/denied call is an
  `Err`, same event type as a success) and `response_item.function_call`
  (non-MCP tools like `exec_command`) are understood and unit-tested for
  correct parsing, but there is no wire contract yet to turn them into a
  Teamster activity-feed signal. That's a real feature gap (parity with
  Claude Code's hook-derived tool-call feed), not a correctness risk — flag
  for a future work package if per-tool-call Codex visibility is wanted
  independent of hooks.

### Retention / archive behavior (characterized live, zero model tokens)

`codex archive <session_id>` **moves** the rollout file from
`sessions/YYYY/MM/DD/rollout-*.jsonl` to a flat `archived_sessions/rollout-*.jsonl`
(same filename, no content change); `codex unarchive` moves it back,
reconstructing the `YYYY/MM/DD` path from the timestamp embedded in the
filename. The tailer's file discovery walks both trees, so an archived
session is picked up at its new location automatically. Because the
tailer's cursor is keyed by absolute path, a move loses the persisted byte
offset and forces a full re-scan of that file's content from byte 0 —
harmless: the derived `message_id`s are content-position-based, so a re-scan
reproduces the identical keys already in `token_ledger` and the unique-index
upsert makes the re-insert a no-op (verified by
`TestProcessFile_ArchiveRescanIdempotent`). `history.max_bytes` truncation is
handled the same way `token-scraper` handles file rotation: if the file on
disk is smaller than the persisted cursor offset, the cursor resets to zero.

### Design note for reviewers: why session upsert is direct-store, not HTTP

The tailer owns two responsibilities with two different write paths:
ledger rows go through hookd's `/telemetry` HTTP endpoint (reusing its
existing batching/fallback-spool/idempotent-upsert machinery, same as
`token-scraper`), while the Codex sessions row is upserted via a **direct
`store.Open(TEAMSTER_STORE_DSN)` connection** (same pattern `classify` and
`rollup` already use for work outside hookd's narrow HTTP surface). This was
an open design question in the WP3 brief (hookd's `/telemetry` never touches
`sessions` today); direct-store access for a periodic Go binary is an
established pattern in this codebase, not a new one, and avoids extending
hookd's wire contract for a low-frequency, non-batched write. Flagged here in
case a reviewer prefers a hookd-endpoint alternative instead.
