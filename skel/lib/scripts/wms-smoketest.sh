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

# The schema is throwaway but the hook stream is not: if this script is run
# from a shell that already has TEAMSTER_HOOK_SERVER_URL set (any dev/live
# session hooked into a real hookd), wms-mcp inherits it and registers a
# HookObserver (cmd/wms-mcp/main.go:93-94), posting every scenario's status
# changes to the LIVE hookd — bumping the real entity-count gauge and
# surfacing spurious close-out warnings for entities that only ever existed
# in this throwaway schema. Unset it (and its siblings, defensively, though
# only *_URL is read by wms-mcp today) so this binary never has a hookd to
# talk to, full stop.
unset TEAMSTER_HOOK_SERVER_URL TEAMSTER_HOOK_SERVER_PORT TEAMSTER_HOOK_SERVER_BIND
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

# ── Scenario 2: Cascade removed (WP7/R1) ─────────────────────────────────────
# Transition: w1 pending→active→review→done, w2 pending→active→review→done;
#             the outcome must NOT auto-complete — WP7 removed the
#             WorkUnit→Outcome rollup. Closing an Outcome is now deliberate
#             only (wms_updateOutcomeStatus), never a side effect of its last
#             WorkUnit finishing.

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
# are backslash-escaped on the wire (...status\":\"pending\"...). Match the
# escaped key:value pair, tolerating the raw (unescaped) form too. o1 was
# created with no explicit status (defaults to pending, see Scenario 1) and
# nothing in this scenario transitions the outcome itself, so it must still
# read pending here — proving the cascade did not fire, not just that it
# didn't reach "done" by some other path.
if echo "$r" | grep -qE 'status\\?":\\?"pending'; then
    pass "cascade removed: outcome does not auto-complete"
else
    fail "cascade removed: outcome does not auto-complete" "$r"
fi

# ── Scenario 3: Invalid transition rejected ───────────────────────────────────
# o1 is still "pending" (Scenario 2 proved no cascade); there is no declared
# pending→done transition either way. Use a fresh pending outcome and attempt
# the undeclared pending→done jump, which must be rejected.

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
# must both be "done" for this to prove anything, and rename must still
# succeed, proving it bypasses status entirely rather than just working on
# this one status by coincidence. w1 already reached "done" directly in
# Scenario 2 (its own pending→active→review→done transitions, unaffected by
# WP7 — only the OUTCOME rollup was removed), but o1 no longer free-rides on
# that cascade, so this scenario now closes it explicitly first.

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"o1","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"o1","status":"done"}}}' \
)

r=$(resp_for "$responses" 3)
if echo "$r" | grep -q 'Updated outcome'; then
    pass "explicit close: o1 → done"
else
    fail "explicit close: o1 → done" "$r"
fi

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

# ── Scenario 8: Reopen clears the resolution tag (LF-gfx-1) ─────────────────
# o3 closes done (resolution:achieved tagged manually), then reopens
# done→review — the sole edge leaving `done` (R2). The engine must clear the
# stale resolution tag right there and journal the cleared value, so a later
# review→abandoned can never again assert "achieved" on an abandoned entity.

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_createOutcome","arguments":{"id":"o3","title":"Reopen Clears Resolution"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"o3","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"o3","status":"done"}}}' \
    '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"wms_tagEntity","arguments":{"entityType":"outcome","entityID":"o3","tagKey":"resolution","tagValue":"achieved","source":"manual"}}}' \
    '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"wms_getEntityTags","arguments":{"entityType":"outcome","entityID":"o3"}}}' \
)

r=$(resp_for "$responses" 4)
if echo "$r" | grep -q 'Updated outcome'; then
    pass "o3 active→done"
else
    fail "o3 active→done" "$r"
fi

r=$(resp_for "$responses" 6)
if echo "$r" | grep -q 'achieved'; then
    pass "resolution:achieved bound before reopen"
else
    fail "resolution:achieved bound before reopen" "$r"
fi

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"o3","status":"review"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_getEntityTags","arguments":{"entityType":"outcome","entityID":"o3"}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wms_getHistory","arguments":{"entityType":"outcome","entityID":"o3","limit":20}}}' \
)

