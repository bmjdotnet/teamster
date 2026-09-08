# dispatch-pack

**Loads at:** first dispatch — once team mode is confirmed and the lead is
about to bootstrap a team / send the first task brief. Per ruling R-P5:
"Wire execution-loop.md and rubrics.md into dispatch-pack... Available when
leads need the review gate without adding to startup cost."

**Audience:** the lead. This is what a lead needs to organize, brief, and
monitor a team — not what an individual teammate needs to do bounded
implementation work (see `../implementation-pack/` for that).

## Contents

| File | What it's for |
|------|---------------|
| `eight-rules.md` | The core coordination protocol — team naming, agent naming, routing by affinity, model tiering, idle-agent handling, direct peer communication, autonomous verification. |
| `field-guide.md` | Practical lessons on what goes wrong when the Eight Rules aren't followed, plus general development and Teamster-specific practices. |
| `muster-guide.md` | Agent roster + health awareness — read as part of the normal dispatch/monitor cycle (checking teammate context fill, liveness, when to reap and replace). |
| `execution-loop.md` | The 4-phase IMPLEMENT → VALIDATE → ADVERSARIAL REVIEW → COMMIT loop and who runs each phase. |
| `rubrics.md` | The concrete checklists applied during the ADVERSARIAL REVIEW phase. |
| `decomposition-guidance.md` | How to split work across parallel agents — file independence, not domain grouping. Field evidence: the taxonomy team's lead collided two agents on a shared file by grouping the dispatch by domain instead of by file ownership. |

## Why this set, together

This is the same five-plus-one set that was previously read at bootstrap
time (Step 3 of the old `bootstrap/SKILL.md`, before Step 4's tool
loading) — unchanged in content, just relocated and given a name that
matches when it actually loads. `decomposition-guidance.md` is new,
added per the R-P5 field-evidence note.

If a teammate is assigned an execution-loop phase (VALIDATE or ADVERSARIAL
REVIEW in particular), point them at `execution-loop.md` and `rubrics.md`
directly from their brief rather than duplicating the content into
`implementation-pack/` — one canonical copy, referenced from wherever it's
needed.
