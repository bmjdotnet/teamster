---
name: teamster-solo
description: Stand up a Teamster solo session — one primary agent, no team. Creates the strategic WMS Outcome directly, runs the context-tag interview with the operator, sets focus, then proceeds with work inline. Use when the operator wants Teamster's WMS bookkeeping (cost attribution, tags, the dashboard) for this session's work. Explicit invocation only — mention $teamster-solo.
---

# Start a Teamster Solo Session

Codex sessions have no Agent Teams layer, so every Codex session working
under Teamster is inherently solo: **one primary agent, no team, no dispatch
routing.** You do the WMS bookkeeping inline, then proceed with the work
directly. (Codex's own subagents — spawned when you explicitly ask for one —
are fine to use for bounded sub-tasks; they're ephemeral, spawn/wait/collect,
not a persistent team, and nothing here restricts them.)

Run this at the start of a session that involves non-trivial work (not just a
quick question). Skip it for throwaway conversations.

## Step 1 — Get the focus slug

A focus slug describes what this session is working on. It seeds the
strategic Outcome's id and frames the context-tag interview.

- If the user's message (beyond the `$teamster-solo` mention) already
  describes the work, derive the slug from that.
- Otherwise, ask in plain conversation: "What is this session focused on? (a
  short phrase)" — offer 2-3 plausible guesses inferred from the
  conversation, recent files, and the working directory, and let the operator
  correct or replace them.

There is no team name to generate — solo sessions have no team.

### Verify referenced artifacts before proceeding

