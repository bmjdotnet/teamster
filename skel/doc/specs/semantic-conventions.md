# Semantic Conventions

This document is the normative reference for all field names, tag constants,
and entity conventions used in Teamster's JSONL activity stream. It covers
every field a consumer (feed, dashboard, analytics) may encounter.

---

## 1. Core Event Fields

These fields are present on every record in the JSONL stream. They are set by
the hook client (`teamster` binary on hub, `teamster.py` on remotes) before
the record is POSTed to hookd.

| Field | Type | Description | Source | Stability |
|-------|------|-------------|--------|-----------|
| `hook_event_name` | string enum | Claude Code hook event that triggered this record. Values: `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `Stop`, `UserPromptSubmit`, `SubagentStart`, `SubagentStop`, `TeammateIdle`, `TaskCompleted`, `WMSStatusChange`, `WMSFocusChange` | Claude Code / hookobserver | stable |
| `session_id` | string | Opaque Claude Code session identifier. Falls back to `"unknown"` if absent. | Claude Code | stable |
| `tool_name` | string | Name of the Claude Code tool being called (e.g. `Bash`, `Read`, `SendMessage`). Absent on `Stop` and WMS synthetic events. | Claude Code | stable |
| `tool_input` | object\|string | Raw tool input as provided by Claude Code. May be a JSON object or a string-encoded JSON object. | Claude Code | stable |
| `tool_response` | string | Tool output returned to Claude Code. Present on `PostToolUse` only. | Claude Code | stable |
| `agent_id` | string | Opaque agent identifier assigned by Claude Code to a teammate. | Claude Code | stable |
| `agent_type` | string | Human-readable agent name as set when the agent was spawned (e.g. `store`, `engine`). Used to derive `_agent_name`. On the hub/Linux this is inline in the payload; on macOS teammate payloads it is **absent** and the Python hook client backfills it from the transcript's `agentName` (see §1.1). | Claude Code / teamster.py | stable |
| `cwd` | string | Working directory of the Claude Code process at the time of the hook event. | Claude Code | stable |
| `transcript_path` | string | Filesystem path to the JSONL transcript for this session. Present on `Stop` events. | Claude Code | stable |
| `stop_response` | string | Final assistant message from a `Stop` event, if available. | Claude Code | stable |
| `last_assistant_message` | string | Fallback final message used when `stop_response` is empty. | Claude Code | stable |
| `host` | string | Originating hostname, set by the Python hook client on remote installs. The Go client uses `_host` instead. | Python hook client | stable |
| `ts` | string | RFC3339 UTC timestamp of the event (`2006-01-02T15:04:05Z`). Set by WMS synthetic events; Claude Code events carry their own timestamps. | hookobserver / hookd | stable |

### 1.1 Agent identity: `agent_type` derivation (Linux vs macOS)

How a teammate's identity reaches the stream differs by platform, because
Claude Code's Agent-Teams runtime structures teammate sessions differently:

- **Hub / Linux.** Dispatched teammates share the **lead's `session_id`**, and
  each teammate's hook payload carries an inline **`agent_type`** field. The
  lead has no `agent_type`. `_agent_name` is derived directly:
  `"@" + agent_type` (empty for the lead).

- **macOS (remote).** Each teammate runs as a **separate top-level session** —
  its own `session_id`, its own transcript at
  `~/.claude/projects/<proj>/<session>.jsonl` (**not** under a `subagents/`
  subdirectory) — and its hook payloads carry **no `agent_type`**. The teammate
  name lives only in the transcript's top-level **`agentName`** field
  (e.g. `{"agentName":"PizzaDude",...}` near the head of the file). Lead
  sessions have no `agentName`.

  To keep the stream and cost attribution correct, the Python clients derive
  identity from the transcript:

  - **`teamster.py`** (hook client): when a payload has no `agent_type` but
    carries `transcript_path`, it scans the transcript head (bounded to
    256 KB) for the first non-empty `agentName` and sets `event["agent_type"]`
    to it — so hookd's `EnrichRecord` then produces `_agent_name = "@<name>"`
    as usual. Fork-per-event, so no caching.
  - **`token-scraper.py`**: for each top-level session transcript it scans the
    head for `agentName` and attributes that session's cost to `@<agentName>`
    rather than the lead. Memoised per process **only when non-empty** — an
    empty result (the `agentName` record not yet written when the scraper
    polls) is not cached, so the next poll retries instead of permanently
    misattributing the teammate's cost to the lead.

  Both scans are best-effort and never raise. Use `TEAMSTER_DEBUG_RAW=1`
  (which makes `teamster.py` dump verbatim hook stdin to
  `~/teamster/var/raw-hook-debug.jsonl`) to confirm what a macOS payload
  actually contains.

### 1.2 UserPromptSubmit `additionalContext` (nudge parity for remotes)

On the hub, the Go hook client injects the activity-reporting reminder and the
team-dispatch mandate locally from constants. The remote Python client has no
copy of that text, so **hookd returns it in the HTTP response**: on a
`UserPromptSubmit` event, hookd sets `additionalContext` in its JSON response to
`ACTIVITY_INSTRUCTION + TEAM_DISPATCH_INSTRUCTION`, and `teamster.py` echoes it
back as `hookSpecificOutput.additionalContext` (it echoes `additionalContext`
on both `PreToolUse` and `UserPromptSubmit`). The hub Go client ignores this
response field — no double-injection.

**Limitation:** hookd cannot observe a remote session's solo/team marker (it is
client-local state, never sent over the wire), so hookd always returns the
**team** context on `UserPromptSubmit`. A solo remote session will still see the
team-dispatch text; this is the least-harm default since the common remote case
is team work and the text is guidance, not enforcement.

---

## 2. Enriched Fields

These fields are added by the hook client (Go or Python) during enrichment, or
by hookd's `EnrichRecord` on the hub side for records that arrived without
enrichment. All names begin with `_` to distinguish them from raw Claude Code
fields.

`EnrichRecord` is idempotent: if a field is already set (client-side
enrichment), the hub does not overwrite it.

| Field | Type | Description | Source | Stability |
|-------|------|-------------|--------|-----------|
| `_agent_name` | string | Display-ready agent identity: `"@" + agent_type`. Example: `@store`. Empty for the lead (no `agent_type`). On macOS the `agent_type` is backfilled from the transcript's `agentName` before this is derived (see §1.1). | hook client / EnrichRecord | stable |
| `_host` | string | Canonical hostname for the originating machine. Derived from `os.Hostname()` on the hub client; WSL hosts append `-wsl`. On remotes, copied from the `host` field. | hook client / EnrichRecord | stable |
| `_session_id` | string | Copy of `session_id`, normalized to `"unknown"` if blank. Written to `~/.claude/current-session-id` so wms-mcp can read it. | Go hook client | stable |
| `_model` | string | Model identifier read from `~/.claude/settings.json`, or extracted from the session transcript on `Stop`. | hook client / EnrichRecord | stable |
| `_tool_tag` | string | Display category tag (4-char code). One of the 16 values in the tag taxonomy (section 3). | hook client / EnrichRecord | stable |
| `_tool_display` | string | Human-readable description of the tool call. May contain `__param__` markers (rendered in cyan by feed) and entity references (`@agent`, `#team`, `<model>`). Max 256 chars. | hook client / EnrichRecord | stable |
| `_bash_cmd` | string | Raw shell command for a Bash tool call. Set alongside `_tool_display` for `bash_split` results (where description differs from command), or alone for `bash_exec_only`. | hook client / EnrichRecord | stable |
| `_thought` | string | Activity message from `reportActivity` MCP tool. Represents what the agent is doing right now. | hook client / EnrichRecord | stable |
| `_focus` | string | Mission text from `setOverallIntent` MCP tool, or from `wms_setFocus`. See note on two-focus distinction below. | hook client / EnrichRecord | stable |
| `_done` | string | Completion message from `completeActivity` MCP tool. | hook client / EnrichRecord | stable |
| `_team` | string | Team identifier for the session. Implicit in Claude Code v2.1.178+; one team per session. | hook client / EnrichRecord | stable |
| `_usage` | object | Token usage summary extracted from the session transcript on `Stop`. Fields: `input_tokens` (int), `output_tokens` (int), `cache_creation_tokens` (int), `cache_read_tokens` (int), `model` (string). | hook client / EnrichRecord | stable |
| `_warn_msg` | string | Operator warning message. Emitted for orphan dispatch events (no WMS task tracked for the session). Rendered as `[WARN]` display line in feed. | server (dispatchObservability) | stable |
| `cost_details` | object | Per-model USD cost breakdown computed from token counts via `knownModelPricing` / `computeCostDetails`. Fields: `input_cost_usd`, `output_cost_usd`, `cache_read_cost_usd`, `cache_write_cost_usd` (all float64). Written to WMS entities via `SetCost` on `Stop` events. | server (writeCostForSession) | stable |
| `usage_details` | object | Token usage extracted from `_usage` on `Stop` events. Fields: `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens` (all float64). Written alongside `cost_details` to WMS entities. | server (extractUsageDetails) | stable |

