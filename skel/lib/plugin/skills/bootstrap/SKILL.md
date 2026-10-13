---
name: bootstrap
description: Stand up a Teamster team for this session. Creates the team, loads WMS tools, creates the strategic Outcome, runs the context-tag interview, and teaches the lead to dispatch work — all inline, no intermediary agent.
disable-model-invocation: true
argument-hint: "[focus slug — what this team is working on]"
---

# Bootstrap a Teamster Team

Run before any work that requires parallel agents. This creates the team and
sets up WMS tracking. The lead is the orchestrator, not the implementer.
The lead owns: operator communication, strategic decisions, WMS state,
dispatch routing, tagging, and reviewing results. Implementation, testing,
bug fixing, and iteration happen in dispatched teammates — the lead creates
the brief and verifies the outcome, not the code.

> **Solo session.** This skill is for **team** work. In a solo session
> (`TEAMSTER_SOLO=1`, one primary agent) there is no team — use
> `/teamster:solo` instead: it does the same WMS setup (strategic Outcome +
> context-tag interview) inline without a team.

## Session setup — follow session-protocol.md

Read `../shared/session-protocol.md` and follow its Steps 1–9 for the
shared intake mechanics: focus slug, artifact verification, session mode,
team identity, WMS tool loading, strategic Outcome creation/resume, the
context-tag interview, focus attribution, and close-out. This file covers
only what's specific to **team** mode — everything below assumes you've
just come from (or are about to run) that shared protocol.

**Team-mode deltas on the shared steps:**

- **Step 3 (declare mode):** call `mcp__activity__setMode(mode="team")`.
- **Step 5 (load WMS tools):** also load `wms_assignWorkUnit` and
  `wms_retireTag` in the same ToolSearch batch — team mode needs both for
  dispatch routing and tag stewardship.
- **Step 6 (create/resume/rework Outcome):** before moving on to Step 7
  (tags), run the "Is this part of something bigger?" check below — it
  only applies in team mode. Runs the same regardless of which of the
  three triage branches (new/continuation/rework) Step 6 took.

If you arrived via `/teamster:start`, it already ran session-protocol
Steps 1–2 (slug + artifact verification) and the batched mode+tag
interview — skip straight to session-protocol Step 4 (team identity)
using the slug and tags start handed down. Do NOT re-ask the slug or
re-run the tag interview.

## Is this part of something bigger?

After Step 6 lands on a strategic Outcome (new, resumed, or a fresh rework
Outcome), before the tag interview (Step 7): does this session's Outcome
belong under a larger, longer-lived deliverable — a project, epic, or
initiative that will span many sessions? If the operator's framing, the
focus slug, or a recently seen Outcome title suggests one:

- If the parent Outcome already exists: `mcp__wms__wms_addOutcomeParent`
  (parentID=<the bigger one>, childID=<this session's Outcome>).
- If it doesn't exist yet but clearly should (this is session 1 of a
  multi-session effort): create it with `wms_createOutcome` and pass
  `parentOutcomeIDs` at creation time — the field already accepts this,
  you don't need `wms_addOutcomeParent` for a brand-new parent.
- If genuinely unsure, ask the operator in one line rather than guessing
  or skipping silently — a wrong parent is cheap to fix
  (`wms_removeOutcomeParent`), but a missing one is invisible in every
  future rollup.

This is optional when there's truly no larger context (a one-off fix, a
genuinely standalone session) — do not force an edge where none belongs.
The failure mode this step exists to prevent is silently defaulting to
"no parent" out of habit, not "correctly having no parent."

## Read the protocol references (first dispatch)

Once the strategic Outcome, tags, and focus are set (session-protocol
Steps 6–8) and you're about to dispatch the first teammate, read these
three files — they define how you operate as team lead for the rest of
this session:

- ${CLAUDE_SKILL_DIR}/references/dispatch-pack/eight-rules.md
- ${CLAUDE_SKILL_DIR}/references/dispatch-pack/field-guide.md
- ${CLAUDE_SKILL_DIR}/references/dispatch-pack/muster-guide.md

These load at first-dispatch time, not at session start — the interview
and Outcome/tag setup above don't need them. See
`${CLAUDE_SKILL_DIR}/references/dispatch-pack/MANIFEST.md` for the full
pack (also includes `execution-loop.md`, `rubrics.md`, and
`decomposition-guidance.md` — read those now too; the dispatch protocol
below assumes you have).

