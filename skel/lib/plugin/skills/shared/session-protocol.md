# Teamster Session Protocol

This is the **canonical source** for the intake mechanics shared by
`/teamster:start`, `/teamster:bootstrap`, and `/teamster:solo`. Each of
those skills reads the steps below and follows them inline as part of its
own flow — they do not keep their own copies. If you're changing intake
behavior (the focus interview, team identity, Outcome creation, the
context-tag interview, focus attribution, or close-out), change it **here**.
The three mode files carry only what's genuinely mode-specific: `start`'s
batched-interview UI and dispatch logic, `bootstrap`'s team dispatch
protocol, and `solo`'s carve-out and per-step WMS discipline.

Steps are numbered for cross-reference (e.g. "session-protocol Step 6") —
follow them in order unless the calling skill says otherwise.

## Step 1 — Focus slug

A focus slug is a short phrase describing what the session is working on.
It seeds the strategic Outcome's id, the team name (Step 4), and the
context-tag interview (Step 7).

- If `$ARGUMENTS` is non-empty, use it as the focus slug.
- Otherwise ask with AskUserQuestion:
  - header: "Session focus" (`bootstrap` may use "Team focus" instead —
    same mechanic, and may add "— used to generate a team name" to the
    question text, since the slug feeds team-name generation there).
  - question: "What is this session focused on? (a short phrase)"
  - options: one per plausible focus inferred from the conversation, recent
    files, and the working directory (2–3 max). The operator can always
    pick "Other" and type their own.

If you arrived via `/teamster:start`, it already ran this step (and Step 2)
— use what it handed down, do NOT re-ask.

## Step 2 — Verify referenced artifacts

