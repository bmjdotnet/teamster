---
name: teamster-status
description: Show current WMS outcomes and work units for this project. Use when the user asks about progress, status, or what's happening.
---

# Work Status

Show the current state of all WMS outcomes and work units. (Codex sessions
have no Agent Teams layer, so there is no team roster to report — this is
WMS state only.)

## Instructions

1. Call `mcp__wms__wms_listOutcomes` with no `parentOutcomeID` and `status:
   "open"` (returns root/strategic outcomes that are not done or abandoned —
   pending, active, review, blocked, or on_hold. `on_hold` includes both a
   human's deliberate pause and anything the nightly `wms review-sweep`
   timer has parked — call this out in the summary as parked, not just
   "on_hold", when you can tell the two apart).

2. Walk the outcome tree recursively, starting from the roots (depth 0):
   - Keep a set of visited outcome IDs. Skip any outcome already visited (an
     outcome with two parents appears under both in the DAG; report it once
     in full and mark later appearances "(shown above)").
   - For each outcome, call `mcp__wms__wms_listWorkUnits` with its ID as
     `outcomeID`, and `mcp__wms__wms_listOutcomes` with its ID as
     `parentOutcomeID` and `status: "open"` (child outcomes), then repeat
     for each child.
   - Stop descending at depth 5 and note "(depth limit)" on any outcome that
     still has open children.
   - Compute each outcome's "{N}/{M} work units done" from the WorkUnits you
     actually fetched for it (N = status `done`, M = all returned); never
     infer counts from child outcomes or assume them.

3. Present a concise summary:

## Outcome: {title} [{status}]
Focus: {focus}

Indent child outcomes two spaces per depth level:

### Outcome tree
- [{status}] {title} ({N}/{M} work units done)
  - [{status}] {child title} ({N}/{M} work units done)

### Work Units (per outcome, at every depth)
| # | Title | Status |
|---|-------|--------|

Keep it dense and scannable. Skip empty sections.
