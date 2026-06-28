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
- `lib/installrunner.sh --basedir=PATH` **stages** binaries and skel into PATH
  but does **not** touch `/etc/systemd/system/`, `~/.claude/settings.json`, or
  MCP registration. Safe for verification. Adding `--wire` makes it touch
  global state — only use `--wire` on a disposable test VM or when you
  explicitly intend to replace the live config.
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
    activity-mcp/             MCP stdio: reportActivity/setOverallIntent/completeActivity/setMode
    wms-mcp/                  MCP stdio: outcome/workunit CRUD, tags, focus, dependencies
    rollup/                   Cost-attribution pipeline (allocate, recover, sweep)
    classify/                 Interval phase + work-type classifier (systemd timer)
    token-scraper/            Session transcript token-usage scraper
    teamster-install/         Installer binary (called by lib/installrunner.sh)
    demogen/                  Synthetic data generator for dashboards
    backup/                   Standalone backup binary (systemd timer)
  internal/
    activity/                 Shared activity-tool handler logic
    classify/                 Rule-based classifier engine (work-type, phase)
    config/                   Env-var config (TEAMSTER_* namespace)
    display/                  Entity colors, tag colors, ANSI rendering
    hook/                     Tool extraction, tag taxonomy, enrichment
    llm/                      Anthropic API client (used by sweep-llm)
    logging/                  Structured slog setup (TEAMSTER_LOG_LEVEL)
    mcp/                      Shared MCP JSON-RPC plumbing
      activity/               Activity MCP tool handlers
      wms/                    WMS MCP tool handlers
    observability/            Prometheus collectors (attribution, cost, entities, sessions, sweep)
    pricing/                  Per-model token pricing tables
    redact/                   Credential redaction for activity feed
    render/                   Display-string rendering helpers
    rollup/                   Cost allocation + recovery passes (gap, synthesize, sweep-llm)
    server/                   HTTP receiver + JSONL writer + focus nudge
    store/                    Store interface + integration tests
      mysql/                  MySQL/MariaDB store + migrations (v1–v43)
    transcript/               Session transcript reader (focus timeline, window)
    tui/                      Bubbletea TUI (tag setup wizard + editor)
    backup/                   Backup engine (config, drivers, manifest, retention, flock)
    version/                  Build-time version info
    web/                      SSE dashboard + WMS hierarchy + cost-flow + tag browser
    wms/                      WMS engine (Outcome/WorkUnit), state machines, HookObserver

skel/                         Assets copied to BASEDIR at install time
  doc/specs/                  architecture.md, wms-dashboard-spec.md, semantic-conventions.md
  etc/
    teamster-hookd.service.tmpl      Systemd unit template (uses __BASEDIR__)
    teamster-rollup.service.tmpl     Rollup one-shot service
    teamster-rollup.timer.tmpl       Rollup timer
    teamster-classify.service.tmpl   Classifier one-shot service
    teamster-classify.timer.tmpl     Classifier timer (every 5 min)
    teamster-sweep.service.tmpl      Hourly sweep one-shot (rollup --sweep + claude --print)
    teamster-sweep.timer.tmpl        Sweep timer (every hour)
    teamster-backup.service.tmpl     Backup one-shot service
    teamster-backup.timer.tmpl       Backup timer (configurable, default 1h)
    teamster-relay.service.tmpl      Event relay (hub→replica), --relay-mode=install
    teamster-repl-push.service.tmpl  Repl-push MySQL sync server (hub side)
    teamster-prometheus-replica.yml.tmpl  Replica Prometheus scrape config
    grafana-anonymous.ini            Replica Grafana anonymous-access config
    grafana/                         Provisioned dashboards + datasource configs
  lib/
    .claude-plugin/marketplace.json  Plugin marketplace root (NOTE: above plugin/)
    plugin/                   Claude Code plugin (skills/ only; marketplace.json is in .claude-plugin/ above)
      skills/{bootstrap,start,solo,plan,review,status,tags,sweep,seasoning}/SKILL.md
      skills/bootstrap/references/{eight-rules.md, execution-loop.md, field-guide.md, rubrics.md}
      skills/tags/references/
    hook/teamster.py          Python hook client used on remote installs
    scripts/                  selftest, remote-setup, session-explorer, wms-smoketest,
                              install-remote.sh, token-scraper.py, ccusage-scraper.py

