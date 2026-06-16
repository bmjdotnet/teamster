# Quickstart — from a fresh clone to a running dashboard

This walks you from nothing to a live Teamster install with an activity feed and
web dashboard, on a single host (the **hub**). It takes about five minutes plus
build time.

For multi-host setups — one hub plus many lightweight remote clients — finish
this guide first, then see [specs/REMOTE-INSTALL.md](specs/REMOTE-INSTALL.md).

---

## 1. Prerequisites

You need a Linux host with:

- **Go 1.25+** — builds the binaries (`go version` to check).
- **MySQL or MariaDB** — backs the work-management store. A local instance is
  easiest; you'll supply a DSN during install.
- **Claude Code CLI** (`claude`) — Teamster wires itself into Claude Code's
  hooks and MCP configuration.

Teamster installs everything it needs into `~/teamster/`. A hub install touches
your system in exactly three places: the `~/teamster/` base directory, your
Claude Code config (`~/.claude/`), and — in the default systemd supervision
mode — a single systemd unit at `/etc/systemd/system/teamster-hookd.service`,
which the installer writes via `sudo` and tells you about. Choosing
`supervisor` mode (for hosts without systemd) skips the unit and keeps every
change inside your home directory.

---

## 2. Clone and install

```bash
git clone https://github.com/bmjdotnet/teamster.git && cd teamster
```

Run the installer:

```bash
./install.sh
```

With no flags, `install.sh` runs as a **guided installer** — it probes your
host for existing services, port conflicts, and prior installs, then walks you
through every choice: install mode (hub vs client), base directory, how to
supervise the event server, whether to install or reuse monitoring services
(otelcol, Prometheus, Grafana), and your MySQL/MariaDB DSN. See
[wizard.md](wizard.md) for a field-by-field reference.

The installer compiles the binaries, copies them into `~/teamster/bin/`,
materializes a systemd unit for the event server, and merges the necessary
hooks, environment, MCP servers, and plugin registration into your Claude Code
config. It is idempotent — re-running it upgrades in place.

---

## 3. Start the services

```bash
teamster start
```

Then confirm everything is up:

```bash
teamster status
```

You'll see a table with one row per service (event server, store, and any
monitoring components you enabled), each showing its status, mode, and endpoint.

To check what version you installed:

```bash
teamster version
# teamster 0.1.0 (a1b2c3d, 2026-06-09T19:13:54Z)
```

---

## 4. Configure your tag vocabulary

Tags drive Teamster's cost attribution and dashboard drill-downs. Run the guided
setup once after installing:

```bash
teamster setup tags
```

On first run this opens an eight-screen interview: pick which external systems
you integrate with (GitHub, Jira, and so on), name your products, and review the
context and lifecycle keys that get seeded. On later runs the same command opens
a three-column editor. Full walkthrough in [wizard.md](wizard.md) (the
installer prompts and tag setup sections).

You can also manage tags non-interactively:

```bash
teamster tags add-key component --category context --cardinality single
teamster tags add-value product:myproduct
```

---

## 5. Watch the activity stream

Open a second terminal and run the live feed:

```bash
~/teamster/bin/feed
```

Every read, edit, command, thought, and completion from every agent in your
sessions streams here, colorized by entity and tagged by activity type. Leave it
running while you work.

The same data is available in the browser:

- `http://localhost:9125/` — live activity stream
- `http://localhost:9125/wms` — work-management hierarchy
- `http://localhost:9125/wms/tags` — tag vocabulary browser
- `http://localhost:9125/wms/cost-flow` — cost-flow Sankey diagram

If you enabled Grafana during install, the installer also provisions the
**Entity Cost Explorer** and **Tag Stack Explorer** dashboards.

---

## 6. Start your first session

Open a Claude Code session in your project and run the front-door skill:

```
/teamster:start
```

It interviews you about your objective, recommends a full **team** or a single
**subagent** based on the shape of the work, and on your confirmation sets up the
right mode. From there the lead decomposes the work, spawns domain-named
teammates, and tracks everything as work-management entities — all of which you
can watch in the feed and dashboard.

That's the full loop: a team of agents doing work, with every action visible in
real time and every unit of work tracked.

---

## 7. Attribution sweep (optional)

The installer sets up a systemd timer that runs `rollup --sweep` hourly
(15 minutes after boot, then every hour). This chains all deterministic
recovery passes — warmup attribution, gap recovery, transcript-focus
recovery — into a recurring deep-clean. To also enable LLM-assisted
synthesis for orphan sessions (requires `ANTHROPIC_API_KEY`):

```bash
rollup --sweep --sweep-llm
```

You can preview what would change without writing:

```bash
rollup --sweep --dry-run
```

---

## Where to go next

- [wizard.md](wizard.md) — every `install.sh` prompt and tag-setup screen explained.
- [terminology.md](terminology.md) — glossary of Teamster terms, WMS concepts,
  and cost attribution methods.
- [specs/REMOTE-INSTALL.md](specs/REMOTE-INSTALL.md) — add lightweight remote
  clients that report to this hub.
- [../README.md](../README.md) — feature overview, CLI reference, slash commands.
- `../skel/doc/specs/architecture.md` — how the pieces fit together (after
  installation, find it at `~/teamster/doc/specs/architecture.md`).
