# CLAUDE.md — Teamster

Guidance for Claude Code sessions working in this repo. Read this first.

## What Teamster is

A Claude Code Agent Teams overlay providing three things:

1. **Observability** — real-time activity stream (`feed`, web dashboard)
   showing what every agent is doing, thinking, completing.
2. **Workflow enforcement** — the Eight Rules + slash commands that teach
   the lead how to decompose work, name agents, route by affinity, verify
   autonomously.
3. **Work management** — Outcome → WorkUnit hierarchy in MySQL/MariaDB,
   exposed via the `wms` MCP server. Scheduled cost-attribution sweep
   recovers unallocated spend from session transcripts.

Go module: `github.com/bmjdotnet/teamster`. MySQL/MariaDB via `go-sql-driver/mysql`.
Single-binary distribution, installs to `~/teamster/`.

## Three Teamsters — don't confuse them

Teamster is often used to develop Teamster: the session editing this repo may
itself be hooked into a running Teamster instance while it edits the repo that
produces the next one. Always be clear which Teamster you mean.

| Name | What | Where |
|------|------|-------|
| **the repo** | This source tree. Editing here changes future installs. | your checkout of the repo |
| **the live instance** | The Teamster your current Claude session is hooked into, if any. Already-deployed binaries, JSONL, WMS DB. | `~/teamster/` (BASEDIR), `~/.claude/settings.json` |
| **a test instance** | A clean install used to validate changes — on a disposable test VM, or a throwaway BASEDIR. | a test VM, or another BASEDIR you passed to `lib/installrunner.sh --basedir=...` |

Rules of thumb:

- Editing `src/` does **nothing** to the live instance until you run
  `./install.sh` and the live `hookd` is restarted. Old binaries keep running.
- Running `./install.sh` in this repo will **replace your live instance**.
  It interviews you, builds the right flags, then calls `lib/installrunner.sh`
  which compiles, stages, and restarts.
- `lib/installrunner.sh --basedir=PATH` (no `--wire`) **stages** binaries and
  skel into PATH and does not touch `~/.claude/settings.json` or MCP
  registration. `--wire` is the intended sole gate for global-state mutation
  — only use it on a disposable test VM or when you explicitly intend to
  replace the live config. **Incident history (2026-07-07,
  installrunner-wire-guard WU):** this same claim, worded unconditionally
  ("safe, no global state touched"), was wrong in practice — two systemd
  hookd-stop call sites keyed off the fixed `teamster-hookd` unit name with
  no `--wire` gate (one) or no basedir check (both), so a `--basedir=`-only
  run on a host with a live `teamster-hookd` running under a *different*
  basedir stopped it anyway. Both sites are now gated on `$WIRE -eq 1` AND on
  the currently-installed unit's own `ExecStart=` basedir matching this run's
  `$BASEDIR` (`hookd_unit_matches_basedir` in `lib/installrunner.sh`) — a
  `--basedir=PATH` run without `--wire` is now genuinely a no-op against any
  running service, verified live in an isolated container against an
  unrelated systemd unit. Trust the code path, not just this sentence, if
  you're ever unsure — the whole reason this note now has this much detail.
- For true isolation, use a **disposable test VM**: reset it, then run a full
  `lib/installrunner.sh --wire` there, never touching your dev host's systemd
  or settings.
- Never test destructive changes against the live instance. Use a test VM so a
  broken installer or hook client can't take down the dev session you're
  sitting in.
- When in doubt, ask: "is this command going to touch `~/teamster/`?"
- Activity stream events you generate while developing are visible in the
  same stream you may be reading from. Filter aggressively or use a
  separate session salt to avoid feedback loops.

## Repo layout

