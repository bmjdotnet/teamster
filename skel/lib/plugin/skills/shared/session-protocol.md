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
Outcome:   pending -> active -> review -> done
WorkUnit:  pending -> active -> review -> done
Both:      (any) -> blocked,  blocked -> (any prior)
```

`done` is the only terminal state. "complete", "achieved", "assigned",
"planning", "open", "closed", "in_progress" are NOT valid status strings.
NEVER run SQL directly against the WMS database. All state changes go
through MCP tools.

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
found. Call `mcp__wms__wms_createOutcome` with an id based on the focus
slug. A root Outcome (no parent) IS the strategic one — its altitude comes
from DAG position, so there is no altitude tag to apply. Set status to
`active` (`mcp__wms__wms_updateOutcomeStatus`). No relation edge — there is
no antecedent. Proceed to Step 7.

**B. Continuation** — the operator picks an existing outcome whose status
is **not** `done` (`pending`, `active`, `review`, or `blocked`). Skip
creation — use the selected outcome as-is as the strategic Outcome; do not
change its status. Proceed to Step 7 (tags) — check existing tags first
(`wms_getOutcome`) and skip any already present. Then Step 8 (focus).

**C. Rework** — the operator picks an existing outcome whose status **is**
`done`. **Never reactivate a done Outcome.** Flipping `done` → `active`
erases the fact that the prior work actually shipped — this is the exact
failure mode the triage exists to prevent. Instead:

1. Create a **new** Outcome (id based on the focus slug, as in branch A) —
   this is the strategic Outcome for this session, not the done one. Set
   status to `active`.
2. Record why with a typed relation: call `mcp__wms__wms_listRelationKinds`
   (if not already called this session) and `mcp__wms__wms_addRelation`
   with `fromType="outcome"`, `fromID=<the new outcome>`,
   `toType="outcome"`, `toID=<the done outcome>`, and the `kind` that
   matches why this work exists:
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
   kind.
3. This edge is **advisory** — offer it and use your best judgment on the
   kind, but do not block Outcome creation on the operator confirming the
   exact kind. Still record the edge with your best-fit kind even if they
   wave it off — a wrong kind is cheap to fix later (`wms_removeRelation`
   + re-add); a missing edge is invisible to every future rework-tax
   rollup.
4. Proceed to Step 7 (tags) as a **new** Outcome — run the tag interview
   fresh. Context tags do not carry over from the closed prior outcome.

## Step 7 — The context-tag interview

> **Skip if arrived via `/teamster:start`.** The start skill already ran
> the batched interview (mode + tags in one prompt) and confirmed the tag
> set with the operator. The tags are ready to apply — do so now (Step 7d)
> on the strategic Outcome you just created (or resumed) in Step 6, then
> proceed to Step 8. Do NOT re-ask or re-propose tags.

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

Once the operator has confirmed the set, apply them yourself with
`mcp__wms__wms_tagEntity` on the strategic Outcome (source `manual`).
Reuse existing `(tag_key, tag_value)` pairs whenever one fits
(case-insensitive slug match — don't create `product:Teamster` when
`product:teamster` exists). For a genuinely new value, pass a
`description`.

**Apply the team name tag too.** Also apply `team:<team-name>` on the
Outcome (the name generated in Step 4a). This is the WMS-layer
counterpart to the `registerPeer` call in Step 4b — both must be set for
the team identity to appear consistently in ctop AND Grafana.

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

## Step 9 — Close-out ritual (end of session)

The session is not done when the code works — it's done when the WMS
state reflects reality. At the end of the session, run all of:

1. **Every WorkUnit is `done`** — walk the WorkUnits under the Outcome
   (`mcp__wms__wms_listWorkUnits`) and close
   (`mcp__wms__wms_updateWorkUnitStatus` ... `done`) any still
   `active`/`pending` whose work is finished. Nothing should be parked.
2. **Mark the Outcome `done`** — `mcp__wms__wms_updateOutcomeStatus`.
3. **Apply `resolution:achieved`** —
   `mcp__wms__wms_tagEntity(entityType="outcome", entityID=<outcome>,
   tagKey="resolution", tagValue="achieved")` (or `abandoned` if the work
   was dropped). The Outcome is not closed out without a `resolution` tag.

An Outcome left `active` with a `pending` WorkUnit and no `resolution` tag
is the unmistakable signature of a session that forgot to close out. The
Teamster engine **warns** when it detects these misses — but the warning
is a safety net, not the plan: run this ritual yourself so the engine has
nothing to flag.