### Two-focus distinction

`_focus` carries two conceptually distinct values that happen to share the
same field:

1. **Cosmetic activity focus** — set via `setOverallIntent(message)`. Records
   what the agent is working on *right now* for the activity stream display.
   This is cheap, frequent, and affects no cost attribution.
2. **Cost-bearing WMS focus** — set via `wms_setFocus(entityID)`. Opens (or
   moves) the agent's focus interval in `wms_intervals` (`kind='focus'`), which
   the cost allocator queries to attribute spend to a WMS entity. Omitting this
   call means cost lands in `unallocated`.

Both calls write to `_focus` in the JSONL record, so the activity stream
cannot distinguish them. The critical operating point: **only `wms_setFocus`
affects cost attribution**. In a solo session, the primary agent must call
`wms_setFocus` for its work to appear in the Sankey and entity cost tables.

### Notes on `_tool_display` markers

Display strings use `__param__` double-underscore markers for dynamic values.
The source sets what is a parameter; the renderer applies styling. The feed
renders `__param__` content in cyan (#51). Never pattern-match verb prefixes
at display time.

Examples:
- `Reading __enrich.go__`
- `Created goal __int-proto__: __integrate prototype__`
- `Updating task __t-42__ → __complete__`

---

## 3. Tag Taxonomy

The 17-tag taxonomy is the primary display-category system. Each JSONL record
carries exactly one `_tool_tag` value (or none, if the event is not a tool
call). Tags are 4 characters wide (some have a leading space) so they align
in monospace columns.

Tags from MCP activity tools are set directly; all other tags are derived by
the hook client from the tool name via the `TOOL_TAGS` map, with `TOOL` as the
fallback.

| Tag | Width | Source tools / events | Description | Color (RGB) | Stability |
|-----|-------|-----------------------|-------------|-------------|-----------|
| `GOAL` | 4 | `setOverallIntent` (MCP) | Mission declaration — agent's overall intent for the session | 255,200,60 (warm gold) | stable |
| `THNK` | 4 | `reportActivity` (MCP) | Transient activity report — what the agent is doing right now | 140,170,130 (sage) | stable |
| `DONE` | 4 | `completeActivity` (MCP); `TaskUpdate` status=completed; `Stop`, `SubagentStop` (real), `TaskCompleted` events | Completion signal — task finished or turn concluded | 0,102,0 (deep green) | stable |
| `RCAP` | 4 | Phantom `SubagentStop` event (no `agent_type`, recap heuristic match) | Idle recap — Claude Code's context summary after ~3 min inactivity | 100,160,180 (muted teal) | stable |
| `READ` | 4 | `Read`, `Grep`, `Glob` | Passive file or code read | 0,0,255 (deep blue) | stable |
| `EDIT` | 4 | `Edit`, `Write`, `NotebookEdit` | Active file write or edit | 128,128,0 (olive) | stable |
| `GREP` | 4 | (reserved; `Grep` maps to `READ`) | Search variant — reserved in color table | 120,140,200 (periwinkle) | experimental |
| ` ACT` | 4 | `Bash` (with description) | Bash tool with intent description — human-readable action | 230,150,50 (amber) | stable |
| `EXEC` | 4 | `Monitor` | Background process monitoring | 180,160,60 (olive dim) | stable |
| `TEAM` | 4 | `Agent`; `SubagentStart` event | Agent lifecycle — spawning teammates | 180,120,220 (purple) | stable |
| `COMM` | 4 | `SendMessage` | Inter-agent or agent-to-human communication | 150,140,210 (lavender) | stable |
| `TASK` | 4 | `TaskCreate`, `TaskGet`, `TaskList`, `TaskUpdate`; all `mcp__wms__*` tools | Work management operations | 240,110,170 (vivid pink) | stable |
| ` WEB` | 4 | `WebSearch`, `WebFetch` | External web access | 210,130,180 (dusty rose) | stable |
| ` ASK` | 4 | `AskUserQuestion` | Human attention required | 255,100,100 (coral red) | stable |
| `PLAN` | 4 | `EnterPlanMode`, `ExitPlanMode` | Plan mode entry/exit | 160,160,220 (cool lavender-grey) | stable |
| `WARN` | 4 | `_warn_msg` field (server-side) | Operator warning — orphan dispatch or structural issue. Display-layer label (like `[EXEC]`), not a `_tool_tag` value. | 255,160,40 (orange) | stable |
| `TOOL` | 4 | Any unrecognized tool name | Fallback for tools not in the taxonomy | 160,160,160 (neutral grey) | stable |

### Tag assignment notes

- `Grep` and `Glob` both map to `READ` in `TOOL_TAGS`, not `GREP`. The `GREP`
  tag exists in the color table but is not currently assigned by any tool
  mapping. It is reserved for future use (e.g., if grep-family tools warrant
  distinct display).
- ` ACT` and ` WEB` and ` ASK` each have a leading space in their 4-char
  string. This is intentional: the space is part of the tag value used as a
  map key in `TOOL_TAGS` and `tagColors`.
- `DONE` is emitted from five distinct sources: `completeActivity` MCP,
  `TaskUpdate` with `status=completed`, the `Stop` and `SubagentStop` (real,
  with `agent_type`) hook events (first sentence of the final assistant
  message), and the `TaskCompleted` hook event (`completed #<task_id>:
  <task_subject>`).
- `RCAP` is emitted from phantom `SubagentStop` events: Claude Code fires
  `SubagentStop` with no `agent_type` for suggested next prompts (suppressed)
  and idle recaps (tagged `RCAP`). The heuristic: text starting with an
  uppercase letter and containing a space is classified as a recap.
- `SubagentStart`/`SubagentStop` fire for Agent-tool (Task tool) subagent
  spawns — Agent Teams teammates do not fire these (see the main repo's
  CLAUDE.md Pitfalls section). `TeammateIdle` (teammate idle transition) and
  `TaskCompleted` (per in_progress task at turn end) carry `teammate_name`/
  `team_name` instead of `agent_type`; `TeammateIdle` sets no `_tool_tag` —
  its only effect is refreshing the health dashboard's per-agent turn state.
- `ToolSearch` is explicitly skipped — it is internal plumbing and never
  produces a tag.
- When a Bash tool call omits the `description` parameter (`bash_exec_only`
  path), no `_tool_tag` is set. The event renders in the feed as `[EXEC]`-only
  (from `_bash_cmd`) with no `[ACT]` line. This is by-design — see field-guide
  lesson 19.
- The `[EXEC]` display label appears in two contexts: (1) as a tag for
  `Monitor` tool calls, and (2) as a hardcoded display-layer label for the
  `bash_cmd` field — rendered for ALL Bash tool calls regardless of tag,
  independently of the tag line.
- `[WARN]` is a display-layer label (like `[EXEC]` for bash_cmd), not a
  `_tool_tag` value. It renders from the `warn_msg` JSONL field, which is
  set server-side by `dispatchObservability`.

---

## 4. WMS Event Fields

WMS synthetic events (`WMSStatusChange`, `WMSFocusChange`) are posted directly
by `HookObserver` (in `wms-mcp`) rather than by the hook client. They follow
the same JSONL wire format but have a distinct `hook_event_name` and carry
additional WMS-specific fields.

### 4.1 WMS Entity Types

The entity model uses a two-level hierarchy.

| Constant | String value | Description |
|----------|-------------|-------------|
| `EntityOutcome` | `"outcome"` | Top-level initiative or goal |
| `EntityWorkUnit` | `"workunit"` | Atomic agent-level assignment under an outcome |
| `EntityInterval` | `"interval"` | Tag-target type for a `wms_intervals` row |

### 4.2 WMS Status Strings

Both Outcome and WorkUnit share the same status set:

| Status | Description |
|--------|-------------|
| `pending` | Initial state — not yet started |
| `active` | Work is in progress |
| `review` | Work complete, awaiting review |
| `done` | Terminal — work finished |
| `blocked` | Progress blocked by external dependency |

### 4.3 Valid State Transitions

Only the transitions listed here are permitted by the engine. Illegal
transitions are rejected with an error. Both entity types share identical
transition rules.

**Outcome and WorkUnit:** `pending→active`, `pending→blocked`,
`active→review`, `active→blocked`, `active→done`,
`review→active`, `review→done`, `review→blocked`,
`blocked→pending`, `blocked→active`, `blocked→review`, `blocked→done`

WorkUnit completion cascades: when all WorkUnits under an Outcome reach `done`,
the engine automatically transitions the Outcome to `done`. Outcome-to-Outcome
parent-child relationships (via dependencies) also cascade upward.

### 4.4 WMS Synthetic Event Fields

These fields appear only on `WMSStatusChange` and `WMSFocusChange` records.

| Field | Type | Description | Stability |
|-------|------|-------------|-----------|
| `wms_entity_type` | string | One of `outcome`, `workunit` | stable |
| `wms_entity_id` | string | The entity's ID string | stable |
| `wms_old_status` | string | Status before the transition | stable |
| `wms_new_status` | string | Status after the transition | stable |

`WMSFocusChange` records carry `_focus` (section 2) but no `wms_*` fields.

### 4.5 Terminal States

The `IsTerminal` predicate determines whether a `DONE` tag is emitted instead
of `TASK` for a status change event.

| Entity | Terminal statuses |
|--------|-------------------|
| outcome | `done` |
| workunit | `done` |

---

## 5. Entity Naming Conventions

These conventions govern how agents, teams, and models are referenced in
display strings and in communication between agents.

| Sigil | Form | Example | Rendering |
|-------|------|---------|-----------|
| `@` | `@agent-name` | `@store`, `@engine`, `@alice` | Bold, deterministic color derived from MD5(salt+name) |
| `#` | `#team-name` | `#wms-build`, `#proto-forge` | Bold, deterministic color derived from MD5(salt+name) |
| `<` | `<model-id>` | `<sonnet>`, `<opus>`, `<haiku>` | Dimmed, in base tag color |

Entity colors are derived via `EntityColor(name, salt)`: MD5 of salt+name
produces an RGB triple clamped to 60–210, then the dominant channel is boosted
+40 (cap 230) and the weakest channel is dimmed -30 (floor 40). The salt is
per-session so the same agent name gets consistent color within a session but
may vary across sessions.

The `nameRe` pattern that identifies entities in display text is:
```
([@#][\w-]+|<[\w.-]+>)
```

---

## 6. Inconsistencies and Open Questions

The following were observed during the audit and may warrant future cleanup:

1. **`GREP` tag is dead code.** It appears in `tagColors` (display.go:19) but
   `Grep` maps to `READ` in `TOOL_TAGS` (hook.go:46). The `GREP` color entry
   is unreachable through normal tag assignment. Either `Grep` should be
   reassigned to `GREP`, or the color entry should be removed.

2. **Leading-space tags.** ` ACT`, ` WEB`, and ` ASK` have a leading space as
   part of their tag string. This is functional (map keys match exactly) but
   fragile — a consumer normalizing whitespace would break. The convention is
   not documented anywhere in the codebase. Consider right-padding the others
   instead: `ACT `, `WEB `, `ASK `.

3. **`_session_id` vs `session_id`.** The Go client writes both `session_id`
   (raw Claude Code field) and `_session_id` (enriched copy, normalized to
   `"unknown"`). The Python client and `EnrichRecord` may not write `_session_id`.
   Feed consumers should treat `session_id` as canonical.

4. **`_model` set from two sources.** On `Stop`, `_model` is extracted from
   the session transcript and may override a value already set from
   `settings.json`. Both paths write the same field; the last write wins.
   The transcript value is preferred because it reflects the actual model used.

5. **`host` vs `_host`.** The Python client sends `host` (no underscore); the
   Go client sends `_host`. `EnrichRecord` bridges this by copying `host →
   _host` when `_host` is absent. Downstream consumers should read `_host`
   as the canonical field.

6. **`mcp__wms__listBlockers` and `mcp__wms__listDependents`** have no display
   case in either `ProcessEvent` (hook.go) or `EnrichRecord` (enrich.go). They
   will be emitted with no `_tool_tag` or `_tool_display`. This is likely an
   oversight — they should emit `TASK` tag like the other list operations.

---

## 7. Session Mode and Subagent Attribution

### 7.1 `setMode` signal and `.mode` marker

`mcp__activity__setMode(mode)` is a no-op MCP tool (like the existing activity
tools) whose real work is hook-side. When the hook client sees this call in
`PreToolUse`, it writes a per-session mode marker at
`$TEAMSTER_DEDUP_DIR/<sid[:12]>.mode`. The marker content is exactly `"solo"`
or `"team"`. Only the hook writes this file — skills emit the signal; they do
not write the file directly.

The hook reads the marker once at the top of `ProcessEvent` into `effectiveSolo`:

```
effectiveSolo = true   if marker content == "solo"  (relax all three gates)
              = false  if marker content == "team"   (enforce, beats TEAMSTER_SOLO env)
              = cfg.Solo (from TEAMSTER_SOLO env)    if no marker / stale / garbage
              = false (enforce)                      if neither source set
```

Safety invariant: only an exact `"solo"` value relaxes. Garbage, empty, or
stale content (mtime > 12h TTL) is treated as absent. A corrupt marker cannot
flip a team session to solo.

The marker is refreshed (mtime touched) on every honored read so an active
session never ages past the TTL. It is NOT removed on `Stop` (which fires
per-turn); the TTL is the sole crash-cleanup path.

### 7.2 Cost attribution in subagent mode

The cost allocator in `src/internal/rollup/rollup.go` records a `method`
value in `usage_attribution.method` for every attributed message:

| Method | Meaning |
|--------|---------|
| `temporal_join` | Agent has a direct covering focus interval at message time |
| `temporal_join_lead_fallback` | Agent had no own focus interval; cost inherited from the lead's (`""`) focus in the same session |
| `temporal_join_lead_session_fallback` | Lead had no own focus interval; cost attributed to the entity the session held focus on at that instant under any agent, preferring strategic-tier entities (Outcomes) over a teammate's narrow WorkUnit |
| `transcript_focus_recovery` | Unallocated message re-attributed by the recovery pass using the agent's intended-focus timeline from `.claude` transcripts (most-recent `wms_setFocus` at-or-before message ts, same thread). Reversible; weight stays 1.0 |
| `admin_warmup` | Pre-first-`setFocus` warmup cost re-attributed to the session's resolved Outcome under `phase=admin`. Covers the `[session start, first setFocus)` interval. Provenance in `warmup_evidence`. Reversible via `--unrecover-warmup` |
| `synthesized_outcome` | No-focus session cost re-attributed to an LLM-synthesized Outcome (produced by the orchestrator, consumed by `--synthesize-focus <mapping-file>`). Lower fidelity than `transcript_focus_recovery`. Provenance in `synthesis_evidence`. Reversible via `--unsynthesize` |
| `gap_recovery` | Lead/teammate thread gap attributed to the session's strategic Outcome by inference from other attributed messages in the same session. Provenance in `gap_evidence`. Reversible via `--unrecover-gaps` |
| `brief_directive_recovery` | Focus-less **remote teammate** cost attributed to the exact entity its dispatch brief named — the mandated `wms_setFocus(entityType, entityID)` directive the teammate was told to call first but never did. The remote scraper parses the directive from the teammate's own transcript head and ships it to `/focus-timeline` as a `brief_directive` focus interval; `--recover-directives` resolves it and re-attributes the session's `unallocated`/`sweep_skipped` cost. Deterministic (no LLM, protocol-grounded link), so higher fidelity than `synthesized_outcome`. Provenance in `directive_evidence`. Reversible via `--unrecover-directives` |
| `synthesized_remote_floor` | Remote-orphan session cost attributed by **temporal correlation** — the session had no focus interval, no brief directive, and no accessible transcript, so the hub attributed it to whatever WMS entity concurrent sessions on the same host were focused on. Lowest-fidelity deterministic method (Step 8, after `brief_directive_recovery`). Provenance in `synthesis_evidence` with `mapping_source='temporal_correlation'`. Reversible via `--unsynthesize-remote-floor` |
| `sweep_skipped` | The sweep gave up on attributing the session — either the LLM sweep examined it and found no objective (reason in `synthesis_evidence.evidence_excerpt`) or `--count-orphans` found no local transcript to synthesize from. A "tried, still unallocatable" marker: it always carries `entity_type=''` (it is written off an `unallocated` row without setting an entity). Excluded from future `--count-orphans`. Reclaimable by `brief_directive_recovery` or `synthesized_remote_floor` when a directive or concurrent focus is later found, and re-derived by `--reallocate` — whose clear-set is every entity-less row (`entity_type=''`), so a `sweep_skipped` message whose covering focus was materialized by a later identity backfill is recovered on the next allocate; otherwise re-examine manually if needed |
| `unallocated` | No covering interval found anywhere |

`temporal_join_lead_fallback` fires only when `agentName != ""` (so the lead's
own misses are not re-queried against itself) and only when the agent's own
`focusAt` returned no cover. Named teammates in team mode that have their own
focus intervals land in `temporal_join` and are never re-queried.

`temporal_join_lead_session_fallback` is the symmetric case for the lead itself
(P1a). It fires on an otherwise-unallocated lead message (`agentName == ""`)
when the lead's own `focusAt` missed. The session's covering focus under any
agent is used as the fallback, preferring strategic-tier entities (Outcomes)
over narrow WorkUnits — consistent with the lead's cross-cutting coordination
role. Runs every allocator pass; no transcript dependency. The
`usage_attribution.method` column is `VARCHAR(48)` to accommodate this label's
length.

`transcript_focus_recovery` is written by the standing re-attribution pass
(`--recover-focus` flag on `cmd/rollup`). It reads `wms_setFocus` `tool_use`
blocks from the `.claude` session transcript and re-attributes a
`method='unallocated'` row to the most-recent setFocus at-or-before the
message's timestamp on the same thread (lead thread = `agent_name=""`; optional
teammate→lead chaining mirrors `temporal_join_lead_fallback`). Replaces the
`unallocated` row for the same `message_id` (weight=1.0, never adds a second
row). Reversible: `DELETE FROM usage_attribution WHERE
method='transcript_focus_recovery'` followed by a normal allocate pass restores
the prior state. **Transcript scoping — host and username play different roles**
(`token_ledger.host` + `token_ledger.username`; `username=''` treated as the
current user for legacy rows):

