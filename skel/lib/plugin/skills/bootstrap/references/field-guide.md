# Teamster Field Guide

Practical lessons for operating as a Teamster team lead. These are learned
from real failures — read them before your first dispatch.

The Eight Rules tell you WHAT to do. This guide tells you what goes wrong
when you don't, and how to avoid the common traps.

> **Solo sessions.** Lessons 1–8 are about running a **team**. In a **solo
> session** (one primary agent, `TEAMSTER_SOLO=1`) they apply only where they
> aren't about coordinating peers — lesson 6 (verify autonomously) and lesson 7
> (right model for the job) still hold; lessons about dispatch, affinity, idle
> agents, and briefing parallel peers do not. Lessons 9–11 (focus/tags, source
> markup, verify the deployed binary) apply in both modes.

---

## 1. The team is step zero — when you need a team

Don't spawn agents ad hoc then try to organize them retroactively. When work
calls for parallel agents, create the team first (`/teamster:bootstrap`),
read the protocol, then spawn domain agents. If you skip this step
and dispatch parallel work anyway, you'll spawn bare subagents that are
invisible to monitoring, have no persistent identity, and lose all context
between tasks.

The team is not mandatory for *all* work — it's the right tool when the work
decomposes across multiple agents. A **solo session** (`TEAMSTER_SOLO=1`) skips
the team entirely: the one primary agent does its own WMS bookkeeping inline
(see `/teamster:solo`) and spawns ephemeral subagents only for bounded sub-tasks
like a fresh-context review. The rule is: don't dispatch *parallel* work without
a team — not "never work without a team."

## 2. Name agents for what they know, not what they do

All agents can build, test, and review. Their value is accumulated file
context. `@store` knows store.go. `@engine` knows engine.go. `@builder`
knows nothing specific.

When a task involves store.go, send it to `@store` — even if it's a
test-writing task. The agent that already has the file in context writes
better tests than a fresh `@tester` that has to re-read everything from
scratch.

## 3. The lead briefs, agents collaborate, the lead verifies

Your job is three things: set up the team, create tasks, and verify final
results. Everything in between — implementation, testing, bug fixing,
iteration — happens between agents directly via SendMessage.

