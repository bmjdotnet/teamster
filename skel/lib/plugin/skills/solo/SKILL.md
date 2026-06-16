---
name: solo
description: Stand up a Teamster SOLO session — one primary agent, no team. Creates the strategic WMS Outcome directly, runs the context-tag interview with the operator, sets focus, then proceeds with work inline. Use when TEAMSTER_SOLO=1 and the work fits a single agent. For multi-agent work use /teamster:start instead.
disable-model-invocation: true
argument-hint: "[focus slug — what this session is working on]"
---

# Start a Teamster Solo Session

This is the **teamless** counterpart to team mode. Run it when this project is
configured for solo operation (`TEAMSTER_SOLO=1` in the project's
`.claude/settings.json` env) and the work fits **one primary agent**. There is
no team, no dispatch routing. You — the one primary agent — do the WMS
bookkeeping inline, then proceed with the work directly.

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
  `bootstrap/references/eight-rules.md`. Rules IV (right model), VI (consistent
  naming), and VIII (verify before presenting) still apply.
- The hook does not inject the "you MUST use Agent Teams" mandate, does not nag
  you to stand up a team, and does not block a bare `Agent` spawn. You may
  spawn ephemeral subagents for bounded sub-tasks.
- You still report activity (`reportActivity` / `setOverallIntent` /
  `completeActivity`) — observability is unchanged in solo mode.

## Step 1 — Get the focus slug

A focus slug describes what this session is working on. It seeds the strategic
Outcome's id and frames the context-tag interview.

- If `$ARGUMENTS` is non-empty, use it as the focus slug.
- Otherwise, ask with AskUserQuestion:
  - question: "What is this solo session focused on? (a short phrase)"
  - header: "Session focus"
  - options: one option per plausible focus you can infer from the
    conversation, recent files, and the working directory (2–3 max). The
    operator can always pick "Other" and type their own.

There is no team name to generate — solo sessions have no team.

**Set the session mode (first action).** Load and call the mode signal so the
Teamster hook records that this is a subagent session (this is what relaxes the
team-dispatch mandate and the bare-`Agent` block for this session):

```
ToolSearch("select:mcp__activity__setMode")
mcp__activity__setMode(mode="solo")
```

`setMode` is a no-op confirmation tool; the hook does the real work. Call it
**once**, before any other work — not every turn; the hook keeps the marker
fresh on its own while the session is active. (If you arrived here via
`/teamster:start`, it already called `setMode("solo")` — calling it again is
harmless, the hook is idempotent.)

## Step 2 — Load the WMS tools

Load the WMS tools in one
batch (these are deferred tools — load before first use):

```
ToolSearch("select:mcp__wms__wms_createOutcome,mcp__wms__wms_createWorkUnit,mcp__wms__wms_updateOutcomeStatus,mcp__wms__wms_updateWorkUnitStatus,mcp__wms__wms_listWorkUnits,mcp__wms__wms_setFocus,mcp__wms__wms_tagEntity,mcp__wms__wms_listTags,mcp__wms__wms_defineTag")
```

Do this once. Do not call ToolSearch for WMS tools again after this.

## Step 3 — Create the strategic Outcome

Call `mcp__wms__wms_createOutcome` with an id based on the focus slug. A root
Outcome (no parent) IS the strategic one — its altitude comes from DAG position,
so there is no altitude tag to apply. Set status to `active`
(`mcp__wms__wms_updateOutcomeStatus`).

This is the same strategic Outcome the team-mode flow creates.

## Step 4 — The context-tag interview

This is the genuinely valuable part of session setup, and it applies fully in
solo mode. **Tag classification IS the goal-setting conversation:** a session that
knows it's working a "p1 feature for teamster" operates differently than one
with no context.

### 4a — Inspect the vocabulary

Call `mcp__wms__wms_listTags`. Each entry is `{tag_key, tag_value, category,
cardinality, description, is_seed}`:

- **`category=context`** tags (`product`, `feature`/`bug`, `component`,
  `priority`, `product-version`, plus any integration keys like `github.*`) are
  the durable ones you propose now — they characterize the work and inherit down
  the DAG.
- **`category=lifecycle`** tags (`work-type`, `phase`, `resolution`) track
  execution — **do NOT propose these now**; they are applied per work unit as
  work progresses.
- Read each `description` to avoid proposing a near-duplicate; note
  `cardinality` (`single` replaces on re-tag, `multi` accumulates).

### 4b — Extract, then propose

Best-effort extraction (skip any step that fails):

