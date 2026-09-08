#!/usr/bin/env bash
# Acceptance harness for `teamster clone`.
#
# Loop: revert -> clone -> verify -> assert-no-source-writes -> revert.
# Design reference: teamster-clone-kit's DESIGN.md (architecture, rulings,
# the I1-I7 isolation contract) and WP4-validation.md (the narrative version
# of every assertion below, cross-referenced by A-number). This file is the
# executable half; read those for the "why" behind anything that looks
# over-engineered here.
#
# STATUS:
#   - Sections 1-2 (revert, source-unmodified proof) are real and runnable
#     today against any host with SSH to both SOURCE_HOST and TARGET_HOST.
#   - assert_content_fingerprint_match / assert_fresh_directory_enforced are
#     pure git/shell plumbing -- also runnable today, no dependency on
#     `teamster clone` existing.
#   - Everything else is a fully-specified assertion that composes into
#     run_clone, which is the one true stub: it documents the exact pipeline
#     shape `teamster clone` must implement (WP1 ref/ship, WP2 topology
#     translation + MySQL 8.4 install, WP3 data leg) and returns nonzero
#     until that command exists. Wiring it up once it lands is argument
#     plumbing, not a redesign -- the assertions underneath don't change.
#
# SOURCE_HOST is treated as read-only for the entire run: `teamster sql` has
# no read-only guard yet (see assert_source_read_only_guard_enforced), so
# every source-side query in this file is SELECT-only by discipline, no
# exceptions. TARGET_HOST must always be a disposable target -- never point
# it at anything else.
#
# Usage: scripts/clone-acceptance-test.sh
set -euo pipefail

SOURCE_HOST="${WP4_SOURCE_HOST:?WP4_SOURCE_HOST must be set}"
TARGET_HOST="${WP4_TARGET_HOST:?WP4_TARGET_HOST must be set}"
TARGET_IP="${WP4_TARGET_IP:-192.168.10.226}"
REVERT_HOST="${WP4_REVERT_HOST:?WP4_REVERT_HOST must be set}"
REVERT_PORT="${WP4_REVERT_PORT:-9177}"
REVERT_VM="${WP4_REVERT_VM:-chunk}"
SNAP_DIR="${WP4_SNAP_DIR:?WP4_SNAP_DIR must be set}"
mkdir -p "$SNAP_DIR"

CHECKS_RUN=0
CHECKS_PASSED=0
CHECKS_SKIPPED=0

# run_check invokes an assertion/test function, tallies the result, and
# distinguishes "not yet wired" (exit 2) from a real pass/fail -- most
# functions below can't do anything but skip until `teamster clone` exists.
run_check() {
	local name="$1"
	shift
	CHECKS_RUN=$((CHECKS_RUN + 1))
	if "$@"; then
		CHECKS_PASSED=$((CHECKS_PASSED + 1))
		return 0
	fi
	local rc=$?
	if [[ $rc -eq 2 ]]; then
		CHECKS_SKIPPED=$((CHECKS_SKIPPED + 1))
		echo "SKIP: $name" >&2
	fi
	return $rc
}

# =============================================================================
# section 1: revert protocol
# =============================================================================
# Wire format: raw TCP, newline-terminated command, single-line response,
# connection closes. Measured once, cleanly: ack ~2s, SSH-ready ~17s -- a
# single sample, not an SLA. Run a 5-sample study before treating this as a
# timing budget for anything (a CI timeout, a progress-bar estimate).

revert_target() {
	local sent_at ack_at resp
	sent_at=$(date +%s.%N)
	resp=$(printf 'revert %s\n' "$REVERT_VM" | timeout 10 nc -w 8 "$REVERT_HOST" "$REVERT_PORT")
	ack_at=$(date +%s.%N)
	echo "revert ack: $resp (latency $(echo "$ack_at - $sent_at" | bc)s)" >&2
	[[ "$resp" == OK* ]] || { echo "revert_target: unexpected response: $resp" >&2; return 1; }
}

# `who -b` reports the snapshot's original boot record, not when the revert
# actually completed -- do not use it as a completion signal. /proc/uptime
# resetting (or, more simply, retry-SSH succeeding) is the only valid one.
wait_target_ready() {
	local timeout_s="${1:-180}" start now
	start=$(date +%s.%N)
	for ((i = 0; i < timeout_s; i++)); do
		if ssh -o BatchMode=yes -o ConnectTimeout=2 -o StrictHostKeyChecking=no "$TARGET_HOST" true 2>/dev/null; then
			now=$(date +%s.%N)
			echo "target ready after $(echo "$now - $start" | bc)s" >&2
			return 0
		fi
		sleep 1
	done
	echo "wait_target_ready: timed out after ${timeout_s}s" >&2
	return 1
}

# =============================================================================
# section 2: source-unmodified proof
# =============================================================================
# The single most important assertion in the suite. The source is a live
# production system -- ordinary traffic legitimately mutates var/ and some DB
# tables during the test window. This proof is deliberately scoped to what
# should NEVER change if the clone operation genuinely never wrote to the
# source: schema/migration state, the systemd unit inventory, static config
# file checksums, and inbound-connection provenance. It is not a whole-
# filesystem checksum -- that would false-positive on the source's own
# normal operation and teach the team to ignore the alarm.

SOURCE_STATIC_PATHS=(
	"${WP4_SOURCE_BASEDIR:-$HOME/teamster}/etc"
	"${WP4_SOURCE_BASEDIR:-$HOME/teamster}/bin"
)

# Table enumeration is dynamic (SHOW TABLES against the live source), not a
# hand-picked list -- a fixed list is a whitelist that a partial restore
# (`mysql --force` continues past per-statement errors) can pass silently on
# any table the list omits. Every table the source actually has gets
# checked, so new tables are covered by default.
get_source_tables() {
	# claude_telemetry tables are qualified (claude_telemetry.<table>) since
	# claude_telemetry.sessions and teamster.sessions are two different
	# tables that happen to share a name.
	local teamster_tables telemetry_tables
	teamster_tables=$(ssh -o BatchMode=yes "$SOURCE_HOST" "teamster sql -N -e 'SHOW TABLES'")
	telemetry_tables=$(ssh -o BatchMode=yes "$SOURCE_HOST" "teamster sql -N -e 'SHOW TABLES FROM claude_telemetry'")
	printf '%s\n' "$teamster_tables"
	printf '%s\n' "$telemetry_tables" | sed 's/^/claude_telemetry./'
}