r=$(resp_for "$responses" 2)
if echo "$r" | grep -q 'Updated outcome'; then
    pass "o3 done→review (reopen)"
else
    fail "o3 done→review (reopen)" "$r"
fi

r=$(resp_for "$responses" 3)
if [[ -n "$r" ]] && ! echo "$r" | grep -q 'achieved'; then
    pass "resolution tag cleared on reopen"
else
    fail "resolution tag cleared on reopen" "$r"
fi

r=$(resp_for "$responses" 4)
if echo "$r" | grep -q 'cleared' && echo "$r" | grep -q 'achieved'; then
    pass "journal records the cleared resolution value"
else
    fail "journal records the cleared resolution value" "$r"
fi

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"o3","status":"abandoned"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_getEntityTags","arguments":{"entityType":"outcome","entityID":"o3"}}}' \
)

r=$(resp_for "$responses" 2)
if echo "$r" | grep -q 'Updated outcome'; then
    pass "o3 review→abandoned"
else
    fail "o3 review→abandoned" "$r"
fi

r=$(resp_for "$responses" 3)
if [[ -n "$r" ]] && ! echo "$r" | grep -q 'achieved'; then
    pass "abandoned o3 never reasserts resolution:achieved (LF-gfx-1)"
else
    fail "abandoned o3 never reasserts resolution:achieved (LF-gfx-1)" "$r"
fi

# ── Scenario 9: WP3 review sweep (dry-run then --confirm, both stages) ────────
# Unlike Scenarios 1-8, which drive wms-mcp over JSON-RPC exclusively, this
# scenario also invokes the `teamster` binary directly (the new
# `wms review-sweep` subcommand), against the SAME throwaway schema.

TEAMSTER_CLEANUP_BINARY=""
if [[ -x "$BASEDIR/bin/teamster" ]]; then
    TEAMSTER_BINARY="$BASEDIR/bin/teamster"
else
    TEAMSTER_BINARY="$(mktemp -d)/teamster"
    TEAMSTER_CLEANUP_BINARY="$TEAMSTER_BINARY"
    echo "Building teamster from $SRC_DIR..."
    go -C "$SRC_DIR" build -o "$TEAMSTER_BINARY" ./cmd/teamster
fi

# review-sweep goes through config.Load(), not a bare env-var check like
# wms-mcp's own hookd registration — config.Default() always constructs
# http://<hostname>:9125/event even when TEAMSTER_HOOK_SERVER_URL is unset,
# so on a host that happens to run a real hookd on the default port, merely
# unsetting the var (Scenarios 1-8's existing precaution, :87-96 above) is
# NOT sufficient here and would leak a spurious notification to it (caught
# live during development — see the WU deliverable for the incident). Belt
# and suspenders, per the lead's explicit ruling after that incident: point
# the URL at a definitely-unreachable local port AND separately disable
# hookd notification outright via ReviewSweep.NotifyHookd — two independent
# reasons this run can never reach a real hookd, not one.
run_review_sweep() {
    TEAMSTER_STORE_DSN="$TEAMSTER_STORE_DSN" \
    TEAMSTER_HOOK_SERVER_URL="http://127.0.0.1:1/event" \
    TEAMSTER_DATA_DIR="$(mktemp -d)" \
    TEAMSTER_REVIEW_SWEEP_ENABLED=1 \
    TEAMSTER_REVIEW_SWEEP_NOTIFY_HOOKD=0 \
    "$TEAMSTER_BINARY" wms review-sweep "$@"
}

