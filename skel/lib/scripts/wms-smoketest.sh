#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

PASS=0
FAIL=0

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1 — $2"; FAIL=$((FAIL + 1)); }

# Resolve the wms-mcp binary. The Go module lives under <repo>/src, not the
# repo root, so a bare `go build ./cmd/wms-mcp` from the script's parent fails
# with "cannot find main module". Two layouts:
#   - installed: this script is at <basedir>/lib/scripts/, the prebuilt binary
#     is at <basedir>/bin/wms-mcp — use it as-is, no Go toolchain needed.
#   - in-repo dev: build from the src/ module via `go -C`.
BASEDIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
CLEANUP_BINARY=""
if [[ -x "$BASEDIR/bin/wms-mcp" ]]; then
    BINARY="$BASEDIR/bin/wms-mcp"
    echo "Using installed wms-mcp at $BINARY"
else
    SRC_DIR=""
    for _cand in "$SCRIPT_DIR/../../../src" "$SCRIPT_DIR/../../src" "$SCRIPT_DIR/../src"; do
        if [[ -f "$_cand/go.mod" ]]; then
            SRC_DIR="$(cd "$_cand" && pwd)"
            break
        fi
    done
    if [[ -z "$SRC_DIR" ]]; then
        echo "ERROR: no installed wms-mcp and no src/go.mod found — cannot build" >&2
        exit 1
    fi
    BINARY="$(mktemp -d)/wms-mcp"
    CLEANUP_BINARY="$BINARY"
    echo "Building wms-mcp from $SRC_DIR..."
    go -C "$SRC_DIR" build -o "$BINARY" ./cmd/wms-mcp
fi

# wms-mcp is MySQL-only: it exits non-zero unless TEAMSTER_STORE_DSN is a
# mysql:// URL. The smoketest provisions a throwaway schema on a test MySQL,
# runs the scenarios against it, and drops it on exit so no real data is
# touched. TEAMSTER_SMOKETEST_DSN points at a base mysql:// DSN we can CREATE
# DATABASE on; default to the dedicated test instance. If it is unreachable we
# skip rather than fail — a smoketest with no DB is not a regression.
BASE_DSN="${TEAMSTER_SMOKETEST_DSN:-mysql://root:test@127.0.0.1:13306/}"

# Decompose the base DSN into mysql client connection flags. We need a real
# admin connection to CREATE/DROP the throwaway schema; the per-test schema is
# then handed to wms-mcp via TEAMSTER_STORE_DSN.
_dsn_user=$(printf '%s' "$BASE_DSN" | sed -n 's|mysql://\([^:@]*\).*|\1|p')
_dsn_pass=$(printf '%s' "$BASE_DSN" | sed -n 's|mysql://[^:]*:\([^@]*\)@.*|\1|p')
_dsn_host=$(printf '%s' "$BASE_DSN" | sed -n 's|mysql://[^@]*@\([^:/]*\).*|\1|p')
_dsn_port=$(printf '%s' "$BASE_DSN" | sed -n 's|mysql://[^@]*@[^:]*:\([0-9]*\)/.*|\1|p')
[[ -z "$_dsn_port" ]] && _dsn_port="3306"

# Pass the password via MYSQL_PWD rather than -p on argv: the env var is read
# by the mysql client but never lands on the command line that the activity
# feed's [EXEC] view captures.
mysql_admin() {
    MYSQL_PWD="$_dsn_pass" mysql -h "$_dsn_host" -P "$_dsn_port" -u "$_dsn_user" "$@" 2>/dev/null
}

if ! command -v mysql >/dev/null 2>&1; then
    echo "SKIP: mysql client not found — cannot provision a throwaway schema"
    [[ -n "$CLEANUP_BINARY" ]] && rm -f "$CLEANUP_BINARY"
    exit 0
fi
if ! mysql_admin -e "SELECT 1" >/dev/null 2>&1; then
    echo "SKIP: test MySQL ($_dsn_host:$_dsn_port) not reachable — set TEAMSTER_SMOKETEST_DSN"
    [[ -n "$CLEANUP_BINARY" ]] && rm -f "$CLEANUP_BINARY"
    exit 0
fi

SCHEMA="wms_smoketest_$$"
if ! mysql_admin -e "CREATE DATABASE \`$SCHEMA\`"; then
    echo "ERROR: could not create throwaway schema $SCHEMA" >&2
    [[ -n "$CLEANUP_BINARY" ]] && rm -f "$CLEANUP_BINARY"
    exit 1
fi
trap 'mysql_admin -e "DROP DATABASE IF EXISTS \`$SCHEMA\`"; [[ -n "$CLEANUP_BINARY" ]] && rm -f "$CLEANUP_BINARY"' EXIT