snapshot_source() {
	local label="$1" out="$SNAP_DIR/source-$1.snapshot"
	{
		echo "# source snapshot: $label @ $(date -u -Iseconds)"

		echo "## schema_version (must be byte-identical pre/post -- any drift"
		echo "## means an installer or migration ran against the source)"
		ssh -o BatchMode=yes "$SOURCE_HOST" \
			'teamster sql -N -e "SELECT MAX(version) FROM schema_version"' 2>&1 || echo "ERROR"

		echo "## roster: no clone-target entry should ever appear (I2/I6 --"
		echo "## a clone never registers into the source's roster)"
		ssh -o BatchMode=yes "$SOURCE_HOST" \
			'teamster sql -N -e "SELECT host FROM agent_roster WHERE host LIKE '"'"'%'"$REVERT_VM"'%'"'"'"' 2>&1 || echo "ERROR"

		echo "## static config/binary checksums"
		for p in "${SOURCE_STATIC_PATHS[@]}"; do
			ssh -o BatchMode=yes "$SOURCE_HOST" \
				"find '$p' -type f -exec sha256sum {} \\; 2>/dev/null | sort" || echo "ERROR: $p"
		done

		echo "## systemd unit inventory (teamster-* only)"
		ssh -o BatchMode=yes "$SOURCE_HOST" \
			"systemctl list-units --all --no-pager 'teamster-*' 2>&1"

		echo "## row counts, dynamic table enumeration"
		while IFS= read -r t; do
			[[ -z "$t" ]] && continue
			printf '%s\t' "$t"
			ssh -n -o BatchMode=yes "$SOURCE_HOST" \
				"teamster sql -N -e 'SELECT COUNT(*) FROM $t'" 2>&1 || echo "ERROR"
		done <<<"$(get_source_tables)"

		echo "## inbound SSH sessions from the target's IP during the test"
		echo "## window (I6 -- the target must never successfully reach in)"
		ssh -o BatchMode=yes "$SOURCE_HOST" \
			"journalctl -u ssh --since '-1 hour' --no-pager 2>&1 | grep -F '$TARGET_IP' || echo NONE"
	} >"$out"
	echo "$out"
}

diff_source_snapshots() {
	local pre="$1" post="$2"
	if diff -u "$pre" "$post"; then
		echo "PASS: source snapshot unchanged"
		return 0
	else
		echo "FAIL: source snapshot drifted -- see diff above. Hard stop." >&2
		return 1
	fi
}

# =============================================================================
# section 3: clone-side assertions (run against the target, post-clone)
# =============================================================================

# --- A1 / Stage A: teamster --version over SSH, not /health ---------------
# The Leg 2 gate reads the installed binary directly rather than /health: it
# needs no daemon running (hookd never auto-starts on a fresh install, so
# there is nothing to query at this point in the pipeline anyway), and it is
# the more direct source in any case -- /health only ever echoes the same
# -ldflags stamp.
#
# SCOPE, and it is narrower than it looks: this does NOT verify provenance.
# It reads back whatever TEAMSTER_COMMIT value clone itself injected via the
# env var -- if the installer compiled from the wrong directory, the stamp
# is still the value clone injected, and this check passes regardless. It
# verifies "did the env-var export reach the compiled binary" (a build-
# pipeline check), not "is the tree the installer compiled from actually the
# resolved commit" (a provenance check). The real provenance gate is
# assert_content_fingerprint_match below -- it runs earlier, before the
# installer is even invoked, independent of anything clone injects. Keep
# both; neither subsumes the other.
assert_version_commit_gate() {
	# Post-fix this must assert the real hash matches, not merely "not
	# none" -- a lazy check that only verifies non-"none" would pass on a
	# WRONG-but-non-none commit just as easily as the right one.
	local expected_full="$1"
	local out commit
	out=$(ssh -o BatchMode=yes "$TARGET_HOST" "teamster --version")
	commit=$(printf '%s' "$out" | python3 -c 'import sys,re; m=re.search(r"\(([0-9a-f]+)", sys.stdin.read()); print(m.group(1) if m else "")')
	if [[ "$commit" == "none" || -z "$commit" ]]; then
		echo "FAIL: teamster --version reports commit '$commit' -- TEAMSTER_COMMIT passthrough not wired" >&2
		return 1
	fi
	case "$expected_full" in
	"$commit"*) echo "PASS: teamster --version commit '$commit' is a prefix of '$expected_full'" ;;
	*)
		echo "FAIL: teamster --version commit '$commit' does not prefix-match '$expected_full' -- must not proceed to the data leg" >&2
		return 1
		;;
	esac
}

# --- A11: the real provenance gate -----------------------------------------
# Runs at Leg 2, immediately after enforced-fresh extraction, before the
# installer is ever invoked. Unlike most assertions in this file, this one
# is genuinely runnable today: pure git/shell plumbing, no dependency on
# `teamster clone` existing.

compute_expected_fingerprint() {
	# Run at the source, once <full_hash> is resolved.
	local source_repo="$1" full_hash="$2"
	git -C "$source_repo" ls-tree -r "$full_hash" | sha256sum | awk '{print $1}'
}

compute_actual_fingerprint() {
	# Run at the target, right after extraction, before the installer runs.
	# `git hash-object` is git's blob-hash primitive and needs no .git dir,
	# so this reconstructs the same listing shape as compute_expected_
	# fingerprint's ls-tree-based one over the actual extracted files.
	local target_dir="$1"
	ssh -o BatchMode=yes "$TARGET_HOST" "find '$target_dir' -type f -print0 | sort -z \
		| while IFS= read -r -d '' f; do
			printf '%s %s\n' \"\$(git hash-object \"\$f\")\" \"\${f#$target_dir/}\"
		  done | sha256sum | awk '{print \$1}'"
}

