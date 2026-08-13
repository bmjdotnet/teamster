---
name: teamster-tags
description: Tag steward — refine your tag vocabulary, merge or add reporting dimensions, and roll back changes. Explicit invocation only — mention $teamster-tags.
---

# Tag Steward

The day-two-and-beyond companion to `teamster setup tags`. The TUI wizard
seeds the tag vocabulary on day zero, when descriptions are generic and the
operator may not fully understand the taxonomy yet. This skill is the
conversational tool the operator returns to once they have real data and real
opinions — to refine descriptions, merge or split values, add reporting
dimensions, and roll back mistakes.

You are the steward of the tag vocabulary. Your job is **not** to apply tags
as fast as possible. It is to make the vocabulary good enough that
classification becomes obvious — then apply it, with the operator in the
loop, reversibly.

## The description IS the rule

Every tag value carries a `description`. In day-zero seeding these are
informational ("the dashboard and its panels" for `component:dashboard`).
Here they become the **classification rubric** — clear enough that a cheap
LLM or a human can reliably classify an entity by reading the description
alone.

This reframes the whole session. When you find an ambiguous description
("the dashboard"), that is a quality bug you fix
("The web dashboard, its SSE feed, and the panel templates — NOT the CLI feed
viewer, which is `component:feed`") **before** you classify anything against
it. The durable artifact of a steward session is improved descriptions. The
tags you apply to entities are a side effect.

There is no separate rules table, no config file, no DSL. The rules live in
the `description` field, and you read them back on the next invocation. The
vocabulary converges on clarity over time, one session at a time.

## Day-zero vs. day-two

| | Day zero | Day two (this skill) |
|---|---|---|
| Tool | `teamster setup tags` (TUI wizard) | `$teamster-tags` (conversational) |
| Input | No data — generic defaults | Real tagged data + operator opinions |
| Goal | Seed a reasonable keyspace | Refine descriptions, merge/add values, add dimensions |
| Cadence | Once, at install | Whenever the vocabulary needs to evolve |

Don't reinvent the wizard. If the operator wants to re-seed defaults or do a
guided first-time setup, point them at `teamster setup tags`.

## Step 0 — Set up accounting and read the intent

The `wms` and `activity` MCP tools are registered at install time. On some
Codex builds they are callable directly; on builds that defer-load MCP tools,
surface one before first use with Codex's own tool search — search by **what
you want to DO** ("list tags," "tag an entity"), not a bare `wms_` name
(identifier queries reliably return zero). See AGENTS.md → "Finding
WMS/activity MCP tools."

### Cost attribution — do this before any other work

Your token spend must be attributed to a WMS entity or it lands in the
unallocated bucket. Set up accounting immediately:

1. **Declare your intent** (cosmetic, for the activity feed):
   ```
   mcp__activity__setOverallIntent(message="steward tag vocabulary")
   mcp__activity__reportActivity(type="planning", message="steward tags")
   ```

2. **Find or create a work unit.** Check if a parent outcome exists for the
   current session's work (`mcp__wms__wms_listOutcomes`). If one does, create
   a work unit under it. If not, create a standalone outcome first:
   ```
   mcp__wms__wms_createOutcome(id="tag-steward-<date>", title="Tag vocabulary stewardship", status="active")
   mcp__wms__wms_createWorkUnit(id="wu-tags-<date>", title="Steward tag vocabulary", outcomeID="<outcome>", status="active")
   ```

3. **Tag the work unit** with its context:
   ```
   mcp__wms__wms_tagEntity(entityType="workunit", entityID="wu-tags-<date>", tagKey="work-type", tagValue="admin")
   mcp__wms__wms_tagEntity(entityType="workunit", entityID="wu-tags-<date>", tagKey="phase", tagValue="build")
   mcp__wms__wms_tagEntity(entityType="workunit", entityID="wu-tags-<date>", tagKey="component", tagValue="tagging")
   ```
   Context tags (`product`, `feature`, etc.) inherit from the parent outcome
   — do not re-apply them on the work unit.

4. **Set cost-bearing focus** — this is what actually attributes your
   tokens:
   ```
   mcp__wms__wms_setFocus(entityType="workunit", entityID="wu-tags-<date>", focus="steward tags")
   ```
   Call `wms_setFocus` again if you switch entities. Call `reportActivity` as
   you work (cosmetic only — it does NOT move focus).

5. **Close out when done:**
   ```
   mcp__wms__wms_updateWorkUnitStatus(id="wu-tags-<date>", status="done")
   mcp__activity__completeActivity(message="tag stewardship complete")
   ```

Parse the operator's intent from the rest of their message. If it's empty,
ask what they want to do — offer the common entry points:

- "I want to add a product-version dimension" → enter at Stage 1 (new key)
- "my component values are too granular, can we merge some?" → enter at Stage 1
- "the priority descriptions are vague, let's sharpen them" → enter at Stage 1
- "what context tags need attention?" → run a quick Stage 1 audit across the
  context keys
- "undo what we did last time" / "roll back yesterday's component changes" →
  jump straight to **Rollback** below

These are **context** keys — the durable dimensions the operator owns
(`product`, `component`, `priority`, `product-version`, `feature`/`bug`,
integration keys). That is this skill's home ground. Do **not** raise
`work-type`, `phase`, `resolution`, or other lifecycle tags on your own —
those are engine-managed and out of scope unless the operator names them (see
the lifecycle check below).

You do not march through all six stages every time. Enter at the stage that
matches the intent, and loop back when a correction reveals an upstream
problem (a bad classification almost always means a bad description — go fix
Stage 1).

### Lifecycle check — only if the operator asks

**If and only if** the operator's intent names a lifecycle key (`work-type`,
`phase`, `resolution`, `lifecycle`) or describes lifecycle work — e.g.
"backfill `work-type` coverage", "classify the done workunits by
`work-type`", "fix the phase descriptions" — then read the
`references/lifecycle-tags.md` file located alongside this SKILL.md (same
skill directory) and follow it; it covers the engine-managed keys, the
refine-only constraint, the fact that lifecycle tags live on **WorkUnits**
(not Outcomes — only context tags inherit down the DAG), and the `work-type`
backfill playbook. Otherwise never raise lifecycle tags — keep the whole
conversation on context keys.

