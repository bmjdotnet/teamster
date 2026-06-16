---
name: bootstrap
description: Stand up a Teamster team for this session. Creates the team, loads WMS tools, creates the strategic Outcome, runs the context-tag interview, and teaches the lead to dispatch work — all inline, no intermediary agent.
disable-model-invocation: true
argument-hint: "[focus slug — what this team is working on]"
---

# Bootstrap a Teamster Team

Run before any work that requires parallel agents. This creates the team and
sets up WMS tracking. The lead owns everything: operator communication,
strategic decisions, WMS state, dispatch routing, tagging, and agent briefs.

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

**Set the session mode (first action).** Team mode is the default, but set the
marker explicitly so a session that previously chose solo is returned to
enforcement. Load and call the mode signal:

```
ToolSearch("select:mcp__activity__setMode")
mcp__activity__setMode(mode="team")
```

`setMode` is a no-op confirmation tool; the Teamster hook does the real work
(here, clearing any solo marker so the team-dispatch mandate and the bare-`Agent`
block are enforced). Call it **once** on entry — not every turn. If you arrived
via `/teamster:start` it already set team mode — calling again is harmless
(idempotent).

## Step 2 — Generate a team name and create the team

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

Call `TeamCreate` with the generated name.

If `TeamCreate` fails because the name already exists, generate a
**variation** (different portmanteau, add a memorable suffix like
`-redux`, `-mk2`, an animal, a color) and retry. Do **not** fall back to
`teamster` or any other generic name. Repeat until create succeeds.

If a team is already attached to this session, skip to Step 3 and use that
team — don't create a second one.

## Step 3 — Read the protocol

Before loading tools or creating anything, read these two files now (use the
Read tool) — they define how you operate as team lead for the rest of this
session:

- ${CLAUDE_SKILL_DIR}/references/eight-rules.md
- ${CLAUDE_SKILL_DIR}/references/field-guide.md

## Step 4 — Load WMS tools

You own all WMS state directly. Load the tools in one batch (these are deferred
tools — load before first use):

```
ToolSearch("select:mcp__wms__wms_createOutcome,mcp__wms__wms_createWorkUnit,mcp__wms__wms_updateOutcomeStatus,mcp__wms__wms_updateWorkUnitStatus,mcp__wms__wms_assignWorkUnit,mcp__wms__wms_listOutcomes,mcp__wms__wms_listWorkUnits,mcp__wms__wms_setFocus,mcp__wms__wms_tagEntity,mcp__wms__wms_listTags,mcp__wms__wms_defineTag,mcp__wms__wms_retireTag")
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

## Step 5 — Create the strategic Outcome

Call `mcp__wms__wms_createOutcome` with an id based on the focus slug. A root
Outcome (no parent) IS the strategic one — its altitude comes from DAG position,
so there is no altitude tag to apply. Set status to `active`
(`mcp__wms__wms_updateOutcomeStatus`).

## Step 6 — The context-tag interview (set the strategic Outcome's tags + goals)

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

### Step 6a — Inspect the tag vocabulary

Call `mcp__wms__wms_listTags`. Each entry is `{tag_key, tag_value, category,
cardinality, description, is_seed}`:

- **`category=context`** tags (`product`, `feature`/`bug`, `component`,
  `priority`, `product-version`, plus any integration keys like `github.*`)
  are the durable ones you propose now. They characterize the work and are
  inherited down the DAG. The operator may name additional context keys —
  seed those with `wms_defineTag`.
- **`category=lifecycle`** tags (`work-type`, `phase`, `resolution`) track
  execution. **Do NOT propose these at bootstrap** — the engine and classifier
  apply them as work progresses.
- Read each `description` to know an existing value's meaning and avoid
  proposing a near-duplicate; note `cardinality` (a `single` key replaces on
  re-tag, a `multi` key accumulates).

### Step 6b — Extract and propose context tags

Before asking the operator to type anything, extract what the environment
already knows. Then propose inferred + extracted values together, with source
attribution so the operator can verify at a glance.

#### Proactive extraction (best-effort — skip any step that fails)

**Git/GitHub metadata.** Run `git remote -v` and `git branch --show-current`:
- Parse the remote URL to extract integration keys:
  - `github.com/Org/Repo.git` -> `github.owner:Org`, `github.repo:Repo`
  - `gitlab.com/Group/Project.git` -> `gitlab.group:Group`, `gitlab.project:Project`
  - Other remotes -> `git.remote:<url>`
- Set `git.branch` from the current branch.
- If no remote exists (local-only repo), set `git.repo` to the directory name.

**Project metadata.** Read the project's CLAUDE.md (if it exists) for declared
product name, version, component names, issue tracker references, or any other
tag-like metadata the project author documented.

**MCP tool availability.** Check which MCP tool families are available (don't
call them — just note their presence):
- Jira MCP tools (`mcp__jira__*`) -> note that `jira.id` / `jira.project` can
  be populated from active issues during work.
- GitHub MCP tools -> note that `github.issue` / `github.pr` can be populated.
- Mention availability to the operator so they know the values can be added
  when relevant context exists (a ticket ID, a PR number).

#### Inference from context

From the focus slug (Step 1) + the extraction above + the conversation so far,
propose the context tags that fit:

- **`product`** — infer from the repo name, CLAUDE.md, or working dir. Check
  `wms_listTags` for existing values first — reuse, don't duplicate.
- **`feature`** or **`bug`** — infer from the focus slug. Mutually exclusive.
- **`component`** — a lead judgment call, applied **per WorkUnit** at dispatch
  time, not on the strategic Outcome. Each WorkUnit targets a specific
  subsystem; the Outcome usually spans several. Reuse existing values from
  `wms_listTags` where they fit; propose new values only when genuinely new
  and reusable ("cli", "harness", "wms", "dashboard" are good; one-off task
  descriptions are not).
- **`priority`** — match to any urgency signal or propose a default.
- **`product-version`** — only if a release target exists.
- **integration keys** — propose any values you extracted above.
- **operator-named keys** — if the operator wants to track another dimension,
  seed it with `wms_defineTag` before applying.

#### Propose with source attribution

Present the full set conversationally, showing WHERE each value came from:

```
Based on your focus "fix auth bug" and the git remote, I'd tag the
strategic Outcome with:

  product:scrollz       -- from github.com/ScrollZ/ScrollZ
  github.owner:ScrollZ  -- from git remote
  github.repo:ScrollZ   -- from git remote
  git.branch:beta       -- current branch
  bug:auth-timeout      -- from your focus slug
  priority:p1           -- no urgency signal, defaulting high

