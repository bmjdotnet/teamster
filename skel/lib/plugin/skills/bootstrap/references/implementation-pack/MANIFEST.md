# implementation-pack

**Loads at:** into teammate briefs — not the lead's own context, and not at
the same time as `dispatch-pack/`. This is what gets referenced (by path or
by excerpt) when the lead writes a SendMessage/Agent brief for a teammate.

**Audience:** teammates doing bounded implementation, validation, or review
work. Not the intake/dispatch protocol — teammates don't run the interview
or decide how to decompose a batch, so they don't need
`decomposition-guidance.md` or `muster-guide.md`.

## Contents

| File | What it's for |
|------|---------------|
| `teammate-guide.md` | Distilled, self-contained guidance for a teammate: which Eight Rules apply directly (IV, VI, VIII), set-focus-first, how to run an execution-loop phase if assigned one, direct peer communication, and which field-guide lessons apply to hands-on work (6, 7, 9, 10–24). |

## Why a distillation instead of full copies

`eight-rules.md` and `field-guide.md` are lead-authored documents that also
contain lead-only material (team formation, dispatch decomposition, briefing
parallel peers from the dispatching side). Copying them wholesale into a
teammate's context would reintroduce the same startup-cost problem this
restructure exists to fix, and a second copy of the same text is a drift
risk (`session-protocol.md`'s "canonical source, not a copy" principle
applies here too).

`teammate-guide.md` instead extracts exactly the subset relevant to a
teammate and points back to `../dispatch-pack/` (`eight-rules.md`,
`field-guide.md`, `execution-loop.md`, `rubrics.md`) for full text when a
teammate needs it — e.g. a teammate assigned ADVERSARIAL REVIEW should read
`../dispatch-pack/rubrics.md` directly rather than a re-summarized version.

## Judgment call — open for review

Whether `teammate-guide.md`'s distillation is the right level of detail
(too thin / too thick) is a judgment call, not a measured decision. If the
token-savings measurement (SO-1 verification step) shows this pack is
disproportionately large relative to what a bounded task actually needs,
trim it further before shipping.