If the brief or focus slug names specific local files, paths, or kits (e.g.
"apply the fix from the pricing kit," "pick up the devkit at
`/path/to/thing`"), verify each one exists — `ls` the path or read the file
— before treating it as real. If something named is missing, say so and
confirm intent with the operator (wrong path? not built yet? proceed
without it?) rather than silently assuming it exists or improvising its
contents. A session that burns its first several turns acting on a kit that
was never actually delivered is a worse outcome than a 10-second existence
check up front.

## Step 2 — Reach the WMS and activity tools

Teamster's `wms` and `activity` MCP servers are registered at install time. On
some Codex builds their tools are callable directly; on builds that defer-load
MCP tools you must surface a tool before first use with Codex's own tool
search. If a tool you expect (`wms_createOutcome`, `reportActivity`, …) isn't
callable, search for **what you want to DO** in natural-language verbs —
"create a new outcome," "set focus on an entity" — not a bare `wms_` tool name
(identifier queries reliably return zero). Search again with different wording
before concluding a tool is missing. See AGENTS.md → "Finding WMS/activity MCP
tools" for the full guidance.

### WMS entity model

```
Outcome (what you're trying to achieve) — DAG, can have multiple parents
  +-- WorkUnit (bounded work assigned to an agent) — always under one Outcome
```

Strategic outcomes are root nodes (no parent); tactical outcomes are
nested children (pass `parentOutcomeIDs`). WorkUnits are concrete, bounded
work items.

### WMS state machine (use EXACTLY these strings)

```
Outcome:   pending -> active -> review -> done | abandoned
WorkUnit:  pending -> active -> review -> done | abandoned
Non-terminal (pending/active/review/blocked/on_hold) <-> blocked
Non-terminal (pending/active/review/blocked/on_hold) <-> on_hold
Non-terminal (pending/active/review/blocked/on_hold) -> abandoned   [terminal, one-way]
done -> review                                                     [the only reopen edge]
```

This diagram shows the intended flow (deliver, then advance) — it is not an
exhaustive edge list. `active -> done` and `blocked -> done` are also legal
(the engine allows completing directly from either), the ritual above just
doesn't route you there. `done` and `abandoned` are both terminal — neither
has any outbound transition except `done -> review`. "complete", "achieved",
"assigned", "planning", "open", "closed", "in_progress" are NOT valid status
strings. NEVER run SQL directly against the WMS database. All state changes
go through MCP tools.

**Reopening a `done` entity.** `done -> review` is the sole way back — it
reuses `review`'s existing exits (`active`, `done`, `blocked`) instead of
adding a separate edge for each. `review` is also the state most likely to
sit idle: follow the reopen with the transition you actually intend
(usually `review -> active`) in the same turn, rather than leaving a
freshly-reopened entity parked in `review`. The reopen also clears any
`resolution` tag the entity carried — the old value is preserved in the
journal, not lost — so closing it again needs a fresh resolution.

**`abandoned` is a status, not a `resolution` tag.** Use it when the work
was dropped rather than finished (Step 7 below) — it counts as neither
"done" nor "open" in status summaries. `on_hold` is for a WorkUnit or
Outcome paused by choice, not stuck on a dependency (that's `blocked`).

**`on_hold` also has a second, automated source: the nightly
`wms review-sweep` timer.** A WorkUnit left sitting in `review`, or an idle
Outcome with no live work under it, gets parked to `on_hold` after
`ReviewSweep.OlderThan` (default 168h/7 days) of no activity — reversible
through the ordinary tools, no SQL: `wms_updateWorkUnitStatus`/
`wms_updateOutcomeStatus` back to `active` or `review` rescues it. The
sweep never touches a human-set `on_hold` at any age — only an entity it
parked itself, and only after a further `ReviewSweep.AbandonAfter` (default
720h/30 days) of total silence, moves on to `abandoned`, which is one-way.
If you notice a WorkUnit or Outcome you're still actively working sitting
in `on_hold`, reactivate it — don't assume someone meant to pause it. A
WorkUnit the sweep closed `done` because a deliverable existed but nobody
reviewed it carries `resolution:swept-unreviewed` — a third resolution
value, alongside `achieved`, that says nothing about whether the work was
actually good.

**Creating a WorkUnit under a terminal Outcome is rejected.** An Outcome
that is `done` or `abandoned` does not silently accept new work under it —
the create call fails with a message pointing at the `done -> review`
reopen edge. Reopen the Outcome first if the new WorkUnit genuinely
belongs there; otherwise it belongs under a different Outcome.

## Step 3 — Create (or resume) the strategic Outcome

Before creating a new Outcome, search **all** outcomes matching the focus
slug, not just open ones — a match on a `done` outcome is exactly the
rework-detection signal this step needs. Extract 2-3 keywords from the slug
(e.g., "fix the auth timeout bug" → `"auth timeout"`) and call
`wms_listOutcomes(query="<keywords>")` (omit `status` so both open and
`done` outcomes surface).

If matches are found, present them in plain conversation ("Found these
outcomes matching your focus — continue existing work, rework something
closed, or start new?", listing each candidate's id, title, and status, plus
"start a new outcome" as an option) and let the operator pick. Show the 3
most recently updated if more match, and mention there are more. The status
in each candidate (`active`, `done`, etc.) tells you which branch below
applies.

**If the operator picks an existing outcome whose status is NOT `done` or
`abandoned`** (`pending`, `active`, `review`, `blocked`, or `on_hold`) —
continuation:
- Skip creation — use the selected outcome as-is as the strategic Outcome.
  Do not change its status.
- Proceed to Step 4 (tags) — skip tags already present on the outcome.
- Then Step 5 (focus).

**If the operator picks an existing outcome whose status IS `done` (or
`abandoned`)** — rework. **Never reactivate a closed Outcome.** Flipping
`done`/`abandoned` back to `active` erases the fact that the prior work
actually shipped or was dropped. `done` has a legal reopen edge (`done ->
review`) for correcting the prior Outcome itself — a separate, deliberate
act from starting new or follow-on work, and not what this branch is for.
The reopen also clears any `resolution` tag the entity carried — the old
value is preserved in the journal, not lost — so closing it again needs a
fresh resolution. Instead:
1. Mint an id for a **new** Outcome (based on the focus slug, as in the
   "new outcome" case below) — this is the strategic Outcome for this
   session, not the closed one. **Do not create it yet** — creation
   happens in Step 4c.
2. Decide the relation kind now, so it's ready the moment the new Outcome
   exists — which `kind` matches why this work exists (call
   `mcp__wms__wms_listRelationKinds` if not already called this session):
   `remediates` (the delivered outcome had a defect), `addresses-limitation`
   (it did what was asked but the result can't be used as needed now),
   `fulfills-realization` (a requirement nobody identified at the time), or
   `supersedes` (requirements, scale, or environment changed; the prior work
   was right for its time — not taxed). If it's not obvious from the
   operator's framing, ask in one line ("is this fixing something that
   shipped broken, or building on something that's since changed?") rather
   than defaulting to a taxable kind.
3. Proceed to Step 4 (tags) as a **new** Outcome — run the tag interview
   fresh. Context tags do not carry over from the closed prior outcome.
4. Once Step 4c creates the new Outcome, immediately record the relation
   edge decided in step 2: call `mcp__wms__wms_addRelation` with
   `fromType="outcome"`, `fromID=<the new outcome>`, `toType="outcome"`,
   `toID=<the done/abandoned outcome>`, `kind=<decided above>` — before
   proceeding to Step 5. It needs the new Outcome's real id, which exists
   only after 4c's create call.

**If the operator picks "new outcome" or no matches were found:**
Mint an id for the new Outcome from the focus slug, but **do not create it
yet** — run the Step 4 tag interview first, then create it there with the
confirmed tags inline (Step 4c). A root Outcome (no parent) IS the
strategic one — its altitude comes from DAG position, so there is no
altitude tag to apply.

## Step 4 — The context-tag interview

> **When resuming an existing outcome:** it may already have context tags
> applied. Call `wms_getOutcome` to check, and only propose tags that aren't
> already set. Skip the interview entirely if the outcome is fully tagged.

This is the genuinely valuable part of session setup. **Tag classification IS
the goal-setting conversation:** a session that knows it's working a "p1
feature for teamster" operates differently than one with no context.

### 4a — Inspect the key manifest

Call `mcp__wms__wms_listTags` (no args) to get the role-shaped manifest. The
response groups keys by role — no interpretation needed:

- **`propose`** — keys to offer the operator. `values` lists options (when
  present); `n` means drill down with `wms_listTags(tagKey=...)`. Respect
  `exclusive` (at most one key per exclusion group). Apply `scope: "outcome"`
  keys to the Outcome; keys without scope can go on either.
- **`autoExtract`** — extract silently from the environment (git, env).
- **`requiredLifecycle`** — lifecycle keys you MUST apply to every WorkUnit
  before starting it. Values are included (e.g. `phase`:
  design/build/test/review/iterate). Which keys land here is the manifest's
  decision; read it each time instead of assuming a layout.
  Do NOT propose these at the Outcome interview — apply them per-WorkUnit.
- **`required`** — non-lifecycle keys required on every WorkUnit before
  close-out (e.g. `product`, `work-type`; check the manifest).
- **`engineManaged`** — engine-only keys: do not propose, set, or modify.
  The engine is confirmed the only writer.
- **`ritualManaged`** — also skips the interview like `engineManaged`, but a
  documented ritual sets these by hand — e.g. `resolution`, which Step 7's
  close-out ritual sets via `wms_tagEntity`. Set them when the ritual calls
  for it; do not propose them in the interview.

### 4b — Extract and propose

**Auto-apply:** For each key in `autoExtract`, extract its value from the
named source (run `git remote -v`, `git branch --show-current` for `git`
sources; read the repo's AGENTS.md/CLAUDE.md for project metadata). Apply
silently in Step 4c.

**Propose:** From the `propose` group, infer values using the focus slug,
project docs, repo name, and existing vocabulary. Present with source
attribution, e.g.:

```
Based on your focus "fix auth bug" and the git remote, I'd tag the
strategic Outcome with:

  product:webapp        — from this repo's docs
  bug:auth-timeout      — from your focus slug
  priority:p1           — no urgency signal, defaulting high

I'll also auto-apply: github.owner:acme, github.repo:webapp,
git.branch:main (extracted from git).

Sound right?
```

Respect `exclusive` — propose only one key per exclusion group. Apply
`scope: "outcome"` keys to the Outcome; note `requiredLifecycle` and
`required` keys for WorkUnit dispatch time.

### 4c — Confirm and apply

The operator confirms, corrects, or redirects. **When confirmed and the
Outcome doesn't exist yet** (the "new outcome"/rework branches above): create
it now, with the confirmed tags passed inline:
```
mcp__wms__wms_createOutcome(id=<minted in Step 3>, title=<...>, description=<...>,
    status="active",
    tags={<confirmed tag set>})
```
Rework branch: immediately after this call, record the relation edge decided
in Step 3's rework-branch step 2 — it needs this Outcome's real id.

**When resuming an existing Outcome** (continuation branch): apply tags the
old way, serially — `mcp__wms__wms_tagEntity` on the strategic Outcome
(source `manual`) for each confirmed tag not already present.

Either way:
- Reuse an existing `(tag_key, tag_value)` whenever one fits (case-insensitive
  slug match — don't create `product:Teamster` when `product:teamster`
  exists).
- For a genuinely new *value*, use the `{value, description}` form inline
  (new-outcome/rework) or pass `description` to `wms_tagEntity`
  (continuation) — same create-on-apply semantics either way.
- For a genuinely new *dimension*, seed it with `mcp__wms__wms_defineTag`
  before applying/creating.

### Per-entity tagging: Outcome vs. WorkUnit

Context tags on the Outcome are inherited down the DAG automatically — you do
NOT re-apply them per WorkUnit. The manifest's `scope` on each key tells you
where it belongs: `scope: "outcome"` keys go on the Outcome and inherit down;
`required` keys (as listed in the manifest) must be set per-WorkUnit before close-out.

**Reuse existing values.** Always call `wms_listTags` before inventing new tag
values. If an existing value fits (case-insensitive), use it.

## Step 5 — Set focus on the Outcome

Call `mcp__wms__wms_setFocus(entityType="outcome", entityID=<the Outcome id>,
focus=<short what>)`. This attributes your token cost to this Outcome. In
solo mode the one primary agent's focus is the session's focus — there are no
peers to attribute against.

### The two "focus" notions — do not confuse them

There are **two different things called "focus"**, and only one of them
drives cost. Refreshing the wrong one is the single easiest discipline to get
wrong in a solo session (it leaves your whole session's cost in the
`unallocated` bucket):

| | What it is | Tool | What it affects |
|---|---|---|---|
| **Activity focus** | A narration string in the live feed | `mcp__activity__reportActivity` / `setOverallIntent` | **Cosmetic only** — the feed display. Drives **no** attribution. |
| **WMS focus** | The cost-bearing focus interval on a WMS entity | `mcp__wms__wms_setFocus` | **Cost attribution** — every token you spend lands on the entity your WMS focus currently points at. |

Updating your `reportActivity` message does **not** move the WMS focus — they
are separate calls against separate state. As your work moves from one entity
or step to the next you must call `mcp__wms__wms_setFocus` **again** — not just
narrate the move with `reportActivity`. If you only refresh the activity
narration, the WMS focus stays frozen on whatever it last pointed at and your
spend mis-attributes.

## Step 6 — Proceed with work (the per-step discipline)

You own the WMS bookkeeping — decomposing work into WorkUnits, advancing
status, refreshing focus, tagging phase, closing out. Run that discipline
inline, on **every** step. This is not advice you can defer to the end — the
value of WMS (cost-by-entity, the burn-rate per outcome, the phase trail)
only exists if you keep it current *as you work*. The most common solo
failure is doing the setup ceremony (Step 3-5) well and then never touching
WMS again — one monolithic WorkUnit stuck at `pending`, the Outcome left
`active`, WMS focus frozen on the first entity, and the whole session's cost
in `unallocated`.

### Decompose — one WorkUnit per bounded piece

As the work fans out, create a WMS WorkUnit for **each bounded piece**, not
one WorkUnit for the whole session (`mcp__wms__wms_createWorkUnit` with
`outcomeID`). "Implement, benchmark, review, re-benchmark" is **four**
WorkUnits, not one. A single-step session may need only one WorkUnit; the
moment the work has distinct phases or pieces, give each its own. A
monolithic WorkUnit covering many phases collapses the phase/work-type trail
(single-cardinality tags overwrite, so only the last value survives) and
hides where the cost actually went.

### Per-work-step checklist — run this at the START of each distinct piece of work

Before you begin a WorkUnit (or spawn a subagent for it):

1. **Create it with its required tags inline** —
   `mcp__wms__wms_createWorkUnit(outcomeID=<outcome>, ...,
   tags={<every key the manifest lists under requiredLifecycle and required,
   with valid values>})` if it doesn't exist yet (decompose, per above).
   Call `wms_listTags` and follow the manifest's role groups rather than
   assuming a fixed layout: which keys sit under `requiredLifecycle` (e.g.
   `phase`) versus `required` (e.g. `product`, `work-type`) is the
   manifest's call and can change. Never set keys the manifest lists under
   `engineManaged`, `component` included when it is listed there; the
   engine writes those itself. One call instead of a create followed by up
   to three separate `wms_tagEntity` calls. (Context
   tags from the Outcome are inherited automatically — this is about
   WorkUnit-level required keys, not Outcome context.) Applying required
   tags at creation, not after, is what keeps the cost-by-work-type trail
   honest; deferring them is the most common way the trail ends up patchy.

   **Work-scope slug on the Outcome.** If the strategic Outcome does not yet
   carry a work-scope slug tag and the work type is clear, apply one now with
   `wms_tagEntity` on the Outcome. The slug key matches the `work-type` you're
   setting on the WorkUnit (e.g., `work-type:bug` → `bug:<slug>` on the
   Outcome). Skip if the Outcome already has a work-scope slug from the
   interview.
2. **Claim it** — `mcp__wms__wms_claimWorkUnit('<this WU id>')`. This is the
   **preferred** path: in one call it sets status to `active` atomically and
   requests the **cost-bearing** focus interval (Step 5's table) alongside
   it — not just a `reportActivity` narration. The interval open itself is
   hookd's, asynchronous and best-effort; it can decline, in which case a
   follow-up nudge tells you to call `wms_setFocus` yourself. Until
   attribution actually lands, your spend still attributes to the previous
   entity (or to the Outcome, or to nothing).
   Fall back to manual `mcp__wms__wms_updateWorkUnitStatus(... active)` +
   `mcp__wms__wms_setFocus(entityType="workunit", entityID=<this WU>,
   focus=<short what>)` only if claiming isn't appropriate — e.g. resuming a
   WU that's already `active` and owned by you. **If you're resuming a
   WorkUnit created before this fix shipped and it's missing its lifecycle
   tags**, apply them the old way (`wms_tagEntity`) — there's no create
   call left to fold them into.

As the piece moves through the loop, **update the phase tag** to match what
you're actually doing — `build` → `test` → `review` — with `wms_tagEntity` on
*this* WorkUnit.

When the piece is **finished**:

3. **Deliver the result** — `mcp__wms__wms_deliverResult(id=<this WU>,
   summary=<headline>, result=<full write-up>)`. This stores the deliverable
   durably in WMS and transitions the WorkUnit `active` -> `review`
   automatically.
4. **Advance status to `done`** — `mcp__wms__wms_updateWorkUnitStatus(...
   done)`. This closes its focus + state intervals. A WorkUnit left at
   `active` (or worse, `pending`) reads as work that never happened.
5. **Check whether that was the Outcome's last open WorkUnit**
   (`mcp__wms__wms_listWorkUnits(outcomeID=<outcome>)`) — if so (and any
   child Outcomes are also all terminal), run Step 7's close-out
   consideration on this Outcome right now, rather than waiting for
   end-of-session.

### Verification gate

When a step benefits from **fresh context** — adversarial review, validation
that shouldn't be self-certified — spawn a subagent for that bounded step and
have it exit when done, rather than self-certifying your own work. This is
how solo mode preserves the verification gate without a team: a
fresh-context reviewer that isn't the author.

A subagent needs no WMS bookkeeping of its own — its token cost rolls up to
the session and attributes to whatever WMS focus you hold while it runs. Keep
focus on the WorkUnit the subagent is helping with and its spend lands there
automatically; you do not set focus for the subagent.

Commit only when the operator asks (the acceptance gate still applies).

If no specific work was named, tell the operator the solo session is ready
(mention the focus and the strategic Outcome) and ask what to work on.

## Step 7 — Close-out: consider, don't assume

The session is not done when the code works — it's done when the WMS
state reflects reality. There is no `closeoutAudit` tool to run this for
you. This step used to auto-apply `done` + `resolution:achieved`
unconditionally once every WorkUnit was finished — that's gone
(wms-hygiene-kit README §5 R1): we don't waterfall projects, and it's
unlikely every WorkUnit is known from the start, so "everything's
terminal" is no longer license to close the Outcome by itself. Reason
about it, form a recommendation, then ask.

1. **No open WorkUnits** — walk the WorkUnits under the Outcome
   (`mcp__wms__wms_listWorkUnits`) and `mcp__wms__wms_updateWorkUnitStatus(...
   done)` any still `active`/`pending` whose work is finished. Confirm each
   carries every `requiredLifecycle` and `required` key the manifest
   lists, and any context key before it goes `done`. (You should already have
   done this eagerly per the per-work-step checklist's own eager-trigger
   step, the moment each WorkUnit's close made it the last one under its
   Outcome — this walk is the end-of-session catch-all for anything that
   wasn't. The engine now appends a one-line close-out readiness hint
   (permanent, not the focus nudge; exact text in `semantic-conventions.md`
   §7.4) to that status-change response when it happens — paraphrased: "all
   N work units under the outcome are now terminal, it does not close
   automatically, run the close-out consideration" — treat it as
   confirmation, not a substitute for the walk here.)
2. **For each Outcome not already resolved by the eager trigger above**
   (*resolved* includes an Outcome the human answered "Not yet" on and
   whose own state has not changed since) — this session's strategic
   Outcome and any child Outcomes it created — reason first: is every
   WorkUnit under it terminal (`done` or `abandoned`), and (if it has
   any) is every child Outcome also terminal?
   If not, **say nothing and leave it open** — a partially-done Outcome is
   the normal state. Never walk up to a *parent* just because this Outcome
   or a sibling *looks* ready — see step 4's propagation rule below, which
   only fires *after* an actual confirmed close, never on mere
   completeness. Never treat an `outcome_relations` edge
   (`remediates`/`supersedes`/etc.) as evidence of readiness either —
   that's lineage, not DAG structure.
3. **If it's ready, ask — don't close it silently.** Codex has no
   `AskUserQuestion`; ask in plain conversation and wait for a reply:
   - Close as achieved (default recommendation — state your one-line
     reasoning, e.g. "last WorkUnit just delivered, nothing signals this
     was dropped")
   - Close as abandoned (the work was dropped, not finished)
   - Not yet (leave it open)
   - Talk it through first
4. **Apply the answer:**
   - **Achieved** — `mcp__wms__wms_updateOutcomeStatus(<outcome>, "done")`
     + `mcp__wms__wms_tagEntity(entityType="outcome", entityID=<outcome>,
     tagKey="resolution", tagValue="achieved")`.
   - **Abandoned** — `mcp__wms__wms_updateOutcomeStatus(<outcome>,
     "abandoned")`. No separate `resolution` tag — the status itself
     carries the meaning.
   - **Not yet** — no WMS write. Don't re-ask later in the same session
     unless the Outcome's own state actually changed (another WorkUnit
     or child Outcome went terminal).

   **After Achieved or Abandoned — propagate to the parent(s).** Call
   `mcp__wms__wms_getOutcome(id=<the Outcome just closed>)` and read
   `parent_ids`. For each parent, run this same steps-1-through-4
   evaluation on it fresh, immediately — not deferred to end-of-session.
   If a parent also closes, repeat for *its* parent(s). Stop at "not yet,"
   "talk it through," or no parents left. This is the one-hop,
   confirmation-gated propagation that reaches a multi-level DAG's higher
   Outcomes — without it, an Outcome whose readiness depends on a *child*
   Outcome closing (not a WorkUnit closing directly under it) is never
   considered at all.
5. **Focus is not left on a closed entity** — your WMS focus should not
   still point at a WorkUnit or Outcome you just marked `done` (or
   `abandoned`). Tokens spent after close-out otherwise attribute to a
   closed entity.

The engine still **warns** (`CloseoutWarnings`) when an Outcome is marked
`done` with open children or no `resolution` tag — that's a backstop for a
slip in this ritual, not the mechanism itself; this reasoning step is what
should make the warning fire rarely.
