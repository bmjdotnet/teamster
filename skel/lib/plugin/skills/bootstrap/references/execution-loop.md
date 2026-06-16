# The Execution Loop

The Eight Rules tell you how to organize agents. This document tells you how
to execute work once the team is running. Without execution discipline, the
implementing agent self-certifies its own output — the #1 quality failure mode.

Apply this loop to any non-trivial task: multi-file changes, anything touching
shared interfaces, complex logic, or production infrastructure. Single-file
cosmetic changes may skip to VALIDATE.

---

## The 4-Phase Loop

```
IMPLEMENT → VALIDATE → ADVERSARIAL REVIEW → COMMIT
```

Each phase has a different agent. Fresh context is structural, not optional.

**The loop maps to the `phase` tag.** As the task moves through the loop, the lead
updates its `phase` tag via `wms_tagEntity` so spend is faceable by phase:
IMPLEMENT → `phase=build`, VALIDATE → `phase=test`, ADVERSARIAL REVIEW →
`phase=review` (and `phase=design` before IMPLEMENT for design-heavy tasks). A
send-back from review or validate is `phase=rework`.

---

## Phase I: IMPLEMENT

**Who:** The domain agent with file affinity (see Rule III). Not the lead.

**What:** Build the change. Run the build. Fix compilation errors. This phase is
complete when the code compiles and the implementer believes it is correct.

**First action — set focus.** Before touching code, the implementer calls
`wms_setFocus(entityType=task, entityID=<its task id>, focus=<short what>)`.
This is what attributes the agent's token cost to the task; without it the cost
can only land in the "unallocated" bucket.

**Done when:**
- `go build ./...` passes (or project-equivalent)
- Implementer has read back the diff and caught obvious mistakes
- Implementer sends "ready for validation" to the validator via SendMessage

**The implementer does NOT run the full test suite as a substitute for VALIDATE.**
Their context is contaminated by the implementation. Validation requires fresh eyes.

---

## Phase II: VALIDATE

**Who:** A different agent from the implementer. Same model tier is acceptable.
Never the same agent that wrote the code.

**What:** Exercise the change. Run tests. Check that the stated behavior is
actually what the code produces. This is mechanical verification, not judgment.

**Done when:**
- `go test ./...` passes
- `go vet ./...` clean
- Behavioral smoke test confirms the change does what the task says
- Validator sends "validation passed" or reports specific failures

**Failure:** Validator sends the failure details directly to the implementer
(see Rule VII — no lead relay). Implementer fixes and re-declares "ready for
validation." This counts as one iteration.

---

## Phase III: ADVERSARIAL REVIEW

**Who:** A reviewer that is neither the implementer nor the validator. Use a
higher-tier model when the change touches production infrastructure, shared
interfaces, or architectural boundaries — opus reviews sonnet's work.

**What:** Challenge the implementation. The reviewer's job is to find what the
implementer and validator both missed: correctness gaps, security issues,
scope violations, unintended side effects.

**The reviewer reads the diff cold.** They do not ask the implementer for
explanations. If the diff requires an explanation to pass review, it needs
to be rewritten.

**Done when:**
- Reviewer approves (sends "review passed" to the lead)
- OR reviewer sends specific findings to the implementer, triggering a new
  IMPLEMENT cycle

**Reviewer is not a rubber stamp.** "Looks fine" without applying the rubrics
(see `rubrics.md`) is a failed review.

---

## Phase IV: COMMIT

**Who:** The lead, after receiving reviewer approval.

**What:** Persist the result. Create the commit. Mark the task complete in WMS.

**Done when:**
- `git commit` created
- WMS task status updated
- Lead reports completion to the human

**The lead does NOT commit before reviewer approval.** A commit before COMMIT
phase means the loop was short-circuited.

---

## Agent Assignment Rules

| Phase | Agent | Constraint |
|-------|-------|------------|
| IMPLEMENT | Domain agent (file affinity) | Must not be the lead |
| VALIDATE | Any agent | Must not be the implementer |
| ADVERSARIAL REVIEW | Any agent | Must not be the implementer; prefer higher-tier for risky changes |
| COMMIT | Lead | After reviewer approval only |

Never assign two phases to the same agent when one of them is IMPLEMENT.
The implementer may assist with COMMIT logistics (naming, WMS IDs) but does
not self-review.

---

## Iteration Limits

**Max 3 IMPLEMENT → VALIDATE → ADVERSARIAL REVIEW cycles before human escalation.**

After 3 failed cycles:
1. Stop iterating
2. Lead reports to the human: what was attempted, what review found, why it isn't converging
3. Human decides: descope, reassign, or unblock a blocker

Continuing past 3 iterations without escalation wastes tokens and hides a
real problem.

**Counting a cycle:** One cycle = implementer declares ready → validator
runs → reviewer sends it back. A passing review ends the loop regardless
of which iteration it is.

---

## Communication Pattern

```
IMPLEMENT:   @domain-agent → @validator  ("ready for validation")
VALIDATE:    @validator    → @domain-agent (failure details) OR
             @validator    → @reviewer  ("validation passed, ready for review")
REVIEW:      @reviewer     → @domain-agent (findings) OR
             @reviewer     → lead  ("review passed")
COMMIT:      lead          → human  (task complete)
```

The lead does not relay. It monitors. See Rule VII.

---

## When to Apply the Full Loop

**Full loop required:**
- Multi-file changes
- Changes to shared interfaces (MCP tools, hook contracts, JSONL fields)
- Anything touching the installer, systemd units, or settings.json
- Security-relevant code (auth, secrets, input validation)
- Database schema changes

**VALIDATE only (skip ADVERSARIAL REVIEW):**
- Single-file changes with no interface impact
- Documentation-only changes
- Test additions with no production code change

**Skip loop entirely:**
- Cosmetic formatting fixes
- Comment-only changes
- README/NOTES updates

---

## Violations

| Violation | Consequence |
|-----------|-------------|
| Implementer self-validates | Fresh context lost; bugs survive to commit |
| Implementer self-reviews | Adversarial review is theater; issues get missed |
| Validator gives "looks good" without running tests | Not validation — it's optimism |
| Reviewer approves without applying rubrics | Review gate provides no signal |
| Lead commits before reviewer approval | Loop short-circuited; quality guarantee voided |
| Continuing past 3 iterations without escalation | Real blockers hidden; tokens wasted |
| Lead relaying between phases | Rule VII violation; adds latency, obscures failures |
| Skipping loop for "simple" multi-file changes | Simple is subjective; the loop is cheap |