## Dispatch protocol — every time you dispatch work

As the lead, you own the full dispatch cycle. For each piece of work:

**1. Create the WMS work unit WITH its required tags in one call, BEFORE dispatching.**
`wms_createWorkUnit` takes the same inline `tags` map `wms_createOutcome`
does (`{tagKey: tagValue, ...}` or `{tagKey: {value, description}, ...}`,
applied in the same request, best-effort). Pass a clear title, `outcomeID`,
the full assignment text as `brief` (store the assignment durably in WMS,
not just in the SendMessage that follows — `wms_claimWorkUnit` hands this
`brief` back to the agent when it claims the unit, Step 3), and every key
from `requiredLifecycle` in the manifest (session-protocol Step 7a) inline
in `tags` — at minimum `work-type`
(feature|bug|refactor|polish|investigation|research|test|docs|infra|admin|processor)
and `phase=design` (or whatever phase the work starts in). Include
`component` too when it's already known at dispatch time (not required to
know it yet):

```
mcp__wms__wms_createWorkUnit(id=<id>, title=<title>, outcomeID=<outcome>,
    brief=<full assignment text>,
    tags={"work-type": "<value>", "phase": "design", "component": "<value if known>"})
```

Use the values from `requiredLifecycle` in the manifest — no drill-down
needed. Do **not** follow the create call with separate `wms_tagEntity`
calls for these keys when you already included them inline — one call
replaces what would otherwise be up to four.

`work-type` is a **required** tag: a work unit must carry it before it goes out.
The lead knows the work type at dispatch time — the agent's brief should never
say "figure out what kind of work this is." Applying it up front (not at
close-out) is what keeps the cost-by-work-type dashboards honest from the
first token. If this WorkUnit's work is substantial enough that it might later
be recognized as its own tactical Outcome rather than a WorkUnit under the
current one, say so in the WorkUnit's description now — that note is what a
later `wms_addOutcomeParent` call gets built from, and it's much cheaper to
write down at dispatch time than to reconstruct after the fact.

**Work-scope slug on the Outcome.** If the strategic Outcome does not yet carry
a work-scope slug tag (`feature:<slug>`, `bug:<slug>`, `refactor:<slug>`, etc.),
and the work type is clear, apply one now with `wms_tagEntity` on the Outcome.
The slug key must match the `work-type` you're setting on the WorkUnit — if the
WorkUnit is `work-type:bug`, the Outcome should have `bug:<slug>`. This is the
"which specific bug/feature/etc." tag that the context-tag interview may have
already set; if it did, this step is a no-op. If the Outcome already has a
different work-scope slug (e.g., `feature:auto-tag`), do NOT override it — the
Outcome's scope was set at interview time.

**Rework edges at intake.** This is the WorkUnit-level case: a specific
WorkUnit under an already-decided Outcome that redoes specific prior
delivered work. Outcome-level rework — the operator picking a `done`
Outcome at intake — is handled earlier, in session-protocol Step 6 branch
C (new Outcome + relation to the closed one); by the time you're creating
WorkUnits the Outcome-level relation, if any, already exists.