assert_content_fingerprint_match() {
	# On mismatch: hard-fail before the installer runs at all. Catches a
	# wrong-directory install, a corrupted-but-checksum-matching extraction,
	# and (given extraction is enforced-fresh, see assert_fresh_directory_
	# enforced) a stale-reuse scenario with nothing else to detect it.
	# Independent of assert_version_commit_gate above -- that one reads back
	# what clone itself injected; this one verifies tree content against the
	# resolved commit's git blob hashes, which clone had no hand in.
	local expected_fingerprint="$1" target_dir="$2"
	local actual_fingerprint
	actual_fingerprint=$(compute_actual_fingerprint "$target_dir")
	if [[ "$expected_fingerprint" != "$actual_fingerprint" ]]; then
		echo "FAIL: content-manifest mismatch (expected=$expected_fingerprint actual=$actual_fingerprint) -- wrong directory, corrupted extraction, or stale reuse" >&2
		return 1
	fi
	echo "PASS: extracted tree content matches resolved commit's git blob hashes"
}

# --- A12: fresh-directory enforcement ---------------------------------------
assert_fresh_directory_enforced() {
	# `mkdir <target-dir>` with no -p must fail loudly (nonzero exit, "File
	# exists") on a pre-existing path -- structurally impossible to silently
	# reuse or overwrite a leftover directory from an aborted prior run.
	# Caller pre-seeds a stale directory, invokes clone a second time
	# against the same hash, and passes clone's own exit code here.
	local clone_exit_code="$1"
	if [[ "$clone_exit_code" -eq 0 ]]; then
		echo "FAIL: clone succeeded against a pre-existing target directory -- fresh-directory enforcement did not fire" >&2
		return 1
	fi
	echo "PASS: clone refused a pre-existing target directory (exit $clone_exit_code)"
}

# --- /health + build_info: post-restore confirmation only, never a gate ---
# CRITICAL CAVEAT: /health and build_info are NOT independent verification
# of the stamp -- both derive from the same -ldflags values
# assert_version_commit_gate already checked. They confirm hookd is up,
# reachable, and serving correctly after its genuine first start; they do
# not add a second independent channel of identity confidence.

assert_health_commit_confirmation() {
	local expected_full="$1"
	local resp commit
	resp=$(curl -fsS "http://${TARGET_HOST#*@}:9125/health")
	commit=$(printf '%s' "$resp" | python3 -c 'import sys,json; print(json.load(sys.stdin)["commit"])')
	case "$expected_full" in
	"$commit"*) echo "PASS (confirmation only): /health commit '$commit' prefix-matches" ;;
	*)
		echo "FAIL (confirmation): /health commit '$commit' does not prefix-match '$expected_full' -- serving/transport fault, since the gate already passed on this hash" >&2
		return 1
		;;
	esac
}

assert_health_version_dirty() {
	# The version string synthesizes the -dirty suffix unconditionally
	# whenever --allow-dirty was used, checked on both channels: the
	# --version gate output and the post-restore /health confirmation.
	# Format varies with whether the resolved commit has a reachable tag
	# (e.g. "v0.2.6-dirty" vs "some-tag-1-g<hash>-dirty") -- the check below
	# is deliberately format-agnostic (suffix presence only), not pinned to
	# either base string.
	local expect_dirty="$1"
	local ssh_version resp health_version
	ssh_version=$(ssh -o BatchMode=yes "$TARGET_HOST" "teamster --version")
	resp=$(curl -fsS "http://${TARGET_HOST#*@}:9125/health")
	health_version=$(printf '%s' "$resp" | python3 -c 'import sys,json; print(json.load(sys.stdin)["version"])')
	local fail=0
	for pair in "gate:$ssh_version" "confirmation:$health_version"; do
		local label="${pair%%:*}" version="${pair#*:}"
		if [[ "$expect_dirty" == "true" ]]; then
			[[ "$version" == *-dirty* ]] || { echo "FAIL: $label version '$version' missing -dirty suffix on a dirty ship" >&2; fail=1; }
		else
			[[ "$version" != *-dirty* ]] || { echo "FAIL: $label version '$version' shows -dirty on a clean ship" >&2; fail=1; }
		fi
	done
	[[ $fail -eq 0 ]] && echo "PASS: dirty-decoration state correct on both channels"
	return $fail
}

# --- A2: row counts match, source vs target --------------------------------
assert_row_counts_match() {
	local fail=0
	local tables
	tables=$(get_source_tables)
	while IFS= read -r t; do
		[[ -z "$t" ]] && continue
		local src tgt
		src=$(ssh -n -o BatchMode=yes "$SOURCE_HOST" "teamster sql -N -e 'SELECT COUNT(*) FROM $t'")
		tgt=$(ssh -n -o BatchMode=yes "$TARGET_HOST" "teamster sql -N -e 'SELECT COUNT(*) FROM $t'")
		if [[ "$src" != "$tgt" ]]; then
			echo "FAIL: $t row count mismatch (source=$src target=$tgt)" >&2
			fail=1
		fi
	done <<<"$tables"
	return $fail
}

# --- I1, layer 1 of 2: config.tar.gz never transferred ---------------------
assert_config_not_shipped() {
	# The transfer step itself must never ship teamster/config.tar.gz -- not
	# just have restore discard it after arrival. This is the stronger
	# guarantee of the two.
	local transferred_dir="$1"
	if ssh -o BatchMode=yes "$TARGET_HOST" "test -f '$transferred_dir/teamster/config.tar.gz'"; then
		echo "FAIL: teamster/config.tar.gz was transferred -- should never leave the source" >&2
		return 1
	fi
	echo "PASS: config.tar.gz absent from transferred backup dir"
}

# --- I1, layer 2 of 2: whole etc/ tree unchanged by restore ----------------
capture_etc_tree_manifest() {
	# Snapshot every file under the clone's etc/ dir, checksummed. Call
	# right after install (Leg 2), before quiesce/restore (Leg 3).
	ssh -o BatchMode=yes "$TARGET_HOST" \
		"find ~/teamster/etc -type f -exec sha256sum {} \\; | sort"
}

