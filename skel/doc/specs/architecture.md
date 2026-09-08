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
│         →  /mcp/roster (agent roster MCP)                              │
│         →  /mcp/health (agent health MCP)                              │
│         →  /metrics (Prometheus)                                       │
│                                                                        │
│  health-collector (daemon, 15s poll) → agent_health_gauge (MySQL)      │
│                                                                        │
│  systemd timers:                                                       │
│    teamster-rollup.timer   → rollup --sweep (full deterministic sweep)│
│    teamster-classify.timer → classify (phase/work-type tagging)       │
│    teamster-sweep.timer    → claude --print (LLM sweep, gated)       │
│    teamster-backup.timer   → backup (snapshot MySQL, OTel, config)    │
│    teamster-wms-review-sweep.timer → review-sweep (nightly, gated)    │
│    teamster-mcp-scraper.timer → mcp-scraper (events.jsonl → mcp_tool_calls, gated) │
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
│    POST /mcp/roster     ← agent roster queries                       │
│    POST /mcp/health     ← agent health queries                       │
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
│  systemd timers: rollup, classify, sweep, backup, wms-review-sweep, │
│                  mcp-scraper                                        │
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

## Clone (`teamster clone`)

`teamster clone <user>@<host>` stands up a **disposable development peer**:
an independent instance running the exact commit and data of a source
instance (default: the local one), on a target host reached over SSH. It is
not a replica and not a failover — it is writable, standalone, and expected
to be destroyed. `teamster clone` always runs **from** the source host and
pushes outward; it never installs on, migrates, or mutates the source.

The design principle: **instance identity travels, host topology is
re-derived.** The git commit, the DB rows, event history, WMS state, and
cost history all travel to the clone unchanged. Which services are managed
vs. external, DSNs, hostnames, ports, and filesystem paths are never
copied — `clonetopology.Translate` (`internal/clonetopology`) re-derives
them at install time the same way any fresh install does (`os.Hostname()`,
`findFreePort()`, a freshly-generated DSN). Conflating the two is the
feature's primary failure mode; the isolation contract below exists to
prevent it structurally rather than by convention.

### Pipeline

```
teamster clone <user>@<host>
  1. Resolve build (Leg 1, internal/clone)
       local `teamster --version` (or remote GET /health when --source names
       another host) discloses the commit the source instance is actually
       running; re-resolved to a full hash inside --repo-dir or a
       --github-repo fetch (a short hash cannot be expanded without a local
       repo — the clean refusal when the commit never reached GitHub, the
       normal case for day-to-day private development)
  2. Probe target — `printenv HOME` over SSH, once, up front
       every remote path built downstream is absolute (path.Join from this
       value); a literal ~ is never passed into a remote command (see
       "SSH transport" below)
  3. Ship (Leg 2a, internal/clone: dirty.go, ship.go, fingerprint.go)
       git archive <full-hash> → scp tarball → sha256 transport check →
       enforced-fresh extraction (mkdir with no -p: an existing target dir
       fails loudly rather than being silently reused) → content-manifest
       fingerprint gate: git ls-tree blob hashes at the source vs.
       git hash-object over the extracted tree at the target — the real
       provenance check, independent of and in addition to the transport
       checksum. --allow-dirty ships the enumerated dirty fileset instead
       (tracked-modified + untracked-not-ignored, respecting .gitignore) and
       both the CLI summary and the clone's own `Version` string carry a
       -dirty marker (the commit hash itself cannot encode that — it is
       identical for a clean and dirty tree at the same revision).
  4. Translate topology (internal/clonetopology, pure function)
       source teamster.yaml → lib/installrunner.sh flag vector: five
       always-explicit --*-mode=install flags, --store-engine=mysql-8.4,
       --env=clone, --wire; relay/store.dsn/tags are dropped, never
       remapped; refuses an explicit --basedir under any configured
       forbidden prefix (I5)
  5. Invoke the installer over SSH (TEAMSTER_COMMIT/TEAMSTER_VERSION
       exported immediately before `./lib/installrunner.sh "$@"` — without
       this the .git-less extracted tree makes installrunner.sh's own
       `git rev-parse` fail and silently stamp commit "none")
  6. Stage A verify — `<binary> --version` over SSH, prefix-matched against
       the shipped hash. Needs no daemon running (reads the ldflags stamp
       straight off the binary); blocks the data leg on mismatch. The
       installer's own exit code is never trusted as the success signal —
       it exits 0 even when schema migrations fail, and exits 0 having
       started none of otelcol/Prometheus/Grafana on a fresh install.
  7. Assert target schema_version — SELECT MAX(version) must equal this
       binary's own max known migration (installrunner.sh's migration step
       runs as `cmd && printf ok || printf WARN`, non-fatal under
       `set -euo pipefail`, so a clean install exit code alone doesn't
       guarantee the schema actually landed)
  8. Mask teamster-sweep.timer, teamster-backup.timer,
       teamster-wms-review-sweep.timer, and teamster-mcp-scraper.timer
       permanently (systemctl mask, not stop — R11: sweep makes paid
       `claude --print` API calls hourly and would pass its own orphan-gate
       against the restored history; backup would fire on the clone's next
       reboot; review-sweep's failure mode is data loss — a disposable
       clone of production-shaped data must never autonomously park or
       abandon entities in its copy; mcp-scraper masks for a fourth,
       distinct reason — measurement integrity, not money or destructive
       action — a clone must not ledger its own local MCP traffic into
       `mcp_tool_calls` as if it were the hub's, corrupting the volume
       figure that table exists to support)
  9. Move the data (Leg 3, internal/clonedata + cmd/teamster/clone_data.go)
       resolve the source's backup_dir/latest snapshot (pinned by default —
       no write against the source; --fresh-backup opts in to triggering
       one) → transfer candidate files push-only, excluding config.tar.gz
       (I1's actual v1 mechanism — RestoreTeamster silently skips a missing
       config.tar.gz) → stop teamster-rollup.timer / teamster-classify.timer
       / teamster-health-collector.service / teamster-mcp-scraper.timer (all
       four connect to MySQL directly — mcp-scraper to claude_telemetry,
       the other three to the app DSN — and would race a just-migrated,
       still-empty schema; their OnBootSec deadlines are boot-relative and
       have already elapsed by this point in the run, so they fire
       immediately once enabled — not the "10-minute interval, no realistic
       collision" a steady-state install would have; HTTP-only codex-scraper
       is correctly absent from this list, having no direct DB connection of
       its own to race)
       → `teamster restore --force <dir>` → restart those four units →
       verify row counts (SHOW TABLES-driven on both sides, never a
       hand-picked subset; source-side queries run through the
       clone_verify_ro read-only credential, I7 — never the app DSN;
       target-side queries use the target's own already-provisioned app DSN,
       since the target is disposable) → `teamster start` (a fresh install
       never auto-starts hookd or the managed otelcol/prometheus/grafana
       bundle; this is what brings the clone's stack up for the first time)
```

