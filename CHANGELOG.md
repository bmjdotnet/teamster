# Changelog

All notable changes to Teamster are documented in this file.
Format follows [Keep a Changelog](https://keepachangelog.com/).

## v0.3.1

### Added
- **Store-backed model pricing.** Rates are now externalized configuration (`model_pricing` table) instead of compiled-in Go constants. Rate changes take effect without a recompile; each rate carries an effective-date range so history prices against the rate that was active at the time. GET `/rates` endpoint exposes the rate card to processes without a store connection.
- **Reprice tooling.** `rollup --reprice` detects rows whose stored cost no longer matches the current rate card; `--reprice-backfill` stamps historical rows that predate the rate system with their resolved rate; `--reprice-apply` corrects drift (dry-run by default, requires `--reprice-reason`). All changes journaled to `reprice_journal`. See [docs/pricing-runbook.md](docs/pricing-runbook.md).
- **Pricing Prometheus metrics** — unknown-model counter, refresh-error counter, and loaded-rates gauge, exposed via the health-collector's optional `--metrics-addr`.
- **Codex subagent visibility.** Codex subagent threads now appear in ctop with proper identity, parent linkage, and cost attribution — previously invisible because the scraper booked all subagent spend under the root session.
- **Agent roster description labels.** The `description` from Agent tool calls is captured on roster rows and displayed in ctop and the health API, giving unnamed sub-subagents a human-readable label.
- **Remote Codex context subscriber.** Remote hosts with Codex now run the context subscriber daemon, reporting thread context-window fill to the hub in real time. Previously, remote Codex sessions showed no context gauge updates until the next scraper run (up to 10 minutes).
- **Rate card Grafana panel** on the cost explorer dashboard.
- **OTel cost verification.** Every rollup pass now cross-checks per-session ledger cost against Claude Code's OTel cost counters and records a verdict, surfacing discrepancies before they compound.
- **`teamster pricing` CLI** — `list`, `add`, and `close` subcommands for managing the rate card without touching SQL directly. See [docs/pricing-runbook.md](docs/pricing-runbook.md).

### Changed
- **ctop groups agents by lineage** (parent_ref chain) instead of team name. Sessions that reuse the same team name across projects no longer merge into one group.
- **Python scrapers fetch rates from the hub** via the `/rates` endpoint instead of using hardcoded pricing tables. The embedded tables remain as a fallback when the hub is unreachable.

### Fixed
- **Codex token ingestion** (issue #29). Both Go (hub) and Python (remote) scrapers now handle the `token_usage_record` event introduced in Codex CLI 0.159.x, with persisted dedup against the legacy `event_msg:token_count`. Added pricing for gpt-6-luna, gpt-6-astra, gpt-6.1-sol and gpt-6-sol (including cache-write rates). Reads `cache_write_input_tokens` from rollout data.
- **Agent identity on respawn** (issue #27). A respawned teammate no longer inherits the dead predecessor's roster row, health gauge, and cost — it gets its own row, so gauges and cost stay with the instance that earned them.
- **Blank upserts wiping session metadata.** Codex context-subscriber heartbeats (and any re-post that omits WMS/team/focus fields) no longer overwrite populated values with blanks.
- **Teammate transcript resolution.** The health-collector now resolves teammate transcripts by agent_id instead of name + newest-mtime, which broke when auto-numbered names (`@name-2`) didn't match the sidecar's original name.
- **Haiku 4.5 pricing.** Corrected from Haiku 3.5's $0.80/$4 to the published Haiku 4.5 rate ($1/$5) — about 20% undercounted since the model launched.
- **Sonnet 5 pricing.** Updated to the permanent $2/$10 rate (the planned rise to $3/$15 was cancelled). Added entries for Sonnet 5.5, Opus 5, Opus 5.5, and Fable 5.1.
- **Sweep model selection** (issue #31). The hourly LLM sweep service now pins `--model sonnet` instead of inheriting the operator's default Claude Code model.
- **Status skill traversal** (issue #36). Status reports now recurse through nested outcome hierarchies and retrieve WorkUnits at every level, with DAG deduplication for shared descendants.
- **Codex documentation drift** (issue #37). `codex-hook.py`'s header no longer claims Codex support is hub-local Linux only, `CODEX-INSTALL.md` separates the verified 0.137.0 baseline from historical context, and both solo skills now teach agents to follow the `wms_listTags` manifest's role groups (and never set `engineManaged` keys) instead of hardcoding a tag layout with a manual `component`.
- **Codex focus-warning delivery documented** (issue #39). The Codex hook client discards hookd's response body, so focus nudges and the declined-claim follow-up warning never reach Codex agents (and no `UserPromptSubmit` hook is registered). The Codex solo skill no longer promises that warning and tells agents to call `wms_setFocus` after claiming; `CODEX-INSTALL.md` and `codex-hook.py` state the limitation and why forwarding is not implemented.
- **Missing `active` timeline interval on claim** (issue #33). `wms_claimWorkUnit` now records the pending→active transition in the timeline, so `wms_getTimeline` shows the active segment and duration reporting no longer counts active work as pending time.
- **WMS list tools returning `null`** (issue #34). `wms_getHistory`, `wms_getTimeline`, `wms_listWorkUnits`, `wms_listDeliverables`, `wms_listRelations` and `wms_listRelationKinds` now yield an empty array instead of JSON `null` when nothing matches, in both the MCP handlers and the MySQL and SQLite stores.

## v0.3.0 (2026-09-08)

### Added
- **`teamster clone`.** `teamster clone <user>@<host>` stands up a disposable Teamster instance on a remote host — same commit, copy of the same data — without touching the source. See [docs/clone.md](docs/clone.md).
- **Nightly review sweep** (`teamster wms review-sweep`, issue #11). Parks stale `review`-state WorkUnits and idle Outcomes to `on_hold`, closes delivered-but-unreviewed WorkUnits, and abandons only what it parked itself after a second grace period. Dry-run by default; `--confirm` on the command line is the only way to execute for real. Off until enabled — drain your existing backlog by hand first (see the quickstart burn-in section).
- **Sweep Report dashboard** — what the review sweep parked, closed, and is still waiting on a human for.
- **Outcome decomposition.** Outcomes can now be nested under parent Outcomes, so sessions can record their work as part of a larger deliverable.
- **True cost rollup.** Parent Outcomes now include the cost of all descendants, not just direct costs.
- **Rework tracking.** Typed relations record why post-delivery work exists (bug escape, design gap, revert), enabling prevention-lever reporting.
- **Claim system.** Mechanized work assignment and delivery as core WMS functionality (`wms_claimWorkUnit`, `wms_deliverResult`, `wms_listDeliverables`). Cost attribution for dispatched work is now automatic.
- **Dispatch feedback.** Protocol violations now reach the agent that caused them, not just the activity log.
- **MCP tool-call ledger** (`mcp-scraper`) — tails hookd's event log into an `mcp_tool_calls` table for per-tool usage reporting. Off until enabled in `teamster.yaml`.
- **Model x Phase Cost Matrix** on the Usage & Effectiveness dashboard, for spotting model-fit issues by pipeline stage.
- **Work-type vocabulary consolidation.** 18 values → 11. Removed duplicates and ambiguous categories; added `polish` for post-delivery refinements.
- Status changes can carry a reason (`notes`) that lands in the journal; CLI closes record host and command automatically.
- `wms_createOutcome` / `wms_createWorkUnit` accept multiple values per tag key inline.

### Changed
- **otelcol, prometheus and grafana now run under their own systemd units** and survive a reboot. Supervisor mode is unchanged.
- **Outcomes no longer auto-close** when their last WorkUnit or child Outcome finishes. Closing an Outcome is a deliberate step in the session close-out.
- **New statuses `on_hold` and `abandoned`.** Written-off work no longer counts as `done`. `done → review` is the single reopen edge, and reopening clears the entity's `resolution` tag.
- **Focus intervals no longer close at every turn boundary.** They close on handoff, on terminal status, or when a session goes stale (`TEAMSTER_GC_STALE_HOURS`, default 2). An idle teammate whose lead is still connected is never reaped.
- `wms gc --confirm` now requires typing `yes` at a terminal and refuses to run non-interactively; the ignored `--dry-run` flag is gone. gc also closes the intervals of what it abandons.
- `teamster wms close` validates the transition before writing and names the remedy when it refuses.
- Creating a WorkUnit under a `done` or `abandoned` Outcome is rejected.
- Pre-delivery correction phase renamed from `rework` to `iterate`. "Rework" now refers exclusively to post-delivery relation tracking.
- Phase tags and interval phase columns are now kept in sync automatically.
- `wms_setPhase` enforces a closed vocabulary (design, build, test, review, iterate, admin).
- Upgrades now always ship the current `interceptors.yaml` instead of preserving operator customizations. The prior version is backed up as `interceptors-<version>.yaml.bak`.
- `teamster status` shows abandoned and sweep-parked (`on_hold`) counts.
- `ctop` activity log now shows newest messages at the top (issue #21).
- Schema migrations on upgrade seed new rows only, no table alters. Older binaries refuse the upgraded database, so upgrade every host that shares it.

### Fixed
- Fixed the hook event log never rotating; the relay and feed viewer now survive rotation.
- Fixed hook events being attributed by a truncated session id, which made repaired intervals permanently unallocatable.
- Fixed hookd not being enabled at install, so it did not survive a reboot.
- Fixed the installer unmasking systemd units on upgrade, which silently restarted the paid sweep on clone targets.
- Fixed `teamster stop` killing systemd-managed components behind systemd's back.
- Fixed `wms_claimWorkUnit` never opening a focus interval, so claimed work went unattributed.
- Fixed the close-out required-tags check ignoring tags inherited from the parent Outcome, so it warned on nearly every close.
- Fixed a tag manifest bug that returned inconsistent metadata depending on database row ordering.
- Fixed a cardinality bug allowing multiple work-types to be assigned to a single entity.
- Fixed a bug where ~11% of teammate cost was silently unattributed due to long agent names overflowing an internal limit.
- Fixed a race condition where concurrent MCP calls could mis-attribute one agent's identity to another.
- Fixed a bug where bundled scout/reviewer agents could not attribute their own cost.
- Fixed a bug where the fleet view showed agents under the wrong parent in the hierarchy.
- Fixed a bug where ghost agents lingered indefinitely in the fleet view and activity log.
- Fixed a bug where agent names in ctop and fleet view didn't use consistent colors (issue #20).
- Fixed journal history returning same-second rows in arbitrary order.
- MCP tools now report what they actually did (`wms_claimWorkUnit` focus status, `wms_setPhase` errors, redelivery while in review) instead of silently succeeding.
- Silent attribution failures and failed hookd notifications are now logged.
- Documentation overhaul: status vocabulary, review sweep, supervisor units, clone masking, and burn-in recipes.

## v0.2.6 (2026-08-13)

### Added
- Added top-n limiter control to AI Spend Trace dashboard (reduces Sankey diagram complexity)
- **MCP tool interceptor registry** — externalized `mcp__*` tool enrichment (tag, display text, suppress) from hardcoded Go into a YAML config (`interceptors.yaml`), operator-editable without recompiling
- `teamster check-config` CLI verb — validates an edited `interceptors.yaml` without restarting hookd
- Remote subagent health & token usage now visible in fleet view
- Sweep skill prescribes `work-type:processor` for rote data-processing runs

### Fixed
- Hardened WMS tag handling
- `registerPeer` auto-populates `session_id` from MCP call metadata when the caller omits it
- Fleet-view dashboard column alignment
- Sub-subagent spawn detection & visualization in activity log

### Changed
- Hardened test harness to limit parallel store tests that degrade performance
- Activity log upgraded to realtime fleet view

## v0.2.5 (2026-07-16)

### Added
- `wms_renameOutcome`/`wms_renameWorkUnit` MCP tools — rename an outcome or work unit's title directly, without state-machine validation
- **Fleet Dashboard** (`ctop`) — a single terminal dashboard showing the full agent hierarchy per session: subagents and sub-subagents, their models, cost, activity, and context pressure
- **Live model tracking** — activity logs now reflect the model you're actually using, even after a mid-session `/model` switch
- **Recap tagging** — session recaps and suggested next steps are now tagged distinctly instead of showing up as false "done" entries

### Fixed
- Cost figures are now accurate for extended (1-hour) prompt caching
- hookd now retries if MySQL isn't ready yet at boot instead of failing outright
- `token-scraper` and `health-collector` start, stop, and report status correctly on systemd-managed installs
- Fixed a replication compatibility issue affecting MySQL/MariaDB
- Team name now shows up correctly across dashboards
- Added rollup & classify guards to prevent continuous rescanning of historical wms data

### Changed
- Installer skips the tag-setup wizard on upgrades (already configured)
- Golden schema fixture regenerated through migration v61

### Known limitations
- Fleet dashboard & health monitoring only support Claude Code runtime (Codex integration pending)
- Teammates can briefly appear "closed" between turns

## v0.2.4 (2026-07-08)

### Added
- Codex agent support — full WMS, telemetry, and cost attribution for OpenAI Codex sessions alongside Claude Code (identity resolution, MCP server configuration, hooks channel, skill porting, OTEL metrics, model pricing)
- Codex remote client support — capture parity for remotes running Codex (Python rollout tailer, hookd session endpoint, config patcher, `--codex-mode` on install paths)
- Codex scraper — systemd-managed JSONL tailer for Codex rollout token/session capture
- Codex-specific Grafana dashboard (`codex-metrics.json`) with dedicated OTEL receiver
- `$start` front-door skill for Codex — thin pointer to `teamster-solo`, ambient and visible
- `wms_getEntityTags` MCP tool — read-only view of direct and inherited entity tags
- Brief artifact existence check in bootstrap/start/solo skills — flags missing referenced files before acting
- LXD cleanroom test harness (`cleanroom.sh`) with Codex install matrix support
- OpenAI/Codex model pricing entries (gpt-5.5, gpt-5.4, gpt-5.3-codex, o3, o4-mini)

### Changed
- Runtime enum renamed `claude` → `claude_code` (enum: `claude_code`, `codex`, `unknown`)
- `otelcol-contrib` pinned to 0.156.0 (was 0.95.0) with `deltatocumulative` processor
- AGENTS.md deferred-tool search guidance reconciled with actual Codex behavior

### Fixed
- Token-bucket double-count — `cached_input`/`reasoning_output` are subsets, not additive
- Silent `$0` fallback for unknown pricing models replaced with loud warning
- `JournalObserver.OnStatusChange` dropping `SessionID`/`AgentName`/`Host` from audit trail
- Rollup `--reallocate` attribution race — sweep clears by `entity_type=''` instead of `method='unallocated'`
- Install runner wire guard — systemd hookd-stop gated on `$WIRE` + basedir ownership check, preventing stage-only runs from stopping live services
- `PostToolUse` object `tool_response` 400 error in hook payloads
- Non-interactive hook environment resolution
- Cron `pipefail` on single-line crontab
- Cleanroom hookd health-check race condition (retry loop replaces fixed sleep)

## v0.2.3 (2026-07-07)

### Added
- Backend-neutral persistence API: role-based sub-interfaces, `store.Open` registry, typed error sentinels, portable migration framework
- SQLite validation backend (`modernc.org/sqlite`) proving zero-callsite backend swap
- 6-dimension conformance suite (CRUD, atomicity, concurrency, sentinels, migrations, cross-backend equivalence)

### Removed
- `DB() *sql.DB` escape hatch — all persistence goes through the store interface

### Fixed
- `TokenLedgerRows` silent zero-row return (DATETIME scan mismatch)
- `OpenFocusInterval`/`WriteFocusInterval` concurrency race (advisory lock)
- `CloseSessionIntervals` raw error leak (now `ErrConflict`)
- Rollup `TRUNCATE`-in-tx non-atomicity (replaced by `AtomicReplace`)
- Fixed various code test units

## v0.2.2 (2026-07-06)

### Added
- `teamster search sessions` + `wms_search` MCP tool — find sessions/entities by what they worked on, across hosts and operators (WMS-backed)

### Fixed
- Fixed a classifier bug causing old intervals to not be processed
- Fixed an install.sh bug that caused display of 'localhost' instead of hostname

### Changed
- Changed default prometheus data retention to 365d

## v0.2.1 (2026-06-28)

### Fixed
- Addressed various tagging bugs introduced in v0.2.0
- Installer now prompts for `backup_dir` and schedule; backup service degrades gracefully when unconfigured instead of crashing
- Fixed a bug causing rollup crashes
- Fixed a bug causing unnecessary agent focus nudges
- Fixed a bug causing inflated cost displays
- Fixed a bug causing negative-duration intervals

## v0.2.0 (2026-06-24)

### Added
- macOS remote client support — enroll a Mac with `teamster install-remote user@mac` from the hub (full activity, telemetry, and work management participation; launchd-based token scraping)
- `teamster status` command with interactive terminal dashboard showing session overview, service health, and cost breakdown
- Backup and restore engine (`teamster backup`, `teamster restore`) with configurable retention and scheduling via systemd timer
- Tag conventions system — define scope, exclusion groups, and auto-extraction rules per tag key through the database, YAML config, or the TUI wizard
- Compact tag manifest endpoint (`wms_listTags`) with role-based grouping and keyword search, replacing the full dictionary dump
- Outcome keyword search (`wms_listOutcomes`) — session startup finds and offers to resume matching open outcomes
- Automatic relay detection in the installer
- Named agent role definitions (`@scout`, `@implementer`, `@reviewer`) shipped in the plugin
- Cost attribution recovery for remote teammates using dispatch brief analysis
- Cost attribution recovery for untracked remote sessions using temporal correlation with concurrent focused sessions
- Transcript caching and killswitch for resilient remote token tracking
- `TEAMSTER_DEBUG_RAW` opt-in diagnostic mode for remote hook payload inspection
- Pre-upgrade backup before replacing binaries on reinstall

### Changed
- Hub installer requires Linux — macOS hosts are enrolled as remote clients via `install-remote`
- Bundled Grafana upgraded to v13.0.2 with Pathfinder learning plugin
- Dashboard navigation redesigned with cross-dashboard tabs and a welcome landing page

### Fixed
- Cost attribution errors from concurrent session writers — ordering-safe close prevents inverted intervals; one-time repair corrects historical inversions (reversible)
- Crash when draining duplicate open focus intervals
- 400 errors from oversized hook payloads (field stripping and size cap applied)
- Unnecessary sweep processing when no orphan sessions exist
- Cross-panel color inconsistency in Grafana dashboards
- Go version gate rejecting valid Go installations
- Various activity feed and session display issues

## v0.1.0 (2026-06-16)

Initial release.
