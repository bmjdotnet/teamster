# Teamster Architecture

## Overview

Teamster is a Claude Code Agent Teams overlay providing observability, workflow
enforcement, and work management. It installs to `~/teamster/` via `./install.sh`.

Three things Teamster provides:

1. **Observability** — real-time activity stream (`feed`, web dashboard) showing
   what every agent is doing, thinking, completing.
2. **Workflow enforcement** — the Eight Rules + slash commands teach the lead how
   to decompose work, name agents, route by affinity, and verify autonomously.
3. **Work management** — Outcome → WorkUnit hierarchy in MySQL, exposed via the
   `wms` MCP server. Scheduled sweep recovers unallocated cost attribution.

---

## Deployment Topologies

### Single-host (simple case)

All components run on one machine. Claude Code sessions on that machine talk to
a local hookd. This is the default out of `./install.sh` with no flags.

```
┌─────────────────────────── single host ───────────────────────────────┐
│                                                                        │
│  Claude Code session                                                   │
│    ├─ [hooks] → teamster (Go)  ──POST /event──→  hookd :9125          │
│    ├─ [MCP stdio] activity-mcp                                         │
│    └─ [MCP stdio] wms-mcp  ──→  MySQL  ──HookObserver──→  hookd       │
│                                                                        │
│  hookd  →  events.jsonl  →  feed (terminal viewer)                    │
│         →  SSE dashboard (browser)                                     │
│         →  /wms page (WMS hierarchy)                                   │
│         →  /mcp/roster (agent roster MCP, 7 tools)                     │
│         →  /mcp/health (agent health MCP, 4 tools)                     │
│         →  /metrics (Prometheus)                                       │
│                                                                        │
│  health-collector (daemon, 15s poll) → agent_health_gauge (MySQL)      │
│                                                                        │
│  systemd timers:                                                       │
│    teamster-rollup.timer   → rollup --sweep (full deterministic sweep)│
│    teamster-classify.timer → classify (phase/work-type tagging)       │
│    teamster-sweep.timer    → claude --print (LLM sweep, gated)       │
│    teamster-backup.timer   → backup (snapshot MySQL, OTel, config)    │
└────────────────────────────────────────────────────────────────────────┘
```

### Hub/remote (production)

One **hub** runs all server-side components. One or more **remotes** each run
only the Python hook client. No daemons, databases, or Go binaries on remotes.

```
┌────────────────────────── hub ─────────────────────────────────────┐
│                                                                     │
│  hookd :9125                                                        │
│    POST /event          ← hook events from hub AND remote clients   │
│    POST /mcp/activity   ← JSON-RPC 2.0 from remote Claude sessions  │
│    POST /mcp/wms        ← JSON-RPC 2.0 from remote Claude sessions  │
│    POST /mcp/roster     ← agent roster queries (7 tools)             │
│    POST /mcp/health     ← agent health queries (4 tools)             │
│    GET  /               ← SSE dashboard (htmx)                     │
│    GET  /events/stream  ← SSE feed of events.jsonl                 │
│    GET  /wms            ← WMS hierarchy page                       │
│    GET  /wms/cost-flow  ← Sankey cost-flow visualization           │
│    GET  /wms/tags       ← Tag browser (collapsible key groups)     │
│    GET  /wms/api/cost-flow ← JSON API for cost-flow data           │
│    GET  /wms/api/tags   ← JSON API for tag data                    │
│    GET  /health         ← {"status":"ok"}                          │
│    GET  /metrics        ← Prometheus metrics                       │
│                                                                     │
│  MySQL (WMS store) activity-mcp (stdio, hub sessions only)          │
│  wms-mcp (stdio, hub sessions only)                                 │
│  events.jsonl      feed (terminal viewer)                           │
│  health-collector (daemon, polls token_ledger → health gauges)      │
│  supervisor (manages hookd + optional monitoring bundle)            │
│                                                                     │
│  systemd timers: rollup, classify, sweep, backup                    │
└─────────────────────────────────────────────────────────────────────┘
         ▲                         ▲
         │ HTTP POST /event        │ HTTP POST /mcp/*
         │                         │
┌──── remote A ────┐    ┌──── remote B ────┐
│ teamster.py      │    │ teamster.py      │
│ (Python hook     │    │ (Python hook     │
│  client, fired   │    │  client)         │
│  per hook event) │    │                  │
│                  │    │ MCP: HTTP → hub  │
│ Claude Code      │    │ Claude Code      │
└──────────────────┘    └──────────────────┘
```

On a remote, `TEAMSTER_HOOK_SERVER_URL` and the MCP endpoints point at the hub.
`TEAMSTER_HOST` carries the short hostname so the hub can attribute events to
the right machine.

A remote that also runs the Codex CLI is wired the same way: Codex's
`config.toml` gets `url =`-form MCP servers pointed at the same hub, and a
Python `codex-scraper` (cron/launchd) tails Codex's own rollout JSONL. See
"Codex runtime (second runtime)" below for the full remote-Codex data flow.

**Hub URL uses the hub's hostname, not `localhost`.** The installer writes
`TEAMSTER_HOOK_SERVER_URL` as `http://<hub-hostname>:9125/event` (from
`os.Hostname()`, falling back to `localhost` only if the hostname can't be
resolved). hookd binds all interfaces (`0.0.0.0:9125`), so a hostname URL serves
both hub-local sessions and remote clients — and `teamster install-remote`
derives the remote's `--server` from this value, so the default is already
remote-reachable. A reinstall heals a stale `localhost`/`127.0.0.1` value but
preserves a real hostname/FQDN or an explicit `--hookd-endpoint`. See
`hubHost()` / `isStaleLocalhostURL` in `src/cmd/teamster-install/`.

**macOS is a remote-only platform.** The hub installer hard-fails on Darwin;
macOS hosts join only as remotes (`teamster install-remote <user>@<mac>` from
the Linux hub). Two macOS-specific behaviors matter:

- The token-scraper runs as a **launchd LaunchAgent** (cron on macOS needs Full
  Disk Access), with `ProgramArguments` invoking the **absolute** `python3`
  resolved at install time (launchd has a minimal PATH).
- **Teammates run as separate top-level sessions** with no `agent_type` in their
  hook payloads — identity is derived from the transcript's `agentName` (see
  *Remote teammate identity derivation* below).

### Replica (read-only mirror)

