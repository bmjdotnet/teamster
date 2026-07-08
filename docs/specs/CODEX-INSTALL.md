# Codex Support — Install & Operate

**Status: complete (v1).** Installer wiring, MCP servers, OTEL, skills,
hooks, the audit-trail residual-risk note, codex-scraper, and uninstall are
all covered below. This is the authoritative Codex-support doc — merge new
content in here rather than starting a second one.

## Overview

Teamster support for the Codex CLI is a solo-only, opt-in overlay: it wires
Codex's own config (`~/.codex/config.toml`, `AGENTS.md`) into the same
observability/WMS system Claude Code uses, without requiring Codex to be
present and without touching anything the operator didn't ask for. A host
with no `codex` binary in `PATH` installs unchanged — Codex wiring is
skipped, informationally, not an error.

Every session row and cost-ledger row Teamster records carries a `runtime`
column, `{claude_code, codex, unknown}` (mysql/sqlite migration v51,
additive-only — see codex-scraper's Schema additions below for the full
column list). This is the one identity fact every other section below
assumes: Codex data is never merged into or confused with Claude Code data,
it's a parallel, identically-shaped stream distinguished by this enum.

## Installer integration

### `--codex-mode` flag

`lib/installrunner.sh` and the underlying `teamster-install` binary both
accept `--codex-mode={install|none}` (unset = auto-detect, the default):

| Value | Behavior |
|---|---|
| unset (default) | Auto-detect: if `codex` is in `PATH`, wire it; if not, skip silently. |
| `install` | Force-wire. Hard error (install aborts) if `codex` is not in `PATH` — use when the operator knows Codex must be present and wants a missing binary to fail loudly instead of silently skipping. |
| `none` | Skip Codex wiring even if `codex` is present. For operators who want Claude Code only on a host that happens to have Codex installed too. |

`install.sh`'s guided interview does not currently ask about this — it's an
advanced/scripted-install flag only, passed straight through to
`lib/installrunner.sh` and then to `teamster-install`.

### Probe, then wire

`cmd/teamster-install/main.go`'s `run()` calls `probeCodex()` (step 11,
after the Claude Code steps) — `exec.LookPath("codex")` plus `codex
--version`, both graceful. Given a version string (or `--codex-mode=install`
forcing the issue), it calls `wireCodex(basedir, home, storeDSN,
hookServerURL, host, env, otelCodexHTTPPort)`, which sequences, in order:

1. `codexconfig.WriteMCPServers` — the `activity` and `wms` MCP server
   tables (below).
2. `codexconfig.WriteOtelConfig` — only if `otelCodexHTTPPort != 0` (i.e.
   `--otelcol-mode=install`; skipped entirely rather than pointing Codex's
   exporter at a collector that isn't running).
3. `codexconfig.InstallSkills` + `mergeCodexAgentsMD` — skills file-copy and
   the `AGENTS.md`/`AGENTS.override.md` protocol append.
4. `codexconfig.WriteHooks` — the three hook registrations plus their trust
   state.

Every one of these steps writes through `internal/codexconfig`'s shared
backup-then-doctor-gate machinery: a pre-write `.pre-teamster`/timestamped
`.bak` snapshot (via `installbackup`, the same package retrofitted onto the
Claude Code config-write paths in WP2), then `codex --strict-config doctor
--json` gated on `checks["config.load"].status` — never `overallStatus`,
which can fail for unrelated reasons like missing auth. A failing gate rolls
the write back to the pre-write backup; `wireCodex` returns an error rather
than leaving a partially-applied, doctor-failing `config.toml` in place. The
install summary reports one of: `not detected — skipped`, `skipped
(--codex-mode=none)`, or `wired (codex <version>)`.

### MCP servers

Two tables are written, both marker-bounded (`# >>> teamster:mcp_servers.wms
>>>` / `<<< ... <<<`) and `SkipIfPresent` — an operator's own hand-edit to a
previously-written server table survives every subsequent install run:

- `[mcp_servers.activity]` — same no-op activity tools as Claude Code's
  side (`reportActivity`/`setOverallIntent`/`completeActivity`); real
  attribution still comes from the hook client, not this MCP surface.
- `[mcp_servers.wms]` — `wms-mcp`, pointed at `TEAMSTER_STORE_DSN` and the
  hook server URL via an inline `env` table (rendered compact and sorted,
  never a `[mcp_servers.wms.env]` sub-table — both parse identically, this
  is just visual consistency with the rest of the file).

Both tables set `default_tools_approval_mode = "approve"` — the verified
fix for `codex exec`'s silent-cancel-without-a-TTY behavior (`codex exec`
has no TUI to prompt for approval; without this setting every mutating WMS
tool call is silently cancelled, not silently allowed — the failure mode
looks like "the tool didn't do anything," not an error). This is also the
setting the audit-trail note below is about: it removes Codex's
human-in-the-loop confirmation over every mutating tool `wms-mcp` exposes.

