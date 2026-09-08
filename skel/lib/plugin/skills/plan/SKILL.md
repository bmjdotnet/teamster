---
name: plan
description: Decompose work into WMS entities (outcomes, work units) and propose team composition with domain-named agents. Use when starting a new initiative or breaking down a complex task.
disable-model-invocation: true
argument-hint: "<description of work>"
---

# Decompose and Plan Work

Analyze a description of work, decompose it into WMS entities, propose team
composition, and create the entities after human approval.

## Step 1 — Understand the work

Read `$ARGUMENTS`. If no description was provided, ask for one before proceeding.

## Step 2 — Decompose into WMS entities

Apply the Outcome → WorkUnit hierarchy:

- **Outcome (strategic)**: the overall initiative (one per planning session)
- **Outcome (tactical)**: 2-5 measurable results nested under the strategic outcome
- **WorkUnit**: concrete, bounded work assigned to an agent under each tactical outcome

Present the decomposition as a tree for human review. Include suggested IDs.

## Step 3 — Propose team composition

Analyze which files and components the work units touch. Propose domain-named agents:

```
Suggested team:
  @{component-a} (sonnet) — handles {files/packages}
  @{component-b} (sonnet) — handles {files/packages}
  @{component-c} (haiku) — read-only exploration of {area}
```

Follow Rule II (domain names) and Rule IV (model tiering). Explain the
affinity reasoning for each agent.

## Step 4 — Get approval

Present the full plan: WMS entities + team composition + work unit assignments.
Wait for human approval before creating anything.

## Step 5 — Create WMS entities

After approval:
1. `mcp__wms__wms_createOutcome` for the strategic outcome (a root outcome
   with no parent is strategic by DAG position — no scope tag needed)
2. `mcp__wms__wms_createOutcome` for each tactical outcome (pass `parentOutcomeIDs`
   — a child outcome is tactical by DAG position)
3. `mcp__wms__wms_createWorkUnit` for each work unit (pass `outcomeID`)
4. `mcp__wms__wms_addDependency` for any work unit ordering constraints

## Step 6 — Spawn agents and dispatch

> **Solo session.** This whole step is team-only. In a solo session
> (`TEAMSTER_SOLO=1`) there is no one to spawn or assign to — `wms_assignWorkUnit`
> and the `agent_id` it sets are inert with a single agent. Skip Step 6: keep the
> WMS entities from Step 5, claim the work unit you're starting
> (`wms_claimWorkUnit`, falling back to `wms_setFocus` for non-WU-scoped work),
> and do the work yourself, spawning an ephemeral subagent only for a bounded
> sub-task like a fresh-context review. See `/teamster:solo`.

1. Spawn each approved agent via the Agent tool with a descriptive `name`
2. Store the full assignment as the WorkUnit's `brief` (pass it to
   `wms_createWorkUnit` in Step 5) rather than only in the SendMessage that
   follows — `wms_claimWorkUnit` hands this brief back to the agent when it
   claims the unit.
3. Send a short pointer via SendMessage: the WorkUnit id, and the
   instruction "your FIRST action is `wms_claimWorkUnit('<id>')`" — this
   returns the brief and transitions the unit to `active` atomically, and
   requests a focus interval alongside it (hookd opens it asynchronously,
   best-effort; it may decline). Point the agent at
   `../bootstrap/references/implementation-pack/teammate-guide.md` for the
   rest of the protocol that applies to it.
4. Tell each agent who else is working in parallel and which files they own
   (shared-worktree rule)

See `bootstrap/SKILL.md`'s "Dispatch protocol" for the full claim/deliver
cycle, including how the agent reports back (`mcp__wms__wms_deliverResult`).