# Fixture set, matching WP3-DESIGN.md §11's smoketest table:
#  - wu-s9-chain:        review, idle 8d, no deliverable -> Stage 1: on_hold;
#                        after its parking row is backdated 31d -> Stage 2: abandoned
#                        (the full review -> on_hold -> abandoned chain, §4's own example)
#  - wu-s9-deliv:        review, idle 8d, HAS a deliverable -> done, resolution:swept-unreviewed
#  - wu-s9-fresh:        review, idle 0d -> untouched (AC2)
#  - wu-s9-human-parked: on_hold via a plain MCP call (agent_id is NOT
#                        wms-review-sweep), its status row backdated 1000d ->
#                        untouched at every pass, at any age — the single
#                        most important fixture in this scenario (AC10)
#  - oc-s9-childless:    active, idle 8d, no children -> Stage 1: on_hold
#  - oc-s9-livechild:    active, idle 8d, direct WorkUnit child is ACTIVE (fresh)
#                        -> untouched (direct interlock)
#  - oc-s9-grandparent:  active, idle 8d; child oc-s9-child-done is DONE but
#                        itself holds a live WorkUnit -> untouched
#                        (MAJOR-1's exact shape: terminality is not transitive)

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_createOutcome","arguments":{"id":"oc-s9-root","title":"S9 root"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"oc-s9-root","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wms_createWorkUnit","arguments":{"id":"wu-s9-chain","title":"S9 chain","outcomeID":"oc-s9-root"}}}' \
    '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"wu-s9-chain","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"wu-s9-chain","status":"review"}}}' \
    '{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"wms_createWorkUnit","arguments":{"id":"wu-s9-deliv","title":"S9 delivered","outcomeID":"oc-s9-root"}}}' \
    '{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"wu-s9-deliv","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"wms_deliverResult","arguments":{"id":"wu-s9-deliv","summary":"s9 delivered","result":"s9 delivered result"}}}' \
    '{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"wu-s9-deliv","status":"review"}}}' \
    '{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"wms_createWorkUnit","arguments":{"id":"wu-s9-fresh","title":"S9 fresh","outcomeID":"oc-s9-root"}}}' \
    '{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"wu-s9-fresh","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"wu-s9-fresh","status":"review"}}}' \
    '{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"wms_createWorkUnit","arguments":{"id":"wu-s9-human-parked","title":"S9 human parked","outcomeID":"oc-s9-root"}}}' \
    '{"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"wu-s9-human-parked","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":16,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"wu-s9-human-parked","status":"on_hold"}}}' \
    '{"jsonrpc":"2.0","id":17,"method":"tools/call","params":{"name":"wms_createOutcome","arguments":{"id":"oc-s9-childless","title":"S9 childless"}}}' \
    '{"jsonrpc":"2.0","id":18,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"oc-s9-childless","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"wms_createOutcome","arguments":{"id":"oc-s9-livechild","title":"S9 live child"}}}' \
    '{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"oc-s9-livechild","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"wms_createWorkUnit","arguments":{"id":"wu-s9-livechild-wu","title":"S9 live child WU","outcomeID":"oc-s9-livechild"}}}' \
    '{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"wu-s9-livechild-wu","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"wms_createOutcome","arguments":{"id":"oc-s9-grandparent","title":"S9 grandparent"}}}' \
    '{"jsonrpc":"2.0","id":24,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"oc-s9-grandparent","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":25,"method":"tools/call","params":{"name":"wms_createOutcome","arguments":{"id":"oc-s9-child-done","title":"S9 child done"}}}' \
    '{"jsonrpc":"2.0","id":26,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"oc-s9-child-done","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":27,"method":"tools/call","params":{"name":"wms_addOutcomeParent","arguments":{"parentID":"oc-s9-grandparent","childID":"oc-s9-child-done"}}}' \
    '{"jsonrpc":"2.0","id":28,"method":"tools/call","params":{"name":"wms_createWorkUnit","arguments":{"id":"wu-s9-live-grandchild","title":"S9 live grandchild","outcomeID":"oc-s9-child-done"}}}' \
    '{"jsonrpc":"2.0","id":29,"method":"tools/call","params":{"name":"wms_updateStatus","arguments":{"entityType":"workunit","entityID":"wu-s9-live-grandchild","status":"active"}}}' \
    '{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"wms_updateOutcomeStatus","arguments":{"id":"oc-s9-child-done","status":"done"}}}' \
)

r=$(resp_for "$responses" 9)
if echo "$r" | grep -q 'wu-s9-deliv\|stored\|Delivered\|delivered'; then
    pass "s9 fixture: wms_deliverResult on wu-s9-deliv"