The installer never writes `projects.*.trust_level` — that decision is
always the operator's, not this installer's, and is out of scope for both
`WriteMCPServers` and Teamster generally.

### OTEL

Codex gets its own dedicated `otlp/codex` OTLP receiver
(`skel/etc/otelcol.yaml.tmpl`), never sharing a port with Claude Code's
receiver, at `otelCodexHTTPPort` (prior-value-preserving across reinstalls,
`findFreePort(4329)` fallback on first install, computed only when
`--otelcol-mode=install`). The `[otel]` table in `config.toml` is always
rewritten in full (`AlwaysUpsert`, not `SkipIfPresent` like the MCP server
tables) — a stale prior write (changed collector port, flipped otelcol
mode) must be replaced every run, not preserved, because Codex's schema is
narrow enough that a half-stale table is more likely to fail to load than
to harmlessly persist.

Two schema quirks worth knowing if this ever needs debugging: Codex's
`otlp-http` exporter always POSTs to the configured endpoint's bare root
regardless of any path component (live-verified against Codex 0.137.0), and
`protocol` is a **mandatory** key for the `otlp-http` backend, not optional
— omitting it fails config load with `missing field 'protocol' in
otel.metrics_exporter`. `otlp-grpc` was tried and rejected: it never
attempted a TCP connection to the configured endpoint in live testing, so
`otlp-http` is the only backend Teamster ever writes. `exporter`/
`trace_exporter` (logs/traces) are always `"none"` — enabling them
alongside metrics on the same dedicated receiver instance is a collector
startup panic, not merely redundant, and `log_user_prompt` is always
`false` (privacy default; the JSONL rollout tailer already gives Teamster
full local prompt content without shipping it off-host via OTEL too).

### Skills

Skills are delivered by **file-copy** to `~/.codex/skills/<name>/`
(`codexconfig.InstallSkills`), **not** the Codex plugin system
(`.codex-plugin/plugin.json` + `agents/plugins/marketplace.json`, installed
via `codex plugin marketplace add`/`codex plugin add`). This was an
empirical finding, not a preference: registering a Codex plugin cannot be
done by hand-writing `config.toml` the way MCP servers can — it requires
shelling out to the `codex` binary itself, whose plugin-cache
implementation is internal and under active refactor across Codex releases.
File-copy has no such dependency and was verified working for both plain
skill discovery and `agents/openai.yaml`'s `policy.allow_implicit_invocation`
suppression. The plugin-shaped assets are still shipped
(`skel/lib/codex-plugin/`, `skel/lib/.agents/`) as a documented fallback if
a future work package wants the plugin system's namespacing/uninstall
visibility instead — they are not wired up as of v1.