Row-count mismatches are reported, not fatal — the source is live
production, so append-only tables (`token_ledger`, cost facts) and gauge
tables (`agent_health_gauge`) drift between the pinned backup snapshot and a
live query by construction. The acceptance harness
(`scripts/clone-acceptance-test.sh`) is the real correctness gate.

Step 8's mask is not only a clone-time action. `lib/installrunner.sh`'s
`unit_is_masked` guard — applied at every systemd unit-install site, both
through the shared `install_and_enable_unit` helper and hookd through
its own compare-then-sync, not only these four timers — checks every
unit for an existing mask before installing or enabling it and skips with
a logged `left masked: <unit>` line instead of overwriting the `/dev/null`
symlink. A later in-place upgrade on the clone target, or any host with a
hand-masked unit, cannot silently undo it.

### SSH transport layer

Every remote command runs as a real script file, never concatenated argv
handed to `ssh` — `ssh(1)` joins anything past the target with a bare space
and re-parses it in the remote login shell, which is what caused a
recurring class of quoting bugs (PATH prefixes colliding with SQL parens,
tilde expansion breaking under quoting). `internal/clone`'s transport
instead writes the command to a local temp file, `scp`s it to a throwaway
remote path, and runs it as one pre-quoted `bash <path>` string — quoting is
then interpreted exactly once, by the script actually executing.

Two runner shapes cover this:

- **`SSHRunner`** — a single command (`probe`, `systemctl stop`, `teamster
  sql`), built via `scriptForArgs` (each argument individually
  shell-quoted).
- **`SSHScriptRunner`** — a multi-line script with positional parameters
  (the installer invocation, the remote fingerprint reconstruction), passed
  through with a shared `PATH` augmentation so `~/.local/bin`-installed
  tools resolve under SSH's minimal non-interactive `PATH`.

**No literal `~` ever reaches a remote command.** `ProbeTarget` interrogates
the target's real `$HOME` once, at the very start of a run; every path
built afterward (`clone-src/<hash>/`, the installed binary,
`.claude/settings.json`, `clone-data/<hash>/`) is `path.Join`'d from that
absolute value, because SSH does not reliably expand `~` once the argument
carrying it has already been shell-quoted — which the script-based
transport always does.

**Transport is push-only (I6)**: `Uploader` always shells out to `scp` from
source to target, never a pull. This holds even where a shortcut is
available — a target sharing the source's NFS export could read the
source's backup directory directly, which would be faster and would
quietly violate both I6 (no longer a push) and I5 (reaching into
source-owned storage). The implementation always does a genuine copy into a
directory outside the forbidden prefixes.

### MySQL 8.4 provisioning

`lib/installrunner.sh --store-engine=mariadb|mysql-8.4` (only valid with
`--store-mode=install`) selects the store package; `clonetopology.Translate`
always emits `--store-engine=mysql-8.4` for a clone. This is clone-specific,
not a change to the default for ordinary installs, which keep today's
`default-mysql-server` (MariaDB) behavior unchanged.

The reason is fidelity: a source instance can run genuine MySQL 8.0, whose
dumps carry `utf8mb4_0900_ai_ci` — a MySQL-8-only collation baked into the
`CREATE TABLE` statements themselves, which MariaDB does not recognize
(every restore would fail `Unknown collation` on both `teamster` and
`claude_telemetry`). MySQL 8.0 itself is not apt-installable on Debian
trixie (Oracle ships 8.4-LTS/9.5+ only there); MySQL 8.4 LTS is the same
engine one release ahead, is apt-installable, and needs no collation
rewrite. (The rewrite itself — `utf8mb4_0900_ai_ci` → `utf8mb4_general_ci`,
the pattern `repl-push-server.sh` already uses for its MySQL→MariaDB
transition — remains the documented fallback for any target platform that
cannot run genuine MySQL; it is simply not needed on the required path.)

