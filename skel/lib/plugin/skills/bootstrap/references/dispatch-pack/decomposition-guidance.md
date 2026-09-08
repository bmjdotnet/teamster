# Dispatch Decomposition Guidance

How to split work across parallel agents. Read this before writing task
briefs for any multi-agent batch — the decision you make here determines
whether the batch runs clean or collides.

---

## The rule: split by file independence, not domain grouping

When breaking a task into parallel dispatches, the axis that matters is
**which files each piece touches** — not which domain or subsystem the work
conceptually belongs to.

```
WRONG:  @frontend-agent (all UI work)   @backend-agent (all API work)
        — both end up editing shared/types.go because the domain split
        didn't account for the shared file

RIGHT:  @store  (store.go, store_test.go)
        @engine (engine.go, engine_test.go)
        @display (display.go)
        — each agent owns a disjoint file set; no coordination needed
        mid-flight
```

Domain grouping *feels* natural — "the frontend person does frontend" — but
domains overlap in files more often than they don't: shared types, shared
config, shared migrations. File-independence grouping is mechanical and
verifiable before you dispatch: list the files each piece of work will
touch, confirm the lists are disjoint, then dispatch.

**Field evidence:** the taxonomy team's lead fell into dispatching by domain
grouping instead of file independence, and hit exactly the collision this
rule exists to prevent (R-P5 field evidence, taxonomy devreport). This
guidance is wired into the dispatch pack — not just startup — specifically
because habituation on this point is fast; leads revert to domain grouping
within a few dispatches unless the check is in front of them at dispatch
time.

## The check, before you dispatch

1. Write down the file set each piece of work will touch.
2. Diff the sets. Any overlap means those two pieces are not independent —
   either merge them into one dispatch, sequence them, or split the
   overlapping file out as a third, serialized piece.
3. Only dispatch in parallel what survives step 2 with zero overlap.

If two pieces of work have no shared files **and** no data dependency
(neither reads output the other produces), they are safe to run in
parallel with no further coordination. If either condition fails, they are
not independent — sequence them or explicitly coordinate (see below).

## When independence isn't fully achievable

Some batches can't be cleanly disjoint — e.g. one agent adds a field to a
shared type that two other agents' code will consume. In that case:

- Land the shared-file change first, serially, before dispatching the
  agents that depend on it.
- Or: name the affected agents explicitly to each other and require
  SendMessage coordination before either touches the shared file (see
  `eight-rules.md`'s "Shared-worktree coordination" section for the brief
  wording this needs).

Silence on this point is what causes collisions — an agent that doesn't
know a peer is touching the same file has no reason to check before
editing.
