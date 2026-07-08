---
name: teamster-review
description: Assess readiness of current work. Checks git status, WMS task completion, build/test results, and identifies blockers. Use before presenting work to the operator. Explicit invocation only — mention $teamster-review.
---

# Readiness Assessment

Check what's done, what's left, and whether the work is ready to present.
(Codex sessions have no Agent Teams layer — there is no team roster to check;
this covers git, WMS, build/test, and an adversarial-review pass only.)

## Step 1 — Git status

Run `git status` and `git log --oneline -10` to see recent commits and
uncommitted changes. Note the current branch.

## Step 2 — WMS task inventory

Run the same outcome/work-unit traversal as `$teamster-status`: list open
root outcomes, their child outcomes, and their work units. Group work units
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

For any work unit that completed its implementation phase, review the diff
before marking it ready — a review that doesn't reach a finding on at least a
few items wasn't thorough. Check each of:

- **Code quality** — naming, dead code, error handling.
- **Architecture** — single responsibility, no circular dependencies,
  interface boundaries respected.
- **Security** — no injection, no hardcoded secrets, input validated at
  boundaries.
- **Testing** — new behavior covered, existing tests green, edge cases
  present.
- **Planning** — the diff matches the task, no scope creep, no partial
  implementations.

**When to run the full pass:** multi-file changes, changes to shared
interfaces (MCP tools, JSONL fields, hook contracts), anything touching the
installer or production infrastructure.

**When to skip to build/test only:** single-file changes with no interface
impact, documentation-only changes, test additions.

Prefer a fresh-context subagent for this pass when the change is non-trivial
— self-certifying your own diff is weaker than an independent read.

## Step 5 — Readiness verdict

Present one of:
- **Ready**: all tasks complete, build passes, tests green
- **Blocked**: list specific blockers with what's needed to unblock
- **In progress**: list what's left with estimated scope

If blocked or in progress, suggest next actions.
