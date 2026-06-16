# WMS Dashboard Spec v2

The current dashboard (`/wms`) is a static page dump of the MySQL database.
It shows the hierarchy but has no interactivity, no live updates, no visual
indicators of progress, and no connection to the activity stream. (Note: a
15-second meta-refresh was added on 2026-05-28 as an interim measure; the
Phase 2 SSE-driven live update approach remains the target.) This spec
describes what it should become.

## Design principles

1. **The dashboard is the operator's command center.** It answers: what's
   happening right now, what's blocked, what's done, and where is the work.
2. **Live, not stale.** The page updates without refresh. Status changes from
   the engine (rollup, unblock) appear within seconds.
3. **Connected to the activity stream.** Each entity links to its recent
   activity — the agent events that touched it.
4. **Dark terminal aesthetic.** Consistent with the activity stream page and
   feed. Monospace, high contrast, dense information.

## Data model reference

```
Outcome  (pending → active → review → done | blocked)
  └─ WorkUnit  (pending → active → review → done | blocked)
```

Both entity types share the same status set. `done` is the sole terminal status.
Each entity has: ID, title, description, status, prior_status, focus (free text),
origin_host, origin_session, origin_agent, timestamps.
WorkUnits have: outcome_id, agent_id.
Dependencies: blocker_type/blocker_id → blocked_type/blocked_id (cross-entity, cycle-detected).

## Pages

### `/wms` — Overview (the main view)

#### Header bar
- Title: "Teamster WMS"
- Nav: Activity Stream (`/`) | WMS State (`/wms`)
- Summary stats: N outcomes, N work units (M done / K blocked), N agents assigned

#### Outcome cards

Each outcome is a collapsible card. Collapsed view shows:

```
┌─────────────────────────────────────────────────────────┐
│ ● Auth Rewrite              [active]  focus: hardening  │
│   4 work units (2 ✓ 1 ◆ 1 ○)  · 2 agents               │
└─────────────────────────────────────────────────────────┘
```

- Status dot: green=active, grey=pending, red=blocked, bright green=done
- Inline progress: `2 ✓` done, `1 ◆` blocked, `1 ○` pending/active
- Agent count: distinct agent_ids assigned to work units under this outcome
- Click to expand

#### Expanded outcome

Shows work units in a flat list under the outcome:

```
▼ Auth Rewrite                          [active]
  focus: hardening token validation

    ■ Map entry points                  [done]       @auth
    ▸ Refactor token validation         [active]     @auth
    □ Update integration tests          [pending]    —
    ◆ Deploy to staging                 [blocked]    blocked by: Refactor token validation
```

Status icons:
- `■` done (filled square)
- `□` pending (empty square)
- `▸` active (filled triangle / in progress)
- `◆` blocked (diamond)
- `★` review (star)

#### Dependency visualization

Blocked tasks show their blocker inline: `blocked by: {blocker title}`.
Clicking the blocker scrolls/navigates to it.

For complex dependency graphs, a separate section at the bottom shows:
```
Dependencies
  Refactor token validation → Deploy to staging
  task-a → task-b → task-c (chain)
```

### `/wms/outcome/{id}` — Outcome detail