install.sh                    Interactive installer entrypoint (guided, interview-driven)
lib/installrunner.sh          Build/install backend (called by install.sh)
docs/                         User-facing docs (specs + guides)
docs/specs/REMOTE-INSTALL.md  Current spec for hub/remote model
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
lib/installrunner.sh --basedir=PATH                       # stage to PATH only — safe, no global state touched
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

`--relay-mode=install` (default `none`) builds `relay` and installs the
relay + repl-push services on a hub, pushing events and MySQL data to a
read-only replica. Requires `--relay-target` and `--repl-push-remote`. On the
replica side, `--hookd-read-only` materializes `TEAMSTER_HOOKD_READ_ONLY=1`
into the hookd unit so hookd rejects MCP/telemetry/drain while still serving
reads + `/event`. See `docs/specs/replication.md`.

**Client mode** is for remote hosts: stages only the Python hook client +
the plugin, points settings.json at a hub URL. No Go required on the remote.

`teamster install-remote user@host [--server hub:9125]` is the hub-side
command that drives the client install over SSH. It execs the shell script
at `$BASEDIR/lib/scripts/install-remote.sh`, passing all args through.
During development, the script can also be run directly from
`skel/lib/scripts/install-remote.sh`.

**Post-install:** run `teamster setup tags` to configure the tag keyspace
via the TUI wizard (8-screen guided flow on first run, 3-column editor
on subsequent runs). Or use `teamster tags add-key`/`add-value` for
non-interactive setup.

## Test

```bash
cd src && go test ./...        # unit tests (wms engine, store, hook enrichment)
go vet ./...
# clean install: reset a disposable test VM, then run ./install.sh there
skel/lib/scripts/selftest.sh   # 7 automated checks via claude --print
skel/lib/scripts/wms-smoketest.sh
```

