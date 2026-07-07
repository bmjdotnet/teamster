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
#   SELFTEST_BASEDIR    Teamster BASEDIR in-container (default: ~USER/teamster)
#   SELFTEST_JSONL      event log path             (default: $SELFTEST_BASEDIR/var/events.jsonl)
set -euo pipefail

CONTAINER="${SELFTEST_CONTAINER:-teamster-test}"
USER="${SELFTEST_USER:-teamster}"
BASEDIR="${SELFTEST_BASEDIR:-/home/$USER/teamster}"
# JSONL lives at $BASEDIR/var/events.jsonl (skel/CLAUDE.md's documented
# layout) — a prior default here pointed at ~/.local/share/teamster/, a path
# nothing in the current architecture ever writes to. That stale default made
# every check_tag/check_focus call fail regardless of whether hooks actually
# fired (found running this script for real against a fresh cleanroom
# container: events.jsonl had the exact expected tags, but every check still
# reported FAIL until this was fixed) — not a Codex-support regression, a
# pre-existing latent bug this was the first real end-to-end run to surface.
JSONL="${SELFTEST_JSONL:-$BASEDIR/var/events.jsonl}"
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

# run_codex sends a prompt to `codex exec` inside the container, same shape
# as run_prompt but for the second runtime.
run_codex() {
    local prompt="$1"
    lxc exec "$CONTAINER" -- su - "$USER" -c \
        "export PATH=\"\$HOME/.local/bin:\$HOME/bin:\$PATH\" && \
         timeout 60 codex exec --skip-git-repo-check $(printf '%q' "$prompt") 2>/dev/null" \
        || true
}

# check_file verifies a file exists inside the container (backups, systemd
# units, etc.) — a lighter check than check_tag since it doesn't depend on
# JSONL content, just that an install-time write actually happened.
check_file() {
    local path="$1" label="$2"
    TOTAL=$((TOTAL + 1))
    if lxc exec "$CONTAINER" -- su - "$USER" -c "test -e $(printf '%q' "$path")" 2>/dev/null; then
        echo "  PASS: [$label] ($path)"
    else
        echo "  FAIL: [$label] not found: $path"
        FAILURES=$((FAILURES + 1))
    fi
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

# --- Codex tests (graceful skip: a container without Codex installed is not
# a failure, same opposite-polarity posture as the installer's own probe) ---

if lxc exec "$CONTAINER" -- su - "$USER" -c 'command -v codex' >/dev/null 2>&1; then
    echo ""
    echo "--- Test D: Codex config.toml doctor gate ---"
    TOTAL=$((TOTAL + 1))
    DOCTOR_JSON=$(lxc exec "$CONTAINER" -- su - "$USER" -c \
        'export PATH="$HOME/.local/bin:$HOME/bin:$PATH" && codex --strict-config doctor --json' 2>/dev/null || echo '{}')
    DOCTOR_STATUS=$(echo "$DOCTOR_JSON" | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin)["checks"]["config.load"]["status"])
except Exception:
    print("parse-error")' 2>/dev/null)
    if [[ "$DOCTOR_STATUS" == "ok" ]]; then
        echo "  PASS: [DOCTOR] config.load status=ok"
    else
        echo "  FAIL: [DOCTOR] config.load status=$DOCTOR_STATUS (expected ok)"
        FAILURES=$((FAILURES + 1))
    fi

    echo ""
    echo "--- Test E: Codex client-config backups exist (both runtimes) ---"
    # A .pre-teamster backup is only created for a file that EXISTED before
    # Teamster's first write to it (installbackup.Backup's documented no-op
    # semantics — nothing to preserve on a target that didn't exist yet). On
    # a from-scratch container, settings.json/CLAUDE.md are typically
    # created BY this install run (no prior file), so those two legitimately
    # have no backup on a first run — expect them to start passing from a
    # second (reinstall) run onward. config.toml and .claude.json usually
    # DO pre-exist by first-install time (Codex/Claude Code's own install
    # step creates a stub), so those two are meaningful checks even on a
    # fresh single-install run.
    check_file "/home/$USER/.codex/config.toml.pre-teamster" "codex config.toml backup"
    check_file "/home/$USER/.claude/settings.json.pre-teamster" "claude settings.json backup"
    check_file "/home/$USER/.claude.json.pre-teamster" "claude .claude.json backup"
    check_file "/home/$USER/.claude/CLAUDE.md.pre-teamster" "claude CLAUDE.md backup"

    echo ""
    echo "--- Test F: codex-scraper systemd unit staged ---"
    # This harness invokes teamster-install directly (never
    # lib/installrunner.sh's systemd orchestration — true of the pre-existing
    # Claude-only cleanroom too, which never enables teamster-hookd.service
    # either), so `systemctl is-enabled` would fail regardless of whether
    # staging worked correctly. Check that the unit file itself was staged
    # under BASEDIR/etc — the thing this harness can actually speak to.
    check_file "$BASEDIR/etc/teamster-codex-scraper.service" "codex-scraper unit staged"
    check_file "$BASEDIR/etc/teamster-codex-scraper.timer" "codex-scraper timer staged"

    echo ""
    echo "--- Test G: Codex hook fires with zero interactive trust ---"
    clear_jsonl
    run_codex "Run the shell command: echo teamster-selftest-codex"
    sleep 2
    TOTAL=$((TOTAL + 1))
    if lxc exec "$CONTAINER" -- test -s "$JSONL" 2>/dev/null; then
        echo "  PASS: [CODEX-HOOK] events.jsonl received at least one event post-install, no trust prompt needed"
    else
        echo "  FAIL: [CODEX-HOOK] no events reached events.jsonl from codex exec"
        FAILURES=$((FAILURES + 1))
    fi
    dump_tags
else
    echo ""
    echo "--- Codex tests skipped: codex CLI not found in $CONTAINER (informational, not a failure) ---"
fi

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
