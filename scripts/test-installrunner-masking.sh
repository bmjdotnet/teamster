#!/usr/bin/env bash
# Exercises lib/installrunner.sh's masking guard (wh2-upgrade-undoes-clone-masks)
# and hookd's enable-on-restart fix (wh2-hookd-enable-on-install): an in-place
# upgrade must not silently undo a masked unit (any unit masked by hand, or the
# three `teamster clone` masks under R11), must not swallow a REAL install
# failure while doing so, and must enable — not merely start — hookd so it
# survives a reboot, unless hookd is itself masked. Everything below is
# extracted from installrunner.sh via sed/awk rather than copied, so nothing
# here can drift from what ships:
#
#   1. unit_is_masked <unit>        — the predicate itself.
#   2. install_and_enable_unit(...) — the one function all ten of the eleven
#      guarded units call (every unit but hookd, which has its own
#      compare-then-sync shape). Called BARE (never via `||`/`&&`) so a real
#      failure inside it still trips `set -e`, same as the unguarded
#      original code — see case 2b, the regression touchstone's review
#      caught in an earlier version of this fix.
#   3. The call-site pattern itself — `if unit_is_masked U; then
#      masked_unit_notice ...; else install_and_enable_unit ...; fi` —
#      extracted verbatim (comment header through closing `fi`) for TWO
#      real blocks (sweep: one of the three R11 units; classify: a unit
#      `teamster clone` never masks, proving the guard is not R11-specific)
#      and run standalone under `set -euo pipefail`, masked and unmasked.
#   4. hookd's `systemd)` restart arm — its own case-arm, not
#      install_and_enable_unit's shape (curl health check, HOOKD_PORT) — run
#      the same way: masked leaves it alone, unmasked calls `enable --now`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLRUNNER="$SCRIPT_DIR/../lib/installrunner.sh"

extract_fn() { sed -n "/^${1}() {/,/^}/p" "$INSTALLRUNNER"; }
# extract_block <comment-anchor-substring>: from the named "# Sync the ..."
# comment line through the closing `fi` of the `if` immediately following
# it — the real, unmodified per-unit block, not a retyped stand-in. Plain
# substring match (index()), not regex, so the anchor text needs no escaping.
extract_block() {
    awk -v anchor="$1" '
        index($0, anchor) { grab=1 }
        grab { print; if (/^if/) { seen_if=1 } if (seen_if && /^fi$/) { exit } }
    ' "$INSTALLRUNNER"
}

