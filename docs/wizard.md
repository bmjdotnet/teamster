# Interactive Setup Guides

Teamster ships two interactive guided interfaces: `install.sh` for installation,
and `teamster setup tags` for tag vocabulary configuration.

---

# install.sh — Interactive Installer

`install.sh` is the main entry point for installing Teamster. It probes the
host for prior installs, port conflicts, and stale config, walks through all
install choices interactively, then calls the build backend
(`lib/installrunner.sh`) to compile and install.

**Linux only.** `install.sh` (and `lib/installrunner.sh`) hard-fail on macOS:
the hub requires apt-based Linux. macOS is supported as a **remote only** — do
not run this installer on the Mac. To enroll a Mac, run
`teamster install-remote <user>@<mac>` from your Linux hub over SSH. See
[specs/REMOTE-INSTALL.md](specs/REMOTE-INSTALL.md).

## Guided mode vs flags

- Run `./install.sh` with no flags for a guided first install or when you
  want guidance on service mode decisions.
- Pass flags directly (e.g. `./install.sh --basedir=PATH --wire`) when you
  know exactly what you want, are re-running an existing install, or are
  scripting from CI.

## Pre-install probes

Before asking any questions the wizard runs read-only probes:

- **settings.json** — reads existing `TEAMSTER_STORE_DSN`, `TEAMSTER_HOOK_SERVER_URL`,
  `OTEL_EXPORTER_OTLP_ENDPOINT`, and port env vars from `~/.claude/settings.json`.
- **teamster.yaml** — reads `~/teamster/etc/teamster.yaml` (if present) to seed
  service mode and endpoint defaults from the prior install. On upgrade re-runs this
  prevents the wizard from losing config that was not written to settings.json
  (e.g. external-mode service ports). The hostname in the yaml's `health:` URLs is
  authoritative for endpoint defaults: it overrides the `localhost` URLs the
  systemd/TCP probes produce, so an external service that happens to run on this
  same host keeps its configured hostname across upgrades instead of being
  re-offered as `localhost`.
- **Port scan** — checks which Teamster ports are bound (`ss`).
- **TCP probes** — detects Prometheus (`:9090`), Grafana (`:3000`), and otelcol
  (`:4317`/`:4318`) by attempting a TCP connection.
- **systemd** — checks `systemctl is-active` for `prometheus`, `grafana-server`,
  `mysql`/`mysqld`/`mariadb`, and existing `teamster-*` units.

## What install.sh asks

The installer runs in two phases: pre-install probes (no user input), then an
interview:

**Q1 — Install mode**
- Hub: this host runs `hookd` locally (default)
- Client: this host points at an existing hub elsewhere (supply `host:port`)

**Q2 — Base directory** (`~/teamster` default)

**Q3 — hookd supervision mode** (hub only)
- `systemd` (default for hub installs)
- `supervisor` (for containers / hosts without systemd)

**Q4–Q6 — Per-service decisions** for `otelcol`, `prometheus`, `grafana`:
- If a server is already detected (from yaml, settings.json, systemd, or TCP probe):
  managed / install / external / none
- If not detected: install / external / managed / none

**Q7 — Store backend**: MySQL is the only supported backend. The wizard probes for a
running MySQL/MariaDB instance and, if found, offers managed (default) or fresh
install. If not found, offers install / external / managed. Prompts for the DSN
(`mysql://user:pass@host:port/db`).

**Q8 — Environment label** (`production` default)

**Q9 — Plane integration** (opt-in; prompts for URL + API key)

**Q10 — Prometheus retention** (only asked if prometheus is `install`; `365d` default)

**Q11–Q13 — Build from source?** (only for `install`-mode otelcol / prometheus / grafana; default N)

**Q14 — Auto-start?** Run `teamster start` after install (default Y,
only asked when at least one component is bundled)

**Replication relay** (hub installs only, opt-in, default N) — the wizard asks
whether to set up event relay to a read-only replica host. If yes, it collects
the replica hookd URL (`--relay-target`, e.g. `http://replica:9125/event`) and
the SCP destination for repl-push (`--repl-push-remote`, e.g. `user@replica`),
and emits `--relay-mode=install`. See
[specs/replication.md](specs/replication.md).

**Replica mode** (opt-in, default N) — the wizard asks whether this host is a
read-only replica that receives data from a hub. If yes, it emits
`--hookd-read-only`, which makes hookd reject MCP/telemetry/drain endpoints
while still serving reads and `/event`. See
[specs/replication.md](specs/replication.md).