## The six-stage flow

### Stage 1 — Vocabulary audit

Call `mcp__wms__wms_listTags`. Each entry is `{tag_key, tag_value, category,
cardinality, description, is_seed, required}`. For the target context key (or
every context key, for a general "what needs attention" audit), assess
quality:

- **Clarity.** Is each description sharp enough to classify against? Flag the
  vague ones. "the CLI" is a problem; "The `teamster` CLI and its subcommands
  — NOT the MCP servers, which are `component:mcp`" is a rule.
- **Near-duplicates.** Are two values splitting hairs the operator doesn't
  care about? Propose a merge — retire one with `wms_retireTag`, fold its
  meaning into the survivor's description with `wms_describeTag` (the
  refinement tool; see Stage 3).
- **Gaps.** Is there a dimension value with no home? ("You tag
  `component:cli` and `component:wms` but nothing for the installer — should
  we add `component:install`?")
- **Required-ness.** Should this context key be `required`? Set it via
  `wms_defineTag` with the `required` flag — a per-KEY property; see Stage 6.
- **New key from scratch.** If the operator wants a whole new dimension
  (`product-version`, `risk`, `customer`), help them define the values and
  write each description as a rule, before any data exists to classify.

Present findings and proposed vocabulary changes. **Wait for confirmation
before modifying anything.** The audit is a proposal, not an edit.

### Stage 2 — Data inspection

Now look at the actual entities, both tagged and untagged for the target
context key. Use `mcp__wms__wms_listWorkUnits` / `mcp__wms__wms_listOutcomes`
to enumerate, `mcp__wms__wms_getWorkUnit` for detail. Report:

- **Coverage.** How many entities carry the key? How many are missing it? (A
  context key like `component` gets stewarded precisely when coverage is
  patchy.)
- **Distribution.** Across the values that ARE applied — is it lopsided? Is
  one value a dumping ground?
- **Patterns in the tagged set.** What do `component:dashboard`-tagged
  entities have in common? Look at title keywords, the parent outcome's
  theme, and sibling context tags. These patterns are the raw material for
  the rules you'll write in Stage 3.
- **A sample of the untagged set.** Show the operator a handful and ask them
  to classify a few by hand. This is the highest-value moment in the whole
  session: the operator's manual classifications teach you what they actually
  mean by each value, and any hesitation reveals an ambiguous description to
  fix.

Cross-reference other WMS signal that can inform classification: other
context tags on the entity, the parent outcome's title and tags, and the
`git.branch` / session context. Present what you found; the operator
confirms, corrects, or adds nuance. **Corrections feed back to Stage 1** — if
a correction exposes a fuzzy description, sharpen it before going further.

### Stage 3 — Rule expression

Help the operator put classification rules into words, then **bake those
words into the descriptions** — because the description IS the rule. Rules
range from simple to conditional to hierarchical:

- Simple: "title mentions a panel or the SSE feed → `component:dashboard`"
- Conditional on other tags / dates: "`product-version:v1.0` when
  `git.branch=main` and created before 2026-06-05"
- Hierarchical: "anything under outcome `installer` is `component:install`"

Write the rule into the value's `description` so the next session reads it
back. Example for `component:dashboard`:

> "The web dashboard served by hookd: the SSE feed, the WMS hierarchy view,
> the cost-flow and tag-browser pages, and their panel templates.
> Indicators: title mentions a panel, a chart, SSE, or `/wms`. NOT the
> terminal `feed` viewer (that's `component:feed`) and NOT the hookd HTTP
> plumbing (that's `component:server`)."

To change an **existing** value's description, use
`mcp__wms__wms_describeTag(tagKey, tagValue, description)`. This is the one
tool that refines a description in place. Refine until it classifies cleanly
— a description sharp enough to classify against is the deliverable.
Descriptions are capped at **1024 characters** — budget for it while
drafting a rich rubric (prioritize indicators and NOT-this-value exclusions
over exhaustive prose) rather than discovering the limit when
`wms_describeTag` rejects it and truncating under pressure.

Do not reach for the other two tools to refine — they can't:

- `mcp__wms__wms_tagEntity`'s `description` is recorded **only** when a value
  is first introduced (create-only — it never overwrites an existing one).
  So it defines a brand-new value, but it cannot refine one that already
  exists.
- `mcp__wms__wms_defineTag` defines/seeds **context-key** vocabulary and sets
  the `required` flag. Use it for a new context value or to mark a key
  required, not to rewrite an existing description — that's
  `wms_describeTag`.

### Stage 4 — Batch proposal

Apply the refined vocabulary to the untagged entities. **Pick your execution
model by volume** — this is the model-tier decision:

- **≤20 entities — classify inline.** The descriptions are now sharp; walk
  the entities yourself and call `mcp__wms__wms_tagEntity` in a loop (source
  `steward`, see Stage 5). Present each batch to the operator for review.
- **20+ entities — don't classify inline.** Either:
  - **Hand a cheaper agent the work.** Give a lighter-weight subagent the
    refined descriptions as the rubric and the entity list, and have it emit
    a proposed-tags plan as a JSONL file. You review the plan with the
    operator, then apply it. The whole point of sharpening the descriptions
    is that a cheaper model can now classify against them reliably.
  - **Or write a one-off script** when classification is mechanical
    (conditional on other tags, date ranges, cross-entity joins) — a
    standalone script or SQL query that reads the entities and context,
    applies the rules, and writes a JSONL plan. Review the plan, then apply.

Either way the output is a reviewable **plan**, not a fait accompli. The
operator reviews in batches; corrections refine the rules (back to Stage 3);
the cycle repeats until they're satisfied. Never bulk-apply without a review
pass.

### Stage 5 — Execute with rollback

**Snapshot before you apply — one logical batch, one call.** Rollback is a
first-class operation, not an afterthought, so every applied batch is
captured first. `wms_snapshotEntityTags` writes `<batchID>.jsonl` fresh on
every **successful** call — a second call reusing the same batch ID
**replaces** the file, it does not append. That replace is success-gated:
the tool builds the new snapshot in a private temp file and swaps it in only
once every line is written without error, so a call that fails partway (a
store error, a full disk) leaves whatever snapshot was already at that batch
ID untouched — no corrupt half-written file, no lost prior snapshot. If a
snapshot call errors, nothing changed at that batch ID; just retry the whole
call. If a batch spans both outcomes and workunits, snapshot them **together
in one call**, not as two calls sharing a batch ID — a *successful* second
call still silently erases the first's rollback data, both calls report
success, and you find out only when a rollback comes up short.

That retry safety holds **only before you've applied anything.** Do not
re-run `wms_snapshotEntityTags` under the same batch ID once an apply is
already underway. A partial apply — say `wms_tagEntity` fails on entity 60 of
149, e.g. an over-length description — leaves the first 59 entities already
carrying the steward's new value with source `steward`. Re-snapshot now and
that steward-applied value becomes the recorded "prior" state: `wms_rollbackTags`
will find each steward-sourced binding, delete it, see a non-empty
`old_value`, and restore it right back to the value it just deleted —
reporting `reverted: 59` while having changed nothing. If an apply fails
partway through a batch, do not restart Stage 5 from the snapshot step; work
from the snapshot you already have.

This is different from splitting a mixed batch across two **different** batch
IDs (e.g. `...-outcomes` and `...-workunits`) — that pattern remains valid.
Each file rolls back independently via its own batchID, so an existing
`-out`/`-wu` pair on disk from before this fix is not corrupt; roll each one
back separately if you ever need to. The bug was reusing one batch ID across
two calls, not using two batch IDs.

Choose a batch id in the format `steward-<key>-<YYYYMMDD-HHMMSS>` (e.g.
`steward-component-20260610-2230`). Then snapshot the current state of every
entity you're about to touch, regardless of type, in that one call:

```
mcp__wms__wms_snapshotEntityTags(entities=[{entityType, entityID}, ...], tagKey, batchID)
```

The `entities` array is the recommended shape for any batch — it's what lets
outcomes and workunits go in together. A legacy single-type form
(`entityType` + `entityIDs[]`) still works and is fine when a batch is
entirely one entity type; send one form or the other, never both in the same
call.

It writes one JSONL line per entity to
`$TEAMSTER_BASEDIR/var/tag-steward/<batch-id>.jsonl` and returns the path.
Each line records the **pre-change** state — captured before any tag is
applied, so it never carries the new value:

```jsonl
{"entity_type":"workunit","entity_id":"wu-foo","tag_key":"component","old_value":"","old_source":"","batch":"steward-component-20260610-2230"}
```

`old_value: ""` means the tag didn't exist before; `old_source` is whatever
the prior tag's source was (empty when there was no prior tag). That stored
shape is exactly what `wms_rollbackTags` reads back to restore.

**Verify before you apply.** `wc -l` the snapshot file and compare the count
to the total number of entities you meant to include in this logical batch —
not just what you passed to the most recent call, but the sum across every
`snapshotEntityTags` call you made under this batch ID. Under the one-call
rule above that's the same number either way, but the check is what catches
you if you slip back into the old two-call habit: the P0 incident's snapshot
also "matched" its own inputs at every step, 128 lines then 21, and still
lost 128 rows.

A matching total line count does not confirm every row is *correct*, though.
The `entities` form scatters an independently-typable `entityType` across
each row, and a mistyped one (`outcome` where you meant `workunit`) produces
a normal-looking `old_value:""` line — the count still balances, and
rollback would silently skip that entity later. For a mixed batch, also
spot-check the per-type split, e.g. `grep -c '"entity_type":"outcome"'
<path>` against how many outcomes you expected. Run both checks every time,
not just when something feels off, and do not proceed to apply on any
mismatch.

Apply the tags with `mcp__wms__wms_tagEntity` using **source `steward`** —
that is what makes them distinguishable from `manual`, `classifier`, and
`inherited`, and it is what rollback keys on.

### Stage 6 — Persist

The durable artifacts of a steward session, all already in place by the time
you finish a batch — nothing extra to write:

- **Refined descriptions** in the `tags` table — the improved vocabulary,
  the real prize.
- **Applied `entity_tags`** with `source='steward'`.
- **Rollback snapshots** under `$TEAMSTER_BASEDIR/var/tag-steward/`.
- **The conversation itself** in the transcript — the reasoning behind it
  all.

If the audit surfaced required-ness or new-key decisions, persist those too:
mark a key required or add values with `mcp__wms__wms_defineTag`; demote an
obsolete value with `mcp__wms__wms_retireTag` (non-destructive — it stays in
history, just leaves the active keyspace).

## Rollback

Rollback is first-class. The operator says "undo what we just did" or "roll
back the component changes from yesterday." To do it:

1. **List available batches** from `$TEAMSTER_BASEDIR/var/tag-steward/` (the
   `<batch-id>.jsonl` files). Match the operator's words to a batch id; if
   it's ambiguous, show the candidates and ask.
2. **Revert** via `mcp__wms__wms_rollbackTags(batchID)`. It reads the
   snapshot and, for each entry, reverts the tag **only if its source is
   still `steward`** — meaning a human hasn't overridden it since. If the
   source changed to `manual`, it **skips** that entry, respecting the
   human's override.
3. **Report** what it returns: counts of reverted, skipped
   (human-overridden), and failed. Relay these to the operator plainly — a
   skip is not an error, it's the system declining to clobber a manual
   decision.

Rollback reverts tags. It does **not** undo description refinements — those
are deliberate vocabulary improvements and the durable value of the session.
If the operator wants a description changed back, that's a Stage 1/3 edit,
not a rollback.

## What the steward can modify

| Action | How |
|--------|-----|
| Refine an existing value's description | `mcp__wms__wms_describeTag(key, value, description)` |
| Apply tags to entities | `mcp__wms__wms_tagEntity` with source `steward`, or a dispatched plan |
| Propose new tag values | `mcp__wms__wms_defineTag` (context keys) with a description; `wms_tagEntity` records a description only on first introduction |
| Mark a key required | `mcp__wms__wms_defineTag` with the `required` flag (per-KEY) |
| Retire a value | `mcp__wms__wms_retireTag` — non-destructive demotion |
| Snapshot before a batch | `mcp__wms__wms_snapshotEntityTags` |
| Roll back a batch | `mcp__wms__wms_rollbackTags` |

The steward works on **tags only**. It must NOT touch entity titles,
descriptions, status, or schema — and it never runs SQL directly against the
WMS database; all changes go through the MCP tools above.

## Operating discipline

- **Vocabulary changes need confirmation.** Never refine a description,
  merge values, or mark a key required without showing the proposal first.
- **Bulk application needs a review pass.** Always produce a reviewable plan
  for 20+ entities; never bulk-apply blind.
- **Snapshot before every batch.** No applied batch without a snapshot —
  that is the rollback contract.
- **Respect human overrides.** Rollback skips anything a human re-tagged.
  Don't work around that.
- **Sharpen, don't sprawl.** The win is fewer, clearer values with rules
  baked into their descriptions — not an ever-growing keyspace of one-off
  labels.
- **Context by default; lifecycle only on request.** Keep the conversation on
  context keys. Raise `work-type`/`phase`/`resolution` only if the operator
  does — then read the lifecycle reference and follow it.

## Reference

- [lifecycle-tags.md](references/lifecycle-tags.md) — the engine-managed
  keys (`work-type`/`phase`/`resolution`/`lifecycle`): the refine-only
  constraint and the `work-type` backfill playbook. Read it **only** when the
  operator asks for lifecycle work (see Step 0's lifecycle check).
