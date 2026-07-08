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
   "open"` (returns root/strategic outcomes that are not done — pending,
   active, review, or blocked).

2. For each outcome returned:
   - Call `mcp__wms__wms_listOutcomes` with its ID as `parentOutcomeID` and
     `status: "open"` (child/tactical outcomes)
   - Call `mcp__wms__wms_listWorkUnits` with its ID as `outcomeID`

3. Present a concise summary:

## Outcome: {title} [{status}]
Focus: {focus}

### Child Outcomes
- [{status}] {title} ({N}/{M} work units done)

### Work Units
| # | Title | Status |
|---|-------|--------|

Keep it dense and scannable. Skip empty sections.
