---
name: solo
description: Stand up a Teamster SOLO session — one primary agent, declares a team name for identity. Creates the strategic WMS Outcome directly, runs the context-tag interview with the operator, sets focus, then proceeds with work inline. Use when TEAMSTER_SOLO=1 and the work fits a single agent. For multi-agent work use /teamster:start instead.
disable-model-invocation: true
argument-hint: "[focus slug — what this session is working on]"
---

# Start a Teamster Solo Session

This is the **single-agent** counterpart to team mode. Run it when this project
is configured for solo operation (`TEAMSTER_SOLO=1` in the project's
`.claude/settings.json` env) and the work fits **one primary agent**. There is
no dispatch routing, but you still declare a team name for identity — "solo"
means one agent, not anonymous. You do the WMS bookkeeping inline, then proceed
with the work directly.

If the work genuinely needs parallel agents working different files at once,
stop and use `/teamster:start` instead — that stands up a real team. Solo mode
is for single-agent work that still wants WMS attribution, context tags, and
the dashboard.

## When solo applies

Solo mode is keyed by `TEAMSTER_SOLO=1` in the project's `.claude/settings.json`
env, resolved once at session launch (the hook reads it per event; it is fixed
for the session — there is no mid-session toggle and no per-prompt switch). To
turn solo on or off for a project, edit that project's `.claude/settings.json`
and start a fresh session; a running session keeps the mode it launched with.
Default (unset) is team-first, and the setting is per-project — a team session
in one project does not affect a solo session in another. In a solo session:

- The Eight Rules' team-coordination rules (I, III, V, VII and the
  shared-worktree section) do not apply — see the solo carve-out at the top of
  `bootstrap/references/dispatch-pack/eight-rules.md`. Rules IV (right model),
  VI (consistent naming), and VIII (verify before presenting) still apply.
- The hook does not inject the "you MUST use Agent Teams" mandate, does not nag
  you to stand up a team, and does not block a bare `Agent` spawn. You may
  spawn ephemeral subagents for bounded sub-tasks.
- You still report activity (`reportActivity` / `setOverallIntent` /
  `completeActivity`) — observability is unchanged in solo mode.

## Session setup — follow session-protocol.md

Read `../shared/session-protocol.md` and follow its Steps 1–8 for the
shared intake mechanics: focus slug, artifact verification, session mode,
team identity, WMS tool loading, strategic Outcome creation/resume, the
context-tag interview, and focus attribution. Use `mode="solo"` at Step 3.
Solo mode needs only the core WMS tool batch from Step 5 — no additions.

If you arrived via `/teamster:start`, it already ran session-protocol
Steps 1–2 (slug + artifact verification) and the batched mode+tag
interview — skip straight to session-protocol Step 4 (team identity, and
within it, **register it via 4b — do not skip this**) using the slug and
tags start handed down. Do NOT re-ask the slug or re-run the tag
interview.

Once session-protocol Step 8 (focus set on the Outcome) is done, continue
with Step 6 below — that's where solo mode's own per-step discipline
starts.

## Step 6 — Proceed with work (the per-step discipline)

You own the WMS bookkeeping — decomposing work into WorkUnits, advancing status,
refreshing focus, tagging phase, closing out. Run that discipline inline, on
**every** step. This is not
advice you can defer to the end — the value of WMS (cost-by-entity, the burn-rate
per outcome, the phase trail) only exists if you keep it current *as you work*.
The most common solo failure is doing the setup ceremony well and then never
touching WMS again — one monolithic WorkUnit stuck at `pending`, the Outcome
left `active`, WMS focus frozen on the first entity, and the whole session's
cost in `unallocated`.

### Decompose — one WorkUnit per bounded piece

As the work fans out, create a WMS WorkUnit for **each bounded piece**, not one
WorkUnit for the whole session (`mcp__wms__wms_createWorkUnit` with `outcomeID`).
"Implement, benchmark, review, re-benchmark" is **four** WorkUnits, not one. A
single-step session may need only one WorkUnit; the moment the work has distinct
phases or pieces, give each its own. A monolithic WorkUnit covering many phases
collapses the phase/work-type trail (single-cardinality tags overwrite, so only
the last value survives) and hides where the cost actually went.

### Per-work-step checklist — run this at the START of each distinct piece of work

Before you begin a WorkUnit (or dispatch a subagent for it):

1. **Create it with its required tags inline** —
   `mcp__wms__wms_createWorkUnit(outcomeID=<outcome>, ...,
   tags={"work-type": "<feature|docs|test|...>", "phase": "build",
   "component": "<value if known>"})` if it doesn't exist yet (decompose,
   per above). Check the `requiredLifecycle` map in the manifest
   (session-protocol Step 7a) for valid values — no extra lookup needed.
   Include `component` when it's already known; it's not required to be
   known at dispatch time, only before close-out. One call instead of a
   create followed by up to three separate `wms_tagEntity` calls. (Context
   tags from the Outcome are inherited automatically — this is about
   WorkUnit-level required keys, not Outcome context.) Applying required
   tags at creation, not after, is what keeps the cost-by-work-type trail
   honest; deferring them is the most common way the trail ends up patchy.
