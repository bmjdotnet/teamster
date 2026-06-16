---
name: review
description: Assess readiness of current work. Checks git status, WMS task completion, build/test results, and identifies blockers. Use before presenting work to the human.
disable-model-invocation: true
---

# Readiness Assessment

Check what's done, what's left, and whether the work is ready to present.

## Step 1 — Git status

Run `git status` and `git log --oneline -10` to see recent commits and
uncommitted changes. Note the current branch.

## Step 2 — WMS task inventory

Follow the same project/goal/task traversal as /teamster:status. Group tasks
by status:
- Complete: what's done
- Active/Review: what's in progress
- Blocked: what's waiting
- Pending: what hasn't started

## Step 3 — Build verification

Run the project's build and test commands:
```
go build ./...
go test ./...
go vet ./...
```
(Or the project's equivalent.) Report pass/fail for each.

## Step 4 — Adversarial review (if applicable)

For any task that completed IMPLEMENT phase, apply the execution loop before
marking work ready. See `bootstrap/references/execution-loop.md` for the full
4-phase loop (IMPLEMENT → VALIDATE → ADVERSARIAL REVIEW → COMMIT) and the
rules on agent assignment and iteration limits.

**When to run full review:** multi-file changes, changes to shared interfaces
(MCP tools, JSONL fields, hook contracts), anything touching the installer or
production infrastructure.

**When to skip to VALIDATE only:** single-file changes with no interface impact,
documentation-only changes, test additions.

Apply the rubric checklists from `bootstrap/references/rubrics.md` during
ADVERSARIAL REVIEW. All five rubrics are relevant to code changes:
- Code Quality — naming, dead code, error handling
- Architecture — single responsibility, no circular deps, interface boundaries
- Security — no injection, no hardcoded secrets, input validation at boundaries
- Testing — new behavior covered, existing tests green, edge cases present
- Planning — diff matches task, no scope creep, no partial implementations

A review that doesn't reach a finding on at least a few items wasn't thorough.

## Step 5 — Agent status

> **Solo session.** Skip this step in a solo session (`TEAMSTER_SOLO=1`) — there
> are no teammates to enumerate. The git, WMS, and build/test steps above still
> apply; the verification gate is a fresh-context review subagent or
> `/code-review`, not a peer (see `/teamster:solo`).

Check the team config for active teammates. For each:
- What was their last task?
- Are they idle (available for rework) or active?

## Step 6 — Readiness verdict

Present one of:
- **Ready**: all tasks complete, build passes, tests green
- **Blocked**: list specific blockers with what's needed to unblock
- **In progress**: list what's left with estimated scope

If blocked or in progress, suggest next actions.
