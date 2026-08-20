---
name: start
description: The Teamster front door. Gathers the session objective, recommends an operating model and proposes context tags in a single batched prompt, then dispatches to team or solo setup. Use this at the start of any session.
disable-model-invocation: true
argument-hint: "[focus slug — what this session is working on]"
---

# Start a Teamster Session

This is the recommending front door. It gathers the objective, pre-computes a
mode recommendation and context-tag proposals, then presents **both questions in
a single batched AskUserQuestion** — one round-trip instead of a serial
interview. On confirmation it dispatches to the committed path:

- **Team** → the team-mode flow (persistent named teammates, WMS
  tracking, dispatch routing).
- **Subagent (solo)** → the solo-mode flow (one primary agent, strategic Outcome
  + context-tag interview inline, ephemeral review subagents).

Here "dispatch" means: **read the chosen skill's file and follow its steps inline
in this same session** — NOT a Skill-tool call (those skills set
`disable-model-invocation` and cannot be invoked that way; see Step 4).

The recommendation is an aid to choosing, never a gate. `/teamster:solo` remains
directly invocable **by the operator** (user-typed slash command) — someone who
already knows they want solo mode skips this interview by typing the command.
Team mode is the safe default: when the signals are ambiguous, recommend team.

This skill is the one place `../shared/session-protocol.md`'s Steps 1–2 and
the outcome-search half of Step 6 run for most sessions — the dispatched
mode skill (`bootstrap` or `solo`) is told to skip its own copy of them and
use what's gathered here instead. Everything below is what's genuinely
`start`-specific: the batched mode+tag interview and the dispatch handoff.

## Step 1 — Gather the objective + context (once)

Establish what this session is for. This is the same intake
`session-protocol.md` Steps 1–2 describe — run it here ONCE and hand the
result down so the dispatched skill does not re-ask.

**Focus slug.** Follow `../shared/session-protocol.md` Step 1 — use
`$ARGUMENTS` if non-empty, otherwise ask with AskUserQuestion (header
"Session focus").

**Load WMS tools needed for context gathering** (deferred — load before first
use). The dispatched skill loads the full set (session-protocol Step 5);
start only needs these for the interview:

```
ToolSearch("select:mcp__wms__wms_listTags,mcp__wms__wms_listOutcomes")
```

**Context (best-effort, skip any step that fails).** Gather what the dispatched
skill would otherwise gather, so it inherits it:
- `git remote -v` and `git branch --show-current` (integration keys + branch).
- The project's CLAUDE.md (declared product, version, components, trackers).
- The conversation so far and recently-touched files.
- Call `mcp__wms__wms_listTags` to inspect the tag vocabulary (needed for Step 2b).

**Outcome search.** Extract 2–3 keywords from the focus slug (e.g., "fix the
auth timeout bug in the login flow" → `"auth timeout"`) and call
`wms_listOutcomes(query="<keywords>")` — the search half of session-protocol
Step 6 (do NOT create an Outcome here; that happens in the dispatched skill).
Omit `status` so `done` outcomes surface too, not just open ones — a match on
a `done` outcome is the signal for the rework branch in Step 3/4 below. Note
any matches — they'll be presented in Step 3 if found. If more than 3
outcomes match, keep only the 3 most recently updated.

**Verify referenced artifacts.** Follow `../shared/session-protocol.md`
Step 2. Do this now, since this is the interview that actually runs for
most sessions — the dispatched skill's own copy of this check only fires
when the operator invokes it directly, bypassing `start`.

Keep this context — you pass it to the dispatched skill in Step 4 so it skips
its own redundant gather. One interview, not two.

## Step 2 — Pre-compute the batched interview (internal, no output)

Before presenting questions, do the internal work silently:

### 2a — Form a mode recommendation

Read the objective and the gathered context and **reason** toward team or
subagent. This is judgment in prose — there is no score, weight, or threshold.
Match the objective's shape to the signals below.

**Signals that lean TEAM** (breadth / parallelism / coordination value):
- the objective spans **multiple subsystems** or files that can progress
  independently;
- it is **multi-phase with handoffs** where fresh-context separation pays;
- it is **broad / parallelizable** — decomposes into 2–5 pieces with little
  shared mutable state;
- it explicitly wants **adversarial review by a different agent** as a hard gate.

**Signals that lean SUBAGENT** (focus / tight coupling / exploration):
- a **single tightly-coupled change** — one file or one cohesive unit;
- **exploratory / investigative** work where the path isn't yet decomposable;
- **short-horizon** work that wouldn't amortize the team setup;
- the operator signals they want to **drive it themselves**.

**When signals conflict or are thin, recommend TEAM** — the enforcing default
that preserves the no-bare-agents rail and review separation. Say so plainly
in the rationale.

### 2b — Pre-compute context-tag proposals (from manifest)

Call `wms_listTags` (no args) to get the role-shaped manifest — see
`../shared/session-protocol.md` Step 7a for what each role group
(`propose`/`autoExtract`/`requiredLifecycle`/`required`/`engineManaged`)
means and Step 7b for the work-scope-slug convention. Apply that same
reasoning here to build candidates; the difference from the shared
interview is only in how they're presented (batched multiSelect below,
instead of a free-form conversational confirm).