2. **Claim it** — `mcp__wms__wms_claimWorkUnit('<this WU id>')`. This is the
   **preferred** path: in one call it sets status to `active` atomically and
   requests the **cost-bearing** focus interval (session-protocol Step 8's
   table) alongside it — not just a `reportActivity` narration. The interval
   open itself is hookd's, asynchronous and best-effort; it can decline, in
   which case a follow-up nudge tells you to call `wms_setFocus` yourself.
   Until attribution actually lands, your spend still attributes to the
   previous entity (or to the Outcome, or to nothing). Fall back to manual
   `mcp__wms__wms_updateWorkUnitStatus(... active)` +
   `mcp__wms__wms_setFocus(entityType="workunit", entityID=<this WU>,
   focus=<short what>)` only if claiming isn't appropriate — e.g. resuming a
   WU that's already `active` and owned by you. **If you're resuming a
   WorkUnit created before this fix shipped and it's missing its lifecycle
   tags**, apply them the old way (`wms_tagEntity`) — there's no create
   call left to fold them into.

   **Work-scope slug on the Outcome.** If the strategic Outcome does not yet
   carry a work-scope slug tag and the work type is clear, apply one now with
   `wms_tagEntity` on the Outcome. The slug key matches the `work-type` you're
   setting on the WorkUnit (e.g., `work-type:bug` → `bug:<slug>` on the
   Outcome). Skip if the Outcome already has a work-scope slug from the
   interview.

As the piece moves through the loop, **keep declaring the phase** to match
what you're actually doing — `build` → `test` → `review` — with
`wms_setPhase` on *this* WorkUnit (`iterate` on a send-back; never `revise`
or `rework`). This is advisory: the classifier backfills gaps, but a
declared phase always wins and lands in real time instead of the
classifier's ~10-minute lag. Don't skip a phase declaration just because you
skipped narrating it; the phase trail is how the dashboard knows test
happened.

When the piece is **finished**:

3. **Deliver the result** — `mcp__wms__wms_deliverResult(id=<this WU>,
   summary=<headline>, result=<full write-up>)`. This stores the deliverable
   durably in WMS and transitions the WorkUnit `active` -> `review`
   automatically.
4. **Advance status to `done`** — `mcp__wms__wms_updateWorkUnitStatus(... done)`.
   This closes its focus + state intervals. A WorkUnit left at `active` (or worse,
   `pending`) reads as work that never happened.

5. **Check whether that was the Outcome's last open WorkUnit**
   (`mcp__wms__wms_listWorkUnits(outcomeID=<outcome>)`) — if so (and any
   child Outcomes are also all terminal), run session-protocol.md's Step 9b
   ("Consider one Outcome") on this Outcome right now, rather than waiting
   for the close-out ritual below. This is Step 9a's eager trigger.

### Verification gate (Eight Rules VIII still applies)

When a step benefits from **fresh context** — adversarial review, validation that
shouldn't be self-certified — spawn an **ephemeral** review subagent for that
bounded step, or use `/code-review` for the diff. The subagent does the one task
and exits; you don't keep a team alive. This is how solo mode preserves the
verification gate without a team: a fresh-context reviewer that isn't the author.

Commit only when the operator asks (the acceptance gate still applies).

If no specific work was named, tell the operator the solo session is ready
(mention the focus and the strategic Outcome) and ask what to work on.

## Close-out

At the end of the session, run session-protocol.md Step 9: close every
finished WorkUnit, then run 9a's end-of-session sweep — 9b's
reason-then-ask on every Outcome not already resolved by the eager trigger
above. This is a **consideration**, not an automatic close — see Step 9
for why.

## What solo mode does NOT do

- No dispatch routing or affinity — there are no peers to route to.
- No "keep teammates alive" discipline — ephemeral subagents are meant to exit.
- It does not replace team mode. If the work needs durable parallel agents on
  different files, use `/teamster:start` and stand up a real team.

Solo mode still declares a team name (session-protocol Step 4) — "solo"
means one agent, not anonymous. The team name is the canonical session
identifier in ctop, health dashboards, and Grafana.

## Reference

- [session-protocol.md](../shared/session-protocol.md) — the shared intake
  mechanics this skill builds on.
- [eight-rules.md](../bootstrap/references/dispatch-pack/eight-rules.md) — see
  the solo carve-out at the top (which rules apply in solo mode).
- [field-guide.md](../bootstrap/references/dispatch-pack/field-guide.md) —
  lesson 1 explains when a team IS needed vs. when solo suffices.