A **replica** is a third topology: a full read-only stack that mirrors a
hub's data outward for read-only consumption (DR/standby, staging,
stakeholder dashboards, public demo). Unlike a remote — which only feeds
events *into* a hub — a replica runs its own hookd, MySQL, Prometheus, and
Grafana, all read-only. The hub pushes; the replica never initiates a
connection back, so even a compromised replica cannot write to the hub.

```
┌──────── hub (internal) ────────┐         ┌──── replica (DMZ) ────────┐
│  hookd → events.jsonl          │         │  hookd --read-only         │
│  MySQL :3306                   │         │    (accepts /event,        │
│                                │ relay   │     serves GET routes,     │
│  relay ─ tails events.jsonl ───┼────────▶│     rejects MCP/telemetry) │
│          POST /event          │         │  MySQL replica :3306       │
│                                │ repl-   │  Prometheus (local scrape) │
│  repl-push-server ─ mysqldump ─┼────────▶│  Grafana (anonymous)       │
│          + SCP + binlog pos    │         │                            │
└────────────────────────────────┘         └────────────────────────────┘
```

Two data planes flow hub → replica:

- **Live events** — the `relay` binary (`cmd/relay/`) tails the hub's
  `events.jsonl` and POSTs each line to the replica hookd's `/event`. This
  drives the SSE dashboard and the replica's own JSONL.
- **MySQL data** (WMS, cost attribution, tags) — the repl-push pipeline
  (`repl-push-server.sh` on the hub, `repl-push-client.sh` on the replica)
  bootstraps the replica database with `mysqldump` + SCP, then native
  MySQL/MariaDB replication keeps it in sync.

The relay and repl-push-server run on the **hub** (installed via
`--relay-mode=install`). The replica runs hookd with `--read-only`
(`TEAMSTER_HOOKD_READ_ONLY=1`), a Prometheus that scrapes its own hookd
`/metrics`, and an anonymous-access Grafana. See
`docs/specs/replication.md` for the full specification, flags, environment
variables, and service templates.

---

## Component Map

```
Claude Code session (hub-local)
  │
  ├─[PreToolUse/PostToolUse/Stop/UserPromptSubmit/SubagentStart/
  │  SubagentStop/TeammateIdle/TaskCompleted hooks]
  │   └─→ ~/teamster/bin/teamster   (Go, forked per hook event)
  │           ├─ reads hook JSON from stdin
  │           ├─ extracts agent identity (agent_type → @name)
  │           ├─ maps tool_name → _tool_tag + _tool_display
  │           ├─ injects additionalContext (activity reporting reminder)
  │           ├─ enforces guardrail (orphan Agent dispatch in team mode)
  │           └─→ POST http://localhost:9125/event
  │
  ├─[MCP stdio: activity tools]
  │   └─→ ~/teamster/bin/activity-mcp
  │           ├─ reportActivity(type, message) → confirmation string (no-op)
  │           ├─ setOverallIntent(message)     → confirmation string (no-op)
  │           ├─ completeActivity(message)     → confirmation string (no-op)
  │           └─ setMode(mode)                 → confirmation string (no-op)
  │           (real data extracted from PreToolUse payload by hook client)
  │
  └─[MCP stdio: WMS tools]
      └─→ ~/teamster/bin/wms-mcp
              ├─ createOutcome / getOutcome / listOutcomes / updateOutcomeStatus
              ├─ createWorkUnit / getWorkUnit / listWorkUnits / updateWorkUnitStatus
              ├─ assignWorkUnit / claimWorkUnit / classifyEntity / listRelated
              ├─ updateStatus / setFocus / getFocus / getHistory / getTimeline
              ├─ addDependency / removeDependency / listBlockers / listDependents
              ├─ tagEntity / untagEntity / listTags / defineTag / retireTag
              ├─ describeTag / setPhase / snapshotEntityTags / rollbackTags
              └─→ MySQL (via internal/store/mysql/)
                  └─→ HookObserver → POST http://localhost:9125/event
                       (status + focus changes appear in the activity stream)

Claude Code session (remote)
  │
  ├─[hooks] → ~/teamster/bin/teamster  (Python script, teamster.py)
  │               └─→ POST http://<hub>:9125/event
  │
  ├─[MCP HTTP: activity]  → POST http://<hub>:9125/mcp/activity  (JSON-RPC 2.0)
  └─[MCP HTTP: WMS]       → POST http://<hub>:9125/mcp/wms       (JSON-RPC 2.0)

~/teamster/bin/hookd  (HTTP event server, always on hub)
  ├─ POST /event           → enrich → append to ~/teamster/var/events.jsonl
  │                          → SSE publish to dashboard subscribers
  │                          → session tracker / entity count updates
  │                          → focus-nudge check (injects additionalContext
  │                            if agent has no focus interval (open or closed), max 1/session+agent/turn)
  ├─ GET  /health          → {"status":"ok"}
  ├─ GET  /                → SSE activity dashboard (htmx, streaming HTML)
  ├─ GET  /events/stream   → SSE feed (raw JSONL rendered as HTML divs)
  │                          ?history=N replays last N lines before subscribing
  ├─ GET  /wms             → WMS hierarchy page (reads MySQL store read-only)
  ├─ GET  /wms/cost-flow   → Sankey cost-flow visualization (3 views)
  ├─ GET  /wms/tags        → Tag browser (collapsible key groups, entity counts)
  ├─ GET  /wms/api/cost-flow → JSON cost-flow data
  ├─ GET  /wms/api/tags    → JSON tag data
  ├─ GET  /metrics         → Prometheus metrics (default registry)
  ├─ POST /mcp/activity    → JSON-RPC 2.0 activity MCP (for remote sessions)
  ├─ POST /mcp/wms         → JSON-RPC 2.0 WMS MCP (for remote sessions)
  ├─ POST /mcp/roster      → JSON-RPC 2.0 roster MCP (7 tools: agent roster,
  │                           liveness, registration, token verification;
  │                           registerPeer propagates a set team_name onto
  │                           the session row and sibling roster entries
  │                           already bound to that session_id)
  └─ POST /mcp/health      → JSON-RPC 2.0 health MCP (4 tools: agent health
                              snapshots, team summaries, pressure alerts)

~/teamster/bin/feed         → tail ~/teamster/var/events.jsonl, ANSI render

~/teamster/bin/rollup       → cost-attribution pipeline (systemd timer)
  ├─ allocates token spend to WMS entities via focus intervals
  ├─ --recover-focus: transcript-based recovery of unallocated messages
  ├─ --recover-warmup: admin-phase warmup capture
  ├─ --recover-gaps: deterministic lead/teammate gap resolution
  ├─ --sweep: chains all deterministic passes
  └─ --sweep-llm: adds LLM-assisted synthesis pass

~/teamster/bin/classify     → interval phase + work-type classifier (systemd timer)

~/teamster/bin/codex-scraper → Codex rollout-JSONL cost/ledger tailer (systemd timer, oneshot)
  ├─ tails ~/.codex/sessions/**/rollout-*.jsonl (+ archived_sessions/)
  ├─ POST hookd /telemetry → token_ledger rows (runtime='codex')
  ├─ upserts the Codex sessions row via a direct store connection
  └─ books thread_spawn subagent spend under the parent session (@<role>)

~/teamster/bin/health-collector → agent health gauge collector (hub daemon, 15s poll)
  ├─ polls token_ledger for per-agent token usage (E2 exception: direct SQL read)
  │   and per-agent cost (session total shown on the lead/team header)
  ├─ context window: Claude Code's own StatusLine report when available;
  │   for Agent-Teams teammates (no StatusLine channel), derived from the
  │   teammate's own subagents/ transcript + .meta.json sidecar; falls back
  │   to a model-class table, then the lead's window (same model only)
  ├─ resolves roster_id per agent via store.ResolveRosterID
  └─ writes agent_health_gauge rows via GaugeStore.Upsert (overwrite semantics)

~/teamster/bin/backup       → timestamped snapshots of MySQL, OTel, and teamster config/state
  ├─ no sudo required (uses --defaults-extra-file for MySQL, DSN from teamster.yaml)
  ├─ Prometheus disabled by default (ephemeral data)
  ├─ Grafana.db skipped in external mode
  └─ retention policy applied after each run

supervisor process
  ├─ manages hookd as child (when TEAMSTER_HOOKD_MODE=supervisor)
  └─ manages optional monitoring bundle (otelcol, Prometheus, Grafana)
       TEAMSTER_BUNDLE=all|otelcol|prom|grafana selects components
```