else
    fail "s9 fixture: wms_deliverResult on wu-s9-deliv" "$r"
fi

r=$(resp_for "$responses" 30)
if echo "$r" | grep -q 'Updated outcome'; then
    pass "s9 fixture: oc-s9-child-done -> done despite live WorkUnit (advisory warning only)"
else
    fail "s9 fixture: oc-s9-child-done -> done despite live WorkUnit (advisory warning only)" "$r"
fi

# Backdate updated_at for every "idle 8d" fixture — 8 days clears the 168h
# (7d) Stage 1 default threshold; wu-s9-fresh and the live-child fixtures are
# deliberately left at NOW.
mysql_admin -D "$SCHEMA" -e "
UPDATE workunits SET updated_at = NOW() - INTERVAL 8 DAY WHERE id IN ('wu-s9-chain','wu-s9-deliv');
UPDATE outcomes SET updated_at = NOW() - INTERVAL 8 DAY WHERE id IN ('oc-s9-childless','oc-s9-livechild','oc-s9-grandparent');
"

# wu-s9-human-parked's on_hold status row was written by this scenario's own
# plain MCP call — its agent_id is whatever the smoketest's own (non-sweep)
# identity is, never 'wms-review-sweep'. Backdate it 1000 days to prove the
# human-parked exemption holds at any age, not just within some plausible
# window.
mysql_admin -D "$SCHEMA" -e "
UPDATE wms_journal SET created_at = NOW() - INTERVAL 1000 DAY
WHERE entity_type = 'workunit' AND entity_id = 'wu-s9-human-parked' AND field = 'status'
ORDER BY id DESC LIMIT 1"

echo "--- review-sweep dry-run (Stage 1) ---"
dryrun_out=$(run_review_sweep)
echo "$dryrun_out"

# Provably hookd-safe, not just believed to be: the command must itself
# report "none" as its notification target — not merely "we set a dead URL
# and hoped". This is the exact line an operator or another harness should
# grep for before trusting any run of this command is isolated.
if echo "$dryrun_out" | grep -q 'hookd notification target: none'; then
    pass "s9 run reports hookd notification target: none (provably isolated)"
else
    fail "s9 run reports hookd notification target: none (provably isolated)" "$dryrun_out"
fi

if echo "$dryrun_out" | grep -q 'wu-s9-chain' && echo "$dryrun_out" | grep -q 'wu-s9-deliv' && echo "$dryrun_out" | grep -q 'oc-s9-childless'; then
    pass "s9 dry-run lists the expected Stage 1 candidates"
else
    fail "s9 dry-run lists the expected Stage 1 candidates" "$dryrun_out"
fi
if echo "$dryrun_out" | grep -qE 'wu-s9-fresh|wu-s9-livechild-wu|oc-s9-livechild|wu-s9-human-parked'; then
    fail "s9 dry-run must NOT list fixtures that are never candidates at all" "$dryrun_out"
else
    pass "s9 dry-run does not list fixtures that are never candidates at all"
fi

# oc-s9-grandparent IS a Stage-1 candidacy-filter candidate (idle past
# threshold, direct child terminal) that the descendant-safety walk then
# blocks (MAJOR-1's shape: the child, though done, holds a live grandchild).
# It must now appear in the dry-run listing, evaluated but left open — the
# audit-trail completeness fix (@bench/@auditor, 2026-09-02): every
# evaluated candidate gets a line (§8), not only the ones actually disposed.
if echo "$dryrun_out" | grep -q 'oc-s9-grandparent.*outcome-idle-skipped'; then
    pass "s9 dry-run lists oc-s9-grandparent as evaluated-but-blocked (outcome-idle-skipped)"
else
    fail "s9 dry-run lists oc-s9-grandparent as evaluated-but-blocked (outcome-idle-skipped)" "$dryrun_out"
fi

