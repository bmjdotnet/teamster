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
mode — one systemd unit per managed daemon under `/etc/systemd/system/`
(`teamster-hookd.service`, plus `teamster-otelcol`/`-prometheus`/`-grafana`
for whichever monitoring components you choose to install), which the
installer writes via `sudo` and tells you about. Choosing `supervisor` mode
(for hosts without systemd) skips all of these units and keeps every change
inside your home directory.

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

Each monitoring component you choose to install gets its own systemd unit
and survives a reboot on its own — no separate `teamster start` needed after
one. Masking one (`sudo systemctl mask teamster-prometheus.service`) takes
it fully offline with no fallback to a supervisor-managed process; see
[specs/architecture.md](specs/architecture.md) for the masking and
downgrade details before you touch any of these units by hand.

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

## 8. Nightly review sweep (optional)

Distinct from the attribution sweep above and named unfortunately
similarly: `teamster wms review-sweep`, run nightly by
`teamster-wms-review-sweep.timer`, closes WMS entities nobody has looked at
in a long time — a WorkUnit resting in `review`, or an Outcome with no live
work left under it. It is off by default (`ReviewSweep.Enabled: false` in
`teamster.yaml`) and, once enabled, still only previews what it would do
until you explicitly confirm it (`ReviewSweep.Confirm: false`).

Every close is deliberately reversible first: a stale entity is parked to
`on_hold`, not abandoned outright, and stays there — visible, and
reversible through the ordinary status tools with no SQL — for a second,
longer window before the sweep will abandon it. It never touches an
`on_hold` a human set by choice, at any age.

**Before enabling it on an existing hub**, review your current backlog of
long-stale `review`-state WorkUnits and idle Outcomes by hand first — the
population this sweep acts on can be large on a hub that predates it, and a
first look before automating closes over it is worth the few minutes.

This project's own operators wrote and reviewed a runbook for exactly that
kind of one-time drain (kept outside this repo) — the specific queries, a
descendant-safety walk, and the order to run it in; run a drain like it
before you flip `confirm: true` in step 3 below, not before enabling the
sweep itself.

**Burn-in, the recommended order:**

1. Edit `~/teamster/etc/teamster.yaml`'s `review-sweep:` block and set
   `enabled: true`, leaving `confirm: false` (the default). **This one flip
   needs more than a config edit** — unlike the `Confirm` flip in step 3,
   the installer only registers this timer with systemd when `Enabled` was
   already `true` at install/upgrade time; a fresh install with the default
   `enabled: false` never runs `systemctl enable` on it, so there is no
   unit for `systemctl start` to find yet. Either re-run `./install.sh` (it
   picks the new value up and installs + enables the timer — since
   2f59a23, mask-aware: on a masked host it leaves the mask alone and
   installs nothing), or, to avoid a full reinstall, register the unit by
   hand — the `.service`/`.timer` files are already staged under
   `~/teamster/etc/` from your last install/upgrade regardless of
   `Enabled`, only not yet copied into systemd's unit directory. **Check
   for a mask first, every time:** `teamster clone` masks this exact timer
   permanently on every clone target (`systemctl mask`, a one-way
   defensive floor — see [clone.md](clone.md)), and `install -m 0644` over
   a masked unit silently replaces its `/dev/null` symlink with a real
   file, undoing the mask and re-arming the sweep that autonomously
   abandons entities. The commands below check for that themselves and do
   nothing if the unit is masked; do not skip that check to "just get it
   working" on a host you don't control:
   ```bash
   case "$(systemctl is-enabled teamster-wms-review-sweep.timer 2>/dev/null)" in
     masked*)
       echo "teamster-wms-review-sweep.timer is masked on this host — stop." >&2
       echo "If this is a clone target, the sweep must stay off. If you" >&2
       echo "masked it yourself, unmask deliberately first:" >&2
       echo "  sudo systemctl unmask teamster-wms-review-sweep.timer" >&2
       ;;
     *)
       sudo install -m 0644 ~/teamster/etc/teamster-wms-review-sweep.service /etc/systemd/system/
       sudo install -m 0644 ~/teamster/etc/teamster-wms-review-sweep.timer /etc/systemd/system/
       sudo systemctl daemon-reload
       sudo systemctl enable --now teamster-wms-review-sweep.timer
       ;;
   esac
   ```
2. Watch the nightly dry-run listing for a few nights:
   ```bash
   tail -f ~/teamster/var/review-sweep.log
   ```
3. Once the listing looks right, set `confirm: true` in the same block and
   save — **no reinstall this time**, no restart either: the command
   re-reads `teamster.yaml` at the start of every run, so the next
   scheduled run just picks the new value up.

See `~/teamster/doc/specs/semantic-conventions.md` §4.8 for the full
disposition-rule table and `architecture.md`'s Configuration table for
every `review-sweep:` key, its default, and its env-var override.

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
