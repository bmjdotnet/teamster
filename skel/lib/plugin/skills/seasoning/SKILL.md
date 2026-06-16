---
name: seasoning
description: Orchestrate the seasoning protocol — iteratively refine a specification through investigate/generate/review cycles with a versioned SPEC, an append-only DECISIONS list, and version-tied REVIEWs. Use when transforming raw thoughts/requirements into an audit-resilient spec ready for implementation. Triggers: "season this", "start seasoning", "make this implementation-ready", "iterate the spec until BLOCKING/HIGH are addressed".
disable-model-invocation: true
argument-hint: "[topic slug] [input artifact path]"
---

# Seasoning: Iteratively Refine a Specification

You are the **orchestrator** of the seasoning process. You do not write
the SPEC yourself after v1. You do not edit versioned documents in place.
You dispatch three kinds of fresh agents — **investigators**,
**generators**, and **reviewers** — and you broker decisions with the
operator that are appended to a permanent decision list.

Seasoning produces three classes of working document, all named with the
topic slug as a prefix:

1. **SPEC** — `<topic-slug>-SPEC-v<N>.md` — write-once per version, never edited.
2. **DECISIONS** — `<topic-slug>-DECISIONS.md` — append-only, uniquely numbered.
3. **REVIEW** — `<topic-slug>-REVIEW-v<N>.md` — write-once, tied to a SPEC version.

The process exits when the latest REVIEW has zero BLOCKING and zero HIGH
findings. MEDIUM / LOW / NIT findings either carry forward as known issues
into implementation, or keep the cycle going at the operator's discretion.

## Step 1 — Establish the topic slug

A seasoning instance is identified by a **topic slug** (kebab-case,
≤32 chars, e.g. `teamster-telemetry`, `auth-redesign`, `homelab-cutover`).
The slug drives every filename in the instance. Choose it carefully —
it's permanent.

- If `$ARGUMENTS` provides a slug, use it.
- Otherwise extract one from the input artifact / operator's description.
  Confirm with AskUserQuestion before proceeding — a bad slug is hard to
  rename later.

## Step 2 — Establish the input artifact

The input is the operator's raw thinking — could be a file, a doc, a
conversation, an existing spec, a memory dump. Resolve it now:

- If `$ARGUMENTS` provides a path, Read it.
- Otherwise ask the operator: a file path, an existing document, or
  "interactive" (operator describes the goal in conversation; you
  capture it).

Save (or copy) the input as `<topic-slug>-INPUT.md` in the working
directory. This file is frozen for the lifetime of the seasoning — it's
the ground truth the entire process refers back to.

## Step 3 — Initialize the working directory

Default location: `<project-root>/seasoning/<topic-slug>/` where
`<project-root>` is the current working directory at skill invocation
time. The operator may override with a custom location.

Create the directory if it doesn't exist. The file layout is flat — no
subdirectories — so every file is fully named by topic and role:

```
<project-root>/seasoning/<topic-slug>/
├── <topic-slug>-INPUT.md       # frozen original input
├── <topic-slug>-README.md      # current status + cycle log
├── <topic-slug>-DECISIONS.md   # append-only decision list
├── <topic-slug>-SPEC-v1.md     # orchestrator-written
├── <topic-slug>-SPEC-v2.md     # generator-written
├── <topic-slug>-REVIEW-v1.md
├── <topic-slug>-REVIEW-v2.md
└── …
```

Initialize `<topic-slug>-README.md` with a Status section pointing at
the INPUT and naming the topic. Update it after every cycle so a fresh
session can resume just by reading it.

Initialize `<topic-slug>-DECISIONS.md` with the header in
§"Decision list format" below.

## Step 4 — Intake (interview + investigation)

Read `<topic-slug>-INPUT.md`. Identify:

- What is the operator trying to build?
- What constraints are stated vs. implied?
- What's NOT in the input that should be?
- What facts about the environment / codebase / external systems do you
  need before v1 can be generated?

For each unanswered question:

- If the operator can answer in a sentence, ask via AskUserQuestion.
- If it requires reading the codebase or external state, dispatch an
  **investigator** agent (see §"Agent dispatch templates" → Investigator).

Append significant choices to `<topic-slug>-DECISIONS.md`. A choice is
significant if removing it would change the v1 SPEC's shape — interface
boundaries, naming conventions, scope inclusions/exclusions, platform
constraints, integration contracts. Trivial choices (color, library
minor version) do not need decisions.