Store and migration tests SKIP (vacuous green) unless `TEAMSTER_TEST_MYSQL_DSN`
is set. Dedicated test MySQL at `127.0.0.1:13306` (root/test). The DSN must
point at a server-level connection (no database name) for the per-test-schema
harness to work.

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
| `teamster` (Go) | Hook client + CLI on the hub | Forked per hook event. Reads JSON from stdin, enriches, POSTs to hookd. Must exit 0 always. Also serves as the CLI: `start`/`stop`/`status`/`wms-reset`/`tags`/`setup tags`/`wms drain`/`wms list`/`wms close`/`install-remote`/`backup`/`restore`. |
| `teamster.py` (Python) | Hook client on remotes | Pure stdlib, no third-party deps. Same wire contract as Go version. On macOS, derives teammate identity from the transcript's `agentName` (sets `agent_type` when the payload lacks one) and echoes hookd's `additionalContext` on PreToolUse **and** UserPromptSubmit. `TEAMSTER_DEBUG_RAW=1` dumps raw hook stdin to `var/raw-hook-debug.jsonl`. |
| `hookd` | HTTP event server | POST `/event` → JSONL append. Serves dashboard at `/`, WMS at `/wms`, SSE at `/events/stream`. Focus-absent nudge on PreToolUse (max 1 per session+agent per turn). Returns activity + team-dispatch `additionalContext` on UserPromptSubmit so remote Python clients get the nudge (the hub Go client ignores it — no double-inject; hookd can't see a remote's solo/team marker, so it always sends team context). |
| `feed` | Terminal activity viewer | Tails JSONL, ANSI colorizes. Built from `cmd/feed/`. |
| `activity-mcp` | MCP stdio (activity) | **No-op.** Tools just return confirmation strings. Real data extraction happens in the hook from PreToolUse payloads — that's how we get `agent_type` for teammate attribution. Includes `setMode` for session mode switching. |
| `wms-mcp` | MCP stdio (WMS) | Outcome/WorkUnit CRUD, tags, focus, dependencies over MySQL/MariaDB via `TEAMSTER_STORE_DSN`. State changes posted to hookd via `HookObserver` when `TEAMSTER_HOOK_SERVER_URL` is set. |
| `rollup` | Cost-attribution pipeline | Allocates token spend to WMS entities via focus intervals. Flags: `--reallocate`, `--recover-focus`, `--recover-warmup`, `--recover-gaps`, `--recover-directives` (focus-less remote teammates → entity named in their dispatch brief), `--repair-focus-intervals` (one-time fix of negative-width focus intervals from the dual-writer/async race), `--synthesize-remote-orphans` (remote sessions with no focus/directive/transcript → temporal correlation with concurrent focused sessions on the same host), `--synthesize-focus <file>`, `--sweep` (chains all deterministic passes), `--sweep-llm` (adds LLM-assisted synthesis), `--count-orphans` (print processable orphan count; checks local transcript existence). Reversible: `--unrecover`, `--unrecover-warmup`, `--unrecover-gaps`, `--unrecover-directives`, `--unrepair-focus-intervals`, `--unsynthesize-remote-floor`, `--unsynthesize`. |
| `classify` | Interval phase + work-type classifier | Derives `phase` and `work-type` tags on intervals/workunits from rule-based signals. Recovers missing required lifecycle tags on work units (safety net for dispatch gaps). Run by systemd timer every 5 min. `--reclassify` re-derives from scratch. `--dry-run` logs lifecycle recovery intent without writing. |
| `token-scraper` | Session transcript scraper | Reads Claude Code session JSONL transcripts and POSTs per-message token usage to hookd's `/telemetry` endpoint. |
| `teamster-install` | Installer | Called by `lib/installrunner.sh`. Explicit `--basedir/--repo/--builddir` flags — no path inference. |
| `demogen` | Synthetic data generator | Creates correlated demo data across token_ledger/wms_intervals/usage_attribution/entity_tags/cost_rollup. `--clean` for teardown. |
| `backup` | Backup engine | Standalone binary for systemd timer. Takes timestamped snapshots of MySQL, OTel, and teamster config/state. No sudo. CLI also accessible via `teamster backup` and `teamster restore`. |
| `relay` | Event relay | Tails hub JSONL, POSTs each line to replica hookd `/event`. Built by installer when `--relay-mode=install`. See `docs/specs/replication.md`. |
| `teamster tags` | CLI tag management | Subcommands: `list`, `add-key`, `add-value`, `retire`, `describe`. Built into `cmd/teamster/tags.go`. |
| `teamster setup tags` | TUI tag wizard | Bubbletea-based guided setup for tag keyspace. 8-screen wizard on first run, 3-column editor on subsequent runs. Built from `internal/tui/`. |

### Systemd timers

| Unit | Schedule | Purpose |
|------|----------|---------|
| `teamster-rollup.timer` | Every 10 minutes | Runs `rollup --sweep` (full deterministic pipeline: entity hygiene + attribution recovery + aggregation) |
| `teamster-classify.timer` | Every 5 minutes | Runs `classify` to derive phase/work-type |
| `teamster-sweep.timer` | Every hour | Runs `claude --print /teamster:sweep` for LLM-assisted synthesis, gated on `--count-orphans` (skips when nothing to process) |
| `teamster-backup.timer` | Configurable (default 1h) | Runs backup to snapshot all stores |

## Key conventions

- **Display strings use `__param__` markers** for dynamic values. The source
  decides what's a parameter; the renderer styles it. Never pattern-match
  verb prefixes at display time. (`__file__`, `__pattern__`, `__id__`.)
- **Entity naming**: `@agent`, `#team`, `<model>` — colorized in the stream.
- **Tag taxonomy** (see `skel/doc/specs/semantic-conventions.md` §3 for the
  full table): GOAL/THNK/DONE come from MCP activity tools; READ/EDIT/GREP/ACT/
  EXEC/TEAM/COMM/TASK/WEB/ASK/PLAN come from tool names; TOOL is the fallback.
- **MCP no-op + hook extraction**: MCP tools are callable surface area for
  the model only. The hook client pulls the actual args out of PreToolUse.
  This is how we attribute MCP calls to teammates (MCP servers can't see
  `agent_type`, hooks can).
- **JSONL is the contract** between hook client and feed. Enriched fields
  (`_tool_tag`, `_tool_display`, `_focus`, `_bash_cmd`, `_agent_name`) must
  be kept in sync across both sides of any change.