---

## Data Flows

### Hub hook event (tool use, hub-local session)

```
Claude Code hook fires
  → stdin JSON to ~/teamster/bin/teamster (Go)
  → ProcessEvent(): agent_type, tool_name, tool_input → tag + display text
  → additionalContext injection (activity reminder)
  → POST http://localhost:9125/event  (enriched JSON)
  → hookd enriches (if needed), appends JSONL line to var/events.jsonl
  → hookd focus-nudge check: if PreToolUse + no focus interval (open or closed),
    injects additionalContext nudge (max 1 per session+agent per turn)
  → hookd SSE-pushes rendered HTML div to dashboard subscribers
  → feed reads new JSONL line, renders ANSI to terminal
```

### Remote hook event (tool use, remote session)

```
Claude Code hook fires on remote
  → stdin JSON to ~/teamster/bin/teamster (Python: teamster.py)
  → adds host field from TEAMSTER_HOST or socket.gethostname()
  → POST http://<hub>:9125/event  (2s timeout, silently drops on failure)
  → hub hookd appends to events.jsonl (same pipeline as hub events)
  → appears in hub feed with remote hostname in host field
```

### Remote MCP call

```
Claude Code on remote calls reportActivity / wms_createOutcome / etc.
  → MCP transport opens HTTP connection to hub
  → POST http://<hub>:9125/mcp/activity  or  /mcp/wms  (JSON-RPC 2.0)
  → hookd dispatches to mcpactivity or mcpwms handler package
  → wms-mcp handler writes to MySQL, HookObserver posts status change event
  → response JSON-RPC result returned to remote Claude Code
```

### Remote teammate identity derivation (macOS)

```
On the hub/Linux: teammate hook payloads carry agent_type inline; teammates
  share the lead's session_id. Identity = _agent_name = "@" + agent_type.

On macOS: each teammate is a SEPARATE top-level session — own session_id, own
  ~/.claude/projects/<proj>/<session>.jsonl transcript (NOT under subagents/),
  and hook payloads carry NO agent_type. Identity lives only in the transcript's
  top-level "agentName" field. Two clients compensate:

  teamster.py (hook client, fork-per-event):
    if payload has no agent_type but has transcript_path:
      scan transcript head (≤256 KB) for first non-empty "agentName"
      set event["agent_type"] = agentName   → hookd resolves @<name> in feed

  token-scraper.py (long-running, per-poll):
    for each top-level session transcript:
      agent_name = "@" + agentName from transcript head (≤256 KB)
      attribute that session's cost to agent_name (not the lead)
      memoise per-process, NON-EMPTY only (a not-yet-written agentName retries
      next poll rather than permanently misattributing to the lead)

  Both scans are best-effort and never raise.
```

### Remote UserPromptSubmit context (nudge parity)

```
Hub Go client: injects activity + team-dispatch text locally from constants;
  ignores hookd's additionalContext response field (no double-injection).

Remote Python client: has no copy of that text. So hookd returns it:
  on UserPromptSubmit, hookd sets resp["additionalContext"] =
    ACTIVITY_INSTRUCTION + TEAM_DISPATCH_INSTRUCTION
  teamster.py echoes it as hookSpecificOutput.additionalContext
    (echoes on PreToolUse AND UserPromptSubmit)

Limitation: hookd cannot observe a remote session's solo/team marker (it is
  client-local state, never sent over the wire), so remote UserPromptSubmit
  always receives TEAM context. Least-harm default: common remote case is team,
  and the text is guidance, not enforcement.
```

### WMS status change flow

```
Agent calls wms-mcp tool (e.g., updateOutcomeStatus)
  → wms-mcp Engine validates transition (see transitions.go)
  → writes new status to MySQL
  → HookObserver.OnStatusChange() fires
  → POST http://localhost:9125/event  with hook_event_name=WMSStatusChange
  → hookd dispatchObservability: increments entity counts, WMS metrics
  → event appears in activity stream with [TASK] or [DONE] tag
```

### Cost attribution flow