## What flags it passes to lib/installrunner.sh

The installer passes flags to `lib/installrunner.sh` derived from your
answers:

| Answer | Flag emitted |
|--------|-------------|
| Hub mode | `--hookd-mode=systemd` (or `supervisor`) |
| Base dir | `--basedir=PATH` |
| MySQL DSN | `--store-dsn=mysql://user:pass@host:port/db` |
| Store mode | `--store-mode=install\|managed\|external` |
| otelcol mode | `--otelcol-mode=install\|external\|managed\|none` |
| otelcol URL | `--otelcol-endpoint=URL` |
| Prometheus mode | `--prometheus-mode=install\|external\|managed\|none` |
| Prometheus URL | `--prometheus-endpoint=URL` |
| Grafana mode | `--grafana-mode=install\|external\|managed\|none` |
| Grafana URL | `--grafana-endpoint=URL` |
| Client mode hub URL | `--hookd-mode=external --hookd-endpoint=URL` |
| Env label | `--env=LABEL` |
| Prometheus retention | `--prometheus-retention=365d` |
| Prometheus retention size cap (optional, scripted install only) | `--prometheus-retention-size=SIZE` |
| Plane URL | `--plane-url=URL` |
| Plane API key | `--plane-api-key=KEY` |
| Relay (replica opt-in) | `--relay-mode=install` |
| Relay target URL | `--relay-target=URL` |
| Repl-push destination | `--repl-push-remote=user@host` |
| Replica mode (read-only hookd) | `--hookd-read-only` |
| Wire (always for real installs) | `--wire` |

## Key invariant

The interview phase of `install.sh` mutates NOTHING on the host. All changes
— `settings.json`, systemd units, binaries — go through `lib/installrunner.sh`.
The interview only inspects and guides; `--wire` is passed to
`lib/installrunner.sh` based on your choices, not applied by the interview
itself.

## Debug logging

```bash
./install.sh --debug-log=/tmp/install.log
```

Writes structured trace to the log file. `lib/installrunner.sh` gets a
sibling log automatically when `--debug-log` is set.

---

# teamster setup tags — Tag Vocabulary Setup

`teamster setup tags` is a bubbletea TUI for configuring the tag vocabulary
that drives Teamster's cost attribution, dashboards, and drill-down analysis.

## Two modes

On **first run** (no product values exist in the vocabulary), it launches
the guided interview wizard. On **subsequent runs**, it opens the 3-column
tag editor.

To re-run the interview after initial setup:

```bash
teamster setup tags --interview
```

## Interview wizard (8 screens)

The wizard walks through tag vocabulary setup in eight steps:

| Screen | Title | What it does |
|--------|-------|-------------|
| 1 | Welcome | Explains what tags are and why they matter |
| 2 | Select Integrations | Checkbox selection of external systems (GitHub, GitLab, Jira, Local Git, Redmine, OpenProject, Plane, Taiga) — seeds their tag keys |
| 3 | Universal Context Keys | Education screen showing the default context keys that will be seeded |
| 4 | Add Your Products | Text input for product slugs (the primary aggregation axis) |
| 5 | Universal Key Review | Browse seeded keys, toggle inclusion, view descriptions |
| 6 | Integration Keys Review | Review the keys seeded by your integration selections |
| 7 | Lifecycle Tags | Read-only orientation on engine-managed tags (work-type, phase, status) |
| 8 | Summary + Apply | Review all choices and commit to the database |

Navigate with arrow keys, Enter to proceed, Esc to go back.

### Default context keys

These are always seeded regardless of integration choices:

| Key | Description |
|-----|------------|
| `product` | The ongoing product or area of work. Primary aggregation axis. |
| `feature` | The specific feature being built. |
| `bug` | The specific bug being fixed. |
| `component` | Subsystem within a product (e.g. networking, harness, ui). |
| `priority` | Urgency: p0=critical, p1=high, p2=normal, p3=low. |
| `product-version` | Version or milestone being targeted. |

### Integration keys

Selecting an integration seeds its namespace of keys. For example, selecting
GitHub seeds: `github.owner`, `github.repo`, `github.pr`, `github.issue`,
`github.milestone`.

## Tag editor (3-column)

After initial setup, `teamster setup tags` opens the full-screen editor:

