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

## Step 1.1 — Generate a team name

Even in solo mode, every session declares a **team name** — a creative,
purpose-aligned identifier that becomes the canonical session identity in
ctop, the health dashboard, and Grafana. "Solo" means one agent, not
anonymous.

Generate a **concise, punchy team name** from the focus slug:

- 1-3 words, lowercase, hyphenated, <=24 chars
- Memorable and on-brand for the objective — a portmanteau of the slug's
  key nouns, or something creative/amusing that evokes the work. Examples:
  - focus "fix the wms dashboard" -> `wms-wrangler` or `dashfix`
  - focus "add prometheus exporter" -> `promscout` or `metricsmith`
  - focus "refactor the hook client" -> `hookwright` or `fishhook`
- Avoid generic names (`teamster`, `team`, `dev`, `build`) — they collide.
- Avoid the project's own name as the bare team name.

**Cross-session reuse is fine.** If an existing `team` tag value fits the
work (check `wms_listTags(tagKey="team")`), reuse it — two sessions sharing
a team name signals they're part of the same effort. Don't force uniqueness
when continuity is the right signal.

Record this name — you'll use it in Step 1.2 and when tagging the Outcome
(Step 4).

## Step 1.2 — Register the team name

Call `registerPeer` to write the team name into the operational tables so
it is visible to ctop, the health API, and roster scoping.

**You MUST pass `session_id`.**  hookd auto-registers a roster entry for
your session on the first hook event (before this skill runs). When you
pass `session_id`, `registerPeer` finds that existing entry and updates its
`team_name` in place — no duplicate, no orphan. Without `session_id`, it
creates a second unbound entry that ctop and health never see.

Extract your session_id from the scratchpad path in your system prompt
(the UUID segment, e.g. `.../6ebee3a6-.../scratchpad` → `6ebee3a6-...`).

**`agent_name` must be empty string `""` for the lead** — it is the agent's
display name, NOT the relationship. Passing `"lead"` creates a ghost roster
entry that duplicates the lead row in ctop.

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

This makes the session's team identity visible in the operational layer —
separate from the `team` WMS tag (Step 4), which is for cost attribution.
Both must be set.

### Verify referenced artifacts before proceeding