echo "--- review-sweep --confirm (Stage 1 pass) ---"
run_review_sweep --confirm >/dev/null

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_getWorkUnit","arguments":{"id":"wu-s9-chain"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_getWorkUnit","arguments":{"id":"wu-s9-deliv"}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wms_getWorkUnit","arguments":{"id":"wu-s9-fresh"}}}' \
    '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"wms_getWorkUnit","arguments":{"id":"wu-s9-human-parked"}}}' \
    '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"wms_getOutcome","arguments":{"id":"oc-s9-childless"}}}' \
    '{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"wms_getOutcome","arguments":{"id":"oc-s9-livechild"}}}' \
    '{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"wms_getOutcome","arguments":{"id":"oc-s9-grandparent"}}}' \
    '{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"wms_getEntityTags","arguments":{"entityType":"workunit","entityID":"wu-s9-deliv"}}}' \
)

check_status() { # $1=response $2=want-substring $3=label
    # Backslash-normalize first: the MCP text-content wrapper may or may not
    # escape the inner JSON's quotes depending on serialization depth: this
    # makes the check match either form.
    local normalized
    normalized=$(printf '%s' "$1" | tr -d '\\')
    if echo "$normalized" | grep -q "\"status\":\"$2\""; then
        pass "$3 -> $2"
    else
        fail "$3 -> $2" "$1"
    fi
}
check_status "$(resp_for "$responses" 2)" "on_hold" "s9 wu-s9-chain (Stage 1)"
check_status "$(resp_for "$responses" 3)" "done" "s9 wu-s9-deliv"
check_status "$(resp_for "$responses" 4)" "review" "s9 wu-s9-fresh (untouched)"
check_status "$(resp_for "$responses" 5)" "on_hold" "s9 wu-s9-human-parked (untouched, still on_hold)"
check_status "$(resp_for "$responses" 6)" "on_hold" "s9 oc-s9-childless (Stage 1)"
check_status "$(resp_for "$responses" 7)" "active" "s9 oc-s9-livechild (untouched, direct interlock)"
check_status "$(resp_for "$responses" 8)" "active" "s9 oc-s9-grandparent (untouched, MAJOR-1 descendant walk)"

r=$(resp_for "$responses" 9)
if echo "$r" | grep -q 'swept-unreviewed'; then
    pass "s9 wu-s9-deliv tagged resolution:swept-unreviewed"
else
    fail "s9 wu-s9-deliv tagged resolution:swept-unreviewed" "$r"
fi