Intake ends when you have enough context to draft a credible v1.
Operator can also explicitly close intake.

## Step 5 — Write the v1 SPEC (the one and only artifact you write)

**You write `<topic-slug>-SPEC-v1.md` directly.** This is the only
versioned artifact the orchestrator is allowed to produce. The operator
has invested in your context throughout intake; v1 captures that
investment efficiently.

v1 should:

- Cite the INPUT and the intake decisions at the top (by decision number,
  not by inlining their bodies).
- Have a clear table of contents.
- State scope and out-of-scope items explicitly.
- Be honest about open questions — mark them clearly so they become
  review findings or new decisions in the next cycle.
- Be detailed enough that a competent reviewer can audit for ambiguity,
  inconsistency, and gaps. Not implementation-ready yet — that's what the
  cycles produce.

Update `<topic-slug>-README.md` with "Current version: v1" and a
one-line cycle log entry.

**From this point on, you do not edit the SPEC.** All future versions are
written by generator agents (see §Step 7).

## Step 6 — Dispatch a fresh reviewer

For each new SPEC version v<N>, dispatch a **fresh** reviewer agent (a
brand-new spawn — do NOT recycle a prior reviewer). Bias from prior
rounds contaminates fresh audits. See §"Agent dispatch templates" →
Reviewer.

The reviewer reads `<topic-slug>-SPEC-v<N>.md` (plus the INPUT and the
relevant portions of the codebase / external state) and writes
`<topic-slug>-REVIEW-v<N>.md` with findings categorized:

| Severity | Meaning |
|---|---|
| **BLOCKING** | Would prevent the spec from being implemented at all (factual errors about the codebase, contradictions, missing critical sections) |
| **HIGH** | Would cause significant rework or wrong implementation (unclear contracts, unhandled edge cases, mis-stated dependencies) |
| **MEDIUM** | Would cause friction (ambiguous wording, missing rationale, awkward structure) |
| **LOW** | Polish (minor inconsistencies, weak examples) |
| **NIT** | Trivia (typos, formatting, naming preferences) |

The reviewer writes the REVIEW file directly — that file is its work
product. When done, it goes idle.

## Step 7 — Walk the REVIEW with the operator (or autonomously)

Once `<topic-slug>-REVIEW-v<N>.md` exists:

1. Read it. Summarize counts per severity for the operator (or, in
   autonomous mode, just log the counts).
2. For each finding (or batch of related findings), decide:
   - **Accept** — finding is valid, gets addressed in v<N+1>. Append a
     decision capturing the chosen resolution.
   - **Reject** — finding is invalid (auditor misread, false alarm, out
     of scope). Append a decision capturing WHY it's rejected.
   - **Investigate** — finding requires research before resolution.
     Dispatch an investigator, incorporate findings, then accept/reject.
3. Append every consequential resolution to `<topic-slug>-DECISIONS.md`
   as you go. Decisions are numbered monotonically. Each decision cites
   the review finding it addresses (e.g.,
   `Closes <topic-slug>-REVIEW-v2#H3`).
4. Once every BLOCKING and HIGH finding has a corresponding decision,
   the cycle is ready to advance.

The mode of this walk (interactive vs autonomous) is governed by
§"Autonomous mode" below.

## Step 8 — Dispatch the next generator

Spawn a **fresh** generator agent (see §"Agent dispatch templates" →
Generator). The generator MUST use `an opus-class model` — no
downgrades. Pass it:

- `<topic-slug>-SPEC-v<N>.md` (the prior version, in full)
- `<topic-slug>-REVIEW-v<N>.md` (the review of that version, in full)
- A **decision index** (decision number + one-line title only) sourced
  from `<topic-slug>-DECISIONS.md`.
- The path to `<topic-slug>-DECISIONS.md` itself so the generator can
  look up the full body of any decision it needs.

The generator's brief instructs it to consult only the decisions it
needs (traced by REVIEW citations like "Closes #N"), with the option to
trace backwards through `supersedes` chains or to grep the decisions
file by domain keyword. The default behavior is targeted lookup, NOT
bulk read — large decision lists make full-read expensive and noisy.

The generator produces `<topic-slug>-SPEC-v<N+1>.md` directly. It
writes a NEW file; it does NOT edit v<N>.

Update `<topic-slug>-README.md` to reflect the new current version.

## Step 9 — Repeat Steps 6–8

Each cycle:

```
v<N> exists → dispatch fresh reviewer → <topic-slug>-REVIEW-v<N>.md
            → walk findings, append decisions
            → dispatch fresh generator (opus) → <topic-slug>-SPEC-v<N+1>.md
            → cycle continues or exits
```