- **host** is a hard filter. A session on a different host (`token_ledger.host
  != cfg.Host`, where `cfg.Host` = `TEAMSTER_HOST` else `os.Hostname()`) is
  **deferred**: the transcript is on another machine and cannot be read here.
  Recovered later by (a) running `--recover-focus` on that host, or (b) a
  future fetch-based pass.
- **username** selects the home this pass reads. A session is local when its
  `token_ledger.username` equals `cfg.User` (where `cfg.User` = `TEAMSTER_USER`
  else `os/user.Current()`) **or is `''`** — the LENIENT rule (`localToUser`):
  unstamped rows on a single-user host are the operator's own,
  and the live backlog is entirely `username=''`, so a strict match would defer
  the whole historical recovery. A local session reads `$HOME/.claude/projects`
  directly. A genuinely different **non-empty** user on the same host is
  **deferred** — its transcript lives in a home this pass does not read; the
  pass makes no speculative `/home/<username>` read (dropped in `959cfae`).

Both defer cases (other host, and same-host different non-empty user) land in
one residual: `RecoverStats.Deferred` / `DeferredMessages`, counted and logged
(`WARN` per distinct `host\x00username` with message count) — no silent swallow.

The residual categories the operator observes: **recovered** (transcript-
based), **admin_warmup** (warmup interval attributed to session Outcome under
`phase=admin`), **synthesized_outcome** (LLM-mapped no-focus sessions),
**sweep_skipped** (LLM examined, no attributable objective),
**unallocated** (no attribution source available), and **deferred** (other
host, or same host with a different non-empty user).
**Provenance:** each recovered row has a corresponding entry in the
`recovery_evidence` table (keyed by `message_id`) recording the
matched setFocus timestamp and entity — so the operator can answer "why is this
$0.16 on outcome X" with the exact transcript signal that placed it there.
Similarly, `warmup_evidence` records the warmup interval bounds
(`warmup_start`, `first_focus_at`) per `admin_warmup` message,
`synthesis_evidence` records the mapping source, confidence level, and
evidence excerpt per `synthesized_outcome` message, `gap_evidence`
records the inferred entity, thread context, and evidence count per
`gap_recovery` message, and `directive_evidence` records the brief-named
entity per `brief_directive_recovery` message.