UNIT_IS_MASKED_SRC=$(extract_fn unit_is_masked)
MASKED_NOTICE_SRC=$(extract_fn masked_unit_notice)
INSTALL_AND_ENABLE_SRC=$(extract_fn install_and_enable_unit)
SWEEP_BLOCK_SRC=$(extract_block 'Sync the sweep service')
CLASSIFY_BLOCK_SRC=$(extract_block 'Sync the classify service')
# extract_case_arm <case-arm-line>: the exact "    <pattern>)" line through
# its own closing "        ;;" — hookd's restart step doesn't fit
# install_and_enable_unit's shape (curl health check, HOOKD_PORT, its own
# case statement), so this pulls just its `systemd)` arm verbatim rather
# than reimplementing it (wh2-hookd-enable-on-install).
extract_case_arm() {
    awk -v start="$1" '
        $0 == start { grab=1 }
        grab { print; if (/^        ;;$/) { exit } }
    ' "$INSTALLRUNNER"
}
HOOKD_SYSTEMD_ARM_SRC=$(extract_case_arm '    systemd)')
# extract_range <start-anchor-substring> <end-anchor-substring>: from the
# first line containing start-anchor through the first line containing
# end-anchor (inclusive), verbatim — for a printed-advice block that doesn't
# fit extract_block's if/fi shape (wh2-installrunner-wire-help-mask): the
# stage-only WIRE=0 block's mask check is followed by more echo lines AFTER
# its own `fi`, which extract_block would stop at, so this anchors on a
# unique leading comment instead and reads through to the block's own last
# line. Both anchors must be unique in the file — an ambiguous start-anchor
# would silently grab from the wrong (earlier) occurrence.
extract_range() {
    awk -v start="$1" -v end="$2" '
        index($0, start) { grab=1 }
        grab { print; if (index($0, end)) { exit } }
    ' "$INSTALLRUNNER"
}
# The WIRE=0 stage-only "wire manually" advisory (wh2-installrunner-wire-help-mask):
# anchored on its own unique lead-in comment through its last printed line,
# so it also picks up the mask-aware `if unit_is_masked ...; then ... fi` AND
# the unconditional "Or wire manually:" recipe that follows it — extract_block
# alone would stop at the inner `fi` and miss the recipe.
WIRE0_ADVICE_SRC=$(extract_range 'Printed advice, not executed code' 'register MCP servers')
# The `none)` running-mode arm's advisory (same WU) — same case-arm shape as
# hookd's `systemd)` arm above, so extract_case_arm is reused as-is.
NONE_ARM_SRC=$(extract_case_arm '    none)')
# The otelcol/prometheus/grafana supervisor-group blocks (wh2-supervisor-systemd-units)
# use install_and_enable_unit's own shape, same as sweep/classify — each has
# its own one-line anchor comment in installrunner.sh (added specifically so
# this extraction is unambiguous; the guard's first physical line is shared
# verbatim with several other blocks, including sweep/classify/health-collector/
# token-scraper, so nothing shorter would pick out any one of the three).
OTELCOL_BLOCK_SRC=$(extract_block 'otelcol supervisor-group unit')
PROMETHEUS_BLOCK_SRC=$(extract_block 'prometheus supervisor-group unit')
GRAFANA_BLOCK_SRC=$(extract_block 'grafana supervisor-group unit')
# The mcp-scraper enable block (WP11-TAILER-DESIGN.md §9, wh2-wp11-mcp-tailer):
# same install_and_enable_unit shape as sweep/classify, PLUS review-sweep's
# own mask-first-then-yaml-check gating (mcp-scraper.enabled). Its nested
# `if [[ -f "$MCP_SCRAPER_YAML" ]]; then ... fi` block is indented, so
# extract_block's `/^fi$/` (column-0 only) still finds the right outer `fi`
# and does not stop early at the inner one — same property sweep/classify's
# own nested if/fi shape already relies on.
MCP_SCRAPER_BLOCK_SRC=$(extract_block 'Sync the mcp-scraper service')
for name_val in "unit_is_masked:$UNIT_IS_MASKED_SRC" "masked_unit_notice:$MASKED_NOTICE_SRC" \
    "install_and_enable_unit:$INSTALL_AND_ENABLE_SRC" "sweep block:$SWEEP_BLOCK_SRC" \
    "classify block:$CLASSIFY_BLOCK_SRC" "hookd systemd) arm:$HOOKD_SYSTEMD_ARM_SRC" \
    "otelcol block:$OTELCOL_BLOCK_SRC" "prometheus block:$PROMETHEUS_BLOCK_SRC" \
    "grafana block:$GRAFANA_BLOCK_SRC" "wire0 advice:$WIRE0_ADVICE_SRC" \
    "none arm:$NONE_ARM_SRC" "mcp-scraper block:$MCP_SCRAPER_BLOCK_SRC"; do
    if [[ -z "${name_val#*:}" ]]; then
        echo "FAIL: could not extract ${name_val%%:*} from $INSTALLRUNNER" >&2
        exit 1
    fi
done
eval "$UNIT_IS_MASKED_SRC"
eval "$MASKED_NOTICE_SRC"
eval "$INSTALL_AND_ENABLE_SRC"

# --- stubs: no real sudo, no real systemd, in a plain user-owned temp dir ---
# shellcheck disable=SC2034 # consumed by the eval'd/extracted bodies above, invisible to static analysis
C_RESET='' C_GREEN='' C_CYAN='' C_YELLOW='' C_BOLD_RED='' C_BOLD_GREEN='' C_BOLD_CYAN='' C_BOLD_WHITE=''
# shellcheck disable=SC2317 # redefined again in 2c to capture calls, then restored — not dead code
dlog() { :; }
sudo() { "$@"; }
SYSTEMCTL_CALLS=()
systemctl() { SYSTEMCTL_CALLS+=("$*"); return 0; }

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

pass=0
fail=0
examined=0
check() {
    local desc="$1" expect="$2" actual="$3"
    examined=$((examined + 1))
    if [[ "$expect" == "$actual" ]]; then
        echo "  ok: $desc"
        pass=$((pass + 1))
    else
        echo "  FAIL: $desc (expected [$expect], got [$actual])" >&2
        fail=$((fail + 1))
    fi
}

result_of() {
    if unit_is_masked "$1"; then echo masked; else echo not-masked; fi
}

echo "--- 1. unit_is_masked ---"
export SYSTEMD_UNIT_DIR="$TMPDIR/predicate"
mkdir -p "$SYSTEMD_UNIT_DIR"
check "absent unit is not masked" "not-masked" "$(result_of no-such.timer)"
echo "real file content" > "$SYSTEMD_UNIT_DIR/teamster-real.timer"
check "regular file is not masked" "not-masked" "$(result_of teamster-real.timer)"
ln -s /dev/null "$SYSTEMD_UNIT_DIR/teamster-masked.timer"
check "symlink to /dev/null is masked" "masked" "$(result_of teamster-masked.timer)"
ln -s /etc/hostname "$SYSTEMD_UNIT_DIR/teamster-other-symlink.timer"
check "symlink to a non-null target is not masked" "not-masked" "$(result_of teamster-other-symlink.timer)"
SYSTEMD_UNIT_DIR="$TMPDIR/does-not-exist"
check "nonexistent unit directory is not masked" "not-masked" "$(result_of teamster-sweep.timer)"