Exit condition: latest REVIEW has **zero BLOCKING and zero HIGH**
findings.

Circuit breaker: see §"Circuit breakers" — if not converged after
configurable N cycles (default 3), escalate to the operator BEFORE
generating the next version.

## Step 10 — Handoff

When the cycle exits cleanly:

- Update `<topic-slug>-README.md` with final status: final version,
  link to the SPEC, summary of remaining MEDIUM/LOW/NIT (if any), and
  the full cycle log.
- Tell the operator the SPEC is ready for implementation. If
  implementation will use a Teamster team, suggest spawning domain
  agents to own each deliverable section.
- Do NOT shut down any seasoning agents — they remain idle in case the
  operator wants to extend the cycle or query a prior reviewer for
  context. Keep them alive until the operator explicitly accepts.

## Hard rules (violations break the seasoning contract)

1. **The orchestrator NEVER edits a versioned document after writing it.**
   The v1 SPEC is your one allowed write. After that, only generators
   write SPEC files; only reviewers write REVIEW files; only you append
   to `<topic-slug>-DECISIONS.md` (with operator concurrence).

2. **Versioned documents are write-once.** Never re-open
   `<topic-slug>-SPEC-v2.md` to "fix a typo." Spawn a new cycle; the
   fix lands in v3.

3. **DECISIONS is append-only.** A decision may explicitly `supersede`
   or `repeal` a prior decision, but the old decision's text stays. The
   decision list is a permanent record of intent over time.

4. **Each generator and each reviewer is a fresh spawn.** No recycling.
   Bias across rounds is the failure mode this rule prevents.

5. **Document generators are strictly `an opus-class model`. No
   downgrades.** Reviewers are also opus by default. Investigators may
   downgrade to sonnet for pure lookup with low cognitive load, but
   default to opus for any work that requires synthesis.

6. **The orchestrator's writable surface is small.** TeamCreate, Agent
   (spawn), SendMessage, TaskCreate, AskUserQuestion, Read, and
   Write/Edit ONLY on these files:
   - `<topic-slug>-INPUT.md` (initial capture; frozen thereafter)
   - `<topic-slug>-README.md` (status + cycle log; updated each cycle)
   - `<topic-slug>-DECISIONS.md` (append-only; never edit prior entries)
   - `<topic-slug>-SPEC-v1.md` (one-time)

   No SPEC-v2+ writes, no REVIEW writes, no edits to anything else.

## Autonomous mode

Autonomous mode is **all-or-nothing**. Either the orchestrator runs the
cycle interactively (every accept/reject/investigate decision goes to
the operator) or it runs autonomously (the orchestrator makes every
decision on its own, including BLOCKING and HIGH findings).

**Entry:** The operator explicitly authorizes autonomous mode for the
topic. Log this as a decision in `<topic-slug>-DECISIONS.md` with
`Source: operator-initiated` and `Status: active`.

**Behavior in autonomous mode:**

- All findings (every severity, every cycle) are auto-decided by the
  orchestrator.
- Each auto-decision is appended to DECISIONS with body field
  including `(autonomous)` so future readers can see which were
  operator-approved vs orchestrator-judged.
- Investigators are dispatched without confirmation when research is
  needed.
- The orchestrator continues cycling until exit condition or circuit
  breaker.

**The andon-cord:** The orchestrator retains the right to pause and
escalate to the operator AT ANY TIME during autonomous mode, for any
reason — confidence is low, scope appears to be drifting, an
unexpected operator-only decision surfaces, the operator joins the
conversation, etc. To pull the cord:

- Append a decision to DECISIONS with `Source: andon-cord` describing
  why escalation is happening.
- Update README with "Status: paused — awaiting operator input on …"
- Surface a clear summary to the operator.
- Wait for direction before resuming.

**Exit from autonomous:** Operator instruction (verbal or via the andon
cord); automatic on circuit-breaker trigger; automatic on completion.

## Circuit breakers

Circuit breakers prevent the cycle from running forever or escalating
silently. They escalate to the operator BEFORE the next generator
spawn, never after.

### Default: convergence after N cycles

If the cycle has produced N SPECs (default N=3) and the latest REVIEW
still has BLOCKING or HIGH findings, **pause the cycle and escalate**.
The operator decides whether to:

- Generate v(N+1) anyway (one more pass; the breaker resets for one
  cycle).
