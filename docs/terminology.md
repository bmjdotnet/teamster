# Terminology

Canonical disambiguation of overlapping terms used in Teamster and Claude Code.

---

## Team/Group concepts

| Term | Context | Meaning |
|------|---------|---------|
| **Agent Team** | Claude Code | Implicit group of teammates spawned via the Agent tool within a single session. Shared session ID. One team per session; no explicit TeamCreate call needed. Dissolved at session end. |
| **Teammate** | Claude Code | An agent spawned into an Agent Team via the Agent tool. On the hub/Linux, identified by `agent_type` in hook payloads (shared lead session). On macOS, teammates run as separate top-level sessions with no `agent_type` — identity is derived from the transcript's `agentName` (see `skel/doc/specs/semantic-conventions.md` §1.1). |
| **Lead** | Claude Code / Teamster | The primary agent in a session. In team mode, the lead orchestrates teammates. In subagent mode, the lead works alone. |

---

## Work hierarchy

Teamster's work management system (WMS) uses a two-level hierarchy:

| Term | Meaning |
|------|---------|
| **Outcome** | A high-level objective or deliverable. Has a focus string, status, and tags. Outcomes are the primary unit for cost rollup and reporting. Created via `wms_createOutcome`. |
| **Work Unit** | A concrete, bounded piece of work under an Outcome. Assigned to an agent. Tracks status, focus, and cost. Created via `wms_createWorkUnit`. |
| **Claude Task** | Claude Code's built-in flat task item (via TaskCreate). Separate from WMS — no hierarchy, no cost attribution, no rollup. |

Status transitions follow a state machine: `pending` -> `active` -> `review` -> `done` (with `blocked` as an alternative). `abandoned` is a resolution tag (applied via `wms_tagEntity` with key `resolution`), not a status. Status changes cascade: completing the last Work Unit under an Outcome can trigger the Outcome's status to advance.

---

## Focus and cost attribution

| Term | Meaning |
|------|---------|
| **Focus (cost-bearing)** | Set via `wms_setFocus(entityType, entityID, focus)`. Links an agent's subsequent tool calls to a WMS entity for cost attribution. This is the primary mechanism for attributing token spend to work. |
| **Focus (cosmetic)** | Set via `setOverallIntent(message)`. Displayed as `[GOAL]` in the activity feed. Describes what the agent is doing but does NOT drive cost attribution. |
| **`/goal`** | Claude Code's built-in condition-based evaluation gate. A pass/fail predicate ("all tests pass"), NOT a focus declaration. Completely different concept. |
| **Focus interval** | A time range during which an agent was focused on a specific WMS entity. Used by the cost allocator to attribute token spend. |
| **Focus nudge** | When hookd detects an agent without a WMS focus, it injects an `additionalContext` reminder (up to 3 times) prompting the agent to call `wms_setFocus`. |

---

## Cost attribution methods

When Teamster attributes token costs to WMS entities, each attribution row
carries a `method` describing how it was derived:

| Method | Meaning |
|--------|---------|
| `temporal_join` | Standard: agent had a `wms_setFocus` active during the tool call. |
| `temporal_join_lead_fallback` | Lead or ephemeral subagent attributed via the lead's focus. |
| `admin_warmup` | Pre-first-setFocus warmup cost attributed to the session's outcome with `phase=admin`. |
| `gap_recovery` | Partial-gap session filled from existing attributions. |
| `transcript_focus_recovery` | Focus recovered by reading `.claude` session transcripts. |
| `synthesized_outcome` | LLM-assisted synthesis created an Outcome for an orphan session. |
| `unallocated` | No attribution possible — the residual bucket. |

---

## Sweep

The `rollup --sweep` command chains all recovery passes into a single run
(driven by the `teamster-rollup.timer` systemd timer). It runs entity hygiene
(drain, reclassify), then the full attribution pipeline (allocate,
recover-focus, recover-warmup, recover-gaps), then aggregation and
reconciliation. A separate `teamster-sweep.timer` gates LLM-assisted synthesis
(`claude --print /teamster:sweep`) on whether orphan sessions exist, skipping
the LLM invocation when there is nothing to process.

---

## Tags

Tags are key-value pairs attached to WMS entities. They drive cost attribution
drill-downs and dashboard analysis.

| Category | Examples | Meaning |
|----------|----------|---------|
| **Context tags** | `product`, `feature`, `bug`, `component` | What the work is about. Set by the operator or agent. |
| **Lifecycle tags** | `phase`, `work-type`, `status`, `resolution` | Where the work is in its lifecycle. Managed by the WMS engine. |
| **Integration tags** | `github.repo`, `jira.issue` | Links to external systems. Seeded during `teamster setup tags`. |
| **Source tag** | `source` | How the entity was created (e.g. `manual`, `sweep`). |

---

## Activity tags

The activity feed uses a 17-tag taxonomy to categorize tool calls:

| Tag | Source | Meaning |
|-----|--------|---------|
| `[GOAL]` | `setOverallIntent` | Agent declares session mission |
| `[THNK]` | `reportActivity` | Agent declares current turn intent |
| `[DONE]` | `completeActivity` / `TaskUpdate(completed)` / Stop | Completion signal |
| `[RCAP]` | Phantom `SubagentStop` (no `agent_type`, recap heuristic) | Idle recap |
| `[READ]` | Read/Glob | File read |
| `[EDIT]` | Edit/Write | File modification |
| `[GREP]` | Grep | File search |
| `[ ACT]` | Bash (description) | Agent's intent for a command |
| `[EXEC]` | Bash (command) | Actual command that ran |
| `[TEAM]` | Agent | Agent lifecycle — spawning teammates |
| `[COMM]` | SendMessage | Inter-agent communication |
| `[TASK]` | TaskCreate/TaskUpdate/TaskGet/TaskList | Task lifecycle |
| `[ WEB]` | WebSearch/WebFetch | Web access |
| `[ ASK]` | AskUserQuestion | Question to operator |
| `[PLAN]` | EnterPlanMode/ExitPlanMode | Plan mode |
| `[TOOL]` | Any unclassified tool | Fallback |

---

## Session mode

Teamster supports two operating modes, chosen per-session via `/teamster:start`:

| Mode | How it works |
|------|-------------|
| **Team mode** (default) | Persistent named teammates, lead owns dispatch, full Agent Teams protocol. |
| **Subagent mode** | One primary agent, ephemeral `Agent` subagents for bounded tasks, no persistent team. |

Both modes keep WMS, cost attribution, activity reporting, and dashboards fully on.
Mode is recorded via a per-session marker written by the hook on `setMode`.
