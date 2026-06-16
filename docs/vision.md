# Vision

---

## What Teamster does

Teamster makes Claude Code's Agent Teams usable at scale. It provides:

1. **Observability** — a real-time activity stream showing what every agent is
   doing, thinking, and accomplishing, via terminal feed and web dashboard.
2. **Work management** — a structured Outcome -> Work Unit hierarchy with state
   machines, cost attribution, and tag-driven drill-downs.
3. **Protocol enforcement** — standing instructions and guardrails that teach
   the lead agent how to decompose work, select models, brief teammates, and
   manage task lifecycle.
4. **Cost transparency** — automatic token/USD attribution to work entities,
   with deterministic and LLM-assisted recovery for unattributed sessions.

---

## Design principles

- **The protocol is the product.** Teamster's value comes from teaching agents
  how to work well together, not from the tooling alone. The Eight Rules, the
  Field Guide, and the skills encode operational knowledge that makes teams
  effective.

- **Observability is not optional.** Both team mode and subagent mode keep the
  full activity stream, cost attribution, WMS tracking, and dashboards on.
  You never lose visibility by choosing a simpler operating mode.

- **Cost-bearing focus drives attribution.** The `wms_setFocus` call is the
  primary mechanism for attributing token spend to work. Recovery passes
  (warmup, gaps, transcript, synthesis) exist to fill what agents miss, not
  to replace the discipline.

- **Single install, per-session mode.** The same Teamster install serves both
  team mode (persistent named teammates) and subagent mode (one primary agent).
  Mode is chosen at session start via `/teamster:start`, not at install time.

---

## Architecture overview

Teamster runs as a **hub** with optional **remote** clients:

- **Hub** — runs `hookd` (event server + dashboard), both MCP servers (activity
  and WMS), the MySQL/MariaDB store, and optional monitoring (Prometheus,
  Grafana, otelcol). One hub per fabric.
- **Remote** — any host running Claude Code that participates. Runs only the
  Python hook client (stateless, per-event) and the plugin. No daemons.

All events flow through the hook client to hookd via HTTP POST, are written to
JSONL, and (for WMS-relevant events) update the MySQL store. The activity feed
tails the JSONL; the dashboard serves it via SSE; Grafana queries Prometheus
metrics and MySQL directly.

---

## Roadmap

Teamster's protocol and work-management abstractions are designed to be
transport-independent. The tag taxonomy, observability patterns, and cost
attribution model can serve as the foundation for integrations beyond
Claude Code's hook system.