# Ground-truth journal check for the audit-trail completeness fix
# (@bench/@auditor, 2026-09-02): oc-s9-grandparent, blocked by the
# descendant-safety walk above rather than disposed, must still have left a
# durable field='sweep_evaluated' row — the case that was previously a bare
# `continue` with no trace at all.
grandparent_skip=$(mysql_admin -D "$SCHEMA" -N -e "
SELECT COUNT(*) FROM wms_journal
WHERE entity_type = 'outcome' AND entity_id = 'oc-s9-grandparent'
  AND field = 'sweep_evaluated' AND new_value = 'skipped'
  AND agent_id = 'wms-review-sweep'
  AND notes LIKE 'outcome-idle-skipped %'")
if [[ "$grandparent_skip" -ge 1 ]]; then
    pass "s9 oc-s9-grandparent has a durable sweep_evaluated journal row (outcome-idle-skipped)"
else
    fail "s9 oc-s9-grandparent has a durable sweep_evaluated journal row (outcome-idle-skipped)" "count=$grandparent_skip"
fi

# Age wu-s9-chain's fresh parking row 31 days (past the 720h/30d Stage 2
# default) and re-run — proving the full review -> on_hold -> abandoned
# chain, and that wu-s9-human-parked (aged 1000d above) is STILL untouched.
#
# Everything ELSE in the entity's pre-park history must also move safely
# before the park, or its real (recent) timestamps read as "activity AFTER
# the (artificially backdated) park" — caught live twice while writing this
# fixture: first wms_journal's own pre-existing pending->active/active->review
# rows, then wms_intervals' auto-created kind='state' rows (the engine opens
# one per transition via the MCP tool path — a bookkeeping side effect, not
# independent activity, but indistinguishable from it by timestamp alone once
# only the park row is moved). Both times the entity was left correctly and
# safely on_hold rather than wrongly abandoned — the "no later activity"
# check fails closed on an inconsistent history, which is reassuring, but it
# means the POSITIVE abandon path this fixture exists to test was not
# actually exercised until every table sharing the fixture's timeline moved
# together. Deliverables need no such fix — wu-s9-chain never received one.
mysql_admin -D "$SCHEMA" -e "
UPDATE wms_journal SET created_at = NOW() - INTERVAL 40 DAY
WHERE entity_type = 'workunit' AND entity_id = 'wu-s9-chain' AND field = 'status'
  AND NOT (agent_id = 'wms-review-sweep' AND new_value = 'on_hold');
UPDATE wms_intervals SET started_at = started_at - INTERVAL 40 DAY,
                          ended_at = ended_at - INTERVAL 40 DAY
WHERE entity_type = 'workunit' AND entity_id = 'wu-s9-chain';
UPDATE wms_journal SET created_at = NOW() - INTERVAL 31 DAY
WHERE entity_type = 'workunit' AND entity_id = 'wu-s9-chain' AND field = 'status' AND agent_id = 'wms-review-sweep'
ORDER BY id DESC LIMIT 1"

echo "--- review-sweep --confirm (Stage 2 pass) ---"
run_review_sweep --confirm >/dev/null

responses=$(rpc_batch \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wms_getWorkUnit","arguments":{"id":"wu-s9-chain"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wms_getWorkUnit","arguments":{"id":"wu-s9-human-parked"}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wms_getHistory","arguments":{"entityType":"workunit","entityID":"wu-s9-chain","limit":20}}}' \
)
check_status "$(resp_for "$responses" 2)" "abandoned" "s9 wu-s9-chain (Stage 2)"
check_status "$(resp_for "$responses" 3)" "on_hold" "s9 wu-s9-human-parked (AC10: untouched at any age)"

r=$(resp_for "$responses" 4)
if echo "$r" | grep -q 'review' && echo "$r" | grep -q 'on_hold' && echo "$r" | grep -q 'abandoned'; then
    pass "s9 wu-s9-chain journal shows the full review -> on_hold -> abandoned chain"
else
    fail "s9 wu-s9-chain journal shows the full review -> on_hold -> abandoned chain" "$r"
fi

# AC8 (design §12), now that both stages have run: the rule-id set this
# fixture set can reach is a strict subset of the design's eight (this
# scenario has no fixture for a walk-error/op-budget-exceeded case, and
# doesn't exercise wu-review-skipped or a positive outcome-onhold-abandon)
# — assert the five it SHOULD reach are present, and that no journal row
# this sweep wrote lacks a parseable rule-id prefix, rather than asserting
# the full eight against a fixture set that can't produce all of them.
rule_ids=$(mysql_admin -D "$SCHEMA" -N -e "
SELECT DISTINCT SUBSTRING_INDEX(notes, ' ', 1) FROM wms_journal
WHERE agent_id = 'wms-review-sweep' ORDER BY 1")
for want in wu-review-undelivered wu-review-delivered outcome-idle outcome-idle-skipped wu-onhold-abandon; do
    if echo "$rule_ids" | grep -qx "$want"; then
        pass "s9 AC8: rule id '$want' present in wms_journal"
    else
        fail "s9 AC8: rule id '$want' present in wms_journal" "$rule_ids"
    fi
done
blank_notes=$(mysql_admin -D "$SCHEMA" -N -e "
SELECT COUNT(*) FROM wms_journal WHERE agent_id = 'wms-review-sweep' AND (notes = '' OR notes IS NULL)")
if [[ "$blank_notes" -eq 0 ]]; then
    pass "s9 AC8: no wms-review-sweep journal row lacks a parseable rule-id prefix"
else
    fail "s9 AC8: no wms-review-sweep journal row lacks a parseable rule-id prefix" "count=$blank_notes"
fi

[[ -n "$TEAMSTER_CLEANUP_BINARY" ]] && rm -f "$TEAMSTER_CLEANUP_BINARY"

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