echo ""
echo "--- 2a. install_and_enable_unit called bare: succeeds and installs for real ---"
SYSTEMD_UNIT_DIR="$TMPDIR/bare-success"
mkdir -p "$SYSTEMD_UNIT_DIR" "$TMPDIR/src"
echo "svc source" > "$TMPDIR/src/teamster-sweep.service"
echo "timer source" > "$TMPDIR/src/teamster-sweep.timer"
SYSTEMCTL_CALLS=()
install_and_enable_unit install.sweep-timer "--- Syncing sweep timer ---" \
    teamster-sweep.timer 1 "enabled teamster-sweep.timer" \
    "$TMPDIR/src/teamster-sweep.service" "$TMPDIR/src/teamster-sweep.timer" \
    > "$TMPDIR/bare-success-out.txt" 2>&1
rc=$?
out=$(cat "$TMPDIR/bare-success-out.txt")
check "bare unmasked call's own return code" "0" "$rc"
check "bare unmasked call installed the service file" "svc source" "$(cat "$SYSTEMD_UNIT_DIR/teamster-sweep.service" 2>/dev/null || echo MISSING)"
check "bare unmasked call printed the success line" "enabled teamster-sweep.timer" "$(echo "$out" | grep -o 'enabled teamster-sweep.timer' || true)"
check "bare unmasked call invoked enable --now" "1" "$(printf '%s\n' "${SYSTEMCTL_CALLS[@]}" | grep -c '^enable --now teamster-sweep.timer$')"

echo ""
echo "--- 2b. install_and_enable_unit called bare with a FAILING source: set -e aborts (the touchstone MAJOR fix) ---"
# A prior version of this fix wrapped every call in \`|| true\` to survive the
# masked-unit return code — but bash disables set -e for a function's ENTIRE
# body when it's invoked in an ||/&& list, so that also swallowed a genuine
# \`sudo install\` failure inside the SAME function on every unmasked call.
# This proves the fixed (bare-call) shape restores the original abort
# behavior: a failing source file must stop the script, not log a false
# "installed" line and continue.
HARNESS="$TMPDIR/failing-install-harness.sh"
{
    echo 'set -euo pipefail'
    echo "$INSTALL_AND_ENABLE_SRC"
    echo 'sudo() { "$@"; }'
    echo 'systemctl() { return 0; }'
    echo "dlog() { echo \"DLOG:\$*\" >> '$TMPDIR/dlog-capture.txt'; }"
    echo "C_RESET=''; C_BOLD_CYAN=''; C_YELLOW=''"
    echo "SYSTEMD_UNIT_DIR='$TMPDIR/bare-failure'"
    echo "mkdir -p \"\$SYSTEMD_UNIT_DIR\""
    echo 'echo MARKER-BEFORE'
    echo "install_and_enable_unit install.sweep-timer '' teamster-sweep.timer 1 'enabled teamster-sweep.timer' '$TMPDIR/src/does-not-exist.service'"
    echo 'echo MARKER-AFTER-UNREACHABLE'
} > "$HARNESS"
set +e
bash "$HARNESS" > "$TMPDIR/failing-install-out.txt" 2>&1
harness_rc=$?
set -e
harness_out=$(cat "$TMPDIR/failing-install-out.txt")
check "bare failing install: harness exits nonzero" "nonzero" "$([[ "$harness_rc" -ne 0 ]] && echo nonzero || echo zero)"
check "bare failing install: must NOT continue past the failure" "did-not-continue" "$(echo "$harness_out" | grep -q MARKER-AFTER-UNREACHABLE && echo continued || echo did-not-continue)"
check "bare failing install: no false 'installed' dlog line recorded" "absent" "$([[ -f "$TMPDIR/dlog-capture.txt" ]] && grep -q '"installed"' "$TMPDIR/dlog-capture.txt" 2>/dev/null && echo present || echo absent)"

echo ""
echo "--- 2c. install_and_enable_unit's dlog line: one space-free 'src=' pair per source, not one space-joined value ---"
# The file's own debug-log grammar (Round 0, top of file) is locked at
# "<msg>[ k=v ...]" — a single value containing a space is ambiguous to any
# parser splitting the trailer on whitespace. Verifies the fix directly
# rather than trusting the code comment.
DLOG_CAPTURE=()
# shellcheck disable=SC2317 # called before being restored to the no-op two lines below — not dead code
dlog() { DLOG_CAPTURE+=("$*"); }
install_and_enable_unit install.sweep-timer "" teamster-sweep.timer 1 "enabled teamster-sweep.timer" \
    "$TMPDIR/src/teamster-sweep.service" "$TMPDIR/src/teamster-sweep.timer" > /dev/null
