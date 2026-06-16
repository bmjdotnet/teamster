# Replication

Teamster supports replicating data from a hub to one or more read-only
replica hosts. This enables DR/standby, staging mirrors, stakeholder
dashboards, and public-facing demo instances.

A replica is a third topology alongside the single-host and hub/remote
models (see `skel/doc/specs/architecture.md`). A **remote** runs only the
hook client and feeds events *into* a hub. A **replica** runs a full
read-only stack — hookd, MySQL, Prometheus, Grafana — and mirrors a hub's
data *out* for read-only consumption. The hub pushes; the replica never
initiates a connection back, so even a fully compromised replica cannot
write to the hub or reach the internal network.

## Architecture

```
┌─────────────── hub (internal) ────────────────────────────┐
│                                                            │
│  hookd :9125  ──→ events.jsonl                            │
│  MySQL :3306  (WMS, cost attribution)                     │
│  Prometheus, Grafana, MCPs, timers                        │
│                                                            │
│  relay              tails events.jsonl, POSTs each        │
│                     line to the replica hookd /event      │
│  repl-push-server   socat HTTP server; on /push runs      │
│                     mysqldump + SCPs to the replica       │
│                                                            │
└──────────────────── push-only to replica ─────────────────┘
              │ relay: POST /event (JSONL events)
              │ repl-push: SCP dump + binlog position
              │ MySQL replication: replica → hub:3306 (read-only user)
              ▼
┌─────────────── replica (DMZ / public-facing) ─────────────┐
│                                                            │
│  hookd :9125  (--read-only: accepts /event, serves all    │
│               GET routes, rejects MCP/telemetry/drain)    │
│  MySQL :3306  (read-only replica of the hub's teamster db)│
│  Prometheus   (scrapes local hookd /metrics)              │
│  Grafana      (anonymous Viewer; datasources → local      │
│               MySQL replica + local Prometheus)           │
│                                                            │
│  NO: activity-mcp, wms-mcp, rollup, classify, sweep,     │
│      token-scraper, feed, teamster CLI                    │
└────────────────────────────────────────────────────────────┘
```

Two independent data planes flow from hub to replica:

1. **Live event stream** — the `relay` binary tails the hub's
   `events.jsonl` and POSTs each line to the replica's hookd `/event`
   endpoint. This drives the SSE activity dashboard and the replica's own
   JSONL/feed.
2. **MySQL data** (WMS, cost attribution, tags) — the `repl-push` pipeline
   uses `mysqldump` + SCP to bootstrap the replica's database, then native
   MySQL/MariaDB replication keeps it in sync. This drives the WMS pages,
   cost-flow, tag browser, Grafana MySQL panels, and `/metrics`.

Prometheus data needs no special pipeline: the replica's Prometheus scrapes
the replica's own hookd `/metrics`, which is computed from the replicated
MySQL.

## Components

### relay (Go binary)

`cmd/relay/` — a standalone forwarder built by the installer alongside the
other binaries. It tails a JSONL file and POSTs each non-empty line to a
remote hookd's `POST /event`.

| Flag | Default | Purpose |
|------|---------|---------|
| `--source` | `$TEAMSTER_DATA_DIR/events.jsonl`, else `$TEAMSTER_BASEDIR/var/events.jsonl` | JSONL file to tail |
| `--target` | (required) | Destination hookd URL, e.g. `http://replica:9125/event` |
| `--history` | `0` | Replay this many trailing lines on start (0 = tail from end) |
| `--version` | | Print version and exit |

Behavior:

- Reads in 8 KB chunks, tracks a byte offset, polls every 100 ms when at the
  end of the file. New lines appended by hookd are picked up and forwarded.
- On a failed POST it backs off linearly (500 ms × consecutive failures,
  capped at 30 s) and retries the same line — events are not dropped on a
  transient replica outage.
