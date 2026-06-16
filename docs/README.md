# Teamster documentation

Start here to find your way around. For what Teamster is and how to install it,
see the top-level [README](../README.md) first.

## Getting started

| Document | What it covers |
|----------|----------------|
| [quickstart.md](quickstart.md) | Fresh clone to a running dashboard on one host — the recommended first read. |
| [wizard.md](wizard.md) | Field-by-field reference for the guided installer (`install.sh`) and the tag-setup TUI (`teamster setup tags`). |
| [specs/REMOTE-INSTALL.md](specs/REMOTE-INSTALL.md) | The hub-and-remote model: one host runs the services, others run only the lightweight hook client. |

## Reference

| Document | What it covers |
|----------|----------------|
| [terminology.md](terminology.md) | Glossary of Teamster terms: WMS hierarchy (outcomes, work units), cost attribution methods, activity tags, session modes, and the tag taxonomy. |
| [vision.md](vision.md) | The product vision and how Teamster's workflow model fits the broader design. |
| [session-explorer-guide.md](session-explorer-guide.md) | Primer for driving interactive programs (Claude, ssh, wizards) via tmux — used by test agents. |

## Deeper system docs

These live alongside the shipped assets under [`../skel/doc/specs/`](../skel/doc/specs/):

- `architecture.md` — full system design: hub/remote topology, every component,
  data flows, environment variables, and operating modes.
- `semantic-conventions.md` — JSONL field conventions, the tag taxonomy, and the
  work-management state machine.
- `wms-dashboard-spec.md` — the work-management dashboard, implemented and planned.

For developers working on Teamster itself, the repo-root `CLAUDE.md` is the
developer guide (build, test, repo layout, and pitfalls).