assert_etc_tree_unchanged_by_restore() {
	# Checksums every file under etc/, not a named list -- restore's
	# otel/otelcol.yaml write (RestoreOTel writes into the target's own
	# cfg.Files entry) is not host-neutral: it carries the source's
	# deployment.environment and a Prometheus remote-write target that won't
	# match the clone's self-assigned port. A one-file check would never
	# catch that; the whole-tree check does, by construction.
	#
	# TIMING IS LOAD-BEARING: caller must call this BEFORE hookd's first
	# start, or it will false-fail on a correct run. otelcol.yaml is
	# genuinely re-rendered by teamster's own supervisor (StartOtelcol ->
	# renderOtelcolConfig) on every start, not by the installer -- a clone
	# is install-mode by construction, so its supervisor's first start
	# (after restore, in this pipeline) legitimately re-renders it with the
	# clone's own --env=clone label and self-assigned port. That is correct,
	# expected behavior, not an I1 violation. Do not "fix" a future failure
	# here by excluding otelcol.yaml -- fix it by capturing the second
	# manifest before the first `teamster start` ever runs. See
	# assert_only_expected_files_rendered_by_first_start for the (correctly
	# scoped) check that covers that later window instead of ignoring it.
	#
	# Caller must pass capture_etc_tree_manifest's output from right after
	# install, before restore ran. This function captures its own second
	# manifest immediately -- call it right after restore completes and
	# before the pipeline's first `teamster start` invocation.
	local post_install_manifest="$1"
	local post_restore_pre_start_manifest
	post_restore_pre_start_manifest=$(capture_etc_tree_manifest)
	if [[ "$post_install_manifest" != "$post_restore_pre_start_manifest" ]]; then
		echo "FAIL: etc/ tree changed between install and restore (before any start):" >&2
		diff <(printf '%s' "$post_install_manifest") <(printf '%s' "$post_restore_pre_start_manifest") >&2
		return 1
	fi
	echo "PASS: entire etc/ tree unchanged by restore (checked before first start)"
	# Callers needing this manifest for assert_only_expected_files_rendered_
	# by_first_start should call capture_etc_tree_manifest a second time
	# themselves, immediately before invoking this function, and keep that
	# copy -- it can't be returned via stdout from a function that also
	# echoes PASS/FAIL text.
}

assert_only_expected_files_rendered_by_first_start() {
	# The first-start transition is `teamster start`, which launches hookd
	# and every install-mode service on the same call (a fresh --wire
	# install's supervisor-launch block never fires on its own -- see
	# run_clone step 8g). Each of otelcol/prometheus/grafana legitimately
	# renders into etc/ on that first launch:
	#   - StartOtelcol -> renderOtelcolConfig writes etc/otelcol.yaml
	#   - StartPrometheus -> renderPrometheusConfig writes etc/prometheus.yaml
	#   - StartGrafana -> renderGrafanaConfigs writes etc/grafana/grafana.ini,
	#     two provisioning YAMLs, and copies dashboard JSONs
	#   - Grafana's secret_key and other persistent-secret files live under
	#     var/grafana/, not etc/ -- they never enter this manifest at all.
	# This is the complete, source-verified set of files this specific
	# transition legitimately touches -- checked exhaustively against
	# "anything else changed," not assumed safe by category or by one
	# filename. If a new install-mode service starts rendering into etc/,
	# this exclusion list needs updating from the source, not guessed.
	local pre_first_start_manifest="$1"
	local post_first_start_manifest
	post_first_start_manifest=$(capture_etc_tree_manifest)
	local changed_paths unexpected
	changed_paths=$(diff <(printf '%s\n' "$pre_first_start_manifest") <(printf '%s\n' "$post_first_start_manifest") \
		| grep -E '^[<>]' | awk '{print $NF}' | sort -u)
	unexpected=$(printf '%s\n' "$changed_paths" | grep -vE \
		'otelcol\.yaml$|prometheus\.yaml$|grafana/grafana\.ini$|grafana/provisioning/datasources/teamster\.yaml$|grafana/provisioning/dashboards/teamster\.yaml$|grafana/dashboards/.*\.json$' \
		|| true)
	if [[ -n "$unexpected" ]]; then
		echo "FAIL: unexpected etc/ file(s) changed at first start (beyond known otelcol/prometheus/grafana renders): $unexpected" >&2
		return 1
	fi
	echo "PASS: only the known config renders happened at first start"
}

# --- A13: managed services actually running, not just declared ------------
assert_managed_services_running() {
	# A fresh --wire install can exit 0 with otelcol-contrib, Prometheus,
	# and Grafana all down (the supervisor-launch block is gated on a PID
	# file a fresh target cannot have). This is what catches that: process
	# liveness, not the installer's exit code, not "the systemd unit
	# exists," not A3's config-declares-mode check (declared intent, not
	# runtime state).
	#
	# HONEST LIMIT: the pgrep checks below are solid. The port-discovery
	# greps against prometheus.yaml/grafana.ini are unverified sketches
	# against the skel templates' presumed format -- verify against a real
	# rendered config the first time this runs for real. If the format
	# doesn't match, they NOTE-and-skip rather than false-fail (the safe
	# direction), but that also means they may currently check nothing.
	local fail=0

	for bin in bin/hookd bin/otelcol-contrib bin/prometheus bin/grafana-server; do
		local running
		running=$(ssh -o BatchMode=yes "$TARGET_HOST" "pgrep -f '$bin' >/dev/null 2>&1 && echo yes || echo no")
		if [[ "$running" != "yes" ]]; then
			echo "FAIL: no running process for $bin" >&2
			fail=1
		fi
	done

	local prom_port
	prom_port=$(ssh -o BatchMode=yes "$TARGET_HOST" \
		"grep -oE 'listen-address[^0-9]*[0-9]+' ~/teamster/etc/prometheus.yaml 2>/dev/null | grep -oE '[0-9]+$'" || true)
	if [[ -n "$prom_port" ]]; then
		ssh -o BatchMode=yes "$TARGET_HOST" "curl -fsS http://127.0.0.1:$prom_port/-/healthy" >/dev/null \
			|| { echo "FAIL: prometheus port $prom_port not answering /-/healthy" >&2; fail=1; }
	else
		echo "NOTE: could not read prometheus's listen port from rendered config -- port-health check skipped" >&2
	fi

	local graf_port
	graf_port=$(ssh -o BatchMode=yes "$TARGET_HOST" \
		"grep -oE '^http_port\s*=\s*[0-9]+' ~/teamster/etc/grafana/grafana.ini 2>/dev/null | grep -oE '[0-9]+$'" || true)
	if [[ -n "$graf_port" ]]; then
		ssh -o BatchMode=yes "$TARGET_HOST" "curl -fsS http://127.0.0.1:$graf_port/api/health" >/dev/null \
			|| { echo "FAIL: grafana port $graf_port not answering /api/health" >&2; fail=1; }
	else
		echo "NOTE: could not read grafana's http_port from rendered config -- port-health check skipped" >&2
	fi
	# hookd's own liveness/correctness is already covered by
	# assert_health_commit_confirmation -- not duplicated here.

	[[ $fail -eq 0 ]] && echo "PASS: hookd, otelcol-contrib, prometheus, grafana all have running processes"
	return $fail
}