```
src/                          Go source (go.mod: github.com/bmjdotnet/teamster)
  cmd/                        One subdir per binary
    teamster/                 Hook client + CLI (Go) — reads hook JSON from stdin; also
                              supervisor subcommands (start/stop/status/wms-reset/tags/setup)
    hookd/                    HTTP event server + dashboard
    feed/                     Terminal activity viewer (replaces gatail)
    ctop/                     Muster: terminal fleet dashboard (Bubbletea). Single view
                              (fleet_view.go): multi-team tree, hierarchy, activity log.
    activity-mcp/             MCP stdio: reportActivity/setOverallIntent/completeActivity/setMode
    wms-mcp/                  MCP stdio: outcome/workunit CRUD, tags, focus, dependencies, search
    rollup/                   Cost-attribution pipeline (allocate, recover, sweep)
    classify/                 Interval phase + work-type classifier (systemd timer)
    health-collector/         Muster: per-agent health gauge collector (polls token_ledger,
                              writes agent_health_gauge). Hub daemon, 15s poll interval.
                              teammate_context.go: transcript-derived context occupancy for
                              Agent-Teams teammates (statusLine never fires for them — reads
                              each teammate's own transcript JSONL + .meta.json sidecar under
                              subagents/agent-<id>.*); cost_test.go covers per-agent cost
                              (costForRows, component token columns) and session_total_cost_usd.
    token-scraper/            Session transcript token-usage scraper (Claude Code)
    codex-scraper/            Codex rollout-JSONL cost/ledger tailer (systemd timer, oneshot)
    mcp-scraper/              MCP tool-call tailer: events.jsonl → claude_telemetry.mcp_tool_calls
                              (systemd timer, oneshot)
    teamster-install/         Installer binary (called by lib/installrunner.sh)
    demogen/                  Synthetic data generator for dashboards
    backup/                   Standalone backup binary (systemd timer)
  internal/
    activity/                 Shared activity-tool handler logic
    classify/                 Rule-based classifier engine (work-type, phase)
    clone/                    Leg 1 (ref resolution) + ship half of Leg 2 for
                              `teamster clone`: SSH transport (script-based
                              scp+run, absolute-$HOME target probing so a
                              literal ~ never reaches a remote command),
                              content-manifest fingerprint provenance gate
                              (git ls-tree vs. git hash-object), Stage A
                              post-install verification
    clonedata/                Leg 3 (data) pure logic for `teamster clone`:
                              transfer-candidate filtering (I1: never ships
                              config.tar.gz), post-restore row-count diff,
                              clone_verify_ro (I7) DSN construction
    clonetopology/            Pure-function translator: source teamster.yaml
                              → lib/installrunner.sh flag vector for a clone
                              target (instance identity travels, host
                              topology is re-derived; refuses a --basedir
                              under configured forbidden prefixes — I5)
    config/                   Env-var config (TEAMSTER_* namespace)
    codexconfig/              Codex CLI config wiring: ~/.codex/config.toml MCP
                              servers, OTEL, hooks + trust state, skills file-copy,
                              AGENTS.md merge — all backup-then-doctor-gated
    display/                  Entity colors, tag colors, ANSI rendering
    hook/                     Tool extraction, tag taxonomy, enrichment
    intercept/                YAML-driven MCP tool interceptor registry:
                              loads/compiles/matches interceptors.yaml (tag +
                              display + suppress rules per mcp__* tool name);
                              EnrichRecord checks it before the hardcoded
                              mcp__activity__/mcp__wms__/generic-MCP fallback
    llm/                      Anthropic API client (used by sweep-llm)
    logging/                  Structured slog setup (TEAMSTER_LOG_LEVEL)
    mcp/                      Shared MCP JSON-RPC plumbing
      activity/               Activity MCP tool handlers
      health/                 Muster: health MCP tool handlers (health_listAgents,
                              health_getAgentSnapshot, health_getTeamSummary,
                              health_getPressureAlerts)
      roster/                 Muster: roster MCP tool handlers (roster_listAgents,
                              roster_getAgent, roster_resolveId, registerPeer,
                              verifyToken, roster_bindSession, getRosterEntry) +
                              liveness derivation (computed from last_seen, never stored)
      wms/                    WMS MCP tool handlers
    observability/            Prometheus collectors (attribution, cost, entities, sessions, sweep)
    pricing/                  Rate cards: embedded fallback tables (Known/classRates via
                              ComputeCost) + a store-backed Resolver (resolver.go) over the
                              model_pricing table — cached snapshot, loud unknown-model WARN
                              + counter, embedded fallback when no store has loaded — plus
                              an HTTP RateSource client (httpclient.go, NewHTTPSource) that
                              reads hookd's GET /rates for processes with no store
                              connection. Every Go scraper prices via Resolver; the embedded
                              tables are the fallback path (with a WARN) when the store or
                              hookd is unreachable. Each priced token_ledger row is stamped
                              with token_ledger.rate_id (the model_pricing row that priced
                              it, via /telemetry; migration v75). Fallback-priced rows send
                              the wire value rate_id=-1 (store.RateIDEmbeddedFallback),
                              which hookd swaps for the runtime's zero-rate sentinel row
                              (model_key embedded-fallback-v1, migration v76); sentinel rows
                              are excluded from rollup --reprice drift detection
    redact/                   Credential redaction for activity feed
    reconciler/               OTel cost reconciler: compares per-session token_ledger spend
                              against Claude Code's own OTel cost counters (read-only;
                              deliberately does not import internal/pricing). storeadapter/
                              binds it to the store; run by cmd/rollup/reconcile.go
    render/                   Display-string rendering helpers
    rollup/                   Cost allocation + recovery passes (gap, synthesize, sweep-llm)
    roster/                   Muster: shared roster utilities — GenerateRosterID (UUID v4),
                              MintToken, VerifyToken, RegisterPeer (token lifecycle)
    agenthealth/              Muster: agent-health concern subtree (R2 private storage)
      gauge/                  GaugeStore interface (overwrite semantics, not append-only)
        mysql/                MySQL GaugeStore implementation (agent_health_gauge table)
    server/                   HTTP receiver + JSONL writer + focus nudge + roster
                              auto-registration + turn-state tracker + /mcp/roster +
                              /mcp/health endpoints
    store/                    Backend-neutral Store interface (store.go: role-based
                              sub-interfaces — SessionStore, IntervalStore,
                              MaintenanceStore, AllocationStore, RecoveryStore,
                              RosterStore, PricingStore, ReconciliationStore, etc.),
                              typed errors (errors.go: ErrNotFound/ErrConflict/
                              ErrPrecondition), backend registry + store.Open
                              (factory.go), portable migration framework (migrate.go),
                              conformance suite (conformance_dim1-4_test.go, dim6_test.go,
                              conformance_dim7_test.go — roster/token ops,
                              conformance_dim8_test.go — model_pricing rate card,
                              conformance_dim9_test.go — reconciliation aggregates)
      mysql/                  MySQL/MariaDB backend + migrations (v1–v73); search.go:
                              SQL behind wms.Search/SearchSessions
      sqlite/                 SQLite backend (modernc.org/sqlite) — conformance
                              validation only, not an install-time option
      storetest/              Shared MySQL test harness (per-test schema isolation,
                              RawExecutor fixture helpers) for packages needing a
                              real store.Store
    transcript/               Session transcript reader (focus timeline, window)
    tui/                      Bubbletea TUI (tag setup wizard + editor)
    teamsteryaml/             Shared teamster.yaml schema, extracted from
                              cmd/teamster-install/yaml_config.go so
                              teamster-install and internal/clonetopology
                              compile against the same real types instead of
                              a hand-duplicated, drift-prone copy
    backup/                   Backup engine (config, drivers, manifest, retention, flock)
    version/                  Build-time version info
    web/                      SSE dashboard + WMS hierarchy + cost-flow + tag browser
    wms/                      WMS engine (Outcome/WorkUnit), state machines, HookObserver,
                              Search/SearchSessions (session discovery)

skel/                         Assets copied to BASEDIR at install time
  doc/specs/                  architecture.md, wms-dashboard-spec.md, semantic-conventions.md
  etc/
    interceptors.yaml                MCP tool interceptor config: tag colors
                              + per-mcp__*-tool display/suppress rules.
                              Embedded default (also compiled into
                              src/internal/intercept/) + this file as the
                              installed, operator-editable overlay.
                              `teamster check-config` validates an edit
                              without restarting hookd.
    teamster-hookd.service.tmpl      Systemd unit template (uses __BASEDIR__)
    teamster-rollup.service.tmpl     Rollup one-shot service
    teamster-rollup.timer.tmpl       Rollup timer
    teamster-classify.service.tmpl   Classifier one-shot service
    teamster-classify.timer.tmpl     Classifier timer (every 10 min)
    teamster-sweep.service.tmpl      Hourly sweep one-shot (rollup --sweep + claude --print)
    teamster-sweep.timer.tmpl        Sweep timer (every hour)
    teamster-backup.service.tmpl     Backup one-shot service
    teamster-backup.timer.tmpl       Backup timer (configurable, default 1h)
    teamster-codex-scraper.service.tmpl  Codex rollout-tailer one-shot service
    teamster-codex-scraper.timer.tmpl    Codex-scraper timer (every 10 min)
    teamster-codex-context-subscriber.service.tmpl  Codex context-gauge daemon (Type=simple)
    teamster-mcp-scraper.service.tmpl    MCP tool-call tailer one-shot service
    teamster-mcp-scraper.timer.tmpl      MCP-scraper timer (every 10 min)
    teamster-relay.service.tmpl      Event relay (hub→replica), --relay-mode=install
    teamster-repl-push.service.tmpl  Repl-push MySQL sync server (hub side)
    teamster-prometheus-replica.yml.tmpl  Replica Prometheus scrape config
    grafana-anonymous.ini            Replica Grafana anonymous-access config
    grafana/                         Provisioned dashboards + datasource configs
  lib/
    .claude-plugin/marketplace.json  Plugin marketplace root (NOTE: above plugin/)
    plugin/                   Claude Code plugin (skills/ only; marketplace.json is in .claude-plugin/ above)
      skills/{bootstrap,start,solo,plan,review,status,tags,sweep,seasoning}/SKILL.md
      skills/bootstrap/references/
        startup-min/MANIFEST.md          loads at interview time (deliberately empty
                                          otherwise — interview needs no protocol docs)
        dispatch-pack/{eight-rules.md, execution-loop.md, field-guide.md,
                       muster-guide.md, rubrics.md, decomposition-guidance.md,
                       MANIFEST.md}      loads once team mode is confirmed, lead-only
        implementation-pack/{teammate-guide.md, MANIFEST.md}
                                          loads into teammate briefs only, never the
                                          lead's own context — distilled Eight Rules
                                          subset (IV, VI, VIII) for hands-on work
      skills/shared/session-protocol.md  canonical intake mechanics (interview flow,
                                          mode selection, outcome creation, tag
                                          application), shared by start/bootstrap/solo
      skills/tags/references/
    hook/teamster.py          Python hook client used on remote installs
    hook/codex-hook.py        Python hook client for Codex (imports teamster.py's
                              redaction/error helpers; the two ship together)
    codex-plugin/             skills/ (file-copy installed, hub and remote alike) +
                              agents-protocol.md (the AGENTS.md merge text, single-
                              sourced — both the Go installer's loadCodexAgentsProtocol
                              and the Python remote-codex-setup.py read this same file,
                              never two copies); plugin-shaped assets also shipped here
                              as a documented fallback (v1 installs skills by file-copy,
                              not the Codex plugin system)
    .agents/                  Codex agents/ assets (openai.yaml) for that fallback
    scripts/                  selftest, remote-setup, session-explorer, wms-smoketest,
                              install-remote.sh, token-scraper.py, ccusage-scraper.py,
                              codex-scraper.py (Python rollout tailer, remote/client-mode),
                              codex-context-subscriber.py (Codex context-gauge daemon, hub only),
                              remote-codex-setup.py (Python codexconfig port, remote/client-mode),
                              test_codex_scraper.py, test_remote_codex_setup.py

install.sh                    Interactive installer entrypoint (guided, interview-driven)
lib/installrunner.sh          Build/install backend (called by install.sh)
docs/                         User-facing docs (specs + guides)
docs/specs/REMOTE-INSTALL.md  Current spec for hub/remote model
scripts/                      Dev/ops scripts: test-with-mysql.sh (disposable
                              test MySQL for internal/store conformance),
                              clone-acceptance-test.sh (`teamster clone`
                              acceptance harness: revert → clone → verify →
                              assert-no-source-writes → revert)
build/                        Compiled binaries (gitignored)
README.md                     User-facing quick start
```

## Build and install

`./install.sh` is the only supported entry point for installing Teamster.
Do not run `go build` ad-hoc and copy binaries around — the installer is
the contract between source and a working install.