dlog_line="${DLOG_CAPTURE[-1]}"
check "dlog 'installed' line has exactly 2 space-free src= pairs, not 1 joined value" "src=$TMPDIR/src/teamster-sweep.service src=$TMPDIR/src/teamster-sweep.timer" "$(echo "$dlog_line" | sed -n 's/^INFO install\.sweep-timer installed //p')"
# shellcheck disable=SC2086 # word-split intentional: inspect each whitespace-separated token of the captured line for an embedded space
check "no pair's value itself contains an internal space" "0" "$(printf '%s\n' $dlog_line | grep -c '^src=.* .*$' || true)"
dlog() { :; }

# run_block_case <label> <block-src> <anchor-unit> <case: masked|unmasked> <basedir-fixture-cmds...>
# Builds a standalone script combining the real, extracted call-site block
# (comment header through closing fi) with stubs and fixtures, runs it under
# `set -euo pipefail`, and returns its stdout + exit code via globals
# BLOCK_OUT / BLOCK_RC / BLOCK_UNIT_DIR.
run_block_case() {
    local label="$1" block_src="$2" anchor_unit="$3" case_kind="$4"
    local unit_dir="$TMPDIR/${label}-${case_kind}"
    mkdir -p "$unit_dir" "$TMPDIR/etc"
    echo "svc source" > "$TMPDIR/etc/${anchor_unit%.timer}.service"
    echo "timer source" > "$TMPDIR/etc/${anchor_unit}"
    if [[ "$case_kind" == masked ]]; then
        ln -sf /dev/null "$unit_dir/$anchor_unit"
    fi
    local harness="$TMPDIR/${label}-${case_kind}-harness.sh"
    {
        echo 'set -euo pipefail'
        echo "$UNIT_IS_MASKED_SRC"
        echo "$MASKED_NOTICE_SRC"
        echo "$INSTALL_AND_ENABLE_SRC"
        echo 'sudo() { "$@"; }'
        echo 'systemctl() { return 0; }'
        echo 'dlog() { :; }'
        echo "C_RESET=''; C_BOLD_CYAN=''; C_YELLOW=''"
        echo "SYSTEMD_UNIT_DIR='$unit_dir'"
        echo "WIRE=1; HOOKD_READ_ONLY=0; HOOKD_MODE=''; BASEDIR='$TMPDIR'"
        echo 'echo MARKER-BEFORE'
        echo "$block_src"
        echo 'echo MARKER-AFTER'
    } > "$harness"
    set +e
    bash "$harness" > "$TMPDIR/${label}-${case_kind}-out.txt" 2>&1
    BLOCK_RC=$?
    set -e
    BLOCK_OUT=$(cat "$TMPDIR/${label}-${case_kind}-out.txt")
    BLOCK_UNIT_DIR="$unit_dir"
}

echo ""
echo "--- 3a. real sweep block (one of the three R11-masked units), extracted verbatim ---"
run_block_case sweep "$SWEEP_BLOCK_SRC" teamster-sweep.timer masked
check "sweep/masked: script continues past the block (set -e did not abort)" "0" "$BLOCK_RC"
check "sweep/masked: printed the generic left-masked line" "1" "$(echo "$BLOCK_OUT" | grep -c 'left masked: teamster-sweep.timer (masked on this host; the installer never unmasks)')"
check "sweep/masked: message does not name R11/clone" "0" "$(echo "$BLOCK_OUT" | grep -c 'R11\|clone target')"
check "sweep/masked: destination is still the /dev/null symlink" "/dev/null" "$(readlink "$BLOCK_UNIT_DIR/teamster-sweep.timer")"

run_block_case sweep "$SWEEP_BLOCK_SRC" teamster-sweep.timer unmasked
check "sweep/unmasked: script completes normally" "0" "$BLOCK_RC"
check "sweep/unmasked: installed the timer with source content" "timer source" "$(cat "$BLOCK_UNIT_DIR/teamster-sweep.timer" 2>/dev/null || echo MISSING)"
check "sweep/unmasked: printed the success line" "1" "$(echo "$BLOCK_OUT" | grep -c 'enabled teamster-sweep.timer')"

echo ""
echo "--- 3b. real classify block (a unit teamster clone NEVER masks — proves the guard is not R11-specific) ---"
run_block_case classify "$CLASSIFY_BLOCK_SRC" teamster-classify.timer masked
check "classify/masked-by-hand: script continues past the block" "0" "$BLOCK_RC"
check "classify/masked-by-hand: printed the generic left-masked line" "1" "$(echo "$BLOCK_OUT" | grep -c 'left masked: teamster-classify.timer (masked on this host; the installer never unmasks)')"
check "classify/masked-by-hand: destination is still the /dev/null symlink" "/dev/null" "$(readlink "$BLOCK_UNIT_DIR/teamster-classify.timer")"