# --- A8: schema actually migrated, not inferred from the installer's exit
# code (which is non-fatal on migration failure) -----------------------------
assert_schema_migrated() {
	# The install-time migration call is `... && printf applied || printf
	# "WARN: migration failed"`, exit 0 either way, and hookd never auto-
	# starts to provide a second migration chance -- so this is the only
	# check that actually exists for "did the schema migrate." Compare
	# target's schema_version (captured right after install, before
	# quiesce/restore) against the source's current value: same commit is
	# already guaranteed by Leg 1/I7, and the source (production) is by
	# definition fully migrated, so its current version is simply the
	# target's expected one.
	local src_version tgt_version
	src_version=$(ssh -o BatchMode=yes "$SOURCE_HOST" "teamster sql -N -e 'SELECT MAX(version) FROM schema_version'")
	tgt_version=$(ssh -o BatchMode=yes "$TARGET_HOST" "teamster sql -N -e 'SELECT MAX(version) FROM schema_version'" 2>&1 || true)
	if [[ -z "$tgt_version" ]] || [[ "$tgt_version" == *ERROR* ]] || [[ "$tgt_version" == *"doesn't exist"* ]]; then
		echo "FAIL: target schema_version unreadable ('$tgt_version') -- migration did not run despite install reporting success" >&2
		return 1
	fi
	if [[ "$src_version" != "$tgt_version" ]]; then
		echo "FAIL: target schema_version ($tgt_version) does not match source ($src_version)" >&2
		return 1
	fi
	echo "PASS: target schema migrated to v$tgt_version, matches source -- must run before quiesce/restore, not after"
}

# --- A3: all five services teamster-managed, not external ------------------
assert_services_managed() {
	local yaml
	yaml=$(ssh -o BatchMode=yes "$TARGET_HOST" "cat ~/teamster/etc/teamster.yaml")
	for key in 'hookd:' 'store:' 'otelcol:' 'prometheus:' 'grafana:'; do
		echo "$yaml" | grep -A2 "^$key" | grep -qE 'mode: *(install|managed|systemd)' \
			|| { echo "FAIL: $key not install/managed/systemd on clone" >&2; return 1; }
	done
	echo "PASS: all five services teamster-managed"
}

# =============================================================================
# section 3b: cheap dry-run tier (I2/I3/I4/I5)
# =============================================================================
# Zero network/target access needed -- these run before the target is even
# reachable. $1 in each function is the captured stdout of:
#   teamster clone --dry-run --repo-dir=<path> <target>
# which prints the resolved flag vector verbatim, one flag per line.

# --- A3a / I4: all five --*-mode flags explicit -----------------------------
assert_dryrun_five_mode_flags() {
	local dryrun_output="$1" fail=0
	for flag in '--hookd-mode=systemd' '--store-mode=install' '--otelcol-mode=install' \
		'--prometheus-mode=install' '--grafana-mode=install'; do
		echo "$dryrun_output" | grep -qF -- "$flag" \
			|| { echo "FAIL: dry-run output missing $flag" >&2; fail=1; }
	done
	return $fail
}

# --- I2: relay dropped, never remapped --------------------------------------
assert_dryrun_no_relay() {
	# Caller's fixture must include a populated relay: block so this proves
	# real suppression, not a false negative from an empty source.
	local dryrun_output="$1"
	if echo "$dryrun_output" | grep -qE -- '--relay-mode|--relay-target|--repl-push-remote'; then
		echo "FAIL: dry-run output contains relay flags -- should be dropped entirely" >&2
		return 1
	fi
	echo "PASS: no relay flags in dry-run output"
}

# --- I3: store.dsn regenerated at the target, never copied -----------------
assert_dryrun_no_store_dsn() {
	local dryrun_output="$1"
	if echo "$dryrun_output" | grep -qF -- '--store-dsn'; then
		echo "FAIL: dry-run output contains --store-dsn -- must be absent, not just different" >&2
		return 1
	fi
	echo "PASS: no --store-dsn in dry-run output"
}

# --- I5: no clone path under forbidden basedirs -----------------------------
# I5 uses a configurable clone.forbidden_basedirs list in the source's
# teamster.yaml (not a hardcoded prefix). TEST_FORBIDDEN_BASEDIR
# is a generic path standing in for whatever the operator has actually
# configured -- caller must add it to clone.forbidden_basedirs on the source
# before invoking dry-run.
TEST_FORBIDDEN_BASEDIR="/shared/nfs"

assert_dryrun_basedir_refusal() {
	# A "does it refuse" test, not a "grep the output" test -- Translate()
	# returns an error, not a flag vector, for a violating --basedir. Caller
	# must configure clone.forbidden_basedirs on the source to include
	# $TEST_FORBIDDEN_BASEDIR, invoke with --basedir=$TEST_FORBIDDEN_BASEDIR/whatever,
	# and pass the resulting exit code + stderr here.
	local exit_code="$1" stderr_output="$2"
	[[ "$exit_code" -ne 0 ]] || { echo "FAIL: dry-run with forbidden basedir should have refused (exit 0)" >&2; return 1; }
	echo "$stderr_output" | grep -qiF -e 'I5' -e "$TEST_FORBIDDEN_BASEDIR" \
		|| { echo "FAIL: refusal doesn't name I5 or $TEST_FORBIDDEN_BASEDIR -- silent-different-vector risk" >&2; return 1; }
	echo "PASS: forbidden basedir refused cleanly"
}