Full page for one outcome. Shows:
- Outcome metadata (title, status, focus, created/updated)
- All work units expanded
- Activity feed filtered to this outcome's entities (via SSE with filter)
- Focus history (if we track focus changes — currently we don't, future feature)

### `/wms/timeline` — Timeline view

Horizontal timeline showing entity state changes over time. Each row is a
work unit. Blocks are colored by status. Shows when things started,
how long they took, and where time was spent.

```
                  10:00    11:00    12:00    13:00    14:00
Map entry points  [■■■■■■■■■■■■■■■■■■]
Refactor tokens              [□□□▸▸▸▸▸▸▸▸▸▸▸▸▸▸▸▸▸▸▸▸
Update tests                                [□□□□□□□□□□□
Deploy staging                              [◆◆◆◆◆◆◆◆◆◆
```

This requires storing status change history — currently the engine logs
these via the HookObserver. The JSONL has the raw events; the timeline
reconstructs state from `[TASK] task X: old → new` events.

## Live updates

### SSE integration

The `/wms` page connects to `/events/stream` (same SSE as the activity page)
and listens for WMS events. When a `WMSStatusChange` event arrives:

1. Find the affected entity card in the DOM
2. Update its status badge, progress counters, and icon
3. Flash the card briefly (subtle highlight animation)
4. If a task auto-completed (rollup), cascade the visual update to its parent goal and project

Use htmx SSE extension for the connection. The event HTML from SSE already
contains the entity type and ID in the display text — parse it client-side
to target the right DOM element. Or: add a new SSE event type `wms-update`
that sends structured JSON instead of pre-rendered HTML, specifically for
dashboard consumption.

### Recommended approach: dual SSE events

In the HookObserver, publish two events for each status change:
1. The existing HTML-formatted event (for the activity stream page)
2. A `wms-update` JSON event (for the WMS dashboard)

```
event: message
data: <div class="event">...</div>

event: wms-update
data: {"entityType":"task","entityID":"t1","oldStatus":"active","newStatus":"complete"}
```

The activity page listens for `message` events (default). The WMS dashboard
listens for `wms-update` events and updates the DOM accordingly.

## API endpoints

Add to hookd alongside the existing `/event` and `/events/stream`:

| Method | Path | Returns |
|--------|------|---------|
| GET | `/wms` | HTML dashboard page |
| GET | `/wms/api/outcomes` | JSON array of all outcomes |
| GET | `/wms/api/outcome/{id}` | JSON outcome with work units nested |
| GET | `/wms/api/workunits?outcomeID=X` | JSON work units for an outcome |
| GET | `/wms/api/dependencies?entityType=X&entityID=Y` | JSON blockers and dependents |
| GET | `/wms/api/stats` | JSON summary stats (counts by status) |

The HTML page uses htmx to call these APIs for expand/collapse and filtering.
The JSON APIs are also useful for external tools (Grafana JSON datasource,
custom scripts, etc.).

## Implementation notes

### Database access

hookd opens the MySQL WMS database read-only (already implemented). The
`/wms/api/*` endpoints query it directly. The WMS MCP server is the only
writer — hookd never writes to the WMS store.

### No new dependencies

- htmx loaded from CDN (already done for activity page)
- Go `html/template` for server-side rendering
- `embed.FS` for static assets
- No JS framework, no build step

### Template structure

Use Go template inheritance:
- `base.html` — nav bar, dark theme CSS, htmx script tags
- `dashboard.html` — activity stream (extends base)
- `wms.html` — WMS overview (extends base)
- `wms-project.html` — project detail (extends base)

Or keep it simple: each page is self-contained with shared CSS via a
template partial.

### Status-to-color mapping (CSS)

| Status | Background | Text | Icon |
|--------|-----------|------|------|
| pending | #21262d | #8b949e | □ |
| active | #1a3a1a | #3fb950 | ▸ |
| review | #2d2a00 | #e3b341 | ★ |
| done | #0a2a0a | #3fb950 | ■ |
| blocked | #3a1a1a | #f85149 | ◆ |

### Progress counters

For each outcome, compute inline:
- Total work units
- Done work units (■)
- Blocked work units (◆)
- Active/review work units (▸/★)
- Pending work units (□)

Display as: `4 work units (2 ■ 1 ◆ 1 □)` — dense, scannable.

### Agent summary

At outcome level, list distinct agents assigned to work units:
`agents: @auth @api @frontend` — colored with entity colors (same MD5 hash
as feed uses, but output as CSS hex).

## Testing

### Unit tests
- `loadWMSData` returns correct outcome/workunit hierarchy from test MySQL database
- Progress counter computation
- Status-to-icon mapping

### Integration test
- Start hookd with a test MySQL database containing known data
- Curl `/wms` and verify HTML contains expected project/goal/task text
- Curl `/wms/api/projects` and verify JSON structure
- POST a WMS status change event, verify SSE `wms-update` event fires

### Live verification (Rule VIII)
- Use session explorer to launch Claude, create WMS entities via MCP tools
- Open `/wms` in a second session (curl or browser)
- Verify entities appear and status badges are correct
- Trigger a rollup (complete all work units under an outcome)
- Verify the outcome status updates live on the dashboard

## Implemented pages (beyond this spec)

The following hookd-served pages have been built and are live. They extend
the `/wms` namespace but were developed outside the phased plan above.

### `/wms/cost-flow` — Sankey Cost-Flow Diagram

Visual cost-flow diagram showing how spend flows from Total → Outcome →
Phase → Agent. Three switchable views. Requires a hookd restart after
upgrade to pick up the new route.

### `/wms/tags` — Tag Browser

Browse the tag vocabulary: all defined keys, their values, entity counts,
and metadata. Dark-terminal aesthetic consistent with the activity stream.

### Grafana dashboards

Ten provisioned dashboards in `skel/etc/grafana/dashboards/` (see
architecture.md for the full list). Key analytical dashboards:

- **Entity Cost Explorer** — per-entity cost table with status, model
  breakdown, and attribution coverage trend. Answers "where did the money
  go?"
- **Tag Stack Explorer** — composable drill-down by any tag keys. Template
  variables let you pick your analysis axes (e.g. product → feature → phase).
- **Cost by Tag Value** — cost breakdown by tag value across entities.

## What this spec does NOT cover

- Authentication/authorization (no users, no login)
- Multi-host aggregation (single MySQL database)
- Historical analytics (cost, duration, throughput) — future feature
- Playwright-based browser testing — deferred until harness is available
- Mobile responsiveness — terminal operators use desktops

## Scope for implementation team

Phase 1: Expand the current `/wms` page with collapsible outcome cards,
progress counters, status icons, dependency display, and agent summary.
Add the `/wms/api/*` JSON endpoints. No live updates yet.

Phase 2: Add SSE `wms-update` events and client-side DOM updates. Add the
outcome detail page.

Phase 3: Timeline view (requires status change history reconstruction from
JSONL or a new status_history table).
