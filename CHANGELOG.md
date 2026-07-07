# Changelog

All notable changes to Teamster are documented in this file.
Format follows [Keep a Changelog](https://keepachangelog.com/).

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