# --- A4/I2 and A5/I5: live confirmation of the dry-run checks above --------
assert_no_relay() {
	local yaml units
	yaml=$(ssh -o BatchMode=yes "$TARGET_HOST" "cat ~/teamster/etc/teamster.yaml")
	if echo "$yaml" | grep -qE '^relay:|repl_push_remote'; then
		echo "FAIL: relay:/repl_push_remote present in clone's teamster.yaml" >&2
		return 1
	fi
	units=$(ssh -o BatchMode=yes "$TARGET_HOST" "systemctl list-unit-files 'teamster-relay*' 'teamster-repl-push*' --no-pager 2>&1")
	if echo "$units" | grep -qE 'teamster-relay|teamster-repl-push'; then
		echo "FAIL: relay/repl-push systemd units present on clone" >&2
		return 1
	fi
	echo "PASS: no relay/repl-push config or units on clone"
}

assert_no_forbidden_basedir_paths() {
	local yaml units
	yaml=$(ssh -o BatchMode=yes "$TARGET_HOST" "cat ~/teamster/etc/teamster.yaml")
	units=$(ssh -o BatchMode=yes "$TARGET_HOST" "systemctl cat 'teamster-*' --no-pager 2>&1")
	if echo "$yaml$units" | grep -qF "$TEST_FORBIDDEN_BASEDIR"; then
		echo "FAIL: $TEST_FORBIDDEN_BASEDIR path found in clone config or units" >&2
		return 1
	fi
	echo "PASS: no forbidden-basedir paths in clone config or units"
}

# --- A7: clone's MySQL is genuine MySQL 8.4, not a MariaDB regression -----
assert_mysql_engine_fidelity() {
	# The clone installs genuine MySQL 8.4 LTS via apt specifically so it
	# validates migrations against the same engine as production -- a
	# silent regression to trixie's default MariaDB would defeat that
	# invisibly. This is a direct fidelity check on that decision, not a
	# proxy for it (e.g. "did install report success").
	local ver
	ver=$(ssh -o BatchMode=yes "$TARGET_HOST" "teamster sql -N -e 'SELECT VERSION()'")
	if [[ "$ver" == *MariaDB* ]]; then
		echo "FAIL: clone's MySQL reports '$ver' -- regressed to MariaDB" >&2
		return 1
	fi
	if [[ "$ver" != 8.4.* ]]; then
		echo "FAIL: clone's MySQL reports '$ver' -- expected 8.4.x (8.0 is not apt-installable on trixie and is EOL)" >&2
		return 1
	fi
	echo "PASS: clone runs genuine MySQL $ver, not MariaDB"
}

# --- fallback only, not wired into run_clone --------------------------------
assert_collation_rewritten() {
	# Kept only as the documented fallback for a target platform that
	# genuinely cannot run MySQL 8.4 -- do not wire this into the default
	# pipeline. With genuine MySQL 8.4 (assert_mysql_engine_fidelity above),
	# utf8mb4_0900_ai_ci is native and no rewrite is needed.
	local fixed_dump="$1"
	local remaining
	remaining=$(ssh -o BatchMode=yes "$TARGET_HOST" "zcat '$fixed_dump' | grep -c utf8mb4_0900_ai_ci || true")
	if [[ "$remaining" != "0" ]]; then
		echo "FAIL: $fixed_dump still contains $remaining occurrence(s) of utf8mb4_0900_ai_ci -- restore would fail against MariaDB" >&2
		return 1
	fi
	echo "PASS: $fixed_dump has no remaining MySQL-8-only collation"
}

# =============================================================================
# section 3c: daemon stop/restart around the restore window (I7-adjacent)
# =============================================================================
# Two checkable facts per daemon, both derivable after the fact rather than
# needing to catch a live query mid-act: (1) stopped before restore started,
# (2) genuinely restarted after -- not merely "was never actually stopped."
#
# TIMING: teamster-rollup.timer and teamster-classify.timer use
# OnBootSec=2min relative to system boot, not to when --wire enables them.
# Clone's install leg (revert, ship, compile, download prometheus/grafana/
# otelcol) will almost certainly exceed two minutes, so by enable time their
# boot-relative deadline has already elapsed and systemd fires them
# immediately on activation, not on a comfortable steady-state cadence. This
# two-fact design (is-active inactive before / ActiveEnterTimestamp after)
# is correct regardless of that timing detail -- it observes what actually
# happened rather than assuming a cadence. Do not drop the guard on the
# assumption of a 10-minute buffer; there isn't one.
#
# RESIDUAL, not fixed here: the timers are enabled during install, before
# this guard's stop step ever runs, so rollup/classify have almost certainly
# already fired at least once against the freshly-migrated (and, per
# assert_schema_migrated above, possibly unmigrated) schema before the guard
# engages. Harmless against an empty schema; the reason assert_schema_
# migrated must run early is precisely to catch a bad migration before this
# window, not after.
DAEMONS_TO_GUARD=(teamster-rollup.timer teamster-classify.timer teamster-health-collector.service teamster-sweep.timer)

assert_daemon_stopped_before_restore() {
	local unit="$1"
	local state
	state=$(ssh -o BatchMode=yes "$TARGET_HOST" "systemctl is-active '$unit'" 2>&1 || true)
	if [[ "$state" != "inactive" ]]; then
		echo "FAIL: $unit is '$state', not 'inactive', immediately before restore" >&2
		return 1
	fi
	echo "PASS: $unit inactive before restore"
}

assert_daemon_restarted_after_restore() {
	# $2 = restore's own completion timestamp (epoch seconds), captured by
	# the caller around the `teamster restore` invocation.
	local unit="$1" restore_end_ts="$2"
	local active_enter_iso active_enter_ts
	active_enter_iso=$(ssh -o BatchMode=yes "$TARGET_HOST" \
		"systemctl show '$unit' --property=ActiveEnterTimestamp --value")
	active_enter_ts=$(date -d "$active_enter_iso" +%s 2>/dev/null || echo 0)
	if [[ "$active_enter_ts" -le "$restore_end_ts" ]]; then
		echo "FAIL: $unit's ActiveEnterTimestamp ($active_enter_iso) is not after restore's completion ($restore_end_ts) -- looks like it was never actually stopped" >&2
		return 1
	fi
	echo "PASS: $unit genuinely restarted after restore ($active_enter_iso)"
}