**Split into interview buckets:**
- Auto-apply: everything from `autoExtract` (extract values, apply silently)
- Confirm via multiSelect: up to 4 keys from `propose`

**Work-scope slug.** If the focus slug implies a work type (e.g., "fix auth
timeout" → bug, "add prometheus exporter" → feature), include the matching slug
key in the multiSelect proposals per session-protocol Step 7b's convention —
drill down with `wms_listTags(tagKey=...)` to find existing values, propose a
new value only if no existing one fits.

**Reuse existing values:** Check `wms_listTags` results for existing
`(tag_key, tag_value)` pairs — reuse (case-insensitive slug match), don't
duplicate.

## Step 3 — Present the batched interview

Present **one AskUserQuestion** with both questions. This is the single
round-trip that replaces the old serial mode-then-tags interview.

Write a short introductory sentence with the mode rationale tied to the focus
slug, then call AskUserQuestion. If matching outcomes were found in Step 1,
include Q0 before the mode and tag questions; otherwise skip Q0 entirely:

```
AskUserQuestion with questions:
  Q0 (single-select, header "Outcome") — ONLY if matching outcomes were found:
    question: "Found outcomes that may match your focus. Continue existing
               work, rework something closed, or start new?"
    options (up to 3 matches + "New outcome"):
      - label: "<outcome-id>: <title> (<status>)"
        description: "<description snippet or focus string>"
      - ... (up to 3 most recent matches — status in the label tells you
        continuation vs. rework: `done` is rework, anything else continues)
      - label: "New outcome"
        description: "Create a fresh strategic Outcome for this session"

  Q1 (single-select, header "Mode"):
    question: "<rationale tied to the focus slug — why you recommend this mode>"
    options (3):
      - label: "<Recommended mode> (Recommended)"
        description: "<what this mode means + why it fits>"
      - label: "<Other mode>"
        description: "<what this mode means>"
      - label: "Let's discuss this"
        description: "I want to talk through the mode choice before deciding"

  Q2 (multiSelect, header "Tags"):
    question: "Which context tags for the strategic Outcome?
               (I'll also auto-apply <list auto-apply tags>.)"
    options (up to 4, each with source attribution in description):
      - label: "product:<value>"    description: "from CLAUDE.md / repo name"
      - label: "feature:<slug>"     description: "from your focus slug"
      - label: "priority:<level>"   description: "defaulting <level>"
      - label: "<other tag>"        description: "<source>"
```

**Notes on the tag multiSelect:**
- Mention the `autoExtract` tags in the question text so the operator knows
  they're being set, but don't waste a multiSelect slot on them.
- Only include keys from the `propose` group in the multiSelect options.
  Respect `exclusive` — propose at most one key per exclusion group.
- Do NOT include `requiredLifecycle` keys in the interview — the lead applies
  them per-WorkUnit at dispatch time (see the mode skill's dispatch step), not
  on the strategic Outcome.
- The operator can add tags via "Other" (free text) — parse additions and apply
  them alongside the selected tags.

### Handling "Let's discuss this"

If the operator selects **"Let's discuss this"** on the mode question:
1. Ignore the tag selections from this batch (they may change after discussion).
2. Engage conversationally on the mode choice — explain the tradeoffs, ask what
   aspect they want to explore.
3. Once the mode is resolved, re-present the tag question alone (a single
   AskUserQuestion) so the operator can confirm tags.

If the operator uses "Other" on the tag question with discussion-like text
("let's talk about this", "not sure", "what do you recommend for X"):
1. Treat it as a request to discuss tags conversationally.
2. Engage on the specific concern, then re-present the tag confirmation.

If neither escape is triggered, proceed directly to Step 4.

## Step 4 — Set the mode, apply tags, and dispatch

Once all answers are confirmed:

**If the operator selected an existing outcome (Q0), check its status —
this is the same three-way triage as session-protocol Step 6, just decided
here instead of there:**

- **Not `done`** (session-protocol Step 6 branch B — continuation): do NOT
  create a new Outcome in the dispatched skill — use the selected one. Set
  focus on it (`wms_setFocus`). Do not change its status. Apply any
  confirmed tags that aren't already on the outcome (call `wms_getOutcome`
  to check existing tags first). Carry the selected outcome forward to the
  dispatched skill so it skips its own creation step.
- **`done`** (session-protocol Step 6 branch C — rework): **never reactivate
  it.** Do NOT carry it forward as the strategic Outcome and do NOT skip the
  dispatched skill's Step 6 — treat this exactly like the "New outcome" path
  below (the dispatched skill creates a fresh Outcome), but pass along which
  done outcome triggered the rework so the dispatched skill can run branch
  C's relation step in full: `wms_listRelationKinds` +
  `wms_addRelation(fromType="outcome", fromID=<new outcome>,
  toType="outcome", toID=<the done outcome>, kind=<best-fit kind>)` once the
  new Outcome exists. Apply the confirmed tags to the new Outcome fresh (same
  as the "New outcome" path — a rework Outcome doesn't inherit tags from the
  closed one it's related to).

If the operator selected "New outcome" or Q0 was not shown (no matches),
proceed as today — the dispatched skill creates the Outcome
(session-protocol Step 6 branch A).

**Set the mode.** Follow `../shared/session-protocol.md` Step 3, using the
mode chosen here (`mode="solo"` for Subagent, `mode="team"` for Team). Call
it **once**, before dispatching.

**Apply the confirmed tags.** Using `mcp__wms__wms_tagEntity` on the strategic
Outcome (source `manual`) — the apply mechanic is session-protocol Step 7d —
after the dispatched skill creates it:
- Apply the tags the operator selected in the multiSelect.
- Apply the auto-apply integration keys (git.branch, github.owner, etc.).
- For tags added via "Other", parse and apply them.
- Reuse existing `(tag_key, tag_value)` pairs (case-insensitive). For genuinely
  new values, pass a `description`. For genuinely new keys, seed with
  `wms_defineTag` before applying.

**Dispatch by following the chosen skill INLINE — do NOT call the Skill tool.**
`/teamster:solo` and the bootstrap skill both set `disable-model-invocation`,
so a `Skill(teamster:solo)` / `Skill(teamster:bootstrap)` call **errors**. To
dispatch, **Read the chosen skill's file and follow its steps yourself, inline,
in this same session**, reusing the focus slug + Step-1 context AND the
confirmed tag set.

- **Subagent** → Read `../solo/SKILL.md` and follow it inline. It sends you
  to `session-protocol.md` for team identity (Step 4 — **register it via
  `registerPeer` at 4b, do not skip this**), WMS tool loading (Step 5), and
  Outcome creation/resume (Step 6). **Skip session-protocol Step 7** (the tag
  interview — already confirmed here) — apply the confirmed tags yourself
  per Step 7d (including `team:<name>`) once the Outcome exists, then
  continue with Step 8 (focus) and solo's own Step 6 (proceed with work).
  (You already called `setMode("solo")`.)
- **Team** → Read `../bootstrap/SKILL.md` and follow it inline. Same pattern:
  it sends you to `session-protocol.md` for team identity (Step 4 —
  **register it via `registerPeer` at 4b, do not skip this**), WMS tool
  loading (Step 5), and Outcome creation/resume (Step 6). Once the Outcome
  exists, run bootstrap's "is this part of something bigger?" check, then
  **skip session-protocol Step 7** (the tag interview — already confirmed
  here) — apply the confirmed tags yourself per Step 7d, then continue with
  Step 8 (focus) and bootstrap's dispatch protocol. (You already called
  `setMode("team")`.) Do not call `registerPeer` a second time — it is
  called exactly once, at session-protocol Step 4b.

Carry the context forward — do **not** re-ask the focus slug, do **not**
re-run the tag interview, and do **not** call the Skill tool to enter the
sibling skill.

## Notes

- A session that never runs `/teamster:start` (or `/teamster:solo`) behaves
  exactly as today: the team mandate and the bare-`Agent` block are enforced.
  The interview is an aid to choosing, never a precondition for safety.
- Run this before any dispatch — it is step zero. A subagent spawned before the
  mode is set runs under the enforcing default (team), which is the safe
  direction.

## Reference

- [session-protocol.md](../shared/session-protocol.md) — the shared intake
  mechanics both dispatch targets build on.
- [solo/SKILL.md](../solo/SKILL.md) — the dispatched subagent path.
- [bootstrap/SKILL.md](../bootstrap/SKILL.md) — the dispatched team path.
- [eight-rules.md](../bootstrap/references/dispatch-pack/eight-rules.md) —
  the protocol + the solo carve-out (which rules apply in each mode).
