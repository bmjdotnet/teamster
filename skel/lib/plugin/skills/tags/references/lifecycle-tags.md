# Lifecycle Tags — Steward Reference

Read this only when the operator's intent names a lifecycle key
(`work-type`, `phase`, `resolution`, `lifecycle`) or describes lifecycle work
(e.g. "backfill work-type coverage", "classify the done workunits by
work-type"). For ordinary context-key stewardship — `product-version`,
`component`, `priority`, integration keys — you do not need this file; stay in
the main skill.

## What lifecycle tags are

The WMS tag keyspace splits two ways:

- **Context tags** (`category=context`): durable metadata the operator owns —
  `product`, `component`, `priority`, `product-version`, `feature`/`bug`,
  integration keys. The steward freely defines values, refines descriptions,
  merges, retires, and backfills these. This is the main skill's home ground.
- **Lifecycle tags** (`category=lifecycle`): execution tracking the **engine and
  classifier own** — `work-type`
  (feature|bug|refactor|polish|investigation|research|test|docs|infra|admin|processor),
  `phase` (design|build|test|review|iterate), `resolution` (two values
  written now: `achieved` by a human's close-out, `swept-unreviewed` by the
  nightly `wms review-sweep` timer closing a delivered-but-unreviewed
  WorkUnit — deliberately silent on whether the work was good, only that a
  deliverable exists and nobody looked; a dropped Outcome or WorkUnit gets
  the `abandoned` status instead, not this tag), `lifecycle` (archived).
  These values are seeded by migration and managed by the system, not by
  hand. **Legacy note:** you may still find `resolution:abandoned` on
  entities closed before v0.3.0, from when abandonment was `status=done`
  plus this tag — it's historical, never applied going forward.

The split matters because the tools behave differently on the two categories
(below). Surface lifecycle work only when the operator asks for it.

## Lifecycle tags live on WORKUNITS, not outcomes

This trips people up, so state it plainly before you classify anything: **only
context tags inherit down the Outcome→WorkUnit DAG.** Context tags set on an
Outcome are inherited automatically by every WorkUnit beneath it — you do not
re-apply them per WorkUnit. **Lifecycle tags do not inherit; they are
per-WorkUnit.** `work-type`, `phase`, and `resolution` are applied to each
WorkUnit individually.

So when you backfill or refine `work-type`, the entities you target are
**WorkUnits**, not Outcomes. Coverage is measured over WorkUnits; you snapshot,
apply, and roll back WorkUnit tags. An Outcome legitimately carries no
`work-type` of its own — that is correct, not a gap to fill. (The one exception
is `resolution`, applied on an Outcome that closed `achieved`, or on a
WorkUnit the nightly `wms review-sweep` closed `done` with
`swept-unreviewed` — but you do not backfill either; the close-out step or
the sweep sets it. An `abandoned` Outcome or WorkUnit that reached
`abandoned` via a `done → review` reopen carries no `resolution` tag — the
engine clears it on that edge; the status is the record. A `resolution` tag
applied by hand to an entity that was never `done` is untouched by that
clear and survives to `abandoned`.)

## The hard rule: you do not reshape lifecycle vocabulary

Lifecycle values are migration-owned. You must **not** add, retire, or
recategorize them, and the tools enforce this:

- `wms_defineTag` **refuses** the system-managed lifecycle keys
  (`work-type`/`phase`/`resolution`/`lifecycle`). You cannot add a new
  `work-type` value or flip a lifecycle key's category through it. That refusal
  is expected, not a bug.
- `wms_retireTag` likewise does not demote migration-seeded lifecycle values.

What you **can** do with lifecycle tags, and the only two things you should:

1. **Refine a value's description** with
   `wms_describeTag(tagKey, tagValue, description)`. This is the one tool that
   rewrites an existing description in place, and it works for any key — including
   lifecycle. Sharpening `work-type:bug` so a cheaper model can classify against
   it is exactly its purpose.
2. **Apply / backfill values** onto entities with
   `wms_tagEntity(... source="steward")`, always wrapped in the snapshot +
   rollback contract from the main skill's Stage 5 (snapshot the batch first,
   apply with source `steward`, roll back via `wms_rollbackTags`).

If the operator wants a genuinely new work classification, that is a vocabulary
change to the migration, not a steward action — tell them so rather than
reaching for `defineTag`.

## Why `work-type` is the canonical backfill case

`work-type` ships **required** (per-KEY flag, set by migration). A workunit is
supposed to carry it before it reaches `done`, but historical data predates the
rule, so coverage is patchy. Filling that gap is the original reason this skill
exists — and it is pure lifecycle backfill, which is why it lives here and not in
the main flow.

The same six-stage flow from the main skill applies; the only differences are
that you refine (never redefine) the lifecycle vocabulary, and `work-type` is
your usual target. Walk it as: audit the `work-type` descriptions → inspect
coverage and tagged-set patterns → bake rules into the descriptions → propose a
batch → execute with snapshot/rollback → persist.

### Refined-description worked example

The durable artifact is a description sharp enough to classify against. For
`work-type:bug`, refine via `wms_describeTag("work-type", "bug", ...)` to:

> "Fixes incorrect existing behavior. Indicators: title contains 'fix', entity
> has a `bug:*` context tag, parent outcome is about debugging or repair,
> interval phases show build→test→iterate (the correction pattern). NOT infra
> even when it fixes a build script — infra fixes tooling, bug fixes product
> behavior."

A description like that lets a cheap model (or a human) classify reliably; a
vague one ("a bug fix") does not. Sharpen the description **before** classifying
anything against it.

### Cross-reference other WMS signal

When classifying an untagged workunit's `work-type`, the title alone is rarely
enough. Pull in the rest of the WMS context:

- **Sibling context tags** — a `bug:*` tag points at `work-type:bug`; a
  `feature:*` tag points at `feature`.
- **Parent outcome** — its title and tags frame what the work was for (an
  outcome about "docs cleanup" makes `docs` likely).
- **Interval phase history** — a build→test→iterate arc reads as a correction
  (bug); a clean build→test→done arc reads as new work (feature).
- **Git branch / session context** — branch names and the session's other work
  often disambiguate.

Show the operator a sample of untagged entities and ask them to classify a few
by hand first — their answers teach you what they mean by each value and expose
any description that's still too vague to fix.

## Volume → execution model (same as the main skill)

The model-tier decision is identical to the main skill's Stage 4:

- **≤20 entities — classify inline.** Walk them yourself, call
  `wms_tagEntity(... source="steward")` in a loop, present each batch for review.
- **20+ entities — don't classify inline.** Either hand a cheaper agent
  (sonnet, or haiku for obvious cases) the refined descriptions as the rubric
  plus the entity list and have it emit a JSONL plan you review and apply, or
  write a one-off script (Go/SQL) when the rule is mechanical (conditional on
  other tags, date ranges, joins). Either way the output is a reviewable plan,
  never a blind bulk-apply.

## Execute with rollback — unchanged

Lifecycle backfill uses the exact snapshot/rollback contract from the main
skill's Stage 5: choose a batch id `steward-<key>-<YYYYMMDD-HHMMSS>`, call
`wms_snapshotEntityTags` before applying, apply with source `steward`, and roll
back with `wms_rollbackTags(batchID)` — which reverts only entries whose source
is still `steward` and skips anything a human re-tagged as `manual`. Refining a
description is **not** rolled back; that is a deliberate vocabulary improvement.