assert_all_daemons_guarded() {
	local phase="$1" restore_end_ts="${2:-}" fail=0
	for unit in "${DAEMONS_TO_GUARD[@]}"; do
		if [[ "$phase" == "pre" ]]; then
			assert_daemon_stopped_before_restore "$unit" || fail=1
		else
			assert_daemon_restarted_after_restore "$unit" "$restore_end_ts" || fail=1
		fi
	done
	return $fail
}

# --- A9: paid/write timers neutralized permanently, not just quiesced -----
# teamster-sweep.timer is deliberately in both this list and
# DAEMONS_TO_GUARD above, not a conflict: it's stopped-then-restarted across
# the restore window as part of the quiesce guard, then, as the true final
# step of the whole pipeline (after the clone is confirmed live), all four
# units below get masked permanently. `mask`, specifically, not `disable`:
# --wire already ran enable/enable-now once, so only mask actually prevents
# a future reboot or manual `systemctl start` from reawakening them.
#
# Without this, sweep.timer's oneshot wrapper runs `claude --print
# /teamster:sweep` hourly gated on `rollup --count-orphans`, which passes
# immediately once restored onto the source's full history -- a disposable
# VM would start making paid API calls on its own, unattended, forever.
# backup.timer (enabled without --now) has the same problem on any reboot.
# review-sweep.timer would abandon/park the clone's own WMS entities on its
# nightly run; mcp-scraper.timer would corrupt the hub's call-volume
# measurement by ledgering the clone's local traffic as if it were the hub's
# (see CLAUDE.md's mcp-scraper row -- single-writer invariant).
NEUTRALIZE_PERMANENTLY=(teamster-sweep.timer teamster-backup.timer teamster-wms-review-sweep.timer teamster-mcp-scraper.timer)

assert_scheduled_timers_neutralized() {
	local unit="$1" expected_state="${2:-masked}"
	local state
	state=$(ssh -o BatchMode=yes "$TARGET_HOST" "systemctl is-enabled '$unit'" 2>&1 || true)
	if [[ "$state" != "$expected_state" ]]; then
		echo "FAIL: $unit is '$state', expected '$expected_state' -- an unattended clone would keep spending money / writing backups indefinitely" >&2
		return 1
	fi
	echo "PASS: $unit is $expected_state, will not fire unattended"
}

assert_all_timers_neutralized() {
	local fail=0
	for unit in "${NEUTRALIZE_PERMANENTLY[@]}"; do
		assert_scheduled_timers_neutralized "$unit" || fail=1
	done
	return $fail
}

# --- A10 / I7 enforcement: STUB, v1 blocker, not deferred work -------------
# Every source-side query in this file (assert_row_counts_match,
# assert_schema_migrated, snapshot_source) currently rides the existing
# app-user `teamster sql` DSN, which holds GRANT ALL on the source, because
# `teamster sql` has no read-only guard (a statement returning no result set
# produces no output and no error). "SELECT-only by discipline" is the only
# current control. The real fix -- a dedicated read-only MySQL user plus a
# --read-only mode on `teamster sql` that clone passes unconditionally
# against the source -- is designed but not yet shipped. Do not ship v1
# without it. Once it lands, this function should assert the mechanism is
# actually in effect (a non-SELECT statement through the read-only path
# gets rejected, or the DSN used for source queries resolves to the
# read-only user, not the app user).
assert_source_read_only_guard_enforced() {
	echo "STUB: source read-only enforcement not yet designed/shipped -- v1 blocker." >&2
	return 2
}

# =============================================================================
# section 4: failure-path test cases
# =============================================================================
# Each function documents the exact assertion shape and performs it once
# `teamster clone` exists to invoke -- until then it's a stub that prints
# what it will check and returns 2 (skip), distinguishing "not yet wired"
# from "ran and failed."

test_failure_unreachable_ref() {
	# --github-repo against a ref absent from the public mirror must refuse
	# cleanly, before any transfer:
	#   teamster clone --github-repo=<public mirror url> <target>
	# Assert: nonzero exit; stderr carries the resolved full hash, the
	# refusal reason, and the exact retry flag (--repo-dir=<path>); zero new
	# SSH sessions to the target (the refusal fires during ref resolution,
	# strictly before any archive/transfer step runs). Same assertions apply
	# to the --dry-run variant -- it still performs the real reachability
	# probe but stops before shipping.
	echo "SKIP: test_failure_unreachable_ref -- requires 'teamster clone --github-repo' (WP1)." >&2
	return 2
}

test_failure_dirty_tree() {
	# A dirty --repo-dir without --allow-dirty must refuse before any
	# transfer, same structural guarantee as the unreachable-ref case (zero
	# new SSH sessions). The positive case (--allow-dirty) must succeed, but
	# the CLI's own stdout/stderr must carry a "shipped WITH uncommitted
	# local changes" line -- /health's commit field will NOT reflect a dirty
	# ship (the hash doesn't change just because dirty content rode along),
	# so that line is the only honest record of it. A test that only checks
	# /health here would silently pass on a case that should read as
	# "succeeded, but flagged," not "succeeded, full stop."
	echo "SKIP: test_failure_dirty_tree -- requires 'teamster clone' + --allow-dirty (WP1)." >&2
	return 2
}

test_failure_schema_mismatch() {
	# Restoring an older-schema dump against a newer binary. Two variants
	# worth keeping across the fix landing:
	#   pre-fix (documents the current gap): restore succeeds silently at
	#   call time; the real failure only surfaces later, on the next
	#   store.Open (e.g. a hookd restart). Keep this to prove the gap is
	#   real -- flip its polarity once the precheck lands rather than
	#   deleting it, so a regression back to silent-accept gets caught.
	#   post-fix: restore must refuse before touching the target DB, with
	#   the informational (not blocking) case -- older dump, newer binary --
	#   asserted separately as "proceeds with a logged note."
	echo "SKIP: test_failure_schema_mismatch -- requires WP3 restore + a schema-version precheck." >&2
	return 2
}

