# Teamster

This is your Teamster install directory. Teamster provides observability,
workflow enforcement, and work management for Claude Code Agent Teams.

## Getting started

1. **Start a session:** Run `/teamster:start` in Claude Code. It interviews
   you on the session's focus, recommends team vs solo mode, sets up WMS
   tracking, and ensures your token spend is attributed.

2. **Configure tags:** Run `teamster setup tags` to set up your tag vocabulary
   via the TUI wizard (guided on first run, editor on subsequent runs).

3. **Open the dashboard:** The web dashboard runs at the hookd URL shown by
   `teamster status` (default port 9125). Grafana dashboards are provisioned
   alongside it.

4. **Watch the activity stream:** Run `feed` in a terminal to see real-time
   agent activity.

## Key commands

| Command | What it does |
|---------|-------------|
| `teamster status` | Show service health, ports, and store connectivity |
| `teamster tags list` | Show the tag vocabulary |
| `teamster sql` | Credential-safe database queries |
| `teamster wms drain` | Close dangling focus intervals |
| `teamster wms list` | List open outcomes and work units |
| `teamster wms close` | Close an outcome or work unit |
| `teamster backup` / `list` / `status` | Take a backup, list backups, show timer status |
| `teamster restore <path>` | Restore from a backup directory |
| `feed` | Real-time terminal activity stream |

## Architecture at a glance

| Component | Role |
|-----------|------|
| `hookd` | HTTP event server + web dashboard. Receives POST `/event`, serves SSE at `/events/stream`. |
| `activity-mcp` | MCP server: `reportActivity`, `setOverallIntent`, `completeActivity`, `setMode`. |
| `wms-mcp` | MCP server: outcome/work-unit CRUD, tags, focus, dependencies. |
| `feed` | Terminal activity viewer (tails JSONL, colorizes). |
| `rollup` | Cost-attribution pipeline. Allocates token spend to WMS entities. Runs on a systemd timer. |
| `classify` | Phase and work-type classifier. Derives tags from activity signals. Runs every 5 minutes. |
| `backup` | Backup engine. Snapshots MySQL, OTel config, and teamster state to timestamped directories. Runs on systemd timer. |
| `teamster` | Hook client + CLI. Forked per hook event; also the CLI entry point for status/tags/wms/sql. |

Configuration lives in `etc/teamster.yaml`. Systemd units and timers are in `etc/`.

## Directory layout

```
bin/          Compiled binaries (hookd, feed, teamster, MCPs, rollup, classify)
doc/          Specs and architecture docs
etc/          Config (teamster.yaml), systemd units, Grafana provisioning
lib/          Plugin, scripts, hook client
var/          Runtime data (events.jsonl, Grafana data, Prometheus data)
```

## Troubleshooting

**Services not running?**
Run `teamster status` to see health. Restart hookd with
`sudo systemctl restart teamster-hookd`.

**No events appearing?**
Check that the hook is configured in `~/.claude/settings.json` — look for
`teamster` in the `hooks` section. Verify hookd is reachable at its port.

**Dashboard empty?**
Confirm the Grafana datasource points at the correct MySQL instance. Run
`teamster status` to verify store connectivity.

**Cost not attributed?**
Make sure you call `wms_setFocus` at the start of each work session. Without
a focus interval, token spend lands in `unallocated`. Run `teamster wms drain`
to close stale intervals.

## Further reading

- `doc/specs/architecture.md` — full system architecture, data flows, env vars
- `doc/specs/semantic-conventions.md` — JSONL fields, tag taxonomy, WMS entities
- `doc/specs/wms-dashboard-spec.md` — dashboard design and implemented pages
- `lib/plugin/skills/` — available `/teamster:*` slash commands