**Brief-directive recovery (focus-less remote teammates).** A teammate
dispatched by a lead receives a `teammate-message` brief whose first
instruction is the protocol-mandated `wms_setFocus(entityType, entityID, …)`
directive (see the bootstrap skill, §"Write the technical brief"). A teammate
that does work but never actually calls `wms_setFocus` produces a focus-less
session; on a **remote** host the hub can't read its transcript, so its cost
would stay `unallocated` forever (`--recover-focus`/`--recover-warmup` defer it,
and the LLM sweep — also host-local — marks it `sweep_skipped`). The remote
`token-scraper` closes this gap deterministically: when a session has **no**
real `setFocus` but its brief carries the directive, the scraper parses the
named entity from its own transcript head and POSTs it to `/focus-timeline`
with `directive: true`. The hub writes it as a `kind='focus'` interval with
`identity_source='brief_directive'`, **subordinate** to any real focus (it is
inserted only when the session+agent has no focus interval of any source, and
the allocator's `focusAt`/`focusInSession` and `--recover-warmup`'s DB-interval
reader all **exclude** `brief_directive`, so a real `setFocus` always wins).
`rollup --recover-directives` then resolves the directive's named entity
(skipping a dangling entity) and re-attributes the whole session's
`unallocated`/`sweep_skipped` cost to it with method
`brief_directive_recovery`. It needs no transcript and is host-neutral, so it
runs on the hub for remote sessions. Reversible via `--unrecover-directives`
(deletes the attribution + `directive_evidence`; the durable `brief_directive`
interval is retained as provenance, and a follow-up allocate leaves the rows
`unallocated` because the allocator never consults directive intervals).