- **Left column** — tag key list (navigate with arrow keys)
- **Center column** — values for the selected key
- **Right column** — key metadata (category, cardinality, description)

The editor supports adding products, adding keys and values, retiring keys,
and re-running the interview.

## Related: teamster tags CLI

For scriptable (non-interactive) tag management, use `teamster tags`:

```bash
teamster tags list                           # list all keys
teamster tags list --key product             # list values for a key
teamster tags add-key mykey                  # add a new key
teamster tags add-key mykey --category context --cardinality single --description "..."
teamster tags add-value product:myproduct    # add a value
teamster tags add-value product:myproduct --description "..."
teamster tags retire mykey                   # demote a key (non-destructive)
teamster tags describe mykey "new desc"      # update key description
```

---

# Single-agent (subagent) mode — interview-driven selection

Subagent mode is a **runtime, per-session choice**, not an install option.
There is no installer flag and no install prompt for it: the same install
serves both team mode and subagent mode, and the installer writes nothing for
it. Mode selection happens through an **interview** at the start of each
session.

## Starting a session: /teamster:start

Run `/teamster:start` at the beginning of any session (team *or* subagent):

```
/teamster:start
```

The skill gathers your objective and context, recommends team vs subagent with a
one-line rationale, and on your confirmation calls
`mcp__activity__setMode("solo"|"team")`. The Teamster hook intercepts that MCP
call and writes a **per-session mode marker** (keyed on the session id) that the
hook reads on every subsequent event. The mode is sticky for the whole session —
no restart needed after the interview.

If you already know the mode, skip the interview and invoke directly:

```
/teamster:start       # recommends team or subagent based on your objective
/teamster:solo        # subagent — WMS bookkeeping, no team
```

Both named skills also call `setMode` on entry, so the marker is always set
regardless of how you launch.

## How the session mode marker works

The marker is a small file written by the hook (not by the skill) under a path
keyed on the Claude Code session id. Because the hook writes it using the
authoritative session id from the event payload, there is no concurrency race —
each session's marker is isolated. The hook refreshes the marker's mtime on
every event so it stays fresh as long as the session is active; if a session
crashes or is abandoned, the marker ages out after 12 hours and the next session
starts fresh (defaulting to team mode).

The effective mode for each event is:

1. `"solo"` marker present and fresh → subagent mode
2. `"team"` marker present and fresh → team mode (beats the env default)
3. No marker → `TEAMSTER_SOLO` env var (if set)
4. Neither → team mode (the enforcing default)

## Optional pre-launch default

If a project is *always* solo you can commit a one-line env to its
`.claude/settings.json` as a pre-seed:

```json
{ "env": { "TEAMSTER_SOLO": "1" } }
```

This acts as the fallback when no marker is present (case 3 above). The
`/teamster:start` interview can override it: choosing "Team" writes a `team`
marker that beats the env for the current session. Without running any
`/teamster:*` skill, the env var alone determines the mode (as before).

`TEAMSTER_SOLO` rides the same settings-precedence path Teamster already uses
for `TEAMSTER_HOOK_SERVER_URL` and `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS`:
Claude Code merges the project `.claude/settings.json` `env` block over
user-global `~/.claude/settings.json` and injects the result into the hook
subprocess environment. The env value is resolved once at session launch
(fixed-at-launch) and cannot be changed mid-session — use the marker (via the
interview) for in-session control.

## Scope and defaults

- **Default (no marker + no env)** = team-first behavior, byte-identical to
  before. Sessions that never run any `/teamster:*` skill are untouched.
- **Per-session**, not host-global: the marker is keyed on session id; a team
  session in one project does not affect a subagent session in another.
- A session that never runs `/teamster:start` or `/teamster:solo` stays in the
  enforcing default — team mode is never silently relaxed.

## What changes in subagent mode

| Behavior | Team mode (default) | Subagent mode |
|----------|---------------------|---------------|
| Bare `Agent` call | hard-blocked | allowed (subagents are the mechanism) |
| Team-mode nudge | injected when no team | suppressed |
| "MUST use Agent Teams" dispatch prose | injected | suppressed |
| Activity reporting | on | on (identical) |
| WMS / cost attribution / dashboards | on | on (identical) |
| Review/quality gate | separate reviewer teammate | fresh review subagent and/or `/code-review` |

Observability is not a team feature — WMS, cost attribution, the activity
stream, and the dashboards work the same in both modes.
