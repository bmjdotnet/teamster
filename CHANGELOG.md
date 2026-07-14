# Changelog

All notable changes to Teamster are documented in this file.
Format follows [Keep a Changelog](https://keepachangelog.com/).

## v0.2.5 (unreleased)

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