If the focus slug or the operator's message names specific local files,
paths, or kits (e.g. "apply the fix from the pricing kit," "pick up the
devkit at `/path/to/thing`"), verify each one exists (`ls` or `Read`)
before treating it as real. If something named is missing, say so and
confirm intent with the operator (wrong path? not built yet? proceed
without it?) rather than silently assuming it exists or improvising its
contents. A session — or a teammate about to be dispatched — that burns
its first several turns acting on a kit that was never actually delivered
is a worse outcome than a 10-second existence check up front.

## Step 3 — Declare the session mode

Load and call the mode signal (deferred MCP tool — load once):

```
ToolSearch("select:mcp__activity__setMode")
mcp__activity__setMode(mode="team")   # or "solo"
```

`setMode` is a no-op confirmation tool; the Teamster hook does the real
work — it records the session's mode so the runtime gates behave correctly
(solo relaxes the team-dispatch mandate and the bare-`Agent` block; team
keeps them enforced). Call it **once**, before creating or dispatching
anything — not every turn; the hook keeps the marker fresh on its own
while the session is active. If you arrived via `/teamster:start`, it
already called `setMode` for the confirmed mode — calling again is
harmless (idempotent).

## Step 4 — Team identity

Every session — team or solo — declares a **team name**: a creative,
purpose-aligned identifier that becomes the canonical session identity in
ctop, the health dashboard, and Grafana. "Solo" means one agent, not
anonymous.

### 4a — Generate the name

Generate a **concise, punchy team name** from the focus slug:

- 1-3 words, lowercase, hyphenated, <=24 chars
- Memorable and on-brand for the objective — a portmanteau of the slug's
  key nouns, or something creative/amusing that evokes the work. Examples:
  - focus "fix the wms dashboard" -> `wms-wrangler` or `dashfix`
  - focus "implement remote install" -> `homing-pigeon` or `remoter`
  - focus "refactor the hook client" -> `hookwright` or `fishhook`
  - focus "add prometheus exporter" -> `promscout` or `metricsmith`
- Avoid generic names (`teamster`, `team`, `dev`, `build`) — they collide.
- Avoid the project's own name as the bare team name.

**Cross-session reuse is fine.** If an existing `team` tag value fits the
work (check `wms_listTags(tagKey="team")`), reuse it — two sessions
sharing a team name signals they're part of the same effort. Don't force
uniqueness when continuity is the right signal.

Record this name — you'll use it in 4b and when tagging the strategic
Outcome (Step 7d) with `team:<name>`.

### 4b — Register it

Call `registerPeer` to write the team name into the operational tables so
it is visible to health dashboards and scopes roster/health queries to
team members.

**You MUST pass `session_id`.** hookd auto-registers a roster entry for
your session on the first hook event (before this skill runs). When you
pass `session_id`, `registerPeer` finds that existing entry and updates
its `team_name` in place — no duplicate, no orphan. Without `session_id`,
it creates a second unbound entry that ctop and health never see.

Extract your session_id from the scratchpad path in your system prompt
(the UUID segment, e.g. `.../6ebee3a6-.../scratchpad` → `6ebee3a6-...`).

**`agent_name` must be empty string `""` for the lead** — it is the
agent's display name, NOT the relationship. Passing `"lead"` creates a
ghost roster entry that duplicates the lead row in ctop.

```
ToolSearch("select:mcp__roster__registerPeer")
registerPeer(
  agent_name: "",
  runtime: "claude_code",
  relationship: "lead",
  team_name: "<team-name>",
  session_id: "<your session UUID>"
)
```

This is NOT the same as the `team` tag on the Outcome (Step 7d) — that is
WMS-domain work attribution. This is operational identity: it makes the
team name visible in ctop, the health API, and roster scoping. Both must
be set. Call `registerPeer` exactly once per session — do not repeat it if
you arrived here via `/teamster:start` having already called it.

## Step 5 — Load WMS tools

Load the core WMS tools in one batch (deferred — load before first use):

```
ToolSearch("select:mcp__wms__wms_createOutcome,mcp__wms__wms_createWorkUnit,mcp__wms__wms_updateOutcomeStatus,mcp__wms__wms_updateWorkUnitStatus,mcp__wms__wms_listOutcomes,mcp__wms__wms_getOutcome,mcp__wms__wms_listWorkUnits,mcp__wms__wms_setFocus,mcp__wms__wms_setPhase,mcp__wms__wms_tagEntity,mcp__wms__wms_listTags,mcp__wms__wms_defineTag,mcp__wms__wms_listRelationKinds,mcp__wms__wms_addRelation")
```

Team mode additionally needs `wms_assignWorkUnit` (dispatch) and
`wms_retireTag` (tag stewardship) — add them to the same batch. Do this
once. Do not call ToolSearch for WMS tools again after this.

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
was dropped rather than finished (Step 9 below) — it counts as neither
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
belongs there; otherwise it belongs under a different Outcome — see Step
6's rework branch.

## Step 6 — Create, resume, or rework the strategic Outcome

> **Skip search if arrived via `/teamster:start` and the operator already
> picked an outcome.** The start skill already ran the outcome search and
> the operator selected an existing outcome or "New outcome" — use that
> decision, routed through the same three-way triage below: if they picked
> "New outcome", that's branch A; if they picked an existing outcome, check
> its status — `done` is branch C (rework), anything else is branch B
> (continuation).

Before creating a new Outcome, search **all** outcomes matching the focus
slug, not just open ones — a match on a `done` outcome is exactly the
rework-detection signal this step needs. Extract 2–3 keywords (e.g., "fix
the auth timeout bug" → `"auth timeout"`) and call
`wms_listOutcomes(query="<keywords>")` (omit `status` so both open and
`done` outcomes surface).

If matches are found, present them to the operator with AskUserQuestion:

```
AskUserQuestion (single-select, header "Outcome"):
  question: "Found outcomes matching your focus. Continue existing work,
             rework something closed, or start new?"
  options (up to 3 most recent matches + "New outcome"):
    - label: "<outcome-id>: <title> (<status>)"
      description: "<description snippet or focus string>"
    - ...
    - label: "New outcome"
      description: "Create a fresh strategic Outcome for this session"
```

If more than 3 outcomes match, show the 3 most recently updated and
mention there are more. The status in each label (`active`, `done`, etc.)
is what tells you — and the operator — which branch below applies.

### Three-way triage

**A. New work** — the operator picks "New outcome", or no matches were
found. Mint an id for the new Outcome from the focus slug, but **do not
create it yet** — run the Step 7 tag interview first, then create it there
with the confirmed tags inline (Step 7d). A root Outcome (no parent) IS the
strategic one — its altitude comes from DAG position, so there is no
altitude tag to apply. No relation edge — there is no antecedent. Proceed
to Step 7.

**B. Continuation** — the operator picks an existing outcome whose status
is **not** `done` or `abandoned` (`pending`, `active`, `review`, `blocked`,
or `on_hold`). Skip creation — use the selected outcome as-is as the
strategic Outcome; do not change its status. Proceed to Step 7 (tags) —
check existing tags first (`wms_getOutcome`) and skip any already present.
Then Step 8 (focus). This branch never creates an Outcome, so the
inline-tags collapse doesn't apply to it — tags are still applied serially
via `wms_tagEntity` in 7d, exactly as before.

**C. Rework** — the operator picks an existing outcome whose status **is**
`done` (or `abandoned`). **Never reactivate a closed Outcome.** Flipping
`done`/`abandoned` back to `active` erases the fact that the prior work
actually shipped or was dropped — this is the exact failure mode the
triage exists to prevent. `done` has a legal reopen edge (`done ->
review`) for the rare case where the prior Outcome itself needs correcting
— that is a deliberate, separate act from starting new or follow-on work,
and not what this branch is for. Instead:

1. Mint an id for a **new** Outcome (based on the focus slug, as in branch
   A) — this is the strategic Outcome for this session, not the closed
   one. **Do not create it yet** — creation happens in Step 7d, same as
   branch A.
2. Decide the relation kind now, so it's ready the moment the new Outcome
   exists — which `kind` matches why this work exists (call
   `mcp__wms__wms_listRelationKinds` if not already called this session):
   - `remediates` — the delivered outcome had a defect, didn't do what it
     was supposed to (taxable, code miss)
   - `addresses-limitation` — it did what was asked, but the result can't
     be used as needed now (taxable, design miss)
   - `fulfills-realization` — a requirement nobody identified at the time
     (taxable, spec miss)
   - `supersedes` — requirements, scale, or environment changed; the prior
     work was right for its time (not taxed)
   If the distinction isn't obvious from the operator's framing, ask in one
   line ("is this fixing something that shipped broken, or building on
   something that's since changed?") rather than defaulting to a taxable
   kind. This step is **advisory** — decide your best-fit kind and move on
   even if the operator waves it off; a wrong kind is cheap to fix later
   (`wms_removeRelation` + re-add), a missing edge is invisible to every
   future rework-tax rollup.
3. Proceed to Step 7 (tags) as a **new** Outcome — run the tag interview
   fresh. Context tags do not carry over from the closed prior outcome.
4. Once Step 7d creates the new Outcome, immediately record the relation
   edge decided in step 2: call `mcp__wms__wms_addRelation` with
   `fromType="outcome"`, `fromID=<the new outcome>`, `toType="outcome"`,
   `toID=<the done/abandoned outcome>`, `kind=<decided above>` — before
   proceeding to Step 8. It needs the new Outcome's real id, which exists
   only after 7d's create call.

## Step 7 — The context-tag interview

> **Skip if arrived via `/teamster:start`.** The start skill already ran
> the batched interview (mode + tags in one prompt) and confirmed the tag
> set with the operator. Do NOT re-ask or re-propose tags. What's left
> depends on the Step 6 branch:
> - **Branch A/C (new/rework Outcome):** already done — `start` created the
>   Outcome with the confirmed tags inline as part of its own dispatch
>   step (see `start/SKILL.md` Step 4). Nothing left to apply here; proceed
>   straight to Step 8.
> - **Branch B (continuation):** apply the confirmed tags now (Step 7d,
>   serial `wms_tagEntity`) on the resumed Outcome, then proceed to Step 8.

> **When resuming an existing outcome:** The outcome may already have
> context tags applied. Call `wms_getOutcome` to check, and only propose
> tags that aren't already set. Skip the interview entirely if the outcome
> is fully tagged.

This is the genuinely valuable part of session setup. **Tag
classification IS the goal-setting conversation:** a session that knows
it's working a "p1 feature for teamster" operates differently than one
with no context. The tags you confirm here are applied directly to the
strategic Outcome and inherit down the DAG to every Outcome and WorkUnit
below it.

This is a **prompt pattern**, not an engineered system. Use judgment to
match context to vocabulary and express uncertainty in plain language —
there is no scoring or confidence metric to compute.

### 7a — Inspect the key manifest

Call `mcp__wms__wms_listTags` (no args) to get the role-shaped manifest.
The response groups keys by role — no interpretation needed:

- **`propose`** — keys to offer the operator. `values` lists options
  (when present); `n` means drill down with `wms_listTags(tagKey=...)`.
  Respect `exclusive` (at most one key per exclusion group). Apply
  `scope: "outcome"` keys to the Outcome; keys without scope can go on
  either.
- **`autoExtract`** — extract silently from the environment (git, env).
- **`requiredLifecycle`** — lifecycle keys that MUST be applied to every
  WorkUnit at/before dispatch. Values are included (e.g. `phase`:
  design/build/test/review/iterate; `work-type`: feature/bug/refactor/…).
  Do NOT propose these at the Outcome interview — apply them per-WorkUnit.
- **`required`** — non-lifecycle keys required on every WorkUnit before
  close-out.
- **`engineManaged`** — engine-only keys: do not propose, set, or modify.
  The engine is confirmed the only writer.
- **`ritualManaged`** — also skips the interview like `engineManaged`, but a
  documented ritual sets these by hand — e.g. `resolution`, which Step 9b's
  close-out ritual sets via `wms_tagEntity`. Set them when the ritual calls
  for it; do not propose them in the interview.

### 7b — Extract and propose

**Auto-apply:** For each key in `autoExtract`, extract its value from the
named source (`git remote -v`, `git branch --show-current` for `git`
sources; read CLAUDE.md for project metadata). Apply silently in 7d.

**Propose:** From the `propose` group, infer values using the focus slug,
CLAUDE.md, repo name, and existing vocabulary. For keys with `values`,
reuse existing options (case-insensitive slug match); for keys with only
`n`, call `wms_listTags(tagKey=...)` to drill down before proposing.
Respect `exclusive` — propose only one key per exclusion group.

**Work-scope slug convention.** The `propose` group includes slug keys
that parallel `work-type` values: `feature:<slug>`, `bug:<slug>`,
`refactor:<slug>`, `polish:<slug>`, `infra:<slug>`, `docs:<slug>`,
`research:<slug>`, `test:<slug>`, `admin:<slug>`. These identify WHICH
specific feature/bug/refactor/etc. the Outcome is about. When the work
type can be inferred from the focus slug, propose the matching slug key
with a value derived from the slug. All share the `work-scope` exclusion
group — propose at most one. If no existing value fits, mint a new one
(pass a `description` to `wms_tagEntity`). Not every Outcome needs a
work-scope slug — omit it when the work is too generic or cross-cutting
for a single identity.

Present the full set conversationally with source attribution:

```
Based on your focus "fix auth bug" and the git remote, I'd tag the
strategic Outcome with:

  product:webapp        — from this repo's CLAUDE.md
  bug:auth-timeout      — from your focus slug
  priority:p1           — no urgency signal, defaulting high

I'll also auto-apply: github.owner:acme, github.repo:webapp,
git.branch:main (extracted from git).

Sound right?
```

### 7c — Feedback loop

The operator confirms, corrects, or redirects. Treat each answer as
refining BOTH the tags AND the goal context — they're the same
conversation.

**New values vs. new keys.** A new *value* on an existing key is
create-on-apply: pass a one-line `description` to `wms_tagEntity` so
future sessions understand it. A new *key* (a whole new dimension) is a
vocabulary change: seed it with `wms_defineTag(tagKey, category,
cardinality, values, description)` — pick `single` or `multi` — before
applying.

### 7d — Apply the confirmed tags

**Branches A and C (no Outcome exists yet):** create it now, with the
confirmed tag set — including `team:<team-name>` (the name generated in
Step 4a; this is the WMS-layer counterpart to the `registerPeer` call in
Step 4b, both must be set for the team identity to appear consistently in
ctop AND Grafana) — passed inline:
```
mcp__wms__wms_createOutcome(id=<minted in Step 6>, title=<...>, description=<...>,
    status="active",
    tags={<confirmed tag set from 7c>, "team": "<team-name>"})
```
Reuse existing `(tag_key, tag_value)` pairs whenever one fits
(case-insensitive slug match — don't create `product:Teamster` when
`product:teamster` exists). For a genuinely new value, use the `{value,
description}` form. **Branch C:** immediately after this call, record the
relation edge decided in the triage's branch-C step 2 — it needs this
Outcome's real id, which now exists.

**Branch B (Outcome already exists):** apply tags the old way, serially —
`mcp__wms__wms_tagEntity` on the strategic Outcome (source `manual`) for
each confirmed tag not already present, plus `team:<team-name>` per the
same reasoning above.

Then proceed to Step 8, carrying the context you just established.

### Per-entity tagging: Outcome vs. WorkUnit

Context tags on the Outcome are inherited down the DAG automatically —
you do NOT re-apply them per WorkUnit. The manifest's `scope` on each key
tells you where it belongs: `scope: "outcome"` keys go on the Outcome and
inherit down; `requiredLifecycle` keys (phase, work-type) must be set
per-WorkUnit at/before dispatch; `required` keys must be set per-WorkUnit
before close-out. When a session mixes values from both sides of an
`exclusive` group, apply the specific value to each WorkUnit rather than
picking one for the Outcome.

**Reuse existing values.** Always call `wms_listTags` before inventing new
tag values. If an existing value fits (case-insensitive), use it. New
values must be genuinely reusable across future sessions — not one-off
labels.

## Step 8 — Set focus on the Outcome

Call `mcp__wms__wms_setFocus(entityType="outcome", entityID=<the Outcome
id>, focus=<short what>)`. This attributes your token cost to this
Outcome. Hold this focus throughout the session; refresh it (call
`wms_setFocus` again) every time your work returns to Outcome-level
coordination after a dispatch, a WorkUnit, or any other entity-level step.

### The two "focus" notions — do not confuse them

There are **two different things called "focus"**, and only one of them
drives cost. Refreshing the wrong one is the single easiest discipline to
get wrong — it leaves spend in the `unallocated` bucket:

| | What it is | Tool | What it affects |
|---|---|---|---|
| **Activity focus** | A narration string in the live feed | `mcp__activity__reportActivity` / `setOverallIntent` | **Cosmetic only** — the feed display. Drives **no** attribution. |
| **WMS focus** | The cost-bearing focus interval on a WMS entity | `mcp__wms__wms_setFocus` | **Cost attribution** — every token you spend lands on the entity your WMS focus currently points at. |

Updating `reportActivity` does **not** move the WMS focus. They are
separate calls against separate state. As your work moves from one entity
to the next you must call `wms_setFocus` **again** — not just narrate the
move with `reportActivity`.

## Step 9 — Close-out: consider, don't assume

The old ritual auto-applied `done` + `resolution:achieved` unconditionally
once every WorkUnit was finished. That's gone (README §5 R1 of the
wms-hygiene-kit) along with the engine's two auto-close cascades — "every
WorkUnit terminal" is no longer license to close the Outcome for the same
reason the cascades came out: **we don't waterfall projects, and it's
unlikely every WorkUnit is known from the start.** Closing an Outcome is
now something you reason about and the operator decides.

### 9a — When this runs

Three triggers, not one — waiting only for session end lets an Outcome
that finished early in a long session sit unconsidered the whole time,
which is exactly the stale/`review`-resting-state problem this kit exists
to fix, and relying only on a WorkUnit-close event leaves every
Outcome-above-Outcome case unreachable:

1. **Eager, on WorkUnit close.** The moment closing a WorkUnit makes its
   Outcome's own WorkUnit set fully terminal, run 9b on **that Outcome**
   right there — don't wait for session end. `bootstrap.md`'s dispatch
   protocol and `solo.md`'s per-work-step checklist each have their own
   call site for this (see their own file edits); this step doesn't
   duplicate their text, it defines the procedure they call. The engine
   itself now appends a one-line close-out readiness hint (permanent, not
   the focus nudge; exact text in `semantic-conventions.md` §7.4) to that
   WorkUnit's status-change response when this happens — paraphrased: "all N
   work units under the outcome are now terminal, it does not close
   automatically, run the close-out consideration" — the prose and the
   machine agree; treat the hint as confirmation you're at the right moment,
   not a substitute for
   running 9b.
2. **Eager, on Outcome close (parent propagation).** The moment 9b closes
   an Outcome via step 1 or step 3 below, check its parent(s)
   (`wms_getOutcome`'s `parent_ids`) and run 9b on each parent whose own
   child-Outcome set just became fully terminal as a result. This is
   defined inside 9b's "Apply the answer" section, not a separate call
   site in the mode files — it fires from wherever a close just happened,
   which is always inside 9b itself.
3. **End of session.** Walk every Outcome this session created or resumed
   that steps 1–2 haven't already resolved — *resolved* includes an
   Outcome the human answered "Not yet" on and whose own state has not
   changed since — and run 9b on each.

All three triggers call the same procedure (9b) on **one Outcome at a
time**. None of them ever walks upward to a parent just because a child
Outcome (or a relation-partner) *looks* ready — trigger 2 only fires
*after* 9b has actually closed the child, with the human's confirmation
already given at that level; see "The partial case" below for why that
distinction (propagate on confirmed closure, never on mere completeness)
is what keeps this different from the removed cascades.

### 9b — Consider one Outcome

**Reason first, read-only:**
- `mcp__wms__wms_listWorkUnits(outcomeID=<this>)` — is every WorkUnit
  terminal (`done` or `abandoned`)?
- `mcp__wms__wms_listOutcomes(parentOutcomeID=<this>)` — if this Outcome
  has child Outcomes, are they all terminal too?
- **If either check finds something open: stop. Say nothing, write
  nothing.** An Outcome with real open work is the expected state, not
  something to surface. 9a's triggers bring it back once its own state
  actually changes.
- **If both come back clean** (or neither WorkUnits nor child Outcomes
  exist at all): form a one-line recommendation. Default to "achieved"
  unless something in the session's own conversation signals the work was
  dropped or superseded rather than finished — name that signal in the
  question below rather than silently defaulting.

**Then ask — multiple choice, `start/SKILL.md`-style:**
```
AskUserQuestion (single-select, header "Close out?"):
  question: "<outcome-id>: <title> looks done — <N> WorkUnit(s)[, <M> child
             Outcome(s),] all terminal. <one-line reasoning>. Close it?"
  options:
    - label: "Close — achieved"      [mark "(Recommended)" unless reasoning
                                       favors abandoned]
      description: "Mark done, resolution:achieved"
    - label: "Close — abandoned"
      description: "Work was dropped, not finished"
    - label: "Not yet"
      description: "Leave it open — more is coming, or I'm not sure"
    - label: "Let's discuss this"
      description: "Talk it through before deciding"
```

**Apply the answer:**
- **Achieved** — `mcp__wms__wms_updateOutcomeStatus(<outcome>, "done")` +
  `mcp__wms__wms_tagEntity(entityType="outcome", entityID=<outcome>,
  tagKey="resolution", tagValue="achieved")`. Same two calls the old
  ritual already made — this WP adds no WMS-call volume for the confirmed
  case.
- **Abandoned** — `mcp__wms__wms_updateOutcomeStatus(<outcome>,
  "abandoned")`. No separate `resolution` tag — the status itself carries
  the meaning, and a dropped Outcome must stop counting as `done` in
  completion metrics.
- **Not yet** — no WMS write at all. Don't re-ask later in the same
  session unless the Outcome's own state actually changed (another
  WorkUnit or child Outcome went terminal) — re-asking against unchanged
  state is the nagging failure mode this design exists to avoid.
- **Discuss** — engage conversationally, then re-run 9b's ask once
  resolved.

**After Achieved or Abandoned — propagate to the parent(s), 9a trigger 2.**
Call `mcp__wms__wms_getOutcome(id=<the Outcome just closed>)` and read
`parent_ids`. For each parent: run 9b on it fresh — the same read-first
check (all its own WorkUnits terminal, all its own child Outcomes
terminal), the same "say nothing if not ready" rule, the same ask if it
is. Do this **immediately**, not deferred to end-of-session — a parent
whose last child Outcome you just closed is exactly the case 9a trigger 2
exists for. If the parent also closes, repeat this same step for *its*
parent(s), and so on — a plain loop up the DAG. Stop when a level says
"not yet" or "discuss," or when an Outcome has no parents left.

**What 9b never does**: inspect `outcome_relations` edges
(`remediates`/`supersedes`/`addresses-limitation`/`fulfills-realization`/
`reverts`/`follows-up-on`/`discovered-during`) as evidence of readiness.
Those record lineage — why this Outcome exists — not whether it's done.

### 9c — Batch multiple ready Outcomes

If more than one Outcome is ready for 9b at once (typically 9a's
end-of-session sweep, walking several at a time) — present **one**
`AskUserQuestion` with one question per ready Outcome, the same
single-round-trip principle `start/SKILL.md` Step 3 already uses for its
own batched interview. Don't serialize N round-trips for N Outcomes ready
at the same moment.

### 9d — Required tags, unchanged from before

Before closing a WorkUnit `done` (the precondition for 9a's eager
trigger), confirm it still carries its `requiredLifecycle` keys and any
`required` context key — unchanged from the old ritual, just no longer
followed by an unconditional Outcome close.

### The partial case — built in, not bolted on

**9b operates on exactly one Outcome, using only that Outcome's own direct
children.** It never walks upward. A parent is only ever considered
because *its own* children independently went terminal — never because a
child's or a relation-partner's closure "suggests" the parent might be
ready too. An Outcome with real open work never reaches the human at all.