`./install.sh` is the interactive installer (formerly `wizard.sh`). It
probes the host, interviews the operator on service mode decisions, builds
the right command line, then calls `lib/installrunner.sh` with the
assembled flags. `install.sh` itself only accepts `--debug-log` and
`--help` — all other flags belong to `lib/installrunner.sh`.

```bash
# Guided interactive install (recommended):
./install.sh                                      # interviews, then calls lib/installrunner.sh
./install.sh --debug-log=/tmp/install.log         # guided install with debug logging

# Direct backend invocation (advanced / scripted installs):
lib/installrunner.sh --basedir=PATH                       # stage to PATH only — no --wire, no global-state mutation (see "Three Teamsters" above for the incident this now-corrected claim caused)
lib/installrunner.sh --basedir=PATH --wire                # stage + wire to PATH (dangerous: touches systemd + settings.json)
lib/installrunner.sh --relay-mode=install --relay-target=http://replica:9125/event --repl-push-remote=user@replica  # hub: set up replication
```

`lib/installrunner.sh` is the build/install backend. It compiles Go
binaries to `build/`, then runs the installer to copy them into
`BASEDIR/bin/`, materialize systemd units and timers, merge
`~/.claude/settings.json` (hooks, env, MCPs, permissions,
`enabledPlugins`), write/merge `~/.claude/CLAUDE.md` global protocol, and
register the plugin. Idempotent. Detects a running `hookd` (systemd or
pgrep) and restarts it.

`--store-engine=mariadb|mysql-8.4` (only valid with `--store-mode=install`;
default `mariadb`, today's `default-mysql-server` behavior, unchanged for
every existing caller) selects the store package. `mysql-8.4` provisions
genuine MySQL 8.4 LTS via Oracle's apt repo (`install_mysql_84`) instead of
Debian's default MariaDB — `teamster clone` always passes this, since a
clone's fidelity requirement (matching the source engine's collation,
`utf8mb4_0900_ai_ci`) is not a concern ordinary installs have. See
`skel/doc/specs/architecture.md`'s "MySQL 8.4 provisioning" section.

`--relay-mode=install` (default `none`) builds `relay` and installs the
relay + repl-push services on a hub, pushing events and MySQL data to a
read-only replica. Requires `--relay-target` and `--repl-push-remote`. On the
replica side, `--hookd-read-only` materializes `TEAMSTER_HOOKD_READ_ONLY=1`
into the hookd unit so hookd rejects MCP/telemetry/drain while still serving
reads + `/event`. See `docs/specs/replication.md`.

**Client mode** is for remote hosts: stages the Python hook client + the
plugin, points settings.json at a hub URL, and (unconditionally, auto-detect
by default) stages + wires Codex support the same way `teamster install-remote`
does — see "Hub vs remote install model" and `docs/specs/CODEX-INSTALL.md`'s
Remote Codex support section. No Go required on the remote.

`teamster install-remote user@host [--server hub:9125]` is the hub-side
command that drives the client install over SSH. It execs the shell script
at `$BASEDIR/lib/scripts/install-remote.sh`, passing all args through.
During development, the script can also be run directly from
`skel/lib/scripts/install-remote.sh`.

**Post-install:** run `teamster setup tags` to configure the tag keyspace
via the TUI wizard (8-screen guided flow on first run, 3-column editor
on subsequent runs). Or use `teamster tags add-key`/`add-value` for
non-interactive setup. `lib/installrunner.sh` only auto-launches this wizard
on a **fresh** install (captured as `IS_UPGRADE` before the binary swap) —
an upgrade already has a configured keyspace, so re-running mid-upgrade would
be disruptive; run `teamster setup tags` manually any time to adjust it.

## Test

```bash
cd src && go test ./...        # unit tests (wms engine, store, hook enrichment)
go vet ./...
# clean install: reset a disposable test VM, then run ./install.sh there
skel/lib/scripts/selftest.sh   # 7 automated checks via claude --print
skel/lib/scripts/wms-smoketest.sh
scripts/clone-acceptance-test.sh  # teamster clone: revert -> clone -> verify -> revert
                                   # (needs SSH to SOURCE_HOST/TARGET_HOST/REVERT_HOST)
```

In a worktree, `go build`/`go vet`/`go test` need `GOFLAGS=-buildvcs=false` —
the worktree's `.git` is a pointer file, not a real repo, so Go's VCS stamping
fails. `lib/installrunner.sh` sets this already; set it yourself for ad-hoc
`go` invocations in a worktree.

`internal/store`'s conformance suite (`conformance_dim1-4_test.go`,
`dim6_test.go`) runs the same behavioral tests against every registered
backend via the `backends()` table in `store_test.go`: CRUD round-trip
(dim1), transactions/atomicity (dim2), concurrency/locking (dim3), typed
error sentinels (dim4), migration lifecycle, and cross-backend attribution
equivalence (dim6). The `sqlite` entry always runs (in-memory, pure Go, no
external server); only the `mysql` entry SKIPs (vacuous green) unless
`TEAMSTER_TEST_MYSQL_DSN` is set. Dedicated test MySQL at `127.0.0.1:13306`
(root/test) — deliberately non-durable (see `scripts/test-with-mysql.sh` for
the tuning and why), so treat it as disposable: a corrupt instance gets
`docker rm -f`'d and recreated via `scripts/test-with-mysql.sh --persistent`,
never repaired in place. The DSN must point at a server-level connection (no
database name) for the per-test-schema harness to work.

Never present a change as done without running `go build ./...`,
`go test ./...`, and (for anything touching the installer, hook client, or
plugin) a clean install on a test VM + selftest. Build success != feature
success.

## Hub vs remote install model

Single-user single-host is the simple case; the production model is one
**hub** with many **remotes**:

- **Hub** runs `hookd`, both MCPs (over HTTP), the WMS MySQL database, the
  dashboard. One hub per fabric.
- **Remote** is any host running Claude Code that participates. It runs
  only the Python hook client (per-event, exits immediately) and has the
  plugin installed. No daemons, no state.

Settings.json on a remote points `TEAMSTER_HOOK_SERVER_URL` and the MCP
endpoints at the hub. `TEAMSTER_HOST` carries the short hostname so the
hub can attribute events. See `docs/specs/REMOTE-INSTALL.md`.

The hub's own `TEAMSTER_HOOK_SERVER_URL` is written with the hub's **hostname**
(`os.Hostname()`), not `localhost` — hookd binds all interfaces, so this works
for both hub-local and remote clients, and lets `teamster install-remote`
propagate a remote-reachable `--server` by default. Reinstall heals a stale
`localhost`/`127.0.0.1` value but preserves a real hostname/FQDN or an explicit
`--hookd-endpoint`.

**macOS is a remote-only platform.** The hub installer (`install.sh` /
`lib/installrunner.sh`) hard-fails on Darwin — run it on the Linux hub and
enroll the Mac with `teamster install-remote <user>@<mac>` over SSH. On macOS
the remote uses a launchd LaunchAgent (not cron) for the token-scraper, and
Agent-Teams teammates run as separate top-level sessions (see Pitfalls).

## Components — what each one is for

