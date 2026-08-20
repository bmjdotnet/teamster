# Teammate Guide

This is what you need to do your work as a teammate on a Teamster team. It
does not cover the intake interview or dispatch decomposition — that's the
lead's job, handled before you were spawned. This is the subset of the
protocol that applies to you.

---

## The three rules that apply to you directly

From `eight-rules.md` (full text in `../dispatch-pack/eight-rules.md` if you
want the complete protocol, including the rules that govern the lead):

- **Rule IV — match the model to the cognitive load.** If you spawn your own
  sub-subagent for a bounded piece of work, pick the tier the work needs:
  haiku for file reads/searches, sonnet for implementation, opus for
  architectural judgment calls. Don't default to your own tier for
  everything you delegate.
- **Rule VI — name entities consistently.** `@agent` for agents and people,
  `#team` for teams, `<model>` for model identifiers. If you spawn a
  sub-subagent, give it a distinct `subagent_type` when possible so it has
  its own identity in the fleet view.
- **Rule VIII — verify autonomously before reporting done.** Build it, test
  it, exercise it the way a human would before you tell anyone it's ready.
  "It should work" is not verification.

## Claim your work unit first, always

Before touching anything, call `wms_claimWorkUnit('<your work unit id>')`.
This is not optional bookkeeping — in one call it returns your assignment
brief, opens the focus interval that attributes your token cost to the task
instead of the "unallocated" bucket, and transitions the work unit to
`active`. Do it as your first action, every time you pick up work.

`wms_setFocus(entityType="workunit", entityID=<your work unit id>,
focus=<short what>)` remains a fallback for work that isn't scoped to a
WorkUnit of your own — e.g. you're helping another agent on a bounded
sub-task. For any WU-scoped assignment, `wms_claimWorkUnit` is the preferred
path: it's also how you receive your brief, not just how you attribute cost.

## Deliver your result when you're done

When your assigned work is complete, call `wms_deliverResult(id=<your work
unit id>, summary=<one-line headline>, result=<the full write-up>)`. This
stores your deliverable durably in WMS and transitions the work unit
`active` → `review` automatically — the lead reads this stored deliverable
instead of trusting your final SendMessage compression, so put the real
content here, not just in chat. Send a short SendMessage pointer to whoever's
waiting on you after delivering — the message is the notification,
`wms_deliverResult` is the record.

## If you're running an execution-loop phase

If your brief assigns you IMPLEMENT, VALIDATE, or ADVERSARIAL REVIEW (see
`../dispatch-pack/execution-loop.md` for the full loop and
`../dispatch-pack/rubrics.md` for what a review actually checks):

- **IMPLEMENT**: build the change, get it compiling, read your own diff
  back before declaring ready. Do not also validate or review your own work
  — fresh context for those phases is the point.
- **VALIDATE**: you are not the implementer. Run the actual tests, don't
  eyeball the diff and say "looks fine." Send failures directly to the
  implementer via SendMessage — the lead does not relay (Rule VII).
- **ADVERSARIAL REVIEW**: read the diff cold, apply the relevant checklist
  in `rubrics.md` item by item. "LGTM" without applying the rubric is a
  failed review. Send findings directly to the implementer, or "review
  passed" to the lead.

## Talk to your peers directly

If you find a bug in another agent's work, or need something from them,
message them directly via SendMessage. Don't route it through the lead —
the lead observes, it doesn't relay (Rule VII). This is also how you keep
working while idle-but-not-shut-down: peers can reach you directly and you
respond with full context intact.

## Practical lessons that apply to you

From `../dispatch-pack/field-guide.md` — the ones written for whoever is
doing hands-on work, not just the lead:

- **Lesson 6** — verify autonomously; the test agent is the human stand-in.
- **Lesson 7** — use the right model tier for whatever you delegate.
- **Lesson 9** — focus and tags are the attribution signal. Claim first
  (above), and if you create or transition WMS entities, know that untagged
  work can't be faceted later.
- **Lessons 10–24** — general development practices (structure at the
  source, verify the deployed binary, ANSI rendering, installer
  non-destructiveness, feed/hookd deploy cycles, Bash `description` fields,
  context-pressure self-monitoring, decomposition/rework vocabulary). Skim
  whichever are relevant to what you're touching.

You do not need lessons 1–5 (team formation, agent naming *by the lead*,
lead-as-non-relay, idle-agent handling *from the lead's side*, briefing
parallel peers *from the lead's side*) — those describe decisions the lead
already made before dispatching you. And you do not need `muster-guide.md`
or `decomposition-guidance.md` — those are the lead's dispatch/monitor
tooling, not yours.