# Point wms-mcp at the throwaway schema. wms-mcp runs migrations on first open.
export TEAMSTER_STORE_DSN="mysql://${_dsn_user}:${_dsn_pass}@${_dsn_host}:${_dsn_port}/${SCHEMA}"
echo "Using throwaway schema $SCHEMA on $_dsn_host:$_dsn_port"

# Run a batch of JSON-RPC requests through the binary and return all responses.
rpc_batch() {
    printf '%s\n' "$@" | "$BINARY" 2>/dev/null
}

# Extract the response line for a given request id.
resp_for() {
    local responses="$1"
    local id="$2"
    echo "$responses" | grep "\"id\":$id" | head -1
}

# ── Scenario 1: Basic CRUD ────────────────────────────────────────────────────
# Outcome o1 with two child work units w1, w2 (v3 vocab).

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_createOutcome","arguments":{"id":"o1","title":"Test Outcome"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_createWorkUnit","arguments":{"id":"w1","title":"Work Unit One","outcomeID":"o1"}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wms_createWorkUnit","arguments":{"id":"w2","title":"Work Unit Two","outcomeID":"o1"}}}' \
    '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"wms_getOutcome","arguments":{"id":"o1"}}}' \
    '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"wms_listWorkUnits","arguments":{"outcomeID":"o1"}}}' \
)

r=$(resp_for "$responses" 1)
if echo "$r" | grep -q '"protocolVersion"'; then
    pass "initialize"
else
    fail "initialize" "$r"
fi

r=$(resp_for "$responses" 2)
if echo "$r" | grep -q 'Created outcome'; then
    pass "createOutcome"
else
    fail "createOutcome" "$r"
fi

r=$(resp_for "$responses" 3)
if echo "$r" | grep -q 'Created work unit'; then
    pass "createWorkUnit w1"
else
    fail "createWorkUnit w1" "$r"
fi

r=$(resp_for "$responses" 4)
if echo "$r" | grep -q 'Created work unit'; then
    pass "createWorkUnit w2"
else
    fail "createWorkUnit w2" "$r"
fi

r=$(resp_for "$responses" 5)
if echo "$r" | grep -q 'Test Outcome'; then
    pass "getOutcome"
else
    fail "getOutcome" "$r"
fi

r=$(resp_for "$responses" 6)
if echo "$r" | grep -q 'w1'; then
    pass "listWorkUnits"
else
    fail "listWorkUnits" "$r"
fi

# ── Scenario 2: Engine rollup ─────────────────────────────────────────────────
# Transition: w1 pending→active→review→done, w2 pending→active→review→done;
#             expect the outcome to auto-complete (WorkUnit→Outcome rollup).

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"w1","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"w1","status":"review"}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"w1","status":"done"}}}' \
    '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"w2","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"w2","status":"review"}}}' \
    '{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"w2","status":"done"}}}' \
    '{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"wms_getOutcome","arguments":{"id":"o1"}}}' \
)

r=$(resp_for "$responses" 2)
if echo "$r" | grep -q 'Updated workunit'; then
    pass "w1 pending→active"
else
    fail "w1 pending→active" "$r"
fi

r=$(resp_for "$responses" 4)
if echo "$r" | grep -q 'Updated workunit'; then
    pass "w1 done"
else
    fail "w1 done" "$r"
fi

r=$(resp_for "$responses" 7)
if echo "$r" | grep -q 'Updated workunit'; then
    pass "w2 done"
else
    fail "w2 done" "$r"
fi

r=$(resp_for "$responses" 8)
# getOutcome returns JSON embedded in the MCP text field, so the inner quotes
# are backslash-escaped on the wire (...status\":\"done\"...). Match the
# escaped key:value pair, tolerating the raw (unescaped) form too.
if echo "$r" | grep -qE 'status\\?":\\?"done'; then
    pass "engine rollup: outcome auto-completed"
else
    fail "engine rollup: outcome auto-completed" "$r"
fi

# ── Scenario 3: Invalid transition rejected ───────────────────────────────────
# o1 is now "done"; there is no transition OUT of done — pending→done is also
# not a declared transition. Use a fresh pending outcome and attempt the
# undeclared pending→done jump, which must be rejected.

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_createOutcome","arguments":{"id":"o2","title":"Outcome Two"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"o2","status":"done"}}}' \
)

r=$(resp_for "$responses" 3)
if echo "$r" | grep -q '"error"'; then
    pass "invalid transition rejected"
else
    fail "invalid transition rejected" "$r"
fi