- **Git/GitHub.** Run `git remote -v` and `git branch --show-current`. Parse the
  remote into integration keys (`github.com/Org/Repo.git` →
  `github.owner:Org`, `github.repo:Repo`; other remotes → `git.remote:<url>`).
  Set `git.branch`. If no remote, set `git.repo` to the directory name.
- **Project metadata.** Read the project's CLAUDE.md for a declared product
  name, version, component names, or tracker references.

Then propose the context tags that fit, with source attribution so the operator
can verify at a glance:

```
Based on your focus "fix auth bug" and the git remote, I'd tag the
strategic Outcome with:

  product:webapp        — from this repo's CLAUDE.md
  github.owner:acme     — from git remote
  github.repo:webapp    — from git remote
  git.branch:main       — current branch
  bug:auth-timeout      — from your focus slug
  priority:p1           — no urgency signal, defaulting high

Sound right?
```

Use judgment — `product` from repo/CLAUDE.md, `feature:<slug>` or `bug:<slug>`
from the focus (mutually exclusive — apply only one), `priority` matched to any
urgency signal or a stated default, `product-version` only if a release target
exists. Do NOT set `component` on the Outcome — it varies per WorkUnit (see
per-entity tagging below).

### 4c — Confirm and apply

The operator confirms, corrects, or redirects. When confirmed, **you apply the
tags yourself** with `mcp__wms__wms_tagEntity` on the strategic Outcome (source
`manual`):