- Switch strategies — restructure the SPEC top-down rather than patch.
- Abandon the topic.
- Adjust the N threshold and continue.

This default of 3 reflects empirical observation: rounds 1→2→3 catch
most genuine issues; rounds 4+ start producing regression-introduced
findings at a rate comparable to the rate they catch real ones.

### Other breakers

- **Decision-list churn:** If the most recent cycle's decisions
  supersede ≥3 prior decisions, escalate. The contract is destabilizing.
- **Finding-count growth:** If two consecutive reviews show
  monotonically growing BLOCKING+HIGH counts (not shrinking), escalate.
  Each generator is making things worse.
- **No-op cycle:** If a generator's output is byte-identical (or
  trivially-different by hash) from its predecessor, escalate. The
  generator failed to absorb the review.

Configure breakers per topic by setting environment variables in the
session or by decision:

- `SEASONING_MAX_CYCLES` (default 3)
- `SEASONING_MAX_SUPERSEDES_PER_CYCLE` (default 3)

Breaker overrides themselves are decisions — append them to DECISIONS
so the trail is preserved.

## Decision list format

`<topic-slug>-DECISIONS.md` opens with:

```markdown
# Decisions — <topic-slug>

Append-only. New decisions may **supersede** or **repeal** prior
entries; they do not edit them. Numbering is strictly monotonic.

## Decision index

| # | Title | Source | Status |
|---|---|---|---|
| 1 | <short title> | intake | active |
| 2 | <short title> | v1-review#B2 | active |
| 3 | <short title> | autonomous | superseded-by-#7 |
| … | … | … | … |
```

(The index is updated as a single edit when appending — the index is
metadata, not a decision itself, so editing it does not violate the
append-only contract.)

Each decision body follows the index:

```markdown
## Decision #N — <short title>

**Date:** YYYY-MM-DD
**Source:** intake | v<M>-review#<finding-id> | autonomous | operator-initiated | andon-cord
**Status:** active | superseded-by-#K | repealed-by-#K

<Body — lead with the decision itself, then **Why:** and
**How to apply:** lines if rationale would help future generators.>
```

## Agent dispatch templates

### Investigator

Use when intake or a review finding requires research (codebase,
external systems, prior art) before a decision can be made. Default
model `an opus-class model`; may downgrade to sonnet for pure lookup
with low cognitive load.

```text
You are an investigator for the seasoning of <topic-slug>.

Question to answer:
<one-paragraph concrete question>

Sources to consult:
<paths, URLs, or known-state systems>

Constraints:
- Read-only. Do not edit any file.
- Report findings via SendMessage to "team-lead".
- Findings under 500 words. Tables, not prose, where structure helps.
- Cite file:line references for any codebase claim.

Then go idle.
```

Investigator names follow Rule II (domain not role): `@store`,
`@homelab`, `@anchor`, `@hookd-source`. Never `@investigator-1`.

### Reviewer

Strictly `an opus-class model`. Fresh spawn per cycle.

```text
You are reviewer-v<N> for spec/<topic-slug>-SPEC-v<N>.md. You are a
FRESH reviewer — you have no prior context from earlier cycles. Do not
read prior reviewers' messages. Find what's wrong with v<N>, not what's
been already fixed.

Read:
- seasoning/<topic-slug>/<topic-slug>-INPUT.md
- seasoning/<topic-slug>/<topic-slug>-SPEC-v<N>.md
- seasoning/<topic-slug>/<topic-slug>-DECISIONS.md  (the contract you
  cannot contradict — decisions in "active" status are settled)
- <relevant codebase paths>

Find findings in five categories:
- BLOCKING: would prevent implementation
- HIGH:     would cause significant rework
- MEDIUM:   would cause friction
- LOW:      polish
- NIT:      trivia

Write your findings to:
seasoning/<topic-slug>/<topic-slug>-REVIEW-v<N>.md

Use this structure:

# Review v<N> — <topic-slug>

**Reviewer:** reviewer-v<N>
**Spec version:** v<N>
**Date:** YYYY-MM-DD

## BLOCKING
- B1 — [§ref] finding (≤60 words). Why it blocks. Concrete fix.
- B2 — ...

## HIGH
- H1 — ...

## MEDIUM
- M1 — ...

## LOW
- L1 — ...

## NIT
- N1 — ...

## Verification — what I confirmed is solid (3–5 items)
- ...

## Confidence note (one paragraph)
Would you stake your reputation on this spec going to implementation as-is?

Cite section numbers and file:line for every finding. Total under
2000 words. Audit only — do not propose rewrites, do not implement.

Then go idle.
```