I also see Jira MCP tools available -- if you have a ticket ID, I can
add jira.id and jira.project.

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
do NOT re-apply them per WorkUnit. But some context tags should vary across
WorkUnits rather than being forced onto the Outcome:

- **`component`** is always per-WorkUnit. Each piece of work targets specific
  code; the Outcome spans them all. Apply `component` to each WorkUnit at
  dispatch time, not to the Outcome.
- **`feature`/`bug`** — when the session mixes both (e.g. a feature that
  also fixes a bug), apply the specific value to each WorkUnit rather than
  picking one for the Outcome.
- **Tags that are truly shared** (`product`, `priority`, `product-version`,
  integration keys) belong on the Outcome and inherit normally.

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
appropriate outcome (`outcomeID`). Then **immediately** apply the required tags
with `mcp__wms__wms_tagEntity` — at minimum `work-type`
(feature|bug|refactor|infra|research|docs|test) plus `phase=design` (or whatever
phase the work starts in).

`work-type` is a **required** tag: a work unit must carry it before it goes out.
If you haven't already this session, call `mcp__wms__wms_listTags` to see which
keys are marked `required` and set every one of them now. The lead knows the
work type at dispatch time — the agent's brief should never say "figure out what
kind of work this is." Applying required tags up front (not at close-out) is also
what keeps the cost-by-work-type dashboards honest from the first token.

**2. Route by affinity.**
Check your idle teammates: which agent already has context on the files this
work touches? Send it to them. If no existing agent has affinity, spawn a new
domain-named agent:
- Name for the component/domain, not the role (Rule II)
- Match the model to the cognitive load (Rule IV)
- Use `team_name` from your team (Rule I)
- Use `subagent_type: general-purpose`

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

### Seeded classifiers (reference)

```
context tags (durable, inherited, confirmed at bootstrap):
  product:          <operator-defined>   (single)
  feature:          <slug>               (single)  -- mutually exclusive with bug
  bug:              <slug>               (single)  -- mutually exclusive with feature
  component:        <slug>               (single)
  priority:         p0 | p1 | p2 | p3   (single)
  product-version:  <version>            (single)

integration tags (context, seeded by setup -- propose if relevant):
  github.owner, github.repo, github.pr, github.issue, github.milestone
  git.repo, git.branch, git.remote
  (also: jira.*, gitlab.*, redmine.*, openproject.*, plane.*, taiga.*)

lifecycle tags (per-WorkUnit, NOT set on the Outcome at bootstrap — applied
  to each WorkUnit at dispatch time, see Step 7.1):
  work-type:  feature | bug | refactor | infra | research | docs | test  (multi)
  phase:      design | build | test | review | rework
  resolution: achieved | abandoned
  lifecycle:  archived
```

**Tag inheritance.** Context tags on the strategic Outcome are inherited
automatically by the WMS engine at read time — you do NOT need to re-apply
them to each WorkUnit. Lifecycle tags (`work-type`, `phase`) are per-entity
and must be applied explicitly to each WorkUnit.

Call `wms_listTags` before tagging to see each value's description and avoid
near-duplicates.

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
