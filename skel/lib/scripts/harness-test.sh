#!/usr/bin/env bash
# harness-test.sh — integration tests for session-explorer.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=session-explorer.sh
source "$SCRIPT_DIR/session-explorer.sh"

PASS=0
FAIL=0

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1"; FAIL=$((FAIL + 1)); }

# Clean up any leftover sessions from a prior failed run.
se_stop "harness-test" 2>/dev/null || true
se_stop "claude-test"  2>/dev/null || true

echo "=== Test 1: basic bash session ==="

se_start "harness-test" bash

if ! se_wait "harness-test" '\$' 10; then
    fail "prompt did not appear within 10s"
else
    pass "bash prompt appeared"

    se_sendline "harness-test" "echo HARNESS_OK"

    if ! se_wait "harness-test" "HARNESS_OK" 10; then
        fail "HARNESS_OK not seen in pane within 10s"
    else
        output=$(se_read "harness-test")
        if echo "$output" | grep -q "HARNESS_OK"; then
            pass "se_read contains HARNESS_OK"
        else
            fail "se_read output missing HARNESS_OK"
        fi
    fi

    se_sendline "harness-test" "exit"
    sleep 1

    if ! se_alive "harness-test"; then
        pass "session gone after exit"
    else
        se_stop "harness-test"
        fail "session still alive after exit (force-killed)"
    fi
fi

echo ""
echo "=== Test 2: claude --print in lxc container ==="

se_start "claude-test" bash
sleep 0.5
se_sendline "claude-test" "lxc exec teamster-test -- su - teamster -c 'export PATH=\$HOME/.local/bin:\$HOME/bin:\$PATH && echo hello-from-claude && claude --print \"say hello\"'"

if ! se_wait_scrollback "claude-test" "hello" 45; then
    fail "claude did not output 'hello' within 30s"
    echo "--- scrollback at timeout ---"
    se_read_scrollback "claude-test" 100
else
    output=$(se_read_scrollback "claude-test" 100)
    if echo "$output" | grep -qi "hello"; then
        pass "scrollback contains 'hello'"
    else
        fail "scrollback missing 'hello'"
        echo "--- scrollback ---"
        echo "$output"
    fi
fi

se_stop "claude-test" 2>/dev/null || true

echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="
[ "$FAIL" -eq 0 ]
