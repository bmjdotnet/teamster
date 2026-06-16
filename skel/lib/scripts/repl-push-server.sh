#!/usr/bin/env bash
set -euo pipefail

PORT="${REPL_PUSH_PORT:-9177}"
DUMP_FILE="/tmp/teamster-repl-dump.sql"
POSITION_FILE="/tmp/teamster-repl-position.txt"
REMOTE_HOST="${REPL_PUSH_REMOTE:?REPL_PUSH_REMOTE must be set (e.g. user@replica-host)}"

log() { echo "$(date -Iseconds) $*" >&2; }

preflight() {
    local binlog
    binlog=$(sudo mysql -N -B -e "SELECT @@log_bin" 2>/dev/null || echo "")
    if [[ "$binlog" != "1" ]]; then
        log "FATAL: binary logging is not enabled on the master (log_bin=${binlog:-unknown})"
        log "       enable log_bin and set a server_id, then restart mysql"
        exit 1
    fi

    if ! sudo -n mysqldump --version >/dev/null 2>&1; then
        log "FATAL: 'sudo mysqldump' is not available without a password"
        log "       grant passwordless sudo for mysqldump to this user"
        exit 1
    fi
}

do_push() {
    log "starting dump" >&2
    local data_flag="--master-data=2"
    if sudo mysqldump --help 2>&1 | grep -q -- '--source-data'; then
        data_flag="--source-data=2"
    fi
    if ! sudo mysqldump --single-transaction "$data_flag" \
        --databases teamster > "$DUMP_FILE" 2>/dev/null; then
        echo '{"status":"error","message":"mysqldump failed"}'
        return 1
    fi

    log "fixing collation" >&2
    sed -i 's/utf8mb4_0900_ai_ci/utf8mb4_general_ci/g' "$DUMP_FILE"

    local master_line
    master_line=$(grep -m1 '^\-\- CHANGE MASTER TO' "$DUMP_FILE" || true)
    if [[ -z "$master_line" ]]; then
        echo '{"status":"error","message":"binlog position not found in dump"}'
        rm -f "$DUMP_FILE"
        return 1
    fi

    local binlog_file binlog_pos
    binlog_file=$(echo "$master_line" | grep -oP "MASTER_LOG_FILE='\\K[^']+")
    binlog_pos=$(echo "$master_line" | grep -oP "MASTER_LOG_POS=\\K[0-9]+")

    log "binlog: file=$binlog_file pos=$binlog_pos" >&2
    printf 'BINLOG_FILE=%s\nBINLOG_POS=%s\n' "$binlog_file" "$binlog_pos" > "$POSITION_FILE"

    log "pushing to $REMOTE_HOST" >&2
    if ! scp -q "$DUMP_FILE" "$POSITION_FILE" "${REMOTE_HOST}:/tmp/"; then
        echo '{"status":"error","message":"scp failed"}'
        rm -f "$DUMP_FILE" "$POSITION_FILE"
        return 1
    fi

    rm -f "$DUMP_FILE" "$POSITION_FILE"
    log "push complete" >&2
    printf '{"status":"pushed","binlog_file":"%s","binlog_pos":%s}\n' "$binlog_file" "$binlog_pos"
}

handle_request() {
    local request_line=""
    read -r request_line || true
    # consume remaining headers
    while read -r header && [[ "$header" != $'\r' && -n "$header" ]]; do :; done

    local path
    path=$(echo "$request_line" | awk '{print $2}')

    case "$path" in
        /health)
            local body='{"status":"ok"}'
            printf 'HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' \
                "${#body}" "$body"
            ;;
        /push)
            local body
            if body=$(do_push); then
                printf 'HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' \
                    "${#body}" "$body"
            else
                printf 'HTTP/1.1 500 Internal Server Error\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' \
                    "${#body}" "$body"
            fi
            ;;
        *)
            local body='{"status":"error","message":"not found"}'
            printf 'HTTP/1.1 404 Not Found\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' \
                "${#body}" "$body"
            ;;
    esac
}

# When invoked with --handle, process a single HTTP request on stdin/stdout
# (called by socat EXEC for each connection).
if [[ "${1:-}" == "--handle" ]]; then
    handle_request
    exit 0
fi

preflight

log "repl-push-server listening on :${PORT}"

while true; do
    socat TCP-LISTEN:"$PORT",reuseaddr EXEC:"$0 --handle",nofork || true
done
