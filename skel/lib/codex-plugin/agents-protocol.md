
## Getting Started with Teamster (Codex)

Teamster tracks this session's work in WMS (Outcome -> WorkUnit) and the live
activity feed. Codex sessions run solo -- there is no Agent Teams layer here,
so none of Claude Code's team-coordination rules apply. When you begin a
session with non-trivial work (not just a quick question), use the
`$start` skill first. It hands off to `$teamster-solo`, which creates
the WMS Outcome, runs the context-tag interview, sets focus, and hands off to
the work itself.

## Available skills

Five Teamster skills are installed. A skill listing may show either name —
both refer to the same skill:

- "Teamster Start" -- invoke by typing `$start` -- the front door for a
  session; hands off to `$teamster-solo`.
- "Teamster Solo" -- invoke by typing `$teamster-solo` -- start a WMS-tracked
  session (Outcome, context tags, focus).
- "Teamster Status" -- invoke by typing `$teamster-status` -- show current
  outcomes and work units.
- "Teamster Tag Steward" -- invoke by typing `$teamster-tags` -- refine the
  tag vocabulary conversationally.
- "Teamster Readiness Review" -- invoke by typing `$teamster-review` --
  check git/WMS/build/test before presenting work.

`$teamster-solo`, `$teamster-tags`, and `$teamster-review` are
explicit-invocation-only -- they will NOT appear if you ask "what skills do
you have" or list skills generically (`$start` and `$teamster-status`
are the only ones that do). That is by design, not a sign they are missing --
invoke them by name anyway whenever the situation calls for them.

## Finding WMS/activity MCP tools

On some Codex builds, MCP tools (`wms_*`, `reportActivity`, etc.) are
defer-loaded behind an internal tool search rather than directly callable --
the `wms` server alone has 31 tools even if only a handful show up
unprompted. If a tool you expect isn't callable:

- Search using natural-language verbs describing what you want to DO --
  "create a new outcome," "tag entities," "update work unit status" -- never
  a bare tool name or a `wms_` prefix (those searches reliably return zero
  results).
- If the first search doesn't surface what you need, search again with
  different descriptive wording before concluding the tool doesn't exist.

## Activity Reporting

The `activity` server provides three tools (search for them first if your
build defer-loads MCP tools — see "Finding WMS/activity MCP tools" above).
Use them:

1. `reportActivity(type, message)` -- call at the start of each turn before
   doing work. Types: thought, reading, writing, executing, planning, reviewing.
   Keep messages under 8 words, imperative: 'fix auth bug', 'explore disk layout'.

2. `setOverallIntent(message)` -- call on your first turn to declare your
   mission. Update when your focus shifts to something fundamentally new.

3. `completeActivity(message)` -- call when you finish a task or turn
   objective. Short phrase: 'fixed auth bug, tests pass'.

4. `wms_setFocus(entityType, entityID, focus)` -- call once when you
   start working on a WMS entity (Outcome or WorkUnit). This is the
   cost-bearing focus: every token you spend lands on the entity your
   WMS focus points at. Set it once; it stays active until you change it.
   Without it, your cost lands in `unallocated`.

This is how Teamster monitors what you're doing. Every turn. No exceptions.

## Working discipline

- Decompose work into WorkUnits, advance status as you go, tag lifecycle keys
  before starting each WorkUnit, and close out (mark done, resolution tag) at
  the end -- `$teamster-solo` documents the full ritual.
- Verify before presenting: build, test, and vet (or the project's
  equivalent) before calling anything done. Spawn a subagent for
  fresh-context review on multi-file or interface-touching changes --
  Codex's subagents are ephemeral (spawn, wait, collect), which is exactly
  what a bounded review step needs; there is no persistent-teammate concept
  to manage.