If you find yourself relaying messages between peers ("@tester says the
build is broken, @hooks please fix"), you're doing it wrong. @tester
should message @hooks directly. You observe.

## 4. Never shut down agents until the human accepts

Idle agents cost nothing and retain full context. The moment you shut
them down, you lose their accumulated file knowledge. If the human
requests a change, you need to spawn fresh agents that re-read everything.

Wait for explicit acceptance: "accepted", "ship it", "looks good",
"merge it". The words "ready to shut down when you accept" followed by
immediate shutdown is the single most common mistake. Wait for the actual
response.

## 5. Brief parallel agents about each other

Agents cannot discover who else is active. They don't know who's editing
which files. If you send @hooks and @dashboard to work on overlapping
files without telling them about each other, they WILL collide — one
agent's edits will break the other's build, and neither will understand
why.

Every dispatch brief for parallel work must include: "you are working in
parallel with @X on Y files — coordinate with them via SendMessage before
editing shared files."

## 6. Verify autonomously — the test agent is the human

Before presenting work as done, exercise the system the way a human would.
The test agent launches programs, sends input, watches output, inspects
logs, and reports what happened. If the human has to run test commands
themselves, your test brief has a gap.

Session explorer (`lib/scripts/session-explorer.sh`) lets a test agent drive
interactive programs, including Claude Code itself. Use it.

## 7. Use the right model for the right job

Sonnet implementations are fast and usually correct for bounded tasks.
But sonnet reviews miss architectural issues. When reviewing anything that
touches production infrastructure, system design, or cross-cutting
concerns, use an opus-class reviewer.

The pattern: sonnet builds, opus reviews. Not the other way around. Don't
waste opus on file reads, and don't trust sonnet with architectural
decisions.

## 8. The protocol emerges from violations

Don't try to anticipate every failure mode up front. Build, observe what
goes wrong, write a rule that prevents recurrence, add enforcement, test
that the enforcement works, document. The strongest rules are forged from
real mistakes.

When an agent violates a principle:
1. Correct the immediate behavior
2. Write a rule
3. Add enforcement (hook, additionalContext, skill instruction)
4. Verify the enforcement works
5. Add to the protocol

This is how the Eight Rules were written. Every one exists because an
agent did the wrong thing and we made it impossible (or at least harder)
to do again.

---

## 9. Focus and tags are the attribution signal, not bookkeeping

Cost attribution is only as good as the focus and tags the team sets while
working. They are operating discipline, not optional metadata:

- **An agent that never calls `wms_setFocus` produces cost that can only land
  in the "unallocated" bucket** — it cannot be attributed to any task, outcome, or
  product. So every working agent sets focus on its task as its first action;
  focus is per-agent (each agent declares its own current entity).
- **The lead is not exempt.** The lead's coordination messages — routing
  decisions, dispatch briefs, review calls — cost real money. If the lead
  never opens a WMS focus interval of its own, all of that spend lands in
  `unallocated`. The lead must call `wms_setFocus` on the strategic Outcome at
  session start (Step 7) and after each dispatch, so its inter-dispatch
  coordination cost has a home. Teammates hold focus on their WorkUnits; the lead
  holds focus on the Outcome between dispatches. This is the single largest
  unallocated bucket in practice — measured at ~52% of all unallocated spend.
- **Untagged work cannot be faceted.** Cost-by-phase, cost-by-work-type, and
  release/priority breakdowns all read the `tags` the lead applies via
  `wms_tagEntity`. The lead owns tagging (it owns task creation + the state
  machine) and stewards the vocabulary.

The tag surface is exactly two methods — dynamic and self-describing, no admin
layer. The flow: **discover, then apply.** Call `wms_listTags` first — it
returns each classifier's `description` ("when to apply"), so you reuse the
existing tag that fits instead of inventing a near-duplicate. Apply with
`wms_tagEntity`; introduce a NEW `(tag_key, tag_value)` only for a genuinely new
dimension, and pass a `description` so it teaches the next caller what it means.
An existing tag's description is never overwritten, so the vocabulary grows
organically yet stays legible.

Treat "did this task get focus + a phase/work-type tag?" as part of the
definition of done, the same way you treat "does it build?".

**The focus-nudge hook catches you if you forget.** The hookd server tracks
per-(session, agent) focus state. If an agent makes tool calls without ever
having called `wms_setFocus`, hookd injects an `additionalContext` nudge into the
PreToolUse response reminding the agent to set focus. It fires up to 3 times then
stops — it's a safety net, not infinite nagging. The nudge is the system enforcing
this lesson automatically, so agents that miss focus at session start get
corrected early.

**The scheduled sweep recovers what the nudge missed.** A systemd timer runs
`rollup --sweep` (deterministic passes: allocate, recover-focus, recover-warmup,
recover-gaps) followed by `/teamster:sweep` (LLM-assisted: synthesize WMS
Outcomes for orphan sessions by reading transcript heads). Between them, three
attribution methods fill in what agents failed to declare:

- `admin_warmup` — re-attributes pre-first-`setFocus` warmup cost to the
  session's resolved outcome with `phase=admin`.
- `gap_recovery` — fills partial gaps in sessions that DO have focus intervals
  by resolving the entity from the session's existing attributions.
- `synthesized_outcome` — creates a new WMS Outcome by reading the transcript
  head, then attributes the session's cost to it.

The sweep is automated and reversible (each method has an `--un*` flag). But
prevention is cheaper than recovery: setting focus on your first action costs one
tool call; recovering it later costs an LLM sweep pass.

---

## General Development Practices

These apply to any multi-agent software project.

## 10. Mark structure at the source, don't infer it at the display layer

When a renderer needs to know about content structure (which part of a
string is a verb vs a parameter), mark it at the point of creation — not
by pattern-matching at display time.

Bad: renderer maintains a list of known verb prefixes ("Reading ",
"Editing ", "Searching for ") and splits on them. Fragile, breaks when
new verbs are added, fails on edge cases.

Good: source wraps parameters in a lightweight markup (`__filename__`).
Renderer strips markers and applies styling. Source controls semantics,
renderer controls presentation.

## 11. Verify the deployed binary, not just the build

`go build` succeeding and the correct binary running in production are
two different things. After every deploy:

- `md5sum /usr/local/bin/binary` vs freshly built binary
- `which binary` to confirm PATH resolution
- Check for stale binaries in other PATH entries

Test installs to `/tmp/` directories that add themselves to PATH will
shadow the real binary. Clean up test artifacts immediately.

## 12. Test ANSI rendering on the actual target terminal

ANSI escape sequences that are "correct" per spec don't always render
identically across terminals. Truecolor (24-bit RGB) and 16-color and
256-color modes produce different results depending on the terminal
emulator and its configuration.

When iterating on terminal colors: write a mock script, have the user
run it in their actual terminal, iterate on feedback. Don't trust
`cat -v` analysis alone.

## 13. An installer must work from a fresh clone

The installer's entry point is always the repo. It can never assume its
own binaries are already installed. The correct architecture:

1. Shell script at repo root is the entry point
2. Shell script compiles source into a build directory
3. Shell script invokes the compiled installer with explicit paths
4. Installer uses flags (`--basedir`, `--repo`, `--builddir`) not
   `os.Executable()` path inference

The installer binary itself is a build artifact — it never gets deployed
to the target.

## 14. Non-destructive config merging

An installer that touches user config files (settings.json, .bashrc)
must:

- Load existing content, merge, write back — never overwrite
- Skip keys that already exist (don't change a working URL to a new port)
- Deduplicate entries by semantic identity, not exact string match
- Back up before modifying: `cp file file.pre-<tool>`

## 15. Clean test environments every time

Never reuse a test container/VM that had a previous install. Test
artifacts persist in PATH, config files, systemd units, and databases.
Destroy and rebuild for every test run.

---

## Developing Teamster Itself

For teams developing Teamster itself — building the hook client, feed,
hookd, WMS, installer, plugin, and protocol.

## 16. Every feed change needs a deploy + restart cycle

feed is a standalone binary. Changing display.go, hook.go, or main.go
requires: build → deploy to bin/ → user restarts feed.
Changes to the hook client (teamster binary) take effect on the next
tool call (forked per event). Changes to hookd require a systemd restart.

Don't tell the user "it's fixed" until:
- The binary is deployed (not just built)
- The correct binary is in the PATH (check `which`)
- The user has restarted the relevant process

## 17. The hook client must never block or crash

The hook client runs on every Claude Code tool call. If it hangs,
Claude Code hangs. If it crashes with a non-zero exit, Claude Code
may show errors. The binary must:

- Exit 0 always (recover from panics)
- Timeout HTTP calls (2 seconds max)
- Swallow all errors (log them, don't propagate)

## 18. JSONL is the contract between hook client and feed

The hook client writes enriched fields (`_tool_tag`, `_tool_display`,
`_focus`, `_bash_cmd`, `_agent_name`) into the JSONL. feed reads
them. Changes to either side must maintain this contract.

When adding a new display feature (like `__param__` markdown), both
sides must be updated and deployed together. A new hook client writing
`__param__` markers with an old feed shows raw underscores.

## 19. The installer is the most fragile component

The installer touches: settings.json (hooks, env, permissions), MCP
registrations, CLAUDE.md, plugin marketplace, systemd units, PATH.
Every one of these can break Claude Code or the monitoring stack if
done wrong.

Test the installer in a disposable VM before ever running it on your
host. After running on your host, immediately verify:
- `feed --no-follow -n 5` shows events
- No duplicate hook entries in settings.json
- MCP servers connected (`claude mcp list`)
- hookd healthy (`curl localhost:9128/health`)

## 20. Bash `description` is the observability contract with the operator

When calling the Bash tool, always provide the `description` parameter.
This short phrase ("Check service status", "Search for config references")
becomes the `[ACT]` line in the activity feed — it's how the operator
understands what you're doing without reading raw shell commands.

Without `description`, only the raw command appears as `[EXEC]`. This is
harder to scan and provides no intent context. The operator can see
which agents are providing good observability and which aren't — be one
of the former.
