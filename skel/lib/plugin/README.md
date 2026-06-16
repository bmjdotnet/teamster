# Teamster Plugin

Claude Code plugin providing Agent Teams workflow orchestration.

## Skills

| Skill | Invocation | Description |
|-------|-----------|-------------|
| start | `/teamster:start` | Front door: gather objective, recommend team vs subagent, dispatch to bootstrap or solo |
| bootstrap | `/teamster:bootstrap` | Bootstrap a team with WMS tracking and dispatch routing |
| solo | `/teamster:solo` | Teamless subagent-mode bootstrap: WMS Outcome + context-tag interview, no team |
| status | `/teamster:status` | Show team state, WMS projects, tasks |
| tags | `/teamster:tags` | Tag steward: refine vocabulary, classify historical work, add reporting dimensions |
| plan | `/teamster:plan` | Decompose work into WMS entities and team composition |
| review | `/teamster:review` | Readiness assessment before presenting work |
| seasoning | `/teamster:seasoning` | Iterative spec refinement |
| sweep | `/teamster:sweep` | Autonomous nightly attribution sweep (headless, `claude --print`) |

## Install

Plugin installation is handled automatically by `install.sh` from the repo root.
The installer copies this directory to `~/teamster/lib/plugin/`, registers it as
a local marketplace, and enables it in `~/.claude/settings.json`.

To install manually (if needed):
```bash
claude plugin marketplace add ~/teamster/lib/plugin
# Then enable in ~/.claude/settings.json: "enabledPlugins": {"teamster@teamster": true}
```

## The Eight Rules

See `skills/bootstrap/references/eight-rules.md` for the full protocol.
