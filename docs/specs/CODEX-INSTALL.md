# Codex Support — Install & Operate

**Status: partial.** WP3 (codex-scraper) and the uninstall recipe are complete
below. WP7 still owns assembling the rest of the installer-integration guide
(MCP server wiring detail, hooks trust provisioning, skills plugin, residual
audit-risk note) — merge that content in here rather than starting a second doc.

## Uninstall

No automated `teamster uninstall` exists yet (same posture as the Claude Code
side — see `REMOTE-INSTALL.md`'s Uninstall section). This is the manual
recipe, in that document's style.

Teamster writes into `~/.codex/config.toml` using marker-bounded sections
(`# >>> teamster:<name> >>>` ... `# <<< teamster:<name> <<<`) so its content
can be stripped without disturbing anything else in the file — an operator's
own comments, `mcp_servers` entries, or `[projects]` trust settings are never
touched by any of this.

```bash
CODEX_CONFIG=~/.codex/config.toml   # or $CODEX_HOME/config.toml if CODEX_HOME is set

# Remove exactly the tables Teamster wrote — safe even if the operator has
# made unrelated edits to config.toml since install. Repeatable; a name with
# no matching span is a no-op.
for name in mcp_servers.activity mcp_servers.wms otel hooks hooks-state; do
  sed -i "/# >>> teamster:${name} >>>/,/# <<< teamster:${name} <<</d" "$CODEX_CONFIG"
done

# Skills Teamster installed via file-copy (not the plugin system — see
# InstallSkills's doc comment for why):
rm -rf ~/.codex/skills/teamster-solo ~/.codex/skills/teamster-status \
       ~/.codex/skills/teamster-tags ~/.codex/skills/teamster-review

# codex-scraper systemd timer/service:
sudo systemctl disable --now teamster-codex-scraper.timer 2>/dev/null || true
sudo rm -f /etc/systemd/system/teamster-codex-scraper.{service,timer}
sudo systemctl daemon-reload

rm -f ~/teamster/var/codex-scraper-cursors.json   # tailer's byte-offset cursor
```

**AGENTS.md protocol text** (`mergeCodexAgentsMD`) is a plain content append
keyed by a heading marker, not a removable marker pair like config.toml's
sections above (same posture as `mergeClaudeMD`/CLAUDE.md on the Claude Code
side — REMOTE-INSTALL.md's Uninstall section is equally hand-wavy about it).
Two options, in order of safety:
1. **Restore from backup** (cleanest): every write Teamster makes to
   `AGENTS.md`/`AGENTS.override.md` is preceded by `installbackup.Backup` —
   `<file>.pre-teamster` holds the exact pre-Teamster content from the very
   first install. `cp ~/.codex/AGENTS.md.pre-teamster ~/.codex/AGENTS.md`
   (or the `.override.md` variant, whichever `mergeCodexAgentsMD` actually
   targeted — check which file contains the `## Getting Started with
   Teamster (Codex)` heading). This reverts anything else appended to the
   file after install too, so only use it if nothing else has touched the
   file since.
2. **Manual excision**: the Teamster block runs from the
   `## Getting Started with Teamster (Codex)` heading to end-of-file (it's
   always appended last) — delete from that heading onward if there's
   trailing operator content you need to keep that was added after install.

**Backups left behind** (all `installbackup`-managed, safe to leave or clean
up as the operator prefers): `~/.codex/config.toml.pre-teamster` +
`~/.codex/config.toml.<timestamp>.bak` per run;
`~/.codex/AGENTS.md.pre-teamster` (or `.override.md`) + timestamped backups
the same way. The Claude Code side has the equivalent
`~/.claude/settings.json.pre-teamster`, `~/.claude.json.pre-teamster`,
`~/.claude/CLAUDE.md.pre-teamster` (retrofitted in WP2 — these did not exist
before Codex support shipped).

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
- **No fresh-vs-resume discriminator exists in rollout JSONL, and the tailer
  doesn't need one.** `session_meta` does carry a `source` field (e.g.
  `"exec"` — the entry-surface the session was launched through), but there
  is exactly ONE `session_meta` record per file even after `codex exec
  resume` appends a second turn — the plan's "discriminate via `source`
  (startup/resume)" language describes the separate **hook** `SessionStart`
  stdin payload (a WP8 concern), not anything present in rollout JSONL
  content. Verified live (settled in the kit's `verification-round3.md`
  Addendum 2). The tailer's scan-order sequence-number design (above) needs
  no such discriminator regardless — every `token_count` event is ledgered
  independently of whether its turn came from the original launch or a
  later resume.

### Schema additions (mysql migration v51 / sqlite v51, additive-only)

- `sessions`: `runtime` (default `'claude_code'`), `cwd`, `model`, `originator`,
  `cli_version`.
- `token_ledger`: `runtime` (default `'claude_code'`), `reasoning_output_tokens`.

The `runtime` enum is `{claude_code, codex, unknown}` (identity-mapped to
OTEL `source` labels). Existing Claude Code rows are unaffected (all new
columns default to `'claude_code'`/empty/0). The golden-schema fixture was
regenerated at `testdata/golden_schema_v51.txt`.

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
hookd's wire contract for a low-frequency, non-batched write.

**Decided (lead, 2026-07-07): keep direct-store for v1.** The precedent
(classify/rollup) is exactly right, and v1 is hub-local by scope — no
rework needed. **Migration path for later:** `codex-scraper` only makes
sense running on the same host as the `codex` CLI it tails, which for a
Codex **remote** (by analogy with Claude Code's hub/remote model) would be
a machine that cannot reach the hub's `TEAMSTER_STORE_DSN` directly. When
Codex remote support lands ([later], not in this kit's v1 scope), the
sessions-row upsert must move behind a hookd HTTP endpoint (the
hookd-endpoint alternative flagged above) so a remote-side scraper can
reach it the same way remote Claude Code hook clients already reach hookd
over HTTP — the direct-store path only works hub-local.