If this WorkUnit exists because of a defect in work that was already
**delivered** — not a fix made before that work shipped, which is
`phase:iterate` inside the original WorkUnit — record a
typed relation now with `mcp__wms__wms_addRelation`: `fromType`/`fromID` is
this new WorkUnit (or its Outcome), `toType`/`toID` is the prior delivered
work (or `toType="external"` with a short description in `toID` if the
antecedent isn't tracked in WMS). Call `mcp__wms__wms_listRelationKinds` for
the current kind vocabulary and pick the one that matches why this work
exists — `remediates` for a plain defect, `addresses-limitation` for "did
what was asked but the result can't be used as needed,"
`fulfills-realization` for a spec gap nobody caught, `reverts` for undoing
something that shipped in error. Kinds like `follows-up-on`, `supersedes`,
and `discovered-during` record lineage without taxing it — reach for one of
those if the new work isn't a correction of a mistake. This is the only
place rework cost comes from — there is no `phase:rework` or
`work-type:rework` anymore, and skipping this step at intake is the same as
skipping it forever: nothing reconstructs a relation later from a WorkUnit
title alone.

**2. Route by affinity.**
Check your idle teammates: which agent already has context on the files this
work touches? Send it to them. If no existing agent has affinity, spawn a new
domain-named agent:
- Name for the component/domain, not the role (Rule II)
- Match the model to the cognitive load (Rule IV)
- Give the agent a descriptive `name` for addressability (Rule II)
- Use `subagent_type: general-purpose`

Before writing the brief, apply
`references/dispatch-pack/decomposition-guidance.md`'s file-independence
check: list the files each piece of the batch will touch, confirm the sets
are disjoint, and only dispatch in parallel what survives that check.

Teammates spawning their own Agent-tool subagents for bounded sub-tasks
should use distinct `subagent_type` values when possible — the fleet view
nests sub-subagents under their spawning teammate by type. Note: CC
currently blocks `name` from teammate Agent tool calls; hookd auto-numbers
same-type siblings to ensure unique identities, so pass a meaningful
`description` — it is shown as the label after the name.

**3. Send a short pointer — the brief already lives in WMS.**
Send via `SendMessage` to the identified (or newly spawned) agent. Since the
full brief is already stored on the work unit (Step 1), the message can be
short: a pointer plus whatever context doesn't belong in WMS. Include:
- The WMS work unit ID
- This instruction: "your FIRST action is `wms_claimWorkUnit('<id>')` — this
  returns your assignment brief and transitions the work unit to active
  atomically, and requests a focus interval alongside it. The interval open
  is hookd's, asynchronous and best-effort — call `wms_setFocus` yourself if
  it doesn't land."
- Who else is working in parallel and which files they touch (shared-worktree
  rule)
- Point the teammate at `references/implementation-pack/teammate-guide.md`
  for the subset of the protocol that applies to them, instead of
  duplicating Eight Rules / execution-loop / field-guide content into the
  brief.

`wms_setFocus` remains valid as a fallback (e.g. non-WU-scoped work), but
`wms_claimWorkUnit` is the preferred path for any WU-scoped dispatch — it is
also how the agent receives its assignment, not just how it attributes cost.

**4. Advance WMS state.**
`wms_claimWorkUnit` already moved the work unit to `active` as part of the
claim — no separate status call needed here. Once active, optionally declare
its starting phase with `mcp__wms__wms_setPhase(entityType="workunit",
entityID=<id>, phase="build")` — advisory, not mandatory (the classifier
backfills any phase you don't declare, but calling it gives real-time accuracy
instead of a ~10-minute lag). As the unit moves through the execution loop,
keep declaring: `"test"` → `"review"`; `phase="iterate"` on a send-back.

**5. When agents complete work.**
The agent calls `mcp__wms__wms_deliverResult(id, summary, result)` to store
its deliverable durably in WMS — this transitions the work unit active ->
review automatically. Read the stored deliverable via
`mcp__wms__wms_listDeliverables(entityID=<work unit id>)` — take the last
row's `summary`/`result` rather than trusting the agent's final SendMessage
compression; the final message is just a headline + pointer, not the source
of truth. Once you've reviewed it, decide what's next and update WMS status
accordingly.

**If you just closed this WorkUnit `done`, check whether it was the last
one open under its Outcome** (`mcp__wms__wms_listWorkUnits(outcomeID=...)`)
— if so (and any child Outcomes are also all terminal), run
session-protocol.md's Step 9b ("Consider one Outcome") on *that* Outcome
right now. This is Step 9a's eager trigger; it only ever fires on the
Outcome the WorkUnit belongs to, never on a parent.

Re-call `wms_setFocus` on the strategic Outcome before the next dispatch so
your coordination cost for this turn stays attributed. Agents communicate
with each other directly via SendMessage for collaboration — you observe,
you do not relay (Rule VII).

## Close-out

At the end of the session, run session-protocol.md Step 9: close every
finished WorkUnit, then run 9a's end-of-session sweep — 9b's
reason-then-ask on every Outcome not already resolved by the eager trigger
in the Dispatch protocol above. This is a **consideration**, not an
automatic close — see Step 9 for why. If no specific work was requested,
tell the user the team is ready (mention the team name and focus) and ask
what they'd like to work on.

## Reference

- [session-protocol.md](../shared/session-protocol.md) — the shared intake
  mechanics this skill builds on.
- [eight-rules.md](references/dispatch-pack/eight-rules.md) — the protocol (what to do)
- [field-guide.md](references/dispatch-pack/field-guide.md) — practical lessons (what goes wrong)
- [execution-loop.md](references/dispatch-pack/execution-loop.md) — the 4-phase loop (implement, validate, review, commit)