```
token-scraper runs (cron or systemd timer)
  → reads Claude Code session JSONL transcripts
  → extracts per-message token counts
  → POSTs to hookd /telemetry endpoint
  → hookd writes to token_ledger table

rollup --sweep runs (systemd timer, every 10 min)
  → entity hygiene: drain dangling intervals, reclassify
  → reads token_ledger + wms_intervals (focus intervals)
  → temporal join: message timestamp ∈ focus interval → attribute to entity
  → fallback chain: direct → lead fallback → session fallback → unallocated
  → writes usage_attribution table
  → recovery passes (recover-focus, recover-warmup, recover-gaps)
  → aggregation + reconciliation

classify runs (systemd timer, every 5 min)
  → reads wms_intervals + tool signals
  → derives phase (spec/build/test/review/admin) and work-type (docs/test/infra)
  → writes tags to entity_tags via classifier rules
```

### Codex runtime (second runtime)

Codex CLI sessions are captured as a parallel, identically-shaped stream: every
`sessions` and `token_ledger` row carries a `runtime` column
(`claude_code` / `codex` / `unknown`), so Codex data is never merged with Claude
Code's. Codex support is opt-in and solo (no persistent Agent Teams); a host
with no `codex` binary installs unchanged. Cost and session identity do **not**
flow through the hook pipeline (Codex hook events are an optional channel
WMS/cost must not depend on) — they come from `codex-scraper` tailing Codex's
own rollout JSONL:

```
codex-scraper runs (systemd timer, every 10 min; oneshot, not a daemon)
  → tails ~/.codex/sessions/**/rollout-*.jsonl (+ archived_sessions/)
    from a persisted per-file byte-offset cursor
  → per token_count event: derive cost from last_token_usage
    (cached_input / reasoning_output are SUBSETS, not extra tokens)
  → POST hookd /telemetry → token_ledger row (runtime='codex')
  → upsert the Codex sessions row via a DIRECT store connection
    (hookd's /telemetry never touches sessions; codex-scraper is its sole writer)

rollup --sweep (unchanged) then attributes those ledger rows to WMS entities
  by the same temporal join used for Claude Code cost.
```

