---
name: start
description: Front door for a Teamster Codex session — the Codex counterpart of Claude Code's `/teamster:start`. Run at the start of a session that involves non-trivial work (not just a quick question) to stand up WMS tracking for it. Ambient-discoverable — surfaces in a generic "what skills do you have" listing, unlike the other Teamster skills.
---

# Start a Teamster Session

This is the front door. It does no work of its own — it hands off to the
`teamster-solo` skill, which is what actually creates the WMS Outcome, runs
the context-tag interview, and sets focus.

Read the sibling skill directory `teamster-solo` (installed alongside this
one under `~/.codex/skills/`) and follow its `SKILL.md` inline, exactly as if
the operator had typed `$teamster-solo` directly.