**Focus-interval ordering safety (dual-writer fix, extended to all closers).**
A focus interval is written by up to two paths: the hub wms-mcp
(`OpenFocusInterval`, `identity_source='direct'`, hub wall-clock ts) and — for
remotes — the token-scraper via `/focus-timeline` (`writeFocusInterval`,
`identity_source='remote_scraper'`, transcript ts). Both formerly ran an
unconditional "close any open focus interval for (session, agent) at MY ts",
so when the two ts were skewed/out-of-order (dual-writer remote) — or when the
hub's own async focus opens raced (single-writer) — one close could stamp
`ended_at < started_at`, a negative-width interval `focusAt` can never cover,
silently dropping the session's cost. The fix: (1) **all** focus-interval
closers (`closeOpenFocusIntervals`, `CloseFocusIntervalForEntity`,
`CloseSessionIntervals`) are now scoped `AND started_at <= <close-ts>`, so a
close that predates an interval's start is ignored (the interval stays open for
a later valid close) — never negative width; (2) both writers re-check the
latest open interval under `FOR UPDATE` and no-op when it is already the same
entity, so one logical setFocus yields one open interval regardless of
writer/arrival order. Hub single-writer behavior is unchanged (the lock is
uncontended; closes always follow the prior open's start). The
already-corrupted rows are healed once by `rollup --repair-focus-intervals`,
which recomputes each inverted interval's `ended_at` from its successor in the
`(session, agent)` focus chain (last one reopened), releases the affected
sessions' dropped `unallocated`/`sweep_skipped` cost, and reallocates —
idempotent, and reversible via `--unrepair-focus-intervals` (prior `ended_at`
saved in `focus_interval_repair`).

**Attribution weight vs tag fractional weight.** `usage_attribution.weight`
(always 1.0 per message today) conserves cost *across entities per message*.
The `work-type` tag fractional weight (`1/COUNT(*) OVER (PARTITION BY entity)`)
conserves cost *across tag values per entity at display time* for multi-valued
tags. These are orthogonal: recovery touches only the former.

### 7.3 Close-out warnings

When an Outcome is transitioned to `done` (via `wms_updateStatus`), the WMS
engine calls `CloseoutWarnings` (`src/internal/wms/closeout.go`) and appends
advisory text to the success response if either:

- any child work units are not in a terminal state (pending/active/review); or
- no `resolution` tag is set on the outcome.

These warnings are advisory only — the transition succeeds regardless. They
are the engine's backstop for close-out discipline, surfaced inline so the
lead doesn't skip bookkeeping. The clean close-out response (no warnings) is
byte-identical to today's response.

---

## 8. Proposed New Conventions

The attributes in this section are **not yet implemented**. They are defined
here to establish naming, type, and source before implementation begins.
`cost_details` and `usage_details` were originally proposed here but are now
implemented and documented in section 2 (Enriched Fields).

Experimental stability means: subject to change without notice. Do not build
stable consumers against these fields. They may be renamed, retyped, or
removed before being promoted to stable.

| Field | Type | Description | Source | Stability |
|-------|------|-------------|--------|-----------|
| `duration_ms` | int | Wall-clock duration of a tool call in milliseconds, measured from `PreToolUse` timestamp to `PostToolUse` timestamp for the same tool invocation. Requires the hook client to correlate Pre/Post events by tool invocation ID, which it does not currently do. | hook client | experimental |
| `agent_model` | string | Model ID of the agent that made the tool call (e.g. `claude-sonnet-4-6`). Disambiguates when multiple teammates with different models share a session. Derived from `settings.json` at startup, scoped to the individual agent rather than the session (cf. `_model`). | hook client / EnrichRecord | experimental |
| `agent_kind` | string enum | Whether the issuing agent is the session lead or a teammate. Values: `lead` (no `agent_type` in payload), `teammate` (`agent_type` present). Purely derived — no new payload field is needed from Claude Code. **Caveat:** on macOS, teammate payloads also lack `agent_type` until backfilled from the transcript `agentName` (§1.1), so this derivation must run *after* that backfill, not directly off the raw payload. | hook client / EnrichRecord | experimental |
| `entity_phase` | string enum | The agent's current execution-loop phase. Values: `implement`, `validate`, `review`, `commit`. Intended to be set explicitly via an MCP tool call, or inferred from surrounding activity patterns. Provides coarser rollup state than `_thought` for dashboard aggregation. | MCP tool (proposed) / inferred | experimental |
| `sync_source` | string enum | How a WMS state change was triggered. Values: `wms` (via wms-mcp tool call), `manual` (direct database write). Present on `WMSStatusChange` events only. Allows consumers to distinguish agent-driven transitions from administrative ones. | hookobserver | experimental |

---

## 9. Tag Keyspace

Tags in the WMS are key-value pairs attached to entities (outcomes, workunits).
Each key has a category (context or lifecycle), a cardinality constraint, and
an optional description. Tags are stored in `tag_keys`/`tag_values` tables and
attached via `entity_tags`.

### 9.0 System-Expected Tags (required)

Three tag keys are marked **required** (`required=1` in the tags table). The
sweep, classifier, cost dashboards, and recovery pipeline depend on
them. A fresh install seeds all three as required. Users can retire them via
`wms_retireTag` but should understand the consequences:

| Key | Category | What depends on it |
|-----|----------|-------------------|
| `work-type` | lifecycle | Cost-by-work-type dashboards, classifier derivation, bootstrap dispatch tagging. Without it, cost cannot be faceted by kind of work. |
| `phase` | lifecycle | Cost-by-phase dashboards, classifier phase derivation, `admin` warmup phase, sweep phase reporting. Without it, cost-by-phase views are empty. |
| `product` | context | Cost-by-product dashboards, bootstrap interview, the primary faceting dimension for "how much did product X cost." Without it, all cost appears product-less. |

The `required` flag is a **soft constraint**: the WMS engine warns when a
required tag is missing on a workunit (`warnings` field in createWorkUnit
response) but does not block creation. The intent is to guide agents toward
complete tagging, not to hard-block work. The bootstrap skill and the
focus-nudge hook reinforce these expectations at session time; the scheduled
sweep catches and classifies anything the classifier can still derive.

### 9.1 Core Context Keys

| Key | Category | Cardinality | Required | Description |
|-----|----------|-------------|----------|-------------|
| `product` | context | single | **yes** | The ongoing product or area of work (e.g. teamster, homelab). Durable — rarely changes. Set on the strategic Outcome at bootstrap; inherited by WorkUnits. |
| `feature` | context | single | no | The specific feature being built. Facet of `work-type`; exclusion group `work-scope`. |
| `bug` | context | single | no | The specific bug being fixed. Facet of `work-type`; exclusion group `work-scope`. |
| `refactor` | context | single | no | The specific refactoring purpose. Facet of `work-type`; exclusion group `work-scope`. |
| `infra` | context | single | no | The specific infrastructure work. Facet of `work-type`; exclusion group `work-scope`. |
| `docs` | context | single | no | The specific documentation effort. Facet of `work-type`; exclusion group `work-scope`. |
| `research` | context | single | no | The specific investigation or exploration. Facet of `work-type`; exclusion group `work-scope`. |
| `test` | context | single | no | The specific validation target. Facet of `work-type`; exclusion group `work-scope`. |
| `admin` | context | single | no | The specific admin task. Facet of `work-type`; exclusion group `work-scope`. |
| `component` | context | multi | no | Architectural component touched (e.g. installer, wms, dashboard). |
| `product-version` | context | single | no | Version of the product being targeted (e.g. v1.0, v2.0). |
| `user` | context | single | no | OS user that created the WMS entity. Auto-applied at creation by the wms-mcp handler (source=classifier, best-effort). |
| `source` | context | single | no | Provenance marker for WMS entities created by automated processes (e.g. `source:synthesized` for LLM-synthesized Outcomes). |

`user` is engine-applied and forward-only. When `CreatorUser` is unset, no tag
is written — `user:''` is never stored. The value is the wms-mcp **process**
user (`TEAMSTER_USER` if set, else `os/user.Current().Username`, else `$USER`),
which equals the hub operator in a single-user homelab. It is NOT necessarily
the session's interactive user in a multi-user fabric — that distinction is
deferred until remote sessions carry the session user on the MCP call.

The `user` tag and the `token_ledger.username` / `sessions.username` DB columns
are set from the same `Config.User` value by construction, so
they agree across WMS entities and telemetry rows.

### 9.2 Lifecycle Keys

| Key | Category | Cardinality | Required | Description |
|-----|----------|-------------|----------|-------------|
| `work-type` | lifecycle | multi | **yes** | Kind of work being done (feature, bug, refactor, infra, research, docs, test). The primary work classification — set at dispatch time by the lead. |
| `phase` | lifecycle | single | **yes** | Current execution phase (design, build, test, review, rework, admin). `admin` is the warmup/orientation phase before the session's first `wms_setFocus` — seeded in v37, assigned by `--recover-warmup` via synthetic state-intervals. |
| `resolution` | lifecycle | single | no | How work concluded (achieved, abandoned). Applied at close-out. |
| `priority` | lifecycle | single | no | Urgency level (p0, p1, p2, p3). |

### 9.3 Integration Key Namespaces

Integration keys are namespaced with a dot separator and seeded at setup time
via `teamster setup tags` (TUI wizard with checkboxes per integration).

| Namespace | Example keys | Purpose |
|-----------|-------------|---------|
| `github.*` | `github.repo`, `github.pr`, `github.issue` | GitHub integration |
| `jira.*` | `jira.project`, `jira.ticket` | Jira integration |
| `gitlab.*` | `gitlab.project`, `gitlab.mr` | GitLab integration |
| `git.*` | `git.branch`, `git.commit` | Git metadata |
| `linear.*` | `linear.project`, `linear.issue` | Linear integration |

### 9.4 Cardinality

- **single**: at most one value per entity per key. Setting a new value replaces the old one.
- **multi**: multiple values per entity per key (e.g. `component` can have several values).

---

## 10. Codex Runtime Conventions

Teamster records Codex CLI activity as a **second runtime** alongside Claude
Code — an identically-shaped, parallel stream that is never merged with Claude
Code data. This section is the normative field/identity reference for that
stream; the operational doc is `docs/specs/CODEX-INSTALL.md`.

### 10.1 The `runtime` enum

Every `sessions` and `token_ledger` row carries a `runtime` column (mysql/
sqlite migration v51, additive-only, default `'claude_code'` so existing rows
are unaffected):

| Value | Meaning |
|-------|---------|
| `claude_code` | Claude Code session (the default; all pre-v51 rows). |
| `codex` | Codex CLI session, written by `codex-scraper`. |
| `unknown` | An MCP connection that could not be positively identified as either (an inspector tool, another agent CLI, etc.). |

The same three values are auto-applied to freshly created WMS entities as a
`runtime` **tag** (`runtimeTag`, `src/internal/mcp/wms/wms.go`) and are
identity-mapped to the OTEL `source` resource attribute (`claude_code`/`codex`)
by the collector's `transform/source_label` processor. Codex `token_ledger`
rows additionally populate `reasoning_output_tokens` — a subset already counted
inside `output_tokens`, kept for raw-count fidelity (it prices at the output
rate, not as extra tokens).

### 10.2 Codex session identity

Codex sessions are **not** discovered through the hook pipeline (Codex hook
events are an optional channel WMS/cost must not depend on). Two independent
mechanisms carry Codex identity:

- **wms-mcp calls.** Codex sends its native per-call metadata under the
  `x-codex-turn-metadata` key on every `tools/call` `_meta` (a `session_id`,
  `thread_id`, and `model`; sent automatically, independent of hooks). Claude
  Code never sends this key. `resolveSessionID` (`internal/mcp/wms/wms.go`)
  prefers `x-codex-turn-metadata.session_id` above all other sources.
- **MCP `clientInfo.name` at initialize** distinguishes the runtimes: Claude
  Code's own client reports `claude-code`; Codex reports `codex-mcp-client`
  (empirically confirmed). A Codex connection is never eligible to fall back to
  the shared `~/.claude/current-session-id` file, so it can never adopt a live
  Claude Code session id; the installer-set `TEAMSTER_RUNTIME=codex` env var
  enforces this even if `clientInfo` drifts in a future Codex release. A
  positively-unidentifiable client buckets to `unknown-<runtime>`.

Cost/ledger rows take their identity instead from `codex-scraper` reading each
rollout file's `session_meta` (not from MCP) — see §10.3.

### 10.3 Codex subagent thread model

Codex 0.142.x's `thread_spawn` subagents each run as their own *thread* with
their own rollout JSONL file. `codex-scraper` books their spend under the
parent session so it attributes exactly like a Claude Code teammate:

| `session_meta` field | Top-level file | Subagent file |
|----------------------|----------------|---------------|
| `id` | the session's own thread UUID | the **subagent's** own thread UUID |
| `session_id` | equal to `id` | the **parent** thread's id |
| `parent_thread_id` | absent | the parent thread's id |
| `agent_role` | absent | the subagent's role (e.g. `explorer`) |

- **`session_id` (parent-resolved)** is what ledger rows and the `sessions`
  upsert use, so subagent cost books under the **same session** as the parent's
  focus intervals. On Codex 0.137.0 `session_meta` has no `session_id` field,
  so the scraper falls back to `id` (harmless — top-level 0.142.x files already
  have `session_id == id`).
- **`agent_name`** is `@<agent_role>` for a subagent (e.g. `@explorer`) and
  `""` for parent/direct spend — the same `@`-prefixed identity wms-mcp opens
  focus intervals under. The `sessions` primary key is `(session_id,
  agent_name)`, so a subagent's `(parent, @role)` row coexists with the
  parent's `(parent, "")` row, exactly like a hub Claude Code teammate.
- **`message_id`** for a Codex ledger row is `codex:<thread-id>:<seq>`, keyed
  by the file's **own** thread id (never the shared `session_id`) so sibling
  subagent files' sequence counters cannot collide onto one key. `<seq>` is
  scan-order, which is why a full re-scan reproduces identical keys (the
  `uq_message` upsert makes the re-insert a no-op).

Cost then flows through the standard attribution methods in §7.2 — there is no
Codex-specific `usage_attribution.method` value; a subagent message resolves as
a `temporal_join` (or `temporal_join_lead_session_fallback`) hit in the parent
session like any other message.