`install_mysql_84()` (`lib/installrunner.sh`): adds Oracle's apt repo via a
dearmored, `signed-by=`-referenced keyring (`apt-key` is fully removed on
trixie), checks the signing key's expiry at runtime and **warns rather than
dies** on an expired key (an unsigned/unverified source is an accepted risk
for the clone target only, never for the source's own trust chain),
installs `mysql-community-server`, then switches `root@localhost` to
`auth_socket` so `sudo mysql` works passwordlessly for the rest of the
installer (mirroring MariaDB's built-in `unix_socket` default — Oracle's 8.4
package doesn't auto-load `auth_socket`, so the installer loads the plugin
first). Database/user creation and grants are shared with the MariaDB path
via `install_mysql()`'s common tail — only the server package install
differs between engines.

`claude_telemetry` is provisioned at install time (`CREATE DATABASE IF NOT
EXISTS`, `utf8mb4_0900_ai_ci` with a plain-`utf8mb4` fallback, `GRANT ALL`
to the app user) — no install path created that database before clone
needed it. The app user also receives `SET_ANY_DEFINER`/`SYSTEM_USER`
(MySQL 8.2+), needed when a restored view or routine's original definer no
longer exists as a user on this server.

### Isolation contract (I1–I7)

A clone that poisons the source defeats the feature's purpose. These are
enforced invariants, not conventions:

| | Invariant | Enforcement |
|---|---|---|
| I1 | Never restore `teamster.yaml` or any `etc/` config verbatim | `config.tar.gz` is never a transfer candidate (`clonedata.TransferCandidates` excludes it) — `RestoreTeamster` silently skips a missing one, so simply not shipping it satisfies I1 |
| I2 | `relay:` / `repl_push_remote` are dropped, never remapped | `clonetopology.Translate` never reads the `Relay` section of the source config |
| I3 | `store.dsn` is regenerated at the target, never copied | Translate never emits `--store-dsn`; `--store-mode=install` with no DSN makes installrunner.sh auto-generate a fresh local one |
| I4 | All five `--*-mode` flags always explicit | Omitting `--store-mode` provisions no MySQL at all; the other four silently skip URL wiring |
| I5 | No clone path under forbidden prefixes | `clonetopology.Translate` refuses an explicit `--basedir` under any prefix listed in `clone.forbidden_basedirs` in `teamster.yaml` (a target may share the source's NFS export) |
| I6 | Transport is push-only, source → target | `Uploader` always `scp`s from source to target; never a pull, never a shared-mount read |
| I7 | The source is read-only for the entire operation | Source-side verification queries run through the dedicated `clone_verify_ro` read-only MySQL credential, never the app DSN |

**`clone_verify_ro`** (`internal/clonedata.CloneVerifyROUser`) is a
least-privilege, `localhost`-scoped MySQL user provisioned on **every**
install that can locally administer MySQL — not only clone targets, since
any source host may later become a clone source and there's no way to know
in advance which one will. `lib/installrunner.sh`'s
`provision_clone_verify_ro` (mirroring the existing `grafana_ro` pattern)
applies `etc/clone-verify-ro-user.sql` and persists the generated password
0600 to `$BASEDIR/var/clone/clone_verify_ro_password`, reused on re-install
so a working credential is never rotated out from under anything holding
it. A missing password file at clone time means the source predates this
fix and must be upgraded — `teamster clone` refuses rather than fall back
to the app DSN.

Two related risks are accepted by explicit ruling rather than closed:
`teamster clone` pins the source's existing `backup_dir/latest` snapshot by
default rather than triggering a fresh backup (itself a write against the
source) — `--fresh-backup` opts in explicitly. And `teamster sql` has no
read-only guard yet — every source-side query `teamster clone` issues is
SELECT-only by discipline pending that follow-up.

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
              ├─ assignWorkUnit / claimWorkUnit / deliverResult / listDeliverables / classifyEntity / listRelated
              ├─ updateStatus / setFocus / getFocus / getHistory / getTimeline
              ├─ addDependency / removeDependency / listBlockers / listDependents
              ├─ addOutcomeParent / removeOutcomeParent
              ├─ addRelation / removeRelation / listRelations / listRelationKinds
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
  ├─ startup: loads the interceptor registry (internal/intercept) —
  │   embedded default + $BASEDIR/etc/interceptors.yaml overlay if present
  │   and valid; a malformed/missing overlay logs a warning and falls back
  │   to the embedded default (never blocks ingest). Also seeds tag display
  │   colors (internal/display) from the same registry.
  ├─ POST /event           → enrich → append to ~/teamster/var/events.jsonl
  │                          → SSE publish to dashboard subscribers
  │                          → session tracker / entity count updates
  │                          → focus-nudge check (injects additionalContext
  │                            if agent has no focus interval (open or closed), max 1/session+agent/turn)
  │                          → WMSStatusChange event, WorkUnit pending→active:
  │                            attempts to auto-open a focus interval for the
  │                            claiming agent, asynchronously and best-effort
  │                            (may decline — see "Claim-success focus
  │                            interval" under Data Flows)
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
  ├─ POST /mcp/roster      → JSON-RPC 2.0 roster MCP (agent roster,
  │                           liveness, registration, token verification;
  │                           registerPeer propagates a set team_name onto
  │                           the session row and sibling roster entries
  │                           already bound to that session_id)
  └─ POST /mcp/health      → JSON-RPC 2.0 health MCP (agent health
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

~/teamster/bin/mcp-scraper  → MCP tool-call telemetry tailer (systemd timer, oneshot,
                               config-gated on MCPScraper.Enabled)
  ├─ tails ~/teamster/var/events.jsonl, filters to completed (PostToolUse)
  │   mcp__* tool calls only — wms-mcp itself never writes telemetry, so
  │   this file is the tailer's sole data source
  ├─ ledgers one row per call into claude_telemetry.mcp_tool_calls (a
  │   call-volume instrument, not an audit trail — no tool_input capture)
  ├─ cursor + generation counter survives copytruncate log rotation;
  │   a lost/corrupt cursor recovers from MAX(source_generation)+1
  └─ single-writer invariant: only the hub's own process ever writes this
      table — teamster-mcp-scraper.timer is permanently masked on every
      `teamster clone` target so a clone never ledgers its own local
      traffic as if it were the hub's (see "Clone" isolation notes above)

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

**`teamster stop` sequence (supervisor-managed components).** `supervisorStop`
first sends `SIGTERM` to the supervisor process itself (from its PID file),
then — before any PID-file or port-based fallback runs — issues `systemctl
stop` for every systemd-managed unit name a supervisor-mode host could be
running (`teamster-hookd`, `teamster-token-scraper`,
`teamster-health-collector`, `teamster-otelcol`, `teamster-prometheus`,
`teamster-grafana`), best-effort: a stop against a nonexistent or inactive
unit just errors, and that error is discarded rather than blocking the rest
of the sequence. Only after this systemd-first pass does it fall back to
directly stopping whatever the supervisor itself actually manages by PID
file (`stopByPidFile`/`killByPort`), which exists to clean up orphans left
by a dead supervisor. This ordering closes an incident (2026-09-05) where a
component with no PID file — the normal state under systemd, which writes
none — fell straight through to `killByPort` and SIGKILLed whatever held
that port behind systemd's back, racing `Restart=on-failure`; it hit
production hookd. `componentSupervisorManaged`/`hookdSupervisorManaged` are
the single eligibility predicates for this decision, called from both the
start and the stop paths, so start and stop can never disagree about which
components a given host actually supervises.

---

## Data Flows

### Hub hook event (tool use, hub-local session)

```
Claude Code hook fires
  → stdin JSON to ~/teamster/bin/teamster (Go)
  → ProcessEvent(): agent_type, tool_name, tool_input → tag + display text
    for built-in tools; for mcp__* tools, only special fields (_focus,
    _thought, _done, mode marker) are set client-side — _tool_tag/
    _tool_display are left for hookd's interceptor registry
  → additionalContext injection (activity reminder)
  → POST http://localhost:9125/event  (enriched JSON)
  → hookd enriches (if needed): EnrichRecord checks the interceptor registry
    first for any mcp__* tool name, falling back to legacy hardcoded blocks
    only on no match; appends JSONL line to var/events.jsonl
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

### Claim-success focus interval (WorkUnit dispatch)

```
Agent calls wms_claimWorkUnit(id)
  → wms-mcp Store.ClaimWorkUnit: atomic CAS on workunits.status/agent_id
      pending              → claim:      status→active, agent_id set, claimed_at stamped
      active, no owner     → adopt:      agent_id set, claimed_at stamped, status unchanged
      active, own owner    → idempotent: agent_id re-set, no status change
      active, other owner  → ErrAlreadyClaimed
      review/done/blocked  → ErrNotClaimable
  → only the pending→active leg calls eng.OnStatusChange (adopt/idempotent
    never change status, so firing one would be a phantom event with a
    fabricated OldStatus)
  → HookObserver posts WMSStatusChange (wms_old_status=pending,
    wms_new_status=active, wms_agent_name, wms_session_id, wms_entity_id)
  → hookd's WMSStatusChange handler detects the pending→active WorkUnit
    transition and calls OpenFocusInterval for the claiming agent —
    success-gated: it only runs after the store CAS has already committed,
    so a claim race's loser (whose UPDATE affected 0 rows) never reaches
    this code and never gets a bogus interval
  → on failure to open: slog WARN + teamster_claim_focus_interval_failures_total
    Prometheus counter + a one-shot wmsWarnings additionalContext nudge
    ("call wms_setFocus manually")
  → the claim response returns the WorkUnit's brief, tags, and claimed_at
```

This converts the named-agent focus-interval slice from voluntary
`wms_setFocus` to mechanical claim-time attribution for the common
"claim a pending WorkUnit" path. Adopt and idempotent re-claim do not emit a
WMSStatusChange event, so they do not get an auto-opened interval — an agent
adopting an unowned active WorkUnit still needs `wms_setFocus`.

### Nightly review sweep (`teamster wms review-sweep`)

```
teamster-wms-review-sweep.timer fires (03:00 nightly, config-gated)
  → cfg.ReviewSweep.Enabled=false → print one line, exit 0 (no-op)
  → cfg.ReviewSweep.Enabled=true:
      Stage 1 — park stale review WorkUnits and idle Outcomes to on_hold
        (idle >= ReviewSweep.OlderThan, default 168h)
      Stage 2 — abandon sweep-parked entities untouched since parking
        (>= ReviewSweep.AbandonAfter, default 720h; never a human-set on_hold)
  → cfg.ReviewSweep.Confirm=false → dry-run: print the per-candidate listing
      and exit 0, writing nothing
  → cfg.ReviewSweep.Confirm=true:
      each disposition → wms.RecordMutation (agent_id="wms-review-sweep")
      each close (not skip) → HookObserver.PostStatusChange
        (SessionID="wms-review-sweep" — never a live session's id)
        → POST hookd /event, hook_event_name=WMSStatusChange
        → hookd: entity-count gauge updates, W2 warning queued for
          wu-review-delivered closes only (newStatus=='done')
      after all writes: CloseIntervalsOnTerminalEntities
        (on_hold is not terminal — Stage 1 parks drain no interval;
         Stage 2 abandons do)
```

This bypasses the WMS engine entirely, the same as `wms_gc.go`/
`wms_close.go` — no dependency-unblock cascade fires for anything blocked
on an entity the sweep closes (a pre-existing, shared gap across all three
engine-bypassing closers, not something this feature introduces). See
`semantic-conventions.md` §4.8 for the full rule-id table, the journal
`notes` grammar, and the `agent_id="wms-review-sweep"` discriminator that
the interlock, the descendant-safety walk, and Sweep Stage 2's own
candidacy queries all read to tell a sweep-set `on_hold` from a
human-set one.

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

classify runs (systemd timer, every 10 min)
  → reads wms_intervals + tool signals
  → derives phase (spec/build/test/review/admin) and work-type (docs/test/infra)
  → writes tags to entity_tags via classifier rules
```

### MCP tool-call telemetry flow

```
mcp-scraper runs (systemd timer, every 10 min; oneshot, config-gated on
MCPScraper.Enabled — no-op exit 0 when disabled)
  → tails ~/teamster/var/events.jsonl from a persisted byte-offset +
    generation cursor (generation increments on a detected copytruncate
    rotation; a lost/corrupt cursor recovers from MAX(source_generation)+1
    against the ledger table itself, never a bare zero)
  → filters to completed (PostToolUse) mcp__* tool-call records only
  → INSERT IGNORE one row per call into claude_telemetry.mcp_tool_calls,
    keyed on (source_generation, source_offset) for dedup
```

This is a call-volume instrument, not an audit trail — no `tool_input` or
entity-id argument is captured, here or anywhere upstream. `hookd`'s own
`events.jsonl` is the tailer's only data source; `wms-mcp` never writes
telemetry directly. The ledger table is created by `lib/installrunner.sh`
at install time only under `--store-mode=install`; under `--store-mode=managed`
or `--store-mode=external` (neither runs install-time DDL) the binary's own
`CREATE TABLE IF NOT EXISTS` is the sole mechanism, and creates it on first
enabled run — which requires the DB credential to hold `CREATE` on
`claude_telemetry`. Single-writer invariant: exactly one `mcp-scraper` process — the
hub's — ever writes this table, which is why `teamster-mcp-scraper.timer`
is permanently masked on every `teamster clone` target (see "Clone" above).

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
Outcome  (pending → active → review → done | abandoned, blocked/on_hold as detours)
  └─ WorkUnit  (pending → active → review → done | abandoned, blocked/on_hold as detours)
```

Both entity types share the same status set: `pending`, `active`, `review`,
`done`, `blocked`, `on_hold`, `abandoned`. `done` and `abandoned` are both
terminal — `abandoned` is a status in its own right (not a `resolution`
tag), for work that was dropped rather than finished. `done → review` is
the sole reopen edge; there is no exit from `abandoned`. `on_hold` is set
either by a human choosing to pause, or by the nightly `teamster wms
review-sweep` timer parking a stale entity — see "Nightly review sweep"
under Data Flows and `semantic-conventions.md` §4.8; the two are
distinguished by who wrote the entity's latest status journal row, and
only a sweep-parked `on_hold` can ever be automatically abandoned.

State machines enforce valid transitions (see `src/internal/wms/transitions.go`).
`IsTerminal()` determines whether a status change should emit a `[DONE]` tag
instead of `[TASK]`. `HookObserver` bridges WMS mutations to the activity stream.

**No automatic close.** The engine does not transition an Outcome to
`done` on its own — not when every WorkUnit under it finishes, and not
when every child Outcome under it finishes (the two auto-close cascades
this design once had were removed: agents don't waterfall projects, and
the full WorkUnit set is rarely known up front). Closing an Outcome is a
deliberate act — a session-based "reason, recommend, ask" step (see
`session-protocol.md` Step 9) or an explicit `teamster wms close`/`wms gc`
call. Creating a WorkUnit under a `done` or `abandoned` Outcome is
rejected, pointing at the `done → review` reopen edge, rather than being
silently accepted.

Close-out guards: when an Outcome transitions to `done` or `abandoned`,
the engine emits advisory warnings (never blocks) if child work units are
non-terminal; a `done` transition also warns if no `resolution` tag is
set (`abandoned` needs no such tag — the status itself carries the
meaning).

### WorkUnit dispatch package (brief, claim, deliver)

Each WorkUnit carries two dispatch-oriented fields beyond title/description:

- **`brief`** (MEDIUMTEXT) — the full dispatch assignment text for the agent
  doing the work. Returned only by `wms_getWorkUnit` and `wms_claimWorkUnit`,
  never by `wms_listWorkUnits`/`wms_listRelated` — list queries never pull a
  potentially large text blob.
- **`claimed_at`** — timestamp of the WorkUnit's first claim or adopt.

`wms_claimWorkUnit` performs an atomic compare-and-swap keyed on the caller's
`agent_id` (from `_meta`, never a client-supplied argument):

| Prior state | Result |
|---|---|
| `pending` | **claim** — status→`active`, `agent_id` set, `claimed_at` stamped, WMSStatusChange fires (see "Claim-success focus interval" under Data Flows) |
| `active`, no owner (`agent_id=''`) | **adopt** — `agent_id` set, `claimed_at` stamped, status unchanged, no status-change event |
| `active`, owned by caller | **idempotent** — no-op re-claim |
| `active`, owned by another agent | `ErrAlreadyClaimed` |
| `review` / `done` / `blocked` | `ErrNotClaimable` |

`wms_deliverResult` is the counterpart: an agent submits its output
(`summary`, full markdown `result`, optional `artifact_paths`), appended to
the `wms_deliverables` table (never overwritten — redelivery is allowed, so a
consumer takes the last row per entity), then transitions the WorkUnit
`active → review`. Only the WorkUnit's owner (or the lead, whose `agent_type`
is empty) may deliver.

### Typed relations (`outcome_relations`)

Distinct from the Outcome-DAG parent/child edges (`wms_addOutcomeParent`/
`wms_removeOutcomeParent`, wrapping the existing `AddOutcomeEdge`/
`RemoveOutcomeEdge` store primitives so decomposition isn't locked to
`wms_createOutcome`-time only): a **relation** records *why* new work exists
relative to prior **delivered** work — a correction edge, not a decomposition
edge. `wms_addRelation` / `wms_removeRelation` / `wms_listRelations` /
`wms_listRelationKinds` (`internal/mcp/wms/`) manage rows in
`outcome_relations`, each carrying a `kind` drawn from `relation_kinds` — a
seeded vocabulary (data, not a Go enum), so a new kind is a migration, not a
schema change. Eight kinds are seeded, four of them taxable and driving
rework-tax reporting; see `semantic-conventions.md` §4.6 for the full
kind-by-kind table (`taxable`/`miss_class`/`lineage`) and the
`phase:iterate`-vs-`outcome_relations` vocabulary split that motivated this
feature.

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
  driver-string target). Migrations v1–v70 (v52–v54: muster roster/tokens,
  v55: agent health gauge, v67–v68: `relation_kinds`/`outcome_relations`
  typed-relations tables, v69: dispatch package — adds `workunits.brief`/
  `claimed_at` and the `wms_deliverables` table, v70: `agent_roster.updated_at`
  for roster-lifecycle sweeping).
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

`wms_deliverables` is an append-only table of agent-submitted
work-completion reports: `id`, `entity_type`, `entity_id`, `agent_id`,
`session_id`, `summary`, `result`, `artifact_paths`, `created_at`, indexed on
`(entity_type, entity_id)`. Written by `wms_deliverResult`; redelivery is
allowed (a reopened or reclaimed WorkUnit can deliver again), so a consumer
reads the last row per entity rather than assuming exactly one.

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
Pitfalls in the repo's `CLAUDE.md`. The test MySQL instance itself is tuned
for suite speed rather than durability (fsync/binlog/doublewrite disabled,
tmpfs datadir — see `scripts/test-with-mysql.sh`) and is treated as fully
disposable: `docker rm -f` and recreate via `--persistent` rather than repair
in place; never point `TEAMSTER_TEST_MYSQL_DSN` at data that needs to
survive. `internal/store/storetest` is a shared
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

### Tag taxonomy (20 tags)

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
| `[TEAM]` | Agent tool; `mcp__roster__*` (registerPeer, listAgents, ...) | Agent lifecycle: spawn teammates, roster events |
| `[COMM]` | SendMessage tool | Inter-agent communication |
| `[TASK]` | TaskCreate/TaskUpdate/TaskGet/TaskList tool; `mcp__wms__*` | Task lifecycle |
| `[ WEB]` | WebSearch/WebFetch tool | Web search or fetch |
| `[ ASK]` | AskUserQuestion tool | Question to human operator |
| `[PLAN]` | EnterPlanMode/ExitPlanMode tool | Plan mode entry/exit |
| `[CHRM]` | `mcp__claude-in-chrome__*`, `mcp__playwright__*`, `mcp__chrome-devtools__*` | Browser automation (3 servers, 1 tag) |
| `[ GIT]` | `mcp__github__*` | GitHub operations |
| `[GDRV]` | `mcp__claude_ai_Google_Drive__*` | Google Drive operations |
| `[WARN]` | `warn_msg` field (server-side) | Operator warning (orphan dispatch or structural issue) |
| `[TOOL]` | Any unclassified tool | Fallback |

`mcp__*` tool tags (`TASK`, `TEAM`, `CHRM`, `[ GIT]`, `GDRV`, and suppressed
tools like `tagEntity`/`untagEntity`/`mcp__health__*`) are defined in
`$BASEDIR/etc/interceptors.yaml`, not hardcoded — see "MCP tool interceptor
registry" below and `semantic-conventions.md` §3.

### MCP tool interceptor registry

`internal/intercept` compiles `$BASEDIR/etc/interceptors.yaml` into a
`Registry` that resolves `_tool_tag`/`_tool_display`/suppress-or-not for
every `mcp__*` tool call — externalizing what used to be per-tool Go logic
hardcoded across `hook.go`/`enrich.go`.

- **Shape**: a `tags:` map (4-char label → RGB color + description) and an
  `interceptors:` list of namespaces, each gated by a `match:` block
  (typically a tool-name `prefix`, e.g. `mcp__wms__wms_`) containing an
  ordered `rules:` list. A rule matches on `method` (the tool name remainder
  after the namespace's prefix is stripped), `suffix`, `exact`, `regex`,
  and/or `params` (tool_input field values), and produces a `tag`, a
  Go-template `display` string (`{{f "field"}}` accessor; `__param__`-style
  markers render cyan in feed), extra `fields` (e.g. `_focus`, `_thought`),
  or `suppress: true` (no feed line emitted at all). A namespace with no
  matching rule falls back to its own `tag`/`display`/`suppress`, or to
  `TOOL` + a generic `server(__method__)` display if it defines neither.
- **Loading**: `intercept.LoadDefault()` builds a Registry from the YAML
  embedded into the `hookd`/`teamster` binaries at compile time
  (`//go:embed interceptors.yaml` in `internal/intercept/config.go` — a
  checked-in copy of `skel/etc/interceptors.yaml`; a test enforces the two
  stay in sync). `intercept.LoadWithOverlay(path)` loads the embedded
  default, then — if `$BASEDIR/etc/interceptors.yaml` exists and parses —
  replaces it wholesale (the file is the full config, not a merge/patch). A
  missing or malformed overlay is not fatal: hookd logs a warning and keeps
  serving off the embedded default, since a cosmetic config file must never
  take down ingest.
- **Validation**: compiling a config hard-fails (surfaced as an `error`,
  which `LoadWithOverlay` turns into "warn + fall back to defaults") on a bad
  tag reference, malformed regex, an invalid RGB/label, or a missing `TOOL`
  tag definition. Shadowed rules (a later rule that can never match because
  an earlier one in the same namespace already covers its cases) are logged
  as warnings only — the config still loads.
- **`teamster check-config`** validates `$BASEDIR/etc/interceptors.yaml`
  standalone (`intercept.Load`, no embedded fallback — a bad file is reported
  as invalid, not silently swallowed) without requiring a hookd restart, so
  an operator can check an edit before applying it.
- **Consumers**: `hook.EnrichRecord` (`internal/hook/enrich.go`) calls
  `Registry.Match(toolName, toolInput)` first for any `mcp__*` tool name; a
  match's `Tag`/`Display`/`Fields`/`Suppress` short-circuits the legacy
  hardcoded `mcp__activity__`/`mcp__wms__`/generic-MCP blocks (kept as a
  Phase 1 fallback per the design doc, pending removal once golden-corpus
  validation lands). `internal/display.SetTagColors` is seeded from the same
  registry's `Tags` map at hookd startup, so a tag defined only in
  `interceptors.yaml` still renders in its declared color.

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

When an Outcome is transitioned to `done` or `abandoned`, the WMS engine
(`src/internal/wms/closeout.go`) emits advisory warnings (never a block) if:

- any child work units are non-terminal (pending/active/review/blocked/
  on_hold) — applies to both `done` and `abandoned`; or
- no `resolution` tag is set on the outcome — `done` only; `abandoned`
  needs none, the status itself carries the meaning.

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
| `TEAMSTER_GC_STALE_HOURS` | `2` | Reaper phase-3 threshold: hours of inactivity after which a session (and the intervals still open on it) is marked stale and closed. Thresholds sessions, not WMS entities — the reaper never touches outcomes or workunits. `0` disables phase 3. |
| `TEAMSTER_REAPER_INTERVAL` | — | Interval between reaper runs (duration string) |
| `TEAMSTER_REVIEW_SWEEP_ENABLED` | — | `1` = enable `teamster wms review-sweep` (both stages). Secondary path alongside `teamster.yaml`'s `review-sweep.enabled`; the yaml key is the intended unattended-flip mechanism. |
| `TEAMSTER_REVIEW_SWEEP_OLDER_THAN` | `168h` | Sweep Stage 1's idle threshold (duration string) — review WorkUnits and stale Outcomes. |
| `TEAMSTER_REVIEW_SWEEP_ABANDON_AFTER` | `720h` | Sweep Stage 2's rescue-window threshold (duration string), measured from the sweep's own parking journal row, not `updated_at`. |
| `TEAMSTER_REVIEW_SWEEP_CONFIRM` | — | `1` = execute for real; unset/`0` = dry-run. `teamster.yaml`'s `review-sweep.confirm` is the source of truth for the unattended timer path regardless of this var. |
| `TEAMSTER_REVIEW_SWEEP_NOTIFY_HOOKD` | — | `0` = disable the per-close hookd/gauge notification for this run. |
| `TEAMSTER_MCP_SCRAPER_ENABLED` | — | `1` = enable the `mcp-scraper` tailer (events.jsonl → `claude_telemetry.mcp_tool_calls`). Secondary path alongside `teamster.yaml`'s `mcp-scraper.enabled`; the yaml key is the intended path. |
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
│   ├── mcp-scraper       (MCP tool-call telemetry tailer: events.jsonl →
│   │                      claude_telemetry.mcp_tool_calls, systemd timer)
│   ├── backup            (backup engine, run by systemd timer)
│   └── teamster-install  (called by lib/installrunner.sh)
├── var/
│   ├── events.jsonl      (append-only JSONL event log)
│   ├── dedup/            (hook client dedup files + session mode markers)
│   ├── sessions/         (session tracker state)
│   └── review-sweep.log  (teamster wms review-sweep stdout/stderr, appended
│                          each run — durable beyond journald's retention)
├── etc/
│   ├── interceptors.yaml         (MCP tool interceptor config: tag colors +
│   │                              per-tool display/suppress rules; embedded
│   │                              default + this file as operator overlay)
│   ├── teamster-hookd.service    (systemd unit, materialized from template)
│   ├── teamster-rollup.service   (rollup one-shot)
│   ├── teamster-rollup.timer     (rollup timer)
│   ├── teamster-classify.service (classifier one-shot)
│   ├── teamster-classify.timer   (classifier timer, every 10 min)
│   ├── teamster-sweep.service    (sweep one-shot)
│   ├── teamster-sweep.timer      (sweep timer)
│   ├── teamster-backup.service   (backup one-shot)
│   ├── teamster-backup.timer     (backup timer, configurable, default 1h)
│   ├── teamster-codex-scraper.service (Codex rollout tailer one-shot, when Codex wired)
│   ├── teamster-codex-scraper.timer   (Codex-scraper timer, every 10 min)
│   ├── teamster-wms-review-sweep.service (review-sweep one-shot, config-gated)
│   ├── teamster-wms-review-sweep.timer   (review-sweep timer, nightly 03:00)
│   ├── teamster-mcp-scraper.service (MCP tool-call tailer one-shot, config-gated)
│   ├── teamster-mcp-scraper.timer   (MCP-scraper timer, every 10 min)
│   ├── teamster-otelcol.service    (OTel collector unit, exec-wrapper; systemd mode)
│   ├── teamster-prometheus.service (Prometheus unit, exec-wrapper; systemd mode)
│   └── teamster-grafana.service    (Grafana unit, exec-wrapper; systemd mode)
├── lib/
│   ├── .claude-plugin/
│   │   └── marketplace.json     (plugin marketplace root)
│   └── plugin/                  (Claude Code plugin: skills + references)
└── doc/
    └── specs/
```

### otelcol/Prometheus/Grafana systemd units — masking and downgrade

Each of the three monitoring components gets its own `teamster-<name>.service`
under the default `hookd_mode: systemd` (the same mode axis hookd itself
uses, `TEAMSTER_HOOKD_MODE` above): `ExecStart` runs `teamster start
--exec=<name>`, an exec-wrapper that loads config, renders it, and
`syscall.Exec`s the real binary in place — no `EnvironmentFile=`, nothing
baked in at install time, and no second command for a config value to go
stale between. The unit files are materialized unconditionally at
install/upgrade, but only installed into systemd's unit directory and
enabled when that component's mode is `install` *and* `hookd_mode` is
`systemd` (not `supervisor` or `external`) — the same masked-unit-aware
install helper every other Teamster unit uses.

**Masking one of the three means that component is off, full stop, with no
fallback to the old supervisor path — the same shape as hookd's own masked
handling.** `sudo systemctl mask teamster-prometheus.service` (or
`-otelcol`/`-grafana`) takes it out of `teamster start`'s systemd branch
entirely: `teamster start` prints that it's left masked and exits 0 anyway
(masking is the operator's own instruction, not a failure), and `teamster
status` reports a distinct masked state rather than a bare "not running."
There is currently no per-component "run this one without systemd" lever —
masking only ever means off, never "fall back to the supervisor-managed
process for just this component." To keep a component supervisor-managed
instead, set `hookd_mode: supervisor` for the **whole host**, which drops
hookd and all three monitoring components back to the crashloop-supervised
`setsid` path together; there is no way to mix systemd for some of the four
and supervisor for others today.

**Downgrading the `teamster` binary after upgrading to a version with these
units, without disabling the units first, will fail to rebind their
ports.** An older binary has no knowledge of `teamster-otelcol.service`/
`-prometheus`/`-grafana`; its own PID-file liveness check reports each
component as "not running" and it will try to launch a fresh instance on a
port the still-running systemd-managed process already holds, and fail to
bind. Disable the units before downgrading:
```bash
sudo systemctl disable --now teamster-otelcol teamster-prometheus teamster-grafana
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
| `teamster` | Go | hub | Hook client. Forked per hook event. Reads stdin JSON, enriches, POSTs to hookd. Must exit 0 always. Also the CLI (`start`/`stop`/`status`/`wms-reset`/`tags`/`setup tags`/`wms drain`/`wms list`/`wms close`/`wms gc`/`wms review-sweep`/`check-config`/`clone`). `wms review-sweep` is the nightly two-stage lifecycle-hygiene sweep (§"Nightly review sweep" under Data Flows, `semantic-conventions.md` §4.8) — distinct from `rollup --sweep`/`teamster-sweep.timer`'s cost-*attribution* sweep despite the shared word. |
| `teamster.py` | Python | remote | Hook client on remotes. Pure stdlib. Same wire contract as Go version. |
| `hookd` | Go | hub | HTTP event server. POST `/event` → JSONL. Dashboard, SSE, WMS page, metrics, MCP routes (`/mcp/activity`, `/mcp/wms`, `/mcp/roster`, `/mcp/health`). Focus-absent nudge on PreToolUse. Attempts to auto-open a focus interval for the claiming agent on WorkUnit claim success (WMSStatusChange pending→active) — asynchronous and best-effort; declines and warns on an identity race rather than guessing. Auto-registers agents on roster from first hook event. Tracks per-agent turn state (processing/idle). |
| `feed` | Go | hub | Long-running terminal viewer. Tails events.jsonl, ANSI colorizes. |
| `activity-mcp` | Go | hub | MCP stdio for activity tools (hub-local sessions). No-op: tools return confirmation strings; real data extracted from PreToolUse by hook client. Includes `setMode`. |
| `wms-mcp` | Go | hub | MCP stdio for WMS CRUD (hub-local sessions). Outcome/WorkUnit lifecycle, rename, tags, focus, dependencies. `wms_claimWorkUnit` returns the WorkUnit's `brief` and performs the `pending`→`active` transition as an atomic CAS, requesting a focus interval alongside it — the interval open itself is hookd's, asynchronous and best-effort, and may be declined; `wms_deliverResult` records an agent's output in `wms_deliverables` and transitions the WorkUnit active→review. Writes MySQL, emits status events via HookObserver. |
| `rollup` | Go | hub | Cost-attribution pipeline. Allocates token spend to WMS entities. Recovery passes for unallocated messages. Run by systemd timer. |
| `classify` | Go | hub | Derives phase and work-type tags on intervals/workunits from rule-based signals. Run by systemd timer every 10 min. |
| `token-scraper` | Go | hub | Reads **Claude Code** session transcripts, extracts per-message token usage, writes to token_ledger. Never reads Codex data. |
| `codex-scraper` | Go | hub | Codex rollout-JSONL cost/ledger tailer (systemd timer, oneshot). Sole writer of Codex `token_ledger` rows (via hookd `/telemetry`) and Codex `sessions` rows (direct store). Books `thread_spawn` subagent spend under the parent session as `@<role>`. No-op on hosts with no `codex` CLI. |
| `mcp-scraper` | Go | hub | MCP tool-call telemetry tailer (systemd timer, oneshot, config-gated on `MCPScraper.Enabled`/`TEAMSTER_MCP_SCRAPER_ENABLED`). Tails `events.jsonl`, filters to completed `mcp__*` tool calls, ledgers one row per call into `claude_telemetry.mcp_tool_calls` — a call-volume instrument, not an audit trail (no `tool_input` capture). Cursor + generation counter survive `copytruncate` rotation. Single-writer invariant: only the hub's own process writes this table, so `teamster-mcp-scraper.timer` is permanently masked on every `teamster clone` target. |
| `health-collector` | Go | hub | Agent health gauge collector. Hub daemon, 15s poll interval. Reads `token_ledger` for per-agent token usage and cost, writes `agent_health_gauge` rows. Context window comes from Claude Code's StatusLine when available; an Agent-Teams teammate (no StatusLine channel) gets its window from its own transcript instead, falling back to a model-class table then the lead's window. Resolves `roster_id` per agent. |
| `ctop` | Go | hub | Terminal Bubbletea dashboard over hookd's `/health/api/*` + `/health/stream` (HTTP client only — no DB/store imports), so `--server` can point it at any hub, hub-local or remote. Four views (keys 1–4): health, focus, cost, and fleet — a multi-team tree (team headers, lead + teammates + sub-spawns with tree connectors, collapse/expand) with a live activity log below the grid. Fleet is the default view on launch. |
| `teamster-install` | Go | hub | Called by `lib/installrunner.sh`. Copies binaries, materializes systemd units, merges settings.json. |
| `demogen` | Go | hub | Synthetic data generator for dashboards. Creates correlated demo data. `--clean` for teardown. |
| `backup` | Go | hub | Backup engine. Takes timestamped snapshots of MySQL, OTel, and teamster config/state. No sudo. Also reachable via `teamster backup` and `teamster restore`. Run by systemd timer. |

---

## Grafana Dashboards

The provisioned dashboards in `skel/etc/grafana/dashboards/`:

| Dashboard | File | Purpose |
|-----------|------|---------|
| Landing Page | `landing-page.json` | Welcome/index page linking to the other dashboards |
| 00 - Realtime Fleet View | `fleet-view.json` | Grafana-embedded fleet view (agent tree, health, activity) — counterpart to `ctop` |
| 01 - AI Spend Explorer | `fd-ai-spend-overview.json` | High-level AI spend overview |
| 02 - Cost Explorer | `fd-cost-explorer.json` | Multi-facet cost drill-down |
| 03 - AI Usage & Effectiveness | `fd-usage-effectiveness.json` | Agent efficiency, model fit, throughput |
| 04.01 - Simple Cost Explorer | `cost-by-tag-value.json` | Cost breakdown by single tag value |
| 04.02 - Multidimension Cost Explorer | `tag-stack-explorer.json` | Composable $stack_level_1/2/3 drill-down by tag key |
| 04.03 - Work Entity Explorer | `entity-cost-explorer.json` | Per-entity cost drill-down |
| 05 - Outcome Accounting | `fd-outcome-accounting.json` | Outcome lifecycle and cost accounting |
| 06 - Outcome Cost Explorer | `outcome-cost-explorer.json` | Per-outcome cost drill-down |
| 06 - Sweep Report | `sweep-report.json` | WMS review-sweep (issue #11/#17) visibility: open WorkUnit/Outcome backlog, the nightly sweep's disposition breakdown by rule id, day-by-day activity history, the sweep-parked `on_hold` queue awaiting Stage 2, and freshness. Number collides with Outcome Cost Explorer above — both dashboards ship titled "06 -", a pre-existing naming quirk, not a doc error. See `semantic-conventions.md` §4.8 for the rule-id/agent_id grammar. |
| 07 - Realtime Activity Feed | `activity-feed.json` | Live agent activity stream in Grafana |
| 08 - Claude Code Metrics (OTEL) | `claude-code-metrics.json` | Per-model token usage and cost metrics |
| 09 - Teamster System Health | `fd-data-quality.json` | Data quality and system health |
| 10 - Codex Metrics (OTEL) | `codex-metrics.json` | Codex CLI turn/token/latency metrics via OpenTelemetry (fleet visibility only — never a cost source; WMS cost attribution for Codex comes exclusively from `codex-scraper`) |

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

