---
name: start
description: The Teamster front door. Gathers the session objective, recommends an operating model (team vs subagent), and on your confirmation dispatches to the right setup — team mode or /teamster:solo for a single-agent session. Use this at the start of any session.
disable-model-invocation: true
argument-hint: "[focus slug — what this session is working on]"
---

# Start a Teamster Session

This is the recommending front door. It runs the shared focus interview **once**,
reads the objective, **recommends** an operating model with a one-line rationale,
and — once the operator confirms — dispatches to the committed path:

- **Team** → the team-mode flow (TeamCreate, persistent named teammates, WMS
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

## Step 1 — Gather the objective + context (once)

Establish what this session is for. This is the same focus interview both
`bootstrap` and `solo` open with — run it here ONCE and hand the result down so
the dispatched skill does not re-ask.

**Focus slug.**
- If `$ARGUMENTS` is non-empty, use it as the focus slug.
- Otherwise ask with AskUserQuestion:
  - question: "What is this session focused on? (a short phrase)"
  - header: "Session focus"
  - options: one per plausible focus you can infer from the conversation, recent
    files, and the working directory (2–3 max). The operator can pick "Other".

**Context (best-effort, skip any step that fails).** Gather what the dispatched
skill would otherwise gather, so it inherits it:
- `git remote -v` and `git branch --show-current` (integration keys + branch).
- The project's CLAUDE.md (declared product, version, components, trackers).
- The conversation so far and recently-touched files.

Keep this context — you pass it to the dispatched skill in Step 4 so it skips its
own redundant gather. One interview, not two.

## Step 2 — Form a mode recommendation

Read the objective and the gathered context and **reason** toward team or
subagent. This is judgment in prose — there is no score, weight, or threshold.
Match the objective's shape to the signals below.

**Signals that lean TEAM** (breadth / parallelism / coordination value):
- the objective spans **multiple subsystems** or files that can progress
  independently ("rework the dashboard *and* the migration *and* the hook");
- it is **multi-phase with handoffs** where fresh-context separation pays
  (implement → adversarial review → integrate across components);
- it is **broad / parallelizable** — decomposes into 2–5 pieces with little
  shared mutable state;
- it explicitly wants **adversarial review by a different agent** as a hard gate.

**Signals that lean SUBAGENT** (focus / tight coupling / exploration):
- a **single tightly-coupled change** — one file or one cohesive unit where
  parallel agents would just contend on the same lines;
- **exploratory / investigative** work where the path isn't yet decomposable
  ("figure out why cost-by-work-type is empty");
- **short-horizon** work that wouldn't amortize the team setup (field-guide
  lesson 1: "if the work fits one agent, skip the team");
- the operator signals they want to **drive it themselves** with occasional
  fresh-context review subagents, not run a dispatch loop.

**When signals conflict or are thin, recommend TEAM** — the enforcing default
that preserves the no-bare-agents rail and review separation. Say so plainly.

## Step 3 — Present the recommendation; operator confirms or overrides

Present it as an AskUserQuestion with the recommended option **pre-marked** and a
**one-line, falsifiable rationale** tied to the stated objective (so the operator
can correct a bad read by restating the goal). Example:

```
Based on your focus "investigate why solo cost-by-work-type is empty" and that
this is a single-subsystem exploratory trace, I'd run this as a SUBAGENT session
— one primary agent, spawning a fresh review subagent for the eventual fix. (A
team would add coordination overhead with nothing to parallelize.)

  ▸ Subagent (recommended)   — one primary agent, ephemeral review subagents
    Team                     — persistent named teammates, lead owns dispatch
```

- header: "Operating model"
- options: "Team" and "Subagent", with the recommended one marked and carrying
  the rationale. Override is just picking the other option — no re-justification.

If the recommendation was team-by-default-from-ambiguity, say that in the
rationale ("not obviously parallel, but defaulting to team for the enforcement +
review separation; pick Subagent if you'd rather drive it solo").

## Step 4 — Set the mode, then dispatch

Once the operator has confirmed a mode, set the session mode marker, then hand
the gathered context to the committed skill.

**Set the mode.** Load and call the mode signal (deferred MCP tool — load once):

```
ToolSearch("select:mcp__activity__setMode")
mcp__activity__setMode(mode="solo")   # for Subagent
mcp__activity__setMode(mode="team")   # for Team
```

`setMode` is a no-op confirmation tool; the Teamster hook recognizes the call and
records the session's mode so the runtime gates behave correctly (solo relaxes
the team-dispatch mandate and the bare-`Agent` block; team keeps them enforced).
Call it **once**, before dispatching, so the marker is in place before any work —
not every turn; the hook keeps the marker fresh on its own while the session is
active.

**Dispatch by following the chosen skill INLINE — do NOT call the Skill tool.**
`/teamster:solo` and the bootstrap skill both set `disable-model-invocation`,
so a `Skill(teamster:solo)` / `Skill(teamster:bootstrap)` call **errors** — they
are user-typed slash commands, not model-invocable. To dispatch, **Read the
chosen skill's file and follow its steps yourself, inline, in this same session**,
reusing the focus slug + Step-1 context (skip the skill's own focus re-ask).
Reading the file is fine; only invoking it via the Skill tool is blocked.

- **Subagent** → Read `../solo/SKILL.md` and follow its steps inline: create the
  strategic Outcome, run the context-tag interview (reusing the context you
  gathered), set focus, and work — spawning ephemeral review subagents for the
  verification gate. (You already called `setMode("solo")` above; solo's own
  "set the session mode" step is then a harmless idempotent repeat.)
- **Team** → Read `../bootstrap/SKILL.md` and follow its steps inline: generate a
  team name from the slug, TeamCreate, load WMS tools, create the strategic
  Outcome, run the context-tag interview, dispatch. (You already called
  `setMode("team")` above.)

Carry the context forward — do **not** re-ask the focus slug, and do **not** call
the Skill tool to enter the sibling skill.

## Notes

- A session that never runs `/teamster:start` (or `/teamster:solo`) behaves
  exactly as today: the team mandate and the bare-`Agent` block are enforced.
  The interview is an aid to choosing, never a precondition for safety.
- Run this before any dispatch — it is step zero. A subagent spawned before the
  mode is set runs under the enforcing default (team), which is the safe
  direction.

## Reference

- [solo/SKILL.md](../solo/SKILL.md) — the dispatched subagent path.
- [bootstrap/SKILL.md](../bootstrap/SKILL.md) — the dispatched team path.
- [eight-rules.md](../bootstrap/references/eight-rules.md) — the protocol + the
  solo carve-out (which rules apply in each mode).