run_block_case classify "$CLASSIFY_BLOCK_SRC" teamster-classify.timer unmasked
check "classify/unmasked: script completes normally" "0" "$BLOCK_RC"
check "classify/unmasked: installed the timer with source content" "timer source" "$(cat "$BLOCK_UNIT_DIR/teamster-classify.timer" 2>/dev/null || echo MISSING)"

# run_hookd_case <case: masked|unmasked>: runs the real, extracted hookd
# systemd) case-arm (wh2-hookd-enable-on-install) as its own bash process —
# unlike run_block_case's in-process systemctl/curl stubs, this arm's
# harness must run as a separate `bash` invocation because it contains its
# own `case ... esac`, so call-capture goes through files, not arrays,
# to survive the process boundary. Returns via globals HOOKD_RC / HOOKD_OUT /
# HOOKD_SYSTEMCTL_CALLS / HOOKD_CURL_CALLS / HOOKD_UNIT_DIR.
run_hookd_case() {
    local case_kind="$1"
    local unit_dir="$TMPDIR/hookd-${case_kind}"
    mkdir -p "$unit_dir"
    if [[ "$case_kind" == masked ]]; then
        ln -sf /dev/null "$unit_dir/teamster-hookd.service"
    fi
    local calls_file="$TMPDIR/hookd-${case_kind}-systemctl-calls.txt"
    local curl_file="$TMPDIR/hookd-${case_kind}-curl-calls.txt"
    : > "$calls_file"
    : > "$curl_file"
    local harness="$TMPDIR/hookd-${case_kind}-harness.sh"
    {
        echo 'set -euo pipefail'
        echo "$UNIT_IS_MASKED_SRC"
        echo "$MASKED_NOTICE_SRC"
        echo 'sudo() { "$@"; }'
        echo "systemctl() { echo \"\$*\" >> '$calls_file'; return 0; }"
        echo "curl() { echo \"\$*\" >> '$curl_file'; return 0; }"
        echo 'dlog() { :; }'
        echo "C_RESET=''; C_BOLD_WHITE=''; C_GREEN=''; C_YELLOW=''"
        echo "SYSTEMD_UNIT_DIR='$unit_dir'"
        echo 'RUNNING_MODE=systemd'
        echo 'HOOKD_PORT=9125'
        echo 'echo MARKER-BEFORE'
        # shellcheck disable=SC2016 # single-quoted on purpose: this is literal text written into the harness file, expanded when THAT script runs, not now
        echo 'case "$RUNNING_MODE" in'
        echo "$HOOKD_SYSTEMD_ARM_SRC"
        echo 'esac'
        echo 'echo MARKER-AFTER'
    } > "$harness"
    set +e
    bash "$harness" > "$TMPDIR/hookd-${case_kind}-out.txt" 2>&1
    HOOKD_RC=$?
    set -e
    HOOKD_OUT=$(cat "$TMPDIR/hookd-${case_kind}-out.txt")
    HOOKD_SYSTEMCTL_CALLS=$(cat "$calls_file")
    HOOKD_CURL_CALLS=$(cat "$curl_file")
    HOOKD_UNIT_DIR="$unit_dir"
}

echo ""
echo "--- 4. hookd's real systemd) restart arm (wh2-hookd-enable-on-install): masked -> left masked, no enable; unmasked -> enable --now + health check ---"
run_hookd_case masked
check "hookd/masked: script continues past the block" "0" "$HOOKD_RC"
check "hookd/masked: printed the generic left-masked line" "1" "$(echo "$HOOKD_OUT" | grep -c 'left masked: teamster-hookd.service (masked on this host; the installer never unmasks)')"
check "hookd/masked: systemctl never invoked" "" "$HOOKD_SYSTEMCTL_CALLS"
check "hookd/masked: curl health check never attempted" "" "$HOOKD_CURL_CALLS"
check "hookd/masked: destination is still the /dev/null symlink" "/dev/null" "$(readlink "$HOOKD_UNIT_DIR/teamster-hookd.service")"

run_hookd_case unmasked
check "hookd/unmasked: script completes normally" "0" "$HOOKD_RC"
check "hookd/unmasked: invoked enable --now on teamster-hookd" "1" "$(echo "$HOOKD_SYSTEMCTL_CALLS" | grep -c '^enable --now teamster-hookd$')"
check "hookd/unmasked: printed the healthy success line" "1" "$(echo "$HOOKD_OUT" | grep -c 'hookd healthy (port 9125)')"