# ── Scenario 4: Dependency cycle rejected ────────────────────────────────────

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_createWorkUnit","arguments":{"id":"w3","title":"Work Unit Three","outcomeID":"o1"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_createWorkUnit","arguments":{"id":"w4","title":"Work Unit Four","outcomeID":"o1"}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wms_addDependency","arguments":{"blockerID":"w3","blockedID":"w4","blockerType":"workunit","blockedType":"workunit"}}}' \
    '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"wms_addDependency","arguments":{"blockerID":"w4","blockedID":"w3","blockerType":"workunit","blockedType":"workunit"}}}' \
)

r=$(resp_for "$responses" 4)
if echo "$r" | grep -q 'Added dependency'; then
    pass "addDependency w3→w4"
else
    fail "addDependency w3→w4" "$r"
fi

r=$(resp_for "$responses" 5)
if echo "$r" | grep -q '"error"'; then
    pass "dependency cycle rejected"
else
    fail "dependency cycle rejected" "$r"
fi

# ── Scenario 5: Focus round-trip ─────────────────────────────────────────────

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_setFocus","arguments":{"entityType":"outcome","entityID":"o1","focus":"shipping v1"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_getFocus","arguments":{"entityType":"outcome","entityID":"o1"}}}' \
)

r=$(resp_for "$responses" 2)
if echo "$r" | grep -q 'Focus set'; then
    pass "setFocus"
else
    fail "setFocus" "$r"
fi

r=$(resp_for "$responses" 3)
if echo "$r" | grep -q 'shipping v1'; then
    pass "getFocus round-trip"
else
    fail "getFocus round-trip" "$r"
fi

# ── Scenario 6: Codex identity resolution + journal audit trail ─────────────
# A mutating call carrying Codex's native _meta["x-codex-turn-metadata"]
# (WP1's identity fix) must land the Codex session UUID — never a Claude
# fallback — in the wms_journal entry the status transition produces
# (wms-mcp's JournalObserver fix, commit 80e47ac). One scenario proves both:
# the primary identity-resolution path and the journal attribution it feeds.

CODEX_SESSION_ID="codex-smoketest-session-$$"

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_createWorkUnit","arguments":{"id":"w-codex","title":"Codex Smoketest Unit","outcomeID":"o1"}}}' \
    "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"wms_updateStatus\",\"arguments\":{\"entityType\":\"workunit\",\"entityID\":\"w-codex\",\"status\":\"active\"},\"_meta\":{\"x-codex-turn-metadata\":{\"session_id\":\"$CODEX_SESSION_ID\",\"thread_id\":\"smoketest-thread\",\"model\":\"gpt-5.5\"}}}}" \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wms_getHistory","arguments":{"entityType":"workunit","entityID":"w-codex"}}}' \
)

r=$(resp_for "$responses" 3)
if echo "$r" | grep -q 'Updated workunit'; then
    pass "codex-originated status transition accepted"
else
    fail "codex-originated status transition accepted" "$r"
fi

r=$(resp_for "$responses" 4)
if echo "$r" | grep -q "$CODEX_SESSION_ID"; then
    pass "journal entry carries the Codex session id (not a Claude fallback)"
else
    fail "journal entry carries the Codex session id" "$r"
fi

# ── Scenario 7: Rename round-trip ────────────────────────────────────────────
# Rename is a title-only update with no state-machine validation — o1 and w1
# are already "done" (Scenario 2's rollup) and rename must still succeed,
# proving it bypasses status entirely rather than just working on this one
# status by coincidence.

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_renameOutcome","arguments":{"id":"o1","title":"Renamed Outcome"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_renameWorkUnit","arguments":{"id":"w1","title":"Renamed Work Unit"}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wms_getOutcome","arguments":{"id":"o1"}}}' \
    '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"wms_getWorkUnit","arguments":{"id":"w1"}}}' \
)

r=$(resp_for "$responses" 2)
if echo "$r" | grep -q 'Renamed outcome'; then
    pass "renameOutcome on a done outcome"
else
    fail "renameOutcome on a done outcome" "$r"
fi

r=$(resp_for "$responses" 3)
if echo "$r" | grep -q 'Renamed workunit'; then
    pass "renameWorkUnit on a done work unit"
else
    fail "renameWorkUnit on a done work unit" "$r"
fi

r=$(resp_for "$responses" 4)
if echo "$r" | grep -q 'Renamed Outcome'; then
    pass "getOutcome reflects new title"
else
    fail "getOutcome reflects new title" "$r"
fi

r=$(resp_for "$responses" 5)
if echo "$r" | grep -q 'Renamed Work Unit'; then
    pass "getWorkUnit reflects new title"
else
    fail "getWorkUnit reflects new title" "$r"
fi

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