| Binary | Purpose | Notes |
|--------|---------|-------|
| `teamster` (Go) | Hook client + CLI on the hub | Forked per hook event. Reads JSON from stdin, enriches, POSTs to hookd. Must exit 0 always. Also serves as the CLI: `start`/`stop`/`status`/`wms-reset`/`tags`/`setup tags`/`wms drain`/`wms list`/`wms close`/`wms gc`/`wms review-sweep`/`install-remote`/`backup`/`restore`/`clone`/`pricing`. `pricing` (`list`/`add`/`close`) edits the `model_pricing` rate card — see `teamster pricing help`; like every CLI subcommand it resolves its DSN from `$TEAMSTER_STORE_DSN`, then `teamster.yaml`, so an unset env var reaches the live hub, and opening the store auto-migrates. `wms review-sweep` is the nightly two-stage lifecycle-hygiene sweep (issue #11): Stage 1 parks a stale `review`-state WorkUnit or idle Outcome to `on_hold` (reversible, no SQL); Stage 2 abandons only what the sweep itself parked, if untouched past a second threshold. Disabled and dry-run by default (`ReviewSweep.Enabled`/`Confirm` in `teamster.yaml`). Distinct from `rollup --sweep`'s cost-attribution sweep despite the shared name — see `skel/doc/specs/semantic-conventions.md` §4.8. |
| `teamster.py` (Python) | Hook client on remotes | Pure stdlib, no third-party deps. Same wire contract as Go version. On macOS, derives teammate identity from the transcript's `agentName` (sets `agent_type` when the payload lacks one) and echoes hookd's `additionalContext` on PreToolUse **and** UserPromptSubmit. `TEAMSTER_DEBUG_RAW=1` dumps raw hook stdin to `var/raw-hook-debug.jsonl`. |
| `hookd` | HTTP event server | Loads the interceptor registry at startup (`intercept.LoadWithOverlay`: embedded default compiled into the binary, overlaid with `$BASEDIR/etc/interceptors.yaml` if present and valid — a malformed or missing file falls back to the embedded default with a logged warning, never blocking ingest). Tag colors for `display` are also seeded from this registry. POST `/event` → JSONL append. Serves dashboard at `/`, WMS at `/wms`, SSE at `/events/stream`. POST `/context` takes context-window gauge reports (Claude Code statusLine, plus Codex `codex-context-subscriber` posts, which must carry `runtime=codex` and `context_source=codex_appserver` together). POST `/telemetry` (Claude Code + Codex ledger rows, stamping `token_ledger.rate_id`; a scraper's `rate_id=-1` becomes the runtime's `embedded-fallback-v1` sentinel row) and POST `/session` (Codex sessions-row upsert, `internal/server/session.go`, wraps `store.UpsertSession` via `store.ValidateSession`; also accepts optional `relationship` + `agent_id` — `relationship:"subagent"` derives `parent_ref` from the root thread's roster row and inherits its `team_name`, and `UpsertRosterEntry` keeps existing `parent_ref`/`team_name`/`agent_id` when a later post omits them) are both hub-local- and remote-callable, and both rejected in read-only mode like `/mcp/*`. GET `/rates` (`internal/server/rates.go`) serves the `model_pricing` rate table as JSON for scrapers with no store connection, optionally filtered by `?runtime=` and `?model=`; a pure read, served in read-only mode too. Muster surfaces: POST `/mcp/roster` (agent roster MCP), POST `/mcp/health` (agent health MCP), both rejected in read-only mode. Auto-registers agents on first hook event (`dispatchObservability` early-upsert creates `sessions` + `agent_roster` rows with `status: active`). Tracks per-agent turn state (processing/idle + in-flight activity) in memory. Focus-absent nudge on PreToolUse (max 1 per session+agent per turn). Returns activity + team-dispatch `additionalContext` on UserPromptSubmit so remote Python clients get the nudge (the hub Go client ignores it — no double-inject; hookd can't see a remote's solo/team marker, so it always sends team context). Claude agent-instance identity is agent_id-first (`registerNewSubagentInstance`, `internal/server/server.go`): `ResolveByAgentID` runs before the Agent-tool name FIFO, so a known instance keeps its roster row. A `(session, name)` roster row with a non-empty `agent_id` is never adopted by a different `agent_id` — the new instance is auto-numbered (`@name-2`) instead, which is the teamster#27 fix (a respawned same-name teammate no longer inherits the dead predecessor's gauges/cost); a legacy row with no `agent_id` is stamped atomically via `ClaimRosterAgentID` so only one racing resume wins (`adoptRosterRow`). The early-registration goroutine in `dispatchObservability` stamps `agent_id` on the row it writes and refuses to overwrite a row owned by another `agent_id` (deferring to SubagentStart); `completeEarlyRegistration` finishes the FIFO/parent/description bookkeeping at SubagentStart. The bounded `.meta.json` sidecar self-heal (`selfHealParentRef`, capped by `selfHealMaxAttempts` and `selfHealMinSpacing`) now also runs from the instance's first tool events, not only SubagentStart: it heals a nil `parent_ref` from the sidecar (setting `relationship=subagent` when `spawnDepth>0`) and the description. `agent_roster.description` (mysql migration v77 / sqlite v76 "roster-description") is a short human label for subagent rows: sourced first from the Agent call's `tool_input.description` captured into the FIFO (`resolveSubagentName`), then overridden by the sidecar `description` when readable (ground truth per `agent_id`); normalised by `store.SanitizeRosterDescription` (control chars collapsed, 255 runes) and written via `SetRosterDescription`. `/health/api/agents` exposes `agent_id` and `description`; the roster MCP exposes `description`. |
| `health-collector` | Muster: health gauge collector | Hub daemon, 15s poll interval. Polls `token_ledger` for per-agent token usage (recorded exception E2 — direct SQL read of another concern's table), computes context window usage, writes `agent_health_gauge` rows via GaugeStore. Prices via `pricing.Resolver` over the store directly (not `pricing.ComputeCost`). Resolves `roster_id` per agent via `ResolveRosterID`. Model sourced from `token_ledger.model` only (never hookd events — see `~/gh/teamster-context-bug.md`). Static context-window table: 200k default, 1M when model contains `[1m]`. Codex-runtime rows instead keep the latest `codex_appserver` report from `codex-context-subscriber` regardless of age (`chooseCodexContext`), never the Claude default; no report yet = 0 with source `heuristic`. Agent-Teams teammates get context occupancy from their own transcript (`teammate_context.go`), since the statusLine channel only ever fires for Agent-tool subagents (`gauge.ContextSourceTranscript`/`Fallback`/`Unavailable`). The transcript is located by the roster's `agent_id` (`findTeammateTranscript` → `findTeammateTranscriptByID`: `subagents/agent-<agent_id>.jsonl`, `.meta.json` taskKind `in_process_teammate` still required) because an auto-numbered `@name-2` teammate keeps sidecar name `name`; the name + newest-mtime match is only the fallback (agent_id not yet stamped, file not yet written), upgraded once to the agent_id path when it becomes known. The `agent_id` is cached from the same `GetRosterEntry` as the team name. Per-agent cost sums each new `token_ledger` row's own component token columns (input/output/cache-read/cache-write, split by 5m/1h TTL tier) via `costForRows`; `session_total_cost_usd` (lead's row only) is a stored-value `SUM(cost_usd)` over the whole session, immune to a stale pricing table. Optional `--metrics-addr` (e.g. `:9126`) serves Prometheus `/metrics`, including the pricing `Resolver` stats collector; empty by default and not set by the systemd unit, so it is opt-in. |
| `ctop` | Muster: terminal fleet dashboard | Bubbletea TUI, `cmd/ctop/`. Single view (fleet_view.go): a multi-team tree with hierarchy indentation, per-agent health/cost, and an activity log. Activity text/tag for a row is resolved by `resolvedActivity`: the health API poll is authoritative, the SSE tracker only overlays when its own timestamp is strictly newer — one rendering path regardless of source. Rows are grouped by lineage (`groupBySession`, `agents.go`): each row's group is the session of the root of its `parent_ref` chain (16-hop cap; a cycle keeps the row in its own session, a broken chain stops at the last resolvable ancestor), so macOS separate-session teammates merge into their lead's group via `parent_ref`; `team_name` is deliberately NOT a merge key. A second group with the same team name is labelled `#team·<prefix8>` (`fleetForestRows`, `fleet_view.go`). `relationship=="subagent"` rows show the roster `description` as a dim label after the name (`renderAgentRow`). |
| `feed` | Terminal activity viewer | Tails JSONL, ANSI colorizes. Built from `cmd/feed/`. |
| `activity-mcp` | MCP stdio (activity) | **No-op.** Tools just return confirmation strings. Real data extraction happens in the hook from PreToolUse payloads — that's how we get `agent_type` for teammate attribution. Includes `setMode` for session mode switching. |
| `wms-mcp` | MCP stdio (WMS) | Outcome/WorkUnit CRUD, tags, focus, dependencies over MySQL/MariaDB via `TEAMSTER_STORE_DSN`. Includes `wms_search`, exposing the `wms.Search` primitive as granular `[]Hit` (the same engine `teamster search sessions` groups into session rollups). Also includes `wms_renameOutcome`/`wms_renameWorkUnit` for title-only renames (no state-machine validation). `wms_claimWorkUnit` returns the WU's `brief`; the `pending`→`active` status transition is an atomic CAS, and the call also requests a focus interval alongside it — that open is hookd's, asynchronous and best-effort, and may be declined — see "dispatch-package" below. `wms_deliverResult` stores an agent's output durably in `wms_deliverables` and transitions the WU active→review; `wms_listDeliverables` reads back stored deliverables for a work unit (oldest first, last row is the current answer). `wms_addOutcomeParent`/`wms_removeOutcomeParent` restructure the Outcome DAG after creation; `wms_addRelation`/`wms_removeRelation`/`wms_listRelations`/`wms_listRelationKinds` manage typed post-delivery relation edges (`outcome_relations`) — see `skel/doc/specs/semantic-conventions.md` §4.6. State changes posted to hookd via `HookObserver` when `TEAMSTER_HOOK_SERVER_URL` is set. |
| `rollup` | Cost-attribution pipeline | Allocates token spend to WMS entities via focus intervals. Flags: `--reallocate`, `--recover-focus`, `--recover-warmup`, `--recover-gaps`, `--recover-directives` (focus-less remote teammates → entity named in their dispatch brief), `--repair-focus-intervals` (one-time fix of negative-width focus intervals from the dual-writer/async race), `--synthesize-remote-orphans` (remote sessions with no focus/directive/transcript → temporal correlation with concurrent focused sessions on the same host), `--synthesize-focus <file>`, `--sweep` (chains all deterministic passes), `--sweep-llm` (adds LLM-assisted synthesis), `--count-orphans` (print processable orphan count; checks local transcript existence), `--reprice` (rate-card drift tool; mutually exclusive with every other rollup flag, and the `--reprice-*` flags require it). `--reprice` recomputes each `token_ledger` row with a non-NULL `rate_id` from that row's own rate and reports rows whose stored `cost_usd` differs by more than 0.000001; `--reprice-dry-run` (default true) writes nothing, `--reprice-apply` rewrites only `cost_usd` (never `rate_id`) and journals each change to `reprice_journal` (requires `--reprice-reason`; conflicts with an explicit `--reprice-dry-run=true`), `--reprice-backfill` instead stamps `rate_id` on NULL rows by resolving (runtime, model) at the seed `valid_from` without changing `cost_usd` (dry-run by default; unresolved models stay NULL), and `--reprice-session`/`--reprice-model`/`--reprice-since <duration>` filter either mode. Embedded-fallback sentinel rows are excluded from drift. See `docs/pricing-runbook.md`. Both the normal and `--sweep` paths end with `reconciler.ReconcileSince` (`cmd/rollup/reconcile.go`) as a non-fatal verification pass: it compares recent sessions' ledger cost against OTel and stores verdicts in `cost_verification`; a failure is logged and never fails the pass. Reversible: `--unrecover`, `--unrecover-warmup`, `--unrecover-gaps`, `--unrecover-directives`, `--unrepair-focus-intervals`, `--unsynthesize-remote-floor`, `--unsynthesize`. |
| `classify` | Interval phase + work-type classifier | Derives `phase` and `work-type` tags on intervals/workunits from rule-based signals. Recovers missing required lifecycle tags on work units (safety net for dispatch gaps). Run by systemd timer every 10 min. `--reclassify` re-derives from scratch. `--dry-run` logs lifecycle recovery intent without writing. |
| `token-scraper` | Session transcript scraper | Reads **Claude Code** session JSONL transcripts and POSTs per-message token usage to hookd's `/telemetry` endpoint. Codex has its own tailer (`codex-scraper`) — this one never reads Codex data. Prices via `pricing.Resolver` over the HTTP `RateSource` (hookd's GET `/rates`; no store connection), falling back to the embedded tables with a WARN. |
| `codex-scraper` | Codex rollout-JSONL cost/ledger tailer | Oneshot (systemd timer, every 10 min), **not** a daemon. Tails `~/.codex/sessions/**/rollout-*.jsonl` (+ `archived_sessions/`) and is the **sole writer** of Codex cost data: POSTs per-`token_count` ledger rows to hookd's `/telemetry` (cost derived from `last_token_usage`) and upserts the Codex `sessions` row via hookd's `POST /session` (a second, identity-only writer of that row is `codex-context-subscriber`; the scraper is the sole *cost* writer; no direct store connection anymore — migrated so the hub binary and the Python remote port (`skel/lib/scripts/codex-scraper.py`, staged on remotes/client-mode installs) share one HTTP code path; hookd's hook pipeline never fires for Codex either way). Every row carries `runtime='codex'`. Codex 0.142.x `thread_spawn` subagents write their own rollout file whose `session_meta.session_id` is the **parent** thread's id, so subagent spend books under the root `session_id` (falls back to the file's own `id` on 0.137.0, which lacks `session_id`); `message_id` is keyed by the file's own thread id to avoid sibling collisions. Subagent files register with `relationship:"subagent"` + `agent_id` (the thread id); `agent_name` follows the identity rule in `skel/doc/specs/semantic-conventions.md` §10.3 (single definition). Prices via `pricing.Resolver` over the HTTP `RateSource` (hookd's GET `/rates`), falling back to the embedded tables with a WARN. See `docs/specs/CODEX-INSTALL.md`. |
| `codex-context-subscriber` | Codex context-window reporter | Pure-stdlib Python daemon (`skel/lib/scripts/codex-context-subscriber.py`, systemd `Type=simple`). Attaches passively (never answers approvals) to Codex's shared app-server daemon over its control socket (WebSocket over `$CODEX_HOME/app-server-control/app-server-control.sock`), subscribes only to Active threads (a resume pins a thread in the daemon: one-shot snapshot at startup / `thread/started`, unsubscribe on idle), and on each `thread/tokenUsage/updated` POSTs to hookd's `/context` with `runtime=codex`, `context_source=codex_appserver` (must be set together; pct clamped to [0,100] on both sides); fill = `last.totalTokens / modelContextWindow`. Same `session_id`/`host` and the same `agent_name` rule as `codex-scraper` (semantic-conventions §10.3). Also POSTs hookd `/session` when it learns a non-ephemeral thread and at most once per 60 s per active thread (heartbeat), so a subagent is visible in `ctop` immediately and its roster row stays live; it lands on the same gauge row (`codex-scraper` stays the sole *cost* writer; this is the second `/session` writer, a recorded exception). Idles with backoff when no Codex socket exists. Unit installed only when `--codex-mode` is not `none`; restarted on upgrade; in `teamster stop`/`status`. Hub-wired only — remote install wiring is not yet done; `codex --no-daemon`/`--oss`/`--profile`/`-c` sessions run in-process and are invisible to it. See `docs/specs/CODEX-INSTALL.md`. |
| `mcp-scraper` | MCP tool-call telemetry tailer | Oneshot (systemd timer, every 10 min), not a daemon. Tails hookd's `events.jsonl`, filters to completed (PostToolUse) `mcp__*` tool calls, and ledgers one row per call into `claude_telemetry.mcp_tool_calls` — a call-volume instrument (WP11 §2), not an audit trail (no `tool_input`/entity-id capture). Cursor + copytruncate guard, generation recovery from `MAX(source_generation)+1`. Ledger table is created at install time under `--store-mode=install`; under `--store-mode=managed`/`external` the binary's own `CREATE TABLE IF NOT EXISTS` creates it on first enabled run (requires `CREATE` on `claude_telemetry`). Config-gated (`MCPScraper.Enabled` in `teamster.yaml`, or `TEAMSTER_MCP_SCRAPER_ENABLED=1`), mirroring the review-sweep pattern. Single-writer invariant: exactly one mcp-scraper process (the hub's) ever writes `mcp_tool_calls`, which is why `teamster-mcp-scraper.timer` is permanently masked on every `teamster clone` target — a clone ledgering its own local traffic would corrupt the hub's volume measurement. |
| `teamster-install` | Installer | Called by `lib/installrunner.sh`. Explicit `--basedir/--repo/--builddir` flags — no path inference. |
| `demogen` | Synthetic data generator | Creates correlated demo data across token_ledger/wms_intervals/usage_attribution/entity_tags/cost_rollup. `--clean` for teardown. |
| `backup` | Backup engine | Standalone binary for systemd timer. Takes timestamped snapshots of MySQL, OTel, and teamster config/state. No sudo. CLI also accessible via `teamster backup` and `teamster restore`. |
| `relay` | Event relay | Tails hub JSONL, POSTs each line to replica hookd `/event`. Built by installer when `--relay-mode=install`. See `docs/specs/replication.md`. |
| `teamster tags` | CLI tag management | Subcommands: `list`, `add-key`, `add-value`, `retire`, `describe`. Built into `cmd/teamster/tags.go`. |
| `teamster setup tags` | TUI tag wizard | Bubbletea-based guided setup for tag keyspace. 8-screen wizard on first run, 3-column editor on subsequent runs. Built from `internal/tui/`. |
| `teamster search sessions <query>` | Find sessions by what they worked on | Session-centric: one row per session, matched via outcome/workunit title-description-focus, tag values, or focus intervals/session focus text (`--type`). Filters: `--user`, `--host`, `--status`, `--tag key=value`, `--since <dur>`, `--limit`; `--json` for scripting. Built from `cmd/teamster/search.go` over `wms.SearchSessions`. |
| `teamster clone <user>@<host>` | Stand up a disposable dev-peer instance | Runs from the source host (R9), pushes outward over SSH — never installs on, migrates, or mutates the source. Pipeline: resolve build → probe target → ship verified source (git archive + scp + content-manifest fingerprint gate) → translate topology (`internal/clonetopology`) → invoke `lib/installrunner.sh` → Stage A verify (`<binary> --version`, blocks the data leg on mismatch) → assert schema version → mask sweep/backup/review-sweep/mcp-scraper timers permanently, four units (R11) → transfer + restore data (`internal/clonedata`) → verify row counts via the `clone_verify_ro` read-only credential (I7) → `teamster start`. `--dry-run`/`--allow-dirty`/`--repo-dir`/`--github-repo`/`--source`/`--ref`/`--fresh-backup` flags. See `skel/doc/specs/architecture.md`'s "Clone" section for the isolation contract (I1–I7). Built from `cmd/teamster/clone.go`, `clone_install.go`, `clone_data.go`. |

### Systemd timers

| Unit | Schedule | Purpose |
|------|----------|---------|
| `teamster-rollup.timer` | Every 10 minutes | Runs `rollup --sweep` (full deterministic pipeline: entity hygiene + attribution recovery + aggregation) |
| `teamster-classify.timer` | Every 10 minutes | Runs `classify` to derive phase/work-type |
| `teamster-sweep.timer` | Every hour | Runs `claude --print /teamster:sweep` for LLM-assisted synthesis, gated on `--count-orphans` (skips when nothing to process) |
| `teamster-backup.timer` | Configurable (default 1h) | Runs backup to snapshot all stores |
| `teamster-codex-scraper.timer` | Every 10 minutes | Runs `codex-scraper` (oneshot) to tail Codex rollout JSONL and ledger Codex cost. Installed only when Codex is wired; a no-op on a host with no `codex` CLI (finds no rollout files, exits 0). |
| `teamster-codex-context-subscriber.service` | Long-running daemon (`Type=simple`, `Restart=always`) | Runs `codex-context-subscriber`; not a timer. Installed only when Codex is wired (`--codex-mode` not `none`) and hookd is in systemd mode; idles with backoff when no Codex app-server socket exists. |
| `teamster-mcp-scraper.timer` | Every 10 minutes (`OnBootSec=2min`, `OnUnitActiveSec=10min`) | Runs `mcp-scraper` (oneshot) to tail hookd's `events.jsonl` and ledger completed `mcp__*` tool calls into `claude_telemetry.mcp_tool_calls`. Config-gated (`MCPScraper.Enabled`). Permanently masked on `teamster clone` targets, same as `teamster-sweep.timer`/`teamster-backup.timer`/`teamster-wms-review-sweep.timer` — a clone must not ledger its own local traffic as if it were the hub's. |
| `teamster-wms-review-sweep.timer` | Nightly, 03:00 | Runs `teamster wms review-sweep` (oneshot): parks stale `review`-state WorkUnits and idle Outcomes to `on_hold`, then abandons only entities the sweep itself parked if still untouched past a second threshold. Name collides with `teamster-sweep.timer` above but the two are unrelated — this one is WMS lifecycle hygiene, not cost attribution. Config-gated (`ReviewSweep.Enabled`, `teamster.yaml`) at BOTH install time and run time: the `.service`/`.timer` files are always staged under `$BASEDIR/etc/` at install/upgrade, but `lib/installrunner.sh` only copies them into systemd's unit directory and enables the timer when `Enabled` was already `true` in `teamster.yaml` at that install — a fresh install (`Enabled: false`, the default) registers nothing with systemd at all, unlike `teamster-sweep`/`teamster-backup`, which install their units unconditionally and only gate whether they run. Flipping `Enabled: true` afterward needs an installer re-run (or a manual `install`/`daemon-reload`/`enable --now` of the two already-staged files) to register the unit; only the later `Confirm: true` flip is a pure config edit with no reinstall. Masked permanently on `teamster clone` targets, same as `teamster-sweep.timer`/`teamster-backup.timer`/`teamster-mcp-scraper.timer`. |

## Key conventions

- **Display strings use `__param__` markers** for dynamic values. The source
  decides what's a parameter; the renderer styles it. Never pattern-match
  verb prefixes at display time. (`__file__`, `__pattern__`, `__id__`.)
- **Entity naming**: `@agent`, `#team`, `<model>` — colorized in the stream.
- **Tag taxonomy** (see `skel/doc/specs/semantic-conventions.md` §3 for the
  full table): GOAL/THNK/DONE/RCAP come from MCP activity tools and Stop events; READ/EDIT/GREP/ACT/
  EXEC/TEAM/COMM/TASK/WEB/ASK/PLAN come from built-in Claude Code tool names
  (`TOOL_TAGS` map in `internal/hook`); TOOL is the fallback. `mcp__*` tool
  tags (TASK, CHRM, ` GIT`, GDRV, TEAM, and any suppressed tools) are no
  longer hardcoded — see the next bullet.
- **`interceptors.yaml` is the source of truth for `mcp__*` tool → tag/
  display mapping.** `$BASEDIR/etc/interceptors.yaml` (embedded default +
  file overlay; loads at hookd startup) defines tag labels/colors and
  per-tool match rules, compiled by `internal/intercept/` into a `Registry`.
  `hook.EnrichRecord` checks the registry first for every `mcp__*` tool name;
  the remaining hardcoded `mcp__activity__`/`mcp__wms__`/generic-MCP blocks
  in `enrich.go` only run when the registry has no match. The Go hook client
  itself defers `_tool_tag`/`_tool_display` enrichment for non-built-in
  (`mcp__*`) tool names to hookd — it still writes the special fields
  (`_focus` from `setFocus`/`setOverallIntent`, `_thought`, `_done`, the
  session mode marker from `setMode`) client-side, since those feed the
  focus-nudge and solo/team gates independently of display. Validate an
  edited `interceptors.yaml` with `teamster check-config` before restarting
  hookd.
- **MCP no-op + hook extraction**: MCP tools are callable surface area for
  the model only. The hook client pulls the actual args out of PreToolUse.
  This is how we attribute MCP calls to teammates (MCP servers can't see
  `agent_type`, hooks can).
- **JSONL is the contract** between hook client and feed. Enriched fields
  (`_tool_tag`, `_tool_display`, `_focus`, `_bash_cmd`, `_agent_name`) must
  be kept in sync across both sides of any change.
- **WMS entity model**: Outcome → WorkUnit (two-level). Both share
  statuses: `pending`, `active`, `review`, `done`, `blocked`, `on_hold`,
  `abandoned`. `done` and `abandoned` are both terminal; `done → review` is
  the sole reopen edge. The engine does not auto-close an Outcome when its
  WorkUnits (or child Outcomes) finish — closing is a deliberate act (see
  `session-protocol.md` Step 9). WorkUnits also carry `brief` (the full
  dispatch assignment text — returned only by
  `wms_claimWorkUnit`/`wms_getWorkUnit`, never by list tools, to keep list
  responses lean) and `claimed_at` (timestamp of the first successful claim,
  nil until then).
- **dispatch-package**: `wms_claimWorkUnit` is the preferred focus-attribution
  path for WU-scoped work — it returns the brief and transitions pending→active
  (or adopts an unowned active WU / no-ops on an idempotent re-claim by the
  same owner) as an atomic status CAS, and requests a focus interval alongside
  it. The interval open itself is hookd's separate, asynchronous, best-effort
  follow-on — it can decline (e.g. an identity race between two claimants),
  in which case a follow-up nudge tells the agent to call `wms_setFocus`
  itself. Replaces the old voluntary `wms_setFocus` pattern for dispatched
  work; `setFocus` remains for the lead's own Outcome focus and non-WU-scoped
  work.
  `wms_deliverResult` stores an agent's output durably in `wms_deliverables`
  (append-only, redelivery allowed — consumers take the last row, last-wins)
  and transitions the WU active→review — the lead reads stored deliverables
  instead of trusting final-message compression.
- **Tag keyspace** uses `product` and 9 work-scope slug keys (`feature`, `bug`, `refactor`, `polish`, `infra`, `docs`, `research`, `test`, `admin`) as core context keys. Slug keys are facets of `work-type` and share the `work-scope` exclusion group.
  Integration key namespaces (`github.*`, `jira.*`, etc.) are seeded at setup time via
  `teamster setup tags`. Phase/resolution/lifecycle keys have single cardinality.
- **Activity tag persists through the full read path**: `last_activity_tag`
  (the READ/EDIT/GOAL/... tag coloring an agent's activity text) flows
  `agent_health_gauge.last_activity_tool` (column name predates its current
  meaning) → health API's `last_activity_tag` JSON field → ctop's
  `resolvedActivity`. Any consumer that renders activity text must carry the
  tag alongside it — text without its tag renders dim/uncolored instead of in
  its canonical tag color.
- **Focus nudge**: hookd checks for any focus interval (open or closed) on
  PreToolUse and injects `additionalContext` if missing. Max 1 nudge per
  (session, agent) per turn. Cache-backed (`src/internal/server/nudge.go`).
- **Store construction**: every composition root builds its `store.Store` via
  `store.Open(ctx, dsn)` (`internal/store/factory.go`), never by naming a
  backend package directly. Backends self-register under a DSN scheme
  (`mysql`/`mariadb`, `sqlite`) from a blank import's `init()` — a composition
  root pulls in only the backend(s) it needs (e.g.
  `_ "github.com/bmjdotnet/teamster/internal/store/mysql"`). There is no
  `.DB()` escape hatch; callers consume `store.Store`'s role-based
  sub-interfaces (`SessionStore`, `IntervalStore`, `AllocationStore`, ...)
  rather than a concrete backend type.
- **Store error handling**: backends map driver errors onto three sentinels —
  `store.ErrNotFound`, `store.ErrConflict`, `store.ErrPrecondition`
  (`internal/store/errors.go`). Check with `errors.Is`, never a driver-specific
  error string or code. `ClaimWorkUnit`'s claim-state matrix adds two more
  specific sentinels, `store.ErrAlreadyClaimed` (WU active, owned by someone
  else — carries `.Owner` via `errors.As(&store.AlreadyClaimedError{})`) and
  `store.ErrNotClaimable` (WU in a terminal status — carries `.Status` via
  `errors.As(&store.NotClaimableError{})`); both also satisfy
  `errors.Is(err, store.ErrPrecondition)` so callers that only know the
  coarser sentinel keep working unchanged.
- **Admin-plane store capabilities** (`RawExecutor`, `BackupEngine`,
  `DemoSeeder`, `CredentialProber`) are optional and reached by type-asserting
  a `store.Store` — they are not part of the core `Store` interface, since a
  backend may legitimately lack them (e.g. `teamster sql` fails cleanly on a
  backend with no `RawExecutor`, rather than a compile break).
- **The protocol lives in the plugin**, not in code. The Eight Rules and
  Field Guide at `skel/lib/plugin/skills/bootstrap/references/` are the canonical
  source. `/teamster:start` is the front door; `/teamster:bootstrap` boots
  a team; `/teamster:solo` starts subagent mode.

## Pitfalls (collected from prior incidents)

- MCP config lives in `~/.claude.json` (`claude mcp add-json --scope user`),
  **not** `~/.claude/mcp.json` — that path is not read by Claude Code.
- On the **hub/Linux**, Agent-Teams teammate events mostly appear as regular
  tool calls within the lead's session_id, and identity comes from the
  `agent_type` field on hook payloads. But `SubagentStart` **does** fire for
  a teammate — on every turn-resume (mailbox wakeup), not just true Agent-tool
  spawns — carrying the SAME `agent_type` as the teammate's very first event.
  `agent_type` alone can't tell "new entity" from "known entity resuming," so
  hookd's `SubagentStart` handler checks the **persistent roster store**
  first (`ResolveRosterID`) before registering — the in-memory session
  tracker isn't a safe-enough guard either, since its own eviction sweep can
  make a long-idle teammate's resume falsely look brand new
  (`internal/server/server.go`'s `dispatchObservability`,
  `subagent_start_dedup_test.go`). Two more hook events exist purely for
  Agent-Teams awareness and carry `teammate_name` instead of `agent_type`:
  `TeammateIdle` (only push signal for a teammate's idle transition — without
  it turn state only flips on `Stop`) and `TaskCompleted` (per finished
  in-progress task at turn end, log-only).
- **macOS teammates differ — separate top-level sessions, no `agent_type`.** On
  macOS, each Agent-Teams teammate runs as its own top-level Claude Code session
  (distinct `session_id`, its own `~/.claude/projects/<proj>/<session>.jsonl`
  transcript, NOT under `subagents/`), and its hook payloads carry **no
  `agent_type`**. The teammate name lives only in the transcript's top-level
  `agentName` field. `teamster.py` derives `agent_type` from `agentName` (via
  `transcript_path`) so the feed shows `@<name>`; `token-scraper.py` does the
  same so cost attributes to `@<name>` instead of the lead. Use
  `TEAMSTER_DEBUG_RAW=1` to confirm what a payload actually contains.
- The hub hook URL defaults to the hub **hostname** (`os.Hostname()`), not
  `localhost`. A `localhost` value breaks remote installs (`install.sh` probe
  warns); reinstall heals stale `localhost` but preserves a real hostname/FQDN
  or `--hookd-endpoint`. hookd binds all interfaces, so a hostname URL is
  correct for both hub-local and remote clients.
- hookd returns activity + team-dispatch `additionalContext` on UserPromptSubmit
  for remote Python clients; the hub Go client generates its own and ignores the
  field (no double-inject). hookd can't see a remote session's solo/team marker
  (client-local state), so it always sends **team** context to remotes.
- Claude Code fires both PreToolUse and PostToolUse for every tool, lead and
  teammate alike, and the Go client sends every event to hookd. It enriches
  PostToolUse for feed display only for the lead and never for Bash
  (`shouldEmit` in `internal/hook/hook.go`), and dedups display lines in
  `<DataDir>/dedup/<session12>.tool`. hookd's Agent-tool name FIFO
  (`resolveSubagentName`) depends on the spawner's `Agent` PreToolUse.
- `UpsertSession` (mysql `store.go`, sqlite `session.go`) COALESCE-protects six
  columns from a blank-upsert wipe — `team_name`, `project_id`, `goal_id`,
  `task_id`, `workitem_id`, `focus` (`COALESCE(NULLIF(new,''), old)`): a re-post
  (e.g. the Codex subscriber's 60s heartbeat) that omits them keeps the stored
  value. The same guard already covered `model`. A deliberate clear of one of
  those columns therefore cannot go through `UpsertSession`.
- Team names are reused across sessions by design. ctop disambiguates by
  lineage (`groupBySession`, `#team·<prefix8>`), but the health API team
  endpoints (`GET /health/api/team/{team_name}`, `health_getTeamSummary`) still
  merge across sessions.
- The hook client must **never** block or crash. Exit 0 always, 2s HTTP
  timeout, swallow all errors. If it hangs, Claude Code hangs.
- Claude Code's `/goal` is a condition-based pass/fail gate. It is **not**
  the same as our `[GOAL]` tag, which is a free-text focus declaration
  from `setOverallIntent`. Different concepts despite the name collision.
- Installer must merge non-destructively. Never overwrite working settings.
  Dedupe by semantic identity, not exact string match. Back up before write.
- After changing `feed`, the user must restart it — it's a long-running
  process. Hook client changes take effect on next tool call (forked).
  `hookd` changes need a systemd restart.
- **`TEAMSTER_TEST_MYSQL_DSN` unset means the mysql conformance/migration
  tests silently SKIP, not fail.** `go test ./...` reports green with the
  mysql half of the suite never having run — the `sqlite` backend entry still
  runs and can mask a mysql-only regression. Always export
  `TEAMSTER_TEST_MYSQL_DSN` (server-level connection, no database name — a
  pre-existing base database hides bugs) before trusting a "tests pass"
  result that touches `internal/store`. Dedicated test MySQL instance:
  `127.0.0.1:13306` (root/test).
- **The test MySQL instance is tuned for speed, not durability — it is
  disposable by design.** `scripts/test-with-mysql.sh` (both its ephemeral
  and `--persistent` modes) runs it with fsync/binlog/doublewrite disabled
  and the datadir on tmpfs, because every test creates a schema and runs the
  full migration chain, so DDL/fsync cost otherwise dominates wall-clock
  time. This means the container's data does not survive a restart and a
  crash mid-write can leave it corrupt — both are fine, since the fix is
  always `docker rm -f teamster-test-mysql && scripts/test-with-mysql.sh
  --persistent`, never manual repair. Never point `TEAMSTER_TEST_MYSQL_DSN`
  at a database whose data needs to survive.
- **Worktrees need `GOFLAGS=-buildvcs=false`.** A `tm wt` worktree's `.git` is
  a pointer file, not a real repository, so Go's default `-buildvcs=true`
  fails trying to stamp VCS info. Required for any ad-hoc `go build`/`go
  vet`/`go test` run outside `lib/installrunner.sh` (which already sets it).
- `ts` in JSONL is an RFC3339 string, not an epoch float. Float64 mis-decode
  silently zeroes all classifier signals.
- Migration races: 5 callers can race `migrate()` on a fresh DB. The fix uses
  an advisory lock over the whole migration loop + `information_schema` column
  guards. Must work on both MySQL 8.0 and MariaDB 11.8.
- The hub's Grafana is `mode=external` (shared with other apps). Teamster is a
  tenant. The installer must never auto-restart a shared `grafana-server`.
- **`crontab` is not scoped by `$HOME` overrides.** Testing `remote-setup.sh`'s
  (or client-mode install's) cron-wiring step (`token-scraper`/`codex-scraper`
  registration via `crontab -l | ... | crontab -`) with a redirected `$HOME`
  for isolation does **not** prevent it from writing to the real, current
  OS user's crontab — `crontab` reads/writes the user's job table, not a path
  under `$HOME`. Any manual test of this step on a real host installs a REAL
  cron entry for that user. Container or VM isolation is mandatory for this
  test, not merely convenient.
- **`teamster stop` issues `systemctl stop` before any PID/port fallback.**
  `supervisorStop` used to fall through straight to `killByPort` whenever no
  PID file existed — the normal state under systemd — SIGKILLing the process
  behind systemd's back and racing `Restart=on-failure`. It now issues
  `sudo systemctl stop` for every supervisor-managed unit first (best-effort:
  errors are discarded, not fatal) before the PID/port fallback ever runs.
  `componentSupervisorManaged`/`hookdSupervisorManaged` are the single
  eligibility predicates called from both the start and stop paths — don't
  reimplement the "is this unit systemd-managed" check inline.
- A claimed WorkUnit's `agent_id`/`claimed_by` reads empty on today's stdio
  path regardless of who claimed it — `p.Meta.AgentType` is empty for a lead
  by design, but it is also empty for every teammate, because the wms-mcp
  stdio client sends no agent identity at all. hookd's identity recovery
  (the PreToolUse stash peek) fixes the *focus-interval* open, not this
  column — it deliberately never writes `WorkUnit.agent_id`, since a late
  async write would race the CAS that decides adoptability. So an empty
  `agent_id` on an active WU is not a data bug, but it is also not evidence
  the lead owns it — ownership on this row stays vacuous for every claimant
  until identity reaches the MCP process, which it does not yet.

## Documentation — what to trust

| File | Status | Use for |
|------|--------|---------|
| `README.md` | **Current** — user-facing quick start | What Teamster is, install, first team, dashboard, subagent-mode opt-in |
| `docs/specs/REMOTE-INSTALL.md` | **Current** | Hub/remote install model, incl. Codex support on remotes (direct-HTTP MCP transport, what's staged, OTEL scoping) |
| `docs/specs/replication.md` | **Current** | Hub→replica replication topology (relay + repl-push) |
| `docs/specs/CODEX-INSTALL.md` | **Current** | Codex CLI support: `--codex-mode` flag, MCP server wiring + default-approve audit-trail risk, OTEL, skills delivery, hooks channel + trust provisioning, codex-scraper (cost/ledger tailer + subagent→parent attribution, now HTTP-only via hookd `/session`), codex-context-subscriber (Codex context gauge via the app-server socket → hookd `/context`), `runtime` enum, deferred-MCP-tool-loading known limitation (newer Codex builds), remote Codex support (Python port, direct-HTTP MCP transport), uninstall (hub + remote) |
| `docs/pricing-runbook.md` | **Current** | Rate-card operations: `teamster pricing list/add/close`, close-and-insert rate changes, `rollup --reprice` drift validation and apply, `--reprice-backfill` rate_id stamping |
| `docs/wizard.md` | **Current** | Guided interactive installer (`install.sh`) + tag setup TUI + per-project subagent-mode opt-in |
| `docs/session-explorer-guide.md` | **Current** | 9-point primer for driving programs via tmux |
| `skel/doc/specs/architecture.md` | **Current** — full system | Hub/remote topology, all components, data flows (incl. Codex runtime), env vars, operating modes, cost attribution, focus nudge, `teamster clone` pipeline + isolation contract |
| `skel/doc/specs/wms-dashboard-spec.md` | Forward-looking + implemented | What `/wms` should become (phases 2/3 not built) + implemented pages (cost-flow, tags, Grafana dashboards) |
| `skel/doc/specs/semantic-conventions.md` | **Current** | JSONL field conventions, tag taxonomy, WMS entity types, state machine, session mode / `setMode` signal, cost attribution methods, close-out warnings, two-focus distinction, Codex runtime conventions (`runtime` enum, session identity, subagent thread model) |
| `skel/lib/plugin/skills/bootstrap/references/dispatch-pack/eight-rules.md` | **Canonical protocol** | The Eight Rules |
| `skel/lib/plugin/skills/bootstrap/references/dispatch-pack/field-guide.md` | **Canonical lessons** | Practical operating and development lessons |
| `skel/lib/plugin/skills/bootstrap/references/dispatch-pack/muster-guide.md` | **Current** | Muster roster + health awareness for team leads |
| `skel/lib/plugin/skills/bootstrap/references/dispatch-pack/decomposition-guidance.md` | **Current** | How to split work by file independence, not domain grouping — added per field evidence of a lead colliding two agents on a shared file |
| `skel/lib/plugin/skills/bootstrap/references/implementation-pack/teammate-guide.md` | **Current** | Distilled teammate-facing protocol (Eight Rules subsets IV/VI/VIII, claim-first, execution-loop phase guidance, direct peer comms) — loads into teammate briefs, not the lead's own context |
| `skel/lib/plugin/skills/shared/session-protocol.md` | **Current** | Canonical intake mechanics (interview flow, mode selection, outcome creation, tag application), shared by `start`/`bootstrap`/`solo` instead of triplicated |
| `skel/lib/plugin/skills/seasoning/SKILL.md` | **Current** | Iterative spec refinement skill |
| `skel/lib/plugin/skills/solo/SKILL.md` | **Current** | Single-agent (subagent) mode — interview-driven selection; authoritative for the shipped solo mode |
| `skel/lib/plugin/skills/sweep/SKILL.md` | **Current** | Attribution sweep for `claude --print` |
| `skel/lib/plugin/skills/tags/SKILL.md` | **Current** | Tag steward — vocabulary refinement, merge/split, rollback |
| `skel/lib/plugin/README.md` | **Current** | Plugin overview, skills table, install instructions |

When updating any of the above, also update its row here if its status
changes.

## Working in this repo

- Edit `src/` for Go code; `skel/` for things that end up in `BASEDIR`;
  `install.sh` for the interactive installer; `lib/installrunner.sh` for
  build/install backend plumbing.
- Anything in `skel/` is shipped — treat it like production data, not
  scratch space.
- Follow the Agent Operating Protocol in `~/.claude/CLAUDE.md` (Eight Rules,
  activity reporting, agent teams). It's loaded into every session here.
- When testing installer or hook client changes, use a disposable test VM —
  never the live instance you're sitting inside.