If the focus slug or the operator's brief names specific local files, paths,
or kits (e.g. "apply the fix from the pricing kit," "pick up the devkit at
`/path/to/thing`"), verify each one exists — `ls` the path or `Read` the
file — before treating it as real. If something named is missing, say so
and confirm intent with the operator (wrong path? not built yet? proceed
without it?) rather than silently assuming it exists or improvising its
contents. A session that burns its first several turns acting on a kit that
was never actually delivered is a worse outcome than a 10-second existence
check up front.

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
ToolSearch("select:mcp__wms__wms_createOutcome,mcp__wms__wms_createWorkUnit,mcp__wms__wms_updateOutcomeStatus,mcp__wms__wms_updateWorkUnitStatus,mcp__wms__wms_listOutcomes,mcp__wms__wms_getOutcome,mcp__wms__wms_listWorkUnits,mcp__wms__wms_setFocus,mcp__wms__wms_tagEntity,mcp__wms__wms_listTags,mcp__wms__wms_defineTag")
```

Do this once. Do not call ToolSearch for WMS tools again after this.

## Step 3 — Create (or resume) the strategic Outcome

> **Skip search if arrived via `/teamster:start` and the operator already
> picked an outcome.** The start skill already ran the outcome search and the
> operator selected an existing outcome or "New outcome" — use that decision.
> If the operator picked an existing outcome, it is your strategic Outcome;
> skip creation and proceed to Step 4. If they picked "New outcome", create
> as below.

Before creating a new Outcome, search for existing open outcomes that match
the focus slug: extract 2–3 keywords from the slug (e.g., "fix the auth
timeout bug" → `"auth timeout"`) and call
`wms_listOutcomes(status="open", query="<keywords>")`.

If matches are found, present them to the operator with AskUserQuestion:

```
AskUserQuestion (single-select, header "Outcome"):
  question: "Found open outcomes matching your focus. Resume or start new?"
  options (up to 3 most recent matches + "New outcome"):
    - label: "<outcome-id>: <title> (<status>)"
      description: "<description snippet or focus string>"
    - ...
    - label: "New outcome"
      description: "Create a fresh strategic Outcome for this session"
```

If more than 3 outcomes match, show the 3 most recently updated and mention
there are more.

**If the operator picks an existing outcome:**
- Skip creation — use the selected outcome as the strategic Outcome.
- If its status is `done`, reactivate to `active`
  (`mcp__wms__wms_updateOutcomeStatus`).
- Proceed to Step 4 (tags) — skip tags already present on the outcome.
- Then Step 5 (focus).

**If the operator picks "New outcome" or no matches were found:**
Call `mcp__wms__wms_createOutcome` with an id based on the focus slug. A root
Outcome (no parent) IS the strategic one — its altitude comes from DAG position,
so there is no altitude tag to apply. Set status to `active`
(`mcp__wms__wms_updateOutcomeStatus`).

This is the same strategic Outcome the team-mode flow creates.

## Step 4 — The context-tag interview

> **Skip if arrived via `/teamster:start`.** The start skill already ran the
> batched interview (mode + tags in one prompt) and confirmed the tag set with
> the operator. The tags are ready to apply — do so now on the strategic Outcome
> you just created (or resumed) in Step 3, then proceed to Step 5. Do NOT
> re-ask or re-propose tags.

> **When resuming an existing outcome:** The outcome may already have context
> tags applied. Call `wms_getOutcome` to check, and only propose tags that
> aren't already set. Skip the interview entirely if the outcome is fully
> tagged.

This is the genuinely valuable part of session setup, and it applies fully in
solo mode. **Tag classification IS the goal-setting conversation:** a session that
knows it's working a "p1 feature for teamster" operates differently than one
with no context.

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
sources; read CLAUDE.md for project metadata). Apply silently in Step 4c.

**Propose:** From the `propose` group, infer values using the focus slug,
CLAUDE.md, repo name, and existing vocabulary. Present with source attribution:

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

Respect `exclusive` — propose only one key per exclusion group. Apply
`scope: "outcome"` keys to the Outcome; note `requiredLifecycle` and
`required` keys for WorkUnit dispatch time.

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

**Apply the team name tag.** After applying the operator-confirmed tags, also
apply `team:<team-name>` on the Outcome (the name you generated in Step 1.1).
This is the WMS-layer counterpart to the `registerPeer` call in Step 1.2 — both
must be set for the team identity to appear consistently in ctop AND Grafana.

### Per-entity tagging: Outcome vs. WorkUnit

Context tags on the Outcome are inherited down the DAG automatically — you do
NOT re-apply them per WorkUnit. The manifest's `scope` on each key tells you
where it belongs: `scope: "outcome"` keys go on the Outcome and inherit down;
`required` keys (e.g., `component`) must be set per-WorkUnit before close-out.
When a session mixes values from both sides of an `exclusive` group, apply the
specific value to each WorkUnit rather than picking one for the Outcome.

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
4. **Tag the `requiredLifecycle` keys for THIS WorkUnit BEFORE you start it** —
   `mcp__wms__wms_tagEntity` with `work-type` (e.g. `feature`/`docs`/`test`) and
   `phase` (`build`). Check the `requiredLifecycle` map in the manifest for valid
   values — no extra lookup needed. Tag the WorkUnit you're on, not a stale one.
   (Context tags from the Outcome are inherited automatically by the engine — you
   only need to set lifecycle tags per WorkUnit.) Applying required tags as you
   begin each piece is what keeps the cost-by-work-type trail honest; deferring
   them to the end is the most common way the trail ends up patchy.

   **Work-scope slug on the Outcome.** If the strategic Outcome does not yet
   carry a work-scope slug tag and the work type is clear, apply one now with
   `wms_tagEntity` on the Outcome. The slug key matches the `work-type` you're
   setting on the WorkUnit (e.g., `work-type:bug` → `bug:<slug>` on the
   Outcome). Skip if the Outcome already has a work-scope slug from the
   interview.

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

- No dispatch routing or affinity — there are no peers to route to.
- No "keep teammates alive" discipline — ephemeral subagents are meant to exit.
- It does not replace team mode. If the work needs durable parallel agents on
  different files, use `/teamster:start` and stand up a real team.

Solo mode still declares a team name (Steps 1.1–1.2) — "solo" means one
agent, not anonymous. The team name is the canonical session identifier in
ctop, health dashboards, and Grafana.

## Reference

- [eight-rules.md](../bootstrap/references/eight-rules.md) — see the solo
  carve-out at the top (which rules apply in solo mode).
- [field-guide.md](../bootstrap/references/field-guide.md) — lesson 1 explains
  when a team IS needed vs. when solo suffices.
