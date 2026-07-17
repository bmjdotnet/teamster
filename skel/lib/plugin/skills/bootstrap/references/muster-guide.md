# Muster — Agent Roster + Health Awareness

Muster gives you visibility into who is running and how they are doing.
Use it as part of your normal dispatch/monitor cycle, not as a separate
activity.

## What muster tracks

**Roster** — every agent registered on the hub: leads, teammates, subagents,
Codex sessions. Each has a roster_id, relationship (lead/teammate/subagent/peer),
liveness (live/idle/stale/closed/unbound), and parent linkage. Agents appear
on the roster automatically when their first hook event arrives — no
explicit registration step. A teammate that spawns its own Agent-tool
subagent registers the sub-subagent with relationship "subagent" and
parent_ref pointing to the spawning teammate (not the lead), enabling
ctop's fleet view to render a nested tree. CC currently blocks `name` from
teammate-spawned Agent tool calls, so these sub-subagents would otherwise
collide on identity; hookd auto-numbers same-type siblings (`@Explore`,
`@Explore-2`, etc.) to keep roster identities unique regardless.

**Health** — per-agent context-window usage, token totals, last activity,
and pressure level. Updated by the health-collector daemon every 15 seconds.
Context occupancy for the lead (and Agent-tool subagents) comes from Claude
Code's own statusLine channel — exact and plan-aware. That channel never
fires for in-process **Agent-Teams teammates**, so their occupancy is
instead derived from each teammate's own transcript JSONL + `.meta.json`
sidecar under `subagents/agent-<id>.*` (same formula as statusLine: most
recent assistant usage row's input + cache-read + cache-creation tokens, not
a cumulative sum). A teammate row's health data carries a `context_source`
of `transcript` (its own signal), `fallback` (borrowed from the lead's
same-model window when its own is unreadable), or `unavailable`.

## MCP surfaces

Muster exposes two hookd HTTP-MCP endpoints alongside the existing surfaces:

| Endpoint | Tools | How agents reach it |
|----------|-------|-------------------|
| `/mcp/roster` | 7 roster tools | Registered as `roster` stdio MCP server (`roster-mcp` binary, agent-callable). |
| `/mcp/health` | 4 health tools | Registered as `health` stdio MCP server (`health-mcp` binary, agent-callable). |
| `/mcp/wms` | WMS tools | Registered as `wms` stdio MCP server (agent-callable). |
| `/mcp/activity` | Activity tools | Registered as `activity` stdio MCP server (agent-callable). |

All four are registered the same way (`registerMCPServer` in
`cmd/teamster-install`, `claude mcp add-json --scope user`) — call the
`mcp__roster__*`/`mcp__health__*` tools directly like any other deferred
tool (load via `ToolSearch` first). The web dashboard and internal hookd
consumers read the same underlying data.

### Roster tools (7)

| Tool | What it does |
|------|-------------|
| `roster_listAgents` | List agents with optional filters (host, bus_team, runtime, relationship, liveness). Default scope: live/idle/unbound — closed agents excluded unless requested. |
| `roster_getAgent` | Single agent detail by roster_id. |
| `roster_resolveId` | Look up roster_id from (session_id, agent_name). |
| `registerPeer` | Register a new agent. Returns roster_id + bearer token. session_id optional (unbound peer). |
| `verifyToken` | Verify a bearer token. Returns identity if valid. |
| `roster_bindSession` | Bind an unbound roster entry to a session_id. |
| `getRosterEntry` | Get roster entry by roster_id or bus_team. |

### Health tools (4)

| Tool | What it does |
|------|-------------|
| `health_listAgents` | Per-agent health snapshot: context fill %, tokens, pressure, collector freshness. Filters by host, runtime, roster_id. |
| `health_getAgentSnapshot` | Detailed health for one agent by (host, session_id, agent_name). |
| `health_getTeamSummary` | Aggregate health for a team: total tokens, average context fill, agents in warning/critical. |
| `health_getPressureAlerts` | Agents in warning or critical pressure. |

## When to query muster

Weave these checks into your dispatch/monitor cycle:

**Before dispatching to an existing teammate.** Check its context fill. An
agent above 70% fill is approaching the ceiling where output quality
degrades. Spawn a fresh replacement and hand off context rather than
pushing a near-full agent further.

**When a teammate goes silent.** Check liveness: `idle` means between turns
(normal — it finished its last turn and is awaiting your next message),
`stale` means no activity for 5+ minutes (probably stuck or crashed). For
a stale agent, check whether its work is salvageable before spawning a
replacement.

**Periodically during long sessions.** Pressure alerts show anyone in
warning or critical. Don't wait for degradation to become visible in the
output — by then you've wasted tokens on low-quality work.

**When assembling a team.** The roster shows who is already registered,
their relationships, and parent linkage. Don't spawn duplicates of agents
that are already live.

**After completing a batch of work.** A team summary gives aggregate token
spend and context pressure across the team — useful for the operator and
for deciding whether to continue or close out.

## Liveness tiers

Liveness is computed at query time from `last_seen` recency, never stored:

| Tier | Meaning |
|------|---------|
| `live` | Active within the last 15 seconds. |
| `idle` | 15 seconds to 5 minutes since last activity. Between turns or waiting on a long tool call. |
| `stale` | Over 5 minutes, no Stop event. Probably stuck or crashed. |
| `closed` | Stop event received. Clean shutdown. |
| `unbound` | Registered but session_id not yet known (spawn-time peer). |

## Turn state

hookd tracks whether each agent is `processing` (mid-turn, between
UserPromptSubmit and the next idle point) or `idle` (turn complete). This is
exposed as `is_processing` on every `health_listAgents`/`health_getAgentSnapshot`
row. For an in-process Agent-Teams teammate, `TeammateIdle` is the only push
signal for the idle transition — without it a teammate between turns would
read as `processing` for its entire life (turn state otherwise only flips on
`Stop`). `TaskCompleted` (a finished in-progress task at turn end) is
log-only — it updates no roster/session state.

## Context pressure — self-monitoring

The lead is not exempt from context ceilings. In long sessions:

- Track your own context usage. When approaching 70% fill, externalize
  accumulated context to a handoff doc before quality degrades.
- Consider whether to continue or spawn a fresh lead session with the
  handoff doc as input.
- A teammate at 70%+ fill should be reaped: externalize its knowledge,
  spawn fresh, route by affinity to the new agent.