Each install is a full remove-then-copy per skill directory (not a
marker-bounded merge like `config.toml`) — skills are pure Teamster-
generated content with no "the operator hand-edited this" case worth
preserving. Some skills are ambient-discoverable by default; others carry
"Explicit invocation only — mention `$skill-name`" in their frontmatter
description (mirroring Claude Code's `disable-model-invocation`) — an open
"list your skills" prompt will only surface the ambient ones, which is
by design, not a bug (confirmed in Phase C testing: `teamster-solo`/
`teamster-tags`/`teamster-review` are explicit-only; `teamster-status` is
ambient).

`start` is a fifth skill, added after the initial four to give Codex a
discoverable front door analogous to Claude Code's `/teamster:start` —
Codex has no slash-command namespace, but a skill directory literally named
`start` gets the trigger `$start` for free, since the trigger is just the
directory name under `~/.codex/skills/`. It ships ambient (like
`teamster-status`) and is a thin pointer: its `SKILL.md` tells the agent to
read and follow the sibling `teamster-solo` skill inline rather than
duplicating that flow.

### Hooks channel

Codex reads hook registrations **only** from inline `[[hooks.<Event>]]`
TOML tables in `config.toml`. A `hooks.json` file (in `CODEX_HOME` or a
project's `.codex/`, flat or `{"hooks": {...}}`-wrapped) is accepted by the
config loader without error but **never fires** — verified across multiple
attempts, in both the TUI and `codex exec`. Its mere presence also adds a
measurable latency tax to `codex exec` (roughly 2-3x baseline, several
extra seconds, observed consistently on retest); an earlier finding
characterized this as a hard ~30s hang, which did not reproduce reliably —
treat it as "adds latency, provides no function," not as a guaranteed hang
to a specific timeout. Teamster never writes `hooks.json`; only the TOML
form, and this doc should not repeat a specific hang duration as fact.

Ten hook events exist in Codex 0.137.0, all PascalCase (`SessionStart`,
`SubagentStart`, `PreToolUse`, `PermissionRequest`, `PostToolUse`,
`PreCompact`, `PostCompact`, `UserPromptSubmit`, `SubagentStop`, `Stop`).
Teamster's v1 installer registers three: `SessionStart`, `PreToolUse`,
`PostToolUse` (`codexconfig.TeamsterHookSpecs`), each pointed at
`python3 <basedir>/lib/hook/codex-hook.py` with matcher `.*` (every event,
every tool — the live feed wants everything) and a 10-second timeout. The
command is written as an explicit `python3 <path>` invocation, not a bare
path relying on the shebang/executable bit, since Codex's hook execution
was confirmed live to handle a multi-word command string correctly — this
removes any dependency on the installer preserving the executable bit.
`codex-hook.py` is pure-stdlib Python (client-side components avoid
requiring a Go toolchain on hosts that only run Codex, the same reasoning
that keeps `teamster.py` in Python) and imports `teamster.py`'s redaction
and error-logging helpers directly, so the two files must ship together in
`lib/hook/`.

Hooks require a one-time trust step Codex normally does interactively via
its TUI. Teamster's installer writes the trust block directly — no TUI, no
`--dangerously-bypass-hook-trust` flag needed — as
`[hooks.state."<absolute-config-path>:<event_snake_case>:0:0"] trusted_hash
= "sha256:..."`. The hash is computed purely from the hook's own definition
(event name, matcher, command string, timeout — canonicalized to sorted
JSON, then sha256'd) and is **not** sensitive to the config file's path;
only the *lookup key* (the state table's name) embeds the absolute config
path, so a trust block must be written under the exact final install path
to ever be found, but the hash itself can be computed and written by an
installer with zero interaction. Any later change to the hook's
command/args/timeout/matcher silently invalidates that hash — no error, no
prompt, the hook just silently stops being trusted — so
`codexconfig.WriteHooks` re-derives and re-writes both the registration and
the trust-state block on every install run to self-heal this rather than
assuming a prior trust grant still applies.

### AGENTS.md merge

`mergeCodexAgentsMD` appends a Codex-specific protocol block to
`AGENTS.md` (or `AGENTS.override.md`, whichever the operator's Codex config
targets), keyed by the heading marker `## Getting Started with Teamster
(Codex)` — idempotent (checks for the marker before appending) but not a
removable section like `config.toml`'s marker pairs; see Uninstall below
for how to reverse it. The content mirrors Claude Code's `CLAUDE.md`
protocol append (activity feed, focus discipline) adapted for the fact that
Codex has no Agent Teams layer — every Codex session runs solo, and its
subagents (where used) are ephemeral spawn-wait-collect calls, not
persistent teammates.

## Known limitation: deferred MCP tool loading on newer Codex builds

**The behavior.** Codex CLI has an internal feature flag,
`tool_search_always_defer_mcp_tools`, that gates whether MCP tools
(`wms_*`, `reportActivity`/`setOverallIntent`/`completeActivity`, etc.) are
directly callable or defer-loaded behind a client-side tool search the model
must run itself. When the flag is on, a tool that was never surfaced by a
search is **hard-uncallable** — not merely discouraged: an operator VM triage
confirmed at the wire level that the model never even emitted a
`tools/call` attempt for a non-surfaced tool, because it simply wasn't in the
model's declared function set. The `wms` server has 31 tools regardless of
this flag; the flag only changes how many of them a given turn can see
without searching first.

**The version story.** This kit is pinned and verified at Codex **0.137.0**,
where `tool_search_always_defer_mcp_tools` is off and all 31 `wms` tools are
directly callable with no search step — confirmed live, twice (a raw
JSON-RPC probe against the real `wms-mcp` binary, and a real `codex exec`
session), both returning the complete 31-tool catalog with no losses. An
operator VM running a **0.142.5** build (which Codex had auto-updated itself
to — Teamster does not pin or manage the Codex CLI version) showed the flag
baked in as `true`. Reproduced in an isolated container built to match
0.142.5's exact behavior (`codex features list` byte-identical to the VM's),
confirming this is a real, version-gated Codex change, not anything specific
to the operator's host or something Teamster's installer wrote.

**No config override exists.** Three independent attempts to flip the flag
back off were all silently ignored (no parse error, no effect on the reported
state): `-c features.tool_search_always_defer_mcp_tools=false`,
`--disable tool_search_always_defer_mcp_tools`, and a persistent
`[features]` table written directly into `config.toml`. This is not a
settings problem an installer or operator can work around — it is baked into
the binary at this version.

**The mitigation — search-style guidance, not a code fix.** There is no lever
Teamster's installer can pull to restore direct tool visibility, so the
mitigation is prompting discipline, burned into `AGENTS.md`/
`AGENTS.override.md` by `mergeCodexAgentsMD` (see AGENTS.md merge above): a
query that reads as natural language describing the intended action —
"create a new outcome," "tag entities," "update work unit status" — reliably
surfaces a substantial subset of the catalog (17-20 of 31 tools observed
across query variations). A query that is a bare tool name or a `wms_`
prefix with no descriptive framing reliably returns **zero** results —
exactly the shape of query that produced the VM's original "5 tools" report,
and exactly the shape `AGENTS.md` now tells every fresh Codex session to
avoid. Recall is not total even with good query style: **7 of 31 tools never
surfaced across every query variation tried** in this kit's own testing
(`wms_addDependency`, `wms_removeDependency`, `wms_getFocus`,
`wms_getHistory`, `wms_getTimeline`, `wms_listBlockers`,
`wms_listDependents` — skewing toward dependency-management and
read/history tools). Whether that remaining gap is a hard ceiling or a
further query-phrasing/limit artifact is unsettled; `AGENTS.md`'s guidance to
retry with different descriptive wording before concluding a tool doesn't
exist is the best mitigation this kit can offer without a Codex-side fix.

**What's still verified intact at 0.142.5.** The identity and ledger paths
this kit's acceptance criteria gate on do not depend on direct tool
visibility: `x-codex-turn-metadata` (WP1's session-identity mechanism) is
still present and correct on every `tools/call` that does get made — a real
`wms_createOutcome` call in the 0.142.5 reproduction container carried
correct turn metadata and produced a real, attributed outcome. `codex-scraper`
parsed a 0.142.5 rollout file cleanly in dry-run mode with no schema-mismatch
warnings — the tool-search mechanism is additive to the rollout format
(new `response_item` types), not a change to the `token_count`/
`session_meta`/`turn_context` shapes the tailer depends on. This limitation
is a **discovery/enumeration UX gap**, not a regression in WMS attribution or
cost capture.

**Evidence:** `research/evidence-round3/wp7-vm-triage/` in the
teamster-codex-kit — `README.md` (0.137.0 baseline, 31/31 tools, the
free-text-self-report methodology caveat) and `0142-causal/README.md` (the
causal reproduction, the three override attempts, and the four search-query
mitigation runs).

## Audit trail limits on default-approve

This installer writes `default_tools_approval_mode = "approve"` for both
`[mcp_servers.activity]` and `[mcp_servers.wms]` (see MCP servers above) —
the verified fix for `codex exec`'s silent-cancel-without-a-TTY behavior.
That setting removes Codex's human-in-the-loop confirmation over every
mutating WMS tool it exposes (roughly 18 tools reachable by prompt-
injectable repo content: status/outcome/work-unit transitions, tagging,
dependencies, focus, phase, work-unit assignment/claim, and creation). The
audit trail an operator would lean on to reconstruct what happened after
the fact covers that surface unevenly:

- **4 tools write a session-attributed `wms_journal` entry**:
  `wms_updateStatus`, `wms_updateOutcomeStatus`, `wms_updateWorkUnitStatus`,
  `wms_claimWorkUnit` (all routed through `EngineImpl.OnStatusChange`).
  `JournalObserver` correctly carries `SessionID`/`AgentName`/`Host` — a
  Codex-originated mutation through one of these four is fully attributable
  to the Codex session UUID.
- **2 tools leave a session-attributed trace outside `wms_journal`**:
  `wms_createOutcome` and `wms_createWorkUnit` stamp
  `OriginSession`/`OriginHost`/`OriginAgent` directly on the entity row and
  open a session-attributed `wms_intervals` record (`store.OpenEventRecord`)
  — arguably better-traced than the four journaled tools, just not in the
  journal table an investigator would check first.
- **4 tools leave a trace, but not a session-attributed one**:
  `wms_tagEntity` (an `entity_tags` row with `source` and `applied_at` —
  records *how* the value was set, not *who* set it); `wms_untagEntity`,
  `wms_snapshotEntityTags`, and `wms_rollbackTags` (all write/read a
  tag-steward JSONL snapshot carrying entity/tag/old-value data with no
  session, agent, or host field at all).
- **7 tools leave no durable trace of the caller at all**:
  `wms_addDependency`, `wms_removeDependency`, `wms_assignWorkUnit`,
  `wms_setFocus`, `wms_defineTag`, `wms_retireTag`, `wms_describeTag`. None
  of their store calls are passed session, host, or agent identity, and no
  other record captures it.
- **`wms_setPhase` is a distinct, sharper risk than "no trace": it can
  produce a misattributed one.** It doesn't create a new record — it
  updates the `phase`/`phase_source` columns on the `wms_intervals` row an
  earlier `wms_updateWorkUnitStatus` call opened (`UPDATE wms_intervals SET
  phase=?, phase_source=?, ... WHERE id=...`, never touching
  `session_id`/`agent_name`/`host`). That row's identity columns still show
  whoever opened the interval, not whoever declared the phase — a
  reasonable-looking but potentially wrong answer if a different session
  calls `setPhase` later. This is a false positive, not an absence, and
  merits its own note rather than folding into the "no record" bucket: the
  two failure modes call for different mitigations (fill in a gap vs. stop
  trusting a misleading field).

Net effect: default-approve's human-gate removal is well backstopped for
status transitions and entity creation (5 of ~18 tools, all
session-attributed), partially backstopped for tag operations (4 tools,
provenance without identity), essentially unbackstopped for
dependency/focus/assignment/tag-vocabulary operations (7 tools), and
actively misleading for one (`setPhase`). This is a pre-existing shape in
`internal/wms`/`internal/mcp/wms`, not something Codex support introduced —
it applies identically to Claude Code — and closing the gap is tracked
separately (WMS outcome `wms-journal-coverage`, not gated on Codex support
v1). Operators who want a stronger audit posture before enabling
default-approve should be pointed at the JSONL rollout tailer (codex-scraper,
below) and the hooks channel (above) as the two channels that do carry full
session identity and tool-call payloads for every mutating call today,
regardless of which of the categories above the specific tool falls into.

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
  stdin payload (a hooks-channel concern, above), not anything present in
  rollout JSONL content. The tailer's scan-order sequence-number design
  (above) needs no such discriminator regardless — every `token_count`
  event is ledgered independently of whether its turn came from the
  original launch or a later resume.

### Subagent attribution (Codex `thread_spawn`)

Codex has no persistent Agent-Teams layer — a Codex session is solo — but
Codex 0.142.x's `thread_spawn` feature lets a session spawn ephemeral
subagents, and each subagent runs as its own *thread* with its own rollout
file. The tailer attributes that subagent spend to the parent session rather
than stranding it as an orphan:

- A subagent file's `session_meta` carries `id` = the subagent's own thread
  UUID, `session_id` = the **parent** thread's id, and `parent_thread_id` =
  that same parent id (a top-level file has `id == session_id` and no
  `parent_thread_id`). Verified live (chunk-test2 evidence). Codex 0.137.0's
  `session_meta` has no `session_id` field at all, so the tailer **falls back
  to `id`** — which also makes top-level 0.142.x files (`session_id == id`)
  behave identically either way.
- **Ledger rows and the `sessions` upsert use the parent-resolved id**
  (`session_meta.session_id`), so a subagent's cost books under the **same
  `session_id` as the parent's own focus intervals**. `rollup`'s existing
  `temporal_join` / `temporal_join_lead_session_fallback` machinery then
  attributes it like any other message in that session. This scraper-time
  resolution is the **single point** of child→parent mapping — no rollup-side
  mapping is added, because a second resolution layer would double-attribute
  (subagent message timestamps fall entirely inside the parent's focus
  windows, so the existing temporal join is sufficient on its own).
- **`message_id` stays keyed by the file's own thread id**, not the shared
  `session_id` (`codex:<thread-id>:<seq>`). Keying on `session_id` would let a
  parent file's `seq 1..N` and a subagent file's `seq 1..M` collide onto the
  same key, and the `uq_message` upsert would silently swallow one file's rows.
- **`agent_name` = `@<agent_role>`** (e.g. `@explorer`), read from
  `session_meta.agent_role` (present only on subagent files; role, not
  nickname). This matches the identity wms-mcp already opens focus intervals
  under, and the `sessions` primary key `(session_id, agent_name)` lets a
  subagent's `(parent, @role)` row coexist with the parent's `(parent, "")`
  row exactly like a Claude Code teammate. Parent/direct spend carries
  `agent_name=""`.

**Upgrading a pre-release install scraped by a pre-fix build.** This fix
changed how `session_id` / `message_id` / `agent_name` are derived. Rows a
**pre-fix** `codex-scraper` already wrote booked subagent threads as orphan
sessions, and incremental polling won't re-read those bytes to heal them. To
repair such an install: reset the scraper cursor for the affected rollout files
(delete or trim `var/codex-scraper-cursors.json`) so the next run re-scrapes
them under the parent `session_id`, then run `rollup --sweep --reallocate`
once. `--reallocate` clears every attribution row not allocated to a real
entity (`entity_type=''` — the `unallocated` bucket plus `sweep_skipped`
give-up markers) and re-derives it, leaving rows that already carry a real
entity structurally untouched (so the parent's allocated cost is never
disturbed). An automatic `rollup --sweep` from `teamster-rollup.timer` firing
in between is harmless — whatever method it leaves the re-scraped rows in, the
one-time `--reallocate` still clears and re-derives them. This applies **only
to pre-release test installs** — the first shipped build derives identity
correctly from the start, so fresh sessions never need a reallocate.

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
- **No persistent Agent Teams.** Codex has no persistent teammate layer, so
  there is no team-mode wiring in v1 (operator decision). Ephemeral
  `thread_spawn` subagents ARE attributed to their parent session — see
  Subagent attribution above; parent/direct spend carries `agent_name=""`,
  subagent spend carries `@<agent_role>`.
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
rm -rf ~/.codex/skills/start ~/.codex/skills/teamster-solo \
       ~/.codex/skills/teamster-status ~/.codex/skills/teamster-tags \
       ~/.codex/skills/teamster-review

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