- A 5 s per-request HTTP timeout. Logs progress every 100 forwarded events.
- Runs on the **hub** (it reads the hub's JSONL). It does not run on the
  replica.

### repl-push-server.sh (hub-side)

`skel/lib/scripts/repl-push-server.sh` — a `socat`-based HTTP server on the
hub (default port 9177). On request it dumps the hub's database and pushes
it to the replica.

Endpoints:

- `GET /health` → `{"status":"ok"}`
- `GET /push` → runs `mysqldump --single-transaction --source-data=2
  --databases teamster`, rewrites `utf8mb4_0900_ai_ci` collations to
  `utf8mb4_general_ci` (MySQL 8.0 hub → MariaDB replica compatibility),
  extracts the binlog file/position from the dump, and SCPs both the dump
  and a position file to `/tmp/` on the replica. Returns
  `{"status":"pushed","binlog_file":...,"binlog_pos":...}`.

Environment variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `REPL_PUSH_PORT` | `9177` | Listen port |
| `REPL_PUSH_REMOTE` | (required) | SCP destination, `user@replica-host`. No default — the server refuses to start without it. |

### repl-push-client.sh (replica-side)

`skel/lib/scripts/repl-push-client.sh` — a one-shot bootstrap run on the
replica. It is **not** a long-running service. It:

1. Exits early if replication is already running (`Slave_IO_Running: Yes`).
2. Requests a push from the hub's repl-push-server (`GET /push`, 3 retries).
3. Waits up to 120 s for the dump + position file to arrive via SCP.
4. Loads the dump, fixes the MariaDB 11.x database/connection collation to
   `utf8mb4_general_ci` (Teamster tables are all `utf8mb4_general_ci`;
   MariaDB 11.x otherwise defaults to `utf8mb4_uca1400_ai_ci`, which causes
   Grafana `ERROR 1267` collation mismatches).
5. Issues `CHANGE MASTER TO` with the dump's binlog coordinates and
   `START SLAVE`, then verifies `Slave_IO_Running`/`Slave_SQL_Running` for
   up to 30 s.

Arguments and environment variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `$1` / `REPL_SERVER_URL` | (required) | Hub repl-push-server URL, e.g. `http://hub:9177` |
| `REPL_PASSWORD` | (required) | Replication account password. Env-only — no fallback. |
| `REPL_USER` | `repl` | Replication account user |
| `REPL_MASTER_HOST` | (required) | Hub hostname the replica connects to for replication |

### hookd --read-only

hookd supports a read-only mode for replica deployments. Install it with the
installer's `--hookd-read-only` flag (which materializes
`TEAMSTER_HOOKD_READ_ONLY=1` into the hookd service unit); hookd also honors
the `--read-only` CLI flag or `TEAMSTER_HOOKD_READ_ONLY=1` directly
(`src/internal/config/config.go`). When enabled:

- `POST /event` is still accepted — the relay needs it.
- All GET routes (dashboard, WMS pages, cost-flow, tags, SSE, `/metrics`,
  `/health`) work normally.
- Write/control endpoints — the MCP endpoints (`/mcp/activity`, `/mcp/wms`),
  `/telemetry`, `/focus-timeline`, and `/wms/api/drain` — return 403.

This is defense-in-depth: a compromised replica cannot be used to inject MCP
calls, telemetry, or drain operations against the mirrored data.

## Installation

### Hub setup

The relay and repl-push services are installed on the hub via
`--relay-mode=install`. This requires a relay target (the replica's hookd
`/event` URL) and a repl-push SCP destination (`user@replica`):

```bash
./install.sh   # the guided installer asks whether to set up a relay

# or non-interactively:
./lib/installrunner.sh --relay-mode=install \
    --relay-target=http://replica:9125/event \
    --repl-push-remote=user@replica
```

`--relay-mode=none` (the default) installs nothing replication-related.

The installer materializes both service templates into `$BASEDIR/etc/` and,
when `--wire` is set, installs and enables them under
`/etc/systemd/system/`. The replication account itself (the MySQL user the
replica uses for `REPLICATION SLAVE`) and any firewall rules are operator
responsibilities — Teamster does not provision them.

### Replica setup

The replica runs a standard hub-style install with hookd in read-only mode,
then bootstraps its database from the hub:

1. Install hookd in read-only mode by passing `--hookd-read-only` to the
   installer:

   ```bash
   ./lib/installrunner.sh --hookd-read-only --wire
   ```

   This materializes `Environment="TEAMSTER_HOOKD_READ_ONLY=1"` into the
   hookd service unit — no manual systemd editing required. Read-only mode
   accepts `/event` POSTs (the relay needs them) but rejects the MCP,
   telemetry, and drain endpoints. Point `TEAMSTER_STORE_DSN` at the local
   replica database.
2. Bootstrap the database by running `repl-push-client.sh` with
   `REPL_SERVER_URL`, `REPL_PASSWORD`, `REPL_USER`, and `REPL_MASTER_HOST`
   set. This pulls the dump and starts MySQL replication.

   **Create-then-overwrite:** install the replica with `--store-mode=install`
   so the installer creates the `teamster` database and runs migrations to
   establish a valid, owned schema. `repl-push-client.sh` then loads the
   hub's `mysqldump` over that freshly-created schema, replacing it wholesale
   with the hub's data and binlog position. This overwrite is expected — the
   install pass exists to provision the database and account; the dump
   supplies the actual content. Do **not** use `--store-mode=managed` on a
   replica: managed mode assumes a pre-existing, app-owned schema it must not
   recreate, which conflicts with the dump-driven bootstrap.