echo ""
echo "--- 5. otelcol/prometheus/grafana supervisor-group blocks (wh2-supervisor-systemd-units), each masked and unmasked ---"
# Same install_and_enable_unit shape as sweep/classify (section 3), so
# run_block_case is reused as-is rather than forked. run_block_case's fixture
# writer names its second file after anchor_unit verbatim and always labels
# its content "timer source" (built for the service+timer pairs sweep/classify
# install) — these three blocks pass a single, non-timer .service file to
# install_and_enable_unit, so the installed content is that same "timer
# source" string under a different, correct name; the mismatched label is a
# quirk of reusing the generic helper for a non-timer unit, not a bug in the
# block under test, and is asserted as such below rather than worked around.
for name in otelcol prometheus grafana; do
    unit="teamster-${name}.service"
    case "$name" in
        otelcol)    block_src="$OTELCOL_BLOCK_SRC" ;;
        prometheus) block_src="$PROMETHEUS_BLOCK_SRC" ;;
        grafana)    block_src="$GRAFANA_BLOCK_SRC" ;;
    esac

    run_block_case "$name" "$block_src" "$unit" masked
    check "$name/masked: script continues past the block (set -e did not abort)" "0" "$BLOCK_RC"
    check "$name/masked: printed the generic left-masked line" "1" "$(echo "$BLOCK_OUT" | grep -c "left masked: $unit (masked on this host; the installer never unmasks)")"
    check "$name/masked: destination is still the /dev/null symlink" "/dev/null" "$(readlink "$BLOCK_UNIT_DIR/$unit")"

    run_block_case "$name" "$block_src" "$unit" unmasked
    check "$name/unmasked: script completes normally" "0" "$BLOCK_RC"
    check "$name/unmasked: installed the unit with source content" "timer source" "$(cat "$BLOCK_UNIT_DIR/$unit" 2>/dev/null || echo MISSING)"
    check "$name/unmasked: printed the success line" "1" "$(echo "$BLOCK_OUT" | grep -c "enabled + started $unit")"
done

echo ""
echo "--- 6. WIRE=0 stage-only 'wire manually' advisory (wh2-installrunner-wire-help-mask): printed text, not executed code, masked and unmasked ---"
# run_snippet_case <label> <snippet-src> <case: masked|unmasked>: runs a
# standalone printed-advice snippet (no install_and_enable_unit/case-arm
# shape) with unit_is_masked in scope and BASEDIR set. Returns via globals
# SNIPPET_RC / SNIPPET_OUT.
run_snippet_case() {
    local label="$1" snippet_src="$2" case_kind="$3"
    local unit_dir="$TMPDIR/${label}-${case_kind}"
    mkdir -p "$unit_dir"
    if [[ "$case_kind" == masked ]]; then
        ln -sf /dev/null "$unit_dir/teamster-hookd.service"
    fi
    local harness="$TMPDIR/${label}-${case_kind}-snippet-harness.sh"
    {
        echo 'set -euo pipefail'
        echo "$UNIT_IS_MASKED_SRC"
        echo "C_RESET=''; C_YELLOW=''"
        echo "SYSTEMD_UNIT_DIR='$unit_dir'"
        echo "BASEDIR='$TMPDIR'"
        echo 'echo MARKER-BEFORE'
        echo "$snippet_src"
        echo 'echo MARKER-AFTER'
    } > "$harness"
    set +e
    bash "$harness" > "$TMPDIR/${label}-${case_kind}-snippet-out.txt" 2>&1
    SNIPPET_RC=$?
    set -e
    SNIPPET_OUT=$(cat "$TMPDIR/${label}-${case_kind}-snippet-out.txt")
}

run_snippet_case wire0 "$WIRE0_ADVICE_SRC" masked
check "wire0/masked: script continues past the block (set -e did not abort)" "0" "$SNIPPET_RC"
check "wire0/masked: prints the unmask-first warning" "1" "$(echo "$SNIPPET_OUT" | grep -c 'the installer never')"
check "wire0/masked: prints the systemctl unmask hint" "1" "$(echo "$SNIPPET_OUT" | grep -c 'sudo systemctl unmask teamster-hookd')"
check "wire0/masked: still also prints the wire-manually recipe (additive, not suppressed by the warning)" "1" "$(echo "$SNIPPET_OUT" | grep -c 'Or wire manually:')"

run_snippet_case wire0 "$WIRE0_ADVICE_SRC" unmasked
check "wire0/unmasked: script continues" "0" "$SNIPPET_RC"
check "wire0/unmasked: no unmask warning printed" "0" "$(echo "$SNIPPET_OUT" | grep -c 'the installer never')"
check "wire0/unmasked: prints the wire-manually recipe" "1" "$(echo "$SNIPPET_OUT" | grep -c 'Or wire manually:')"
check "wire0/unmasked: recipe says enable --now, not a plain start (matches what --wire actually does)" "1" "$(echo "$SNIPPET_OUT" | grep -c 'sudo systemctl enable --now teamster-hookd')"

