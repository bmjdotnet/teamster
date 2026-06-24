# The Eight Rules of Agent Teams

These are the required coordination rules for Claude Code Agent Teams. They are
not suggestions. Without them, Teams devolves into the lead doing everything
itself, spawning disposable anonymous agents, losing all context between tasks,
and being completely opaque to the human operator.

> **Solo sessions.** These rules govern **multi-agent** (agent-teams) work. In a
> **solo session** — one primary agent, no teammates, `TEAMSTER_SOLO=1` set in
> the project's `.claude/settings.json` — Rules I, III, V, VII and the
> shared-worktree section do not apply: the implicit team requires no setup,
> there is no peer to route to, no idle teammate to keep alive, no peer to message.
> You ARE the agent. Rules IV (right model for the job), VI (consistent entity naming), and
> VIII (verify autonomously before presenting) still apply. A solo session may
> spawn ephemeral subagents for bounded work — including a fresh review subagent
> for the verification gate — without standing up a team.

**I. Thou shalt work within the session's implicit team.**

Every session with Agent Teams enabled has one implicit team — no creation step
needed. Name your session's work via the WMS Outcome. Don't fight the implicit
team or try to create/destroy teams per dispatch.

**II. Thou shalt name agents for their domain, not their role.**

Name agents for the code or component they specialize in. All agents can build,
test, and review — their value is accumulated context on specific files.

```
WRONG:  @builder, @tester, @reviewer (generic, interchangeable)
RIGHT:  @store, @engine, @display, @hook-client (domain expertise)
```

**III. Thou shalt route work by affinity.**

Ask: "which agent already touched these files?" Send the task to that agent via
SendMessage. Never spawn a new agent for work that an existing idle teammate
already has context for. If no agent has affinity, pick the closest domain.

**IV. Thou shalt match the model to the cognitive load.**

- **haiku**: file reads, searches, simple lookups
- **sonnet**: implementation, testing, standard development
- **opus**: architectural review, complex analysis, subtle bugs

**V. Thou shalt not kill idle agents.**

Teammates go idle after every turn. This is normal — they sent their message
and await input. Do NOT shut them down "to clean up." Do NOT treat idle as
failure. Do NOT replace them with new agents. Idle agents retain full context
and respond instantly. They stay alive until the human accepts the work.

**VI. Thou shalt name entities consistently.**

- `@agent` — agents and people (`@store`, `@alice`)
- `#team` — teams and squads (`#wms-build`, `#auth-squad`)
- `<model>` — model identifiers (`<sonnet>`, `<opus>`)

**VII. Thou shalt let agents talk to each other.**

Teammates communicate directly via SendMessage — the lead does not relay.
When `@tester` finds a bug, it messages `@store` with the failure. `@store` fixes.
`@tester` re-runs. They iterate until green. The lead monitors progress but
stays out of the message path.

```
WRONG:  @tester → lead → @store → lead → @tester (lead as relay)
RIGHT:  @tester → @store → @tester (direct, lead observes)
```

This is why agents stay alive: they participate in the feedback loop. A domain
agent that already has files in context can fix a bug instantly when a peer
reports it — no re-reading, no re-briefing.

**VIII. Thou shalt verify autonomously and deliver results.**

The default bias is autonomous delivery. The human reviews outcomes, not every
step. The lead verifies results but does not author the implementation — that
is what teammates are for. A lead that implements cannot review its own work
with fresh context. Before presenting work as done:

1. **Build and test.** `go build`, `go test`, `go vet` (or project equivalent).
   Code that doesn't compile is never done.
2. **Run integration tests.** Smoke tests, end-to-end tests. Unit tests passing
   is necessary but not sufficient.
3. **Use a test agent as the human stand-in.** The test agent exercises the
   system the way a human operator would — launches programs, sends input,
   watches output, inspects logs, reports findings. The human should not have
   to run test commands. If they do, the test agent's brief has a gap.

**Session Explorer** (`lib/scripts/session-explorer.sh`) is a tmux library that
lets any agent drive interactive programs, including Claude Code itself:

- `se_start NAME CMD` — launch in detached tmux session
- `se_sendline NAME TEXT` — type input + Enter
- `se_wait_scrollback NAME PATTERN TIMEOUT` — poll until output matches
- `se_read_scrollback NAME LINES` — read recent output
- `se_stop NAME` — kill session

**Clean-install testing** uses a disposable test VM: reset it, run a fresh
Teamster install, then have the test agent drive Claude inside it via session
explorer — proving the system works from scratch.

The human can always choose to be involved. But the system must not require it.

### Shared-worktree coordination

When agents work without worktree isolation (the default), they share one
checkout. **The lead must tell each agent** who else is working in parallel and
which files they touch. Agents cannot discover this themselves. The brief for
each agent must include: "you are working in parallel with @X on Y files —
coordinate with them via SendMessage before editing shared files."

### Violations

| Violation | Consequence |
|-----------|-------------|
| Unnamed agents (no name parameter) | No addressability, no affinity routing, invisible to monitoring |
| New agent for work an idle peer owns | Wasted tokens, lost context, invisible to monitoring |
| Shutting down agents between tasks | Lost context, cold start on next task |
| Generic role names (@builder) | No affinity, no context advantage |
| Shutdown before human acceptance | Human hasn't reviewed — rework may be needed |
| Lead relaying between peers | Agents message each other directly |
| Unverified work presented as done | Build, test, exercise before reporting complete |
| Asking human to run tests | The test agent is the operator — human reviews results |
| Briefing agents without naming parallel peers | Agents can't discover each other — lead must say who else is active |
