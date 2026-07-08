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

## Step 3 — Create (or resume) the strategic Outcome

Before creating a new Outcome, search for existing open outcomes that match
the focus slug: extract 2-3 keywords from the slug (e.g., "fix the auth
timeout bug" → `"auth timeout"`) and call
`wms_listOutcomes(status="open", query="<keywords>")`.

If matches are found, present them in plain conversation ("Found these open
outcomes matching your focus — resume one, or start new?", listing each
candidate's id, title, and status, plus "start a new outcome" as an option)
and let the operator pick. Show the 3 most recently updated if more match,
and mention there are more.

**If the operator picks an existing outcome:**
- Skip creation — use the selected outcome as the strategic Outcome.
- If its status is `done`, reactivate to `active`
  (`mcp__wms__wms_updateOutcomeStatus`).
- Proceed to Step 4 (tags) — skip tags already present on the outcome.
- Then Step 5 (focus).

**If the operator picks "new outcome" or no matches were found:**
Call `mcp__wms__wms_createOutcome` with an id based on the focus slug. A root
Outcome (no parent) IS the strategic one — its altitude comes from DAG
position, so there is no altitude tag to apply. Set status to `active`
(`mcp__wms__wms_updateOutcomeStatus`).

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
  design/build/test/review/rework; `work-type`: feature/bug/refactor/…).
  Do NOT propose these at the Outcome interview — apply them per-WorkUnit.
- **`required`** — non-lifecycle keys required on every WorkUnit before
  close-out.
- **`engineManaged`** — engine-only keys: do not propose, set, or modify.

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

The operator confirms, corrects, or redirects. When confirmed, **you apply the
tags yourself** with `mcp__wms__wms_tagEntity` on the strategic Outcome (source
`manual`):

- Reuse an existing `(tag_key, tag_value)` whenever one fits (case-insensitive
  slug match — don't create `product:Teamster` when `product:teamster`
  exists).
- For a genuinely new *value*, pass a one-line `description` so the next
  session understands it (create-on-apply).
- For a genuinely new *dimension* (a new key the operator wants tracked), seed
  it with `mcp__wms__wms_defineTag(tagKey, category, cardinality, values,
  description)` — pick `single` or `multi` — before applying it.

### Per-entity tagging: Outcome vs. WorkUnit

Context tags on the Outcome are inherited down the DAG automatically — you do
NOT re-apply them per WorkUnit. The manifest's `scope` on each key tells you
where it belongs: `scope: "outcome"` keys go on the Outcome and inherit down;
`required` keys (e.g., `component`) must be set per-WorkUnit before close-out.

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

1. **Create it** — `mcp__wms__wms_createWorkUnit(outcomeID=<outcome>, ...)` if
   it doesn't exist yet (decompose, per above).
2. **Advance status to `active`** — `mcp__wms__wms_updateWorkUnitStatus(...
   active)`. Move it off `pending`; don't leave it parked.
3. **Move the WMS focus to it** — `mcp__wms__wms_setFocus(entityType="workunit",
   entityID=<this WU>, focus=<short what>)`. This is the **cost-bearing**
   focus (Step 5's table) — not just a `reportActivity` narration. Until you
   do this, your spend still attributes to the previous entity (or to the
   Outcome, or to nothing).
4. **Tag the `requiredLifecycle` keys for THIS WorkUnit BEFORE you start it**
   — `mcp__wms__wms_tagEntity` with `work-type` (e.g. `feature`/`docs`/`test`)
   and `phase` (`build`). Check the `requiredLifecycle` map in the manifest
   for valid values — no extra lookup needed. Tag the WorkUnit you're on, not
   a stale one. (Context tags from the Outcome are inherited automatically by
   the engine — you only need to set lifecycle tags per WorkUnit.) The engine
   lets you flip a WorkUnit to `active` (step 2) while it is still untagged and
   only *warns* afterward — so treat steps 2–4 as one atomic startup beat and
   apply the tags as you activate, never deferring them, or the warning is the
   first you'll hear of a protocol miss.

   **Work-scope slug on the Outcome.** If the strategic Outcome does not yet
   carry a work-scope slug tag and the work type is clear, apply one now with
   `wms_tagEntity` on the Outcome. The slug key matches the `work-type` you're
   setting on the WorkUnit (e.g., `work-type:bug` → `bug:<slug>` on the
   Outcome). Skip if the Outcome already has a work-scope slug from the
   interview.

As the piece moves through the loop, **update the phase tag** to match what
you're actually doing — `build` → `test` → `review` — with `wms_tagEntity` on
*this* WorkUnit.

When the piece is **finished**:

5. **Advance status to `done`** — `mcp__wms__wms_updateWorkUnitStatus(...
   done)`. This closes its focus + state intervals. A WorkUnit left at
   `active` (or worse, `pending`) reads as work that never happened.

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

## Step 7 — Close-out ritual (end of session)

The session is not done when the code works — it's done when the WMS state
reflects reality. There is no `closeoutAudit` tool to run this for you, so
walk the checklist by hand and confirm each item:

1. **No open WorkUnits** — walk the WorkUnits under the Outcome
   (`mcp__wms__wms_listWorkUnits`) and `mcp__wms__wms_updateWorkUnitStatus(...
   done)` any still `active`/`pending` whose work is finished. Nothing should
   be parked.
2. **Required tags present** — before a WorkUnit goes `done`, confirm it
   carries its `requiredLifecycle` keys (`work-type`, `phase`) and any
   `required` context key (e.g. `component`) from the Step 4a manifest. A
   done-but-untagged WorkUnit is a coverage gap the dashboard can't fill in
   later.
3. **Mark the Outcome `done`** — `mcp__wms__wms_updateOutcomeStatus(<outcome>,
   "done")`.
4. **Apply `resolution:achieved`** —
   `mcp__wms__wms_tagEntity(entityType="outcome", entityID=<outcome>,
   tagKey="resolution", tagValue="achieved")` (or the resolution that fits,
   e.g. `abandoned`). The Outcome is not closed out without a `resolution`
   tag.
5. **Focus is not left on a closed entity** — your WMS focus should not still
   point at a WorkUnit or Outcome you just marked `done`. Any tokens spent
   after close-out otherwise attribute to a closed entity.

An Outcome left `active` with a `pending` WorkUnit under it and no
`resolution` tag is the unmistakable signature of a session that forgot to
close out. The Teamster engine will **warn** you when it detects these misses
— but the warning is a safety net, not the plan: run this ritual yourself so
the engine has nothing to flag.