3. Run Prometheus on the replica, scraping the local hookd `/metrics`
   (see `skel/etc/teamster-prometheus-replica.yml.tmpl`).
4. Run Grafana with anonymous Viewer access (see
   `skel/etc/grafana-anonymous.ini`), datasources pointed at the local
   MySQL replica and the local Prometheus, and the Teamster dashboards
   provisioned from `skel/etc/grafana/`.

### Service templates

| Template | Installs as | Runs on |
|----------|-------------|---------|
| `skel/etc/teamster-relay.service.tmpl` | `teamster-relay.service` | hub |
| `skel/etc/teamster-repl-push.service.tmpl` | `teamster-repl-push.service` | hub |

The relay unit is `After=teamster-hookd.service` (it reads hookd's JSONL).
The repl-push unit is `After=network.target mysql.service` (it runs
`mysqldump`). Both restart on failure.

Replica-side config assets (not systemd units):

| File | Purpose |
|------|---------|
| `skel/etc/teamster-prometheus-replica.yml.tmpl` | Prometheus scrape config — scrapes the local hookd `/metrics` (`__HOOK_SERVER_PORT__` marker) |
| `skel/etc/grafana-anonymous.ini` | Grafana anonymous Viewer + embedding config for a public replica |

### Environment variables

| Variable | Side | Purpose |
|----------|------|---------|
| `TEAMSTER_HOOKD_READ_ONLY` | replica | `1` puts hookd in read-only mode. Set automatically by the installer's `--hookd-read-only` flag; or pass `--read-only` to hookd directly. |
| `REPL_PUSH_PORT` | hub | repl-push-server listen port (default 9177) |
| `REPL_PUSH_REMOTE` | hub | SCP destination `user@replica-host` (required) |
| `REPL_SERVER_URL` | replica | Hub repl-push-server URL, e.g. `http://hub:9177` |
| `REPL_PASSWORD` | replica | Replication account password (required, env-only) |
| `REPL_USER` | replica | Replication account user (default `repl`) |
| `REPL_MASTER_HOST` | replica | Hub host the replica replicates from (required) |

### Temporary handshake files

The repl-push bootstrap coordinates through two fixed `/tmp` paths. The
hub-side server writes them locally, SCPs them to the same paths on the
replica, and the replica-side client reads them from there. Both sides
delete them when done; the paths must match on both hosts.

| Path | Written by | Read by | Contents |
|------|-----------|---------|----------|
| `/tmp/teamster-repl-dump.sql` | `repl-push-server.sh` (hub) | `repl-push-client.sh` (replica) | `mysqldump` of the `teamster` database, collations rewritten to `utf8mb4_general_ci` |
| `/tmp/teamster-repl-position.txt` | `repl-push-server.sh` (hub) | `repl-push-client.sh` (replica) | `BINLOG_FILE` and `BINLOG_POS` for the `CHANGE MASTER TO` coordinates |

## Configuration reference

### relay flags

`--source`, `--target` (required), `--history`, `--version` — see the relay
component table above.

### Installer flags

| Flag | Modes / value | Purpose |
|------|---------------|---------|
| `--relay-mode` | `none` (default), `install` | Whether to build + install relay and repl-push services on the hub |
| `--relay-target` | URL | Replica hookd `/event` URL; required when `--relay-mode=install` |
| `--repl-push-remote` | `user@host` | SCP destination for repl-push; required when `--relay-mode=install` |
| `--hookd-read-only` | (boolean) | Replica side: materialize `TEAMSTER_HOOKD_READ_ONLY=1` into the hookd unit so hookd rejects MCP/telemetry/drain |

### Service template markers

| Template | Markers |
|----------|---------|
| `teamster-relay.service.tmpl` | `__BASEDIR__`, `__USER__`, `__SOURCE_JSONL__`, `__TARGET_URL__` |
| `teamster-repl-push.service.tmpl` | `__BASEDIR__`, `__USER__`, `__REPL_PUSH_REMOTE__` |
| `teamster-prometheus-replica.yml.tmpl` | `__HOOK_SERVER_PORT__` |

The installer replaces relay/repl-push markers directly (these templates
carry relay-specific markers that `teamster-install` does not handle):
`__SOURCE_JSONL__` defaults to `$BASEDIR/var/events.jsonl`, `__TARGET_URL__`
comes from `--relay-target`, `__REPL_PUSH_REMOTE__` from `--repl-push-remote`.
