# teamster clone — disposable instances on a remote host

`teamster clone` stands up a full, disposable Teamster instance — running the
same commit and a copy of the same data as the source — on a fresh target
host reached over SSH. It runs from the source instance and pushes outward:
it ships verified source code, translates the source's topology into an
installer flag set, invokes the installer on the target, then transfers and
restores a snapshot of the source's data. The source is never installed on,
migrated, or mutated (beyond, optionally, triggering a backup — see
`--fresh-backup` below).

Typical uses: a scratch environment to reproduce a bug against real data, a
demo host, or a one-off review copy — anywhere you want "the real thing" on
another box without touching the source.

## Prerequisites

- **SSH access to the target** (`<user>@<host>`). The whole flow is a series
  of SSH round trips with no interactive prompts on the target side, so
  key-based auth is strongly recommended.
- **sudo on the target.** The installer needs it for package installs and
  systemd unit management, and clone itself uses `sudo systemctl mask` to
  permanently disable the sweep/backup/review-sweep/mcp-scraper timers on
  the clone (see below).
- **The source must be a working Teamster install** — `teamster clone` reads
  the local `teamster.yaml` to resolve topology and reads an existing backup
  snapshot to seed the target's data (or pass `--fresh-backup` to create one
  on the spot).
- **The target should be a fresh host.** `teamster clone` refuses to proceed
  if it detects an existing Teamster remote client on the target already
  pointed at a different hub — it will not silently repoint a real remote
  install at a disposable clone.

## Usage

```bash
# Basic: ship from a local repo/worktree, install the full managed stack,
# restore a copy of the source's data
teamster clone --repo-dir=/path/to/repo user@target-vm

# Preview the full plan (ref resolution, working-tree check, topology
# translation) without touching the target
teamster clone --repo-dir=/path/to/repo --dry-run user@target-vm

# Fetch source from GitHub instead of a local checkout
teamster clone --github-repo=https://github.com/bmjdotnet/teamster user@target-vm

# Trigger a fresh backup on the source instead of pinning the existing
# 'latest' snapshot
teamster clone --repo-dir=/path/to/repo --fresh-backup user@target-vm
```

`--repo-dir` (default mode) and `--github-repo` are mutually exclusive code
sources. `--source` overrides which running instance's data/topology gets
cloned (default: the local instance); `--ref` overrides the detected commit.
`--allow-dirty` permits shipping a dirty working tree, which is refused by
default. See `teamster clone --help` for the full flag reference.

## What the target gets

- The **full managed stack**: MySQL **8.x** (not MariaDB — clones always get
  `--store-engine=mysql-8.4`), Grafana, Prometheus, and the OTEL collector,
  all installed and wired under systemd exactly as a fresh hub install would
  wire them. The clone's env label defaults to `clone` so its data is never
  confused with the source's `production` data in Grafana.
- `teamster-sweep.timer`, `teamster-backup.timer`,
  `teamster-wms-review-sweep.timer`, and `teamster-mcp-scraper.timer`
  **masked permanently** (not just stopped) — a disposable clone must never
  quietly run hourly LLM-assisted sweeps against your API budget, take its
  own backups, autonomously park/abandon entities in its copy of your
  production-shaped data, or ledger its own local MCP traffic into
  `mcp_tool_calls` as if it were the hub's. That last one is a
  measurement-integrity concern rather than cost or data loss: a clone that
  scrapes its own `events.jsonl` into the same ledger table would corrupt
  the call-volume figure that table exists to support.
- **The mask survives later upgrades.** Re-running the installer against
  the clone target checks every systemd unit for an existing mask first
  and leaves a masked one masked — printing `left masked: <unit>` instead
  of reinstalling over the `/dev/null` symlink. The guard is universal,
  not scoped to just these four timers: it honors any masked unit on any
  host, including one an operator masked by hand.
- A **restored copy of the source's data**, transferred from the source's
  backup snapshot and verified via row counts after restore (advisory —
  drift against a live source is expected and does not fail the run).
- A content-manifest fingerprint check confirms exactly what was shipped
  matches what lands on the target before the installer ever runs.

## What it does not do

- **No replication setup.** A clone is a one-time, standalone copy — it does
  not stay in sync with the source afterward. For a live-syncing read-only
  standby instead, see [Replication](../README.md#replication).
- **No teardown/destroy command.** There's no `teamster clone rm` — removing
  a clone is a manual operation on the target host.
- **Does not repoint your tooling.** `TEAMSTER_HOOK_SERVER_URL` and the MCP
  endpoints on the source host keep pointing at the source after a clone
  completes; point a session at the clone deliberately if you want to work
  against it.

## See also

- [README.md](../README.md) — quick-start usage
- [Backup and restore](../README.md#backup-and-restore) — the snapshot
  mechanism clone's data transfer builds on
- [Replication](../README.md#replication) — for a live-syncing standby
  instead of a one-time copy
