#!/usr/bin/env bash
set -euo pipefail

SERVER_URL="${1:-${REPL_SERVER_URL:?REPL_SERVER_URL must be set (e.g. http://hub:9177)}}"
DUMP_FILE="/tmp/teamster-repl-dump.sql"
POSITION_FILE="/tmp/teamster-repl-position.txt"
REPL_PASSWORD="${REPL_PASSWORD:?REPL_PASSWORD must be set}"
REPL_USER="${REPL_USER:-repl}"
REPL_MASTER_HOST="${REPL_MASTER_HOST:?REPL_MASTER_HOST must be set}"

log() { echo "$(date -Iseconds) $*"; }

# check if replication is already running
io_running=$(mariadb -e "SHOW SLAVE STATUS\G" 2>/dev/null | grep -oP 'Slave_IO_Running: \K\w+' || echo "No")
if [[ "$io_running" == "Yes" ]]; then
    log "replication already running, nothing to do"
    exit 0
fi

# trigger the push with retries
log "requesting push from $SERVER_URL"
pushed=false
for attempt in 1 2 3; do
    log "attempt $attempt/3"
    status=$(curl -sf -o /dev/null -w '%{http_code}' "${SERVER_URL}/push" 2>/dev/null || echo "000")
    if [[ "$status" == "200" ]]; then
        pushed=true
        break
    fi
    log "push request returned $status, retrying in 10s"
    sleep 10
done

if [[ "$pushed" != "true" ]]; then
    log "ERROR: push request failed after 3 attempts"
    exit 1
fi

# wait for dump files to arrive via scp
log "waiting for dump files"
elapsed=0
while [[ ! -f "$DUMP_FILE" || ! -f "$POSITION_FILE" ]]; do
    if [[ $elapsed -ge 120 ]]; then
        log "ERROR: dump files not received within 120s"
        exit 1
    fi
    sleep 2
    elapsed=$((elapsed + 2))
done
log "dump files received after ${elapsed}s"

# read binlog position
source "$POSITION_FILE"
if [[ -z "${BINLOG_FILE:-}" || -z "${BINLOG_POS:-}" ]]; then
    log "ERROR: binlog position missing from $POSITION_FILE"
    exit 1
fi
log "binlog: file=$BINLOG_FILE pos=$BINLOG_POS"

# load the dump
log "loading dump"
mariadb < "$DUMP_FILE"

# MariaDB 11.x defaults collation_server to utf8mb4_uca1400_ai_ci, but all
# Teamster tables use utf8mb4_general_ci (set by migrations, and the dump's
# collations are rewritten by repl-push-server). Fix the database default and
# force connection collation so Grafana queries don't hit ERROR 1267.
log "fixing database collation"
mariadb <<SQL2
ALTER DATABASE teamster COLLATE utf8mb4_general_ci;
SET GLOBAL init_connect = 'SET collation_connection = utf8mb4_general_ci, NAMES utf8mb4 COLLATE utf8mb4_general_ci';
SQL2

# configure and start replication
# IDEMPOTENT mode: the dump and replication start are not atomic — rows written
# between the dump's binlog snapshot and START SLAVE appear in both the dump and
# the binlog, causing 1062 dup-key errors. IDEMPOTENT makes INSERT replay into
# REPLACE (dup → silent update) and DELETE of missing rows into no-op. Restored
# to STRICT after catch-up. Same variable on MySQL 8.0 and MariaDB 11.x.
log "configuring replication (idempotent mode for bootstrap)"
mariadb -e "SET GLOBAL slave_exec_mode='IDEMPOTENT';"
mariadb <<SQL
STOP SLAVE;
CHANGE MASTER TO
    MASTER_HOST='${REPL_MASTER_HOST}',
    MASTER_USER='${REPL_USER}',
    MASTER_PASSWORD='${REPL_PASSWORD}',
    MASTER_LOG_FILE='${BINLOG_FILE}',
    MASTER_LOG_POS=${BINLOG_POS},
    MASTER_SSL=0;
START SLAVE;
SQL

# verify replication status (retry up to 30s — Pi can be slow to connect)
for i in $(seq 1 10); do
    sleep 3
    io_running=$(mariadb -e "SHOW SLAVE STATUS\G" | grep -oP 'Slave_IO_Running: \K\w+' || echo "No")
    sql_running=$(mariadb -e "SHOW SLAVE STATUS\G" | grep -oP 'Slave_SQL_Running: \K\w+' || echo "No")
    if [[ "$io_running" == "Yes" && "$sql_running" == "Yes" ]]; then
        log "replication running (IO=$io_running SQL=$sql_running)"
        break
    fi
    log "waiting for replication (attempt $i/10, IO=$io_running SQL=$sql_running)"
done

if [[ "$io_running" != "Yes" || "$sql_running" != "Yes" ]]; then
    log "ERROR: replication not running after 30s (IO=$io_running SQL=$sql_running)"
    mariadb -e "SHOW SLAVE STATUS\G" | grep -E '(Running|Error|Behind)' || true
    mariadb -e "SET GLOBAL slave_exec_mode='STRICT';"
    exit 1
fi

# restore strict mode now that bootstrap catch-up is complete
log "restoring strict replication mode"
mariadb -e "SET GLOBAL slave_exec_mode='STRICT';"

# cleanup
rm -f "$DUMP_FILE" "$POSITION_FILE"
log "bootstrap complete"