**Subagent sessions.** Codex 0.142.x `thread_spawn` subagents write their own
rollout file whose `session_meta.session_id` is the PARENT thread's id (and
`agent_role` names the subagent). codex-scraper books the ledger + sessions
rows under the parent `session_id` with `agent_name=@<role>` (falling back to
the file's own id on 0.137.0, which has no `session_id`); `message_id` is keyed
by the file's own thread id so sibling files never collide. Because the
`sessions` primary key is `(session_id, agent_name)`, the `(parent, @role)` row
coexists with the parent's `(parent, "")` row exactly like a Claude Code
teammate, and rollup's existing temporal join attributes it with no rollup-side
change. See `docs/specs/CODEX-INSTALL.md` and semantic-conventions §10.

**OTEL.** When the monitoring bundle includes the collector, Codex exports its
metrics to a **dedicated** OTLP receiver (`otlp/codex`, default port 4329,
`metrics_url_path: /`), separate from Claude Code's `otlp` receiver. Codex's
metrics are delta-temporality, so the collector runs a `deltatocumulative`
processor before Prometheus; the `transform/source_label` processor tags
`source=codex` to keep the runtime distinguishable.

**Hooks channel.** The optional Codex hooks channel (`SessionStart` /
`PreToolUse` / `PostToolUse` → `codex-hook.py`) is a **feed-only** signal —
WMS and cost attribution never depend on it (they run off codex-scraper and the
wms-mcp `x-codex-turn-metadata` identity).

**Remote Codex.** A remote host running Codex (enrolled via `teamster
install-remote` or a `--hookd-mode=external` client-mode install) gets the
identical data flow, ported to pure-stdlib Python since remotes carry no Go
toolchain:

```
Remote MCP call (WMS/activity tools):
  codex config.toml: [mcp_servers.wms] url = "http://<hub>:9125/mcp/wms"
    (direct HTTP — no proxy, no local MCP process; codex mcp add --url /
     the bare url= form is wire-verified at both the 0.137.0 pin and 0.142.5)
  → same hookd mcpwms handler hub-local Claude Code and remote Claude Code
    already share; x-codex-turn-metadata + clientInfo unchanged by transport

Remote cost/session flow:
  codex-scraper.py (cron every 10 min / launchd on macOS) tails the remote's
    own $CODEX_HOME/sessions/**/rollout-*.jsonl (+ archived_sessions/)
  → POST http://<hub>:9125/telemetry   (ledger rows, runtime='codex')
  → POST http://<hub>:9125/session     (sessions-row upsert — the hub's own
                                         Go codex-scraper now uses this same
                                         endpoint too, not a direct store
                                         connection)
  → rollup --sweep (unchanged) attributes those ledger rows by the same
    temporal join used for hub-local Codex cost

Remote OTEL (only if the hub's own otelcol is running):
  codex [otel] metrics_exporter = { otlp-http = { endpoint =
    "http://<hub>:4329/", protocol = "binary" } }
  → hub's dedicated otlp/codex receiver, same as hub-local Codex metrics
```

See `docs/specs/REMOTE-INSTALL.md`'s "Codex support on remotes" section and
`docs/specs/CODEX-INSTALL.md`'s "Remote Codex support" section for the full
staging layout, flags, and design rationale.

---

## WMS Engine

The Work Management System uses a two-level hierarchy:

```
Outcome  (pending → active → review → done | blocked)
  └─ WorkUnit  (pending → active → review → done | blocked)
```

Both entity types share the same status set: `pending`, `active`, `review`,
`done`, `blocked`. `done` is the sole terminal status for both.

State machines enforce valid transitions (see `src/internal/wms/transitions.go`).
`IsTerminal()` determines whether a status change should emit a `[DONE]` tag
instead of `[TASK]`. `HookObserver` bridges WMS mutations to the activity stream.

WorkUnit completion cascades: when all WorkUnits under an Outcome reach `done`,
the engine automatically transitions the Outcome to `done`. Outcome-to-Outcome
parent-child relationships also cascade upward.

Close-out guards: when an Outcome transitions to `done`, the engine emits
advisory warnings (never blocks) if child work units are non-terminal or no
`resolution` tag is set.

---

## Persistence Layer

`internal/store` defines the backend-neutral persistence surface for all of
Teamster — WMS, sessions, focus intervals, cost attribution, tags, telemetry.
No caller anywhere depends on a concrete backend type; every composition root
(`hookd`, `wms-mcp`, `teamster`, `rollup`, `classify`, `demogen`) constructs a
`store.Store` through the registry and consumes it only through interfaces.

### Backend registry and construction

```go
_ "github.com/bmjdotnet/teamster/internal/store/mysql"  // blank import: registers "mysql", "mariadb"

s, err := store.Open(ctx, dsn, opts...)   // internal/store/factory.go
```

A backend package registers an `OpenFunc` under one or more DSN schemes from
its own `init()` (`store.Register("mysql", Open)`) — the same side-effect-import
idiom `database/sql` drivers use. `store.Open` parses the DSN's scheme,
dispatches to the registered opener, and returns a `store.Store`. A composition
root pulls in only the backend(s) it needs via blank import; there is no
`.DB()` escape hatch and no path that names a concrete backend package
directly outside the backend's own code and its tests.

Two backends exist:

- **`internal/store/mysql`** — the production backend (MySQL/MariaDB via
  `go-sql-driver/mysql`). Schemes `mysql` and `mariadb` (same backend, dual
  driver-string target). Migrations v1–v55 (v52–v54: muster roster/tokens,
  v55: agent health gauge).
- **`internal/store/sqlite`** — a pure-Go backend (`modernc.org/sqlite`, no
  cgo) that exists solely to validate the `Store` contract is truly
  backend-agnostic. It is not exposed as an install-time option (see
  `docs/wizard.md` Q7) — it backs the conformance suite only.

### Role-based sub-interfaces

`store.Store` is the union of `wms.Reader`/`wms.Writer` plus role-based
sub-interfaces, each scoped to one concern and each independently consumable
by a caller that needs only that slice:

`SessionStore`, `IntervalStore`, `MaintenanceStore`, `ActivityStore`,
`StatusStore`, `RelatedStore`, `ClassifierStore`, `TagAdminStore`,
`TelemetryStore`, `AllocationStore`, `RecoveryStore`, `SweepStore`,
`ReportingStore`, `RosterStore`, and the always-present `Prober` (`Ping`).
See `src/internal/store/store.go` for the full method sets — e.g. `rollup`
depends only on `AllocationStore`/`RecoveryStore`, `classify` only on
`ClassifierStore`, without either importing the others.

`RosterStore` manages the `agent_roster` and `agent_tokens` tables — agent
identity, session binding, and bearer-token lifecycle. A separate
`GaugeStore` interface (`internal/agenthealth/gauge/`) owns the
`agent_health_gauge` table — per-agent health snapshots with overwrite
semantics. GaugeStore is deliberately outside `internal/store` (BOUNDARIES
R2: different concern, different package).

### Typed error model

Every backend maps its driver-level errors onto three sentinels
(`internal/store/errors.go`):

| Sentinel | Meaning |
|----------|---------|
| `ErrNotFound` | A lookup or mutation found no matching row |
| `ErrConflict` | A write violated a uniqueness constraint (MySQL 1062, SQLite `SQLITE_CONSTRAINT_UNIQUE`) |
| `ErrPrecondition` | An optimistic/conditional write's guard failed — row existed but wasn't in the expected state |

Callers check with `errors.Is(err, store.ErrNotFound)` etc., never a
driver-specific error string or code. `StoreError` wraps a sentinel with
entity/op context for logs while still satisfying `errors.Is`.

### Portable migration framework

`internal/store/migrate.go` defines a shared, backend-agnostic
`RunMigrations` runner plus the `Migrator` contract each backend implements:
`Lock` (serializes concurrent migration attempts — MySQL uses
`GET_LOCK`/`RELEASE_LOCK`; a single-writer backend like SQLite may no-op),
`CurrentVersion`/`SetVersion` (schema-version bookkeeping), and `Steps`
(the backend's ordered `Migration` list). A `Migration` carries portable SQL,
a backend-specific `Func`, or both; `Func` receives the `Migrator` itself as
its `Execer` so a step cannot escape onto an unlocked connection. The runner
refuses to run against a schema newer than the binary knows (the safeguard
that closes the schema-ahead-of-binary incident class).

### Admin plane

Four capabilities are deliberately **not** part of `Store` — a backend may
legitimately lack them, so callers discover them by type-assertion rather
than a compile-time dependency:

| Interface | Backs | Notes |
|-----------|-------|-------|
| `RawExecutor` | `teamster sql` | Raw exec/query escape hatch; a backend without it fails `teamster sql` cleanly instead of a compile break |
| `BackupEngine` | `backup`/`teamster restore` | Whole-database dump/restore/verify — no finite set of domain calls can express this |
| `DemoSeeder` | `demogen` | Bulk, controlled-timestamp ledger/interval/attribution seeding for synthetic dashboards |
| `CredentialProber` | `teamster status` (grafana_ro check) | Verifies a distinct least-privilege credential authorizes — cannot reuse the store's own connection |

### Conformance suite

`internal/store`'s conformance tests run the identical test bodies against
every registered backend via a `backends()` table (`store_test.go`), so a new
backend either satisfies the same behavioral contract or fails a named test —
never a re-implemented, backend-specific test suite. Six dimensions:

1. **CRUD round-trip** (`conformance_dim1_test.go`) — every entity type
   round-trips through its Store methods unchanged.
2. **Transactions/atomicity** (`conformance_dim2_test.go`) — multi-row writes
   (e.g. `ApplyRecovery`, `BackfillInterval`) are all-or-nothing.
3. **Concurrency/locking** (`conformance_dim3_test.go`) — concurrent writers
   to the same row/interval don't corrupt state (uq_open collisions, migration
   races).
4. **Error sentinels** (`conformance_dim4_test.go`) — each backend raises the
   correct sentinel (`ErrNotFound`/`ErrConflict`/`ErrPrecondition`) for the
   same fault.
5. **Migration lifecycle** — `RunMigrations` behaves identically across
   backends: locking, version gating, ahead-of-binary refusal.
6. **Cross-backend attribution equivalence** (`dim6_test.go`) — the rollup
   allocation algorithm produces the same attribution result whichever
   backend supplies the primitives.

The `sqlite` entry always runs (in-memory, no external server). The `mysql`
entry SKIPs unless `TEAMSTER_TEST_MYSQL_DSN` is set and reachable — see
Pitfalls in the repo's `CLAUDE.md`. `internal/store/storetest` is a shared
harness (per-test schema isolation, `RawExecutor`-based fixture helpers) that
other packages (`internal/rollup`, `internal/server`, `internal/observability`)
use instead of each hand-rolling MySQL setup/teardown.

---

## Observability

### JSONL as contract

`events.jsonl` is the single source of truth. Every hook event, WMS status
change, and focus update is appended as one JSON line. The schema is stable:
both the feed terminal viewer and the SSE dashboard depend on it.

Enriched fields added by the hook client (Go or Python) and by hookd:

| Field | Source | Meaning |
|-------|--------|---------|
| `_tool_tag` | hook client | 16-value tag taxonomy (READ, EDIT, ACT, WARN, ...) |
| `_tool_display` | hook client | Human-readable action with `__param__` markers |
| `_focus` | hook client | Current goal/intent from `setOverallIntent` |
| `_bash_cmd` | hook client | Raw shell command for Bash tool calls |
| `_warn_msg` | server (dispatchObservability) | Operator warning for orphan dispatch (no WMS task tracked) |
| `_agent_name` | hook client | `@`-prefixed agent name from `agent_type` |
| `_host` | hook client | Short hostname (hub or remote) |
| `_model` | hook client | Model identifier from the payload |

### Tag taxonomy (17 tags)

| Tag | Source | Meaning |
|-----|--------|---------|
| `[GOAL]` | `setOverallIntent` MCP tool (PreToolUse) | Agent declares session mission |
| `[THNK]` | `reportActivity` MCP tool (PreToolUse) | Agent declares current turn intent |
| `[DONE]` | `completeActivity` MCP tool, `TaskUpdate(completed)`, or Stop hook | Completion / turn end |
| `[RCAP]` | Phantom `SubagentStop` event (no `agent_type`, recap heuristic) | Idle recap — context summary after inactivity |
| `[READ]` | Read/Glob tool | File read |
| `[EDIT]` | Edit/Write tool | File modification |
| `[GREP]` | Grep tool | File search |
| `[ ACT]` | Bash tool with description field | Agent's intent for a command |
| `[EXEC]` | `Monitor` tool; also display-layer label for `bash_cmd` field | Monitor tool tag; `[EXEC]` display line also renders for ALL Bash tool calls from `bash_cmd` field, independent of tag |
| `[TEAM]` | Agent tool | Agent lifecycle: spawn teammates |
| `[COMM]` | SendMessage tool | Inter-agent communication |
| `[TASK]` | TaskCreate/TaskUpdate/TaskGet/TaskList tool | Task lifecycle |
| `[ WEB]` | WebSearch/WebFetch tool | Web search or fetch |
| `[ ASK]` | AskUserQuestion tool | Question to human operator |
| `[PLAN]` | EnterPlanMode/ExitPlanMode tool | Plan mode entry/exit |
| `[WARN]` | `warn_msg` field (server-side) | Operator warning (orphan dispatch or structural issue) |
| `[TOOL]` | Any unclassified tool | Fallback |

---

## Operating Modes

Teamster supports two runtime collaboration models on the same install. The
mode is **per-project / per-session** — not an install-time choice.

### Agent-Teams mode (default)

The default. When no mode signal is set, all three hook gates enforce the
team mandate: team-dispatch prose is injected, the bootstrap nudge fires, and
bare `Agent` calls are monitored; team-dispatch prose is injected. This is the pre-solo
behavior, byte-identical to any existing install.

### Subagent mode

One primary agent that acts as its own lead. Bare
`Agent` subagents (including ephemeral review subagents) are allowed.
Observability and WMS are fully on; only the team-mandate injection is
suppressed.

#### Session mode marker

The mode for a running session is encoded in a per-session `.mode` file
written by the hook client under `$TEAMSTER_DEDUP_DIR/<sid[:12]>.mode`.
The hook writes the marker when it receives a `mcp__activity__setMode` MCP
signal (from `/teamster:start` or `/teamster:solo`/`/teamster:bootstrap`
directly). The effective mode each event uses is:

```
effectiveSolo = true   if marker reads exactly "solo"
              = false  if marker reads exactly "team"  (beats a TEAMSTER_SOLO=1 env)
              = cfg.Solo (== TEAMSTER_SOLO env)  otherwise
              = false (enforce)  if neither source is set
```

Marker lifecycle: the hook refreshes the mtime on every honored read so an
active session never ages out. A fixed 12-hour TTL (`modeMarkerTTL`)
reclaims markers left by crashed sessions. The marker is NOT removed on the
`Stop` event (which fires per-turn) — it sticks for the whole session.
Garbage, empty, or stale markers are inert and always resolve toward enforcement.

#### Three hook gates (gated on `effectiveSolo`)

All three gates are in `src/internal/hook/hook.go`:

| Gate | Team mode | Subagent mode |
|------|-----------|---------------|
| (a) Team-dispatch prose in `additionalContext` | injected | suppressed |
| (b) Bootstrap nudge | injected when no team | suppressed |
| (c) Bare `Agent` block | hard-block (`decision:"block"`) | silent allow |

Activity reporting (`reportActivity`/`setOverallIntent`/`completeActivity`)
is always on in both modes. The Python remote client has no mandate gates —
remotes are already permissive.

#### Subagent cost attribution

Cost attribution in subagent mode relies on two fixes in
`src/internal/rollup/rollup.go`:

- **P1 (`isAttributable`)** — the lead agent's empty `agent_name` (`""`) is
  now attributable. Before this fix, every lead message short-circuited to
  `unallocated` before `focusAt` ran.
- **P2 (lead-focus fallback)** — a subagent with no own focus interval
  inherits the lead's `""` focus for the same session. The method recorded
  in `usage_attribution.method` is `temporal_join_lead_fallback`. Scoped so
  a named teammate that set its own focus never falls back.

#### Close-out guards

When an Outcome is transitioned to `done`, the WMS engine
(`src/internal/wms/closeout.go`) emits advisory warnings (never a block) if:

- any child work units are non-terminal (pending/active/review); or
- no `resolution` tag is set on the outcome.

Warnings are appended to the MCP tool's success response. This is the engine's
backstop for close-out discipline, surfaced inline so the lead doesn't skip
bookkeeping.

---

## Configuration (environment variables)

All env vars are read by `src/internal/config/config.go`. Defaults shown.

| Variable | Default | Notes |
|----------|---------|-------|
| `TEAMSTER_BASEDIR` | — | Master override: sets DataDir to `BASEDIR/var`, derives all paths |
| `TEAMSTER_DATA_DIR` | `~/teamster/var` | Overrides DataDir only |
| `TEAMSTER_HOOK_SERVER_URL` | `http://localhost:9125/event` | Where hook client POSTs events. Config-level fallback only — the installer writes the hub's **hostname** here (`http://<hub-hostname>:9125/event`), not localhost, so remotes can reach it. |
| `TEAMSTER_HOOK_SERVER_PORT` | `9125` | Port hookd listens on |
| `TEAMSTER_HOOK_SERVER_BIND` | `0.0.0.0` | Bind address for hookd |
| `TEAMSTER_LOG_FILE` | `$DataDir/events.jsonl` | JSONL event log path |
| `TEAMSTER_LOG_LEVEL` | — | Structured log level for slog (debug/info/warn/error) |
| `TEAMSTER_DEDUP_DIR` | `$DataDir/dedup` | Hook client dedup files and session mode markers |
| `TEAMSTER_SESSION_DIR` | `$DataDir/sessions` | Session tracker state |
| `TEAMSTER_STORE_DSN` | — | WMS store DSN: `mysql://user:pass@host:port/db` (required) |
| `TEAMSTER_HOST` | OS hostname | Short hostname for event attribution |
| `TEAMSTER_USER` | OS current user | Username for transcript recovery scoping and `user` tag |
| `TEAMSTER_SESSION_TIMEOUT` | `5m` | Inactivity horizon for session pruning |
| `TEAMSTER_SESSION_SWEEP_INTERVAL` | `30s` | Session sweeper cadence |
| `TEAMSTER_HOOKD_MODE` | `systemd` | `systemd`, `supervisor`, or `external` — who manages hookd |
| `TEAMSTER_BUNDLE` | — | Monitoring bundle: `all`, `otelcol`, `prom`, `grafana` |
| `TEAMSTER_ENV` | `production` | Env label in Prometheus external_labels |
| `TEAMSTER_PROMETHEUS_PORT` | `9190` | Prometheus port (bundle) |
| `TEAMSTER_GRAFANA_PORT` | `3100` | Grafana port (bundle) |
| `TEAMSTER_OTEL_GRPC_PORT` | `4327` | OTel collector gRPC port (bundle) |
| `TEAMSTER_OTEL_HTTP_PORT` | `4328` | OTel collector HTTP port (bundle) |
| `TEAMSTER_PROMETHEUS_RETENTION` | `365d` | Prometheus TSDB retention |
| `TEAMSTER_PROMETHEUS_RETENTION_SIZE` | — | Prometheus TSDB retention size cap (e.g. `50GB`); empty = no cap |
| `TEAMSTER_ATAIL_HISTORY_DEFAULT` | `20` | Default lines of scrollback history for the activity viewer |
| `TEAMSTER_SOLO` | — | `1` = subagent mode pre-seed; see Operating Modes above |
| `TEAMSTER_REQUIRE_TAGS_ON_DONE` | — | `1` = hard close-out enforcement (block transition if tags missing) |
| `TEAMSTER_GC_STALE_HOURS` | — | Stale entity GC threshold in hours |
| `TEAMSTER_REAPER_INTERVAL` | — | Interval between reaper runs (duration string) |
| `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` | `1` | Enables Agent Teams in Claude Code |

Backup configuration lives in the `backup:` section of `teamster.yaml` (merged in by the installer). Key fields: `schedule` (systemd OnCalendar expression), `retention` (keep N snapshots), and per-store enable/disable flags (`mysql`, `otelcol`, `grafana`, `config`). Prometheus is disabled by default (ephemeral data). Grafana.db is skipped when `grafana-mode=external`.

---

## Installation Layout

### Hub layout

```
~/teamster/
├── bin/
│   ├── teamster          (Go hook client + CLI, fired per Claude Code hook event)
│   ├── hookd             (HTTP event server + dashboard)
│   ├── feed              (terminal activity viewer)
│   ├── ctop              (terminal fleet/health/focus/cost dashboard)
│   ├── activity-mcp      (MCP stdio, hub-local sessions)
│   ├── wms-mcp           (MCP stdio, hub-local sessions)
│   ├── rollup            (cost-attribution pipeline)
│   ├── classify          (interval phase + work-type classifier)
│   ├── token-scraper     (session transcript token scraper)
│   ├── backup            (backup engine, run by systemd timer)
│   └── teamster-install  (called by lib/installrunner.sh)
├── var/
│   ├── events.jsonl      (append-only JSONL event log)
│   ├── dedup/            (hook client dedup files + session mode markers)
│   └── sessions/         (session tracker state)
├── etc/
│   ├── teamster-hookd.service    (systemd unit, materialized from template)
│   ├── teamster-rollup.service   (rollup one-shot)
│   ├── teamster-rollup.timer     (rollup timer)
│   ├── teamster-classify.service (classifier one-shot)
│   ├── teamster-classify.timer   (classifier timer, every 5 min)
│   ├── teamster-sweep.service    (sweep one-shot)
│   ├── teamster-sweep.timer      (sweep timer)
│   ├── teamster-backup.service   (backup one-shot)
│   ├── teamster-backup.timer     (backup timer, configurable, default 1h)
│   ├── teamster-codex-scraper.service (Codex rollout tailer one-shot, when Codex wired)
│   └── teamster-codex-scraper.timer   (Codex-scraper timer, every 10 min)
├── lib/
│   ├── .claude-plugin/
│   │   └── marketplace.json     (plugin marketplace root)
│   └── plugin/                  (Claude Code plugin: skills + references)
└── doc/
    └── specs/
```

### Remote layout

```
~/teamster/
├── bin/
│   ├── teamster          (Python hook client: skel/lib/hook/teamster.py)
│   └── token-scraper     (Python token scraper)
└── lib/
    ├── .claude-plugin/
    │   └── marketplace.json
    └── plugin/           (Claude Code plugin: same as hub)
```

No Go binaries. No MCPs. No databases. No daemons. Only the Python hook client,
the token scraper, and the plugin. MCP endpoints point at the hub over HTTP.

---

## Binaries Summary

| Binary | Language | Where | Purpose |
|--------|----------|-------|---------|
| `teamster` | Go | hub | Hook client. Forked per hook event. Reads stdin JSON, enriches, POSTs to hookd. Must exit 0 always. Also the CLI (`start`/`stop`/`status`/`wms-reset`/`tags`/`setup tags`/`wms drain`/`wms list`/`wms close`). |
| `teamster.py` | Python | remote | Hook client on remotes. Pure stdlib. Same wire contract as Go version. |
| `hookd` | Go | hub | HTTP event server. POST `/event` → JSONL. Dashboard, SSE, WMS page, metrics, MCP routes (`/mcp/activity`, `/mcp/wms`, `/mcp/roster`, `/mcp/health`). Focus-absent nudge on PreToolUse. Auto-registers agents on roster from first hook event. Tracks per-agent turn state (processing/idle). |
| `feed` | Go | hub | Long-running terminal viewer. Tails events.jsonl, ANSI colorizes. |
| `activity-mcp` | Go | hub | MCP stdio for activity tools (hub-local sessions). No-op: tools return confirmation strings; real data extracted from PreToolUse by hook client. Includes `setMode`. |
| `wms-mcp` | Go | hub | MCP stdio for WMS CRUD (hub-local sessions). Outcome/WorkUnit lifecycle, tags, focus, dependencies. Writes MySQL, emits status events via HookObserver. |
| `rollup` | Go | hub | Cost-attribution pipeline. Allocates token spend to WMS entities. Recovery passes for unallocated messages. Run by systemd timer. |
| `classify` | Go | hub | Derives phase and work-type tags on intervals/workunits from rule-based signals. Run by systemd timer every 5 min. |
| `token-scraper` | Go | hub | Reads **Claude Code** session transcripts, extracts per-message token usage, writes to token_ledger. Never reads Codex data. |
| `codex-scraper` | Go | hub | Codex rollout-JSONL cost/ledger tailer (systemd timer, oneshot). Sole writer of Codex `token_ledger` rows (via hookd `/telemetry`) and Codex `sessions` rows (direct store). Books `thread_spawn` subagent spend under the parent session as `@<role>`. No-op on hosts with no `codex` CLI. |
| `health-collector` | Go | hub | Agent health gauge collector. Hub daemon, 15s poll interval. Reads `token_ledger` for per-agent token usage and cost, writes `agent_health_gauge` rows. Context window comes from Claude Code's StatusLine when available; an Agent-Teams teammate (no StatusLine channel) gets its window from its own transcript instead, falling back to a model-class table then the lead's window. Resolves `roster_id` per agent. |
| `ctop` | Go | hub | Terminal Bubbletea dashboard over hookd's `/health/api/*` + `/health/stream` (HTTP client only — no DB/store imports), so `--server` can point it at any hub, hub-local or remote. Four views (keys 1–4): health, focus, cost, and fleet — a multi-team tree (team headers, lead + teammates + sub-spawns with tree connectors, collapse/expand) with a live activity log below the grid. Fleet is the default view on launch. |
| `teamster-install` | Go | hub | Called by `lib/installrunner.sh`. Copies binaries, materializes systemd units, merges settings.json. |
| `demogen` | Go | hub | Synthetic data generator for dashboards. Creates correlated demo data. `--clean` for teardown. |
| `backup` | Go | hub | Backup engine. Takes timestamped snapshots of MySQL, OTel, and teamster config/state. No sudo. Also reachable via `teamster backup` and `teamster restore`. Run by systemd timer. |

---

## Grafana Dashboards

Eleven provisioned dashboards in `skel/etc/grafana/dashboards/`:

| Dashboard | File | Purpose |
|-----------|------|---------|
| 01 - AI Spend Explorer | `fd-ai-spend-overview.json` | High-level AI spend overview |
| 02 - Cost Explorer | `fd-cost-explorer.json` | Multi-facet cost drill-down |
| 03 - AI Usage & Effectiveness | `fd-usage-effectiveness.json` | Agent efficiency, model fit, throughput |
| 04.01 - Simple Cost Explorer | `cost-by-tag-value.json` | Cost breakdown by single tag value |
| 04.02 - Multidimension Cost Explorer | `tag-stack-explorer.json` | Composable $stack_level_1/2/3 drill-down by tag key |
| 04.03 - Work Entity Explorer | `entity-cost-explorer.json` | Per-entity cost drill-down |
| 05 - Outcome Accounting | `fd-outcome-accounting.json` | Outcome lifecycle and cost accounting |
| 06 - Outcome Cost Explorer | `outcome-cost-explorer.json` | Per-outcome cost drill-down |
| 07 - Realtime Activity Feed | `activity-feed.json` | Live agent activity stream in Grafana |
| 08 - Claude Code Metrics (OTEL) | `claude-code-metrics.json` | Per-model token usage and cost metrics |
| 09 - Teamster System Health | `fd-data-quality.json` | Data quality and system health |

### Grafana Panel Plugins

Some panels use Grafana plugins not bundled with core Grafana. Teamster's
first such dependency is the **Business Charts** panel
(`volkovlabs-echarts-panel`), which backs the Entity Cost Treemap — core
Grafana has no treemap visualization. The version is **pinned** so the panel's
`pluginVersion` is stable across every install.

Provisioning is **gated on `grafana-mode=install`** (the Teamster-managed
Grafana, which the supervisor starts and owns):

- `lib/installrunner.sh` (`install_grafana_plugins`, runs inside `install_grafana`)
  downloads the pinned plugin zip from `grafana.com` into
  `build/grafana-plugins/<id>/` at install time. Network is required; a failed
  download aborts the install loudly (no silently-broken treemap).
- `teamster-install` clears each staged plugin's destination dir then copies it
  into `BASEDIR/var/grafana/plugins/` (the grafana.ini `plugins` dir). The
  clear-then-copy makes an upgrade a clean replace (a version bump never leaves
  stale files), scoped to the plugin ids Teamster ships — operator BYO plugins
  in that dir are untouched.
- The managed `grafana-server` loads the plugin at **start** (not via reload).
  The plugin ships a PGP-signed `MANIFEST.txt`, so no
  `allow_loading_unsigned_plugins` is needed.

**Upgrade path:** plugins load only at grafana *start*, and the installer's
pre-install `teamster stop` kills the managed grafana. So after staging, when
the supervisor was running before the upgrade (authoritative signal:
`var/pids/teamster.pid` names a live pid), the installer runs `teamster start`
to relaunch the managed bundle — grafana comes up fresh and loads the new
plugin. This is gated to `grafana-mode=install` + `--wire` and only fires if the
supervisor was already running (never auto-starts one the operator hadn't).
`teamster start` is idempotent (each component guarded by `processAlive`).

In **`external`/`managed` mode** Grafana is BYO and Teamster never installs
plugins or restarts a shared instance (see fix 176c562). On such instances the
operator must install `volkovlabs-echarts-panel` themselves for the treemap to
render.

