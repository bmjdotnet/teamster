#!/bin/bash
# selftest.sh — automated Teamster verification harness.
# Runs claude --print prompts inside a disposable LXD test container and
# checks the JSONL event log for expected tags.
# Exit 0 if all checks pass, 1 if any fail.
#
# Requires an LXD container with Teamster installed and hookd running.
# Override the defaults for your environment:
#   SELFTEST_CONTAINER  LXD container name        (default: teamster-test)
#   SELFTEST_USER       user inside the container  (default: teamster)
#   SELFTEST_JSONL      event log path             (default: ~USER/.local/share/teamster/events.jsonl)
set -euo pipefail

CONTAINER="${SELFTEST_CONTAINER:-teamster-test}"
USER="${SELFTEST_USER:-teamster}"
JSONL="${SELFTEST_JSONL:-/home/$USER/.local/share/teamster/events.jsonl}"
FAILURES=0
TOTAL=0

# --- prerequisite checks ---

if ! lxc list "$CONTAINER" 2>/dev/null | grep -q "RUNNING"; then
    echo "ERROR: container '$CONTAINER' is not running."
    echo "  Start it with: lxc start $CONTAINER"
    exit 1
fi

if ! lxc exec "$CONTAINER" -- su - "$USER" -c 'pgrep -x hookd > /dev/null 2>&1'; then
    echo "ERROR: hookd is not running inside $CONTAINER."
    echo "  Start it with: lxc exec $CONTAINER -- su - $USER -c 'nohup ~/bin/hookd > /dev/null 2>&1 &'"
    exit 1
fi

echo "=== Teamster self-test ==="
echo "  container: $CONTAINER"
echo "  hookd:     running"
echo ""

# --- helpers ---

# clear_jsonl empties the event log inside the container.
clear_jsonl() {
    lxc exec "$CONTAINER" -- su - "$USER" -c "> $JSONL" 2>/dev/null || true
}

# run_prompt sends a prompt to claude --print inside the container.
run_prompt() {
    local prompt="$1"
    lxc exec "$CONTAINER" -- su - "$USER" -c \
        "export PATH=\"\$HOME/.local/bin:\$HOME/bin:\$PATH\" && \
         printf '%s' $(printf '%q' "$prompt") | timeout 60 claude --print 2>/dev/null" \
        || true
}

# check_tag verifies that a given tag appears in the JSONL.
check_tag() {
    local tag="$1"
    TOTAL=$((TOTAL + 1))
    if lxc exec "$CONTAINER" -- grep -q "\"tag\":\"$tag\"" "$JSONL" 2>/dev/null; then
        echo "  PASS: [$tag]"
    else
        echo "  FAIL: [$tag] not found in JSONL"
        FAILURES=$((FAILURES + 1))
    fi
}

# check_focus verifies that a focus field (GOAL) appears in the JSONL.
# GOAL is stored as "focus" not "tag" — it's a separate field.
check_focus() {
    TOTAL=$((TOTAL + 1))
    if lxc exec "$CONTAINER" -- grep -q '"focus":' "$JSONL" 2>/dev/null; then
        echo "  PASS: [GOAL] (focus field)"
    else
        echo "  FAIL: [GOAL] no focus field in JSONL"
        FAILURES=$((FAILURES + 1))
    fi
}

# dump_jsonl_tags prints all tags seen, for failure diagnosis.
dump_tags() {
    echo "  (tags seen: $(lxc exec "$CONTAINER" -- grep -o '"tag":"[^"]*"' "$JSONL" 2>/dev/null | sort -u | tr '\n' ' ' || echo 'none'))"
}

# --- test cases ---

echo "--- Test A: MCP activity tools ---"
echo "    setOverallIntent + reportActivity + completeActivity"
clear_jsonl
run_prompt "Call setOverallIntent with message 'selftest goal'. Call reportActivity with type 'thought' and message 'selftest think'. Read /etc/hostname. Call completeActivity with message 'selftest done'."
sleep 2
check_focus
check_tag "THNK"
check_tag "READ"
check_tag "DONE"
dump_tags

echo ""
echo "--- Test B: WebSearch ---"
clear_jsonl
run_prompt "Search the web for 'test query 12345'."
sleep 2
check_tag " WEB"
dump_tags

echo ""
echo "--- Test C: File write and read ---"
clear_jsonl
run_prompt "Create a file /tmp/selftest.txt with the content 'hello'. Then read it back."
sleep 2
check_tag "EDIT"
check_tag "READ"
dump_tags

# --- summary ---

echo ""
echo "==========================="
echo "Results: $((TOTAL - FAILURES))/$TOTAL passed"
if [ "$FAILURES" -gt 0 ]; then
    echo "FAILED: $FAILURES check(s) did not find expected tags."
    exit 1
else
    echo "All checks passed."
    exit 0
fi