echo ""
echo "--- 7. hookd's 'none)' running-mode arm advisory (same WU): printed text, masked and unmasked ---"
# run_none_arm_case <case: masked|unmasked>: same case-arm shape as
# run_hookd_case's systemd) arm, reused via extract_case_arm; this arm has no
# systemctl/curl calls of its own (everything is echoed text), only dlog,
# which is stubbed. Returns via globals NONE_RC / NONE_OUT.
run_none_arm_case() {
    local case_kind="$1"
    local unit_dir="$TMPDIR/none-arm-${case_kind}"
    mkdir -p "$unit_dir"
    if [[ "$case_kind" == masked ]]; then
        ln -sf /dev/null "$unit_dir/teamster-hookd.service"
    fi
    local harness="$TMPDIR/none-arm-${case_kind}-harness.sh"
    {
        echo 'set -euo pipefail'
        echo "$UNIT_IS_MASKED_SRC"
        echo 'dlog() { :; }'
        echo "SYSTEMD_UNIT_DIR='$unit_dir'"
        echo "BASEDIR='$TMPDIR'"
        echo 'RUNNING_MODE=none'
        echo 'echo MARKER-BEFORE'
        # shellcheck disable=SC2016 # single-quoted on purpose: literal text written into the harness file, expanded when THAT script runs, not now
        echo 'case "$RUNNING_MODE" in'
        echo "$NONE_ARM_SRC"
        echo 'esac'
        echo 'echo MARKER-AFTER'
    } > "$harness"
    set +e
    bash "$harness" > "$TMPDIR/none-arm-${case_kind}-out.txt" 2>&1
    NONE_RC=$?
    set -e
    NONE_OUT=$(cat "$TMPDIR/none-arm-${case_kind}-out.txt")
}

run_none_arm_case masked
check "none-arm/masked: script continues past the block" "0" "$NONE_RC"
check "none-arm/masked: prints the unmask-first note" "1" "$(echo "$NONE_OUT" | grep -c 'the installer never')"
check "none-arm/masked: does NOT print Start via: (if/else — mutually exclusive with the warning, unlike wire0's additive shape)" "0" "$(echo "$NONE_OUT" | grep -c 'Start via:')"
check "none-arm/masked: still prints the non-systemd manual fallback either way" "1" "$(echo "$NONE_OUT" | grep -cE 'Or manually: .*bin/hookd &')"

run_none_arm_case unmasked
check "none-arm/unmasked: script continues" "0" "$NONE_RC"
check "none-arm/unmasked: prints Start via: enable --now" "1" "$(echo "$NONE_OUT" | grep -c 'Start via: sudo systemctl enable --now teamster-hookd')"
check "none-arm/unmasked: no mask warning printed" "0" "$(echo "$NONE_OUT" | grep -c 'the installer never')"
check "none-arm/unmasked: still prints the non-systemd manual fallback either way" "1" "$(echo "$NONE_OUT" | grep -cE 'Or manually: .*bin/hookd &')"

echo ""
echo "--- 8. mcp-scraper block (WP11-TAILER-DESIGN.md §9, wh2-wp11-mcp-tailer): mask-first-then-yaml-check, same doctrine as WMS review-sweep ---"
# run_yaml_gated_block_case <label> <block-src> <anchor-unit> <case: masked|unmasked> <yaml: true|false|absent>
# Same shape as run_block_case, plus a teamster.yaml fixture under
# BASEDIR/etc so the block's own mcp-scraper.enabled grep has something (or
# nothing) to read. dlog is captured to a file rather than stubbed to a
# no-op (unlike run_block_case's other cases, this block's "skipped" path
# has no stdout of its own — it only calls dlog). Returns via globals
# BLOCK_OUT / BLOCK_RC / BLOCK_UNIT_DIR / BLOCK_DLOG.
run_yaml_gated_block_case() {
    local label="$1" block_src="$2" anchor_unit="$3" case_kind="$4" yaml_enabled="$5"
    local base="$TMPDIR/${label}-${case_kind}-${yaml_enabled}"
    local etc_dir="$base/etc"
    local unit_dir="$base/systemd"
    local dlog_file="$base/dlog-calls.txt"
    mkdir -p "$etc_dir" "$unit_dir"
    : > "$dlog_file"
    echo "svc source" > "$etc_dir/${anchor_unit%.timer}.service"
    echo "timer source" > "$etc_dir/${anchor_unit}"
    case "$yaml_enabled" in
        true)   printf 'mcp-scraper:\n  enabled: true\n' > "$etc_dir/teamster.yaml" ;;
        false)  printf 'mcp-scraper:\n  enabled: false\n' > "$etc_dir/teamster.yaml" ;;
        absent) : ;; # no teamster.yaml at all — the yaml-missing arm
    esac
    if [[ "$case_kind" == masked ]]; then
        ln -sf /dev/null "$unit_dir/$anchor_unit"
    fi
    local harness="$base/harness.sh"
    {
        echo 'set -euo pipefail'
        echo "$UNIT_IS_MASKED_SRC"
        echo "$MASKED_NOTICE_SRC"
        echo "$INSTALL_AND_ENABLE_SRC"
        echo 'sudo() { "$@"; }'
        echo 'systemctl() { return 0; }'
        echo "dlog() { echo \"\$*\" >> '$dlog_file'; }"
        echo "C_RESET=''; C_BOLD_CYAN=''; C_YELLOW=''"
        echo "SYSTEMD_UNIT_DIR='$unit_dir'"
        echo "WIRE=1; HOOKD_READ_ONLY=0; HOOKD_MODE=''; BASEDIR='$base'"
        echo 'echo MARKER-BEFORE'
        echo "$block_src"
        echo 'echo MARKER-AFTER'
    } > "$harness"
    set +e
    bash "$harness" > "$base/out.txt" 2>&1
    BLOCK_RC=$?
    set -e
    BLOCK_OUT=$(cat "$base/out.txt")
    BLOCK_DLOG=$(cat "$dlog_file")
    BLOCK_UNIT_DIR="$unit_dir"
}