- Reuse an existing `(tag_key, tag_value)` whenever one fits (case-insensitive
  slug match — don't create `product:Teamster` when `product:teamster` exists).
- For a genuinely new *value*, pass a one-line `description` so the next session
  understands it (create-on-apply).
- For a genuinely new *dimension* (a new key the operator wants tracked), seed it
  with `mcp__wms__wms_defineTag(tagKey, category, cardinality, values,
  description)` — pick `single` or `multi` — before applying it.

### Per-entity tagging: Outcome vs. WorkUnit

Context tags on the Outcome are inherited down the DAG automatically — you do
NOT re-apply them per WorkUnit. But some context tags should vary across
WorkUnits rather than being forced onto the Outcome:

- **`component`** is always per-WorkUnit. Each piece of work targets specific
  code; the Outcome spans them all. Apply `component` to each WorkUnit when
  you create it, not to the Outcome. Reuse existing values from `wms_listTags`
  where they fit; propose new values only when genuinely new and reusable
  ("cli", "harness", "wms", "dashboard" are good; one-off labels are not).
- **`feature`/`bug`** — when the session mixes both, apply the specific value
  to each WorkUnit rather than picking one for the Outcome.
- **Tags that are truly shared** (`product`, `priority`, `product-version`,
  integration keys) belong on the Outcome and inherit normally.

**Reuse existing values.** Always call `wms_listTags` before inventing new tag
values. If an existing value fits (case-insensitive), use it. New values must
be genuinely reusable across future sessions — not one-off labels.

## Step 5 — Set focus on the Outcome

Call `mcp__wms__wms_setFocus(entityType="outcome", entityID=<the Outcome id>,
focus=<short what>)`. This attributes your token cost to this Outcome. In solo
mode the one primary agent's focus is the session's focus — there are no peers
to attribute against.

### The two "focus" notions — do not confuse them

There are **two different things called "focus"**, and only one of them drives
cost. Refreshing the wrong one is the single easiest discipline to get wrong in
a solo session (it leaves your whole session's cost in the `unallocated` bucket):

| | What it is | Tool | What it affects |
|---|---|---|---|
| **Activity focus** | A narration string in the live feed | `mcp__activity__reportActivity` / `setOverallIntent` | **Cosmetic only** — the feed display. Drives **no** attribution. |
| **WMS focus** | The cost-bearing focus interval on a WMS entity | `mcp__wms__wms_setFocus` | **Cost attribution** — every token you spend lands on the entity your WMS focus currently points at. |

Updating your `reportActivity` message does **not** move the WMS focus. They are
separate calls against separate state. As your work moves from one entity or step
to the next you must call `mcp__wms__wms_setFocus` **again** — not just narrate
the move with `reportActivity`. If you only refresh the activity narration, the
WMS focus stays frozen on whatever it last pointed at and your spend mis-attributes
(or, if it never left the Outcome, never reaches the WorkUnits at all).

## Step 6 — Proceed with work (the per-step discipline)

You own the WMS bookkeeping — decomposing work into WorkUnits, advancing status,
refreshing focus, tagging phase, closing out. Run that discipline inline, on
**every** step. This is not
advice you can defer to the end — the value of WMS (cost-by-entity, the burn-rate
per outcome, the phase trail) only exists if you keep it current *as you work*.
The most common solo failure is doing the setup ceremony (Step 3–5) well and
then never touching WMS again — one monolithic WorkUnit stuck at `pending`, the
Outcome left `active`, WMS focus frozen on the first entity, and the whole
session's cost in `unallocated`.

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

1. **Create it** — `mcp__wms__wms_createWorkUnit(outcomeID=<outcome>, ...)` if it
   doesn't exist yet (decompose, per above).
2. **Advance status to `active`** — `mcp__wms__wms_updateWorkUnitStatus(... active)`.
   Move it off `pending`; don't leave it parked.
3. **Move the WMS focus to it** — `mcp__wms__wms_setFocus(entityType="workunit",
   entityID=<this WU>, focus=<short what>)`. This is the **cost-bearing** focus
   (Step 5's table) — not just a `reportActivity` narration. Until you do this,
   your spend still attributes to the previous entity (or to the Outcome, or to
   nothing).
4. **Tag the required keys for THIS WorkUnit BEFORE you start it** —
   `mcp__wms__wms_tagEntity` with `work-type` (e.g. `feature`/`docs`/`test`) and
   `phase` (`build`). `work-type` is a **required** tag — set it up front, not at
   close-out; if you haven't already this session, call `mcp__wms__wms_listTags`
   to see which keys are marked `required` and set every one of them on this
   WorkUnit now. Tag the WorkUnit you're on, not a stale one. (Context tags from
   the Outcome are inherited automatically by the engine — you only need to set
   lifecycle tags per WorkUnit.) Applying required tags as you begin each piece
   is what keeps the cost-by-work-type trail honest; deferring them to the end is
   the most common way the trail ends up patchy.

As the piece moves through the loop, **update the phase tag** to match what
you're actually doing — `build` → `test` → `review` — with `wms_tagEntity` on
*this* WorkUnit. Don't skip a phase tag just because you skipped narrating it;
the phase trail is how the dashboard knows test happened.

When the piece is **finished**:

5. **Advance status to `done`** — `mcp__wms__wms_updateWorkUnitStatus(... done)`.
   This closes its focus + state intervals. A WorkUnit left at `active` (or worse,
   `pending`) reads as work that never happened.

### Verification gate (Eight Rules VIII still applies)

When a step benefits from **fresh context** — adversarial review, validation that
shouldn't be self-certified — spawn an **ephemeral** review subagent for that
bounded step, or use `/code-review` for the diff. The subagent does the one task
and exits; you don't keep a team alive. This is how solo mode preserves the
verification gate without a team: a fresh-context reviewer that isn't the author.

Commit only when the operator asks (the acceptance gate still applies).

If no specific work was named, tell the operator the solo session is ready
(mention the focus and the strategic Outcome) and ask what to work on.

## Step 7 — Close-out ritual (end of session)

The session is not done when the code works — it's done when the WMS state
reflects reality. At the
end of the session, run all of:

1. **Every WorkUnit is `done`** — walk the WorkUnits under the Outcome
   (`mcp__wms__wms_listWorkUnits`) and `mcp__wms__wms_updateWorkUnitStatus(... done)`
   any still `active`/`pending` whose work is finished. Nothing should be parked.
2. **Mark the Outcome `done`** — `mcp__wms__wms_updateOutcomeStatus(<outcome>,
   "done")`.
3. **Apply `resolution:achieved`** — `mcp__wms__wms_tagEntity(entityType="outcome",
   entityID=<outcome>, tagKey="resolution", tagValue="achieved")` (or the
   resolution that fits, e.g. `abandoned`). The Outcome is not closed out without
   a `resolution` tag.

An Outcome left `active` with a `pending` WorkUnit under it and no `resolution`
tag is the unmistakable signature of a session that forgot to close out. The
Teamster engine will **warn** you when it detects these misses (an Outcome marked
done with a non-terminal WorkUnit still under it, or closed with no `resolution`)
— but the warning is a safety net, not the
plan: run this ritual yourself so the engine has nothing to flag.

## What solo mode does NOT do

- No `TeamCreate`, no team name.
- No dispatch routing or affinity — there are no peers to route to.
- No "keep teammates alive" discipline — ephemeral subagents are meant to exit.
- It does not replace team mode. If the work needs durable parallel agents on
  different files, use `/teamster:start` and stand up a real team.

## Reference

- [eight-rules.md](../bootstrap/references/eight-rules.md) — see the solo
  carve-out at the top (which rules apply in solo mode).
- [field-guide.md](../bootstrap/references/field-guide.md) — lesson 1 explains
  when a team IS needed vs. when solo suffices.