- **WMS entity model**: Outcome → WorkUnit (two-level). Both share
  statuses: `pending`, `active`, `review`, `done`, `blocked`.
- **Tag keyspace** uses `product` and 8 work-scope slug keys (`feature`, `bug`, `refactor`, `infra`, `docs`, `research`, `test`, `admin`) as core context keys. Slug keys are facets of `work-type` and share the `work-scope` exclusion group.
  Integration key namespaces (`github.*`, `jira.*`, etc.) are seeded at setup time via
  `teamster setup tags`. Phase/resolution/lifecycle keys have single cardinality.
- **Focus nudge**: hookd checks for any focus interval (open or closed) on
  PreToolUse and injects `additionalContext` if missing. Max 1 nudge per
  (session, agent) per turn. Cache-backed (`src/internal/server/nudge.go`).
- **The protocol lives in the plugin**, not in code. The Eight Rules and
  Field Guide at `skel/lib/plugin/skills/bootstrap/references/` are the canonical
  source. `/teamster:start` is the front door; `/teamster:bootstrap` boots
  a team; `/teamster:solo` starts subagent mode.

## Pitfalls (collected from prior incidents)

- MCP config lives in `~/.claude.json` (`claude mcp add-json --scope user`),
  **not** `~/.claude/mcp.json` — that path is not read by Claude Code.
- `SubagentStart` / `SubagentStop` hooks do **not** fire for Agent Teams.
  On the **hub/Linux**, teammate events appear as regular tool calls within the
  lead's session_id, and identity comes from the `agent_type` field on hook
  payloads.
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
- Lead vs teammate hook semantics differ:
  - Lead: PreToolUse only for Bash; PostToolUse for other tools.
  - Teammate: Pre **and** Post fire for all tools — must dedup (file at
    `/tmp/claude-dedup/{session}.tool`, key includes tag).
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
- Store/migration tests SKIP (vacuous green) unless `TEAMSTER_TEST_MYSQL_DSN`
  is set. The dedicated test MySQL instance is at `127.0.0.1:13306` (root/test).
  The DSN must point at a server-level connection (no database name) for the
  per-test-schema harness to work — a pre-existing base database hides bugs.
- `ts` in JSONL is an RFC3339 string, not an epoch float. Float64 mis-decode
  silently zeroes all classifier signals.
- Migration races: 5 callers can race `migrate()` on a fresh DB. The fix uses
  an advisory lock over the whole migration loop + `information_schema` column
  guards. Must work on both MySQL 8.0 and MariaDB 11.8.
- The hub's Grafana is `mode=external` (shared with other apps). Teamster is a
  tenant. The installer must never auto-restart a shared `grafana-server`.

## Documentation — what to trust

| File | Status | Use for |
|------|--------|---------|
| `README.md` | **Current** — user-facing quick start | What Teamster is, install, first team, dashboard, subagent-mode opt-in |
| `docs/specs/REMOTE-INSTALL.md` | **Current** | Hub/remote install model |
| `docs/specs/replication.md` | **Current** | Hub→replica replication topology (relay + repl-push) |
| `docs/wizard.md` | **Current** | Guided interactive installer (`install.sh`) + tag setup TUI + per-project subagent-mode opt-in |
| `docs/session-explorer-guide.md` | **Current** | 9-point primer for driving programs via tmux |
| `skel/doc/specs/architecture.md` | **Current** — full system | Hub/remote topology, all components, data flows, env vars, operating modes, cost attribution, focus nudge |
| `skel/doc/specs/wms-dashboard-spec.md` | Forward-looking + implemented | What `/wms` should become (phases 2/3 not built) + implemented pages (cost-flow, tags, Grafana dashboards) |
| `skel/doc/specs/semantic-conventions.md` | **Current** | JSONL field conventions, tag taxonomy, WMS entity types, state machine, session mode / `setMode` signal, cost attribution methods, close-out warnings, two-focus distinction |
| `skel/lib/plugin/skills/bootstrap/references/eight-rules.md` | **Canonical protocol** | The Eight Rules |
| `skel/lib/plugin/skills/bootstrap/references/field-guide.md` | **Canonical lessons** | Practical operating and development lessons |
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