run_yaml_gated_block_case mcpscraper "$MCP_SCRAPER_BLOCK_SRC" teamster-mcp-scraper.timer masked true
check "mcp-scraper/masked+enabled=true: script continues past the block" "0" "$BLOCK_RC"
check "mcp-scraper/masked+enabled=true: mask wins over enabled=true (checked FIRST)" "1" "$(echo "$BLOCK_OUT" | grep -c 'left masked: teamster-mcp-scraper.timer (masked on this host; the installer never unmasks)')"
check "mcp-scraper/masked+enabled=true: destination is still the /dev/null symlink" "/dev/null" "$(readlink "$BLOCK_UNIT_DIR/teamster-mcp-scraper.timer")"

run_yaml_gated_block_case mcpscraper "$MCP_SCRAPER_BLOCK_SRC" teamster-mcp-scraper.timer masked false
check "mcp-scraper/masked+enabled=false: still left masked (mask holds regardless of yaml)" "1" "$(echo "$BLOCK_OUT" | grep -c 'left masked: teamster-mcp-scraper.timer')"

run_yaml_gated_block_case mcpscraper "$MCP_SCRAPER_BLOCK_SRC" teamster-mcp-scraper.timer unmasked true
check "mcp-scraper/unmasked+enabled=true: script completes normally" "0" "$BLOCK_RC"
check "mcp-scraper/unmasked+enabled=true: installed the timer with source content" "timer source" "$(cat "$BLOCK_UNIT_DIR/teamster-mcp-scraper.timer" 2>/dev/null || echo MISSING)"
check "mcp-scraper/unmasked+enabled=true: printed the success line" "1" "$(echo "$BLOCK_OUT" | grep -c 'enabled teamster-mcp-scraper.timer')"

run_yaml_gated_block_case mcpscraper "$MCP_SCRAPER_BLOCK_SRC" teamster-mcp-scraper.timer unmasked false
check "mcp-scraper/unmasked+enabled=false: NOT installed" "MISSING" "$(cat "$BLOCK_UNIT_DIR/teamster-mcp-scraper.timer" 2>/dev/null || echo MISSING)"
check "mcp-scraper/unmasked+enabled=false: dlog'd the skipped line" "1" "$(echo "$BLOCK_DLOG" | grep -c 'skipped: mcp-scraper.enabled not true in teamster.yaml')"

run_yaml_gated_block_case mcpscraper "$MCP_SCRAPER_BLOCK_SRC" teamster-mcp-scraper.timer unmasked absent
check "mcp-scraper/unmasked+yaml-absent: treated as disabled, NOT installed" "MISSING" "$(cat "$BLOCK_UNIT_DIR/teamster-mcp-scraper.timer" 2>/dev/null || echo MISSING)"
check "mcp-scraper/unmasked+yaml-absent: dlog'd the skipped line" "1" "$(echo "$BLOCK_DLOG" | grep -c 'skipped: mcp-scraper.enabled not true in teamster.yaml')"

echo ""
echo "unit_is_masked + install_and_enable_unit + masked_unit_notice + hookd's systemd)/none) arms + the WIRE=0 stage-only advisory + the mcp-scraper mask-and-yaml gate, driven through 9 real advisory sites (sweep, classify, hookd systemd) arm, otelcol, prometheus, grafana, WIRE=0 wire-manually block, hookd none) arm, mcp-scraper), each masked and unmasked: $pass passed, $fail failed ($examined cases examined)"
[[ "$fail" -eq 0 ]]
