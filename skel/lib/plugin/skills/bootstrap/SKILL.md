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

## Step 1 — Get the focus slug

A team name must be unique across all live Teamster instances on the hub.
Two sessions both calling their team `teamster` collide. The team name is
derived from a **focus slug** describing what this team will work on, so
that the name is meaningful and reasonably unique.

- If you arrived here via `/teamster:start`, it already gathered the focus slug
  and context (git remote/branch, CLAUDE.md, recent files) and confirmed **team**
  mode — use what it handed down and do NOT re-ask the slug; skip to Step 2.
- Otherwise, if `$ARGUMENTS` is non-empty, use it as the focus slug.
- Otherwise, ask the user with AskUserQuestion:
  - question: "What is this team focused on? (a short phrase — used to
    generate a team name)"
  - header: "Team focus"
  - options:
    - one option per plausible focus you can infer from the conversation
      so far (recent files, recent prompts, the working directory's
      purpose). 2-3 options max.
    - The user can always pick "Other" and type their own.

Keep the slug — you'll use it to seed the strategic Outcome (Step 5) and
the context-tag interview (Step 6).

### Verify referenced artifacts before proceeding

If the focus slug or the operator's brief names specific local files, paths,
or kits (e.g. "apply the fix from the pricing kit," "pick up the devkit at
`/path/to/thing`"), verify each one exists — `ls` the path or `Read` the
file — before treating it as real. If something named is missing, say so
and confirm intent with the operator (wrong path? not built yet? proceed
without it?) rather than silently assuming it exists or improvising its
contents. A session — or a teammate you're about to dispatch — that burns
its first several turns acting on a kit that was never actually delivered
is a worse outcome than a 10-second existence check up front.

**Set the session mode (first action).** Team mode is the default, but set the
marker explicitly so a session that previously chose solo is returned to
enforcement. Load and call the mode signal:

```
ToolSearch("select:mcp__activity__setMode")
mcp__activity__setMode(mode="team")
```

`setMode` is a no-op confirmation tool; the Teamster hook does the real work
(here, clearing any solo marker so the Teamster hook knows this is a team session).
Call it **once** on entry — not every turn. If you arrived
via `/teamster:start` it already set team mode — calling again is harmless
(idempotent).

## Step 2 — Generate a team name

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
work (check `wms_listTags(tagKey="team")`), reuse it — two sessions sharing
a team name signals they're part of the same effort. Don't force uniqueness
when continuity is the right signal.

Record this name as the `team` tag value for WMS attribution. The session's
team is implicit — no creation step needed. Use this name when tagging the
strategic Outcome (Step 6d) with `team:<name>`.

## Step 2.1 — Register the team name

Call `registerPeer` to write the team name into the operational tables so
it is visible to health dashboards and scopes roster/health queries to
team members.

**You MUST pass `session_id`.**  hookd auto-registers a roster entry for
your session on the first hook event (before this skill runs). When you
pass `session_id`, `registerPeer` finds that existing entry and updates its
`team_name` in place — no duplicate, no orphan. Without `session_id`, it
creates a second unbound entry that ctop and health never see.

Extract your session_id from the scratchpad path in your system prompt
(the UUID segment, e.g. `.../6ebee3a6-.../scratchpad` → `6ebee3a6-...`).

```
registerPeer(
  agent_name: "",
  runtime: "claude_code",
  relationship: "lead",
  team_name: "<team-name>",
  session_id: "<your session UUID>"
)
```

This is NOT the same as the `team` tag on the Outcome (Step 6d) — that is
WMS-domain work attribution. This is operational identity: it makes the
team name visible in ctop, the health API, and roster scoping.

## Step 3 — Read the protocol

Before loading tools or creating anything, read these three files now (use the
Read tool) — they define how you operate as team lead for the rest of this
session:

- ${CLAUDE_SKILL_DIR}/references/eight-rules.md
- ${CLAUDE_SKILL_DIR}/references/field-guide.md
- ${CLAUDE_SKILL_DIR}/references/muster-guide.md

## Step 4 — Load WMS tools

You own all WMS state directly. Load the tools in one batch (these are deferred
tools — load before first use):

```
ToolSearch("select:mcp__wms__wms_createOutcome,mcp__wms__wms_createWorkUnit,mcp__wms__wms_updateOutcomeStatus,mcp__wms__wms_updateWorkUnitStatus,mcp__wms__wms_assignWorkUnit,mcp__wms__wms_listOutcomes,mcp__wms__wms_getOutcome,mcp__wms__wms_listWorkUnits,mcp__wms__wms_setFocus,mcp__wms__wms_tagEntity,mcp__wms__wms_listTags,mcp__wms__wms_defineTag,mcp__wms__wms_retireTag")
```

Do this once. Do not call ToolSearch for WMS tools again after this.

### WMS entity model

```
Outcome (what you're trying to achieve) — DAG, can have multiple parents
  +-- WorkUnit (bounded work assigned to an agent) — always under one Outcome
```

Strategic outcomes are root nodes (no parent); tactical outcomes are nested
children (pass `parentOutcomeIDs`). WorkUnits are concrete, bounded work items.

### WMS state machine (use EXACTLY these strings)

```
Outcome:   pending -> active -> review -> done
WorkUnit:  pending -> active -> review -> done
Both:      (any) -> blocked,  blocked -> (any prior)
```

`done` is the only terminal state. "complete", "achieved", "assigned",
"planning", "open", "closed", "in_progress" are NOT valid status strings.
NEVER run SQL directly against the WMS database. All state changes go through
MCP tools.

## Step 5 — Create (or resume) the strategic Outcome

> **Skip search if arrived via `/teamster:start` and the operator already
> picked an outcome.** The start skill already ran the outcome search and the
> operator selected an existing outcome or "New outcome" — use that decision.
> If the operator picked an existing outcome, it is your strategic Outcome;
> skip creation and proceed to Step 6. If they picked "New outcome", create
> as below.

Before creating a new Outcome, search for existing open outcomes that match
the focus slug: extract 2–3 keywords from the slug (e.g., "implement remote
install" → `"remote install"`) and call
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
- Proceed to Step 6 (tags) — check existing tags first and skip any already
  present.
- Then Step 7 (focus).

**If the operator picks "New outcome" or no matches were found:**
Call `mcp__wms__wms_createOutcome` with an id based on the focus slug. A root
Outcome (no parent) IS the strategic one — its altitude comes from DAG position,
so there is no altitude tag to apply. Set status to `active`
(`mcp__wms__wms_updateOutcomeStatus`).

## Step 6 — The context-tag interview (set the strategic Outcome's tags + goals)

> **Skip if arrived via `/teamster:start`.** The start skill already ran the
> batched interview (mode + tags in one prompt) and confirmed the tag set with
> the operator. The tags are ready to apply — do so now on the strategic Outcome
> you just created (or resumed) in Step 5, then proceed to Step 7. Do NOT
> re-ask or re-propose tags.

> **When resuming an existing outcome:** The outcome may already have context
> tags applied. Call `wms_getOutcome` to check, and only propose tags that
> aren't already set. Skip the interview entirely if the outcome is fully
> tagged.

This is YOUR conversation with the operator. Its purpose is to characterize
what this work IS before anyone touches code, by proposing **context tags** for
the strategic Outcome. **Tag classification IS the goal-setting conversation:**
an agent that knows it's working a "p0 feature for teamster v0.2" operates
differently than one with no context. The tags you confirm here are applied
directly to the strategic Outcome and inherit down the DAG to every Outcome
and WorkUnit below it.

This is a **prompt pattern**, not an engineered system. You use your judgment
to match context to vocabulary and express uncertainty in plain language —
there is no scoring or confidence metric to compute.

### Step 6a — Inspect the key manifest

Call `mcp__wms__wms_listTags` (no args) to get the role-shaped manifest. The
response groups keys by role — no interpretation needed:

- **`propose`** — keys to offer the operator. `values` lists options (when
  present); `n` means drill down with `wms_listTags(tagKey=...)`. Respect
  `exclusive` (at most one key per exclusion group). Apply `scope: "outcome"`
  keys to the Outcome; keys without scope can go on either.
- **`autoExtract`** — extract silently from the environment (git, env).
- **`requiredLifecycle`** — lifecycle keys the lead MUST apply to every
  WorkUnit at dispatch time. Values are included (e.g. `phase`:
  design/build/test/review/rework; `work-type`: feature/bug/refactor/…).
  Do NOT propose these at the Outcome interview — apply them in Step 7.1.
- **`required`** — non-lifecycle keys required on every WorkUnit before
  close-out. Note for dispatch time.
- **`engineManaged`** — engine-only keys: do not propose, set, or modify.

### Step 6b — Extract and propose

**Auto-apply:** For each key in `autoExtract`, extract its value from the
named source (run `git remote -v`, `git branch --show-current` for `git`
sources; read CLAUDE.md for project metadata). Apply silently in Step 6d.

**Propose:** From the `propose` group, infer values using the focus slug,
CLAUDE.md, repo name, and existing vocabulary. For keys with `values`, reuse
existing options (case-insensitive slug match); for keys with only `n`, call
`wms_listTags(tagKey=...)` to drill down before proposing. Respect `exclusive`
— propose only one key per exclusion group.

**Work-scope slug convention.** The `propose` group includes slug keys that
parallel `work-type` values: `feature:<slug>`, `bug:<slug>`, `refactor:<slug>`,
`infra:<slug>`, `docs:<slug>`, `research:<slug>`, `test:<slug>`,
`admin:<slug>`. These identify WHICH specific feature/bug/refactor/etc. the
Outcome is about. When you can infer the work type from the focus slug, propose
the matching slug key with a value derived from the slug. All share the
`work-scope` exclusion group — propose at most one. If no existing value fits,
mint a new one (pass a `description` to `wms_tagEntity`). Not every Outcome
needs a work-scope slug — omit it when the work is too generic or cross-cutting
to have a single identity.

Present the full set conversationally with source attribution:

```
Based on your focus "fix auth bug" and the git remote, I'd tag the
strategic Outcome with:

  product:scrollz       -- from github.com/ScrollZ/ScrollZ
  bug:auth-timeout      -- from your focus slug
  priority:p1           -- no urgency signal, defaulting high

I'll also auto-apply (from git):
  github.owner:ScrollZ, github.repo:ScrollZ, git.branch:beta

Sound right?
```

### Step 6c — Feedback loop (refines tags AND goals together)

The operator confirms, corrects, or redirects. Treat each answer as refining
BOTH the tags AND the goal context — they're the same conversation.

**New values vs. new keys.** A new *value* on an existing key is
create-on-apply: pass a one-line `description` to `wms_tagEntity` so future
sessions understand it. A new *key* (a whole new dimension) is a vocabulary
change: seed it with `wms_defineTag(tagKey, category, cardinality, values,
description)` before applying.

### Step 6d — Apply the confirmed tags

Once the operator has confirmed the set, apply them yourself with
`mcp__wms__wms_tagEntity` on the strategic Outcome (source `manual`). Reuse
existing `(tag_key, tag_value)` pairs whenever one fits (case-insensitive slug
match). For a genuinely new value, pass a `description`. Then proceed to
dispatch (Step 7) carrying the context you just established.

### Per-entity tagging: Outcome vs. WorkUnit

Context tags on the Outcome are inherited down the DAG automatically — you
do NOT re-apply them per WorkUnit. The manifest's `scope` on each key tells
you where it belongs: `scope: "outcome"` keys go on the Outcome and inherit
down; `requiredLifecycle` keys (phase, work-type) must be set per-WorkUnit
at dispatch time; `required` keys must be set per-WorkUnit before close-out.
When a session mixes values from both sides of an `exclusive` group, apply
the specific value to each WorkUnit rather than picking one for the Outcome.

**Reuse existing values.** Always call `wms_listTags` before inventing new
tag values. If an existing value fits (case-insensitive), use it. New values
must be genuinely reusable across future sessions — not one-off labels.

## Step 7 — Set focus and proceed with work

Call `mcp__wms__wms_setFocus(entityType="outcome", entityID=<the Outcome id>,
focus=<short what>)` to attribute your token cost to this Outcome.

Hold this focus throughout the session. After each dispatch — when you return to
coordination, review, routing decisions, or any inter-dispatch work — call
`wms_setFocus` on the Outcome again. The lead's own messages cost real money; if
no WMS focus is open on the lead's `agent_name`, that cost lands in `unallocated`
and cannot be attributed to any work entity. Teammates hold focus on their
WorkUnits; you hold focus on the strategic Outcome.

### The two "focus" notions — do not confuse them

| | What it is | Tool | What it affects |
|---|---|---|---|
| **Activity focus** | A narration string in the live feed | `mcp__activity__reportActivity` / `setOverallIntent` | **Cosmetic only** — the feed display. Drives **no** attribution. |
| **WMS focus** | The cost-bearing focus interval on a WMS entity | `mcp__wms__wms_setFocus` | **Cost attribution** — every token you spend lands on the entity your WMS focus currently points at. |

Updating `reportActivity` does **not** move the WMS focus. They are separate
calls against separate state. As your work moves from one entity to the next
you must call `wms_setFocus` **again**.

### Dispatch protocol — every time you dispatch work

As the lead, you own the full dispatch cycle. For each piece of work:

**1. Create the WMS work unit and apply its required tags BEFORE dispatching.**
Call `mcp__wms__wms_createWorkUnit` with a clear title, linked to the
appropriate outcome (`outcomeID`). Then **immediately** apply every key from
`requiredLifecycle` in the manifest: at minimum `work-type`
(feature|bug|refactor|infra|research|docs|test) and `phase=design` (or whatever
phase the work starts in). Use the values from `requiredLifecycle` in the
manifest — no drill-down needed.

`work-type` is a **required** tag: a work unit must carry it before it goes out.
The lead knows the work type at dispatch time — the agent's brief should never
say "figure out what kind of work this is." Applying required tags up front (not
at close-out) is what keeps the cost-by-work-type dashboards honest from the
first token.

**Work-scope slug on the Outcome.** If the strategic Outcome does not yet carry
a work-scope slug tag (`feature:<slug>`, `bug:<slug>`, `refactor:<slug>`, etc.),
and the work type is clear, apply one now with `wms_tagEntity` on the Outcome.
The slug key must match the `work-type` you're setting on the WorkUnit — if the
WorkUnit is `work-type:bug`, the Outcome should have `bug:<slug>`. This is the
"which specific bug/feature/etc." tag that the context-tag interview may have
already set; if it did, this step is a no-op. If the Outcome already has a
different work-scope slug (e.g., `feature:auto-tag`), do NOT override it — the
Outcome's scope was set at interview time.

**2. Route by affinity.**
Check your idle teammates: which agent already has context on the files this
work touches? Send it to them. If no existing agent has affinity, spawn a new
domain-named agent:
- Name for the component/domain, not the role (Rule II)
- Match the model to the cognitive load (Rule IV)
- Give the agent a descriptive `name` for addressability (Rule II)
- Use `subagent_type: general-purpose`

Teammates spawning their own Agent-tool subagents for bounded sub-tasks
should use distinct `subagent_type` values when possible — the fleet view
nests sub-subagents under their spawning teammate by type. Note: CC
currently blocks `name` from teammate Agent tool calls; hookd auto-numbers
same-type siblings to ensure unique identities.

**3. Write the technical brief and send it directly.**
Send via `SendMessage` to the identified (or newly spawned) agent. Include:
- File paths and expected outcome
- What "done" looks like
- The WMS work unit ID
- This instruction: "your FIRST action is `wms_setFocus(entityType=workunit,
  entityID=<id>, focus=<short what>)` — this attributes your token cost to
  this work unit."
- Who else is working in parallel and which files they touch (shared-worktree
  rule)

**4. Advance WMS state.**
Set the work unit to `active` once dispatched. As it moves through the
execution loop, update the `phase` tag (build -> test -> review; `rework` on a
send-back).

**5. When agents complete work.**
Confirm focus was set, update WMS status to `done`, and decide what's next.
Re-call `wms_setFocus` on the strategic Outcome before the next dispatch so your
coordination cost for this turn stays attributed. Agents communicate with each
other directly via SendMessage for collaboration — you observe, you do not relay
(Rule VII).

### Tag manifest (reference)

The role-shaped manifest from `wms_listTags` (no args) is the authoritative
source. The grouping IS the instruction:

- **`propose`** — keys to offer the operator (confirmed at bootstrap).
  `exclusive` marks mutual-exclusion groups (e.g., `feature` + `bug` share
  `work-scope`).
- **`autoExtract`** — key → source map. Extract silently from git/env.
- **`requiredLifecycle`** — must be applied per-WorkUnit at dispatch time
  (Step 7.1). Values are included — use them directly (e.g. `phase`:
  design/build/test/review/rework; `work-type`: feature/bug/refactor/…).
- **`required`** — non-lifecycle required keys, set per-WorkUnit before
  close-out.
- **`engineManaged`** — do not touch.

**Tag inheritance.** Context tags on the strategic Outcome are inherited
automatically by the WMS engine at read time — you do NOT need to re-apply
them to each WorkUnit. `requiredLifecycle`, `required`, and `engineManaged`
keys are per-entity and must be applied explicitly to each WorkUnit.

## Step 8 — Close-out ritual (end of session)

The session is not done when the code works — it's done when the WMS state
reflects reality. At the end of the session, run all of:

1. **Every WorkUnit is `done`** — walk the WorkUnits under the Outcome
   (`mcp__wms__wms_listWorkUnits`) and close any still `active`/`pending`
   whose work is finished.
2. **Mark the Outcome `done`** — `mcp__wms__wms_updateOutcomeStatus`.
3. **Apply `resolution:achieved`** — `mcp__wms__wms_tagEntity(entityType="outcome",
   entityID=<outcome>, tagKey="resolution", tagValue="achieved")` (or
   `abandoned` if the work was dropped).

An Outcome left `active` with a `pending` WorkUnit and no `resolution` tag is
the unmistakable signature of a session that forgot to close out. The WMS engine
emits advisory warnings when it detects these misses — but the warning is a
safety net, not the plan: run this ritual yourself so the engine has nothing
to flag.

If no specific work was requested, tell the user the team is ready
(mention the team name and focus) and ask what they'd like to work on.

## Reference

- [eight-rules.md](references/eight-rules.md) — the protocol (what to do)
- [field-guide.md](references/field-guide.md) — practical lessons (what goes wrong)
- [execution-loop.md](references/execution-loop.md) — the 4-phase loop (implement, validate, review, commit)