test_failure_stale_directory() {
	# Pre-seed a leftover ~/clone-src/<full_hash>/ on the target (simulating
	# an aborted prior run), then invoke clone again against the same hash
	# and target. assert_fresh_directory_enforced checks the mkdir (no -p)
	# fails loudly rather than silently reusing or overwriting stale
	# content -- clone must not proceed into extraction, the content-
	# fingerprint check, or the installer against a stale directory.
	echo "SKIP: test_failure_stale_directory -- requires 'teamster clone' (fresh-directory enforcement)." >&2
	return 2
}
# Two more failure paths are covered without a dedicated function here:
# dry-run structural assertions (§3b above -- happy-path label ordering and
# the absent-on-failure check for later labels) and content-manifest
# corruption, which reuses assert_content_fingerprint_match directly against
# a deliberately corrupted extracted file and is runnable today, same as
# that assertion.

# =============================================================================
# section 5: clone invocation -- the one true stub
# =============================================================================
run_clone() {
	echo "run_clone: STUB. Wire up once 'teamster clone' exists. Target shape:"
	cat <<'SHAPE'
  teamster clone --repo-dir=<path-to-repo> <user>@<target-host>

  # Runs on the SOURCE host, same execution model as the existing remote-
  # install: it reads locally and pushes outward over SSH to the target.
  # This harness's SOURCE_HOST/TARGET_HOST split is a testing convenience
  # (any host with SSH to both); the real binary's source-side queries are
  # local, not SSH-indirected -- which is why assert_source_read_only_guard_
  # enforced above is a hard v1 blocker: those unattended source queries are
  # local product code running on the master itself, not even
  # network-adjacent.

  # Composes, in order:
  #   1. Resolve short->full commit hash; compute_expected_fingerprint
  #      (git ls-tree -r <full_hash> | sha256sum); git archive; push-ship.
  #   2. Enforced-fresh extraction: `mkdir <target-dir>` with no -p, e.g.
  #      ~/clone-src/<full_hash>/. Fails loudly on a pre-existing path.
  #      (assert_fresh_directory_enforced)
  #   3. Immediately after extraction, before the installer runs:
  #      compute_actual_fingerprint on the target, compare against the
  #      expected fingerprint from step 1 -- the real provenance gate,
  #      independent of anything clone itself injects.
  #      (assert_content_fingerprint_match)
  #   4. Translate(source teamster.yaml, TargetSpec) -> flag vector, then
  #        TEAMSTER_COMMIT=<short> ./lib/installrunner.sh \
  #          --hookd-mode=systemd --store-mode=install --otelcol-mode=install \
  #          --prometheus-mode=install --grafana-mode=install --env=clone --wire
  #      This install path provisions genuine MySQL 8.4 LTS via apt (clone-
  #      specific, behind a flag) and must also CREATE DATABASE IF NOT
  #      EXISTS + GRANT for claude_telemetry, mirroring what it already does
  #      for the teamster database.
  #   5. assert_schema_migrated -- before quiesce/restore. The install-time
  #      migration call is non-fatal on failure and hookd never auto-starts
  #      to provide a second chance, so this is the only real check.
  #   6. capture_etc_tree_manifest -- snapshot the clone's whole etc/ tree
  #      right after install, for assert_etc_tree_unchanged_by_restore to
  #      diff against post-restore.
  #   7. assert_version_commit_gate -- `ssh <target> teamster --version`,
  #      prefix-match commit, abort before the data leg on mismatch. No
  #      daemon required. Verifies env-var plumbing only, not tree
  #      provenance -- step 3 already covered that, earlier and
  #      independently.
  #   8. Data leg:
  #        a. trigger a fresh backup on the source
  #        b. push-transfer, excluding teamster/config.tar.gz and
  #           otel/otelcol.yaml at the transfer layer -- not shipping them
  #           is how isolation is achieved, no restore flags needed.
  #           (assert_config_not_shipped)
  #        c. stop DAEMONS_TO_GUARD units; capture $restore_start_ts
  #        d. teamster restore <transferred-dir> \
  #             --config=<target-teamster.yaml-path> \
  #             --exclude=teamster/config.tar.gz --force
  #           capture $restore_end_ts
  #        e. restart the DAEMONS_TO_GUARD units stopped in (c)
  #        f. before hookd's first start (load-bearing ordering -- otelcol.
  #           yaml is genuinely re-rendered by the supervisor's first start,
  #           and comparing across that boundary would false-fail on a
  #           correct run): capture a second etc/ manifest (M2), then
  #           assert_row_counts_match + assert_etc_tree_unchanged_by_restore
  #           comparing M1 (step 6) against M2.
  #        g. `teamster start` (not `systemctl start teamster-hookd` -- a
  #           fresh --wire install's supervisor-launch block never fires on
  #           its own). Starts hookd idempotently and launches every
  #           install-mode service on this same call -- the clone's genuine
  #           first start of its whole managed bundle. Then:
  #           assert_health_commit_confirmation + assert_health_version_dirty
  #           (confirmation only, not independent of step 7's gate), then
  #           assert_managed_services_running (do not infer "up" from
  #           `teamster start`'s own exit code), then capture a third etc/
  #           manifest (M3) and run assert_only_expected_files_rendered_by_
  #           first_start(M2, M3).
  #        h. final step of the whole pipeline, after the clone is
  #           confirmed live and correct: mask teamster-sweep.timer,
  #           teamster-backup.timer, teamster-wms-review-sweep.timer, and
  #           teamster-mcp-scraper.timer permanently (four units, R11).
  #           (assert_scheduled_timers_neutralized, expect "masked")
SHAPE
	return 42
}

# =============================================================================
# orchestration
# =============================================================================
main() {
	echo "=== clone acceptance run: $(date -u -Iseconds) ==="
	revert_target
	wait_target_ready

	local pre post
	pre=$(snapshot_source pre)

	if ! run_clone; then
		echo "run_clone is a stub (expected until WP1/2/3 land) -- skipping"
		echo "clone-side assertions, still exercising the revert+snapshot loop."
	fi

	run_check "unreachable-ref refusal" test_failure_unreachable_ref || true
	run_check "dirty-tree refusal" test_failure_dirty_tree || true
	run_check "schema-mismatch handling" test_failure_schema_mismatch || true
	run_check "stale-directory refusal" test_failure_stale_directory || true

	post=$(snapshot_source post)
	diff_source_snapshots "$pre" "$post"

	revert_target
	wait_target_ready

	echo "=== done: $CHECKS_RUN checks run, $CHECKS_PASSED passed, $CHECKS_SKIPPED skipped ==="
}

main "$@"