Reviewer names per cycle: `@reviewer-v<N>` (or domain-flavored if the
review specializes, e.g. `@reviewer-promql-v3`). Each spawn is fresh.

### Generator

Strictly `an opus-class model`. Fresh spawn per cycle. No downgrades.

```text
You are gen-v<N+1> for spec/<topic-slug>-SPEC-v<N+1>.md. You are a
FRESH generator — you have no prior context from earlier cycles.

Read in this order:
1. seasoning/<topic-slug>/<topic-slug>-INPUT.md  (original goal — never deviate)
2. seasoning/<topic-slug>/<topic-slug>-SPEC-v<N>.md  (prior version)
3. seasoning/<topic-slug>/<topic-slug>-REVIEW-v<N>.md  (audit you are resolving)

Decision access:
The full decisions list is at seasoning/<topic-slug>/<topic-slug>-DECISIONS.md.
It opens with a "Decision index" table of decision# + title + status.

DO NOT bulk-read the decisions file. Instead:
- The REVIEW cites specific decisions for each addressed finding (e.g.,
  "Closes <topic-slug>-REVIEW-v<N>#H3 via decision #14"). For each such
  citation, look up that decision body in DECISIONS.md.
- If a decision has a "supersedes #K" tag, also read #K to understand
  what's being replaced.
- For decisions you suspect are relevant by domain (e.g., naming
  conventions, port choices) but aren't cited, scan the decision index
  and read only the bodies that look on-topic.

Decision contract:
Every decision in "active" status MUST be honored in v<N+1>. Superseded
or repealed decisions MUST NOT be applied. If you find an apparent
conflict between the INPUT and an active decision, the decision wins
(decisions explicitly amend the input).

Produce seasoning/<topic-slug>/<topic-slug>-SPEC-v<N+1>.md as a NEW
file. You may share large portions with v<N> verbatim where the review
did not touch. For every BLOCKING and HIGH finding in v<N>-review, the
corresponding decision tells you how to resolve it — apply the
resolution. For MEDIUM/LOW/NIT, apply where straightforward; defer to
the next cycle where not.

Begin v<N+1>.md with:
- Header line: "Version: v<N+1>"
- "Inputs: INPUT.md (frozen), DECISIONS.md (through #<latest-decision>),
   SPEC-v<N>.md, REVIEW-v<N>.md"
- A one-paragraph changelog summarizing what changed from v<N> to v<N+1>
  and which findings were resolved.

Do not edit any file other than <topic-slug>-SPEC-v<N+1>.md. Do not
modify DECISIONS or any prior SPEC/REVIEW version.

Then go idle.
```

Generator names per cycle: `@gen-v<N+1>`. Each spawn is fresh.

## Resumption from a prior session

If a `seasoning/<topic-slug>/` directory already exists when this skill
is invoked:

1. Read `<topic-slug>-README.md` to get current version + cycle log.
2. Read the decision index from `<topic-slug>-DECISIONS.md` (not bodies
   — index only, for orientation).
3. Determine cycle state:
   - SPEC-v<N> exists without a matching REVIEW-v<N> → dispatch a reviewer.
   - Both exist with unresolved findings → resume walking.
   - Both exist with all findings resolved but no SPEC-v<N+1> → dispatch
     a generator.
   - Cycle is at exit condition → tell operator, await direction.
4. Tell the operator the resumption state and ask what they want to do
   next.

## Anti-patterns (regression risks)

- **Lead editing a versioned spec directly.** Across multi-round cycles
  this introduces new bugs at the same rate as old ones are fixed.
  Empirically observed; this is the failure mode the protocol exists to
  prevent.
- **Recycling reviewers.** Prior reviewers carry findings from earlier
  rounds; they can't see the v<N+1> SPEC fresh.
- **Bulk-reading the full decisions list in a generator.** With long
  cycles the list grows. Default to targeted lookup via the index and
  REVIEW citations.
- **Inline decisions made in conversation but not appended.** Future
  generators don't see them. Every consequential decision goes to
  DECISIONS before the next generator runs.
- **Editing prior decisions.** Breaks the contract. Use supersede only.
- **Skipping the cycle for a quick fix.** "It's just a typo" — patch it
  through a v<N+1> cycle anyway. The discipline is the value.
- **Pulling andon-cord without logging.** The audit trail breaks. Every
  pause must be appended as a decision so future sessions know why.
